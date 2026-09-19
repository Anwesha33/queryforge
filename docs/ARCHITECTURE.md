# Architecture

## The premise

An LLM can propose a SQL rewrite. It cannot know whether the rewrite is
correct — and the dangerous case is not the obviously wrong answer, it is the
plausible one. `NOT IN` → `NOT EXISTS` is usually right and silently changes
results when NULLs are present. Dropping a join "that contributes no columns"
changes row multiplicity whenever the relationship was not one-to-one. Adding a
`LIMIT` makes the query faster and different.

So the design question is not "how do we get better rewrites out of the model".
It is: **what would have to be true before a human is shown a rewrite at all?**

The answer is the verification loop, and everything else in this service exists
to feed it.

## Pipeline

```
parse ─▶ introspect ─▶ measure baseline ─▶ rules ─▶ propose ─▶ VERIFY ─▶ index experiments ─▶ report
```

Each stage is separable and each produces evidence that lands in the report.

## Why the real parser

`internal/sqlparse` wraps libpg_query — the actual PostgreSQL grammar, extracted
into a library — rather than matching SQL with regular expressions.

Every safety decision here is a question about the *structure* of a statement:
is it read-only, which tables does it touch, does the candidate select the same
columns. A regular expression answers those wrongly on exactly the cases that
matter:

```sql
SELECT id FROM orders -- DELETE FROM orders          -- a comment, not a delete
SELECT id FROM orders WHERE note = 'drop table x;'   -- a literal, not a statement
WITH gone AS (DELETE FROM orders RETURNING *)        -- parses as a SELECT, deletes rows
SELECT * FROM gone
```

The third is the one that matters. It is a `SelectStmt` at the top level, so a
"is it a SELECT" check passes, and it deletes the table. The parser walks the
`WITH` clause and rejects it. There is a test for each of these.

The same parse also yields the fingerprint — libpg_query's structural hash —
which is what makes "how has this query performed over time" answerable: two
statements differing only in their literals share a fingerprint, so a query's
history survives its parameters changing.

## Why measurement, not cost

Postgres reports a plan cost, and it is tempting to use it: it is free, and
lower is supposed to mean faster.

It is not used for acceptance. Cost is the planner's estimate of work in
arbitrary units, derived from statistics that may be stale and a cost model that
assumes a particular hardware profile. A rewrite that lowers the estimate while
raising the real time is an ordinary outcome. Cost is recorded in every report,
because a large change in it is informative — but the gate is wall-clock time.

Measurement is `EXPLAIN (ANALYZE, BUFFERS)`, which executes the query. That is
safe only because the statement has already been proven read-only *and* because
every execution runs inside a `READ ONLY` transaction, so the server refuses a
write regardless of what the parser concluded. One check in our code, one in the
database, deliberately.

Three details that keep the numbers honest:

- **Median, not mean.** One cold run — a cache miss, an autovacuum, a noisy
  neighbour — moves a mean of five runs far more than it moves their median.
- **A discarded warm-up run.** The first execution pays for cold caches.
  Including it makes every subsequent "improvement" look larger than it is.
- **The spread is recorded.** Max minus min across the timed runs, used by the
  acceptance gate.

## The verification loop

Four gates, cheapest first, in `internal/optimizer/verify.go`.

**1. It parses as a read-only SELECT.** Same parser, same guarantees. A model
asked for a SELECT has been known to return a DELETE.

**2. It has the same output shape.** Column count, column names where both
queries name them, and no table the original never read. This cannot prove
equivalence — it is a cheap filter in front of the expensive gate.

**3. The results are identical.** Both queries are executed and their result
sets hashed:

```sql
SELECT md5(string_agg(h, '' ORDER BY h)), count(*)
FROM (SELECT md5(t::text) AS h FROM ( <query> ) t) z
```

Row order is part of the identity **only when the query has a top-level
`ORDER BY`**. A query without one makes no promise about order, and hashing in
arrival order would report two equivalent queries as different simply because
the planner chose a different join order. A query with one does promise it, and
a rewrite that returns the right rows in the wrong order is not equivalent.
Getting this backwards in either direction produces a wrong answer, which is why
the baseline records which mode it used and the candidate is hashed the same way.

