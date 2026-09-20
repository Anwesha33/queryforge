package engine

import (
	"context"
	"fmt"
	"strings"
)

// DialectForDSN infers which database a DSN points at.
//
// Detection is by shape rather than by configuration because getting it wrong
// is not a subtle failure: a Postgres DSN handed to the MySQL driver produces a
// connection error at startup, not a wrong answer later. An explicit
// TARGET_DIALECT override exists for the cases this cannot decide.
func DialectForDSN(dsn string) (string, error) {
	s := strings.TrimSpace(dsn)
	switch {
	case strings.HasPrefix(s, "postgres://"), strings.HasPrefix(s, "postgresql://"):
		return "postgres", nil
	case strings.HasPrefix(s, "mysql://"):
		return "mysql", nil
	// go-sql-driver's native form: user:pass@tcp(host:port)/db
	case strings.Contains(s, "@tcp("), strings.Contains(s, "@unix("):
		return "mysql", nil
	// A bare key=value Postgres connection string.
	case strings.Contains(s, "host=") && strings.Contains(s, "dbname="):
		return "postgres", nil
	default:
		return "", fmt.Errorf("cannot infer dialect from DSN; set TARGET_DIALECT to postgres or mysql")
	}
}

// normaliseMySQLDSN strips a mysql:// scheme, which go-sql-driver does not
// accept, so that both spellings work in configuration.
func normaliseMySQLDSN(dsn string) string {
	return strings.TrimPrefix(strings.TrimSpace(dsn), "mysql://")
}

// Open connects to the target database using the engine for its dialect.
//
// dialect may be empty, in which case it is inferred from the DSN.
func Open(ctx context.Context, dsn, dialect string, timings Timings) (Engine, error) {
	if dialect == "" {
		d, err := DialectForDSN(dsn)
		if err != nil {
			return nil, err
		}
		dialect = d
	}
	switch strings.ToLower(dialect) {
	case "postgres", "postgresql":
		return NewPostgres(ctx, dsn, timings)
	case "mysql":
		return NewMySQL(ctx, normaliseMySQLDSN(dsn), timings)
	default:
		return nil, fmt.Errorf("unsupported dialect %q", dialect)
	}
}
