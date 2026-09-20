# queryforge

A SQL optimisation service that refuses to take an LLM's word for anything.

You give it a query. It parses it with PostgreSQL's own grammar, introspects the
schema and statistics, measures the query, finds defects with a deterministic
rule engine, asks a model for rewrites — and then **proves or disproves every
proposal by execution** before showing you a single one.

```
                    SQL
                     │
              ┌──────▼───────┐
              │    parser    │  libpg_query — the server's own grammar
              └──────┬───────┘
                     │  read-only SELECT only, verified structurally
      ┌──────────────┼──────────────┬──────────────────┐
      ▼              ▼              ▼                  ▼
  schema +        EXPLAIN       EXPLAIN ANALYZE    historical
  index info      (plan)        (median of N)      metrics
      └──────────────┼──────────────┴──────────────────┘
                     ▼
            ┌────────────────┐        ┌──────────────────┐
            │  rule engine   │───────▶│   AI proposer    │
            │ (deterministic)│        │ (candidates only)│
            └────────┬───────┘        └────────┬─────────┘
                     └──────────┬──────────────┘
                                ▼
                    ╔═══════════════════════╗
                    ║   VERIFICATION LOOP   ║
                    ╟───────────────────────╢
                    ║ parses & read-only?   ║──no──▶ reject
                    ║ same output shape?    ║──no──▶ reject
                    ║ identical results?    ║──no──▶ reject
                    ║ faster beyond noise?  ║──no──▶ reject
                    ╚═══════════╤═══════════╝
                                ▼
                     accepted, with evidence
```

Nothing reaches a human without an identical result checksum and a measured
speedup behind it.

## Measured results

`make bench` against 1.5M orders / 300k users / 2M order items. Full numbers and
caveats in [docs/RESULTS.md](docs/RESULTS.md).

| | Rules only (no LLM) | Rules + LLM |
| --- | ---: | ---: |
| Expected defects detected | 7/7 | 7/7 |
| Queries with a verified improvement | 1 | **3** |
| Rewrites accepted / tested | 1/1 | **4/5** |
| Best verified speedup | 1.7× | **1.9×** |
| Indexes recommended / measured | 4/9 | 4/9 |
| False positives on control queries | **0** | **0** |

Two things worth reading twice. **Five of nine candidate indexes were measured
and rejected** — the service builds each one inside a transaction, re-times the
query, and rolls back, so "add an index on that column" is a measurement rather
than folklore. And **the whole pipeline works with no API key at all**: the rule
engine alone found every planted defect and produced a verified 1.7× rewrite.

The rewrite from the classic example:

```sql
-- original, 52.5ms
WHERE DATE(o.created_at) = '2026-06-01' AND u.country = 'IN'

-- accepted, 27.0ms (49% faster, byte-identical results over the same rows)
WHERE o.created_at >= '2026-06-01 00:00:00'
  AND o.created_at <  '2026-06-02 00:00:00'
  AND u.country = 'IN'
```

## Quick start

```bash
cp .env.example .env      # GEMINI_API_KEY optional — it runs without one
make up                   # postgres + redis + kafka + api + worker, seeds 1.5M rows
make optimize SQL="SELECT * FROM orders WHERE DATE(created_at) = '2026-06-01'"
make report JOB=<uuid from the response>
make bench                # score the optimizer against the benchmark suite
```

## Why the LLM is not in charge

An LLM is good at proposing a rewrite and has no idea whether it is correct. The
plausible wrong answer is the dangerous one: `NOT IN` → `NOT EXISTS` is usually
right and silently changes results when NULLs are involved; dropping a join
"that contributes nothing" changes row multiplicity whenever it was not a
one-to-one relationship.

So the model is one candidate source among two, and its output is subject to the
same gate as everything else:

1. **It must parse as a read-only SELECT.** Checked with libpg_query, the same
   parser the server uses — so a `WITH deleted AS (DELETE ... RETURNING *)`
   smuggled inside a SELECT is caught, which no regular expression manages.
2. **It must have the same output shape** — column count, column names, and no
   table the original did not read.
3. **It must return identical results.** Both queries are executed and their
   result sets hashed. Row order is part of the hash only when the query has an
   `ORDER BY`, because only then does it promise one.
4. **It must be measurably faster** — past a configurable threshold *and* past
   the run-to-run spread of the measurements themselves.

A candidate that fails is recorded with the reason, so rejection is a number you
can look at rather than something that happens quietly.

## What it finds

Deterministic rules, each carrying its evidence:

