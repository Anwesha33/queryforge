package sqlparse

import (
	"errors"
	"strings"
	"testing"
)

func TestParseExtractsStructure(t *testing.T) {
	st, err := Parse(`SELECT o.id, u.email AS addr
	                  FROM orders o
	                  JOIN users u ON o.user_id = u.id
	                  WHERE u.country = 'IN'
	                  ORDER BY o.id LIMIT 10`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(st.Tables) != 2 {
		t.Fatalf("tables = %+v, want orders and users", st.Tables)
	}
	if st.Tables[0].Qualified() != "orders" || st.Tables[0].Alias != "o" {
		t.Fatalf("first table = %+v", st.Tables[0])
	}
	if got := st.OutputCols; len(got) != 2 || got[0] != "id" || got[1] != "addr" {
		t.Fatalf("output columns = %v", got)
	}
	if !st.HasLimit || !st.HasOrderBy || st.SelectStar {
		t.Fatalf("flags wrong: limit=%v order=%v star=%v", st.HasLimit, st.HasOrderBy, st.SelectStar)
	}
	if st.Fingerprint == "" || !strings.Contains(st.Normalized, "$1") {
		t.Fatalf("normalisation failed: fp=%q normalized=%q", st.Fingerprint, st.Normalized)
	}
}

// Two statements differing only in their literals must share a fingerprint —
// that is what makes historical timings for "the same query" comparable.
func TestFingerprintIgnoresLiterals(t *testing.T) {
	a, _ := Parse(`SELECT * FROM orders WHERE user_id = 1`)
	b, _ := Parse(`SELECT * FROM orders WHERE user_id = 99999`)
	c, _ := Parse(`SELECT * FROM orders WHERE seller_id = 1`)
	if a.Fingerprint != b.Fingerprint {
		t.Fatalf("literals changed the fingerprint: %s vs %s", a.Fingerprint, b.Fingerprint)
	}
	if a.Fingerprint == c.Fingerprint {
		t.Fatalf("a different column must change the fingerprint")
	}
}

// Everything in this test is a statement that a regular-expression check would
// wave through or reject wrongly.
func TestParseRejectsAnythingThatCanWrite(t *testing.T) {
	cases := []string{
		`UPDATE orders SET total = 0`,
		`DELETE FROM orders`,
		`INSERT INTO orders (id) VALUES (1)`,
		`DROP TABLE orders`,
		`TRUNCATE orders`,
		`COPY orders TO '/tmp/out.csv'`,
		// Parses as a SelectStmt and deletes every row.
		`WITH gone AS (DELETE FROM orders RETURNING *) SELECT * FROM gone`,
		// Creates a table.
		`SELECT * INTO backup FROM orders`,
		// Takes row locks.
		`SELECT * FROM orders FOR UPDATE`,
	}
	for _, sql := range cases {
		if _, err := Parse(sql); !errors.Is(err, ErrNotReadOnly) {
			t.Fatalf("Parse(%q) error = %v, want ErrNotReadOnly", sql, err)
		}
	}
}

func TestParseRejectsMultipleStatements(t *testing.T) {
	_, err := Parse(`SELECT 1; DROP TABLE orders`)
	if !errors.Is(err, ErrMultipleStatements) {
		t.Fatalf("error = %v, want ErrMultipleStatements", err)
	}
}

// A comment mentioning DELETE, or a literal containing a semicolon, breaks a
// regex-based check and must not break this one.
func TestParseAcceptsInnocentLookalikes(t *testing.T) {
	cases := []string{
		`SELECT id FROM orders -- DELETE FROM orders`,
		`SELECT id FROM orders WHERE note = 'drop table orders;'`,
		`/* UPDATE */ SELECT id FROM orders`,
	}
	for _, sql := range cases {
		if _, err := Parse(sql); err != nil {
			t.Fatalf("Parse(%q) = %v, want success", sql, err)
		}
	}
}

func TestParseFindsTablesInSubqueriesAndIgnoresCTEs(t *testing.T) {
	st, err := Parse(`
		WITH recent AS (SELECT id FROM payments WHERE created_at > now())
		SELECT o.id FROM orders o
		WHERE o.id IN (SELECT id FROM recent)
		  AND o.user_id IN (SELECT id FROM users WHERE country = 'IN')`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	names := map[string]bool{}
	for _, tref := range st.Tables {
		names[tref.Qualified()] = true
	}
	for _, want := range []string{"orders", "users", "payments"} {
		if !names[want] {
			t.Fatalf("missing table %q in %v", want, names)
		}
	}
	if names["recent"] {
		t.Fatalf("a CTE name was reported as a table: %v", names)
	}
}

func TestSelectStarDetected(t *testing.T) {
	st, _ := Parse(`SELECT * FROM orders`)
	if !st.SelectStar {
		t.Fatal("SELECT * not detected")
	}
	st2, _ := Parse(`SELECT o.* FROM orders o`)
	if !st2.SelectStar {
		t.Fatal("qualified star not detected")
	}
	st3, _ := Parse(`SELECT id FROM orders`)
	if st3.SelectStar {
		t.Fatal("false positive on an explicit column list")
	}
}

func TestSameShapeAs(t *testing.T) {
	orig, _ := Parse(`SELECT o.id, o.total FROM orders o JOIN users u ON o.user_id = u.id WHERE u.country = 'IN'`)

	ok, _ := Parse(`SELECT o.id, o.total FROM orders o JOIN users u ON o.user_id = u.id WHERE u.country = 'IN' AND o.total > 0`)
	if err := orig.SameShapeAs(ok); err != nil {
		t.Fatalf("a legitimate rewrite was rejected: %v", err)
	}

	fewerCols, _ := Parse(`SELECT o.id FROM orders o JOIN users u ON o.user_id = u.id`)
	if err := orig.SameShapeAs(fewerCols); err == nil {
		t.Fatal("a different column count must be rejected")
	}

	newTable, _ := Parse(`SELECT o.id, o.total FROM orders o JOIN users u ON o.user_id = u.id JOIN refunds r ON r.order_id = o.id`)
	if err := orig.SameShapeAs(newTable); err == nil || !strings.Contains(err.Error(), "refunds") {
		t.Fatalf("a candidate reading a new table must be rejected, got %v", err)
	}

	droppedTable, _ := Parse(`SELECT o.id, o.total FROM orders o`)
	if err := orig.SameShapeAs(droppedTable); err == nil {
		t.Fatal("a dropped table must be flagged for result verification")
	}
}

func TestParseEmptyAndGarbage(t *testing.T) {
	if _, err := Parse("   "); err == nil {
		t.Fatal("empty input must fail")
	}
	if _, err := Parse("this is not sql"); err == nil {
		t.Fatal("garbage must fail to parse")
	}
}
