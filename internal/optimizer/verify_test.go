package optimizer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Anwesha33/queryforge/internal/engine"
	"github.com/Anwesha33/queryforge/internal/sqlparse"
)

// fakeEngine lets the verification gates be tested without a database, which is
// what makes it possible to assert on the cases that matter — a rewrite that
// changes results, a rewrite that is faster only within noise — deterministically.
type fakeEngine struct {
	hashes   map[string]*engine.ResultHash
	measures map[string]*engine.Measurement
	err      error
}

func (f *fakeEngine) Introspect(context.Context, []string) (*engine.Schema, error) {
	return &engine.Schema{}, nil
}
func (f *fakeEngine) Explain(context.Context, string) (*engine.Plan, error) {
	return &engine.Plan{TotalCost: 100}, nil
}
func (f *fakeEngine) Measure(_ context.Context, sql string, _ int) (*engine.Measurement, error) {
	if f.err != nil {
		return nil, f.err
	}
	if m, ok := f.measures[key(sql)]; ok {
		return m, nil
	}
	return &engine.Measurement{MedianMS: 100, SpreadMS: 2, Plan: &engine.Plan{TotalCost: 100}}, nil
}
func (f *fakeEngine) Checksum(_ context.Context, sql string, ordered bool) (*engine.ResultHash, error) {
	if h, ok := f.hashes[key(sql)]; ok {
		c := *h
		c.Ordered = ordered
		return &c, nil
	}
	return &engine.ResultHash{Hash: "same", RowCount: 10, Ordered: ordered}, nil
}
func (f *fakeEngine) WithHypotheticalIndex(context.Context, string, func(context.Context) error) error {
	return errors.New("not supported by the fake engine")
}
func (f *fakeEngine) Dialect() string { return "fake" }
func (f *fakeEngine) Close()          {}

func key(sql string) string { return strings.ToLower(strings.Join(strings.Fields(sql), " ")) }

const originalSQL = `SELECT id, total_cents FROM orders WHERE created_at >= '2026-06-01'`

func baselineFor(t *testing.T, f *fakeEngine) *Baseline {
	t.Helper()
	st, err := sqlparse.Parse(originalSQL)
	if err != nil {
		t.Fatal(err)
	}
	base, err := EstablishBaseline(context.Background(), f, st, 5)
	if err != nil {
		t.Fatal(err)
	}
	return base
}

func newVerifier(f *fakeEngine) *Verifier {
	return &Verifier{Engine: f, MinImprovementPct: 10, Runs: 5, Log: quiet{}}
}

type quiet struct{}

func (quiet) Info(string, ...any)  {}
func (quiet) Error(string, ...any) {}

// The gate that matters most: a rewrite returning different data is rejected
// however fast it is.
func TestVerifyRejectsResultMismatchEvenWhenMuchFaster(t *testing.T) {
	candidate := `SELECT id, total_cents FROM orders WHERE created_at >= '2026-06-02'`
	f := &fakeEngine{
		hashes: map[string]*engine.ResultHash{
			key(originalSQL): {Hash: "aaa", RowCount: 100},
			key(candidate):   {Hash: "bbb", RowCount: 40},
		},
		measures: map[string]*engine.Measurement{
			key(originalSQL): {MedianMS: 100, SpreadMS: 2, Plan: &engine.Plan{TotalCost: 100}},
			key(candidate):   {MedianMS: 1, SpreadMS: 1, Plan: &engine.Plan{TotalCost: 5}},
		},
	}
	base := baselineFor(t, f)
	res := newVerifier(f).Verify(context.Background(), base, Candidate{SQL: candidate, Source: "llm"})

	if res.Verdict != VerdictRejected || res.Reason != ReasonResultMismatch {
		t.Fatalf("verdict=%s reason=%s, want rejected/results_differ", res.Verdict, res.Reason)
	}
	if !strings.Contains(res.Detail, "different data") {
		t.Fatalf("detail should explain the mismatch: %q", res.Detail)
	}
}

