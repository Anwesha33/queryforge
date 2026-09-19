// Package sqlparse wraps the real PostgreSQL parser (libpg_query, via
// pg_query_go) rather than matching SQL with regular expressions.
//
// This matters more than it sounds. Every safety decision this service makes —
// is this statement read-only, which tables does it touch, does the candidate
// rewrite select the same columns — is a question about the structure of the
// statement. A regular expression answers those questions wrongly on the cases
// that matter most: a comment containing the word DELETE, a CTE that hides a
// DML statement inside a SELECT, a string literal containing a semicolon.
// Using the server's own grammar means the parse this service sees is the parse
// the database will see.
package sqlparse

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	pg "github.com/pganalyze/pg_query_go/v6"
)

// Statement is a parsed SQL statement plus the facts the optimizer needs.
type Statement struct {
	SQL string
	// Normalized replaces literals with placeholders, which is what makes two
	// runs of "the same query" with different constants comparable.
	Normalized string
	// Fingerprint is libpg_query's structural hash. Two statements with the
	// same fingerprint differ only in their literals.
	Fingerprint string

	Tables     []TableRef
	CTENames   []string
	OutputCols []string
	HasLimit   bool
	HasOrderBy bool
	// SelectStar records whether any select list is a bare `*`.
	SelectStar bool

	tree *pg.ParseResult
}

// TableRef is a table named by the statement.
type TableRef struct {
	Schema string
	Name   string
	Alias  string
}

func (t TableRef) Qualified() string {
	if t.Schema != "" && t.Schema != "public" {
		return t.Schema + "." + t.Name
	}
	return t.Name
}

// ErrNotReadOnly is returned for any statement that could modify data. It is a
// distinct error type because it is a refusal, not a failure: the caller
// reports it to the user rather than retrying.
var ErrNotReadOnly = errors.New("only read-only SELECT statements can be optimized")

// ErrMultipleStatements guards against a payload smuggling a second statement
// past a check that only inspects the first.
var ErrMultipleStatements = errors.New("exactly one statement is required")

// Parse parses a single read-only SELECT and extracts its structure.
func Parse(sql string) (*Statement, error) {
	trimmed := strings.TrimSpace(sql)
	if trimmed == "" {
		return nil, errors.New("empty statement")
	}

	tree, err := pg.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if len(tree.Stmts) != 1 {
		return nil, fmt.Errorf("%w: found %d", ErrMultipleStatements, len(tree.Stmts))
	}

	raw := tree.Stmts[0].Stmt
	sel := raw.GetSelectStmt()
	if sel == nil {
		// Anything that is not a SelectStmt is, by definition, not a read-only
		// query: INSERT, UPDATE, DELETE, and also DDL and utility statements
		// like COPY, which can write to the filesystem.
		return nil, fmt.Errorf("%w (statement type: %s)", ErrNotReadOnly, nodeKind(raw))
	}
	// A SELECT can still write: `SELECT * FROM x` is fine, but
	// `WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d` parses as a
	// SelectStmt and deletes rows. Data-modifying CTEs must be rejected.
	if err := rejectDataModifyingCTEs(sel); err != nil {
		return nil, err
	}
	if sel.IntoClause != nil {
		return nil, fmt.Errorf("%w: SELECT INTO creates a table", ErrNotReadOnly)
	}
	if sel.LockingClause != nil && len(sel.LockingClause) > 0 {
		return nil, fmt.Errorf("%w: locking clauses (FOR UPDATE/SHARE) take row locks", ErrNotReadOnly)
	}

	normalized, err := pg.Normalize(trimmed)
	if err != nil {
		normalized = trimmed
	}
	fingerprint, err := pg.Fingerprint(trimmed)
	if err != nil {
		fingerprint = ""
	}

	st := &Statement{
		SQL:         trimmed,
		Normalized:  normalized,
		Fingerprint: fingerprint,
		tree:        tree,
	}
	st.CTENames = cteNames(sel)
	st.Tables = collectTables(raw, st.CTENames)
	st.OutputCols = outputColumns(sel)
	st.SelectStar = hasSelectStar(sel)
	st.HasLimit = sel.LimitCount != nil
	st.HasOrderBy = len(sel.SortClause) > 0
	return st, nil
}

// Tree exposes the parse tree for the rule engine.
func (s *Statement) Tree() *pg.ParseResult { return s.tree }

// SameShapeAs reports whether a candidate rewrite could plausibly be equivalent
// to this statement, and explains why not when it could not.
//
// This is a cheap structural gate in front of the expensive one. It cannot
// prove equivalence — only executing both queries and comparing results can do
// that — but it rejects the obvious rewrites-gone-wrong for free: a different
// number of output columns, a table that appeared from nowhere, a table that
// silently disappeared.
func (s *Statement) SameShapeAs(other *Statement) error {
	if len(s.OutputCols) != len(other.OutputCols) {
		return fmt.Errorf("output column count differs: %d vs %d",
			len(s.OutputCols), len(other.OutputCols))
	}
	// Compare names only where the original names them. `SELECT *` expands at
	// execution time, so its column list is not known here and the result
	// comparison has to carry that check instead.
	if !s.SelectStar && !other.SelectStar {
		for i := range s.OutputCols {
			a, b := s.OutputCols[i], other.OutputCols[i]
			if a != "" && b != "" && !strings.EqualFold(a, b) {
				return fmt.Errorf("output column %d differs: %q vs %q", i+1, a, b)
			}
		}
	}

	origTables := tableSet(s.Tables)
	newTables := tableSet(other.Tables)
	for t := range newTables {
		if !origTables[t] {
			// A rewrite that reads a table the original never touched is not a
			// rewrite of this query, whatever else it may be.
			return fmt.Errorf("candidate reads table %q which the original does not", t)
		}
	}
	for t := range origTables {
		if !newTables[t] {
			// Dropping a table is legitimate sometimes — removing a join that
			// only existed to filter — so this is reported for the verifier to
			// weigh rather than treated as fatal here.
			return fmt.Errorf("candidate drops table %q; result equivalence must be proven", t)
		}
	}
	return nil
}

