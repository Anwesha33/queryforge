package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres is the PostgreSQL implementation of Engine.
type Postgres struct {
	pool    *pgxpool.Pool
	timings Timings
}

func NewPostgres(ctx context.Context, dsn string, timings Timings) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 6
	cfg.MaxConnLifetime = 30 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Postgres{pool: pool, timings: timings}, nil
}

func (p *Postgres) Close()          { p.pool.Close() }
func (p *Postgres) Dialect() string { return "postgres" }

// ---------------------------------------------------------- introspection

const columnsSQL = `
SELECT c.relname,
       n.nspname,
       a.attname,
       format_type(a.atttypid, a.atttypmod) AS data_type,
       NOT a.attnotnull                     AS nullable,
       COALESCE(s.null_frac, 0)             AS null_frac,
       COALESCE(s.n_distinct, 0)            AS n_distinct
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_attribute a ON a.attrelid = c.oid
LEFT JOIN pg_stats s ON s.schemaname = n.nspname AND s.tablename = c.relname AND s.attname = a.attname
WHERE c.relname = ANY($1) AND a.attnum > 0 AND NOT a.attisdropped
  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
ORDER BY c.relname, a.attnum`

const indexesSQL = `
SELECT c.relname AS table_name,
       i.relname AS index_name,
       pg_get_indexdef(x.indexrelid) AS definition,
       x.indisunique,
       x.indisprimary,
       pg_relation_size(x.indexrelid) AS size_bytes,
       COALESCE(st.idx_scan, 0) AS scans
FROM pg_index x
JOIN pg_class c ON c.oid = x.indrelid
JOIN pg_class i ON i.oid = x.indexrelid
LEFT JOIN pg_stat_user_indexes st ON st.indexrelid = x.indexrelid
WHERE c.relname = ANY($1)
ORDER BY c.relname, i.relname`

const tableStatsSQL = `
SELECT c.relname,
       n.nspname,
       GREATEST(c.reltuples, 0)::bigint AS row_estimate,
       pg_total_relation_size(c.oid)    AS total_bytes,
       COALESCE(st.seq_scan, 0)         AS seq_scans,
       COALESCE(st.idx_scan, 0)         AS idx_scans
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_stat_user_tables st ON st.relid = c.oid
WHERE c.relname = ANY($1) AND c.relkind IN ('r', 'p', 'm')`

