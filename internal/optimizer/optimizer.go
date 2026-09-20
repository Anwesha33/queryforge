package optimizer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Anwesha33/queryforge/internal/engine"
	"github.com/Anwesha33/queryforge/internal/llm"
	"github.com/Anwesha33/queryforge/internal/rules"
	"github.com/Anwesha33/queryforge/internal/sqlparse"
)

const proposerSystem = `You rewrite PostgreSQL queries to run faster. You are one source of candidates in a system that verifies every one of them by execution, so propose freely but never claim a rewrite is safe — that is decided by measurement, not by you.

Hard requirements for every candidate:
- It must return EXACTLY the same rows, with the same columns in the same order, as the original. The system compares result checksums and discards anything that differs.
- It must be a single read-only SELECT. Never emit INSERT, UPDATE, DELETE, CREATE, or a data-modifying CTE.
- It must be valid PostgreSQL that runs against the schema given. Do not invent columns, tables or functions.

What is worth proposing:
- Making a predicate sargable so an index can be used: replace a function or cast wrapping an indexed column with an equivalent range or expression on the bare column.
- Replacing a correlated subquery with a join, or a join with EXISTS, where the row multiplicity is genuinely unchanged.
- Replacing NOT IN with NOT EXISTS where the NULL semantics permit it.
- Pushing a filter or a LIMIT down so fewer rows are produced before an expensive step.
- Removing a join whose table contributes no columns and cannot change row multiplicity.
- Replacing DISTINCT with a grouping or an EXISTS where that is what the query actually means.
- Restructuring an OR across different columns into a UNION ALL of two selective branches, where the branches cannot overlap.

What is not worth proposing:
- Cosmetic changes: aliasing, formatting, reordering the select list.
- Adding LIMIT, changing ORDER BY, or anything else that changes which rows come back. That is not an optimisation, it is a different query.
- Hints or planner settings. This system recommends queries and indexes, not session configuration.

If the query is already well formed and you have no substantive rewrite, return an empty candidate list. That is a useful and correct answer.`

// Options configures a run.
type Options struct {
	MaxCandidates     int
	MinImprovementPct float64
	Runs              int
	// TestIndexes controls whether hypothetical index experiments run. They are
	// the slowest part of a job, because each one builds a real index.
	TestIndexes   bool
	MaxIndexTests int
	SkipLLM       bool
}

func DefaultOptions() Options {
	return Options{
		MaxCandidates: 4, MinImprovementPct: 10, Runs: 5,
		TestIndexes: true, MaxIndexTests: 3,
	}
}

// Report is the full outcome of optimising one query.
type Report struct {
	OriginalSQL string `json:"original_sql"`
	Fingerprint string `json:"fingerprint"`
	Dialect     string `json:"dialect"`

	Schema   *engine.Schema  `json:"schema,omitempty"`
	Findings []rules.Finding `json:"findings"`
	// Limitations names analyses that did not run against this database, so an
	// empty findings list is never mistaken for a clean bill of health.
	Limitations []string       `json:"limitations,omitempty"`
	Baseline    BaselineReport `json:"baseline"`

	Candidates []Result          `json:"candidates"`
	Indexes    []IndexExperiment `json:"index_experiments"`

	// Best is the accepted candidate with the largest verified improvement, if
	// any survived.
	Best *Result `json:"best,omitempty"`

	LLMUsed      bool   `json:"llm_used"`
	LLMModel     string `json:"llm_model,omitempty"`
	PromptTokens int    `json:"prompt_tokens"`
	OutputTokens int    `json:"output_tokens"`
	DurationMS   int64  `json:"duration_ms"`
	Summary      string `json:"summary"`
}

// BaselineReport is the measured original.
type BaselineReport struct {
	MedianMS   float64 `json:"median_ms"`
	MinMS      float64 `json:"min_ms"`
	MaxMS      float64 `json:"max_ms"`
	TotalCost  float64 `json:"total_cost"`
	RowCount   int64   `json:"row_count"`
	ActualRows float64 `json:"actual_rows"`
	Plan       string  `json:"plan"`
}

