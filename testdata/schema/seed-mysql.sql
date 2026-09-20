-- Benchmark database for queryforge, MySQL dialect.
--
-- A faithful port of seed.sql: the same four tables, the same row counts, the
-- same skew, and the same deliberate under-indexing — only primary keys exist,
-- so every other index is something the optimizer has to discover and justify.
--
-- MySQL has no generate_series, so the row source is a numbers table built by
-- repeated doubling. That is much faster than a recursive CTE here, which is
-- also capped by cte_max_recursion_depth (1000 by default) and would need
-- raising to produce a million rows.

SET SESSION sql_mode = 'STRICT_ALL_TABLES';
SET SESSION unique_checks = 0;
SET SESSION foreign_key_checks = 0;

DROP TABLE IF EXISTS order_items, orders, users, countries, numbers;

CREATE TABLE countries (
    code VARCHAR(8) PRIMARY KEY,
    name VARCHAR(64) NOT NULL
) ENGINE=InnoDB;

CREATE TABLE users (
    id         BIGINT AUTO_INCREMENT PRIMARY KEY,
    email      VARCHAR(255) NOT NULL,
    country    VARCHAR(8)   NOT NULL,
    status     VARCHAR(16)  NOT NULL,
    created_at DATETIME     NOT NULL
) ENGINE=InnoDB;

CREATE TABLE orders (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id     BIGINT      NOT NULL,
    status      VARCHAR(16) NOT NULL,
    total_cents BIGINT      NOT NULL,
    created_at  DATETIME    NOT NULL
) ENGINE=InnoDB;

CREATE TABLE order_items (
    id       BIGINT AUTO_INCREMENT PRIMARY KEY,
    order_id BIGINT       NOT NULL,
    sku      VARCHAR(32)  NOT NULL,
    qty      INT          NOT NULL,
    cents    BIGINT       NOT NULL
) ENGINE=InnoDB;

-- A numbers table holding exactly 1..2^21, built by repeated doubling.
--
-- The obvious version of this uses AUTO_INCREMENT and `INSERT ... SELECT NULL`.
-- It is wrong, and wrong *quietly*: MySQL 8 defaults to
-- innodb_autoinc_lock_mode=2, under which a bulk insert may leave gaps in the
-- generated values. The sequence is then not contiguous, `WHERE n <= 300000`
-- selects fewer than 300000 rows, and the benchmark silently seeds a smaller
-- database than it claims to. Measured here: 168,947 users instead of 300,000.
--
-- Adding the offset explicitly makes the values arithmetic rather than
-- generated, so the table is exactly 1..2^21 with no gaps by construction.
CREATE TABLE numbers (n BIGINT PRIMARY KEY) ENGINE=InnoDB;
INSERT INTO numbers (n) VALUES (1);
INSERT INTO numbers (n) SELECT n +       1 FROM numbers;  --        2
INSERT INTO numbers (n) SELECT n +       2 FROM numbers;  --        4
INSERT INTO numbers (n) SELECT n +       4 FROM numbers;  --        8
INSERT INTO numbers (n) SELECT n +       8 FROM numbers;  --       16
INSERT INTO numbers (n) SELECT n +      16 FROM numbers;  --       32
INSERT INTO numbers (n) SELECT n +      32 FROM numbers;  --       64
INSERT INTO numbers (n) SELECT n +      64 FROM numbers;  --      128
INSERT INTO numbers (n) SELECT n +     128 FROM numbers;  --      256
INSERT INTO numbers (n) SELECT n +     256 FROM numbers;  --      512
INSERT INTO numbers (n) SELECT n +     512 FROM numbers;  --     1024
INSERT INTO numbers (n) SELECT n +    1024 FROM numbers;  --     2048
INSERT INTO numbers (n) SELECT n +    2048 FROM numbers;  --     4096
INSERT INTO numbers (n) SELECT n +    4096 FROM numbers;  --     8192
INSERT INTO numbers (n) SELECT n +    8192 FROM numbers;  --    16384
INSERT INTO numbers (n) SELECT n +   16384 FROM numbers;  --    32768
INSERT INTO numbers (n) SELECT n +   32768 FROM numbers;  --    65536
INSERT INTO numbers (n) SELECT n +   65536 FROM numbers;  --   131072
INSERT INTO numbers (n) SELECT n +  131072 FROM numbers;  --   262144
INSERT INTO numbers (n) SELECT n +  262144 FROM numbers;  --   524288
INSERT INTO numbers (n) SELECT n +  524288 FROM numbers;  --  1048576
INSERT INTO numbers (n) SELECT n + 1048576 FROM numbers;  --  2097152

INSERT INTO countries (code, name) VALUES
    ('IN','India'), ('SG','Singapore'), ('GB','United Kingdom'), ('US','United States'),
    ('AE','United Arab Emirates'), ('AU','Australia'), ('DE','Germany'), ('FR','France'),
    ('JP','Japan'), ('BR','Brazil');

-- 300k users. Country is skewed towards IN so that `country = 'IN'` is a
-- realistically unselective filter rather than a convenient one.
INSERT INTO users (email, country, status, created_at)
SELECT
    CONCAT('user', n, '@', ELT(1 + (n % 4), 'example.com', 'mail.test', 'shop.test', 'corp.test')),
    ELT(1 + (n % 10), 'IN','IN','IN','IN','SG','GB','US','AE','AU','DE'),
    IF(n % 50 = 0, 'suspended', 'active'),
    NOW() - INTERVAL (n % 900) DAY
FROM numbers
WHERE n <= 300000;

-- 1.5M orders spread over about two and a half years.
INSERT INTO orders (user_id, status, total_cents, created_at)
SELECT
    1 + (n % 300000),
    ELT(1 + (n % 7), 'paid','paid','paid','paid','pending','refunded','cancelled'),
    100 + (n % 500000),
    NOW() - INTERVAL (n % 900) DAY
FROM numbers
WHERE n <= 1500000;

-- 2M order items.
INSERT INTO order_items (order_id, sku, qty, cents)
SELECT
    1 + (n % 1500000),
    CONCAT('SKU-', LPAD(n % 5000, 5, '0')),
    1 + (n % 5),
    50 + (n % 20000)
FROM numbers
WHERE n <= 2000000;

DROP TABLE numbers;

SET SESSION unique_checks = 1;
SET SESSION foreign_key_checks = 1;

-- The optimizer reads information_schema.STATISTICS for index cardinality and
-- TABLES.TABLE_ROWS for row counts. Both are estimates maintained lazily, and a
-- freshly loaded table can report wildly wrong figures until they are refreshed.
ANALYZE TABLE countries, users, orders, order_items;
