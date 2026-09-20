// Command bench scores queryforge against the benchmark queries.
//
// It answers three questions that "the optimizer works" does not:
//
//   - Does it find the defect that is actually there? Each benchmark query
//     records which finding a correct optimizer should produce.
//   - When it accepts a rewrite, is the rewrite real? Acceptance already
//     requires identical results and a measured speedup, so an accepted
//     candidate that is wrong would be a verification bug — this reports the
//     rate so that bug would be visible.
//   - Does it invent work on queries that are already fine? Two benchmark
//     queries are controls. Anything accepted on those is a false positive,
//     and false positives are what make an optimisation tool untrustworthy.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/Anwesha33/queryforge/internal/config"
	"github.com/Anwesha33/queryforge/internal/engine"
	"github.com/Anwesha33/queryforge/internal/llm"
	"github.com/Anwesha33/queryforge/internal/optimizer"
	"github.com/Anwesha33/queryforge/internal/sqlparse"
)

type expectation struct {
	Finding string `json:"finding"`
	Rewrite bool   `json:"rewrite"`
	Index   bool   `json:"index"`
}

type benchQuery struct {
	ID          string      `json:"id"`
	Description string      `json:"description"`
	SQL         string      `json:"sql"`
	Control     bool        `json:"control"`
	Expects     expectation `json:"expects"`
}

type queryFile struct {
	Queries []benchQuery `json:"queries"`
}

type queryResult struct {
	ID          string  `json:"id"`
	Control     bool    `json:"control"`
	BaselineMS  float64 `json:"baseline_ms"`
	BestMS      float64 `json:"best_ms"`
	Improvement float64 `json:"improvement_pct"`
	Speedup     float64 `json:"speedup"`

	ExpectedFinding string   `json:"expected_finding,omitempty"`
	FoundExpected   bool     `json:"found_expected_finding"`
	FindingCodes    []string `json:"finding_codes"`

	CandidatesTested   int            `json:"candidates_tested"`
	CandidatesAccepted int            `json:"candidates_accepted"`
	RejectionReasons   map[string]int `json:"rejection_reasons,omitempty"`

	IndexesTested      int     `json:"indexes_tested"`
	IndexesRecommended int     `json:"indexes_recommended"`
	BestIndexGain      float64 `json:"best_index_gain_pct"`
	BestIndexDDL       string  `json:"best_index_ddl,omitempty"`

	AcceptedSQL string `json:"accepted_sql,omitempty"`
	DurationMS  int64  `json:"duration_ms"`
	Error       string `json:"error,omitempty"`
}

type report struct {
	Model      string        `json:"model"`
	LLMEnabled bool          `json:"llm_enabled"`
	RanAt      time.Time     `json:"ran_at"`
	Results    []queryResult `json:"results"`
	Totals     struct {
		Queries               int            `json:"queries"`
		Failed                int            `json:"failed"`
		ExpectedFindingsFound string         `json:"expected_findings_found"`
		QueriesImproved       int            `json:"queries_with_a_verified_improvement"`
		MedianSpeedup         float64        `json:"median_speedup_on_improved"`
		BestSpeedup           float64        `json:"best_speedup"`
		CandidatesTested      int            `json:"candidates_tested"`
		CandidatesAccepted    int            `json:"candidates_accepted"`
		RejectionReasons      map[string]int `json:"rejection_reasons"`
		IndexesTested         int            `json:"indexes_tested"`
		IndexesRecommended    int            `json:"indexes_recommended"`
		ControlFalsePositives int            `json:"control_false_positives"`
		TotalSecondsSaved     float64        `json:"total_ms_saved_per_execution"`
	} `json:"totals"`
}

