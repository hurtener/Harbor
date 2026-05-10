package postgres_test

import "time"

// defaultTestTimeout is the deadline applied to per-test admin
// connections (CREATE / DROP SCHEMA, TRUNCATE). Generous enough for
// CI's postgres:16 service container under load; tight enough to fail
// loudly if the DB is unreachable.
const defaultTestTimeout = 30 * time.Second
