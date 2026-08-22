package cutover

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

type streamingRows struct {
	count           int
	index           int
	nextCalls       int
	scanCalls       int
	closeCalls      int
	closed          bool
	payloadSize     int
	peakLive        int
	live            int
	cancelAfterScan int
	cancel          context.CancelFunc
}

func (r *streamingRows) Next() bool {
	r.nextCalls++
	if r.closed || r.index >= r.count {
		return false
	}
	r.index++
	return true
}

func (r *streamingRows) Scan(dest ...any) error {
	if r.closed || r.index == 0 || r.index > r.count {
		return fmt.Errorf("scan without current row")
	}
	if len(dest) != 2 {
		return fmt.Errorf("got %d destinations, want 2", len(dest))
	}
	r.live++
	if r.live > r.peakLive {
		r.peakLive = r.live
	}
	defer func() { r.live-- }()
	*dest[0].(*any) = fmt.Sprintf("row-%04d", r.index)
	*dest[1].(*any) = []byte(strings.Repeat("x", r.payloadSize))
	r.scanCalls++
	if r.cancelAfterScan > 0 && r.scanCalls == r.cancelAfterScan && r.cancel != nil {
		r.cancel()
	}
	return nil
}

func (r *streamingRows) Err() error { return nil }

func (r *streamingRows) Close() error {
	r.closeCalls++
	r.closed = true
	return nil
}

func TestInspectRowsFingerprintIterator_StreamsPayloadsWithBoundedRetention(t *testing.T) {
	const rowCount = 2000
	rows := &streamingRows{count: rowCount, payloadSize: 4096}
	manifest, err := inspectRowsFingerprintIterator(context.Background(), rows, TableSpec{Name: "state_records", KeyColumns: []string{"id"}}, []string{"id", "body"})
	if err != nil {
		t.Fatalf("inspectRowsFingerprintIterator: %v", err)
	}
	if manifest.RowCount != rowCount || manifest.ContentSHA256 == "" {
		t.Fatalf("manifest=%+v, want %d rows and a content hash", manifest, rowCount)
	}
	if rows.peakLive != 1 {
		t.Fatalf("peak live rows=%d, want 1", rows.peakLive)
	}
	if rows.nextCalls != rowCount+1 || rows.scanCalls != rowCount {
		t.Fatalf("next=%d scan=%d, want next=%d scan=%d", rows.nextCalls, rows.scanCalls, rowCount+1, rowCount)
	}
	if !rows.closed || rows.closeCalls != 1 {
		t.Fatalf("closed=%v close_calls=%d, want one close", rows.closed, rows.closeCalls)
	}
}

func TestInspectRowsFingerprintIterator_CancellationClosesWithoutFurtherNext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rows := &streamingRows{count: 2000, payloadSize: 4096, cancelAfterScan: 5, cancel: cancel}
	_, err := inspectRowsFingerprintIterator(ctx, rows, TableSpec{Name: "state_records", KeyColumns: []string{"id"}}, []string{"id", "body"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context cancellation", err)
	}
	if rows.scanCalls != 5 || rows.nextCalls != 5 {
		t.Fatalf("scan=%d next=%d, want five scans and no post-cancel Next", rows.scanCalls, rows.nextCalls)
	}
	if !rows.closed || rows.closeCalls != 1 {
		t.Fatalf("closed=%v close_calls=%d, want one close", rows.closed, rows.closeCalls)
	}
}

type inspectProbe struct {
	mu         sync.Mutex
	stateNext  int
	stateClose int
	statePeak  int
}

type inspectConnector struct{ probe *inspectProbe }

func (c *inspectConnector) Connect(context.Context) (driver.Conn, error) {
	return &inspectConn{probe: c.probe}, nil
}

func (c *inspectConnector) Driver() driver.Driver { return inspectDriver{} }

type inspectDriver struct{}

func (inspectDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("inspectDriver.Open is unused; tests use sql.OpenDB")
}

type inspectConn struct{ probe *inspectProbe }

func (*inspectConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("inspect probe rejects prepared statements")
}

func (*inspectConn) Close() error { return nil }

