package sqlmigrate

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"testing"
)

// The adoption path intentionally uses one *sql.Conn transaction rather than
// three independent statements. This small driver gives the test a
// deterministic interruption point without requiring a live PostgreSQL
// service: the transaction keeps private table state until Commit, and a
// restart can retry against the same logical database state.
type adoptionFakeState struct {
	mu             sync.Mutex
	ledger         bool
	identity       bool
	mirror         bool
	failOn         string
	cancelOnLedger context.CancelFunc
	transactions   int
	commits        int
	rollbacks      int
}

type adoptionFakeConnector struct{ state *adoptionFakeState }

func (c *adoptionFakeConnector) Connect(context.Context) (driver.Conn, error) {
	return &adoptionFakeConn{state: c.state}, nil
}

func (*adoptionFakeConnector) Driver() driver.Driver { return adoptionFakeDriver{} }

type adoptionFakeDriver struct{}

func (adoptionFakeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("adoptionFakeDriver.Open is unused; tests use sql.OpenDB")
}

type adoptionFakeConn struct {
	state  *adoptionFakeState
	mu     sync.Mutex
	active *adoptionFakeTx
}

func (c *adoptionFakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("adoption fake rejects prepared statements")
}

func (*adoptionFakeConn) Close() error { return nil }

func (c *adoptionFakeConn) Begin() (driver.Tx, error) {
	return c.beginTx()
}

func (c *adoptionFakeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return c.beginTx()
}

func (c *adoptionFakeConn) beginTx() (driver.Tx, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != nil {
		return nil, errors.New("adoption fake already has an active transaction")
	}
	c.state.mu.Lock()
	tx := &adoptionFakeTx{
		conn:     c,
		ledger:   c.state.ledger,
		identity: c.state.identity,
		mirror:   c.state.mirror,
	}
	c.state.transactions++
	c.state.mu.Unlock()
	c.active = tx
	return tx, nil
}

func (c *adoptionFakeConn) ExecContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	tx := c.active
	c.mu.Unlock()
	if tx == nil {
		return nil, errors.New("adoption fake write outside transaction")
	}
	lower := strings.ToLower(query)
	switch {
	case strings.Contains(lower, "insert into harbor_schema_migrations"):
		tx.ledger = true
		c.state.mu.Lock()
		cancel := c.state.cancelOnLedger
		c.state.cancelOnLedger = nil
		c.state.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	case strings.Contains(lower, "update harbor_store_identity"):
		c.state.mu.Lock()
		fail := c.state.failOn == "identity"
		c.state.mu.Unlock()
		if fail {
			return nil, errors.New("injected identity interruption")
		}
		tx.identity = true
	case strings.Contains(lower, "insert into schema_migrations"):
		c.state.mu.Lock()
		fail := c.state.failOn == "mirror"
		c.state.mu.Unlock()
		if fail {
			return nil, errors.New("injected legacy mirror interruption")
		}
		tx.mirror = true
	}
	return driver.RowsAffected(1), nil
}

type adoptionFakeTx struct {
	conn     *adoptionFakeConn
	ledger   bool
	identity bool
	mirror   bool
	done     bool
}

func (tx *adoptionFakeTx) Commit() error {
	if tx.done {
		return errors.New("adoption fake transaction already finished")
	}
	tx.done = true
	tx.conn.mu.Lock()
	if tx.conn.active != tx {
		tx.conn.mu.Unlock()
		return errors.New("adoption fake transaction is not active")
	}
	tx.conn.active = nil
	tx.conn.mu.Unlock()
	tx.conn.state.mu.Lock()
	tx.conn.state.ledger = tx.ledger
	tx.conn.state.identity = tx.identity
	tx.conn.state.mirror = tx.mirror
	tx.conn.state.commits++
	tx.conn.state.mu.Unlock()
	return nil
}

