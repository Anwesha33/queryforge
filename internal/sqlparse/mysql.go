package sqlparse

import (
	"errors"
	"fmt"
	"strings"

	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/format"
	_ "github.com/pingcap/tidb/pkg/parser/test_driver" // registers the expression node types
)

// The MySQL side uses TiDB's parser: the real MySQL grammar, in Go, for the
// same reason the Postgres side uses libpg_query rather than regular
// expressions. Every safety decision here is a question about the *structure*
// of a statement, and a dialect-mismatched parser answers those wrongly on
// exactly the inputs that matter — `SELECT id FROM t WHERE note = 'DELETE FROM t'`
// is one statement in both dialects, but backtick identifiers, `LIMIT 10, 20`
// and index hints are MySQL-only and make libpg_query reject valid MySQL.
//
// One genuine dialect difference is worth recording: MySQL's CTEs cannot
// contain DML, so the `WITH gone AS (DELETE FROM orders RETURNING *) SELECT *`
// attack that Postgres allows has no MySQL equivalent. The check is still
// structural on both sides; on MySQL the grammar simply refuses to produce that
// tree.

// ParseMySQL parses a single read-only MySQL SELECT and extracts its structure.
func ParseMySQL(sql string) (*Statement, error) {
	trimmed := strings.TrimSpace(sql)
	if trimmed == "" {
		return nil, errors.New("empty statement")
	}

	p := parser.New()
	stmts, warns, err := p.Parse(trimmed, "", "")
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if len(stmts) != 1 {
		return nil, fmt.Errorf("%w: found %d", ErrMultipleStatements, len(stmts))
	}
	// Warnings are not failures, but a statement the parser had to reinterpret
	// is not one to run five times against a database.
	if len(warns) > 0 {
		return nil, fmt.Errorf("parse produced warnings, refusing: %v", warns[0])
	}

	root := stmts[0]
	switch root.(type) {
	case *ast.SelectStmt, *ast.SetOprStmt:
		// SetOprStmt is UNION / INTERSECT / EXCEPT over selects.
	default:
		return nil, fmt.Errorf("%w (statement type: %T)", ErrNotReadOnly, root)
	}

	// A SELECT can still write or lock. These are checked over the whole tree,
	// not just the top level, because a subquery carries them just as well.
	if err := rejectMySQLWriteForms(root); err != nil {
		return nil, err
	}

	normalized, digest := parser.NormalizeDigest(trimmed)

	st := &Statement{
		SQL:         trimmed,
		Normalized:  normalized,
		Fingerprint: digest.String(),
		Dialect:     DialectMySQL,
	}

	c := &mysqlCollector{}
	root.Accept(c)

	st.CTENames = c.cteNames
	st.Tables = filterCTEs(c.tables, c.cteNames)
	st.OutputCols = c.outputCols
	st.SelectStar = c.selectStar
	st.HasLimit = c.hasLimit
	st.HasOrderBy = c.hasOrderBy
	return st, nil
}

// rejectMySQLWriteForms walks the statement for the constructs that make a
// "SELECT" not read-only.
func rejectMySQLWriteForms(root ast.Node) error {
	g := &mysqlGuard{}
	root.Accept(g)
	return g.err
}

type mysqlGuard struct{ err error }

func (g *mysqlGuard) Enter(n ast.Node) (ast.Node, bool) {
	if g.err != nil {
		return n, true
	}
	switch s := n.(type) {
	case *ast.SelectStmt:
		if s.LockInfo != nil && s.LockInfo.LockType != ast.SelectLockNone {
			g.err = fmt.Errorf("%w: locking clauses (FOR UPDATE / LOCK IN SHARE MODE) take row locks", ErrNotReadOnly)
			return n, true
		}
		if s.SelectIntoOpt != nil {
			g.err = fmt.Errorf("%w: SELECT ... INTO writes outside the result set", ErrNotReadOnly)
			return n, true
		}
	case *ast.InsertStmt, *ast.UpdateStmt, *ast.DeleteStmt, *ast.LoadDataStmt:
		g.err = fmt.Errorf("%w: statement contains a %T", ErrNotReadOnly, n)
		return n, true
	}
	return n, false
}

func (g *mysqlGuard) Leave(n ast.Node) (ast.Node, bool) { return n, g.err == nil }