// Optimizer runs the whole pipeline.
type Optimizer struct {
	Engine engine.Engine
	LLM    *llm.Client
	Opts   Options
	Log    Logger
	// Dialect selects the grammar used to parse incoming SQL. Empty means
	// Postgres, which keeps existing callers unchanged.
	Dialect sqlparse.Dialect
}

// Run executes every phase and returns a report.
//
// Phases: parse → introspect → measure baseline → apply rules → propose (LLM) →
// verify every candidate → measure index experiments → summarise.
func (o *Optimizer) Run(ctx context.Context, sql string) (*Report, error) {
	start := time.Now()

	st, err := sqlparse.ParseDialect(sql, o.Dialect)
	if err != nil {
		return nil, err
	}

	rep := &Report{
		OriginalSQL: st.SQL,
		Fingerprint: st.Fingerprint,
		Dialect:     o.Engine.Dialect(),
	}

	tables := make([]string, 0, len(st.Tables))
	for _, t := range st.Tables {
		tables = append(tables, t.Qualified())
	}
	schema, err := o.Engine.Introspect(ctx, tables)
	if err != nil {
		return nil, fmt.Errorf("introspect: %w", err)
	}
	rep.Schema = schema

	base, err := EstablishBaseline(ctx, o.Engine, st, o.Opts.Runs)
	if err != nil {
		return nil, err
	}
	rep.Baseline = BaselineReport{
		MedianMS: base.Measured.MedianMS, MinMS: base.Measured.MinMS, MaxMS: base.Measured.MaxMS,
		TotalCost: base.Plan.TotalCost, RowCount: base.Checksum.RowCount,
		ActualRows: base.Measured.ActualRows,
	}
	if base.Measured.Plan != nil {
		rep.Baseline.Plan = base.Measured.Plan.Summary
	}
	o.Log.Info("baseline measured", "median_ms", base.Measured.MedianMS,
		"rows", base.Checksum.RowCount, "cost", base.Plan.TotalCost)

	analysis := rules.Analyze(st, schema, base.Measured)
	rep.Limitations = analysis.Limitations
	rep.Findings = analysis.Findings
	o.Log.Info("rules applied", "findings", len(analysis.Findings))

	// Candidates come from two places. Rule rewrites first, because they are
	// free, deterministic and reproducible — and because the service must be
	// useful with no API key at all.
	var candidates []Candidate
	for _, f := range analysis.Findings {
		if f.Rewrite != "" {
			candidates = append(candidates, Candidate{
				SQL: f.Rewrite, Source: "rule:" + f.Code, Rationale: f.Title,
			})
		}
	}

	if !o.Opts.SkipLLM && o.LLM.Enabled() {
		proposed, usage, err := o.propose(ctx, st, schema, base, analysis)
		if err != nil {
			// A proposer failure degrades the run; it does not fail it. The
			// rule-derived candidates and the index experiments still stand.
			o.Log.Error("llm proposer failed; continuing with rule-derived candidates", "err", err)
		} else {
			candidates = append(candidates, proposed...)
			rep.LLMUsed = true
			rep.LLMModel = o.LLM.Model()
			rep.PromptTokens = usage.PromptTokens
			rep.OutputTokens = usage.OutputTokens
		}
	}

	verifier := &Verifier{
		Engine: o.Engine, MinImprovementPct: o.Opts.MinImprovementPct,
		Runs: o.Opts.Runs, Log: o.Log, Dialect: o.Dialect,
	}

	seen := map[string]bool{normalizeSQL(st.SQL): true}
	for _, c := range candidates {
		key := normalizeSQL(c.SQL)
		if seen[key] {
			continue
		}
		seen[key] = true

		result := verifier.Verify(ctx, base, c)
		rep.Candidates = append(rep.Candidates, result)
		o.Log.Info("candidate verified", "source", c.Source, "verdict", string(result.Verdict),
			"reason", string(result.Reason), "improvement_pct", result.ImprovementPct)
	}

	for i := range rep.Candidates {
		r := &rep.Candidates[i]
		if r.Verdict != VerdictAccepted {
			continue
		}
		if rep.Best == nil || r.ImprovementPct > rep.Best.ImprovementPct {
			rep.Best = r
		}
	}

	if o.Opts.TestIndexes {
		rep.Indexes = o.runIndexExperiments(ctx, verifier, base, analysis)
	}

	rep.DurationMS = time.Since(start).Milliseconds()
	rep.Summary = summarize(rep)
	return rep, nil
}

