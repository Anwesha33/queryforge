package engine

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// These run against a real MySQL. They are skipped unless MYSQL_TEST_DSN is
// set, so `go test ./...` stays hermetic:
//
//	MYSQL_TEST_DSN='queryforge:queryforge@tcp(127.0.0.1:3307)/shop' go test ./internal/engine -run MySQL
func mysqlTestEngine(t *testing.T) *MySQL {
	t.Helper()
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set")
	}
	eng, err := NewMySQL(context.Background(), dsn, Timings{
		StatementTimeout: 30 * time.Second, Runs: 3, WarmupRuns: 1,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(eng.Close)
	return eng
}

func TestMySQLIntrospect(t *testing.T) {
	eng := mysqlTestEngine(t)
	schema, err := eng.Introspect(context.Background(), []string{"orders", "users"})
	if err != nil {
		t.Fatal(err)
	}
	if len(schema.Tables) != 2 {
		t.Fatalf("got %d tables, want 2", len(schema.Tables))
	}
	orders, ok := schema.Table("orders")
	if !ok {
		t.Fatal("orders not found")
	}
	if orders.RowEstimate < 1_000_000 {
		t.Errorf("orders row estimate = %d, want a seeded table", orders.RowEstimate)
	}
	var sawCreatedAt bool
	for _, c := range orders.Columns {
		if c.Name == "created_at" {
			sawCreatedAt = true
		}
	}
	if !sawCreatedAt {
		t.Errorf("orders columns missing created_at: %+v", orders.Columns)
	}
	// Only the primary key should exist — the benchmark is deliberately
	// under-indexed, and an optimizer that starts with indexes proves nothing.
	for _, idx := range orders.Indexes {
		if !idx.IsPrimary {
			t.Errorf("unexpected pre-existing index on orders: %s", idx.Definition)
		}
	}
}

func TestMySQLExplainFindsFullScan(t *testing.T) {
	eng := mysqlTestEngine(t)
	plan, err := eng.Explain(context.Background(),
		"SELECT id FROM orders WHERE total_cents > 400000")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.SeqScanTables) == 0 {
		t.Errorf("expected a full scan on an unindexed filter; summary:\n%s", plan.Summary)
	}
	if plan.Summary == "" {
		t.Error("empty plan summary")
	}
}

func TestMySQLMeasure(t *testing.T) {
	eng := mysqlTestEngine(t)
	m, err := eng.Measure(context.Background(),
		"SELECT COUNT(*) FROM orders WHERE status = 'refunded'", 3)
	if err != nil {
		t.Fatal(err)
	}
	if m.TimedOut {
		t.Fatal("unexpectedly timed out")
	}
	if m.MedianMS <= 0 {
		t.Errorf("median = %v, want > 0", m.MedianMS)
	}
	if m.MaxMS < m.MinMS {
		t.Errorf("max %v < min %v", m.MaxMS, m.MinMS)
	}
	if m.SpreadMS != m.MaxMS-m.MinMS {
		t.Errorf("spread %v != max-min %v", m.SpreadMS, m.MaxMS-m.MinMS)
	}
}