func main() {
	var (
		queriesPath = flag.String("queries", "testdata/queries.json", "benchmark query file")
		outPath     = flag.String("out", "testdata/bench-report.json", "where to write the report")
		only        = flag.String("only", "", "run only queries whose id contains this string")
		skipLLM     = flag.Bool("skip-llm", false, "use rule-derived candidates only")
		skipIndexes = flag.Bool("skip-indexes", false, "skip index experiments (much faster)")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	slog.SetDefault(log)

	cfg, err := config.Load()
	if err != nil {
		fatal("config: %v", err)
	}

	raw, err := os.ReadFile(*queriesPath)
	if err != nil {
		fatal("read queries: %v", err)
	}
	var qf queryFile
	if err := json.Unmarshal(raw, &qf); err != nil {
		fatal("parse queries: %v", err)
	}

	ctx := context.Background()
	eng, err := engine.Open(ctx, cfg.TargetDSN, cfg.TargetDialect, engine.Timings{
		StatementTimeout: cfg.QueryTimeout, Runs: cfg.MeasurementRuns, WarmupRuns: cfg.WarmupRuns,
	})
	if err != nil {
		fatal("connect to the target database: %v", err)
	}
	defer eng.Close()

	model := llm.New(cfg.GeminiAPIKey, cfg.GeminiModel, cfg.LLMTimeout)
	useLLM := model.Enabled() && !*skipLLM

	opt := &optimizer.Optimizer{
		Engine: eng, LLM: model, Dialect: sqlparse.Dialect(eng.Dialect()),
		Opts: optimizer.Options{
			MaxCandidates: cfg.MaxCandidates, MinImprovementPct: cfg.MinImprovementPct,
			Runs: cfg.MeasurementRuns, TestIndexes: cfg.TestIndexes && !*skipIndexes,
			MaxIndexTests: cfg.MaxIndexTests, SkipLLM: !useLLM,
		},
		Log: quietLogger{},
	}

	rep := report{Model: cfg.GeminiModel, LLMEnabled: useLLM, RanAt: time.Now()}
	rep.Totals.RejectionReasons = map[string]int{}

	fmt.Printf("queryforge benchmark — %d queries, LLM %s\n\n",
		len(qf.Queries), map[bool]string{true: "on (" + cfg.GeminiModel + ")", false: "off"}[useLLM])

	for _, q := range qf.Queries {
		if *only != "" && !contains(q.ID, *only) {
			continue
		}
		res := runOne(ctx, opt, q)
		rep.Results = append(rep.Results, res)
		printOne(res)
	}

	summarize(&rep)
	printSummary(&rep)

	out, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(*outPath, out, 0o644); err != nil {
		fatal("write report: %v", err)
	}
	fmt.Printf("\nreport written to %s\n", *outPath)
}

func runOne(ctx context.Context, opt *optimizer.Optimizer, q benchQuery) queryResult {
	res := queryResult{
		ID: q.ID, Control: q.Control, ExpectedFinding: q.Expects.Finding,
		RejectionReasons: map[string]int{},
	}

	start := time.Now()
	rep, err := opt.Run(ctx, q.SQL)
	res.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = err.Error()
		return res
	}

	res.BaselineMS = rep.Baseline.MedianMS
	res.BestMS = rep.Baseline.MedianMS

	for _, f := range rep.Findings {
		res.FindingCodes = append(res.FindingCodes, f.Code)
		if q.Expects.Finding != "" && f.Code == q.Expects.Finding {
			res.FoundExpected = true
		}
	}
	if q.Expects.Finding == "" {
		res.FoundExpected = true // nothing was expected, so nothing was missed
	}

	res.CandidatesTested = len(rep.Candidates)
	for _, c := range rep.Candidates {
		if c.Verdict == optimizer.VerdictAccepted {
			res.CandidatesAccepted++
		} else {
			res.RejectionReasons[string(c.Reason)]++
		}
	}
	if rep.Best != nil {
		res.BestMS = rep.Best.CandidateMedianMS
		res.Improvement = rep.Best.ImprovementPct
		res.Speedup = rep.Best.SpeedupFactor
		res.AcceptedSQL = rep.Best.Candidate.SQL
	}

	res.IndexesTested = len(rep.Indexes)
	for _, idx := range rep.Indexes {
		if idx.Recommended {
			res.IndexesRecommended++
			if idx.ImprovementPct > res.BestIndexGain {
				res.BestIndexGain = idx.ImprovementPct
				res.BestIndexDDL = idx.DDL
			}
		}
	}
	return res
}

