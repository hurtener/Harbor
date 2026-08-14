package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/sessions/turns/conformancetest"
	"github.com/hurtener/Harbor/internal/sessions/turns/drivers/postgres"
)

// TestPostgres_Conformance runs the canonical turns.Store conformance
// suite against a Postgres connection. The test gates on
// HARBOR_PG_DSN: locally without Postgres available the test skips
// cleanly; CI provides a postgres:16 service container.
func TestPostgres_Conformance(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)

	conformancetest.Run(t, func() (turns.Store, func()) {
		// Each conformance subtest gets its own driver instance so
		// state from one subtest can't bleed into another. We share
		// a single schema across subtests and TRUNCATE between them
		// so the per-test setup cost stays bounded.
		s, err := postgres.New(postgres.Config{DSN: dsn})
		if err != nil {
			t.Fatalf("postgres.New: %v", err)
		}
		truncateTurns(t, dsn)
		return s, func() { _ = s.Close(context.Background()) }
	})
}

// TestPostgres_New_RequiresDSN pins the explicit-DSN-required
// contract. Empty DSN must surface a clear error rather than panic
// inside sql.Open.
func TestPostgres_New_RequiresDSN(t *testing.T) {
	_, err := postgres.New(postgres.Config{DSN: ""})
	if err == nil {
		t.Fatalf("expected error on empty DSN")
	}
	if !strings.Contains(err.Error(), "DSN") {
		t.Errorf("error should mention DSN; got: %v", err)
	}
}

// TestPostgres_Retention_EvictsOldestAndFlagsTruncation proves the
// configured retention bound: a session retains only its newest N
// rows, the oldest are evicted, the truncation flag is surfaced on
// every page, and an evicted turn answers not-found (the projection is
// bounded; eviction is explicit, never silent). DSN-dependent.
func TestPostgres_Retention_EvictsOldestAndFlagsTruncation(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)

	const retention = 5
	s, err := postgres.New(postgres.Config{DSN: dsn, Retention: retention})
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	ctx := context.Background()

	for i := 1; i <= 8; i++ {
		if _, err := s.AppendTurnIf(ctx, fixtureID, freshRow(fmt.Sprintf("run-%d", i))); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	rows, next, info, err := s.ListTurns(ctx, fixtureID, nil, 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != retention {
		t.Errorf("retained %d rows, want %d (newest window)", len(rows), retention)
	}
	if next != nil {
		t.Errorf("next=%v, want nil (the retained window is exhausted)", next != nil)
	}
	if !info.Truncated {
		t.Errorf("truncation flag not set after retention eviction")
	}
	// Newest window retained: run-8..run-4.
	if rows[0].TurnID != "run-8" || rows[retention-1].TurnID != "run-4" {
		t.Errorf("retained window wrong: %v", rowIDs(rows))
	}
	// The evicted oldest turns answer not-found.
	for _, tid := range []string{"run-1", "run-2", "run-3"} {
		if _, err := s.GetTurn(ctx, fixtureID, turns.TurnID(tid)); !errors.Is(err, turns.ErrTurnNotFound) {
			t.Errorf("evicted %q error=%v, want ErrTurnNotFound", tid, err)
		}
	}
	// The evicted turns never appear in a keyset walk either.
	var walked []turns.TurnRow
	var cursor *turns.Cursor
	for {
		page, next, _, err := s.ListTurns(ctx, fixtureID, cursor, 2)
		if err != nil {
			t.Fatalf("walk: %v", err)
		}
		walked = append(walked, page...)
		if next == nil {
			break
		}
		cursor = next
	}
	if len(walked) != retention {
		t.Errorf("walked %d rows, want %d (no resurrection of evicted rows)", len(walked), retention)
	}
}

// TestPostgres_Restart_SurvivesCloseReopen proves the DURABLE leg of
// the store: rows, the monotonic checkpoint, AND the store-local
// erasure fence all survive a process restart (a Close + a fresh
// driver over the same schema). An erased session stays fenced across
// the restart — no replay resurrection. DSN-dependent.
func TestPostgres_Restart_SurvivesCloseReopen(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)
	ctx := context.Background()

	// First "process".
	s1, err := postgres.New(postgres.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("postgres.New (first process): %v", err)
	}
	if !s1.Durable() {
		t.Errorf("Durable()=false, want true (Postgres survives restart)")
	}
	row, err := s1.AppendTurnIf(ctx, fixtureID, freshRow("run-1"))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	sealed := row
	sealed.Status = turns.StatusComplete
	sealed.Sealed = true
	if _, err := s1.SealTurnIf(ctx, fixtureID, "run-1", row.Version, sealed); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := s1.SaveCheckpoint(ctx, fixtureID, 77); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := s1.FenceSession(ctx, fixtureID); err != nil {
		t.Fatalf("fence: %v", err)
	}
	if err := s1.Close(ctx); err != nil {
		t.Fatalf("close (first process): %v", err)
	}

	// Second "process" over the same schema.
	s2, err := postgres.New(postgres.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("postgres.New (second process): %v", err)
	}
	defer func() { _ = s2.Close(ctx) }()

	got, err := s2.GetTurn(ctx, fixtureID, "run-1")
	if err != nil {
		t.Fatalf("post-restart get: %v", err)
	}
	if !got.Sealed || got.Status != turns.StatusComplete {
		t.Errorf("post-restart row lost its sealed terminal form: %+v", got)
	}
	if cp, err := s2.LoadCheckpoint(ctx, fixtureID); err != nil || cp != 77 {
		t.Errorf("post-restart checkpoint = (%d, %v), want (77, nil)", cp, err)
	}
	// The PERMANENT store-local fence survived the restart: every
	// write path is refused, and even a re-erase cannot resurrect the
	// session.
	if _, err := s2.AppendTurnIf(ctx, fixtureID, freshRow("run-2")); !errors.Is(err, turns.ErrErasureFenced) {
		t.Errorf("post-restart append error=%v, want ErrErasureFenced (fence survives restart)", err)
	}
	if err := s2.SaveCheckpoint(ctx, fixtureID, 78); !errors.Is(err, turns.ErrErasureFenced) {
		t.Errorf("post-restart checkpoint error=%v, want ErrErasureFenced", err)
	}
	if n, err := s2.DeleteScope(ctx, fixtureID); err != nil || n == 0 {
		t.Errorf("post-restart erase = (%d, %v), want (>0, nil) — the sealed row is still erasable", n, err)
	}
	if _, err := s2.AppendTurnIf(ctx, fixtureID, freshRow("run-3")); !errors.Is(err, turns.ErrErasureFenced) {
		t.Errorf("post-erase append error=%v, want ErrErasureFenced (no resurrection)", err)
	}
}

