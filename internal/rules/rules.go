// Package rules is the deterministic half of the optimizer.
//
// It analyses the parse tree, the schema and the plan, and produces findings —
// some of which carry a concrete rewrite. This matters architecturally: the
// service can improve a query with no LLM involved at all. The model's job is
// to propose rewrites the rules do not know about, not to be the only source of
// ideas. When the API key is missing, queryforge still works; it just finds
// less.
//
// Every finding carries the evidence that produced it, because "add an index on
// created_at" is advice and "the plan sequentially scans 2.1M rows and discards
// 99.4% of them at the filter on created_at" is a reason.
package rules

import (
	"fmt"
	"sort"
	"strings"

	pg "github.com/pganalyze/pg_query_go/v6"

	"github.com/Anwesha33/queryforge/internal/engine"
	"github.com/Anwesha33/queryforge/internal/sqlparse"
)

// Severity orders findings for reporting.
type Severity string

const (
	SeverityHigh   Severity = "high"
	SeverityMedium Severity = "medium"
	SeverityLow    Severity = "low"
)

// Finding is one detected problem.
type Finding struct {
	Code     string   `json:"code"`
	Title    string   `json:"title"`
	Detail   string   `json:"detail"`
	Severity Severity `json:"severity"`
	Table    string   `json:"table,omitempty"`
	Columns  []string `json:"columns,omitempty"`
	Evidence string   `json:"evidence,omitempty"`
	// Rewrite is a deterministic replacement for the whole statement, when the
	// rule can produce one safely. It still goes through the full verification
	// loop — nothing is trusted here either.
	Rewrite string `json:"rewrite,omitempty"`
	// SuggestedIndex is DDL to be measured, never to be executed for real.
	SuggestedIndex string `json:"suggested_index,omitempty"`
}

// Predicate is a comparison found in a WHERE or JOIN clause.
type Predicate struct {
	// Table is the real relation name, resolved from whatever the query wrote.
	Table string
	// QualifierAsWritten is the qualifier exactly as it appears in the SQL text
	// — usually an alias. Schema lookups need the real name; rewriting the
	// statement text needs this one, because the text says "o.created_at" and
	// no substitution for "orders.created_at" will ever match it.
	QualifierAsWritten string
	Column             string
	Operator           string
	// WrappedBy is the function applied to the column, if any. A predicate with
	// a function around the column cannot use a plain index on that column,
	// which is the most common reason a "correctly indexed" query is slow.
	WrappedBy string
	// CastTo is a type the column was cast to, which defeats an index the same way.
	CastTo string
	// LiteralText is the constant the column was compared against, if any.
	LiteralText string
	// IsJoin marks a predicate comparing two columns rather than a constant.
	IsJoin     bool
	OtherTable string
	OtherCol   string
}

// Analysis is everything the rules found.
type Analysis struct {
	Findings   []Finding   `json:"findings"`
	Predicates []Predicate `json:"predicates"`
	// Limitations names analyses that could not run against this statement.
	//
	// It exists because the alternative is a silent partial result. Most rules
	// here walk a libpg_query parse tree; a MySQL statement carries a TiDB tree
	// instead, so those rules find nothing — which is indistinguishable, in the
	// report, from a query with nothing wrong with it. Saying so turns a silent
	// gap into a stated one.
	Limitations []string `json:"limitations,omitempty"`
}

