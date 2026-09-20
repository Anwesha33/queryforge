package sqlparse

import (
	"errors"
	"strings"
	"testing"
)

// The refusals matter more than the acceptances: everything downstream executes
// the statement five times, so a write that reaches the engine is the worst
// failure this service has.
func TestParseMySQLRefusals(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want error
	}{
		{"delete", "DELETE FROM orders", ErrNotReadOnly},
		{"update", "UPDATE orders SET status='x'", ErrNotReadOnly},
		{"insert", "INSERT INTO orders (id) VALUES (1)", ErrNotReadOnly},
		{"insert select", "INSERT INTO archive SELECT * FROM orders", ErrNotReadOnly},
		{"ddl", "CREATE INDEX i ON orders (id)", ErrNotReadOnly},
		{"truncate", "TRUNCATE TABLE orders", ErrNotReadOnly},
		{"for update", "SELECT id FROM orders FOR UPDATE", ErrNotReadOnly},
		{"lock in share mode", "SELECT id FROM orders LOCK IN SHARE MODE", ErrNotReadOnly},
		{"into outfile", "SELECT id FROM orders INTO OUTFILE '/tmp/x'", ErrNotReadOnly},
		{"subquery for update", "SELECT * FROM (SELECT id FROM orders FOR UPDATE) z", ErrNotReadOnly},
		{"two statements", "SELECT 1; SELECT 2", ErrMultipleStatements},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseMySQL(tc.sql)
			if err == nil {
				t.Fatalf("expected a refusal, got none")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// A comment and a string literal both contain text that looks like a write.
// Matching SQL with regular expressions gets these wrong; a parser does not.
// INTO DUMPFILE is refused, but by the grammar rather than by our guard — the
// parser does not accept it at all. Recorded separately so that a future parser
// version that *does* accept it fails this test rather than silently widening
// what reaches the engine.
func TestParseMySQLRefusesDumpfileSomehow(t *testing.T) {
	if _, err := ParseMySQL("SELECT id FROM orders INTO DUMPFILE '/tmp/x'"); err == nil {
		t.Fatal("INTO DUMPFILE was accepted; it must be refused")
	}
}

func TestParseMySQLAcceptsLookalikes(t *testing.T) {
	cases := []string{
		"SELECT id FROM orders -- DELETE FROM orders",
		"SELECT id FROM orders /* UPDATE orders SET x=1 */",
		"SELECT id FROM orders WHERE note = 'DELETE FROM orders'",
		"SELECT `id` FROM `orders` WHERE `status` = 'new'",
		"SELECT id FROM orders LIMIT 10, 20",
		"SELECT id FROM orders ORDER BY id LIMIT 5",
	}
	for _, sql := range cases {
		if _, err := ParseMySQL(sql); err != nil {
			t.Errorf("ParseMySQL(%q) = %v, want accepted", sql, err)
		}
	}
}

func TestParseMySQLStructure(t *testing.T) {
	st, err := ParseMySQL("SELECT o.id, u.name AS who FROM orders o JOIN users u ON o.user_id = u.id ORDER BY o.id LIMIT 5")
	if err != nil {
		t.Fatal(err)
	}
	if st.Dialect != DialectMySQL {
		t.Errorf("dialect = %q", st.Dialect)
	}
	if !st.HasOrderBy || !st.HasLimit {
		t.Errorf("HasOrderBy=%v HasLimit=%v, want both true", st.HasOrderBy, st.HasLimit)
	}
	if st.SelectStar {
		t.Error("SelectStar = true, want false")
	}
	if got, want := len(st.OutputCols), 2; got != want {
		t.Fatalf("OutputCols = %v, want %d", st.OutputCols, want)
	}
	if st.OutputCols[1] != "who" {
		t.Errorf("OutputCols[1] = %q, want alias %q", st.OutputCols[1], "who")
	}
	names := map[string]string{}
	for _, tr := range st.Tables {
		names[strings.ToLower(tr.Name)] = tr.Alias
	}
	if names["orders"] != "o" || names["users"] != "u" {
		t.Errorf("tables/aliases = %v", names)
	}
}

// A subquery's ORDER BY says nothing about whether the statement promises an
// ordering, and the checksum comparison depends on getting this right.
func TestParseMySQLOrderByIsTopLevelOnly(t *testing.T) {
	st, err := ParseMySQL("SELECT id FROM (SELECT id FROM orders ORDER BY id) z")
	if err != nil {
		t.Fatal(err)
	}
	if st.HasOrderBy {
		t.Error("HasOrderBy = true for a subquery-only ORDER BY, want false")
	}
}

// A CTE is referenced exactly like a table; a schema lookup for one never
// succeeds, so it must not appear in the table list.
func TestParseMySQLExcludesCTEs(t *testing.T) {
	st, err := ParseMySQL("WITH recent AS (SELECT id FROM orders) SELECT r.id FROM recent r")
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range st.Tables {
		if strings.EqualFold(tr.Name, "recent") {
			t.Errorf("CTE %q leaked into Tables: %+v", tr.Name, st.Tables)
		}
	}
	if len(st.CTENames) != 1 || !strings.EqualFold(st.CTENames[0], "recent") {
		t.Errorf("CTENames = %v", st.CTENames)
	}
}

// Two runs of the same query shape differing only in literals must share a
// fingerprint, or measurement history is shattered across parameter values.
func TestParseMySQLFingerprintIgnoresLiterals(t *testing.T) {
	a, err := ParseMySQL("SELECT id FROM orders WHERE status = 'new'")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseMySQL("SELECT id FROM orders WHERE status = 'shipped'")
	if err != nil {
		t.Fatal(err)
	}
	if a.Fingerprint == "" || a.Fingerprint != b.Fingerprint {
		t.Errorf("fingerprints differ: %q vs %q", a.Fingerprint, b.Fingerprint)
	}
}