func (tx *adoptionFakeTx) Rollback() error {
	if tx.done {
		return nil
	}
	tx.done = true
	tx.conn.mu.Lock()
	if tx.conn.active == tx {
		tx.conn.active = nil
	}
	tx.conn.mu.Unlock()
	tx.conn.state.mu.Lock()
	tx.conn.state.rollbacks++
	tx.conn.state.mu.Unlock()
	return nil
}

func newAdoptionFakeDB(t *testing.T, state *adoptionFakeState) (*sql.DB, *sql.Conn) {
	t.Helper()
	db := sql.OpenDB(&adoptionFakeConnector{state: state})
	conn, err := db.Conn(context.Background())
	if err != nil {
		_ = db.Close()
		t.Fatalf("acquire adoption fake connection: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		_ = db.Close()
	})
	return db, conn
}

func adoptionStateSnapshot(state *adoptionFakeState) (ledger, identity, mirror bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.ledger, state.identity, state.mirror
}

func TestRecordNamedMigration_AtomicFailureRollsBackAndRestartIsIdempotent(t *testing.T) {
	for _, failOn := range []string{"identity", "mirror"} {
		t.Run(failOn, func(t *testing.T) {
			state := &adoptionFakeState{failOn: failOn}
			_, conn := newAdoptionFakeDB(t, state)
			migration := migration{name: "0001_init.sql", version: 1}

			err := recordNamedMigration(context.Background(), conn, "memory", migration, strings.Repeat("a", 64), "memory/postgres")
			if err == nil || !strings.Contains(err.Error(), "interruption") {
				t.Fatalf("failure injection error = %v, want injected interruption", err)
			}
			if ledger, identity, mirror := adoptionStateSnapshot(state); ledger || identity || mirror {
				t.Fatalf("failed adoption left partial state ledger=%t identity=%t mirror=%t", ledger, identity, mirror)
			}

			state.mu.Lock()
			state.failOn = ""
			state.mu.Unlock()
			if err := recordNamedMigration(context.Background(), conn, "memory", migration, strings.Repeat("a", 64), "memory/postgres"); err != nil {
				t.Fatalf("restart adoption: %v", err)
			}
			if err := recordNamedMigration(context.Background(), conn, "memory", migration, strings.Repeat("a", 64), "memory/postgres"); err != nil {
				t.Fatalf("idempotent restart adoption: %v", err)
			}
			if ledger, identity, mirror := adoptionStateSnapshot(state); !ledger || !identity || !mirror {
				t.Fatalf("successful restart state ledger=%t identity=%t mirror=%t, want all true", ledger, identity, mirror)
			}
			state.mu.Lock()
			defer state.mu.Unlock()
			if state.rollbacks != 1 || state.commits != 2 || state.transactions != 3 {
				t.Fatalf("transactions=%d commits=%d rollbacks=%d, want 3/2/1", state.transactions, state.commits, state.rollbacks)
			}
		})
	}
}

func TestRecordNamedMigration_CancellationRollsBackAndCanRestart(t *testing.T) {
	state := &adoptionFakeState{}
	db, conn := newAdoptionFakeDB(t, state)
	ctx, cancel := context.WithCancel(context.Background())
	state.mu.Lock()
	state.cancelOnLedger = cancel
	state.mu.Unlock()
	migration := migration{name: "0001_init.sql", version: 1}

	err := recordNamedMigration(ctx, conn, "memory", migration, strings.Repeat("b", 64), "memory/postgres")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled adoption error = %v, want context.Canceled", err)
	}
	if ledger, identity, mirror := adoptionStateSnapshot(state); ledger || identity || mirror {
		t.Fatalf("canceled adoption left partial state ledger=%t identity=%t mirror=%t", ledger, identity, mirror)
	}
	// database/sql retires a driver connection whose context was canceled;
	// the next boot obtains a fresh session while retaining the database state.
	restartConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("acquire restart adoption connection: %v", err)
	}
	defer func() { _ = restartConn.Close() }()
	if err := recordNamedMigration(context.Background(), restartConn, "memory", migration, strings.Repeat("b", 64), "memory/postgres"); err != nil {
		t.Fatalf("restart after canceled adoption: %v", err)
	}
}
