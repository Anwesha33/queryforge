package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql" // database/sql driver, registered as "mysql"
)

// ErrNoHypotheticalIndexes is returned by engines that cannot build an index
// and throw it away again.
//
// It is a distinct error because it is a *capability* statement, not a failure:
// the caller degrades to reporting an unverified candidate rather than
// retrying. MySQL commits DDL implicitly, so `CREATE INDEX ... ROLLBACK` — the
// mechanism that makes Postgres index advice a measurement — leaves a real
// index on the user's database. There is no safe equivalent, and pretending
// otherwise by falling back to the planner's cost estimate would put a guess
// where this service promises a measurement.
var ErrNoHypotheticalIndexes = errors.New("this engine cannot measure hypothetical indexes")

// MySQL implements Engine against MySQL 8.0+.
//
// Two things behave differently from the Postgres engine and both are recorded
// in the report rather than smoothed over:
//
//  1. Timing is wall-clock around a fully drained result set, because MySQL has
//     no equivalent of `EXPLAIN (ANALYZE, BUFFERS)` reporting server-side
//     execution time in a machine-readable form. That means client and network
//     time are inside the number. It is consistent between the baseline and the
//     candidate, so a *comparison* remains meaningful even though the absolute
//     figure is not directly comparable to a Postgres one.
//  2. Index experiments are unavailable. See ErrNoHypotheticalIndexes.
type MySQL struct {
	db      *sql.DB
	timings Timings
}

// NewMySQL opens a connection pool against the database under optimisation.
func NewMySQL(ctx context.Context, dsn string, timings Timings) (*MySQL, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	// Deliberately small. Every connection here runs real queries against a
	// database somebody else depends on, and an optimiser that saturates the
	// database it is measuring is measuring itself.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(time.Hour)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	return &MySQL{db: db, timings: timings}, nil
}

func (m *MySQL) Close()          { m.db.Close() }
func (m *MySQL) Dialect() string { return "mysql" }

// readOnly runs fn inside a READ ONLY transaction.
//
// The parser has already proven the statement is a read-only SELECT. This is
// the second, independent check: the server refuses a write inside a READ ONLY
// transaction regardless of what our parser concluded. One check in our code,
// one in the database — the same pairing the Postgres engine uses, because the
// operation being guarded (executing a caller-supplied statement five times) is
// the one with the worst blast radius in the service.
func (m *MySQL) readOnly(ctx context.Context, fn func(context.Context, *sql.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, m.timings.StatementTimeout)
	defer cancel()

	tx, err := m.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// max_execution_time is MySQL's own guard, in milliseconds, and applies to
	// SELECT only. The context deadline covers everything else.
	ms := m.timings.StatementTimeout.Milliseconds()
	if ms > 0 {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET SESSION max_execution_time = %d", ms)); err != nil {
			return fmt.Errorf("set max_execution_time: %w", err)
		}
	}
	if err := fn(ctx, tx); err != nil {
		return err
	}
	return tx.Rollback()
}

// ------------------------------------------------------------- introspection

const mysqlTableQuery = `
SELECT t.TABLE_SCHEMA, t.TABLE_NAME,
       COALESCE(t.TABLE_ROWS, 0),
       COALESCE(t.DATA_LENGTH, 0) + COALESCE(t.INDEX_LENGTH, 0)
FROM information_schema.TABLES t
WHERE t.TABLE_SCHEMA = DATABASE() AND t.TABLE_NAME IN (%s)`

const mysqlColumnQuery = `
SELECT c.TABLE_NAME, c.COLUMN_NAME, c.COLUMN_TYPE, c.IS_NULLABLE
FROM information_schema.COLUMNS c
WHERE c.TABLE_SCHEMA = DATABASE() AND c.TABLE_NAME IN (%s)
ORDER BY c.TABLE_NAME, c.ORDINAL_POSITION`

const mysqlIndexQuery = `
SELECT s.TABLE_NAME, s.INDEX_NAME, s.COLUMN_NAME, s.SEQ_IN_INDEX,
       s.NON_UNIQUE, COALESCE(s.CARDINALITY, 0)
FROM information_schema.STATISTICS s
WHERE s.TABLE_SCHEMA = DATABASE() AND s.TABLE_NAME IN (%s)
ORDER BY s.TABLE_NAME, s.INDEX_NAME, s.SEQ_IN_INDEX`

