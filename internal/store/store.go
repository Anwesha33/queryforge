// Package store persists optimisation jobs and the history of every measured
// query run.
package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusRetrying  = "retrying"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

var (
	ErrNotFound       = errors.New("not found")
	ErrAlreadyClaimed = errors.New("job already claimed")
)

type Store struct{ pool *pgxpool.Pool }

func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 8
	cfg.MaxConnLifetime = time.Hour
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close()                         { s.pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, schemaSQL)
	return err
}

type Job struct {
	ID             uuid.UUID       `json:"id"`
	IdempotencyKey string          `json:"idempotency_key"`
	SQL            string          `json:"sql"`
	Fingerprint    string          `json:"fingerprint"`
	Status         string          `json:"status"`
	Attempts       int             `json:"attempts"`
	MaxAttempts    int             `json:"max_attempts"`
	LastError      string          `json:"last_error,omitempty"`
	Report         json.RawMessage `json:"report,omitempty"`
	BaselineMS     float64         `json:"baseline_ms"`
	BestMS         float64         `json:"best_ms"`
	ImprovementPct float64         `json:"improvement_pct"`
	DurationMS     int64           `json:"duration_ms"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	FinishedAt     *time.Time      `json:"finished_at,omitempty"`
}

const jobCols = `id, idempotency_key, sql_text, fingerprint, status, attempts, max_attempts,
	last_error, report, baseline_ms, best_ms, improvement_pct, duration_ms,
	created_at, updated_at, finished_at`

func scanJob(row pgx.Row) (*Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.IdempotencyKey, &j.SQL, &j.Fingerprint, &j.Status, &j.Attempts,
		&j.MaxAttempts, &j.LastError, &j.Report, &j.BaselineMS, &j.BestMS, &j.ImprovementPct,
		&j.DurationMS, &j.CreatedAt, &j.UpdatedAt, &j.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

func (s *Store) CreateJob(ctx context.Context, j *Job) (*Job, bool, error) {
	if j.ID == uuid.Nil {
		j.ID = uuid.New()
	}
	if j.MaxAttempts == 0 {
		j.MaxAttempts = 3
	}
	const q = `INSERT INTO optimization_jobs (id, idempotency_key, sql_text, fingerprint, status, max_attempts)
	           VALUES ($1,$2,$3,$4,$5,$6)
	           ON CONFLICT (idempotency_key) DO NOTHING
	           RETURNING ` + jobCols
	created, err := scanJob(s.pool.QueryRow(ctx, q, j.ID, j.IdempotencyKey, j.SQL, j.Fingerprint,
		StatusQueued, j.MaxAttempts))
	if err == nil {
		return created, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}
	existing, err := scanJob(s.pool.QueryRow(ctx,
		`SELECT `+jobCols+` FROM optimization_jobs WHERE idempotency_key = $1`, j.IdempotencyKey))
	return existing, false, err
}

func (s *Store) JobByID(ctx context.Context, id uuid.UUID) (*Job, error) {
	return scanJob(s.pool.QueryRow(ctx, `SELECT `+jobCols+` FROM optimization_jobs WHERE id = $1`, id))
}

// ClaimJob is the concurrency control: the WHERE clause means exactly one
// worker wins a race over the same Kafka message.
func (s *Store) ClaimJob(ctx context.Context, id uuid.UUID) (*Job, error) {
	const q = `UPDATE optimization_jobs
	           SET status=$2, attempts=attempts+1, started_at=now(), updated_at=now()
	           WHERE id=$1 AND status IN ($3,$4)
	           RETURNING ` + jobCols
	j, err := scanJob(s.pool.QueryRow(ctx, q, id, StatusRunning, StatusQueued, StatusRetrying))
	if errors.Is(err, ErrNotFound) {
		if _, e2 := s.JobByID(ctx, id); e2 == nil {
			return nil, ErrAlreadyClaimed
		}
		return nil, ErrNotFound
	}
	return j, err
}

func (s *Store) CompleteJob(ctx context.Context, id uuid.UUID, report any,
	baselineMS, bestMS, improvementPct float64, durationMS int64) error {

	raw, err := json.Marshal(report)
	if err != nil {
		return err
	}
	const q = `UPDATE optimization_jobs
	           SET status=$2, report=$3, baseline_ms=$4, best_ms=$5, improvement_pct=$6,
	               duration_ms=$7, last_error='', finished_at=now(), updated_at=now()
	           WHERE id=$1`
	_, err = s.pool.Exec(ctx, q, id, StatusSucceeded, raw, baselineMS, bestMS, improvementPct, durationMS)
	return err
}

func (s *Store) MarkFailure(ctx context.Context, id uuid.UUID, cause string) (willRetry bool, err error) {
	const q = `UPDATE optimization_jobs
	           SET status = CASE WHEN attempts < max_attempts THEN $2 ELSE $3 END,
	               last_error = $4,
	               finished_at = CASE WHEN attempts < max_attempts THEN NULL ELSE now() END,
	               updated_at = now()
	           WHERE id = $1
	           RETURNING status`
	var status string
	err = s.pool.QueryRow(ctx, q, id, StatusRetrying, StatusFailed, truncate(cause, 4000)).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	return status == StatusRetrying, err
}

// ReclaimStale rescues jobs whose worker died mid-run.
func (s *Store) ReclaimStale(ctx context.Context, olderThan time.Duration) (int64, error) {
	const q = `UPDATE optimization_jobs
	           SET status = CASE WHEN attempts < max_attempts THEN $1 ELSE $2 END,
	               last_error = 'worker lease expired', updated_at = now()
	           WHERE status = $3 AND started_at < now() - $4::interval`
	tag, err := s.pool.Exec(ctx, q, StatusRetrying, StatusFailed, StatusRunning,
		fmt.Sprintf("%d seconds", int(olderThan.Seconds())))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *Store) ListJobs(ctx context.Context, status string, limit int) ([]*Job, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT ` + jobCols + ` FROM optimization_jobs`
	args := []any{}
	if status != "" {
		q += ` WHERE status = $1`
		args = append(args, status)
	}
	q += fmt.Sprintf(` ORDER BY created_at DESC LIMIT %d`, limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Run is one measured execution, kept as history.
type Run struct {
	JobID       *uuid.UUID
	Fingerprint string
	Role        string
	SQL         string
	MedianMS    float64
	MinMS       float64
	MaxMS       float64
	TotalCost   float64
	RowCount    int64
	Runs        int
}

func (s *Store) RecordRun(ctx context.Context, r Run) error {
	const q = `INSERT INTO query_runs (job_id, fingerprint, role, sql_text, median_ms, min_ms, max_ms, total_cost, row_count, runs)
	           VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`
	_, err := s.pool.Exec(ctx, q, r.JobID, r.Fingerprint, r.Role, r.SQL,
		r.MedianMS, r.MinMS, r.MaxMS, r.TotalCost, r.RowCount, r.Runs)
	return err
}

// History is what this query has done before, by fingerprint.
type History struct {
	Fingerprint  string    `json:"fingerprint"`
	Observations int       `json:"observations"`
	BestMS       float64   `json:"best_median_ms"`
	WorstMS      float64   `json:"worst_median_ms"`
	LatestMS     float64   `json:"latest_median_ms"`
	FirstSeen    time.Time `json:"first_seen"`
	LastSeen     time.Time `json:"last_seen"`
}

// HistoryFor returns prior measurements of the same query shape. This is the
// "historical query metrics" input: it lets a report say "this query used to
// run in 40ms and now takes 900ms", which points at a data or statistics change
// rather than at the SQL.
func (s *Store) HistoryFor(ctx context.Context, fingerprint string) (*History, error) {
	const q = `
	SELECT count(*), COALESCE(min(median_ms),0), COALESCE(max(median_ms),0),
	       COALESCE(min(created_at), now()), COALESCE(max(created_at), now()),
	       COALESCE((SELECT median_ms FROM query_runs
	                 WHERE fingerprint=$1 AND role='baseline'
	                 ORDER BY created_at DESC LIMIT 1), 0)
	FROM query_runs WHERE fingerprint = $1 AND role = 'baseline'`
	h := &History{Fingerprint: fingerprint}
	err := s.pool.QueryRow(ctx, q, fingerprint).Scan(&h.Observations, &h.BestMS, &h.WorstMS,
		&h.FirstSeen, &h.LastSeen, &h.LatestMS)
	if err != nil {
		return nil, err
	}
	return h, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