// Analyze runs every rule.
func Analyze(st *sqlparse.Statement, schema *engine.Schema, plan *engine.Measurement) *Analysis {
	a := &Analysis{}

	// The tree-walking rules are written against libpg_query's node types. On
	// any other dialect they would return nothing at all, so they are skipped
	// explicitly and the gap is recorded rather than left to look like a clean
	// bill of health. The rules that read dialect-neutral facts off the
	// Statement — SELECT *, LIMIT without ORDER BY — still run, as do the ones
	// that read the schema and the executed plan.
	treeRules := st != nil && st.Dialect != sqlparse.DialectMySQL

	if treeRules {
		a.Predicates = resolveTables(extractPredicates(st), st, schema)
		a.checkSargability(st, schema)
		a.checkLeadingWildcard(st)
		a.checkNotIn(st)
	} else {
		a.Limitations = append(a.Limitations,
			"predicate-level rules (non-sargable predicates, leading-wildcard LIKE, NOT IN subqueries) "+
				"are implemented against the PostgreSQL parse tree and did not run for this dialect")
	}

	a.checkSelectStar(st, schema)
	a.checkLimitWithoutOrder(st)
	a.checkMissingIndexes(schema)
	if plan != nil {
		a.checkPlan(plan, schema)
	}

	sort.SliceStable(a.Findings, func(i, j int) bool {
		return severityRank(a.Findings[i].Severity) < severityRank(a.Findings[j].Severity)
	})
	return a
}

func severityRank(s Severity) int {
	switch s {
	case SeverityHigh:
		return 0
	case SeverityMedium:
		return 1
	default:
		return 2
	}
}

func (a *Analysis) add(f Finding) { a.Findings = append(a.Findings, f) }

// ------------------------------------------------------------ sargability

// dateLikeTypes are the column types for which a DATE()/date_trunc() equality
// can be rewritten as a half-open range.
var dateLikeTypes = map[string]bool{
	"timestamp with time zone":    true,
	"timestamp without time zone": true,
	"timestamptz":                 true,
	"timestamp":                   true,
	"date":                        true,
}

// checkSargability finds predicates whose column is wrapped in a function or a
// cast, which prevents the planner from using an ordinary index on it.
func (a *Analysis) checkSargability(st *sqlparse.Statement, schema *engine.Schema) {
	for _, p := range a.Predicates {
		if p.WrappedBy == "" && p.CastTo == "" {
			continue
		}
		wrapper := p.WrappedBy
		if wrapper == "" {
			wrapper = "cast to " + p.CastTo
		}

		f := Finding{
			Code:     "non-sargable-predicate",
			Severity: SeverityHigh,
			Table:    p.Table,
			Columns:  []string{p.Column},
			Title:    fmt.Sprintf("%s(%s) in the filter prevents an index scan", wrapper, qualify(p.Table, p.Column)),
			Detail: fmt.Sprintf(
				"The filter applies %s to %s before comparing it. An index on %s stores the raw column "+
					"values, so the planner cannot use it and must compute the expression for every row. "+
					"Either rewrite the predicate so the bare column is compared, or build an expression "+
					"index on %s(%s) — the rewrite is almost always the better answer because it keeps "+
					"the existing index useful for other queries.",
				wrapper, qualify(p.Table, p.Column), qualify(p.Table, p.Column), wrapper, qualify(p.Table, p.Column)),
			Evidence: fmt.Sprintf("predicate: %s(%s) %s %s", wrapper, qualify(p.Table, p.Column), p.Operator, p.LiteralText),
		}

		// The one rewrite that can be produced deterministically and safely:
		// DATE(ts_col) = 'YYYY-MM-DD' becomes a half-open range on the raw
		// column. It is written as [day, day+1) rather than BETWEEN because
		// BETWEEN is inclusive at both ends and would wrongly include midnight
		// of the following day.
		if rewrite, ok := rewriteDateEquality(st, p, schema); ok {
			f.Rewrite = rewrite
			f.Detail += " A rewrite has been generated and will be verified against the original results."
		}
		a.add(f)
	}
}