// Introspect reads the schema for the named tables.
//
// The statistics MySQL exposes are thinner than Postgres's. There is no
// null_fraction at all, and n_distinct has to be derived from index
// cardinality, which only exists for indexed columns. Both gaps are reported as
// zero and the rule engine must treat zero as "unknown" rather than as
// "constant", or it would suppress an index on every unindexed column — the
// exact columns most likely to need one.
func (m *MySQL) Introspect(ctx context.Context, tables []string) (*Schema, error) {
	if len(tables) == 0 {
		return &Schema{}, nil
	}
	names := make([]string, 0, len(tables))
	args := make([]any, 0, len(tables))
	for _, t := range tables {
		// Strip any schema qualifier: DATABASE() scopes the lookup.
		if i := strings.LastIndex(t, "."); i >= 0 {
			t = t[i+1:]
		}
		names = append(names, "?")
		args = append(args, strings.Trim(t, "`"))
	}
	in := strings.Join(names, ",")

	byName := map[string]*Table{}
	var order []string

	err := m.readOnly(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(mysqlTableQuery, in), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t Table
			if err := rows.Scan(&t.Schema, &t.Name, &t.RowEstimate, &t.TotalBytes); err != nil {
				return err
			}
			cp := t
			byName[strings.ToLower(t.Name)] = &cp
			order = append(order, strings.ToLower(t.Name))
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("introspect tables: %w", err)
	}
	if len(byName) == 0 {
		return &Schema{}, nil
	}

	if err := m.readOnly(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(mysqlColumnQuery, in), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var table, name, typ, nullable string
			if err := rows.Scan(&table, &name, &typ, &nullable); err != nil {
				return err
			}
			if t, ok := byName[strings.ToLower(table)]; ok {
				t.Columns = append(t.Columns, Column{
					Name:     name,
					Type:     typ,
					Nullable: strings.EqualFold(nullable, "YES"),
					// MySQL exposes neither a null fraction nor an n_distinct
					// for unindexed columns. Zero means unknown here.
				})
			}
		}
		return rows.Err()
	}); err != nil {
		return nil, fmt.Errorf("introspect columns: %w", err)
	}

	if err := m.readOnly(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(mysqlIndexQuery, in), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		type key struct{ table, index string }
		acc := map[key]*Index{}
		var seen []key
		for rows.Next() {
			var table, index string
			var col sql.NullString
			var seq, nonUnique, cardinality int64
			if err := rows.Scan(&table, &index, &col, &seq, &nonUnique, &cardinality); err != nil {
				return err
			}
			k := key{strings.ToLower(table), index}
			if _, ok := acc[k]; !ok {
				acc[k] = &Index{
					Name:      index,
					IsUnique:  nonUnique == 0,
					IsPrimary: strings.EqualFold(index, "PRIMARY"),
				}
				seen = append(seen, k)
			}
			if col.Valid {
				acc[k].Columns = append(acc[k].Columns, col.String)
			}
			// STATISTICS.CARDINALITY is per prefix; the value at the leading
			// column is the useful one for "how selective is this index".
			if seq == 1 && cardinality > 0 {
				if t, ok := byName[k.table]; ok {
					for i := range t.Columns {
						if col.Valid && strings.EqualFold(t.Columns[i].Name, col.String) {
							t.Columns[i].DistinctValues = float64(cardinality)
						}
					}
				}
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, k := range seen {
			idx := acc[k]
			idx.Definition = fmt.Sprintf("%s (%s)", idx.Name, strings.Join(idx.Columns, ", "))
			if t, ok := byName[k.table]; ok {
				t.Indexes = append(t.Indexes, *idx)
			}
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("introspect indexes: %w", err)
	}

	out := &Schema{}
	for _, n := range order {
		out.Tables = append(out.Tables, *byName[n])
	}
	return out, nil
}

// -------------------------------------------------------------------- plans

// Explain returns the planner's chosen plan without executing the query.
func (m *MySQL) Explain(ctx context.Context, query string) (*Plan, error) {
	var raw string
	err := m.readOnly(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "EXPLAIN FORMAT=JSON "+query).Scan(&raw)
	})
	if err != nil {
		return nil, fmt.Errorf("explain: %w", err)
	}
	return parseMySQLPlan(raw)
}

// ---------------------------------------------------------------- measuring

// Measure executes the query `runs` times and reports the median.
//
// Unlike the Postgres engine, which reads execution time out of
// EXPLAIN ANALYZE, this times the client-visible round trip with the result set
// fully drained. Draining matters: MySQL streams rows, so stopping at the first
// one would time the planner rather than the query.
func (m *MySQL) Measure(ctx context.Context, sql string, runs int) (*Measurement, error) {
	if runs <= 0 {
		runs = m.timings.Runs
	}
	out := &Measurement{Runs: runs}

	plan, err := m.Explain(ctx, sql)
	if err != nil {
		return nil, err
	}
	out.Plan = plan

	timings := make([]float64, 0, runs)
	total := m.timings.WarmupRuns + runs
	for i := 0; i < total; i++ {
		elapsed, rowCount, err := m.timeOnce(ctx, sql)
		if err != nil {
			if isMySQLTimeout(err) {
				out.TimedOut = true
				return out, nil
			}
			return nil, err
		}
		// The warm-up run is discarded: the first execution pays for cold
		// caches and an empty plan cache, and including it makes every
		// subsequent "improvement" look larger than it is.
		if i < m.timings.WarmupRuns {
			continue
		}
		timings = append(timings, elapsed)
		out.ActualRows = float64(rowCount)
	}
	if len(timings) == 0 {
		return out, nil
	}
	sort.Float64s(timings)
	out.MedianMS = median(timings)
	out.MinMS = timings[0]
	out.MaxMS = timings[len(timings)-1]
	out.SpreadMS = out.MaxMS - out.MinMS
	return out, nil
}