| Rule | What it catches |
| --- | --- |
| `non-sargable-predicate` | A function or cast around a column, defeating any index on it. Generates a rewrite for the `DATE(ts) = 'x'` case. |
| `missing-index` | A filter or join column with no index leading on it — suppressed for tiny tables and near-constant columns. |
| `low-selectivity-column` | A column where an index would cost writes and change nothing. |
| `leading-wildcard-like` | `LIKE '%x'`, which no B-tree can range-scan. |
| `not-in-subquery` | The NULL trap, and usually a worse plan than `NOT EXISTS`. |
| `select-star` | Reads every column, prevents index-only scans. |
| `limit-without-order-by` | A correctness bug that looks stable until the plan changes. |
| `seq-scan-large-table` | From the executed plan, not the SQL. |
| `row-estimate-off` | The planner's estimate was wrong by 10× or more — the fix is `ANALYZE`, not a rewrite. |
| `sort-spilled-to-disk` | A sort or hash exceeded `work_mem`. |

## Index experiments

The part that makes index advice trustworthy:

```go
BEGIN;
CREATE INDEX idx_orders_total_cents ON orders (total_cents);
EXPLAIN (ANALYZE) <the original query>;   -- re-measured with the index live
ROLLBACK;                                  -- it never existed
```

PostgreSQL has transactional DDL, so the index is real for the duration of the
measurement and gone afterwards. Measured gains on the benchmark ranged from 99%
down to 14%, and five candidates were measured and *not* recommended.

This is also why the service targets PostgreSQL and not MySQL: MySQL commits DDL
implicitly, so the same experiment would leave a real index behind on someone
else's database. Supporting it needs a different strategy — a shadow schema, or
accepting cost-model estimates instead of measurements — and doing it properly
is a larger piece of work than pretending the interface is enough.

## API

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/v1/optimizations` | Queue a query. `202` with a job id; `422` if the statement is not a read-only SELECT. |
| `GET` | `/v1/optimizations/{id}` | The full report: baseline, findings, every candidate with its verdict, index experiments. |
| `GET` | `/v1/optimizations?status=failed` | Recent jobs. |
| `POST` | `/v1/analyze` | Parse only, no database access — fingerprint, tables, output columns. |
| `GET` | `/v1/history?sql=...` | How this query shape has performed over time, keyed by fingerprint. |
| `GET` | `/healthz`, `/readyz` | Liveness and readiness. |

## Safety

- **Read-only by construction, twice.** The parser rejects anything that can
  write, and every measurement runs inside a `READ ONLY` transaction — so even a
  statement that somehow slipped past the parser is refused by the server.
- **Statement timeouts** on everything, so a pathological query cannot pin a
  worker.
- **Index experiments always roll back**, including on the success path.
- **Point `TARGET_DSN` at a replica.** Index builds are not `CONCURRENTLY`; they
  take a brief exclusive lock on the table.

## Configuration

| Variable | Default | Notes |
| --- | --- | --- |
| `TARGET_DSN` | local `shop` database | The database being optimised. Use a replica. |
| `METADATA_DSN` | local `queryforge` database | Jobs and measurement history. |
| `GEMINI_API_KEY` | — | Optional. Without it, rules and index experiments still run. |
| `MEASUREMENT_RUNS` | `5` | Timed runs per query; must be ≥ 3 for a meaningful median. |
| `WARMUP_RUNS` | `1` | Discarded runs, so cold caches do not flatter every rewrite. |
| `MIN_IMPROVEMENT_PCT` | `10` | The floor a rewrite must clear. |
| `MAX_CANDIDATES` | `4` | Rewrites requested from the model per query. |
| `MAX_INDEX_TESTS` | `3` | Index experiments per job — the slowest phase. |
| `QUERY_TIMEOUT` | `30s` | Statement timeout on the target database. |

## Repository layout

```
cmd/api            Validate, enqueue, return 202
cmd/worker         Kafka consumers, retry tiers, stale-job reclaimer
cmd/bench          Scores the optimizer against the benchmark suite
internal/sqlparse  libpg_query wrapper: read-only proof, tables, output shape
internal/engine    Introspection, EXPLAIN, timing, checksums, index experiments
internal/rules     Deterministic findings and rewrites
internal/optimizer The pipeline and the verification loop
internal/llm       The proposer — candidates only, never trusted
testdata/          Benchmark schema, queries and reports
docs/              Architecture, results, interview guide
```

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — why each piece is shaped this way
- [docs/RESULTS.md](docs/RESULTS.md) — the benchmark, in full, with its limits