// TestPostgres_IdentityIsolation_CrossSessionNoBleed proves identity
// scoping at the driver boundary: the same turn id under a different
// (tenant, user, session) triple is fully independent, and a walk of
// one session never surfaces another session's rows. DSN-dependent.
func TestPostgres_IdentityIsolation_CrossSessionNoBleed(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)

	s, err := postgres.New(postgres.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	ctx := context.Background()

	a := identity.Identity{TenantID: "t-a", UserID: "u-a", SessionID: "s-a"}
	b := identity.Identity{TenantID: "t-b", UserID: "u-b", SessionID: "s-b"}
	c := identity.Identity{TenantID: "t-a", UserID: "u-a", SessionID: "s-c"} // same tenant+user, different session

	for i := 1; i <= 6; i++ {
		if _, err := s.AppendTurnIf(ctx, a, freshRow(fmt.Sprintf("turn-%d", i))); err != nil {
			t.Fatalf("append a %d: %v", i, err)
		}
	}
	// The same turn ids under other triples are independent.
	if _, err := s.AppendTurnIf(ctx, b, freshRow("turn-1")); err != nil {
		t.Fatalf("append b: %v", err)
	}
	if _, err := s.AppendTurnIf(ctx, c, freshRow("turn-1")); err != nil {
		t.Fatalf("append c: %v", err)
	}
	if _, err := s.AppendTurnIf(ctx, b, freshRow("turn-2")); err != nil {
		t.Fatalf("append b2: %v", err)
	}

	walk := func(id identity.Identity) []string {
		t.Helper()
		var out []string
		var cursor *turns.Cursor
		for {
			page, next, _, err := s.ListTurns(ctx, id, cursor, 3)
			if err != nil {
				t.Fatalf("list %v: %v", id, err)
			}
			for _, r := range page {
				out = append(out, string(r.TurnID))
			}
			if next == nil {
				return out
			}
			cursor = next
		}
	}
	if got := walk(a); len(got) != 6 {
		t.Errorf("session a walked %d rows, want 6 (no cross-session bleed)", len(got))
	}
	if got := walk(b); len(got) != 2 {
		t.Errorf("session b walked %d rows, want 2", len(got))
	}
	if got := walk(c); len(got) != 1 {
		t.Errorf("session c walked %d rows, want 1", len(got))
	}
	// A turn under b is not addressable from a.
	if _, err := s.GetTurn(ctx, a, "turn-1"); err != nil {
		t.Fatalf("get a turn-1: %v", err)
	}
	if _, err := s.GetTurn(ctx, b, "turn-1"); err != nil {
		t.Fatalf("get b turn-1: %v", err)
	}
	// Fencing one session never fences its sibling.
	if err := s.FenceSession(ctx, a); err != nil {
		t.Fatalf("fence a: %v", err)
	}
	if _, err := s.AppendTurnIf(ctx, b, freshRow("turn-3")); err != nil {
		t.Errorf("sibling session b must survive a's fence: %v", err)
	}
	if _, err := s.AppendTurnIf(ctx, a, freshRow("turn-7")); !errors.Is(err, turns.ErrErasureFenced) {
		t.Errorf("fenced session a append error=%v, want ErrErasureFenced", err)
	}
}