// rewriteDateEquality turns DATE(col) = 'x' into col >= 'x' AND col < 'x' + 1 day.
func rewriteDateEquality(st *sqlparse.Statement, p Predicate, schema *engine.Schema) (string, bool) {
	if p.Operator != "=" || p.LiteralText == "" {
		return "", false
	}
	fn := strings.ToLower(p.WrappedBy)
	if fn != "date" {
		return "", false
	}
	// Only safe when the column really is a timestamp: on a `date` column the
	// DATE() call is a no-op and the rewrite, while correct, is pointless.
	if schema != nil {
		if t, ok := schema.Table(p.Table); ok {
			for _, c := range t.Columns {
				if strings.EqualFold(c.Name, p.Column) && !dateLikeTypes[strings.ToLower(c.Type)] {
					return "", false
				}
			}
		}
	}

	// Rewrite against the text as written, aliases and all.
	col := qualify(p.QualifierAsWritten, p.Column)
	lit := strings.Trim(p.LiteralText, "'")
	old := fmt.Sprintf("DATE(%s) = '%s'", col, lit)
	replacement := fmt.Sprintf("%s >= '%s'::timestamptz AND %s < '%s'::timestamptz + INTERVAL '1 day'",
		col, lit, col, lit)

	// Rewriting the SQL text is crude next to rebuilding it from the parse
	// tree, but it keeps the user's formatting and comments intact, and the
	// result is verified by execution anyway. A case-insensitive match is used
	// because the original may be written date(...) or Date(...).
	rewritten, ok := replaceFold(st.SQL, old, replacement)
	if !ok {
		return "", false
	}
	return rewritten, true
}

// replaceFold replaces the first case-insensitive occurrence of old, tolerating
// differences in internal whitespace.
func replaceFold(s, old, replacement string) (string, bool) {
	norm := func(x string) string { return strings.Join(strings.Fields(x), " ") }
	target := norm(old)

	lower := strings.ToLower(s)
	targetLower := strings.ToLower(target)
	if idx := strings.Index(lower, targetLower); idx >= 0 {
		return s[:idx] + replacement + s[idx+len(target):], true
	}
	// Fall back to a whitespace-insensitive scan.
	for i := 0; i < len(s); i++ {
		for j := i + 1; j <= len(s) && j-i <= len(target)+16; j++ {
			if strings.EqualFold(norm(s[i:j]), target) {
				return s[:i] + replacement + s[j:], true
			}
		}
	}
	return s, false
}

// ---------------------------------------------------------- other rules

func (a *Analysis) checkSelectStar(st *sqlparse.Statement, schema *engine.Schema) {
	if !st.SelectStar {
		return
	}
	detail := "SELECT * reads and transfers every column, which prevents index-only scans, " +
		"breaks when a column is added or reordered, and moves bytes the caller may not use."
	if schema != nil {
		var wide []string
		for _, t := range schema.Tables {
			if len(t.Columns) >= 10 {
				wide = append(wide, fmt.Sprintf("%s (%d columns)", t.Name, len(t.Columns)))
			}
		}
		if len(wide) > 0 {
			detail += " Affected tables: " + strings.Join(wide, ", ") + "."
		}
	}
	a.add(Finding{
		Code: "select-star", Severity: SeverityMedium,
		Title: "SELECT * reads every column", Detail: detail,
	})
}

func (a *Analysis) checkLeadingWildcard(st *sqlparse.Statement) {
	sqlparse.WalkTree(st, func(n *pg.Node) {
		expr := n.GetAExpr()
		if expr == nil || len(expr.Name) == 0 {
			return
		}
		op := operatorName(expr)
		if op != "~~" && op != "~~*" { // LIKE and ILIKE
			return
		}
		lit, ok := constText(expr.Rexpr)
		if !ok || !strings.HasPrefix(strings.Trim(lit, "'"), "%") {
			return
		}
		col := columnOf(expr.Lexpr)
		a.add(Finding{
			Code: "leading-wildcard-like", Severity: SeverityHigh,
			Table: col.table, Columns: []string{col.column},
			Title: fmt.Sprintf("LIKE '%s' cannot use a B-tree index", strings.Trim(lit, "'")),
			Detail: "A pattern beginning with % has no usable prefix, so a B-tree index cannot be " +
				"range-scanned and the planner falls back to reading every row. Options, in order of " +
				"preference: a trigram index (pg_trgm) on the column, a full-text search column, or " +
				"restructuring the query so the leading wildcard is not needed.",
			Evidence: fmt.Sprintf("%s LIKE %s", qualify(col.table, col.column), lit),
		})
	})
}

