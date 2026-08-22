package postgrespool

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestAllocateDistinctDSNs_UsesOneAggregateBudget(t *testing.T) {
	got := allocate(3, 3)
	if want := []int{1, 1, 1}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("allocate(3, 3) = %v, want %v", got, want)
	}
	got = allocate(3, 2)
	if got[0]+got[1] != 3 || got[0] < 1 || got[1] < 1 {
		t.Fatalf("allocate(3, 2) = %v, want positive entries summing to 3", got)
	}
	if _, err := (Config{MaxOpenConns: 3}).WithDefaults(); err != nil {
		t.Fatalf("valid aggregate config rejected: %v", err)
	}
}

func TestPlan_DeduplicatesEqualDSNsAndBoundsDistinctDSNs(t *testing.T) {
	equal, err := Plan(Config{MaxOpenConns: 3, MaxIdleConns: 1}, []Spec{
		{Subsystem: "state", DSN: "postgres://shared"},
		{Subsystem: "memory", DSN: "postgres://shared"},
	})
	if err != nil {
		t.Fatalf("Plan equal DSNs: %v", err)
	}
	if len(equal) != 1 || equal[0].MaxOpenConns != 3 || equal[0].MaxIdleConns != 1 {
		t.Fatalf("equal-DSN allocation = %+v, want one 3-open/1-idle pool", equal)
	}

	distinct, err := Plan(Config{MaxOpenConns: 3, MaxIdleConns: 1}, []Spec{
		{Subsystem: "state", DSN: "a"},
		{Subsystem: "memory", DSN: "b"},
		{Subsystem: "skills", DSN: "c"},
	})
	if err != nil {
		t.Fatalf("Plan distinct DSNs: %v", err)
	}
	open, idle := 0, 0
	for _, allocation := range distinct {
		if allocation.MaxOpenConns < 1 {
			t.Fatalf("distinct allocation starved a pool: %+v", distinct)
		}
		open += allocation.MaxOpenConns
		idle += allocation.MaxIdleConns
	}
	if open != 9 || idle != 1 {
		t.Fatalf("distinct local ceilings open=%d idle=%d, want 9/1; shared broker enforces aggregate 3", open, idle)
	}
}

func TestBasic4GBFleetBudget_LeavesOperatorReserve(t *testing.T) {
	const (
		overlappingRuntimes = 18
		migrationSessions   = 6
		penguiConnections   = 12
		operatorReserve     = 25
		maxConnections      = 103
	)
	used := overlappingRuntimes*DefaultMaxOpenConns + migrationSessions + penguiConnections + operatorReserve
	if used != 97 {
		t.Fatalf("fleet budget = %d, want documented 97", used)
	}
	if used >= maxConnections {
		t.Fatalf("fleet budget %d must remain below max_connections=%d", used, maxConnections)
	}
}

func TestPlan_AllSixDistinctDSNsRemainUsableUnderSharedThreeConnectionCap(t *testing.T) {
	specs := []Spec{
		{Subsystem: "state", DSN: "state"},
		{Subsystem: "memory", DSN: "memory"},
		{Subsystem: "artifacts", DSN: "artifacts"},
		{Subsystem: "skills", DSN: "skills"},
		{Subsystem: "sessions.turns", DSN: "turns"},
		{Subsystem: "observability.rollups", DSN: "rollups"},
	}
	allocations, err := Plan(Config{MaxOpenConns: 3, MaxIdleConns: 1}, specs)
	if err != nil {
		t.Fatalf("Plan six distinct DSNs: %v", err)
	}
	if len(allocations) != len(specs) {
		t.Fatalf("Plan returned %d pools, want %d", len(allocations), len(specs))
	}
	for _, allocation := range allocations {
		if allocation.MaxOpenConns != 3 {
			t.Fatalf("pool %q local ceiling=%d, want 3", allocation.DSN, allocation.MaxOpenConns)
		}
	}
	if idle := sumIdle(allocations); idle != 1 {
		t.Fatalf("six-pool idle allocation=%d, want aggregate 1", idle)
	}
	if _, err := Plan(Config{MaxOpenConns: 3, MaxIdleConns: 3}, specs); err == nil {
		t.Fatal("Plan accepted max_idle_conns=3 for six distinct pools; this would pin every broker token during bootstrap")
	}
}

func sumIdle(allocations []Allocation) int {
	var total int
	for _, allocation := range allocations {
		total += allocation.MaxIdleConns
	}
	return total
}

func TestBudgetConnector_AllSixDistinctPoolsProgressWithoutExceedingCap(t *testing.T) {
	const poolCount = 6
	const callsPerPool = 12
	budget := newConnectionBudget(3)
	active := new(atomic.Int64)
	peak := new(atomic.Int64)
	connectors := make([]driver.Connector, poolCount)
	for i := range connectors {
		connectors[i] = &budgetConnector{
			inner:  &accountingConnector{active: active, peak: peak},
			budget: budget,
		}
	}
	dbs := make([]*sql.DB, poolCount)
	for i, connector := range connectors {
		dbs[i] = sql.OpenDB(connector)
		dbs[i].SetMaxOpenConns(3)
		dbs[i].SetMaxIdleConns(0)
	}
	t.Cleanup(func() {
		for _, db := range dbs {
			_ = db.Close()
		}
	})

	var wg sync.WaitGroup
	errCh := make(chan error, poolCount*callsPerPool)
	for i := range dbs {
		for call := 0; call < callsPerPool; call++ {
			wg.Add(1)
			go func(db *sql.DB, pool, call int) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				if err := db.PingContext(ctx); err != nil {
					errCh <- fmt.Errorf("pool %d call %d: %w", pool, call, err)
				}
			}(dbs[i], i, call)
		}
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if got := peak.Load(); got > 3 {
		t.Fatalf("peak physical connections=%d, want <=3", got)
	}
	if got := peak.Load(); got == 0 {
		t.Fatal("accounting connector did not open any physical connections")
	}
}

