package postgres_test

import "time"

// defaultTestTimeout is the deadline applied to per-test admin
// connections (CREATE / DROP SCHEMA, TRUNCATE). Mirrors the state +
// artifacts postgres test helpers.
const defaultTestTimeout = 30 * time.Second