func (o *Optimizer) runIndexExperiments(ctx context.Context, v *Verifier, base *Baseline, a *rules.Analysis) []IndexExperiment {
	var out []IndexExperiment
	tested := 0
	for _, f := range a.Findings {
		if f.SuggestedIndex == "" {
			continue
		}
		if tested >= o.Opts.MaxIndexTests {
			o.Log.Info("index experiment budget reached; remaining suggestions are untested",
				"budget", o.Opts.MaxIndexTests)
			break
		}
		tested++
		exp, err := v.MeasureIndex(ctx, base, f.SuggestedIndex, f.Table)
		if errors.Is(err, engine.ErrNoHypotheticalIndexes) {
			// Not a failure: this engine cannot build an index and throw it
			// away, so the candidate is reported unmeasured and explicitly NOT
			// recommended. Recommending an index this service has not timed
			// would be the guess the whole project exists to avoid.
			o.Log.Info("index candidate left unmeasured", "ddl", f.SuggestedIndex, "reason", "engine cannot measure hypothetical indexes")
			out = append(out, IndexExperiment{
				DDL: f.SuggestedIndex, Table: f.Table, Recommended: false,
				Detail: "candidate identified but NOT measured: this engine cannot create and roll back an index, " +
					"so there is no evidence it would help. Measure it on a replica before creating it.",
			})
			continue
		}
		if err != nil {
			o.Log.Error("index experiment failed", "ddl", f.SuggestedIndex, "err", err)
			out = append(out, IndexExperiment{
				DDL: f.SuggestedIndex, Table: f.Table, Recommended: false,
				Detail: "could not be measured: " + err.Error(),
			})
			continue
		}
		o.Log.Info("index measured", "ddl", f.SuggestedIndex,
			"improvement_pct", exp.ImprovementPct, "recommended", exp.Recommended)
		out = append(out, *exp)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ImprovementPct > out[j].ImprovementPct })
	return out
}

// ------------------------------------------------------------- the proposer

type proposal struct {
	SQL       string `json:"sql"`
	Rationale string `json:"rationale"`
}

type proposalEnvelope struct {
	Candidates []proposal `json:"candidates"`
}

func candidateSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"candidates": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"sql":       map[string]any{"type": "string"},
						"rationale": map[string]any{"type": "string"},
					},
					"required": []string{"sql", "rationale"},
				},
			},
		},
		"required": []string{"candidates"},
	}
}

func (o *Optimizer) propose(ctx context.Context, st *sqlparse.Statement, schema *engine.Schema,
	base *Baseline, analysis *rules.Analysis) ([]Candidate, llm.Usage, error) {

	prompt := buildProposerPrompt(st, schema, base, analysis, o.Opts.MaxCandidates)
	resp, err := o.LLM.Generate(ctx, llm.Request{
		System: proposerSystem,
		Prompt: prompt,
		Config: llm.GenerationConfig{
			Temperature:      0.2,
			ResponseMIMEType: "application/json",
			ResponseSchema:   candidateSchema(),
		},
	})
	if err != nil {
		return nil, llm.Usage{}, err
	}

	raw := resp.Text
	if j, ok := llm.ExtractJSON(resp.Text); ok {
		raw = j
	}
	var env proposalEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return nil, resp.Usage, fmt.Errorf("proposer returned unusable JSON: %w", err)
	}

	out := make([]Candidate, 0, len(env.Candidates))
	for _, p := range env.Candidates {
		sql := strings.TrimSpace(p.SQL)
		if sql == "" {
			continue
		}
		out = append(out, Candidate{SQL: sql, Source: "llm", Rationale: p.Rationale})
		if len(out) >= o.Opts.MaxCandidates {
			break
		}
	}
	return out, resp.Usage, nil
}