func (a *Analysis) checkLimitWithoutOrder(st *sqlparse.Statement) {
	if st.HasLimit && !st.HasOrderBy {
		a.add(Finding{
			Code: "limit-without-order-by", Severity: SeverityMedium,
			Title: "LIMIT without ORDER BY returns an arbitrary subset",
			Detail: "Without ORDER BY the rows returned are whichever the plan produced first. " +
				"That is stable enough to look correct in testing and changes the moment the planner " +
				"picks a different join order, an index is added, or the table grows. This is a " +
				"correctness finding, not a performance one.",
		})
	}
}

func (a *Analysis) checkNotIn(st *sqlparse.Statement) {
	sqlparse.WalkTree(st, func(n *pg.Node) {
		sub := n.GetSubLink()
		if sub == nil || sub.SubLinkType != pg.SubLinkType_ANY_SUBLINK {
			return
		}
		// NOT IN (subquery) parses as a negated ANY_SUBLINK.
		a.add(Finding{
			Code: "not-in-subquery", Severity: SeverityMedium,
			Title: "NOT IN with a subquery is a NULL trap and often a slow plan",
			Detail: "If the subquery returns even one NULL, NOT IN yields no rows at all — SQL's " +
				"three-valued logic makes `x NOT IN (1, NULL)` unknown rather than true. NOT EXISTS " +
				"has the intended semantics and usually plans as an anti-join instead of a filter.",
		})
	})
}

// checkMissingIndexes proposes an index for every filtered or joined column
// that has none, and explains when it declines to.
func (a *Analysis) checkMissingIndexes(schema *engine.Schema) {
	if schema == nil {
		return
	}
	proposed := map[string]bool{}

	for _, p := range a.Predicates {
		if p.Table == "" || p.Column == "" {
			continue
		}
		t, ok := schema.Table(p.Table)
		if !ok {
			continue
		}
		if indexLeadsWith(t, p.Column) {
			continue
		}
		// An index on a tiny table is never used: a sequential scan of one page
		// beats any index lookup.
		if t.RowEstimate < 1000 {
			continue
		}
		// A column where almost every row shares a value is not worth indexing:
		// the planner will correctly ignore the index and the write cost is
		// paid anyway. n_distinct below 0 is a negative fraction of the table.
		if col, found := columnOf2(t, p.Column); found {
			if col.DistinctValues > 0 && col.DistinctValues < 3 && !p.IsJoin {
				a.add(Finding{
					Code: "low-selectivity-column", Severity: SeverityLow,
					Table: t.Name, Columns: []string{p.Column},
					Title: fmt.Sprintf("%s has only ~%.0f distinct values; an index would not help",
						qualify(t.Name, p.Column), col.DistinctValues),
					Detail: "Indexing a column with very few distinct values costs write throughput " +
						"and disk without changing the plan, because reading most of the table through " +
						"an index is slower than scanning it.",
				})
				continue
			}
		}

		key := strings.ToLower(t.Name + "." + p.Column)
		if proposed[key] {
			continue
		}
		proposed[key] = true

		ddl := fmt.Sprintf("CREATE INDEX idx_%s_%s ON %s (%s)",
			sanitize(t.Name), sanitize(p.Column), t.Name, p.Column)
		a.add(Finding{
			Code: "missing-index", Severity: SeverityHigh,
			Table: t.Name, Columns: []string{p.Column},
			Title: fmt.Sprintf("No index leads with %s", qualify(t.Name, p.Column)),
			Detail: fmt.Sprintf(
				"%s is used as a %s but no index on %s has it as its leading column, so the planner "+
					"cannot seek on it. The table holds roughly %s rows. The suggested index is measured "+
					"before it is recommended — it is created inside a transaction that is rolled back.",
				qualify(t.Name, p.Column), predicateKind(p), t.Name, humanCount(t.RowEstimate)),
			Evidence:       fmt.Sprintf("existing indexes: %s", describeIndexes(t)),
			SuggestedIndex: ddl,
		})
	}
}

