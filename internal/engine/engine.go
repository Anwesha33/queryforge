// Package engine talks to the database under optimisation: schema
// introspection, plans, timing, result checksums, and hypothetical indexes.
//
// Everything here runs against a *target* database that the service treats as
// read-only. The one exception is the index experiment, which creates an index
// inside a transaction and rolls it back — see WithHypotheticalIndex.
package engine

import (
	"context"
	"time"
)

// Engine is the seam between the optimizer and a specific database.
//
// Only Postgres is implemented. The interface exists because the optimizer's
// logic is dialect-independent, but the honest position is in docs: MySQL needs
// a different strategy for index experiments because it has no transactional
// DDL, so `CREATE INDEX ... ROLLBACK` — the trick that makes measuring a
// hypothetical index safe here — simply does not work there.
type Engine interface {
	Introspect(ctx context.Context, tables []string) (*Schema, error)
	Explain(ctx context.Context, sql string) (*Plan, error)
	Measure(ctx context.Context, sql string, runs int) (*Measurement, error)
	Checksum(ctx context.Context, sql string, ordered bool) (*ResultHash, error)
	WithHypotheticalIndex(ctx context.Context, indexDDL string, fn func(context.Context) error) error
	Dialect() string
	Close()
}

// Column is one column of a table.
type Column struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	// NullFraction and DistinctValues come from the planner's own statistics.
	// They are what turns "add an index on status" into "an index on status is
	// pointless, 94% of rows share one value".
	NullFraction   float64 `json:"null_fraction"`
	DistinctValues float64 `json:"n_distinct"`
}

// Index is one index on a table.
type Index struct {
	Name       string   `json:"name"`
	Definition string   `json:"definition"`
	Columns    []string `json:"columns"`
	IsUnique   bool     `json:"is_unique"`
	IsPrimary  bool     `json:"is_primary"`
	SizeBytes  int64    `json:"size_bytes"`
	// Scans is how many times the planner has actually chosen this index.
	// A large index with zero scans is a pure write tax.
	Scans int64 `json:"scans"`
}

// Table is one table with its statistics.
type Table struct {
	Schema      string   `json:"schema"`
	Name        string   `json:"name"`
	Columns     []Column `json:"columns"`
	Indexes     []Index  `json:"indexes"`
	RowEstimate int64    `json:"row_estimate"`
	TotalBytes  int64    `json:"total_bytes"`
	SeqScans    int64    `json:"seq_scans"`
	IdxScans    int64    `json:"idx_scans"`
}

// Schema is everything the optimizer knows about the tables a query touches.
type Schema struct {
	Tables []Table `json:"tables"`
}

func (s *Schema) Table(name string) (Table, bool) {
	for _, t := range s.Tables {
		if equalFold(t.Name, name) || equalFold(t.Schema+"."+t.Name, name) {
			return t, true
		}
	}
	return Table{}, false
}

// Plan is a parsed EXPLAIN result.
type Plan struct {
	// Raw is the JSON the database returned, kept verbatim so the LLM sees the
	// planner's own words rather than a lossy summary.
	Raw string `json:"-"`
	// Summary is a compact, indented rendering for prompts and reports.
	Summary       string   `json:"summary"`
	TotalCost     float64  `json:"total_cost"`
	StartupCost   float64  `json:"startup_cost"`
	EstimatedRows float64  `json:"estimated_rows"`
	NodeTypes     []string `json:"node_types"`
	// SeqScanTables lists tables read with a sequential scan, which is the
	// single most common cause of a slow query on a large table.
	SeqScanTables []string `json:"seq_scan_tables"`
}

// Measurement is the outcome of timing a query.
type Measurement struct {
	Runs int `json:"runs"`
	// MedianMS is the headline number. The median rather than the mean because
	// one cold run — a cache miss, an autovacuum, a noisy neighbour — moves a
	// mean of five runs far more than it moves their median.
	MedianMS float64 `json:"median_ms"`
	MinMS    float64 `json:"min_ms"`
	MaxMS    float64 `json:"max_ms"`
	// SpreadMS is max-min, used to decide whether a difference between two
	// measurements is signal or noise.
	SpreadMS    float64 `json:"spread_ms"`
	PlanningMS  float64 `json:"planning_ms"`
	ActualRows  float64 `json:"actual_rows"`
	SharedHit   int64   `json:"shared_hit_blocks"`
	SharedRead  int64   `json:"shared_read_blocks"`
	TempWritten int64   `json:"temp_written_blocks"`
	Plan        *Plan   `json:"plan"`
	TimedOut    bool    `json:"timed_out"`
}

// ResultHash identifies a query's result set.
type ResultHash struct {
	Hash     string `json:"hash"`
	RowCount int64  `json:"row_count"`
	// Ordered records whether row order was part of the hash. It must match
	// between the two queries being compared, or the comparison is meaningless.
	Ordered bool `json:"ordered"`
}

// Equal reports whether two result sets are identical.
func (r *ResultHash) Equal(other *ResultHash) bool {
	if r == nil || other == nil {
		return false
	}
	return r.Ordered == other.Ordered && r.RowCount == other.RowCount && r.Hash == other.Hash
}

// Timings holds the tunables for measurement.
type Timings struct {
	StatementTimeout time.Duration
	Runs             int
	// WarmupRuns are executed and discarded. The first execution of a query
	// pays for cold caches and a plan that is not yet in the plan cache, and
	// including it makes every "improvement" look larger than it is.
	WarmupRuns int
}

func DefaultTimings() Timings {
	return Timings{StatementTimeout: 30 * time.Second, Runs: 5, WarmupRuns: 1}
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
