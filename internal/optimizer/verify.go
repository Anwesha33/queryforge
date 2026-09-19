// Package optimizer runs the optimisation pipeline and, more importantly, the
// verification loop that decides whether a proposed rewrite is allowed to be
// recommended.
//
// The rule that governs this file: a candidate is rejected unless it is proven
// both *equivalent* and *faster*. Not "the model was confident", not "the plan
// cost went down" — proven, by executing both queries and comparing their
// results and their measured time.
//
//	candidate
//	   │
//	   ├─ parses, and is a read-only SELECT?          no ──▶ reject
//	   ├─ same output shape and tables?               no ──▶ reject
//	   ├─ identical result checksum?                  no ──▶ reject  ← the load-bearing one
//	   ├─ median time improved beyond noise?          no ──▶ reject
//	   └─ accept, with the evidence attached
package optimizer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Anwesha33/queryforge/internal/engine"
	"github.com/Anwesha33/queryforge/internal/sqlparse"
)

// Verdict is the outcome of verifying one candidate.
type Verdict string

const (
	VerdictAccepted Verdict = "accepted"
	VerdictRejected Verdict = "rejected"
	VerdictError    Verdict = "error"
)

// RejectReason names why a candidate failed, so failures are countable rather
// than anecdotal.
type RejectReason string

const (
	ReasonUnparseable    RejectReason = "candidate_does_not_parse"
	ReasonNotReadOnly    RejectReason = "candidate_is_not_read_only"
	ReasonShapeMismatch  RejectReason = "output_shape_differs"
	ReasonResultMismatch RejectReason = "results_differ"
	ReasonNotFaster      RejectReason = "no_measurable_improvement"
	ReasonTimedOut       RejectReason = "candidate_timed_out"
	ReasonIdentical      RejectReason = "identical_to_original"
	ReasonExecutionError RejectReason = "candidate_failed_to_execute"
)

// Candidate is a proposed rewrite awaiting verification.
type Candidate struct {
	SQL string `json:"sql"`
	// Source records who proposed it: a deterministic rule or the model.
	Source    string `json:"source"`
	Rationale string `json:"rationale"`
}

// Result is a verified candidate with its evidence.
type Result struct {
	Candidate Candidate    `json:"candidate"`
	Verdict   Verdict      `json:"verdict"`
	Reason    RejectReason `json:"reason,omitempty"`
	Detail    string       `json:"detail,omitempty"`

	BaselineMedianMS  float64 `json:"baseline_median_ms"`
	CandidateMedianMS float64 `json:"candidate_median_ms"`
	SpeedupFactor     float64 `json:"speedup_factor"`
	ImprovementPct    float64 `json:"improvement_pct"`

	BaselineCost  float64 `json:"baseline_total_cost"`
	CandidateCost float64 `json:"candidate_total_cost"`

	ResultsIdentical bool   `json:"results_identical"`
	RowCount         int64  `json:"row_count"`
	CandidatePlan    string `json:"candidate_plan,omitempty"`
}

// Baseline is everything measured about the original query.
type Baseline struct {
	Statement *sqlparse.Statement
	Plan      *engine.Plan
	Measured  *engine.Measurement
	Checksum  *engine.ResultHash
	Ordered   bool
}

// Verifier proves or disproves a candidate.
type Verifier struct {
	Engine engine.Engine
	// MinImprovementPct is the floor a candidate must clear. Below this, a
	// "faster" query is indistinguishable from a quiet minute on the machine.
	MinImprovementPct float64
	// Runs is how many timed executions per query.
	Runs int
	Log  Logger
}

type Logger interface {
	Info(msg string, kv ...any)
	Error(msg string, kv ...any)
}

// EstablishBaseline measures the original query so candidates have something to
// beat.
func EstablishBaseline(ctx context.Context, eng engine.Engine, st *sqlparse.Statement, runs int) (*Baseline, error) {
	plan, err := eng.Explain(ctx, st.SQL)
	if err != nil {
		return nil, fmt.Errorf("baseline plan: %w", err)
	}
	measured, err := eng.Measure(ctx, st.SQL, runs)
	if err != nil {
		return nil, fmt.Errorf("baseline timing: %w", err)
	}
	if measured.TimedOut {
		return nil, errors.New("the original query exceeded the statement timeout; raise QUERY_TIMEOUT or reduce the data set")
	}
	// Order is part of the result identity only when the query promises one.
	ordered := st.HasOrderBy
	checksum, err := eng.Checksum(ctx, st.SQL, ordered)
	if err != nil {
		return nil, fmt.Errorf("baseline checksum: %w", err)
	}
	return &Baseline{Statement: st, Plan: plan, Measured: measured, Checksum: checksum, Ordered: ordered}, nil
}