// checkPlan reads the executed plan for problems the SQL alone cannot show.
func (a *Analysis) checkPlan(m *engine.Measurement, schema *engine.Schema) {
	if m.Plan == nil {
		return
	}

	for _, table := range m.Plan.SeqScanTables {
		rows := int64(0)
		if schema != nil {
			if t, ok := schema.Table(table); ok {
				rows = t.RowEstimate
			}
		}
		if rows < 50000 {
			// Sequentially scanning a small table is the correct plan, not a
			// problem. Flagging it trains people to ignore the tool.
			continue
		}
		a.add(Finding{
			Code: "seq-scan-large-table", Severity: SeverityHigh, Table: table,
			Title:  fmt.Sprintf("Sequential scan on %s (~%s rows)", table, humanCount(rows)),
			Detail: "The plan reads the whole table. With a selective filter this is usually a missing or unusable index.",
			Evidence: fmt.Sprintf("execution time %.1fms, %d blocks read from disk, %d from cache",
				m.MedianMS, m.SharedRead, m.SharedHit),
		})
	}

	// A large gap between estimated and actual rows means the planner is
	// choosing between plans using numbers that are wrong, which is worth
	// saying because the fix is ANALYZE or extended statistics, not a rewrite.
	if m.Plan.EstimatedRows > 0 && m.ActualRows > 0 {
		ratio := m.ActualRows / m.Plan.EstimatedRows
		if ratio > 10 || ratio < 0.1 {
			a.add(Finding{
				Code: "row-estimate-off", Severity: SeverityMedium,
				Title: fmt.Sprintf("Planner estimate is off by %.0fx", maxf(ratio, 1/ratio)),
				Detail: "The planner estimated a very different row count from what the query returned. " +
					"Plan choice depends on these estimates, so this often matters more than the query " +
					"text. Run ANALYZE on the tables; if the columns are correlated, CREATE STATISTICS " +
					"lets the planner model that.",
				Evidence: fmt.Sprintf("estimated %.0f rows, actual %.0f rows", m.Plan.EstimatedRows, m.ActualRows),
			})
		}
	}

	if m.TempWritten > 0 {
		a.add(Finding{
			Code: "sort-spilled-to-disk", Severity: SeverityMedium,
			Title:  "A sort or hash spilled to disk",
			Detail: "The operation did not fit in work_mem and used temporary files. Either reduce the rows being sorted (filter earlier, or push the LIMIT down) or raise work_mem for this workload.",
			Evidence: fmt.Sprintf("%d temp blocks written (~%d MB)",
				m.TempWritten, m.TempWritten*8/1024),
		})
	}
}

// ------------------------------------------------------ predicate extraction

type colRef struct{ table, column string }

// resolveTables turns the table name written in the query into the real
// relation name.
//
// Two problems, both of which silently produced zero findings before they were
// handled. A predicate written `o.created_at` carries the alias `o`, and no
// schema lookup for "o" will ever succeed. And a predicate written
// `created_at`, with no qualifier at all, carries no table — which is legal SQL
// whenever the column name is unambiguous across the joined tables.
//
// Aliases are resolved from the statement's own table list. Unqualified columns
// are resolved against the schema, and only when exactly one of the query's
// tables has a column by that name: guessing between two candidates would
// attribute a filter to the wrong table and propose an index on it.
func resolveTables(preds []Predicate, st *sqlparse.Statement, schema *engine.Schema) []Predicate {
	alias := map[string]string{}
	for _, t := range st.Tables {
		if t.Alias != "" {
			alias[strings.ToLower(t.Alias)] = t.Name
		}
		alias[strings.ToLower(t.Name)] = t.Name
	}

	resolveUnqualified := func(column string) string {
		if schema == nil {
			return ""
		}
		var found string
		for _, tref := range st.Tables {
			t, ok := schema.Table(tref.Name)
			if !ok {
				continue
			}
			for _, c := range t.Columns {
				if strings.EqualFold(c.Name, column) {
					if found != "" && !strings.EqualFold(found, t.Name) {
						return "" // ambiguous: refuse to guess
					}
					found = t.Name
				}
			}
		}
		return found
	}

	out := make([]Predicate, 0, len(preds))
	for _, p := range preds {
		p.QualifierAsWritten = p.Table
		if p.Table == "" {
			p.Table = resolveUnqualified(p.Column)
		} else if real, ok := alias[strings.ToLower(p.Table)]; ok {
			p.Table = real
		}
		if p.OtherTable != "" {
			if real, ok := alias[strings.ToLower(p.OtherTable)]; ok {
				p.OtherTable = real
			}
		}
		out = append(out, p)
	}
	return out
}

