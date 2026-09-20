package optimizer

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Anwesha33/queryforge/internal/engine"
	"github.com/Anwesha33/queryforge/internal/sqlparse"
)

type testLogger struct{ t *testing.T }

func (l testLogger) Info(msg string, kv ...any)  { l.t.Logf("INFO  %s %v", msg, kv) }
func (l testLogger) Error(msg string, kv ...any) { l.t.Logf("ERROR %s %v", msg, kv) }

// End to end against a real MySQL: parse → introspect → measure → rules →
// verify, with no model involved. Skipped unless MYSQL_TEST_DSN is set.
func TestMySQLEndToEnd(t *testing.T) {
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set")
	}
	ctx := context.Background()
	eng, err := engine.NewMySQL(ctx, dsn, engine.Timings{
		StatementTimeout: 60 * time.Second, Runs: 3, WarmupRuns: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	opt := &Optimizer{
		Engine:  eng,
		Dialect: sqlparse.DialectMySQL,
		Log:     testLogger{t},
		Opts: Options{
			MaxCandidates: 2, MinImprovementPct: 10, Runs: 3,
			TestIndexes: true, MaxIndexTests: 2, SkipLLM: true,
		},
	}

	rep, err := opt.Run(ctx, "SELECT id, status FROM orders WHERE DATE(created_at) = DATE(NOW() - INTERVAL 30 DAY)")
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if rep.Dialect != "mysql" {
		t.Errorf("dialect = %q", rep.Dialect)
	}
	if rep.Baseline.MedianMS <= 0 {
		t.Errorf("no baseline measurement: %+v", rep.Baseline)
	}
	if rep.Schema == nil || len(rep.Schema.Tables) == 0 {
		t.Error("schema was not introspected")
	}

	// The partial rule coverage must be stated, not silent: an empty findings
	// list on MySQL would otherwise read as a clean bill of health.
	if len(rep.Limitations) == 0 {
		t.Error("expected the report to record which analyses did not run")
	} else {
		t.Logf("limitations: %v", rep.Limitations)
	}

	// Any index candidate must be reported unmeasured and NOT recommended.
	for _, ix := range rep.Indexes {
		if ix.Recommended {
			t.Errorf("an index was recommended without being measured: %+v", ix)
		}
		if !strings.Contains(ix.Detail, "NOT measured") {
			t.Errorf("index detail does not say it was unmeasured: %q", ix.Detail)
		}
	}
	t.Logf("baseline median = %.1fms, findings = %d, indexes = %d, candidates = %d",
		rep.Baseline.MedianMS, len(rep.Findings), len(rep.Indexes), len(rep.Candidates))
}

// A rewrite that changes results must be rejected by the checksum gate, on
// MySQL exactly as on Postgres.
func TestMySQLVerifierRejectsNonEquivalentRewrite(t *testing.T) {
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set")
	}
	ctx := context.Background()
	eng, err := engine.NewMySQL(ctx, dsn, engine.Timings{
		StatementTimeout: 60 * time.Second, Runs: 3, WarmupRuns: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	const original = "SELECT id, status FROM orders WHERE total_cents > 499000"
	st, err := sqlparse.ParseMySQL(original)
	if err != nil {
		t.Fatal(err)
	}
	base, err := EstablishBaseline(ctx, eng, st, 3)
	if err != nil {
		t.Fatal(err)
	}

	v := &Verifier{Engine: eng, MinImprovementPct: 10, Runs: 3, Log: testLogger{t}, Dialect: sqlparse.DialectMySQL}

	// Same shape, different rows — the classic plausible-but-wrong rewrite.
	res := v.Verify(ctx, base, Candidate{SQL: original + " AND status = 'paid'", Source: "test"})
	if res.Verdict == VerdictAccepted {
		t.Fatalf("a result-changing rewrite was accepted: %+v", res)
	}
	t.Logf("rejected as expected: verdict=%s reason=%s", res.Verdict, res.Reason)
}