func buildProposerPrompt(st *sqlparse.Statement, schema *engine.Schema,
	base *Baseline, analysis *rules.Analysis, maxCandidates int) string {

	var b strings.Builder
	b.WriteString("Query to optimise:\n\n```sql\n")
	b.WriteString(st.SQL)
	b.WriteString("\n```\n\n")

	b.WriteString("Schema of the tables it touches:\n\n")
	for _, t := range schema.Tables {
		fmt.Fprintf(&b, "TABLE %s  (~%d rows, %d MB)\n", t.Name, t.RowEstimate, t.TotalBytes/(1024*1024))
		for _, c := range t.Columns {
			null := "NOT NULL"
			if c.Nullable {
				null = "NULL"
			}
			// n_distinct is included because it is what separates a useful
			// index from a useless one, and the model cannot infer it.
			fmt.Fprintf(&b, "  %-24s %-28s %s  (n_distinct=%.0f, null_frac=%.2f)\n",
				c.Name, c.Type, null, c.DistinctValues, c.NullFraction)
		}
		if len(t.Indexes) == 0 {
			b.WriteString("  INDEXES: none\n")
		} else {
			b.WriteString("  INDEXES:\n")
			for _, idx := range t.Indexes {
				fmt.Fprintf(&b, "    %s  (%d scans so far)\n", idx.Definition, idx.Scans)
			}
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "Current plan (EXPLAIN ANALYZE, median of %d runs: %.2fms, returning %d rows):\n\n```\n%s\n```\n\n",
		base.Measured.Runs, base.Measured.MedianMS, base.Checksum.RowCount, base.Measured.Plan.Summary)

	fmt.Fprintf(&b, "Static analysis has already found:\n%s\n", analysis.Summarize())

	fmt.Fprintf(&b, "\nPropose up to %d rewrites. Every one will be executed and its results compared "+
		"byte for byte against the original before it is shown to anyone, so a rewrite that changes "+
		"the result set is wasted effort rather than a risk.\n", maxCandidates)

	if st.SelectStar {
		// Worth stating explicitly: the checksum compares the full row, so
		// narrowing the select list always fails verification. The model will
		// otherwise propose it every time, since it is the most famous SQL
		// advice there is.
		b.WriteString("\nNote: this query uses SELECT *. Narrowing the select list changes the result " +
			"set and will fail verification, so do not propose it — it is reported separately as a " +
			"finding for a human to act on.\n")
	}
	return b.String()
}

func summarize(rep *Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Baseline %.2fms over %d rows. ", rep.Baseline.MedianMS, rep.Baseline.RowCount)

	accepted, rejected := 0, 0
	for _, c := range rep.Candidates {
		if c.Verdict == VerdictAccepted {
			accepted++
		} else {
			rejected++
		}
	}
	fmt.Fprintf(&b, "%d candidate(s) tested, %d verified faster, %d rejected. ",
		len(rep.Candidates), accepted, rejected)

	if rep.Best != nil {
		fmt.Fprintf(&b, "Best rewrite: %.1f%% faster (%.2fms → %.2fms) with identical results. ",
			rep.Best.ImprovementPct, rep.Baseline.MedianMS, rep.Best.CandidateMedianMS)
	} else if len(rep.Candidates) > 0 {
		b.WriteString("No rewrite was proven faster with identical results. ")
	}

	recommended := 0
	for _, idx := range rep.Indexes {
		if idx.Recommended {
			recommended++
		}
	}
	if len(rep.Indexes) > 0 {
		fmt.Fprintf(&b, "%d of %d candidate index(es) measured as worthwhile.", recommended, len(rep.Indexes))
	}
	return strings.TrimSpace(b.String())
}