// extractPredicates walks the tree for comparisons involving a column.
func extractPredicates(st *sqlparse.Statement) []Predicate {
	var out []Predicate
	seen := map[string]bool{}

	sqlparse.WalkTree(st, func(n *pg.Node) {
		expr := n.GetAExpr()
		if expr == nil {
			return
		}
		op := operatorName(expr)
		switch op {
		case "=", "<", "<=", ">", ">=", "<>", "!=":
		default:
			return
		}

		left, leftOK := analyseSide(expr.Lexpr)
		right, rightOK := analyseSide(expr.Rexpr)

		switch {
		case leftOK && rightOK:
			// column = column: a join predicate. Both sides are worth indexing.
			p := Predicate{
				Table: left.ref.table, Column: left.ref.column, Operator: op,
				WrappedBy: left.wrapper, CastTo: left.castTo,
				IsJoin: true, OtherTable: right.ref.table, OtherCol: right.ref.column,
			}
			addPredicate(&out, seen, p)
			addPredicate(&out, seen, Predicate{
				Table: right.ref.table, Column: right.ref.column, Operator: op,
				WrappedBy: right.wrapper, CastTo: right.castTo,
				IsJoin: true, OtherTable: left.ref.table, OtherCol: left.ref.column,
			})
		case leftOK:
			lit, _ := constText(expr.Rexpr)
			addPredicate(&out, seen, Predicate{
				Table: left.ref.table, Column: left.ref.column, Operator: op,
				WrappedBy: left.wrapper, CastTo: left.castTo, LiteralText: lit,
			})
		case rightOK:
			lit, _ := constText(expr.Lexpr)
			addPredicate(&out, seen, Predicate{
				Table: right.ref.table, Column: right.ref.column, Operator: flipOperator(op),
				WrappedBy: right.wrapper, CastTo: right.castTo, LiteralText: lit,
			})
		}
	})
	return out
}

func addPredicate(out *[]Predicate, seen map[string]bool, p Predicate) {
	if p.Column == "" {
		return
	}
	key := strings.ToLower(fmt.Sprintf("%s|%s|%s|%s|%s", p.Table, p.Column, p.Operator, p.WrappedBy, p.LiteralText))
	if seen[key] {
		return
	}
	seen[key] = true
	*out = append(*out, p)
}

type sideInfo struct {
	ref     colRef
	wrapper string
	castTo  string
}

// analyseSide unwraps one side of a comparison down to a column reference,
// recording any function or cast that was in the way.
func analyseSide(n *pg.Node) (sideInfo, bool) {
	if n == nil {
		return sideInfo{}, false
	}
	if cr := n.GetColumnRef(); cr != nil {
		return sideInfo{ref: columnRefName(cr)}, true
	}
	if fc := n.GetFuncCall(); fc != nil {
		name := funcName(fc)
		for _, arg := range fc.Args {
			if inner, ok := analyseSide(arg); ok {
				inner.wrapper = name
				return inner, true
			}
		}
		return sideInfo{}, false
	}
	if tc := n.GetTypeCast(); tc != nil {
		inner, ok := analyseSide(tc.Arg)
		if !ok {
			return sideInfo{}, false
		}
		inner.castTo = typeName(tc)
		return inner, true
	}
	return sideInfo{}, false
}

