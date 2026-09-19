-- Benchmark database for queryforge.
--
-- Deliberately under-indexed: only primary keys exist. Every other index is
-- something the optimizer has to discover, measure and justify. The row counts
-- are chosen so that a sequential scan is genuinely slow (hundreds of
-- milliseconds) without the seed taking minutes to build — below roughly a
-- million rows Postgres scans so fast that no optimisation is measurable, and
-- a benchmark where nothing can improve proves nothing.

DROP TABLE IF EXISTS order_items, orders, users, countries CASCADE;

CREATE TABLE countries (
    code TEXT PRIMARY KEY,
    name TEXT NOT NULL
);

CREATE TABLE users (
    id         BIGSERIAL PRIMARY KEY,
    email      TEXT        NOT NULL,
    country    TEXT        NOT NULL,
    status     TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE orders (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT      NOT NULL,
    status      TEXT        NOT NULL,
    total_cents BIGINT      NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL
);

CREATE TABLE order_items (
    id       BIGSERIAL PRIMARY KEY,
    order_id BIGINT NOT NULL,
    sku      TEXT   NOT NULL,
    qty      INT    NOT NULL,
    cents    BIGINT NOT NULL
);

INSERT INTO countries (code, name)
SELECT c, initcap(c)
FROM unnest(ARRAY['IN','SG','GB','US','AE','AU','DE','FR','JP','BR']) AS c;

-- 300k users. Country is skewed towards IN so that `country = 'IN'` is a
-- realistically unselective filter rather than a convenient one.
INSERT INTO users (email, country, status, created_at)
SELECT
    'user' || g || '@' || (ARRAY['example.com','mail.test','shop.test','corp.test'])[1 + (g % 4)],
    (ARRAY['IN','IN','IN','IN','SG','GB','US','AE','AU','DE'])[1 + (g % 10)],
    CASE WHEN g % 50 = 0 THEN 'suspended' ELSE 'active' END,
    now() - (g % 900) * INTERVAL '1 day'
FROM generate_series(1, 300000) AS g;

-- 1.5M orders spread over about two and a half years.
INSERT INTO orders (user_id, status, total_cents, created_at)
SELECT
    1 + (g % 300000),
    (ARRAY['paid','paid','paid','paid','pending','refunded','cancelled'])[1 + (g % 7)],
    100 + (g % 500000),
    now() - ((g % 900) * INTERVAL '1 day') - ((g % 86400) * INTERVAL '1 second')
FROM generate_series(1, 1500000) AS g;

INSERT INTO order_items (order_id, sku, qty, cents)
SELECT
    1 + (g % 1500000),
    'SKU-' || (g % 5000),
    1 + (g % 4),
    50 + (g % 20000)
FROM generate_series(1, 2000000) AS g;

-- Statistics matter more than the data here: without ANALYZE the planner works
-- from defaults and every measurement is of the wrong plan.
ANALYZE countries;
ANALYZE users;
ANALYZE orders;
ANALYZE order_items;
