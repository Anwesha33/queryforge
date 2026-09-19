// Package api is queryforge's HTTP surface.
//
// The API never optimises anything itself: optimisation executes the query
// several times and may build an index, which is minutes of work. It validates,
// enqueues and returns 202.
//
// Validation here is not a formality. The parse happens at the edge so that a
// statement which is not a read-only SELECT is refused with a 400 the caller
// can act on, rather than accepted, queued, and failed invisibly in a worker.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/Anwesha33/queryforge/internal/config"
	"github.com/Anwesha33/queryforge/internal/queue"
	"github.com/Anwesha33/queryforge/internal/sqlparse"
	"github.com/Anwesha33/queryforge/internal/store"
)

type Server struct {
	cfg      *config.Config
	store    *store.Store
	producer *queue.Producer
	log      *slog.Logger
	started  time.Time
}

func NewServer(cfg *config.Config, st *store.Store, p *queue.Producer, log *slog.Logger) *Server {
	return &Server{cfg: cfg, store: st, producer: p, log: log, started: time.Now()}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/optimizations", s.createOptimization)
	mux.HandleFunc("GET /v1/optimizations", s.listOptimizations)
	mux.HandleFunc("GET /v1/optimizations/{id}", s.getOptimization)
	mux.HandleFunc("POST /v1/analyze", s.analyzeOnly)
	mux.HandleFunc("GET /v1/history", s.history)
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	return s.withLogging(mux)
}

type createRequest struct {
	SQL            string `json:"sql"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type createResponse struct {
	JobID       uuid.UUID `json:"job_id"`
	Status      string    `json:"status"`
	Fingerprint string    `json:"fingerprint"`
	Deduped     bool      `json:"deduped"`
	StatusURL   string    `json:"status_url"`
}

func (s *Server) createOptimization(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return
	}

	// Parse before accepting. A statement that can write is refused here, with
	// a reason, rather than being queued and failing out of sight.
	st, err := sqlparse.Parse(req.SQL)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, sqlparse.ErrNotReadOnly) {
			// 422: the request is well formed, the statement is simply not
			// something this service is willing to run.
			status = http.StatusUnprocessableEntity
		}
		writeError(w, status, err.Error())
		return
	}

	key := req.IdempotencyKey
	if key == "" {
		// Default to the exact statement text. Two identical submissions
		// deduplicate; a submission differing only in literals does not,
		// because different literals can have very different plans.
		key = "sql:" + st.SQL
	}

	job, created, err := s.store.CreateJob(r.Context(), &store.Job{
		IdempotencyKey: key, SQL: st.SQL, Fingerprint: st.Fingerprint,
		MaxAttempts: s.cfg.MaxDeliveryAttempt,
	})
	if err != nil {
		s.log.Error("create job", "err", err)
		writeError(w, http.StatusInternalServerError, "could not create job")
		return
	}

	if created {
		if err := s.producer.Publish(r.Context(), s.cfg.JobTopic, queue.OptimizeRequest{
			JobID: job.ID, SQL: job.SQL, Attempt: 1, EnqueuedAt: time.Now(),
		}); err != nil {
			s.log.Error("publish job", "err", err, "job_id", job.ID)
			writeError(w, http.StatusServiceUnavailable, "job recorded but could not be queued; it will be retried")
			return
		}
	}

	status := http.StatusAccepted
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, createResponse{
		JobID: job.ID, Status: job.Status, Fingerprint: job.Fingerprint,
		Deduped: !created, StatusURL: "/v1/optimizations/" + job.ID.String(),
	})
}

// analyzeOnly runs the parser and returns what the statement is, without
// touching the target database. It exists because "will this be accepted, and
// what does the parser think it does" is a question worth answering in
// milliseconds.
func (s *Server) analyzeOnly(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	st, err := sqlparse.Parse(req.SQL)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, sqlparse.ErrNotReadOnly) {
			status = http.StatusUnprocessableEntity
		}
		writeError(w, status, err.Error())
		return
	}
	tables := make([]string, 0, len(st.Tables))
	for _, t := range st.Tables {
		tables = append(tables, t.Qualified())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"fingerprint":    st.Fingerprint,
		"normalized":     st.Normalized,
		"tables":         tables,
		"output_columns": st.OutputCols,
		"select_star":    st.SelectStar,
		"has_order_by":   st.HasOrderBy,
		"has_limit":      st.HasLimit,
		"read_only":      true,
	})
}

func (s *Server) getOptimization(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid job id")
		return
	}
	job, err := s.store.JobByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such job")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) listOptimizations(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	jobs, err := s.store.ListJobs(r.Context(), r.URL.Query().Get("status"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Reports are large; the list view omits them.
	for _, j := range jobs {
		j.Report = nil
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs, "count": len(jobs)})
}

// history answers "how has this query shape performed over time", keyed by
// fingerprint so different literals count as the same query.
func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	fingerprint := r.URL.Query().Get("fingerprint")
	if sql := r.URL.Query().Get("sql"); sql != "" {
		st, err := sqlparse.Parse(sql)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		fingerprint = st.Fingerprint
	}
	if fingerprint == "" {
		writeError(w, http.StatusBadRequest, "provide fingerprint or sql")
		return
	}
	h, err := s.store.HistoryFor(r.Context(), fingerprint)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "uptime_seconds": int(time.Since(s.started).Seconds()),
	})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "degraded", "metadata_db": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("http", "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration_ms", time.Since(start).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
