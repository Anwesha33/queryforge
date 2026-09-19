#!/bin/bash
# Creates and seeds the benchmark database on first start.
#
# Runs from docker-entrypoint-initdb.d, so it executes exactly once, against a
# fresh data directory. Re-seeding later means `make down` (which drops the
# volume) and `make up`.
set -euo pipefail

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-SQL
	CREATE DATABASE shop;
SQL

echo "seeding the benchmark database (this takes a minute)..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname shop -f /seed/seed.sql
echo "benchmark database ready"