func TestManagerOpen_EqualDSNsReuseAndDistinctDSNsBoot(t *testing.T) {
	cfg := Config{MaxOpenConns: 3, MaxIdleConns: 1, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Second}
	for name, specs := range map[string][]Spec{
		"equal": {
			{Subsystem: "state", DSN: "shared"},
			{Subsystem: "memory", DSN: "shared"},
		},
		"distinct": {
			{Subsystem: "state", DSN: "state"},
			{Subsystem: "memory", DSN: "memory"},
			{Subsystem: "artifacts", DSN: "artifacts"},
			{Subsystem: "skills", DSN: "skills"},
			{Subsystem: "sessions.turns", DSN: "turns"},
			{Subsystem: "observability.rollups", DSN: "rollups"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			allocations, err := Plan(cfg, specs)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			manager := &Manager{config: cfg, pools: make(map[string]*sql.DB), budget: newConnectionBudget(cfg.MaxOpenConns)}
			var connections atomic.Int64
			if err := manager.open(context.Background(), allocations, func(string) (driver.Connector, error) {
				return &accountingConnector{active: &connections, peak: new(atomic.Int64)}, nil
			}); err != nil {
				t.Fatalf("open: %v", err)
			}
			t.Cleanup(func() { _ = manager.Close(context.Background()) })
			if name == "equal" {
				left, leftOK := manager.DB("shared")
				right, rightOK := manager.DB("shared")
				if !leftOK || !rightOK || left != right {
					t.Fatal("equal DSNs did not resolve to one borrowed *sql.DB")
				}
				return
			}
			for _, spec := range specs {
				if _, ok := manager.DB(spec.DSN); !ok {
					t.Fatalf("distinct DSN %q did not boot", spec.DSN)
				}
			}
		})
	}
}

func TestManagerClose_ClosesSharedPhysicalPoolExactlyOnce(t *testing.T) {
	cfg := Config{MaxOpenConns: 3, MaxIdleConns: 1, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Second}
	allocations, err := Plan(cfg, []Spec{{Subsystem: "state", DSN: "shared"}, {Subsystem: "memory", DSN: "shared"}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	var closes atomic.Int64
	manager := &Manager{config: cfg, pools: make(map[string]*sql.DB), budget: newConnectionBudget(cfg.MaxOpenConns)}
	if err := manager.open(context.Background(), allocations, func(string) (driver.Connector, error) {
		return &accountingConnector{active: new(atomic.Int64), peak: new(atomic.Int64), closes: &closes}, nil
	}); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := closes.Load(); got != 1 {
		t.Fatalf("physical pool close count=%d, want exactly once", got)
	}
	if _, ok := manager.DB("shared"); ok {
		t.Fatal("manager returned a pool after Close")
	}
}

func TestManagerClose_IsIdempotentAndRejectsBorrowAfterClose(t *testing.T) {
	db, err := sql.Open("pgx", "postgres://invalid.example/harbor")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	manager := &Manager{config: Config{MaxOpenConns: 1, MaxIdleConns: 1}, pools: map[string]*sql.DB{"dsn": db}}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, ok := manager.DB("dsn"); ok {
		t.Fatal("DB returned a pool after manager close")
	}
	if err := db.PingContext(context.Background()); err == nil {
		t.Fatal("borrowed database accepted Ping after manager close")
	}
}

type accountingConnector struct {
	active *atomic.Int64
	peak   *atomic.Int64
	closes *atomic.Int64
}

func (c *accountingConnector) Connect(context.Context) (driver.Conn, error) {
	active := c.active.Add(1)
	for {
		peak := c.peak.Load()
		if active <= peak || c.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	return &accountingConn{active: c.active, closes: c.closes}, nil
}

func (*accountingConnector) Driver() driver.Driver {
	return accountingDriver{}
}

type accountingDriver struct{}

func (accountingDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("accounting driver does not support Open")
}

type accountingConn struct {
	active *atomic.Int64
	closes *atomic.Int64
}

func (c *accountingConn) Close() error {
	c.active.Add(-1)
	if c.closes != nil {
		c.closes.Add(1)
	}
	return nil
}

func (*accountingConn) Prepare(string) (driver.Stmt, error) {
	return nil, driver.ErrSkip
}

func (*accountingConn) Begin() (driver.Tx, error) {
	return nil, driver.ErrSkip
}

func (accountingConn) Ping(context.Context) error {
	time.Sleep(2 * time.Millisecond)
	return nil
}