**4. It is faster, past two independent bars.** The improvement must clear
`MIN_IMPROVEMENT_PCT`, and it must also exceed the run-to-run spread of the
measurements. The second is the one that matters: if five runs of the original
ranged over 40ms, a candidate that is 20ms "faster" has told us nothing. A
single percentage threshold on a shared machine accepts noise as signal.

Every rejection is recorded with a typed reason, so "why did nothing get
accepted" is a query rather than a mystery.

## Index experiments

The mechanism that makes index advice trustworthy:

```go
BEGIN;
CREATE INDEX idx_orders_total_cents ON orders (total_cents);
EXPLAIN (ANALYZE) <the original query>;   -- measured with the index live
ROLLBACK;                                  -- it never existed
```

PostgreSQL has transactional DDL, so the index is real for the duration of the
measurement and gone afterwards. The measurement must run on the same connection
inside the same transaction, or it will not see the uncommitted index — which is
why `MeasureInTx` exists and takes the transaction from the context.

On the benchmark this rejected five of nine candidate indexes. Conventional
advice would have recommended all nine.

The rollback is unconditional, including on the success path. The index must not
survive the experiment, and a `defer` that only fires on error is how it would.

**This is why the service targets PostgreSQL rather than MySQL.** MySQL commits
DDL implicitly; the same experiment there leaves a real index on someone else's
database. Supporting it properly needs a shadow schema, or accepting cost-model
estimates instead of measurements. The `engine.Engine` interface marks where
that implementation would go, and the README says plainly that it does not exist
rather than shipping a stub that appears to work.

## Why the rules exist alongside the model

`internal/rules` analyses the parse tree, the schema and the executed plan, with
no model involved. It found 7 of 7 planted defects in the benchmark and produced
a verified 1.7× rewrite on its own.

Three reasons it is not redundant:

- **The service works with no API key.** Degrading to "deterministic optimiser"
  is much better than degrading to nothing.
- **Deterministic findings are reproducible.** The same query always produces
  the same findings, which is what makes the benchmark meaningful.
- **The rules feed the model.** The proposer prompt contains the findings, the
  schema with `n_distinct` and null fractions, and the executed plan. A model
  told "there is a sequential scan over 1.5M rows filtered on `created_at`, and
  no index leads with that column" proposes better than one shown only the SQL.

The rules are also where the *negative* knowledge lives — the cases where the
obvious advice is wrong. An index is not proposed for a table under 1,000 rows,
or for a column with two distinct values. Those suppressions are why the control
queries produce nothing.

## Asynchronous by necessity

Optimising one query executes it five times, executes each candidate five times,
and builds up to three real indexes. That is minutes. It cannot happen in an
HTTP request.

The API parses and validates at the edge — so a statement that can write is
refused with a 422 the caller can act on, rather than accepted and failed
invisibly — then writes a job row, publishes to Kafka and returns 202.

The concurrency control is the same claim query used in the other services:

```sql
UPDATE optimization_jobs SET status='running', attempts=attempts+1
WHERE id=$1 AND status IN ('queued','retrying')
RETURNING ...
```

Kafka is at-least-once; the `WHERE` clause means exactly one worker wins. Retries
go to delay topics so a failing job cannot stall healthy traffic behind it, and a
reclaimer rescues jobs whose worker died mid-run.

Worker concurrency defaults to **2**, which is lower than it looks. Every job
runs real queries against the target database; an optimiser that saturates the
database it is measuring produces measurements of itself.

## Where the safety boundary actually is

Stated plainly, because it is the thing to be honest about:

- A checksum match proves the two queries returned identical rows **for the data
  present when they ran**. A rewrite that diverges only for NULLs, or only for a
  value not currently in the table, would pass.
- Index experiments take a brief `ACCESS EXCLUSIVE` lock, because they do not use
  `CONCURRENTLY`. Fine against a replica, not fine against production. The README
  says to point `TARGET_DSN` at a replica.
- The service recommends. It never applies a rewrite or creates a real index.

Verification by execution is much stronger than trusting a model, and it is not
a proof of semantic equivalence. Both halves of that sentence matter.