func TestVerifyAcceptsGenuineImprovement(t *testing.T) {
	candidate := `SELECT id, total_cents FROM orders WHERE created_at >= '2026-06-01'::timestamptz`
	f := &fakeEngine{
		measures: map[string]*engine.Measurement{
			key(originalSQL): {MedianMS: 100, SpreadMS: 3, Plan: &engine.Plan{TotalCost: 100}},
			key(candidate):   {MedianMS: 40, SpreadMS: 2, Plan: &engine.Plan{TotalCost: 50}},
		},
	}
	base := baselineFor(t, f)
	res := newVerifier(f).Verify(context.Background(), base, Candidate{SQL: candidate})

	if res.Verdict != VerdictAccepted {
		t.Fatalf("verdict=%s reason=%s detail=%s", res.Verdict, res.Reason, res.Detail)
	}
	if !res.ResultsIdentical {
		t.Fatal("acceptance requires proven identical results")
	}
	if res.ImprovementPct < 59 || res.ImprovementPct > 61 {
		t.Fatalf("improvement = %.1f%%, want ~60%%", res.ImprovementPct)
	}
	if res.SpeedupFactor < 2.4 || res.SpeedupFactor > 2.6 {
		t.Fatalf("speedup = %.2f, want ~2.5", res.SpeedupFactor)
	}
}

// A 5% gain when runs vary by 40ms is not a gain, it is a quiet minute on the
// machine.
func TestVerifyRejectsImprovementInsideMeasurementNoise(t *testing.T) {
	candidate := `SELECT id, total_cents FROM orders WHERE created_at >= '2026-06-01'::date`
	f := &fakeEngine{
		measures: map[string]*engine.Measurement{
			key(originalSQL): {MedianMS: 100, SpreadMS: 40, Plan: &engine.Plan{TotalCost: 100}},
			key(candidate):   {MedianMS: 80, SpreadMS: 35, Plan: &engine.Plan{TotalCost: 90}},
		},
	}
	base := baselineFor(t, f)
	res := newVerifier(f).Verify(context.Background(), base, Candidate{SQL: candidate})

	if res.Verdict != VerdictRejected || res.Reason != ReasonNotFaster {
		t.Fatalf("verdict=%s reason=%s, want rejection for noise", res.Verdict, res.Reason)
	}
	if !strings.Contains(res.Detail, "noise") {
		t.Fatalf("detail should name the noise: %q", res.Detail)
	}
}

func TestVerifyRejectsBelowThreshold(t *testing.T) {
	candidate := `SELECT id, total_cents FROM orders WHERE created_at >= '2026-06-01'::date`
	f := &fakeEngine{
		measures: map[string]*engine.Measurement{
			key(originalSQL): {MedianMS: 100, SpreadMS: 1, Plan: &engine.Plan{TotalCost: 100}},
			key(candidate):   {MedianMS: 96, SpreadMS: 1, Plan: &engine.Plan{TotalCost: 98}},
		},
	}
	base := baselineFor(t, f)
	res := newVerifier(f).Verify(context.Background(), base, Candidate{SQL: candidate})
	if res.Verdict != VerdictRejected || res.Reason != ReasonNotFaster {
		t.Fatalf("a 4%% gain must not clear a 10%% threshold: %+v", res)
	}
}

// The model has been known to answer a request for a SELECT with a DELETE.
func TestVerifyRejectsNonReadOnlyCandidate(t *testing.T) {
	f := &fakeEngine{}
	base := baselineFor(t, f)
	for _, sql := range []string{
		`DELETE FROM orders WHERE created_at < '2026-06-01'`,
		`WITH x AS (UPDATE orders SET total_cents = 0 RETURNING id) SELECT id, id FROM x`,
	} {
		res := newVerifier(f).Verify(context.Background(), base, Candidate{SQL: sql, Source: "llm"})
		if res.Verdict != VerdictRejected || res.Reason != ReasonNotReadOnly {
			t.Fatalf("Verify(%q) = %s/%s, want rejected/not_read_only", sql, res.Verdict, res.Reason)
		}
	}
}