// Verify runs the full gate for one candidate.
func (v *Verifier) Verify(ctx context.Context, base *Baseline, c Candidate) Result {
	res := Result{
		Candidate:        c,
		BaselineMedianMS: base.Measured.MedianMS,
		BaselineCost:     base.Plan.TotalCost,
		RowCount:         base.Checksum.RowCount,
	}

	// 1. It must parse, and it must be read-only. The parser is the same one
	//    the server uses, so "read-only" here means the same thing it means
	//    there — and a model asked for a SELECT has certainly been known to
	//    return a DELETE.
	candidateStmt, err := sqlparse.Parse(c.SQL)
	if err != nil {
		res.Verdict = VerdictRejected
		res.Reason = ReasonUnparseable
		if errors.Is(err, sqlparse.ErrNotReadOnly) {
			res.Reason = ReasonNotReadOnly
		}
		res.Detail = err.Error()
		return res
	}

	// 2. A candidate identical to the original is not an improvement, and
	//    measuring it would produce a coin-flip "speedup" from timing noise.
	if normalizeSQL(candidateStmt.SQL) == normalizeSQL(base.Statement.SQL) {
		res.Verdict = VerdictRejected
		res.Reason = ReasonIdentical
		res.Detail = "the candidate is the original query"
		return res
	}

	// 3. Cheap structural gate before the expensive execution gate.
	if err := base.Statement.SameShapeAs(candidateStmt); err != nil {
		res.Verdict = VerdictRejected
		res.Reason = ReasonShapeMismatch
		res.Detail = err.Error()
		return res
	}

	// 4. The load-bearing check: execute both and compare result checksums.
	//    Everything above this line is a heuristic. This is the one that makes
	//    an accepted rewrite safe to hand to a human, because "the SQL looks
	//    equivalent" has no bearing on whether it is.
	//
	//    The candidate is hashed with the same ordering rule as the baseline,
	//    which is what makes the two hashes comparable at all.
	candidateHash, err := v.Engine.Checksum(ctx, c.SQL, base.Ordered)
	if err != nil {
		res.Verdict = VerdictRejected
		res.Reason = ReasonExecutionError
		res.Detail = err.Error()
		return res
	}
	if !base.Checksum.Equal(candidateHash) {
		res.Verdict = VerdictRejected
		res.Reason = ReasonResultMismatch
		res.Detail = fmt.Sprintf(
			"the rewrite returns different data: original %d rows (hash %s), candidate %d rows (hash %s)",
			base.Checksum.RowCount, shortHash(base.Checksum.Hash),
			candidateHash.RowCount, shortHash(candidateHash.Hash))
		return res
	}
	res.ResultsIdentical = true

	// 5. It must be measurably faster. Plan cost is recorded but never decides:
	//    cost is the planner's estimate of work in arbitrary units, and a
	//    rewrite that lowers the estimate while raising the real time is a
	//    perfectly ordinary outcome.
	measured, err := v.Engine.Measure(ctx, c.SQL, v.Runs)
	if err != nil {
		res.Verdict = VerdictRejected
		res.Reason = ReasonExecutionError
		res.Detail = err.Error()
		return res
	}
	if measured.TimedOut {
		res.Verdict = VerdictRejected
		res.Reason = ReasonTimedOut
		res.Detail = "the candidate exceeded the statement timeout"
		return res
	}

	res.CandidateMedianMS = measured.MedianMS
	if measured.Plan != nil {
		res.CandidateCost = measured.Plan.TotalCost
		res.CandidatePlan = measured.Plan.Summary
	}
	res.ImprovementPct = improvementPct(base.Measured.MedianMS, measured.MedianMS)
	if measured.MedianMS > 0 {
		res.SpeedupFactor = base.Measured.MedianMS / measured.MedianMS
	}

	if ok, why := v.isFaster(base.Measured, measured); !ok {
		res.Verdict = VerdictRejected
		res.Reason = ReasonNotFaster
		res.Detail = why
		return res
	}

	res.Verdict = VerdictAccepted
	res.Detail = fmt.Sprintf("%.1f%% faster (%.2fms → %.2fms, %.1fx) with identical results over %d rows",
		res.ImprovementPct, base.Measured.MedianMS, measured.MedianMS, res.SpeedupFactor, res.RowCount)
	return res
}