// Introspect gathers schema, indexes and planner statistics for the given
// tables in three queries rather than per table, because a join across five
// tables would otherwise cost fifteen round trips.
func (p *Postgres) Introspect(ctx context.Context, tables []string) (*Schema, error) {
	if len(tables) == 0 {
		return &Schema{}, nil
	}
	bare := make([]string, 0, len(tables))
	for _, t := range tables {
		// pg_class stores the bare relation name; the schema is a separate join.
		if i := strings.LastIndex(t, "."); i >= 0 {
			t = t[i+1:]
		}
		bare = append(bare, t)
	}

	byName := map[string]*Table{}

	rows, err := p.pool.Query(ctx, tableStatsSQL, bare)
	if err != nil {
		return nil, fmt.Errorf("table statistics: %w", err)
	}
	for rows.Next() {
		var t Table
		if err := rows.Scan(&t.Name, &t.Schema, &t.RowEstimate, &t.TotalBytes, &t.SeqScans, &t.IdxScans); err != nil {
			rows.Close()
			return nil, err
		}
		copyT := t
		byName[strings.ToLower(t.Name)] = &copyT
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = p.pool.Query(ctx, columnsSQL, bare)
	if err != nil {
		return nil, fmt.Errorf("columns: %w", err)
	}
	for rows.Next() {
		var table, schema string
		var col Column
		if err := rows.Scan(&table, &schema, &col.Name, &col.Type, &col.Nullable,
			&col.NullFraction, &col.DistinctValues); err != nil {
			rows.Close()
			return nil, err
		}
		t, ok := byName[strings.ToLower(table)]
		if !ok {
			t = &Table{Name: table, Schema: schema}
			byName[strings.ToLower(table)] = t
		}
		t.Columns = append(t.Columns, col)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = p.pool.Query(ctx, indexesSQL, bare)
	if err != nil {
		return nil, fmt.Errorf("indexes: %w", err)
	}
	for rows.Next() {
		var table string
		var idx Index
		if err := rows.Scan(&table, &idx.Name, &idx.Definition, &idx.IsUnique,
			&idx.IsPrimary, &idx.SizeBytes, &idx.Scans); err != nil {
			rows.Close()
			return nil, err
		}
		idx.Columns = indexColumns(idx.Definition)
		if t, ok := byName[strings.ToLower(table)]; ok {
			t.Indexes = append(t.Indexes, idx)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := &Schema{}
	for _, t := range byName {
		out.Tables = append(out.Tables, *t)
	}
	sort.Slice(out.Tables, func(i, j int) bool { return out.Tables[i].Name < out.Tables[j].Name })
	return out, nil
}

// indexColumns pulls the column list out of an index definition. Parsing the
// definition is cruder than reading pg_index.indkey, but it handles expression
// indexes and INCLUDE clauses without a second round trip.
func indexColumns(def string) []string {
	open := strings.Index(def, "(")
	if open < 0 {
		return nil
	}
	depth := 0
	end := -1
	for i := open; i < len(def); i++ {
		switch def[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return nil
	}
	var cols []string
	for _, part := range splitTopLevel(def[open+1 : end]) {
		part = strings.TrimSpace(part)
		for _, suffix := range []string{" DESC", " ASC", " NULLS FIRST", " NULLS LAST"} {
			part = strings.TrimSuffix(strings.TrimSpace(part), suffix)
		}
		if part != "" {
			cols = append(cols, strings.Trim(part, `"`))
		}
	}
	return cols
}

// splitTopLevel splits on commas that are not inside parentheses, so that
// `lower(email), (a + b)` yields two entries rather than four.
func splitTopLevel(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// ---------------------------------------------------------------- planning

// Explain produces a plan without executing the query.
func (p *Postgres) Explain(ctx context.Context, sql string) (*Plan, error) {
	var raw string
	err := p.readOnly(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON, COSTS, VERBOSE false) "+sql).Scan(&raw)
	})
	if err != nil {
		return nil, fmt.Errorf("explain: %w", err)
	}
	return parsePlan(raw)
}

// Measure runs EXPLAIN (ANALYZE, BUFFERS) several times and reports the median.
//
// ANALYZE actually executes the query. That is safe here only because the
// statement has already been proven read-only by the parser and because the
// whole thing runs inside a READ ONLY transaction, which the server enforces
// regardless of what the parser concluded. Belt and braces, deliberately: one
// check is in our code and the other is in the database.
func (p *Postgres) Measure(ctx context.Context, sql string, runs int) (*Measurement, error) {
	if runs <= 0 {
		runs = p.timings.Runs
	}
	m := &Measurement{Runs: runs}
	durations := make([]float64, 0, runs)

	total := runs + p.timings.WarmupRuns
	for i := 0; i < total; i++ {
		var raw string
		err := p.readOnly(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				"EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON, TIMING, COSTS) "+sql).Scan(&raw)
		})
		if err != nil {
			if isTimeout(err) {
				m.TimedOut = true
				return m, nil
			}
			return nil, fmt.Errorf("explain analyze: %w", err)
		}

		plan, parseErr := parsePlan(raw)
		if parseErr != nil {
			return nil, parseErr
		}
		exec, planning, stats := planExecutionStats(raw)

		// Warm-up runs are executed and discarded: the first run pays for cold
		// caches and would make every later comparison look better than it is.
		if i < p.timings.WarmupRuns {
			continue
		}
		durations = append(durations, exec)
		m.Plan = plan
		m.PlanningMS = planning
		m.ActualRows = stats.rows
		m.SharedHit = stats.sharedHit
		m.SharedRead = stats.sharedRead
		m.TempWritten = stats.tempWritten
	}

	if len(durations) == 0 {
		return m, nil
	}
	sorted := append([]float64(nil), durations...)
	sort.Float64s(sorted)
	m.MedianMS = median(sorted)
	m.MinMS = sorted[0]
	m.MaxMS = sorted[len(sorted)-1]
	m.SpreadMS = m.MaxMS - m.MinMS
	return m, nil
}

func median(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// --------------------------------------------------------------- checksums

// Checksum hashes a query's result set so two queries can be compared for
// equivalence by execution rather than by reasoning about their SQL.
//
// This is the load-bearing safety mechanism of the whole service: an LLM rewrite
// is only ever accepted if the rewritten query returns byte-identical results.
//
// ordered controls whether row order is part of the identity. A query with a
// top-level ORDER BY promises an order, so the order is hashed; a query without
// one makes no such promise, and hashing in arrival order would report two
// equivalent queries as different simply because the planner chose a different
// join order.
func (p *Postgres) Checksum(ctx context.Context, sql string, ordered bool) (*ResultHash, error) {
	inner := "SELECT md5(t::text) AS h FROM (" + sql + ") t"
	var wrapped string
	if ordered {
		// row_number() OVER () preserves the order rows arrive in, which for a
		// subquery with ORDER BY is that order.
		wrapped = `SELECT COALESCE(md5(string_agg(h, '' ORDER BY rn)), 'empty'), count(*)
		           FROM (SELECT row_number() OVER () AS rn, h FROM (` + inner + `) x) y`
	} else {
		wrapped = `SELECT COALESCE(md5(string_agg(h, '' ORDER BY h)), 'empty'), count(*)
		           FROM (` + inner + `) z`
	}

	out := &ResultHash{Ordered: ordered}
	err := p.readOnly(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, wrapped).Scan(&out.Hash, &out.RowCount)
	})
	if err != nil {
		return nil, fmt.Errorf("checksum: %w", err)
	}
	return out, nil
}

// -------------------------------------------------------- index experiments

// WithHypotheticalIndex creates an index, runs fn, and always rolls back.
//
// PostgreSQL has transactional DDL, so `BEGIN; CREATE INDEX ...; ROLLBACK;`
// leaves no trace — which makes it possible to measure what an index would
// actually do rather than guess from the planner's cost model. This is the
// single biggest reason this service targets Postgres: MySQL commits DDL
// implicitly, so the same experiment there would leave a real index behind on
// the user's database.
//
// The index is built without CONCURRENTLY, which takes an ACCESS EXCLUSIVE lock
// on the table for the duration. That is acceptable against a staging replica
// and is not acceptable against production, which is why the README says to
// point this at a replica.
func (p *Postgres) WithHypotheticalIndex(ctx context.Context, indexDDL string, fn func(context.Context) error) error {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	// Rollback unconditionally. Even on the success path: the whole point is
	// that the index must not survive the experiment.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d",
		p.timings.StatementTimeout.Milliseconds()*4)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, indexDDL); err != nil {
		return fmt.Errorf("create hypothetical index: %w", err)
	}

	// The measurement must run on this same connection, inside this same
	// transaction, or it will not see the uncommitted index.
	return fn(withTx(ctx, tx))
}