func TestVerifyRejectsUnparseableCandidate(t *testing.T) {
	f := &fakeEngine{}
	base := baselineFor(t, f)
	res := newVerifier(f).Verify(context.Background(), base, Candidate{SQL: "SELECT FROM WHERE", Source: "llm"})
	if res.Verdict != VerdictRejected || res.Reason != ReasonUnparseable {
		t.Fatalf("got %s/%s", res.Verdict, res.Reason)
	}
}

// Re-proposing the original would otherwise produce a coin-flip "improvement"
// from timing noise alone.
func TestVerifyRejectsIdenticalCandidate(t *testing.T) {
	f := &fakeEngine{}
	base := baselineFor(t, f)
	restyled := "select   id, total_cents FROM orders\nWHERE created_at >= '2026-06-01'"
	res := newVerifier(f).Verify(context.Background(), base, Candidate{SQL: restyled})
	if res.Verdict != VerdictRejected || res.Reason != ReasonIdentical {
		t.Fatalf("got %s/%s", res.Verdict, res.Reason)
	}
}

func TestVerifyRejectsShapeChange(t *testing.T) {
	f := &fakeEngine{}
	base := baselineFor(t, f)
	res := newVerifier(f).Verify(context.Background(), base,
		Candidate{SQL: `SELECT id FROM orders WHERE created_at >= '2026-06-01'`})
	if res.Verdict != VerdictRejected || res.Reason != ReasonShapeMismatch {
		t.Fatalf("got %s/%s: %s", res.Verdict, res.Reason, res.Detail)
	}
}

// Ordering is part of the result identity only when the query promises one.
func TestChecksumOrderingFollowsTheQuery(t *testing.T) {
	f := &fakeEngine{}

	unordered, _ := sqlparse.Parse(`SELECT id FROM orders`)
	base, err := EstablishBaseline(context.Background(), f, unordered, 3)
	if err != nil {
		t.Fatal(err)
	}
	if base.Ordered {
		t.Fatal("a query with no ORDER BY must not have order hashed into its identity")
	}

	ordered, _ := sqlparse.Parse(`SELECT id FROM orders ORDER BY id`)
	base2, err := EstablishBaseline(context.Background(), f, ordered, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !base2.Ordered {
		t.Fatal("a query with ORDER BY promises an order, which must be part of its identity")
	}
}

func TestResultHashEquality(t *testing.T) {
	a := &engine.ResultHash{Hash: "x", RowCount: 5, Ordered: true}
	if !a.Equal(&engine.ResultHash{Hash: "x", RowCount: 5, Ordered: true}) {
		t.Fatal("identical hashes must compare equal")
	}
	// Comparing an ordered hash against an unordered one is meaningless and
	// must never report equality.
	if a.Equal(&engine.ResultHash{Hash: "x", RowCount: 5, Ordered: false}) {
		t.Fatal("ordering must be part of the comparison")
	}
	if a.Equal(&engine.ResultHash{Hash: "x", RowCount: 6, Ordered: true}) {
		t.Fatal("row count must be part of the comparison")
	}
	if a.Equal(nil) {
		t.Fatal("nil must never compare equal")
	}
}

func TestImprovementPct(t *testing.T) {
	if got := improvementPct(100, 40); got < 59.9 || got > 60.1 {
		t.Fatalf("improvementPct(100,40) = %f", got)
	}
	if got := improvementPct(100, 120); got > -19.9 || got < -20.1 {
		t.Fatalf("a slower candidate must report a negative improvement, got %f", got)
	}
	if got := improvementPct(0, 10); got != 0 {
		t.Fatalf("a zero baseline must not divide by zero, got %f", got)
	}
}