// TestPostgres_EffectiveAgentIndexed_WithinSession proves the
// effective-agent column is carried and scoped per session: the same
// agent binding resolves independently under each triple (agent id is
// selection metadata, never an isolation principal). DSN-dependent.
func TestPostgres_EffectiveAgentIndexed_WithinSession(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)

	s, err := postgres.New(postgres.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	ctx := context.Background()

	a := identity.Identity{TenantID: "t-a", UserID: "u-a", SessionID: "s-a"}
	other := identity.Identity{TenantID: "t-a", UserID: "u-a", SessionID: "s-b"}

	row := freshRow("run-1")
	row.Agent = turns.Agent{ID: "agent-7", Name: "Agent Seven", BindingSource: turns.AgentBindingExplicit, Complete: turns.CompletenessComplete}
	if _, err := s.AppendTurnIf(ctx, a, row); err != nil {
		t.Fatalf("append: %v", err)
	}
	// The effective agent binding round-trips on the row.
	got, err := s.GetTurn(ctx, a, "run-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Agent.ID != "agent-7" || got.Agent.BindingSource != turns.AgentBindingExplicit {
		t.Errorf("effective agent binding lost through the driver: %+v", got.Agent)
	}
	// Updating the row replaces the effective agent binding.
	next := got
	next.Agent = turns.Agent{ID: "agent-9", Name: "Agent Nine", BindingSource: turns.AgentBindingExplicit, Complete: turns.CompletenessComplete}
	updated, err := s.UpdateTurnIf(ctx, a, "run-1", got.Version, next)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Agent.ID != "agent-9" {
		t.Errorf("updated effective agent=%q, want agent-9", updated.Agent.ID)
	}
	// The same turn id under a different session is independent.
	if _, err := s.AppendTurnIf(ctx, other, freshRow("run-1")); err != nil {
		t.Fatalf("append other session: %v", err)
	}
	gotOther, err := s.GetTurn(ctx, other, "run-1")
	if err != nil {
		t.Fatalf("get other: %v", err)
	}
	if gotOther.Agent.ID != "" {
		t.Errorf("other session's row leaked the agent binding: %q", gotOther.Agent.ID)
	}
}

func rowIDs(rows []turns.TurnRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = string(r.TurnID)
	}
	return out
}

// freshRow builds a minimal valid mutable turn row (the store mints
// the sequence; the driver must not require a caller-supplied one).
func freshRow(turnID string) turns.TurnRow {
	return turns.TurnRow{
		TurnID:     turns.TurnID(turnID),
		SessionID:  fixtureID.SessionID,
		TieBreaker: turns.TurnID(turnID),
		Status:     turns.StatusRunning,
		Query:      turns.Query{Text: "q", Complete: turns.CompletenessComplete},
	}
}
