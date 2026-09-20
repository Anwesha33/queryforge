# Benchmark results

```bash
make up            # seeds the benchmark database
make bench         # rules + LLM
make bench-rules   # rules only, no API key required
```

## Scope

**Every number in this document was measured against PostgreSQL.** MySQL is
supported for everything except index experiments, and it has no benchmark of
its own: the index-experiment column is the headline result here, and it is
exactly the thing MySQL cannot do. The MySQL path is covered by integration
tests against a live database rather than by a scored benchmark — see
`internal/engine/mysql_integration_test.go` and
`internal/optimizer/mysql_e2e_test.go`, run with `make mysql-up && make test-mysql`.

Building a MySQL benchmark would mean porting the nine queries and re-measuring,
and its results table would be missing the column that makes this one
interesting. That is a fair description of the gap rather than a reason to
publish an incomparable number.

## The benchmark

A deliberately under-indexed PostgreSQL database — only primary keys exist —
holding 1.5M orders, 2M order items, 300k users and a 10-row lookup table. Row
counts were chosen so that a sequential scan is genuinely slow: below roughly a
million rows Postgres scans fast enough that no optimisation is measurable, and
a benchmark where nothing can improve proves nothing.

Nine queries. Seven contain a specific, named defect, and the benchmark records
which finding a correct optimizer should produce — so the harness reports not
only "did it get faster" but "did it find the thing that was actually wrong".
Two are **controls**: a primary-key lookup and a scan of a ten-row table, both
already optimal. Anything accepted on a control is a false positive, and false
positives are what make an optimisation tool get switched off.

## Results

`gemini-3.5-flash-lite`, 5 timed runs per measurement plus 1 discarded warm-up,
minimum improvement 10%.

| | Rules only | Rules + LLM |
| --- | ---: | ---: |
| Expected defects detected | 7/7 | 7/7 |
| Queries with a verified improvement | 1 | 3 |
| Rewrites accepted / tested | 1/1 | 4/5 |
| Median speedup on improved queries | 1.7× | 1.6× |
| Best verified speedup | 1.7× | 1.9× |
| Wall-clock saved per execution, all queries | 23ms | 72ms |
| Indexes recommended / measured | 4/9 | 4/9 |
| **False positives on controls** | **0** | **0** |

### Per query (rules + LLM)

| Query | Baseline | Finding | Verified rewrite | Best measured index |
| --- | ---: | --- | --- | --- |
| `q01-date-function` | 52.5ms | found | **49% faster** | — |
| `q02-cast-on-column` | 30.2ms | found | **36% faster** | — |
| `q03-leading-wildcard` | 15.8ms | found | — | — |
| `q04-unindexed-join` | 6.2ms | found | rejected | `users(email)` +56% |
| `q05-not-in-subquery` | 104.1ms | found | **35% faster** | `orders(status)` +14% |
| `q06-unindexed-filter` | 19.3ms | found | — | `orders(total_cents)` **+99%** |
| `q07-aggregate-scan` | 42.0ms | found | — | `orders(created_at)` +84% |
| `q08-control` (PK lookup) | ~0ms | — | — | — |
| `q09-control` (10-row table) | ~0ms | — | — | — |

### The accepted rewrites

The case from the brief — a function around a timestamp column:

```sql
-- 52.5ms
WHERE DATE(o.created_at) = '2026-06-01' AND u.country = 'IN'
-- 27.0ms, 49% faster, identical results
WHERE o.created_at >= '2026-06-01 00:00:00'
  AND o.created_at <  '2026-06-02 00:00:00'
  AND u.country = 'IN'
```

The same defect wearing a cast instead of a function call, 30.2ms → 19.3ms.

And the NULL trap, 104.1ms → 68.1ms:

```sql
-- WHERE u.id NOT IN (SELECT o.user_id FROM orders o WHERE o.status = 'refunded')
WHERE NOT EXISTS (SELECT 1 FROM orders o WHERE o.user_id = u.id AND o.status = 'refunded')
```

Each of these was executed alongside the original and accepted only because the
result checksums matched over the full result set.

## What the numbers actually show

**The rule engine carries more than the model does.** With no API key at all,
the deterministic rules found 7 of 7 planted defects, produced a verified 1.7×
rewrite, and drove every index experiment. The model added two further verified
rewrites — the cast case and `NOT EXISTS` — which is real value, but the service
is not a wrapper around a model, and that is visible in the numbers rather than
just asserted in the README.

**Five of nine candidate indexes were measured and rejected.** This is the
result worth dwelling on. Conventional advice — "you filter on this column, add
an index" — would have recommended all nine. Four were worth it, with measured
gains of 99%, 84%, 56% and 14%. The other five changed nothing measurable and
would have cost write throughput on a 1.5M-row table forever. The only way to
know which is which is to build the index and re-measure, which is exactly what
the transactional-DDL experiment does.

**One candidate was rejected for no measurable improvement.** On `q04` the model
proposed a rewrite that was not faster past the threshold, so it was discarded.
That is the verification loop doing its job on a live example: the proposal was
not obviously wrong, it just was not better.

**Zero false positives on the controls.** Neither the primary-key lookup nor the
ten-row table drew a rewrite or an index recommendation. The rule engine
suppresses index suggestions below 1,000 rows and on near-constant columns, and
the timing gate rejects anything that is not measurably faster.

## What these numbers do not show

- **Nine queries is a small benchmark**, and the defects were planted by the
  same person who wrote the rules that detect them. 7/7 detection says the rules
  work on the cases they were written for. An external corpus — real slow-query
  logs — would be a much stronger test and is the obvious next step.
- **Baselines are small in absolute terms** (6–104ms). The dataset fits in
  `shared_buffers`, so these are CPU-bound plan comparisons rather than
  I/O-bound ones. On a table that does not fit in memory the same defects cost
  far more, and the speedups would be larger — but that is an argument, not a
  measurement.
- **One run of the benchmark.** Each *query* is measured five times and the
  median taken, and the acceptance gate requires the improvement to exceed the
  observed spread, so individual numbers are defensible. The benchmark as a
  whole was not repeated.
- **Acceptance proves equivalence on this data, not in general.** A checksum
  match means the two queries returned identical rows *for the data present when
  they ran*. A rewrite that diverges only for NULLs, or only for a value that
  does not currently exist in the table, would pass. This is the most important
  limitation in the project and it is inherent to verification by execution —
  which is still far stronger than the alternative of trusting a model.
- **`MIN_IMPROVEMENT_PCT` is a policy, not a fact.** At 10% the service is
  conservative. Lowering it would accept more rewrites and more marginal ones.

## Reproducing

```bash
make up                     # ~1 minute to seed
make bench-rules            # deterministic, no API key
GEMINI_MODEL=gemini-flash-latest make bench
```

Reports are written to `testdata/bench-report.json` and
`testdata/bench-report-rules.json`, including every rejected candidate and the
reason it was rejected.
