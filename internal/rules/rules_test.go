package rules

import (
	"strings"
	"testing"

	"github.com/Anwesha33/queryforge/internal/engine"
	"github.com/Anwesha33/queryforge/internal/sqlparse"
)

func schemaFixture() *engine.Schema {
	return &engine.Schema{Tables: []engine.Table{
		{
			Name: "orders", RowEstimate: 2_100_000,
			Columns: []engine.Column{
				{Name: "id", Type: "bigint"},
				{Name: "user_id", Type: "bigint"},
				{Name: "created_at", Type: "timestamp with time zone"},
				{Name: "status", Type: "text", DistinctValues: 2},
				{Name: "total_cents", Type: "bigint"},
			},
			Indexes: []engine.Index{
				{Name: "orders_pkey", Columns: []string{"id"}, IsPrimary: true, IsUnique: true},
			},
		},
		{
			Name: "users", RowEstimate: 400_000,
			Columns: []engine.Column{
				{Name: "id", Type: "bigint"},
				{Name: "country", Type: "text", DistinctValues: 90},
				{Name: "email", Type: "text"},
			},
			Indexes: []engine.Index{
				{Name: "users_pkey", Columns: []string{"id"}, IsPrimary: true, IsUnique: true},
			},
		},
		{
			Name: "currencies", RowEstimate: 180,
			Columns: []engine.Column{{Name: "code", Type: "text"}},
		},
	}}
}

func analyze(t *testing.T, sql string) *Analysis {
	t.Helper()
	st, err := sqlparse.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	return Analyze(st, schemaFixture(), nil)
}

func find(a *Analysis, code string) (Finding, bool) {
	for _, f := range a.Findings {
		if f.Code == code {
			return f, true
		}
	}
	return Finding{}, false
}

// The query from the brief.
func TestDateFunctionPredicateIsDetectedAndRewritten(t *testing.T) {
	a := analyze(t, `SELECT o.id, u.email
	                 FROM orders o
	                 JOIN users u ON o.user_id = u.id
	                 WHERE DATE(o.created_at) = '2026-09-01' AND u.country = 'IN'`)

	f, ok := find(a, "non-sargable-predicate")
	if !ok {
		t.Fatalf("non-sargable predicate not detected; findings: %+v", a.Findings)
	}
	if f.Severity != SeverityHigh || f.Table != "orders" {
		t.Fatalf("finding = %+v", f)
	}
	if f.Rewrite == "" {
		t.Fatal("a deterministic rewrite should have been generated")
	}
	// The rewrite must be a half-open range: BETWEEN would wrongly include
	// midnight of the following day.
	lower := strings.ToLower(f.Rewrite)
	if !strings.Contains(lower, ">=") || !strings.Contains(lower, "<") {
		t.Fatalf("rewrite is not a range: %s", f.Rewrite)
	}
	if strings.Contains(lower, "between") {
		t.Fatalf("rewrite used BETWEEN, which is inclusive at both ends: %s", f.Rewrite)
	}
	if strings.Contains(lower, "date(") {
		t.Fatalf("rewrite still wraps the column in DATE(): %s", f.Rewrite)
	}
	// It must still be valid SQL.
	if _, err := sqlparse.Parse(f.Rewrite); err != nil {
		t.Fatalf("rewrite does not parse: %v\n%s", err, f.Rewrite)
	}
}

func TestMissingIndexProposedForFilterAndJoinColumns(t *testing.T) {
	a := analyze(t, `SELECT o.id FROM orders o JOIN users u ON o.user_id = u.id WHERE u.country = 'IN'`)

	var indexed []string
	for _, f := range a.Findings {
		if f.Code == "missing-index" {
			indexed = append(indexed, strings.Join(f.Columns, ","))
			if f.SuggestedIndex == "" {
				t.Fatalf("missing-index finding has no DDL: %+v", f)
			}
			if !strings.HasPrefix(f.SuggestedIndex, "CREATE INDEX") {
				t.Fatalf("suggested index is not a CREATE INDEX: %q", f.SuggestedIndex)
			}
		}
	}
	joined := strings.Join(indexed, " ")
	if !strings.Contains(joined, "user_id") || !strings.Contains(joined, "country") {
		t.Fatalf("expected indexes for the join key and the filter, got %v", indexed)
	}
}

// The primary key already leads with id, so no index should be proposed for it.
func TestNoIndexProposedWhenOneAlreadyLeads(t *testing.T) {
	a := analyze(t, `SELECT id FROM orders WHERE id = 42`)
	for _, f := range a.Findings {
		if f.Code == "missing-index" {
			t.Fatalf("proposed a redundant index: %+v", f)
		}
	}
}

// Indexing a 180-row lookup table is never the right answer.
func TestNoIndexProposedForTinyTable(t *testing.T) {
	a := analyze(t, `SELECT code FROM currencies WHERE code = 'INR'`)
	if f, ok := find(a, "missing-index"); ok {
		t.Fatalf("proposed an index on a tiny table: %+v", f)
	}
}

