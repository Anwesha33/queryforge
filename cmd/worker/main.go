// Command worker consumes optimisation jobs and runs the pipeline.
//
// Concurrency is deliberately low by default. Every job executes real queries
// against the target database several times, and an optimiser that saturates
// the database it is measuring produces measurements of itself.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/Anwesha33/queryforge/internal/cache"
	"github.com/Anwesha33/queryforge/internal/config"
	"github.com/Anwesha33/queryforge/internal/engine"
	"github.com/Anwesha33/queryforge/internal/llm"
	"github.com/Anwesha33/queryforge/internal/optimizer"
	"github.com/Anwesha33/queryforge/internal/queue"
	"github.com/Anwesha33/queryforge/internal/sqlparse"
	"github.com/Anwesha33/queryforge/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := config.Load()
	if err != nil {
		log.Error("configuration", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := openStoreWithRetry(ctx, cfg.MetadataDSN, log)
	if err != nil {
		log.Error("metadata database", "err", err)
		os.Exit(1)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		log.Error("migrate", "err", err)
		os.Exit(1)
	}

	target, err := openTargetWithRetry(ctx, cfg, log)
	if err != nil {
		log.Error("target database", "err", err)
		os.Exit(1)
	}
	defer target.Close()

	rdb := cache.New(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB)
	if err := rdb.Ping(ctx); err != nil {
		log.Warn("redis unavailable; running without cache", "err", err)
		rdb = nil
	} else {
		defer rdb.Close()
	}

	model := llm.New(cfg.GeminiAPIKey, cfg.GeminiModel, cfg.LLMTimeout)
	if !model.Enabled() {
		// Worth saying loudly rather than failing: the rule engine and the
		// index experiments work without a model, so the service degrades to
		// "deterministic optimiser" rather than stopping.
		log.Warn("GEMINI_API_KEY is not set: running with rule-based candidates only")
	}

	producer := queue.NewProducer(cfg.KafkaBrokers)
	defer producer.Close()

	opt := &optimizer.Optimizer{
		Engine: target,
		LLM:    model,
		Opts: optimizer.Options{
			MaxCandidates:     cfg.MaxCandidates,
			MinImprovementPct: cfg.MinImprovementPct,
			Runs:              cfg.MeasurementRuns,
			TestIndexes:       cfg.TestIndexes,
			MaxIndexTests:     cfg.MaxIndexTests,
			SkipLLM:           !model.Enabled(),
		},
		Log:     slogAdapter{log},
		Dialect: sqlparse.Dialect(target.Dialect()),
	}

	w := &worker{cfg: cfg, store: st, producer: producer, opt: opt, log: log}

	var wg sync.WaitGroup
	for i := 0; i < cfg.WorkerConcurrency; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			w.consumeMain(ctx, n)
		}(i)
	}
	for tier := 1; tier <= len(queue.RetryTiers); tier++ {
		topic, delay, _ := queue.RetryTopic(cfg.JobTopic, tier)
		wg.Add(1)
		go func(topic string, delay time.Duration) {
			defer wg.Done()
			w.consumeRetry(ctx, topic, delay)
		}(topic, delay)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.reclaimLoop(ctx)
	}()

	log.Info("worker started", "concurrency", cfg.WorkerConcurrency,
		"measurement_runs", cfg.MeasurementRuns, "min_improvement_pct", cfg.MinImprovementPct,
		"index_experiments", cfg.TestIndexes, "llm", model.Enabled())

	<-ctx.Done()
	log.Info("shutdown signal received; finishing in-flight jobs")
	wg.Wait()
	log.Info("worker stopped")
}

type worker struct {
	cfg      *config.Config
	store    *store.Store
	producer *queue.Producer
	opt      *optimizer.Optimizer
	log      *slog.Logger
}

func (w *worker) consumeMain(ctx context.Context, n int) {
	c := queue.NewConsumer(w.cfg.KafkaBrokers, w.cfg.JobTopic, w.cfg.ConsumerGrp)
	defer c.Close()
	for {
		msg, err := c.Fetch(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			w.log.Error("fetch", "consumer", n, "err", err)
			time.Sleep(time.Second)
			continue
		}
		w.handle(ctx, msg.Request)
		if err := c.Commit(ctx, msg); err != nil {
			w.log.Error("commit", "err", err, "job_id", msg.Request.JobID)
		}
	}
}

func (w *worker) consumeRetry(ctx context.Context, topic string, delay time.Duration) {
	c := queue.NewConsumer(w.cfg.KafkaBrokers, topic, w.cfg.ConsumerGrp+".retry")
	defer c.Close()
	for {
		msg, err := c.Fetch(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(time.Second)
			continue
		}
		if wait := time.Until(msg.Request.NotBefore); wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
		req := msg.Request
		req.NotBefore = time.Time{}
		if err := w.producer.Publish(ctx, w.cfg.JobTopic, req); err != nil {
			w.log.Error("republish from retry tier", "topic", topic, "err", err)
			continue
		}
		if err := c.Commit(ctx, msg); err != nil {
			w.log.Error("commit retry", "err", err)
		}
	}
}