// The checksum is the gate that decides whether a rewrite is equivalent, so
// these are the tests that matter most.
func TestMySQLChecksumEquivalence(t *testing.T) {
	eng := mysqlTestEngine(t)
	ctx := context.Background()

	const original = "SELECT id, status FROM orders WHERE DATE(created_at) = DATE(NOW() - INTERVAL 30 DAY)"
	const rewrite = "SELECT id, status FROM orders WHERE created_at >= DATE(NOW() - INTERVAL 30 DAY) " +
		"AND created_at < DATE(NOW() - INTERVAL 29 DAY)"

	a, err := eng.Checksum(ctx, original, false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := eng.Checksum(ctx, rewrite, false)
	if err != nil {
		t.Fatal(err)
	}
	if a.RowCount == 0 {
		t.Fatal("the fixture query returned no rows; the test proves nothing")
	}
	if !a.Equal(b) {
		t.Errorf("equivalent rewrite hashed differently:\n orig %+v\n new  %+v", a, b)
	}

	// A rewrite that changes results must not pass.
	c, err := eng.Checksum(ctx, original+" AND status = 'paid'", false)
	if err != nil {
		t.Fatal(err)
	}
	if a.Equal(c) {
		t.Error("a narrower query produced the same checksum")
	}
}

// Ordered and unordered hashes must not be comparable: a query with no ORDER BY
// makes no promise about order, and one with an ORDER BY does.
func TestMySQLChecksumOrderedFlagIsPartOfIdentity(t *testing.T) {
	eng := mysqlTestEngine(t)
	ctx := context.Background()
	const q = "SELECT id FROM orders WHERE status = 'refunded' ORDER BY id LIMIT 500"

	ordered, err := eng.Checksum(ctx, q, true)
	if err != nil {
		t.Fatal(err)
	}
	unordered, err := eng.Checksum(ctx, q, false)
	if err != nil {
		t.Fatal(err)
	}
	if ordered.Equal(unordered) {
		t.Error("ordered and unordered hashes compared equal; Ordered must be part of the identity")
	}
}

// CONCAT_WS skips NULLs, which would make ('x',NULL) and (NULL,'x') collide.
func TestMySQLChecksumDistinguishesNullPositions(t *testing.T) {
	eng := mysqlTestEngine(t)
	ctx := context.Background()

	a, err := eng.Checksum(ctx, "SELECT 'x' AS a, NULL AS b", false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := eng.Checksum(ctx, "SELECT NULL AS a, 'x' AS b", false)
	if err != nil {
		t.Fatal(err)
	}
	if a.Equal(b) {
		t.Error("('x',NULL) and (NULL,'x') hashed the same; the NULL sentinel is not working")
	}
}

// A large result set must not be silently truncated by group_concat_max_len.
func TestMySQLChecksumSurvivesLargeResultSet(t *testing.T) {
	eng := mysqlTestEngine(t)
	ctx := context.Background()

	h, err := eng.Checksum(ctx, "SELECT id FROM orders LIMIT 200000", false)
	if err != nil {
		t.Fatalf("large checksum failed: %v", err)
	}
	if h.RowCount != 200000 {
		t.Errorf("row count = %d, want 200000", h.RowCount)
	}
	// 200k rows of 32-hex MD5 is ~6.4MB, far past the 1024-byte default.
	if h.Hash == "" || h.Hash == "empty" {
		t.Errorf("hash = %q", h.Hash)
	}
}

func TestMySQLRefusesHypotheticalIndexes(t *testing.T) {
	eng := mysqlTestEngine(t)
	err := eng.WithHypotheticalIndex(context.Background(),
		"CREATE INDEX idx_orders_total ON orders (total_cents)",
		func(context.Context) error { return nil })
	if !errors.Is(err, ErrNoHypotheticalIndexes) {
		t.Fatalf("got %v, want ErrNoHypotheticalIndexes", err)
	}
	// And it must not have created anything.
	schema, err := eng.Introspect(context.Background(), []string{"orders"})
	if err != nil {
		t.Fatal(err)
	}
	orders, _ := schema.Table("orders")
	for _, idx := range orders.Indexes {
		if strings.Contains(strings.ToLower(idx.Name), "total") {
			t.Fatalf("an index was created despite the refusal: %s", idx.Definition)
		}
	}
}

// The parser proves a statement is read-only; this proves the database refuses
// a write even if the parser were wrong. Two independent checks, deliberately.
func TestMySQLReadOnlyTransactionRefusesWrites(t *testing.T) {
	eng := mysqlTestEngine(t)
	err := eng.readOnly(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO countries (code, name) VALUES ('ZZ','Nowhere')")
		return err
	})
	if err == nil {
		t.Fatal("a write succeeded inside a READ ONLY transaction")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "read only") &&
		!strings.Contains(strings.ToLower(err.Error()), "read-only") {
		t.Logf("write refused, but with an unexpected message: %v", err)
	}
}