func (m *MySQL) timeOnce(ctx context.Context, query string) (ms float64, rowCount int64, err error) {
	err = m.readOnly(ctx, func(ctx context.Context, tx *sql.Tx) error {
		start := time.Now()
		rows, err := tx.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			rowCount++
		}
		if err := rows.Err(); err != nil {
			return err
		}
		ms = float64(time.Since(start).Microseconds()) / 1000.0
		return nil
	})
	return ms, rowCount, err
}

// MeasureInTx exists on the Postgres engine so an index experiment can measure
// inside the transaction that holds the uncommitted index. MySQL has no such
// experiment, so this is deliberately absent rather than stubbed.

// WithHypotheticalIndex always refuses on MySQL.
func (m *MySQL) WithHypotheticalIndex(ctx context.Context, indexDDL string, fn func(context.Context) error) error {
	return fmt.Errorf("%w: MySQL commits DDL implicitly, so %q cannot be rolled back", ErrNoHypotheticalIndexes, indexDDL)
}

// --------------------------------------------------------------- checksums

// Checksum hashes the query's result set.
//
// Three details carry correctness here, and each is a way to get a *wrong*
// answer rather than an error:
//
//  1. MySQL has no row-to-text cast, so the row hash is built from an explicit
//     column list obtained by running the query with LIMIT 0.
//  2. CONCAT_WS skips NULL arguments, which would make ('x', NULL) and
//     (NULL, 'x') hash identically. Every column is therefore wrapped in a
//     COALESCE to a sentinel no value can produce.
//  3. GROUP_CONCAT truncates at group_concat_max_len — 1024 bytes by default —
//     and *silently returns a shorter string*. That would make two different
//     result sets hash the same. The limit is raised, and the connection is
//     asked afterwards whether it truncated anyway; a truncation is a hard
//     error, never a hash.
func (m *MySQL) Checksum(ctx context.Context, query string, ordered bool) (*ResultHash, error) {
	cols, err := m.resultColumns(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("checksum: %w", err)
	}
	if len(cols) == 0 {
		return nil, errors.New("checksum: query produced no columns")
	}

	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		// \x00NULL\x00 cannot be produced by CAST of any value, so a real value
		// can never collide with the NULL sentinel.
		parts = append(parts, fmt.Sprintf("COALESCE(CAST(`%s` AS CHAR), '\\0NULL\\0')", strings.ReplaceAll(c, "`", "``")))
	}
	rowHash := fmt.Sprintf("MD5(CONCAT_WS('\\x1f', %s))", strings.Join(parts, ", "))

	var agg string
	if ordered {
		// Arrival order is the query's own ORDER BY. ROW_NUMBER preserves it.
		agg = fmt.Sprintf(
			"SELECT COALESCE(MD5(GROUP_CONCAT(h ORDER BY rn SEPARATOR '')), 'empty'), COUNT(*) "+
				"FROM (SELECT ROW_NUMBER() OVER () AS rn, %s AS h FROM (%s) t) x", rowHash, query)
	} else {
		agg = fmt.Sprintf(
			"SELECT COALESCE(MD5(GROUP_CONCAT(h ORDER BY h SEPARATOR '')), 'empty'), COUNT(*) "+
				"FROM (SELECT %s AS h FROM (%s) t) x", rowHash, query)
	}

	out := &ResultHash{Ordered: ordered}
	err = m.readOnly(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "SET SESSION group_concat_max_len = 18446744073709551615"); err != nil {
			return fmt.Errorf("raise group_concat_max_len: %w", err)
		}
		if err := tx.QueryRowContext(ctx, agg).Scan(&out.Hash, &out.RowCount); err != nil {
			return err
		}
		// 1260 is ER_CUT_VALUE_GROUP_CONCAT. If it fired, the hash covers only
		// part of the result set and comparing it would be worse than useless.
		var warnings int
		if err := tx.QueryRowContext(ctx, "SELECT @@warning_count").Scan(&warnings); err != nil {
			return nil // the guard is best-effort; do not fail a good checksum on it
		}
		if warnings > 0 {
			rows, err := tx.QueryContext(ctx, "SHOW WARNINGS")
			if err != nil {
				return nil
			}
			defer rows.Close()
			for rows.Next() {
				var level, msg string
				var code int
				if err := rows.Scan(&level, &code, &msg); err != nil {
					return nil
				}
				if code == 1260 {
					return fmt.Errorf("checksum truncated by group_concat_max_len: %s", msg)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("checksum: %w", err)
	}
	return out, nil
}

// resultColumns runs the query with LIMIT 0 to learn its output column names
// without executing it for real.
func (m *MySQL) resultColumns(ctx context.Context, query string) ([]string, error) {
	var cols []string
	err := m.readOnly(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, fmt.Sprintf("SELECT * FROM (%s) t LIMIT 0", query))
		if err != nil {
			return err
		}
		defer rows.Close()
		cols, err = rows.Columns()
		return err
	})
	return cols, err
}

func isMySQLTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	s := strings.ToLower(err.Error())
	// 3024 is ER_QUERY_TIMEOUT from max_execution_time.
	return strings.Contains(s, "query execution was interrupted") ||
		strings.Contains(s, "maximum statement execution time")
}

// ------------------------------------------------------------- plan parsing

// parseMySQLPlan turns `EXPLAIN FORMAT=JSON` output into the dialect-neutral
// Plan the optimizer and the prompts consume.
//
// MySQL's JSON shape is a nested query_block rather than Postgres's node tree,
// and its cost lives in a "cost_info" object whose fields are strings. The
// access_type of a table node is what says "this is a full scan" — the MySQL
// spelling of Postgres's Seq Scan, and the single most common cause of a slow
// query on a large table.
func parseMySQLPlan(raw string) (*Plan, error) {
	var root map[string]any
	if err := json.Unmarshal([]byte(raw), &root); err != nil {
		return nil, fmt.Errorf("parse plan: %w", err)
	}
	p := &Plan{Raw: raw}
	var b strings.Builder
	qb, _ := root["query_block"].(map[string]any)
	if qb == nil {
		qb = root
	}
	if ci, ok := qb["cost_info"].(map[string]any); ok {
		p.TotalCost = toFloat(ci["query_cost"])
	}
	walkMySQLPlan(qb, 0, &b, p)
	p.Summary = strings.TrimRight(b.String(), "\n")
	return p, nil
}

func walkMySQLPlan(node map[string]any, depth int, b *strings.Builder, p *Plan) {
	if node == nil {
		return
	}
	indent := strings.Repeat("  ", depth)

	if ti, ok := node["table"].(map[string]any); ok {
		access, _ := ti["access_type"].(string)
		name, _ := ti["table_name"].(string)
		rows := toFloat(ti["rows_examined_per_scan"])
		if rows == 0 {
			rows = toFloat(ti["rows_produced_per_join"])
		}
		p.EstimatedRows += rows
		label := access
		if label == "" {
			label = "table"
		}
		p.NodeTypes = append(p.NodeTypes, label)
		// "ALL" is MySQL for a full table scan.
		if strings.EqualFold(access, "ALL") && name != "" {
			p.SeqScanTables = append(p.SeqScanTables, name)
		}
		key, _ := ti["key"].(string)
		if key != "" {
			fmt.Fprintf(b, "%s%s on %s using %s (rows=%.0f)\n", indent, label, name, key, rows)
		} else {
			fmt.Fprintf(b, "%s%s on %s (rows=%.0f)\n", indent, label, name, rows)
		}
		walkMySQLPlan(ti, depth+1, b, p)
		return
	}

	for _, key := range mysqlPlanChildKeys {
		switch child := node[key].(type) {
		case map[string]any:
			if key != "cost_info" && key != "table" {
				fmt.Fprintf(b, "%s%s\n", indent, key)
				walkMySQLPlan(child, depth+1, b, p)
			}
		case []any:
			for _, it := range child {
				if m, ok := it.(map[string]any); ok {
					walkMySQLPlan(m, depth+1, b, p)
				}
			}
		}
	}
}

// mysqlPlanChildKeys are the keys that carry nested plan structure. Listed
// explicitly rather than ranged over, so that a new informational key in a
// future MySQL version cannot start being rendered as if it were a plan node.
var mysqlPlanChildKeys = []string{
	"ordering_operation",
	"grouping_operation",
	"duplicates_removal",
	"nested_loop",
	"materialized_from_subquery",
	"attached_subqueries",
	"optimized_away_subqueries",
	"union_result",
	"query_specifications",
	"query_block",
	"table",
}

// Compile-time proof that both engines satisfy the same seam. If a method is
// added to Engine, both implementations must answer for it.
var (
	_ Engine = (*MySQL)(nil)
	_ Engine = (*Postgres)(nil)
)