// txKey carries the experiment's transaction so Measure can run inside it.
type txKey struct{}

func withTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

// MeasureInTx times a query on the transaction carried by ctx, which is how an
// index experiment measures against an index that has not been committed.
func (p *Postgres) MeasureInTx(ctx context.Context, sql string, runs int) (*Measurement, error) {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	if !ok {
		return p.Measure(ctx, sql, runs)
	}
	if runs <= 0 {
		runs = p.timings.Runs
	}

	m := &Measurement{Runs: runs}
	durations := make([]float64, 0, runs)
	for i := 0; i < runs+p.timings.WarmupRuns; i++ {
		var raw string
		if err := tx.QueryRow(ctx,
			"EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON, TIMING, COSTS) "+sql).Scan(&raw); err != nil {
			if isTimeout(err) {
				m.TimedOut = true
				return m, nil
			}
			return nil, err
		}
		plan, err := parsePlan(raw)
		if err != nil {
			return nil, err
		}
		exec, planning, stats := planExecutionStats(raw)
		if i < p.timings.WarmupRuns {
			continue
		}
		durations = append(durations, exec)
		m.Plan = plan
		m.PlanningMS = planning
		m.ActualRows = stats.rows
		m.SharedHit = stats.sharedHit
		m.SharedRead = stats.sharedRead
	}
	if len(durations) == 0 {
		return m, nil
	}
	sort.Float64s(durations)
	m.MedianMS = median(durations)
	m.MinMS = durations[0]
	m.MaxMS = durations[len(durations)-1]
	m.SpreadMS = m.MaxMS - m.MinMS
	return m, nil
}

// ------------------------------------------------------------------ helpers

// readOnly runs fn inside a READ ONLY transaction with a statement timeout.
//
// The READ ONLY flag is the database's own guarantee, independent of the
// parser's opinion: even if a statement slipped past the parse-time check, the
// server refuses to write.
func (p *Postgres) readOnly(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d",
		p.timings.StatementTimeout.Milliseconds())); err != nil {
		return err
	}
	return fn(ctx, tx)
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "statement timeout") ||
		strings.Contains(msg, "canceling statement due to")
}