// A column with two distinct values gets a warning instead of an index.
func TestLowSelectivityColumnIsNotIndexed(t *testing.T) {
	a := analyze(t, `SELECT id FROM orders WHERE status = 'paid'`)
	if _, ok := find(a, "low-selectivity-column"); !ok {
		t.Fatalf("expected a low-selectivity finding; got %+v", a.Findings)
	}
	for _, f := range a.Findings {
		if f.Code == "missing-index" && contains(f.Columns, "status") {
			t.Fatalf("proposed an index on a two-value column: %+v", f)
		}
	}
}

func TestLeadingWildcardLike(t *testing.T) {
	a := analyze(t, `SELECT id FROM users WHERE email LIKE '%@example.com'`)
	if f, ok := find(a, "leading-wildcard-like"); !ok || f.Severity != SeverityHigh {
		t.Fatalf("leading wildcard not detected: %+v", a.Findings)
	}
	// A trailing-only wildcard is fine and must not be flagged.
	b := analyze(t, `SELECT id FROM users WHERE email LIKE 'anwesha%'`)
	if _, ok := find(b, "leading-wildcard-like"); ok {
		t.Fatal("a prefix pattern is index-friendly and must not be flagged")
	}
}

func TestSelectStarAndLimitWithoutOrder(t *testing.T) {
	a := analyze(t, `SELECT * FROM orders LIMIT 10`)
	if _, ok := find(a, "select-star"); !ok {
		t.Fatal("SELECT * not flagged")
	}
	if _, ok := find(a, "limit-without-order-by"); !ok {
		t.Fatal("LIMIT without ORDER BY not flagged")
	}
	b := analyze(t, `SELECT id FROM orders ORDER BY id LIMIT 10`)
	if _, ok := find(b, "limit-without-order-by"); ok {
		t.Fatal("false positive: LIMIT with ORDER BY is fine")
	}
}

func TestPredicateExtraction(t *testing.T) {
	a := analyze(t, `SELECT o.id FROM orders o JOIN users u ON o.user_id = u.id
	                 WHERE u.country = 'IN' AND o.total_cents > 1000`)

	var joins, filters int
	for _, p := range a.Predicates {
		if p.IsJoin {
			joins++
		} else {
			filters++
		}
	}
	if joins != 2 {
		t.Fatalf("a join predicate should register both sides, got %d: %+v", joins, a.Predicates)
	}
	if filters < 2 {
		t.Fatalf("expected two filter predicates, got %d: %+v", filters, a.Predicates)
	}

	// A literal on the left must flip the operator, not be dropped.
	b := analyze(t, `SELECT id FROM orders WHERE 1000 < total_cents`)
	for _, p := range b.Predicates {
		if p.Column == "total_cents" && p.Operator != ">" {
			t.Fatalf("operator not flipped for a left-hand literal: %+v", p)
		}
	}
}

func TestCastAlsoBreaksSargability(t *testing.T) {
	a := analyze(t, `SELECT id FROM orders WHERE created_at::date = '2026-09-01'`)
	f, ok := find(a, "non-sargable-predicate")
	if !ok {
		t.Fatalf("cast on a column not detected: %+v", a.Findings)
	}
	if !strings.Contains(strings.ToLower(f.Title), "cast") {
		t.Fatalf("finding should name the cast: %q", f.Title)
	}
}

func TestPlanFindings(t *testing.T) {
	st, _ := sqlparse.Parse(`SELECT id FROM orders WHERE total_cents > 1`)
	m := &engine.Measurement{
		MedianMS: 820, ActualRows: 1_200_000, SharedRead: 90000, TempWritten: 4096,
		Plan: &engine.Plan{EstimatedRows: 1000, SeqScanTables: []string{"orders"}},
	}
	a := Analyze(st, schemaFixture(), m)

	if _, ok := find(a, "seq-scan-large-table"); !ok {
		t.Fatalf("sequential scan on a 2.1M row table not flagged: %+v", a.Findings)
	}
	if _, ok := find(a, "row-estimate-off"); !ok {
		t.Fatal("a 1200x row misestimate was not flagged")
	}
	if _, ok := find(a, "sort-spilled-to-disk"); !ok {
		t.Fatal("temp file spill not flagged")
	}
}

// Sequentially scanning a small table is the correct plan and flagging it
// teaches people to ignore the tool.
func TestSmallTableSeqScanIsNotFlagged(t *testing.T) {
	st, _ := sqlparse.Parse(`SELECT code FROM currencies`)
	m := &engine.Measurement{MedianMS: 1, Plan: &engine.Plan{SeqScanTables: []string{"currencies"}}}
	a := Analyze(st, schemaFixture(), m)
	if _, ok := find(a, "seq-scan-large-table"); ok {
		t.Fatal("flagged a sequential scan on a 180-row table")
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if strings.EqualFold(x, want) {
			return true
		}
	}
	return false
}
