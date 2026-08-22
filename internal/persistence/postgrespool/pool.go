// Package postgrespool owns runtime-scoped PostgreSQL connection pools.
//
// A runtime may have one logical database shared by all Harbor-owned
// PostgreSQL projections, or it may retain distinct subsystem DSNs during a
// staged rollout. In both cases this package is the only runtime path that
// opens pools and applies the aggregate connection budget.
package postgrespool

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// DefaultMaxOpenConns is the conservative per-runtime budget for the
// current Render Basic-4GB fleet. The rollout budget is 18 overlapping
// runtimes*3 = 54, plus six direct migration sessions, twelve Pengui /
// capabilities connections, and a 25-connection operator reserve = 97,
// below the observed max_connections=103.
const DefaultMaxOpenConns = 3

// DefaultMaxIdleConns limits retained idle connections per runtime. A small
// idle allowance is intentional: the old six-store defaults retained up to
// thirty idle connections per runtime.
const DefaultMaxIdleConns = 1

// DefaultConnMaxLifetime bounds connection generation retention.
const DefaultConnMaxLifetime = 5 * time.Minute

// DefaultConnMaxIdleTime bounds idle connection retention. Unlike the old
// posture, zero is never used by the runtime defaults.
const DefaultConnMaxIdleTime = 30 * time.Second

// Config configures one runtime's aggregate PostgreSQL pool budget.
type Config struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

// WithDefaults applies the safe fleet defaults to zero fields and validates
// the resulting budget. Negative values and an idle allowance above the open
// allowance fail loudly.
func (c Config) WithDefaults() (Config, error) {
	if c.MaxOpenConns == 0 {
		c.MaxOpenConns = DefaultMaxOpenConns
	}
	if c.MaxIdleConns == 0 {
		c.MaxIdleConns = DefaultMaxIdleConns
	}
	if c.ConnMaxLifetime == 0 {
		c.ConnMaxLifetime = DefaultConnMaxLifetime
	}
	if c.ConnMaxIdleTime == 0 {
		c.ConnMaxIdleTime = DefaultConnMaxIdleTime
	}
	if c.MaxOpenConns < 1 {
		return Config{}, fmt.Errorf("postgres pool: max_open_conns must be at least 1, got %d", c.MaxOpenConns)
	}
	if c.MaxIdleConns < 0 || c.MaxIdleConns > c.MaxOpenConns {
		return Config{}, fmt.Errorf("postgres pool: max_idle_conns must be between 0 and max_open_conns (%d), got %d", c.MaxOpenConns, c.MaxIdleConns)
	}
	if c.ConnMaxLifetime < 0 {
		return Config{}, fmt.Errorf("postgres pool: conn_max_lifetime must be finite and non-negative, got %s", c.ConnMaxLifetime)
	}
	if c.ConnMaxIdleTime < 0 {
		return Config{}, fmt.Errorf("postgres pool: conn_max_idle_time must be finite and non-negative, got %s", c.ConnMaxIdleTime)
	}
	return c, nil
}

// Spec identifies one enabled PostgreSQL subsystem. Empty subsystem names
// and DSNs are rejected by Open.
type Spec struct {
	Subsystem string
	DSN       string
}

// Allocation is the deterministic budget assigned to one physical DSN pool.
// Subsystems lists every logical projection sharing that pool.
type Allocation struct {
	DSN        string
	Subsystems []string
	// MaxOpenConns is the local database/sql ceiling. The shared broker in
	// Manager enforces the aggregate ceiling across every distinct DSN.
	MaxOpenConns int
	MaxIdleConns int
}

// Manager owns every pool opened for one runtime. Callers receive borrowed
// *sql.DB handles through DB; they must not close them. Close is idempotent
// and closes each physical pool exactly once.
type Manager struct {
	mu     sync.Mutex
	config Config
	pools  map[string]*sql.DB
	budget *connectionBudget
	closed bool
}