func columnOf(n *pg.Node) colRef {
	info, _ := analyseSide(n)
	return info.ref
}

func columnOf2(t engine.Table, name string) (engine.Column, bool) {
	for _, c := range t.Columns {
		if strings.EqualFold(c.Name, name) {
			return c, true
		}
	}
	return engine.Column{}, false
}

func columnRefName(cr *pg.ColumnRef) colRef {
	var parts []string
	for _, f := range cr.Fields {
		if s := f.GetString_(); s != nil {
			parts = append(parts, s.Sval)
		}
	}
	switch len(parts) {
	case 0:
		return colRef{}
	case 1:
		return colRef{column: parts[0]}
	default:
		return colRef{table: parts[len(parts)-2], column: parts[len(parts)-1]}
	}
}

func funcName(fc *pg.FuncCall) string {
	var parts []string
	for _, f := range fc.Funcname {
		if s := f.GetString_(); s != nil {
			parts = append(parts, s.Sval)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

func typeName(tc *pg.TypeCast) string {
	if tc.TypeName == nil {
		return ""
	}
	var parts []string
	for _, f := range tc.TypeName.Names {
		if s := f.GetString_(); s != nil {
			parts = append(parts, s.Sval)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

func operatorName(expr *pg.A_Expr) string {
	if len(expr.Name) == 0 {
		return ""
	}
	if s := expr.Name[len(expr.Name)-1].GetString_(); s != nil {
		return s.Sval
	}
	return ""
}

func constText(n *pg.Node) (string, bool) {
	if n == nil {
		return "", false
	}
	if tc := n.GetTypeCast(); tc != nil {
		return constText(tc.Arg)
	}
	c := n.GetAConst()
	if c == nil {
		return "", false
	}
	switch {
	case c.GetSval() != nil:
		return "'" + c.GetSval().Sval + "'", true
	case c.GetIval() != nil:
		return fmt.Sprintf("%d", c.GetIval().Ival), true
	case c.GetFval() != nil:
		return c.GetFval().Fval, true
	case c.GetBoolval() != nil:
		return fmt.Sprintf("%t", c.GetBoolval().Boolval), true
	}
	return "", false
}

func flipOperator(op string) string {
	switch op {
	case "<":
		return ">"
	case "<=":
		return ">="
	case ">":
		return "<"
	case ">=":
		return "<="
	default:
		return op
	}
}

// ------------------------------------------------------------------ helpers

func indexLeadsWith(t engine.Table, column string) bool {
	for _, idx := range t.Indexes {
		if len(idx.Columns) > 0 && strings.EqualFold(idx.Columns[0], column) {
			return true
		}
	}
	return false
}

func describeIndexes(t engine.Table) string {
	if len(t.Indexes) == 0 {
		return "none"
	}
	var parts []string
	for _, idx := range t.Indexes {
		parts = append(parts, fmt.Sprintf("%s(%s)", idx.Name, strings.Join(idx.Columns, ", ")))
	}
	return strings.Join(parts, ", ")
}

func predicateKind(p Predicate) string {
	if p.IsJoin {
		return "join key"
	}
	return "filter"
}

func qualify(table, column string) string {
	if table == "" {
		return column
	}
	return table + "." + column
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func humanCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// Summarize renders findings for the LLM prompt and the report.
func (a *Analysis) Summarize() string {
	if len(a.Findings) == 0 {
		return "(no rule-based findings)"
	}
	var b strings.Builder
	for _, f := range a.Findings {
		fmt.Fprintf(&b, "- [%s] %s: %s\n", strings.ToUpper(string(f.Severity)), f.Code, f.Title)
		if f.Evidence != "" {
			fmt.Fprintf(&b, "    evidence: %s\n", f.Evidence)
		}
		if f.SuggestedIndex != "" {
			fmt.Fprintf(&b, "    candidate index: %s\n", f.SuggestedIndex)
		}
	}
	return b.String()
}