type execStats struct {
	rows        float64
	sharedHit   int64
	sharedRead  int64
	tempWritten int64
}

// planExecutionStats pulls the numbers out of an EXPLAIN ANALYZE JSON document.
func planExecutionStats(raw string) (execMS, planningMS float64, stats execStats) {
	var docs []map[string]any
	if err := json.Unmarshal([]byte(raw), &docs); err != nil || len(docs) == 0 {
		return 0, 0, stats
	}
	doc := docs[0]
	execMS = floatField(doc, "Execution Time")
	planningMS = floatField(doc, "Planning Time")

	plan, _ := doc["Plan"].(map[string]any)
	if plan == nil {
		return execMS, planningMS, stats
	}
	stats.rows = floatField(plan, "Actual Rows")
	// Buffer counts on the root node are cumulative over the whole tree.
	stats.sharedHit = int64(floatField(plan, "Shared Hit Blocks"))
	stats.sharedRead = int64(floatField(plan, "Shared Read Blocks"))
	stats.tempWritten = int64(floatField(plan, "Temp Written Blocks"))
	return execMS, planningMS, stats
}

func parsePlan(raw string) (*Plan, error) {
	var docs []map[string]any
	if err := json.Unmarshal([]byte(raw), &docs); err != nil {
		return nil, fmt.Errorf("decode plan: %w", err)
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("empty plan")
	}
	root, _ := docs[0]["Plan"].(map[string]any)
	if root == nil {
		return nil, fmt.Errorf("plan document has no Plan node")
	}

	p := &Plan{
		Raw:           raw,
		TotalCost:     floatField(root, "Total Cost"),
		StartupCost:   floatField(root, "Startup Cost"),
		EstimatedRows: floatField(root, "Plan Rows"),
	}
	var b strings.Builder
	walkPlanNodes(root, 0, &b, p)
	p.Summary = strings.TrimRight(b.String(), "\n")
	return p, nil
}

// walkPlanNodes renders the plan the way EXPLAIN's text format does, and
// collects the facts the rule engine cares about on the way down.
func walkPlanNodes(node map[string]any, depth int, b *strings.Builder, p *Plan) {
	nodeType, _ := node["Node Type"].(string)
	p.NodeTypes = append(p.NodeTypes, nodeType)

	relation, _ := node["Relation Name"].(string)
	if nodeType == "Seq Scan" && relation != "" {
		p.SeqScanTables = append(p.SeqScanTables, relation)
	}

	indent := strings.Repeat("  ", depth)
	fmt.Fprintf(b, "%s-> %s", indent, nodeType)
	if relation != "" {
		fmt.Fprintf(b, " on %s", relation)
	}
	if idx, ok := node["Index Name"].(string); ok && idx != "" {
		fmt.Fprintf(b, " using %s", idx)
	}
	fmt.Fprintf(b, "  (cost=%.2f..%.2f rows=%.0f",
		floatField(node, "Startup Cost"), floatField(node, "Total Cost"), floatField(node, "Plan Rows"))
	if actual, ok := node["Actual Rows"]; ok {
		fmt.Fprintf(b, " actual_rows=%.0f loops=%.0f time=%.3fms",
			toFloat(actual), floatField(node, "Actual Loops"), floatField(node, "Actual Total Time"))
	}
	b.WriteString(")\n")

	for _, key := range []string{"Filter", "Index Cond", "Recheck Cond", "Hash Cond", "Join Filter", "Sort Key"} {
		if v, ok := node[key]; ok {
			fmt.Fprintf(b, "%s     %s: %v\n", indent, key, v)
		}
	}
	if v, ok := node["Rows Removed by Filter"]; ok {
		fmt.Fprintf(b, "%s     Rows Removed by Filter: %.0f\n", indent, toFloat(v))
	}
	if v, ok := node["Sort Method"]; ok {
		fmt.Fprintf(b, "%s     Sort Method: %v", indent, v)
		if sp, ok := node["Sort Space Used"]; ok {
			fmt.Fprintf(b, " (%.0f kB %v)", toFloat(sp), node["Sort Space Type"])
		}
		b.WriteString("\n")
	}

	children, _ := node["Plans"].([]any)
	for _, c := range children {
		if child, ok := c.(map[string]any); ok {
			walkPlanNodes(child, depth+1, b, p)
		}
	}
}

func floatField(m map[string]any, key string) float64 {
	if v, ok := m[key]; ok {
		return toFloat(v)
	}
	return 0
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}