func (w *worker) handle(ctx context.Context, req queue.OptimizeRequest) {
	log := w.log.With("job_id", req.JobID)

	job, err := w.store.ClaimJob(ctx, req.JobID)
	switch {
	case errors.Is(err, store.ErrAlreadyClaimed):
		log.Info("skipping duplicate delivery; job already claimed")
		return
	case errors.Is(err, store.ErrNotFound):
		log.Warn("message references an unknown job; dropping")
		return
	case err != nil:
		log.Error("claim job", "err", err)
		return
	}

	jobCtx, cancel := context.WithTimeout(ctx, w.cfg.JobTimeout)
	defer cancel()

	start := time.Now()
	report, err := w.opt.Run(jobCtx, job.SQL)
	if err != nil {
		log.Error("optimisation failed", "err", err, "attempt", job.Attempts)
		w.scheduleRetry(ctx, job, req, err.Error())
		return
	}

	// Record every measurement as history, so a later run of the same query
	// shape can be compared against it.
	w.recordRuns(ctx, job, report)

	bestMS, improvement := report.Baseline.MedianMS, 0.0
	if report.Best != nil {
		bestMS = report.Best.CandidateMedianMS
		improvement = report.Best.ImprovementPct
	}
	if err := w.store.CompleteJob(ctx, job.ID, report,
		report.Baseline.MedianMS, bestMS, improvement, time.Since(start).Milliseconds()); err != nil {
		log.Error("persist report", "err", err)
	}

	log.Info("optimisation complete",
		"baseline_ms", report.Baseline.MedianMS, "best_ms", bestMS,
		"improvement_pct", improvement, "candidates", len(report.Candidates),
		"indexes_tested", len(report.Indexes), "duration_ms", time.Since(start).Milliseconds())
}

func (w *worker) recordRuns(ctx context.Context, job *store.Job, report *optimizer.Report) {
	id := job.ID
	_ = w.store.RecordRun(ctx, store.Run{
		JobID: &id, Fingerprint: job.Fingerprint, Role: "baseline", SQL: job.SQL,
		MedianMS: report.Baseline.MedianMS, MinMS: report.Baseline.MinMS, MaxMS: report.Baseline.MaxMS,
		TotalCost: report.Baseline.TotalCost, RowCount: report.Baseline.RowCount,
		Runs: w.cfg.MeasurementRuns,
	})
	for _, c := range report.Candidates {
		if c.Verdict != optimizer.VerdictAccepted {
			continue
		}
		fp := ""
		if st, err := sqlparse.ParseDialect(c.Candidate.SQL, w.opt.Dialect); err == nil {
			fp = st.Fingerprint
		}
		_ = w.store.RecordRun(ctx, store.Run{
			JobID: &id, Fingerprint: fp, Role: "candidate", SQL: c.Candidate.SQL,
			MedianMS: c.CandidateMedianMS, TotalCost: c.CandidateCost,
			RowCount: c.RowCount, Runs: w.cfg.MeasurementRuns,
		})
	}
}

func (w *worker) scheduleRetry(ctx context.Context, job *store.Job, req queue.OptimizeRequest, cause string) {
	willRetry, err := w.store.MarkFailure(ctx, job.ID, cause)
	if err != nil {
		w.log.Error("mark failure", "err", err, "job_id", job.ID)
	}
	next := req
	next.Attempt = job.Attempts + 1
	next.LastError = cause

	if willRetry {
		if topic, delay, ok := queue.RetryTopic(w.cfg.JobTopic, job.Attempts); ok {
			next.NotBefore = time.Now().Add(delay)
			if pubErr := w.producer.Publish(ctx, topic, next); pubErr == nil {
				w.log.Info("scheduled retry", "job_id", job.ID, "topic", topic, "delay", delay.String())
				return
			}
		}
	}
	next.NotBefore = time.Time{}
	if err := w.producer.Publish(ctx, w.cfg.DLQTopic, next); err != nil {
		w.log.Error("publish to DLQ", "err", err, "job_id", job.ID)
		return
	}
	w.log.Warn("job dead-lettered", "job_id", job.ID, "attempts", job.Attempts, "cause", cause)
}

func (w *worker) reclaimLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := w.store.ReclaimStale(ctx, 2*w.cfg.JobTimeout)
			if err != nil {
				w.log.Error("reclaim stale jobs", "err", err)
				continue
			}
			if n > 0 {
				w.log.Warn("reclaimed stale jobs", "count", n)
			}
		}
	}
}

type slogAdapter struct{ l *slog.Logger }

func (s slogAdapter) Info(msg string, kv ...any)  { s.l.Info(msg, kv...) }
func (s slogAdapter) Error(msg string, kv ...any) { s.l.Error(msg, kv...) }

func openStoreWithRetry(ctx context.Context, dsn string, log *slog.Logger) (*store.Store, error) {
	var lastErr error
	for attempt := 0; attempt < 30; attempt++ {
		st, err := store.Open(ctx, dsn)
		if err == nil {
			return st, nil
		}
		lastErr = err
		log.Info("waiting for the metadata database", "attempt", attempt+1)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, lastErr
}

func openTargetWithRetry(ctx context.Context, cfg *config.Config, log *slog.Logger) (engine.Engine, error) {
	timings := engine.Timings{
		StatementTimeout: cfg.QueryTimeout,
		Runs:             cfg.MeasurementRuns,
		WarmupRuns:       cfg.WarmupRuns,
	}
	var lastErr error
	for attempt := 0; attempt < 30; attempt++ {
		eng, err := engine.Open(ctx, cfg.TargetDSN, cfg.TargetDialect, timings)
		if err == nil {
			return eng, nil
		}
		lastErr = err
		log.Info("waiting for the target database", "attempt", attempt+1)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, lastErr
}