func tableSet(refs []TableRef) map[string]bool {
	out := make(map[string]bool, len(refs))
	for _, r := range refs {
		out[strings.ToLower(r.Qualified())] = true
	}
	return out
}

// rejectDataModifyingCTEs walks the WITH clause for INSERT/UPDATE/DELETE/MERGE.
func rejectDataModifyingCTEs(sel *pg.SelectStmt) error {
	if sel.WithClause == nil {
		return nil
	}
	for _, cte := range sel.WithClause.Ctes {
		common := cte.GetCommonTableExpr()
		if common == nil || common.Ctequery == nil {
			continue
		}
		switch kind := nodeKind(common.Ctequery); kind {
		case "SelectStmt":
			// Nested CTEs are checked by recursing into the sub-select.
			if inner := common.Ctequery.GetSelectStmt(); inner != nil {
				if err := rejectDataModifyingCTEs(inner); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("%w: CTE %q contains a %s",
				ErrNotReadOnly, common.Ctename, kind)
		}
	}
	return nil
}

func cteNames(sel *pg.SelectStmt) []string {
	if sel.WithClause == nil {
		return nil
	}
	var names []string
	for _, cte := range sel.WithClause.Ctes {
		if c := cte.GetCommonTableExpr(); c != nil {
			names = append(names, strings.ToLower(c.Ctename))
		}
	}
	return names
}

// collectTables walks the whole tree for RangeVar nodes, which is how a table
// reference appears regardless of where it sits — FROM, JOIN, a subquery, a
// sub-select in the WHERE clause.
func collectTables(node *pg.Node, cteNames []string) []TableRef {
	cte := make(map[string]bool, len(cteNames))
	for _, n := range cteNames {
		cte[n] = true
	}

	seen := map[string]bool{}
	var out []TableRef
	walk(node, func(n *pg.Node) {
		rv := n.GetRangeVar()
		if rv == nil {
			return
		}
		// A reference to a CTE is not a table; counting it would make the
		// optimizer try to introspect a relation that does not exist.
		if cte[strings.ToLower(rv.Relname)] && rv.Schemaname == "" {
			return
		}
		ref := TableRef{Schema: rv.Schemaname, Name: rv.Relname}
		if rv.Alias != nil {
			ref.Alias = rv.Alias.Aliasname
		}
		key := strings.ToLower(ref.Qualified()) + "|" + strings.ToLower(ref.Alias)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, ref)
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Qualified() < out[j].Qualified() })
	return out
}

func outputColumns(sel *pg.SelectStmt) []string {
	// A set operation (UNION/INTERSECT/EXCEPT) takes its column names from the
	// left-hand branch.
	if sel.Op != pg.SetOperation_SETOP_NONE && sel.Larg != nil {
		return outputColumns(sel.Larg)
	}
	var cols []string
	for _, target := range sel.TargetList {
		rt := target.GetResTarget()
		if rt == nil {
			continue
		}
		if rt.Name != "" {
			cols = append(cols, rt.Name)
			continue
		}
		cols = append(cols, inferColumnName(rt.Val))
	}
	return cols
}

// inferColumnName reproduces the part of Postgres's naming rules that matters
// here: a bare column reference is named after its last field, and anything
// else is unnamed.
func inferColumnName(val *pg.Node) string {
	if val == nil {
		return ""
	}
	if cr := val.GetColumnRef(); cr != nil {
		fields := cr.Fields
		if len(fields) == 0 {
			return ""
		}
		last := fields[len(fields)-1]
		if s := last.GetString_(); s != nil {
			return s.Sval
		}
		if last.GetAStar() != nil {
			return "*"
		}
	}
	return ""
}

func hasSelectStar(sel *pg.SelectStmt) bool {
	if sel.Op != pg.SetOperation_SETOP_NONE {
		return (sel.Larg != nil && hasSelectStar(sel.Larg)) ||
			(sel.Rarg != nil && hasSelectStar(sel.Rarg))
	}
	for _, target := range sel.TargetList {
		rt := target.GetResTarget()
		if rt == nil || rt.Val == nil {
			continue
		}
		cr := rt.Val.GetColumnRef()
		if cr == nil {
			continue
		}
		for _, f := range cr.Fields {
			if f.GetAStar() != nil {
				return true
			}
		}
	}
	return false
}

func nodeKind(n *pg.Node) string {
	if n == nil {
		return "nil"
	}
	switch {
	case n.GetSelectStmt() != nil:
		return "SelectStmt"
	case n.GetInsertStmt() != nil:
		return "InsertStmt"
	case n.GetUpdateStmt() != nil:
		return "UpdateStmt"
	case n.GetDeleteStmt() != nil:
		return "DeleteStmt"
	case n.GetMergeStmt() != nil:
		return "MergeStmt"
	case n.GetCreateStmt() != nil:
		return "CreateStmt"
	case n.GetDropStmt() != nil:
		return "DropStmt"
	case n.GetTruncateStmt() != nil:
		return "TruncateStmt"
	case n.GetCopyStmt() != nil:
		return "CopyStmt"
	case n.GetExplainStmt() != nil:
		return "ExplainStmt"
	case n.GetTransactionStmt() != nil:
		return "TransactionStmt"
	case n.GetVariableSetStmt() != nil:
		return "VariableSetStmt"
	default:
		return "unsupported statement"
	}
}