// mysqlCollector gathers the same facts the Postgres path collects.
type mysqlCollector struct {
	tables     []TableRef
	cteNames   []string
	outputCols []string
	selectStar bool
	hasLimit   bool
	hasOrderBy bool
	// depth tracks nesting so that only the outermost select contributes the
	// output column list, LIMIT and ORDER BY. A subquery's ORDER BY says
	// nothing about whether *this* query promises an ordering.
	depth int
	// seenTop records that the outermost select has been processed.
	seenTop bool
}

func (c *mysqlCollector) Enter(n ast.Node) (ast.Node, bool) {
	switch v := n.(type) {
	case *ast.WithClause:
		for _, cte := range v.CTEs {
			c.cteNames = append(c.cteNames, cte.Name.O)
		}
	case *ast.TableSource:
		if tn, ok := v.Source.(*ast.TableName); ok {
			c.tables = append(c.tables, TableRef{
				Schema: tn.Schema.O,
				Name:   tn.Name.O,
				Alias:  v.AsName.O,
			})
			// Skip children: the only child is that TableName, and visiting it
			// again would add a second, alias-less entry for the same table.
			// A TableSource wrapping a subquery is *not* skipped, because its
			// children carry the tables the subquery reads.
			return n, true
		}
	case *ast.TableName:
		// Reached without an enclosing TableSource, so there is no alias.
		c.tables = append(c.tables, TableRef{Schema: v.Schema.O, Name: v.Name.O})
	case *ast.SelectStmt:
		if !c.seenTop {
			c.seenTop = true
			c.hasLimit = v.Limit != nil
			c.hasOrderBy = v.OrderBy != nil && len(v.OrderBy.Items) > 0
			if v.Fields != nil {
				for _, f := range v.Fields.Fields {
					if f.WildCard != nil {
						c.selectStar = true
						c.outputCols = append(c.outputCols, "")
						continue
					}
					c.outputCols = append(c.outputCols, mysqlColumnName(f))
				}
			}
		}
	}
	return n, false
}

func (c *mysqlCollector) Leave(n ast.Node) (ast.Node, bool) { return n, true }

// mysqlColumnName is the name a column will carry in the result set: its alias
// if it has one, otherwise the bare column name, otherwise "" for an expression
// whose name only the server can decide.
func mysqlColumnName(f *ast.SelectField) string {
	if f.AsName.O != "" {
		return f.AsName.O
	}
	if col, ok := f.Expr.(*ast.ColumnNameExpr); ok && col.Name != nil {
		return col.Name.Name.O
	}
	return ""
}

// filterCTEs removes CTE names from the table list. A CTE is referenced exactly
// like a table, and a schema lookup for one will never succeed.
func filterCTEs(tables []TableRef, cteNames []string) []TableRef {
	if len(cteNames) == 0 {
		return dedupeTables(tables)
	}
	cte := make(map[string]bool, len(cteNames))
	for _, n := range cteNames {
		cte[strings.ToLower(n)] = true
	}
	out := tables[:0:0]
	for _, t := range tables {
		if t.Schema == "" && cte[strings.ToLower(t.Name)] {
			continue
		}
		out = append(out, t)
	}
	return dedupeTables(out)
}

// dedupeTables collapses repeats of the same table, keeping the entry that
// carries an alias. The same table can legitimately appear twice under
// different aliases (a self-join), so the alias is part of the identity when it
// is present.
func dedupeTables(tables []TableRef) []TableRef {
	seen := make(map[string]int, len(tables))
	out := tables[:0:0]
	for _, t := range tables {
		key := strings.ToLower(t.Schema + "." + t.Name + "\x00" + t.Alias)
		if _, ok := seen[key]; ok {
			continue
		}
		// An alias-less repeat of a table already recorded with an alias is the
		// same reference reached by another path, not a second use of it.
		if t.Alias == "" {
			bare := strings.ToLower(t.Schema + "." + t.Name)
			if _, ok := seen["aliased\x00"+bare]; ok {
				continue
			}
		} else {
			seen["aliased\x00"+strings.ToLower(t.Schema+"."+t.Name)] = len(out)
		}
		seen[key] = len(out)
		out = append(out, t)
	}
	return out
}

// RestoreMySQL renders a parsed MySQL node back to SQL. Used by tests and by
// anything that needs a canonical form of a fragment.
func RestoreMySQL(n ast.Node) (string, error) {
	var sb strings.Builder
	flags := format.DefaultRestoreFlags | format.RestoreStringSingleQuotes
	if err := n.Restore(format.NewRestoreCtx(flags, &sb)); err != nil {
		return "", err
	}
	return sb.String(), nil
}