func (*inspectConn) Begin() (driver.Tx, error) {
	return nil, errors.New("inspect probe rejects transactions")
}

func (c *inspectConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	lower := strings.ToLower(query)
	switch {
	case strings.Contains(lower, "from information_schema.tables"):
		return &inspectRows{columns: []string{"table_name"}, values: [][]driver.Value{{"state_records"}, {LedgerTable}, {IdentityTable}}}, nil
	case strings.Contains(lower, "from harbor_schema_migrations"):
		return &inspectRows{columns: []string{"subsystem", "filename", "version", "checksum_sha256"}, values: [][]driver.Value{{string(SubsystemState), "0001_state.sql", int64(1), strings.Repeat("a", 64)}}}, nil
	case strings.Contains(lower, "from harbor_store_identity"):
		return &inspectRows{columns: []string{"subsystem", "schema_version", "contract_checksum_sha256"}, values: [][]driver.Value{{string(SubsystemState), int64(1), strings.Repeat("a", 64)}}}, nil
	case strings.Contains(lower, "from information_schema.columns"):
		if len(args) != 1 || args[0].Value != "state_records" {
			return nil, fmt.Errorf("unexpected columns argument: %#v", args)
		}
		return &inspectRows{columns: []string{"column_name"}, values: [][]driver.Value{{"id"}, {"body"}}}, nil
	case strings.Contains(lower, `from "state_records"`):
		return &inspectRows{probe: c.probe, state: true, columns: []string{"id", "body"}, count: 2000, payloadSize: 4096}, nil
	default:
		return nil, fmt.Errorf("unexpected inspect query: %s", query)
	}
}

type inspectRows struct {
	probe       *inspectProbe
	state       bool
	columns     []string
	values      [][]driver.Value
	index       int
	count       int
	payloadSize int
	inFlight    int
	closed      bool
}

func (r *inspectRows) Columns() []string { return r.columns }

func (r *inspectRows) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	if r.state && r.probe != nil {
		r.probe.mu.Lock()
		r.probe.stateClose++
		r.probe.mu.Unlock()
	}
	return nil
}

func (r *inspectRows) Next(dest []driver.Value) error {
	if r.closed {
		return errors.New("inspect rows used after close")
	}
	if r.state {
		if r.index >= r.count {
			return io.EOF
		}
		r.inFlight++
		if r.inFlight > 1 {
			return fmt.Errorf("driver retained %d rows concurrently", r.inFlight)
		}
		defer func() { r.inFlight-- }()
		r.index++
		dest[0] = fmt.Sprintf("row-%04d", r.index)
		dest[1] = []byte(strings.Repeat("x", r.payloadSize))
		if r.probe != nil {
			r.probe.mu.Lock()
			r.probe.stateNext++
			if r.inFlight > r.probe.statePeak {
				r.probe.statePeak = r.inFlight
			}
			r.probe.mu.Unlock()
		}
		return nil
	}
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.index])
	r.index++
	return nil
}

func TestInspectSQL_UsesStreamingFingerprintIterator(t *testing.T) {
	probe := &inspectProbe{}
	db := sql.OpenDB(&inspectConnector{probe: probe})
	t.Cleanup(func() { _ = db.Close() })
	snapshot, err := InspectSQL(context.Background(), db, SubsystemState)
	if err != nil {
		t.Fatalf("InspectSQL: %v", err)
	}
	if len(snapshot.TableRows) != 0 || len(snapshot.TableRows["state_records"]) != 0 {
		t.Fatalf("InspectSQL retained row bodies: %#v", snapshot.TableRows)
	}
	fingerprint := snapshot.TableFingerprints["state_records"]
	if fingerprint.RowCount != 2000 || fingerprint.ContentSHA256 == "" {
		t.Fatalf("fingerprint=%+v, want 2000 rows and a content hash", fingerprint)
	}
	probe.mu.Lock()
	next, closed, peak := probe.stateNext, probe.stateClose, probe.statePeak
	probe.mu.Unlock()
	if next != 2000 || closed == 0 || peak != 1 {
		t.Fatalf("driver state next=%d close=%d peak=%d, want next=2000 close>0 peak=1", next, closed, peak)
	}
}
