-- queryforge schema. Applied idempotently at process start.

CREATE TABLE IF NOT EXISTS optimization_jobs (
    id              UUID PRIMARY KEY,
    -- Kafka is at-least-once and clients retry; this makes the pipeline
    -- effectively-once.
    idempotency_key TEXT        NOT NULL UNIQUE,
    sql_text        TEXT        NOT NULL,
    -- The structural fingerprint from libpg_query: two statements differing
    -- only in their literals share one, which is what makes "how has this
    -- query performed over time" a question with an answer.
    fingerprint     TEXT        NOT NULL DEFAULT '',
    status          TEXT        NOT NULL,
    attempts        INTEGER     NOT NULL DEFAULT 0,
    max_attempts    INTEGER     NOT NULL DEFAULT 3,
    last_error      TEXT        NOT NULL DEFAULT '',
    report          JSONB,
    baseline_ms     DOUBLE PRECISION NOT NULL DEFAULT 0,
    best_ms         DOUBLE PRECISION NOT NULL DEFAULT 0,
    improvement_pct DOUBLE PRECISION NOT NULL DEFAULT 0,
    duration_ms     BIGINT      NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at      TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS optimization_jobs_status_idx ON optimization_jobs (status, created_at DESC);
CREATE INDEX IF NOT EXISTS optimization_jobs_fp_idx     ON optimization_jobs (fingerprint, created_at DESC);

-- Every measured execution, whether of an original or a candidate. This is the
-- "historical query metrics" input: the optimizer can see that a query it is
-- being asked about has been measured before, and how it behaved.
CREATE TABLE IF NOT EXISTS query_runs (
    id            BIGSERIAL PRIMARY KEY,
    job_id        UUID REFERENCES optimization_jobs(id) ON DELETE CASCADE,
    fingerprint   TEXT             NOT NULL,
    role          TEXT             NOT NULL,  -- 'baseline' | 'candidate' | 'index_experiment'
    sql_text      TEXT             NOT NULL,
    median_ms     DOUBLE PRECISION NOT NULL,
    min_ms        DOUBLE PRECISION NOT NULL DEFAULT 0,
    max_ms        DOUBLE PRECISION NOT NULL DEFAULT 0,
    total_cost    DOUBLE PRECISION NOT NULL DEFAULT 0,
    row_count     BIGINT           NOT NULL DEFAULT 0,
    runs          INTEGER          NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ      NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS query_runs_fp_idx  ON query_runs (fingerprint, created_at DESC);
CREATE INDEX IF NOT EXISTS query_runs_job_idx ON query_runs (job_id);