func printOne(r queryResult) {
	if r.Error != "" {
		fmt.Printf("  %-24s ERROR %s\n", r.ID, r.Error)
		return
	}
	tag := " "
	if r.Control {
		tag = "C"
	}
	found := "—"
	if r.ExpectedFinding != "" {
		found = map[bool]string{true: "found", false: "MISSED"}[r.FoundExpected]
	}
	rewrite := "—"
	if r.CandidatesAccepted > 0 {
		rewrite = fmt.Sprintf("%.0f%% faster", r.Improvement)
	}
	index := "—"
	if r.IndexesRecommended > 0 {
		index = fmt.Sprintf("%d rec (%.0f%%)", r.IndexesRecommended, r.BestIndexGain)
	}
	fmt.Printf("  %s %-24s %8.1fms  finding:%-7s rewrite:%-14s index:%-14s %d/%d cand\n",
		tag, r.ID, r.BaselineMS, found, rewrite, index, r.CandidatesAccepted, r.CandidatesTested)
}

func summarize(rep *report) {
	var speedups []float64
	expectedTotal, expectedFound := 0, 0

	for _, r := range rep.Results {
		rep.Totals.Queries++
		if r.Error != "" {
			rep.Totals.Failed++
			continue
		}
		if r.ExpectedFinding != "" {
			expectedTotal++
			if r.FoundExpected {
				expectedFound++
			}
		}
		rep.Totals.CandidatesTested += r.CandidatesTested
		rep.Totals.CandidatesAccepted += r.CandidatesAccepted
		for reason, n := range r.RejectionReasons {
			rep.Totals.RejectionReasons[reason] += n
		}
		rep.Totals.IndexesTested += r.IndexesTested
		rep.Totals.IndexesRecommended += r.IndexesRecommended

		if r.Control && (r.CandidatesAccepted > 0 || r.IndexesRecommended > 0) {
			rep.Totals.ControlFalsePositives++
		}
		if r.CandidatesAccepted > 0 {
			rep.Totals.QueriesImproved++
			speedups = append(speedups, r.Speedup)
			rep.Totals.TotalSecondsSaved += r.BaselineMS - r.BestMS
			if r.Speedup > rep.Totals.BestSpeedup {
				rep.Totals.BestSpeedup = r.Speedup
			}
		}
	}

	rep.Totals.ExpectedFindingsFound = fmt.Sprintf("%d/%d", expectedFound, expectedTotal)
	if len(speedups) > 0 {
		sort.Float64s(speedups)
		rep.Totals.MedianSpeedup = speedups[len(speedups)/2]
	}
}

func printSummary(rep *report) {
	t := rep.Totals
	fmt.Printf("\n  queries                      %d (%d failed)\n", t.Queries, t.Failed)
	fmt.Printf("  expected defects detected    %s\n", t.ExpectedFindingsFound)
	fmt.Printf("  queries improved             %d\n", t.QueriesImproved)
	if t.QueriesImproved > 0 {
		fmt.Printf("  speedup on improved          median %.1fx, best %.1fx\n", t.MedianSpeedup, t.BestSpeedup)
		fmt.Printf("  wall clock saved per run     %.0fms across all queries\n", t.TotalSecondsSaved)
	}
	fmt.Printf("  rewrites accepted/tested     %d/%d\n", t.CandidatesAccepted, t.CandidatesTested)
	fmt.Printf("  indexes recommended/tested   %d/%d\n", t.IndexesRecommended, t.IndexesTested)
	fmt.Printf("  false positives on controls  %d\n", t.ControlFalsePositives)
	if len(t.RejectionReasons) > 0 {
		fmt.Printf("  why candidates were rejected:\n")
		reasons := make([]string, 0, len(t.RejectionReasons))
		for r := range t.RejectionReasons {
			reasons = append(reasons, r)
		}
		sort.Strings(reasons)
		for _, r := range reasons {
			fmt.Printf("    %-32s %d\n", r, t.RejectionReasons[r])
		}
	}
}

type quietLogger struct{}

func (quietLogger) Info(string, ...any)  {}
func (quietLogger) Error(string, ...any) {}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "bench: "+format+"\n", args...)
	os.Exit(1)
}