// Open creates the minimum number of physical pools for specs. Equal DSNs
// share one *sql.DB. Distinct DSNs remain independently usable during a
// staged rollout: each database/sql pool may request up to the configured
// local ceiling, while one shared broker enforces the aggregate max-open
// budget across all physical pools. Aggregate idle retention is allocated
// deterministically so the sum of MaxIdleConns remains bounded.
func Open(ctx context.Context, cfg Config, specs []Spec) (*Manager, error) {
	if ctx == nil {
		return nil, errors.New("postgres pool: nil context")
	}
	resolved, err := cfg.WithDefaults()
	if err != nil {
		return nil, err
	}
	allocations, err := plan(resolved, specs)
	if err != nil {
		return nil, err
	}
	if len(allocations) == 0 {
		return &Manager{config: resolved, pools: make(map[string]*sql.DB)}, nil
	}
	manager := &Manager{
		config: resolved,
		pools:  make(map[string]*sql.DB, len(allocations)),
		budget: newConnectionBudget(resolved.MaxOpenConns),
	}
	if err := manager.open(ctx, allocations, func(dsn string) (driver.Connector, error) {
		config, err := pgx.ParseConfig(dsn)
		if err != nil {
			return nil, err
		}
		return stdlib.GetConnector(*config), nil
	}); err != nil {
		return nil, err
	}
	return manager, nil
}

func (m *Manager) open(ctx context.Context, allocations []Allocation, connectorFor func(string) (driver.Connector, error)) error {
	for _, allocation := range allocations {
		connector, err := connectorFor(allocation.DSN)
		if err != nil {
			return fmt.Errorf("postgres pool: parse %s: %w", stringsJoin(allocation.Subsystems), errors.Join(err, m.closePools()))
		}
		db := sql.OpenDB(&budgetConnector{inner: connector, budget: m.budget})
		db.SetMaxOpenConns(allocation.MaxOpenConns)
		// Do not let the eager bootstrap ping pin a shared broker token in
		// an idle pool before the remaining distinct DSNs have booted. The
		// configured idle allowance is applied only after the ping succeeds.
		db.SetMaxIdleConns(0)
		db.SetConnMaxLifetime(m.config.ConnMaxLifetime)
		db.SetConnMaxIdleTime(m.config.ConnMaxIdleTime)
		if err := db.PingContext(ctx); err != nil {
			return fmt.Errorf("postgres pool: ping %s: %w", stringsJoin(allocation.Subsystems), errors.Join(err, db.Close(), m.closePools()))
		}
		db.SetMaxIdleConns(allocation.MaxIdleConns)
		m.pools[allocation.DSN] = db
	}
	return nil
}

// Plan validates specs and returns the exact aggregate allocation without
// opening a database. Runtime tests and operators can use it to prove a
// staged distinct-DSN topology fits the configured hard cap.
func Plan(cfg Config, specs []Spec) ([]Allocation, error) {
	resolved, err := cfg.WithDefaults()
	if err != nil {
		return nil, err
	}
	return plan(resolved, specs)
}

func plan(cfg Config, specs []Spec) ([]Allocation, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	byDSN := make(map[string][]string, len(specs))
	for _, spec := range specs {
		if spec.Subsystem == "" {
			return nil, errors.New("postgres pool: subsystem is required")
		}
		if spec.DSN == "" {
			return nil, fmt.Errorf("postgres pool: %s DSN is required", spec.Subsystem)
		}
		byDSN[spec.DSN] = append(byDSN[spec.DSN], spec.Subsystem)
	}
	keys := make([]string, 0, len(byDSN))
	for dsn := range byDSN {
		keys = append(keys, dsn)
	}
	sort.Strings(keys)
	if len(keys) > 1 && cfg.MaxIdleConns >= cfg.MaxOpenConns {
		return nil, fmt.Errorf("postgres pool: max_idle_conns=%d must be less than max_open_conns=%d when distinct DSN pools share the aggregate broker; reserve one connection token for another DSN or consolidate DSNs", cfg.MaxIdleConns, cfg.MaxOpenConns)
	}
	// Every physical pool gets a usable local ceiling. The shared broker,
	// rather than static division, is the aggregate hard bound; this allows
	// six distinct legacy DSNs to boot with the safe max_open_conns=3 default.
	openAlloc := make([]int, len(keys))
	for i := range openAlloc {
		openAlloc[i] = cfg.MaxOpenConns
	}
	idleAlloc := allocateIdle(cfg.MaxIdleConns, openAlloc)
	result := make([]Allocation, len(keys))
	for i, dsn := range keys {
		result[i] = Allocation{
			DSN:          dsn,
			Subsystems:   append([]string(nil), byDSN[dsn]...),
			MaxOpenConns: openAlloc[i],
			MaxIdleConns: idleAlloc[i],
		}
	}
	return result, nil
}

func allocate(total, n int) []int {
	result := make([]int, n)
	base, remainder := total/n, total%n
	for i := range result {
		result[i] = base
		if i < remainder {
			result[i]++
		}
	}
	return result
}

