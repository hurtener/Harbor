package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/sessions/turns/drivers/sqlite"
)

// Driver-local behavior tests beyond the shared conformance suite:
// retention eviction + the explicit truncation flag, honest overflow /
// availability field round-trips, Durable() honesty, `:memory:`
// isolation, and DSN validation.

func TestRetention_EvictsOldest_SetsTruncationFlag(t *testing.T) {
	dsn := t.TempDir() + "/retention.sqlite"
	s, err := sqlite.New(sqlite.Config{DSN: dsn, Retention: 5})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	ctx := context.Background()
	id := identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a"}

	for i := 1; i <= 8; i++ {
		if _, err := s.AppendTurnIf(ctx, id, richRow(fmt.Sprintf("run-%02d", i), id, i)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	// The 3 oldest rows were evicted; the 5 newest remain.
	if _, err := s.GetTurn(ctx, id, "run-01"); !errors.Is(err, turns.ErrTurnNotFound) {
		t.Errorf("evicted oldest get error=%v, want ErrTurnNotFound", err)
	}
	if _, err := s.GetTurn(ctx, id, "run-03"); !errors.Is(err, turns.ErrTurnNotFound) {
		t.Errorf("evicted get error=%v, want ErrTurnNotFound", err)
	}
	for _, tid := range []string{"run-04", "run-05", "run-06", "run-07", "run-08"} {
		if _, err := s.GetTurn(ctx, id, turns.TurnID(tid)); err != nil {
			t.Errorf("retained %s must survive: %v", tid, err)
		}
	}

	// The truncation flag is explicit, never silent.
	rows, next, info, err := s.ListTurns(ctx, id, nil, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 5 || next != nil {
		t.Fatalf("page=%d rows next=%v, want 5/nil", len(rows), next != nil)
	}
	if !info.Truncated {
		t.Errorf("truncated flag not set after eviction")
	}
	if info.Remaining != 0 || !info.CountExact {
		t.Errorf("post-eviction remaining=%d exact=%v, want 0/true", info.Remaining, info.CountExact)
	}

	// A cursor past an EVICTED boundary row is refused with the
	// expired-cursor error (the boundary is no longer retained).
	page, cur, _, err := s.ListTurns(ctx, id, nil, 2)
	if err != nil || cur == nil {
		t.Fatalf("page: %d rows cur=%v err=%v", len(page), cur != nil, err)
	}
	forged := *cur
	forged.TurnID = "run-01" // evicted boundary
	if _, _, _, err := s.ListTurns(ctx, id, &forged, 2); !errors.Is(err, turns.ErrCursorExpired) {
		t.Errorf("cursor past evicted boundary error=%v, want ErrCursorExpired", err)
	}
}

func TestRetention_DefaultBound_MatchesProjection(t *testing.T) {
	// A zero Retention config means the documented projection default
	// (turns.MaxRetainedTurns): a session retains only its newest N
	// rows and the oldest are evicted.
	dsn := t.TempDir() + "/default-retention.sqlite"
	s, err := sqlite.New(sqlite.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	ctx := context.Background()
	id := identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a"}

	for i := 1; i <= turns.MaxRetainedTurns+3; i++ {
		if _, err := s.AppendTurnIf(ctx, id, richRow(fmt.Sprintf("run-%04d", i), id, i)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	rows, _, info, err := s.ListTurns(ctx, id, nil, turns.MaxListLimit)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != turns.MaxListLimit {
		t.Fatalf("first page=%d rows, want %d", len(rows), turns.MaxListLimit)
	}
	if !info.Truncated {
		t.Errorf("truncation flag not set at the default bound")
	}
	// Walk the whole retained window: exactly MaxRetainedTurns rows.
	total := 0
	var cur *turns.Cursor
	for {
		page, nxt, info, err := s.ListTurns(ctx, id, cur, turns.MaxListLimit)
		if err != nil {
			t.Fatalf("count walk: %v", err)
		}
		total += len(page)
		if nxt == nil {
			break
		}
		cur = nxt
		if !info.CountExact {
			t.Errorf("remaining count not exact")
		}
	}
	if total != turns.MaxRetainedTurns {
		t.Errorf("retained %d rows, want %d (the default projection bound)", total, turns.MaxRetainedTurns)
	}
}

// TestHonesty_OverflowAndAvailabilityFields_RoundTrip pins the honest
// availability / overflow fields the driver must persist byte-exact:
// a partial activity window with More + Dropped, exact totals, a
// partial reasoning component with a dropped count, and per-app /
// per-attachment availability.
func TestHonesty_OverflowAndAvailabilityFields_RoundTrip(t *testing.T) {
	s := openMem(t)
	defer func() { _ = s.Close(context.Background()) }()
	ctx := context.Background()
	id := identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a"}

	row := richRow("run-1", id, 1)
	row.Activity = turns.Activity{
		Rows: []turns.ActivityRow{
			{Position: 0, Tool: "t0", Status: turns.ActivitySucceeded, TerminalClass: turns.ActivityTerminalSucceeded},
			{Position: 1, Tool: "t1", Status: turns.ActivityFailed, TerminalClass: turns.ActivityTerminalFailed},
			{Position: 2, Tool: "t2", Status: turns.ActivityCancelled, TerminalClass: turns.ActivityTerminalCancelled},
		},
		Complete: turns.CompletenessPartial,
		More:     true,
		Dropped:  7,
		Totals:   turns.ActivityTotals{Succeeded: 1, Failed: 1, Cancelled: 1, PolicyExhausted: 1},
	}
	row.Reasoning = turns.Reasoning{
		Steps:    []turns.ReasoningStep{{Index: 0, Kind: turns.ReasoningKindToolCall}},
		Complete: turns.CompletenessPartial,
		Dropped:  31,
		Seq:      9,
	}
	row.Apps = []turns.AppRef{
		{EffectiveAgentID: "a1", ServerID: "s1", ResourceURI: "ui://s1/app", Availability: turns.AppAvailable, Complete: turns.CompletenessComplete},
		{EffectiveAgentID: "", ServerID: "s2", ResourceURI: "ui://s2/app", Availability: turns.AppUnavailable, Complete: turns.CompletenessComplete},
		{EffectiveAgentID: "a2", ServerID: "s3", ResourceURI: "ui://s3/app", Availability: turns.AppDegraded, Complete: turns.CompletenessComplete},
	}
	row.Inputs = []turns.Attachment{{ID: "in_1", Availability: turns.CompletenessComplete}}
	row.Outputs = []turns.Attachment{{ID: "out_1", Availability: turns.CompletenessUnavailable}}

	if _, err := s.AppendTurnIf(ctx, id, row); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err := s.GetTurn(ctx, id, "run-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !reflect.DeepEqual(got.Activity, row.Activity) {
		t.Errorf("activity overflow fields drifted:\n got: %+v\nwant: %+v", got.Activity, row.Activity)
	}
	if !reflect.DeepEqual(got.Reasoning, row.Reasoning) {
		t.Errorf("reasoning overflow fields drifted:\n got: %+v\nwant: %+v", got.Reasoning, row.Reasoning)
	}
	if !reflect.DeepEqual(got.Apps, row.Apps) {
		t.Errorf("app availability drifted:\n got: %+v\nwant: %+v", got.Apps, row.Apps)
	}
	if !reflect.DeepEqual(got.Inputs, row.Inputs) || !reflect.DeepEqual(got.Outputs, row.Outputs) {
		t.Errorf("attachment availability drifted: inputs=%+v outputs=%+v", got.Inputs, got.Outputs)
	}
	// The ordered App collection order is preserved exactly.
	if len(got.Apps) != 3 || got.Apps[0].ResourceURI != "ui://s1/app" || got.Apps[2].ResourceURI != "ui://s3/app" {
		t.Errorf("app collection order lost: %+v", got.Apps)
	}
}

func TestDurable_HonestyByDSN(t *testing.T) {
	ctx := context.Background()
	// File-backed → durable.
	fileStore, err := sqlite.New(sqlite.Config{DSN: t.TempDir() + "/durable.sqlite"})
	if err != nil {
		t.Fatalf("file New: %v", err)
	}
	if !fileStore.Durable() {
		t.Errorf("file-backed store must report Durable() == true")
	}
	_ = fileStore.Close(ctx)
	// :memory: → explicitly NOT durable (restart loss, never a silent
	// claim of durability).
	memStore, err := sqlite.New(sqlite.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("memory New: %v", err)
	}
	if memStore.Durable() {
		t.Errorf(":memory: store must report Durable() == false (explicit restart loss)")
	}
	_ = memStore.Close(ctx)
}

func TestMemoryDSN_PerOpenIsolation(t *testing.T) {
	// Two :memory: stores in one process must never collide (each
	// open gets its own uniquely named shared-cache memory DB).
	ctx := context.Background()
	idA := identity.Identity{TenantID: "t", UserID: "u", SessionID: "sA"}
	idB := identity.Identity{TenantID: "t", UserID: "u", SessionID: "sB"}

	a := openMem(t)
	defer func() { _ = a.Close(ctx) }()
	b := openMem(t)
	defer func() { _ = b.Close(ctx) }()

	if _, err := a.AppendTurnIf(ctx, idA, richRow("run-1", idA, 1)); err != nil {
		t.Fatalf("append A: %v", err)
	}
	// B is a fresh store: its session has no rows.
	if _, err := b.GetTurn(ctx, idB, "run-1"); !errors.Is(err, turns.ErrTurnNotFound) {
		t.Errorf("cross-store get error=%v, want ErrTurnNotFound (per-open isolation)", err)
	}
	if seq, err := b.LoadCheckpoint(ctx, idB); err != nil || seq != 0 {
		t.Errorf("cross-store checkpoint=%d err=%v, want 0 nil", seq, err)
	}
	// Even the SAME session id in a separate store is isolated.
	if _, err := b.GetTurn(ctx, idA, "run-1"); !errors.Is(err, turns.ErrTurnNotFound) {
		t.Errorf("same-session cross-store get error=%v, want ErrTurnNotFound", err)
	}
}

func TestNew_EmptyDSN_FailsLoudly(t *testing.T) {
	if _, err := sqlite.New(sqlite.Config{DSN: ""}); err == nil {
		t.Fatalf("empty DSN must fail loudly, got nil")
	}
}

func TestNew_GarbageFileDSN_FailsLoudly(t *testing.T) {
	// A DSN that cannot be opened as a database must surface a
	// wrapped error, never a silently working stub.
	if _, err := sqlite.New(sqlite.Config{DSN: t.TempDir() + "/no/such/dir/turns.sqlite"}); err == nil {
		t.Fatalf("unopenable DSN must fail loudly, got nil")
	}
}

func TestAppend_EmptyTurnID_FailsLoudly(t *testing.T) {
	s := openMem(t)
	defer func() { _ = s.Close(context.Background()) }()
	ctx := context.Background()
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	if _, err := s.AppendTurnIf(ctx, id, turns.TurnRow{}); !errors.Is(err, turns.ErrInvalidInput) {
		t.Errorf("empty turn id error=%v, want ErrInvalidInput", err)
	}
}