// isFaster decides whether a difference in medians is real.
//
// Two guards, because a single percentage threshold is not enough on a machine
// that is also doing other things:
//
//   - the improvement must clear MinImprovementPct, and
//   - it must be larger than the observed run-to-run spread of the baseline.
//
// The second is the one that matters. If five runs of the original query ranged
// over 40ms, a candidate that is 20ms "faster" has told us nothing at all.
func (v *Verifier) isFaster(base, candidate *engine.Measurement) (bool, string) {
	improvement := base.MedianMS - candidate.MedianMS
	pct := improvementPct(base.MedianMS, candidate.MedianMS)

	if pct < v.MinImprovementPct {
		return false, fmt.Sprintf(
			"%.1f%% improvement (%.2fms → %.2fms) is below the %.0f%% threshold",
			pct, base.MedianMS, candidate.MedianMS, v.MinImprovementPct)
	}
	noise := maxFloat(base.SpreadMS, candidate.SpreadMS)
	if improvement <= noise {
		return false, fmt.Sprintf(
			"the %.2fms improvement is within measurement noise (run-to-run spread was %.2fms); "+
				"re-run with more iterations if this query matters",
			improvement, noise)
	}
	return true, ""
}

// IndexExperiment measures what an index would actually do, by creating it
// inside a transaction that is always rolled back.
type IndexExperiment struct {
	DDL               string  `json:"ddl"`
	Table             string  `json:"table"`
	BaselineMedianMS  float64 `json:"baseline_median_ms"`
	WithIndexMedianMS float64 `json:"with_index_median_ms"`
	ImprovementPct    float64 `json:"improvement_pct"`
	SpeedupFactor     float64 `json:"speedup_factor"`
	Recommended       bool    `json:"recommended"`
	Detail            string  `json:"detail"`
	PlanWithIndex     string  `json:"plan_with_index,omitempty"`
	BuildMS           int64   `json:"index_build_ms"`
}

// MeasureIndex builds the index, re-times the query, and rolls back.
//
// This is what separates an index *recommendation* from an index *guess*. The
// usual advice — "you filter on created_at, add an index on created_at" — is
// right often enough to be dangerous, because the cases where it is wrong (low
// selectivity, an existing composite index that already covers it, a table
// small enough that the scan wins) are exactly the cases where the index costs
// write throughput forever and returns nothing.
func (v *Verifier) MeasureIndex(ctx context.Context, base *Baseline, ddl, table string) (*IndexExperiment, error) {
	pg, ok := v.Engine.(*engine.Postgres)
	if !ok {
		return nil, errors.New("index experiments require the postgres engine")
	}

	exp := &IndexExperiment{DDL: ddl, Table: table, BaselineMedianMS: base.Measured.MedianMS}
	start := time.Now()

	err := pg.WithHypotheticalIndex(ctx, ddl, func(txCtx context.Context) error {
		exp.BuildMS = time.Since(start).Milliseconds()
		measured, err := pg.MeasureInTx(txCtx, base.Statement.SQL, v.Runs)
		if err != nil {
			return err
		}
		if measured.TimedOut {
			return errors.New("timed out while measuring with the index in place")
		}
		exp.WithIndexMedianMS = measured.MedianMS
		if measured.Plan != nil {
			exp.PlanWithIndex = measured.Plan.Summary
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	exp.ImprovementPct = improvementPct(base.Measured.MedianMS, exp.WithIndexMedianMS)
	if exp.WithIndexMedianMS > 0 {
		exp.SpeedupFactor = base.Measured.MedianMS / exp.WithIndexMedianMS
	}

	switch {
	case exp.ImprovementPct < v.MinImprovementPct:
		exp.Recommended = false
		exp.Detail = fmt.Sprintf(
			"measured %.1f%% change (%.2fms → %.2fms) — not worth the write cost. "+
				"Every insert and update to %s would maintain this index for no measured gain.",
			exp.ImprovementPct, base.Measured.MedianMS, exp.WithIndexMedianMS, table)
	default:
		exp.Recommended = true
		exp.Detail = fmt.Sprintf(
			"measured %.1f%% faster (%.2fms → %.2fms, %.1fx) with the index in place. "+
				"Build it with CREATE INDEX CONCURRENTLY in production to avoid taking a write lock.",
			exp.ImprovementPct, base.Measured.MedianMS, exp.WithIndexMedianMS, exp.SpeedupFactor)
	}
	return exp, nil
}

func improvementPct(baseline, candidate float64) float64 {
	if baseline <= 0 {
		return 0
	}
	return (baseline - candidate) / baseline * 100
}

func normalizeSQL(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