func allocateIdle(total int, opens []int) []int {
	if total == 0 {
		return make([]int, len(opens))
	}
	result := make([]int, len(opens))
	remaining := total
	for i, open := range opens {
		if remaining == 0 {
			break
		}
		result[i] = 1
		if result[i] > open {
			result[i] = open
		}
		remaining -= result[i]
	}
	return result
}

func stringsJoin(names []string) string {
	if len(names) == 0 {
		return "unknown"
	}
	return names[0]
}

// DB returns the borrowed pool for dsn. Equal DSNs return the same pointer.
func (m *Manager) DB(dsn string) (*sql.DB, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, false
	}
	db, ok := m.pools[dsn]
	return db, ok
}

// Config returns the resolved aggregate budget.
func (m *Manager) Config() Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.config
}

// Close closes every physical pool once. Borrowed handles reject subsequent
// database calls through database/sql's closed-pool error; runtime stores also
// flip their own closed flags before their caller invokes this method.
func (m *Manager) Close(context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.mu.Unlock()
	return m.closePools()
}

func (m *Manager) closePools() error {
	m.mu.Lock()
	pools := make([]*sql.DB, 0, len(m.pools))
	for _, db := range m.pools {
		pools = append(pools, db)
	}
	m.pools = make(map[string]*sql.DB)
	m.mu.Unlock()
	var errs []error
	for _, db := range pools {
		if err := db.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// connectionBudget is shared by every physical DSN pool in one Manager. It
// gates creation of physical driver connections rather than logical queries,
// so database/sql can still schedule work fairly across distinct DSNs while
// the hard aggregate open limit remains enforceable.
type connectionBudget struct {
	tokens chan struct{}
}

func newConnectionBudget(limit int) *connectionBudget {
	tokens := make(chan struct{}, limit)
	for range limit {
		tokens <- struct{}{}
	}
	return &connectionBudget{tokens: tokens}
}

func (b *connectionBudget) acquire(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.tokens:
		return nil
	}
}

func (b *connectionBudget) release() {
	b.tokens <- struct{}{}
}

type budgetConnector struct {
	inner  driver.Connector
	budget *connectionBudget
}

func (c *budgetConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := c.budget.acquire(ctx); err != nil {
		return nil, err
	}
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		c.budget.release()
		return nil, err
	}
	return &budgetConn{inner: conn, budget: c.budget}, nil
}

func (c *budgetConnector) Driver() driver.Driver {
	return c.inner.Driver()
}

type budgetConn struct {
	inner  driver.Conn
	budget *connectionBudget
	once   sync.Once
}

func (c *budgetConn) release() {
	c.once.Do(func() { c.budget.release() })
}

func (c *budgetConn) Close() error {
	defer c.release()
	return c.inner.Close()
}

func (c *budgetConn) Prepare(query string) (driver.Stmt, error) {
	return c.inner.Prepare(query)
}

func (c *budgetConn) Begin() (driver.Tx, error) {
	if conn, ok := c.inner.(driver.ConnBeginTx); ok {
		return conn.BeginTx(context.Background(), driver.TxOptions{})
	}
	return nil, driver.ErrSkip
}

func (c *budgetConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if conn, ok := c.inner.(driver.ConnPrepareContext); ok {
		return conn.PrepareContext(ctx, query)
	}
	return nil, driver.ErrSkip
}

func (c *budgetConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if conn, ok := c.inner.(driver.ExecerContext); ok {
		return conn.ExecContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *budgetConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if conn, ok := c.inner.(driver.QueryerContext); ok {
		return conn.QueryContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *budgetConn) Ping(ctx context.Context) error {
	if conn, ok := c.inner.(driver.Pinger); ok {
		return conn.Ping(ctx)
	}
	return driver.ErrSkip
}

func (c *budgetConn) ResetSession(ctx context.Context) error {
	if conn, ok := c.inner.(driver.SessionResetter); ok {
		return conn.ResetSession(ctx)
	}
	return nil
}

func (c *budgetConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if conn, ok := c.inner.(driver.ConnBeginTx); ok {
		return conn.BeginTx(ctx, opts)
	}
	return nil, driver.ErrSkip
}

func (c *budgetConn) CheckNamedValue(value *driver.NamedValue) error {
	if conn, ok := c.inner.(driver.NamedValueChecker); ok {
		return conn.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

func (c *budgetConn) IsValid() bool {
	if conn, ok := c.inner.(driver.Validator); ok {
		return conn.IsValid()
	}
	return true
}
