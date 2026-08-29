package protocol

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/sessions/turns"
)

// ---- shared fixtures ----

var fixtureID = identity.Identity{TenantID: "tenant-a", UserID: "user-1", SessionID: "session-1"}

// verifiedCtx seats the verified identity on a fresh context — the
// request-edge anchor the Service reads.
func verifiedCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.WithVerified: %v", err)
	}
	return ctx
}

// newTestService builds the Service over a production projector on an
// in-memory store, with the canonical reach gates wired and the
// recording bus wired unless an option overrides. Returns the Service,
// the store, the projector, and the recording bus.
func newTestService(t *testing.T, opts ...Option) (*Service, *memStore, *turns.Projector, *recordingBus) {
	t.Helper()
	st := newMemStore(true)
	proj := mustProjector(t, st)
	bus := &recordingBus{}
	defaults := []Option{
		WithSessionReachAuthorizer(auth.NewSessionReachAuthorizer()),
		WithAgentReachAuthorizer(auth.NewAgentReachAuthorizer()),
		WithBus(bus),
	}
	svc, err := NewService(proj, append(defaults, opts...)...)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, st, proj, bus
}

// fixtureRow builds one seeded consumer row with the given query and
// agent binding.
func fixtureRow(turnID string, status turns.Status, agentID string) turns.TurnRow {
	agentComplete := turns.CompletenessUnavailable
	binding := turns.AgentBindingUnknown
	if agentID != "" {
		agentComplete = turns.CompletenessComplete
		binding = turns.AgentBindingExplicit
	}
	now := time.Now()
	return turns.TurnRow{
		TurnID:              turns.TurnID(turnID),
		TaskID:              turnID,
		Status:              status,
		Sealed:              status.Terminal(),
		Version:             1,
		StartedAt:           now,
		UpdatedAt:           now,
		Query:               turns.Query{Text: "query-" + turnID, At: now, Complete: turns.CompletenessComplete},
		Agent:               turns.Agent{ID: agentID, Name: agentID, BindingSource: binding, Complete: agentComplete},
		Answer:              turns.Answer{State: turns.AnswerStateEmpty, Complete: turns.CompletenessComplete},
		Usage:               turns.Usage{Model: "model-a"},
		Activity:            turns.Activity{Complete: turns.CompletenessUnavailable},
		Reasoning:           turns.Reasoning{Complete: turns.CompletenessUnavailable},
		LastAppliedEventSeq: 7,
	}
}

// seedN plants n fixture rows with ids turn-00 … turn-<n-1> in
// OLDEST-first seed order (turn-<n-1> gets the lowest sequence), so
// the newest retained row is always turn-00. Returns the stored rows
// in seed order.
func seedN(t *testing.T, st *memStore, id identity.Identity, n int) []turns.TurnRow {
	t.Helper()
	out := make([]turns.TurnRow, 0, n)
	for i := n - 1; i >= 0; i-- {
		row := mustSeedRow(t, st, id, fixtureRow(turnIDAt(i), turns.StatusComplete, ""))
		out = append(out, row)
	}
	return out
}

func turnIDAt(i int) string { return fmt.Sprintf("turn-%02d", i) }

// recordingBus records published events for audit assertions.
type recordingBus struct {
	mu        sync.Mutex
	published []events.Event
}

func (b *recordingBus) Publish(_ context.Context, ev events.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.published = append(b.published, ev)
	return nil
}

func (b *recordingBus) PublishLive(ctx context.Context, ev events.Event) error {
	return b.Publish(ctx, ev)
}

func (b *recordingBus) Subscribe(context.Context, events.Filter) (events.Subscription, error) {
	return nil, errors.New("recordingBus: Subscribe not wired")
}

func (b *recordingBus) Close(context.Context) error { return nil }

func (b *recordingBus) adminEvents() []events.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []events.Event
	for _, ev := range b.published {
		if ev.Type == events.EventTypeAdminScopeUsed {
			out = append(out, ev)
		}
	}
	return out
}

// recordingProjector records which seam methods a call reached — the
// no-fallback / single-read gate.
type recordingProjector struct {
	mu    sync.Mutex
	calls []string
	inner Projector
}

func (r *recordingProjector) List(ctx context.Context, id identity.Identity, opts turns.ListOptions) (turns.Page, error) {
	r.mu.Lock()
	r.calls = append(r.calls, "List")
	r.mu.Unlock()
	return r.inner.List(ctx, id, opts)
}

func (r *recordingProjector) Get(ctx context.Context, id identity.Identity, turnID turns.TurnID) (turns.TurnRow, error) {
	r.mu.Lock()
	r.calls = append(r.calls, "Get")
	r.mu.Unlock()
	return r.inner.Get(ctx, id, turnID)
}

func (r *recordingProjector) OpsTurn(ctx context.Context, id identity.Identity, turnID turns.TurnID) (turns.OpsTurnRow, error) {
	r.mu.Lock()
	r.calls = append(r.calls, "OpsTurn")
	r.mu.Unlock()
	return r.inner.OpsTurn(ctx, id, turnID)
}

func (r *recordingProjector) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// ---- identity is mandatory ----

func TestService_List_IdentityRequired_FailsClosed(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	// No verified identity on ctx at all.
	_, err := svc.List(context.Background(), ListRequest{SessionID: "session-1"})
	if !errIs(err, ErrIdentityRequired) {
		t.Fatalf("List without verified identity: err = %v, want ErrIdentityRequired", err)
	}
	_, err = svc.Get(context.Background(), GetRequest{SessionID: "session-1", TaskID: "turn-a"})
	if !errIs(err, ErrIdentityRequired) {
		t.Fatalf("Get without verified identity: err = %v, want ErrIdentityRequired", err)
	}
}

// ---- request validation ----

func TestService_List_InvalidRequest_FailsLoud(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := verifiedCtx(t, fixtureID)

	if _, err := svc.List(ctx, ListRequest{SessionID: ""}); !errIs(err, ErrInvalidRequest) {
		t.Fatalf("empty session_id: err = %v, want ErrInvalidRequest", err)
	}
	if _, err := svc.List(ctx, ListRequest{SessionID: "session-1", Projection: "bogus"}); !errIs(err, ErrInvalidRequest) {
		t.Fatalf("unknown projection: err = %v, want ErrInvalidRequest", err)
	}
	if _, err := svc.List(ctx, ListRequest{SessionID: "session-1", Projection: ProjectionOperations}); !errIs(err, ErrInvalidRequest) {
		t.Fatalf("operations projection on list: err = %v, want ErrInvalidRequest", err)
	}
	if _, err := svc.List(ctx, ListRequest{SessionID: "session-1", Limit: turns.MaxListLimit + 1}); !errIs(err, ErrInvalidRequest) {
		t.Fatalf("limit above max: err = %v, want ErrInvalidRequest", err)
	}
	if _, err := svc.List(ctx, ListRequest{SessionID: "session-1", Limit: -1}); !errIs(err, ErrInvalidRequest) {
		t.Fatalf("negative limit: err = %v, want ErrInvalidRequest", err)
	}
}

func TestService_Get_InvalidRequest_FailsLoud(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := verifiedCtx(t, fixtureID)

	if _, err := svc.Get(ctx, GetRequest{SessionID: "", TaskID: "turn-a"}); !errIs(err, ErrInvalidRequest) {
		t.Fatalf("empty session_id: err = %v, want ErrInvalidRequest", err)
	}
	if _, err := svc.Get(ctx, GetRequest{SessionID: "session-1", TaskID: ""}); !errIs(err, ErrInvalidRequest) {
		t.Fatalf("empty task_id: err = %v, want ErrInvalidRequest", err)
	}
	if _, err := svc.Get(ctx, GetRequest{SessionID: "session-1", TaskID: "turn-a", Projection: "bogus"}); !errIs(err, ErrInvalidRequest) {
		t.Fatalf("unknown projection: err = %v, want ErrInvalidRequest", err)
	}
}

// ---- session reach ----

func TestService_List_SessionReach_PresentClaimDeniesForeignSession(t *testing.T) {
	svc, st, _, _ := newTestService(t)
	seedN(t, st, fixtureID, 2)

	// A PRESENT session_reach claim that excludes the effective session
	// denies LOUDLY (the settled signed-reach contract).
	ctx := auth.WithSessionReach(verifiedCtx(t, fixtureID), []string{"session-other"})
	if _, err := svc.List(ctx, ListRequest{SessionID: "session-1"}); !errIs(err, ErrSessionReachDenied) {
		t.Fatalf("list with excluding reach claim: err = %v, want ErrSessionReachDenied", err)
	}
	if _, err := svc.Get(ctx, GetRequest{SessionID: "session-1", TaskID: "turn-a"}); !errIs(err, ErrSessionReachDenied) {
		t.Fatalf("get with excluding reach claim: err = %v, want ErrSessionReachDenied", err)
	}

	// A present claim that INCLUDES the effective session passes the
	// reach gate (the exact-session boundary decides next).
	ctx = auth.WithSessionReach(verifiedCtx(t, fixtureID), []string{"session-1"})
	if _, err := svc.List(ctx, ListRequest{SessionID: "session-1"}); err != nil {
		t.Fatalf("list with including reach claim: unexpected err %v", err)
	}

	// An ABSENT claim preserves dynamic selection.
	ctx = verifiedCtx(t, fixtureID)
	if _, err := svc.List(ctx, ListRequest{SessionID: "session-1"}); err != nil {
		t.Fatalf("list with absent reach claim: unexpected err %v", err)
	}
}

func TestService_List_SessionReach_UnwiredGatePasses(t *testing.T) {
	// No session-reach gate wired: the transport edge is the
	// enforcement point; the exact-session boundary still holds.
	svc, st, _, _ := newTestService(t, WithSessionReachAuthorizer(nil))
	seedN(t, st, fixtureID, 1)
	ctx := verifiedCtx(t, fixtureID)
	if _, err := svc.List(ctx, ListRequest{SessionID: "session-1"}); err != nil {
		t.Fatalf("unwired gate list: unexpected err %v", err)
	}
}

// ---- exact-session boundary: non-oracular ----

func TestService_List_ForeignSession_NonOracularNotFound(t *testing.T) {
	svc, st, _, _ := newTestService(t)
	seedN(t, st, fixtureID, 3)

	// A sibling session of the same user, a foreign user, a foreign
	// tenant, and a never-existing session all answer EXACTLY the same
	// typed not-found when the requested session is not the caller's
	// own — cross-identity denial never becomes an existence oracle.
	for _, tc := range []struct {
		name string
		id   identity.Identity
		sess string
	}{
		{name: "sibling session of own user", id: fixtureID, sess: "session-other"},
		{name: "foreign user same tenant", id: identity.Identity{TenantID: "tenant-a", UserID: "user-2", SessionID: "session-2"}, sess: "session-1"},
		{name: "cross tenant", id: identity.Identity{TenantID: "tenant-b", UserID: "user-1", SessionID: "session-b1"}, sess: "session-1"},
		{name: "never existed", id: fixtureID, sess: "session-never"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := verifiedCtx(t, tc.id)
			_, err := svc.List(ctx, ListRequest{SessionID: tc.sess})
			if !errIs(err, ErrTurnNotFound) {
				t.Fatalf("list: err = %v, want ErrTurnNotFound (non-oracular)", err)
			}
			_, err = svc.Get(ctx, GetRequest{SessionID: tc.sess, TaskID: "turn-a"})
			if !errIs(err, ErrTurnNotFound) {
				t.Fatalf("get: err = %v, want ErrTurnNotFound (non-oracular)", err)
			}
		})
	}
}

func TestService_List_OwnEmptySession_HonestEmptyPage(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := verifiedCtx(t, fixtureID)
	// The caller's OWN session with zero turns is an honest empty page
	// (not an error) — distinct from the foreign-session typed not-found.
	resp, err := svc.List(ctx, ListRequest{SessionID: "session-1"})
	if err != nil {
		t.Fatalf("list own empty session: unexpected err %v", err)
	}
	if len(resp.Turns) != 0 {
		t.Fatalf("own empty session page has %d turns, want 0", len(resp.Turns))
	}
	if resp.HasMore {
		t.Fatal("own empty session page reports HasMore")
	}
	if resp.PageCompleteness != turns.CompletenessComplete {
		t.Fatalf("own empty session completeness = %q, want complete", resp.PageCompleteness)
	}
}

// ---- limits, defaults, response shape ----

func TestService_List_DefaultLimitAndShape(t *testing.T) {
	svc, st, _, _ := newTestService(t)
	seedN(t, st, fixtureID, turns.MaxListLimit+2)
	ctx := verifiedCtx(t, fixtureID)

	// Zero limit ⇒ the Protocol-mandated default of 20.
	resp, err := svc.List(ctx, ListRequest{SessionID: "session-1"})
	if err != nil {
		t.Fatalf("list default: unexpected err %v", err)
	}
	if len(resp.Turns) != turns.DefaultListLimit {
		t.Fatalf("default page has %d turns, want %d", len(resp.Turns), turns.DefaultListLimit)
	}
	if resp.Header.SessionID != "session-1" {
		t.Fatalf("header session = %q, want session-1", resp.Header.SessionID)
	}
	if resp.Header.AsOf.IsZero() {
		t.Fatal("header as-of is zero")
	}
	if resp.Order != OrderNewestFirst {
		t.Fatalf("order = %q, want %q", resp.Order, OrderNewestFirst)
	}
	if resp.PageCompleteness != turns.CompletenessComplete {
		t.Fatalf("completeness = %q, want complete", resp.PageCompleteness)
	}
	if resp.PartialReason != "" {
		t.Fatalf("partial reason = %q, want empty", resp.PartialReason)
	}
	if resp.LiveResumeSeq != 7 {
		t.Fatalf("live resume seq = %d, want 7 (the rows' LastAppliedEventSeq)", resp.LiveResumeSeq)
	}
	if !resp.HasMore || resp.NextOlderCursor == "" {
		t.Fatalf("expected a next cursor + HasMore: has_more=%v cursor=%q", resp.HasMore, resp.NextOlderCursor)
	}
	if resp.RemainingOlderCount == nil || *resp.RemainingOlderCount != turns.MaxListLimit+2-turns.DefaultListLimit {
		t.Fatalf("remaining = %v, want %d", resp.RemainingOlderCount, turns.MaxListLimit+2-turns.DefaultListLimit)
	}
	if !resp.CountExact {
		t.Fatal("count_exact should be true on an ungated page")
	}
}

func TestService_List_PartialRetentionHonest(t *testing.T) {
	svc, st, _, _ := newTestService(t)
	seedN(t, st, fixtureID, 3)
	st.setTruncated(fixtureID)
	ctx := verifiedCtx(t, fixtureID)

	resp, err := svc.List(ctx, ListRequest{SessionID: "session-1"})
	if err != nil {
		t.Fatalf("list: unexpected err %v", err)
	}
	if resp.PageCompleteness != turns.CompletenessPartial {
		t.Fatalf("completeness = %q, want partial (retention eviction)", resp.PageCompleteness)
	}
	if resp.PartialReason != "retention_eviction" {
		t.Fatalf("partial reason = %q, want retention_eviction", resp.PartialReason)
	}
}

// ---- cursors ----

func TestService_List_Cursor_TypedOutcomes(t *testing.T) {
	svc, st, _, _ := newTestService(t)
	rows := seedN(t, st, fixtureID, 3)
	ctx := verifiedCtx(t, fixtureID)

	// Malformed cursor fails loud with the domain's invalid-cursor
	// sentinel — never a silent reset to page one.
	if _, err := svc.List(ctx, ListRequest{SessionID: "session-1", OlderCursor: "!!not-base64!!"}); !errIs(err, turns.ErrInvalidCursor) {
		t.Fatalf("malformed cursor: err = %v, want turns.ErrInvalidCursor", err)
	}

	// A valid cursor over the newest page.
	first, err := svc.List(ctx, ListRequest{SessionID: "session-1", Limit: 2})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	cur, err := turns.DecodeCursor(first.NextOlderCursor)
	if err != nil {
		t.Fatalf("decode next cursor: %v", err)
	}

	// Foreign-session cursor: minted for another session ⇒ typed
	// foreign-session.
	foreign := &turns.Cursor{SessionID: "session-other", Snapshot: cur.Snapshot, Seq: cur.Seq, TurnID: cur.TurnID}
	if _, err := svc.List(ctx, ListRequest{SessionID: "session-1", OlderCursor: foreign.Encode()}); !errIs(err, turns.ErrCursorForeignSession) {
		t.Fatalf("foreign cursor: err = %v, want turns.ErrCursorForeignSession", err)
	}

	// Stale-snapshot cursor: snapshot generation advanced (erasure)
	// ⇒ typed stale-snapshot.
	stale := &turns.Cursor{SessionID: "session-1", Snapshot: cur.Snapshot + 1, Seq: cur.Seq, TurnID: cur.TurnID}
	if _, err := svc.List(ctx, ListRequest{SessionID: "session-1", OlderCursor: stale.Encode()}); !errIs(err, turns.ErrCursorSnapshotStale) {
		t.Fatalf("stale snapshot cursor: err = %v, want turns.ErrCursorSnapshotStale", err)
	}

	// Retention-expired cursor: boundary row no longer retained ⇒ typed
	// retention-expired.
	expired := &turns.Cursor{SessionID: "session-1", Snapshot: cur.Snapshot, Seq: cur.Seq, TurnID: "turn-gone"}
	if _, err := svc.List(ctx, ListRequest{SessionID: "session-1", OlderCursor: expired.Encode()}); !errIs(err, turns.ErrCursorExpired) {
		t.Fatalf("expired cursor: err = %v, want turns.ErrCursorExpired", err)
	}

	// Forged cursor: names a RETAINED boundary row with a sequence that
	// does not match the stored row ⇒ typed invalid-cursor (would
	// otherwise silently skip/repeat rows).
	forged := &turns.Cursor{SessionID: "session-1", Snapshot: cur.Snapshot, Seq: cur.Seq + 999, TurnID: cur.TurnID}
	if _, err := svc.List(ctx, ListRequest{SessionID: "session-1", OlderCursor: forged.Encode()}); !errIs(err, turns.ErrInvalidCursor) {
		t.Fatalf("forged cursor: err = %v, want turns.ErrInvalidCursor", err)
	}

	// The genuine cursor pages older rows with no duplicates.
	second, err := svc.List(ctx, ListRequest{SessionID: "session-1", OlderCursor: first.NextOlderCursor, Limit: 2})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	seen := map[string]bool{}
	for _, r := range append(append([]turns.TurnRow{}, first.Turns...), second.Turns...) {
		if seen[string(r.TurnID)] {
			t.Fatalf("duplicate turn %q across pages", r.TurnID)
		}
		seen[string(r.TurnID)] = true
	}
	if !seen[string(rows[0].TurnID)] || !seen[string(rows[1].TurnID)] {
		t.Fatalf("second page skipped an allowed row: seen=%v", seen)
	}
}

func TestService_List_AppendWhilePaging_NoSkipDuplicate(t *testing.T) {
	svc, st, proj, _ := newTestService(t)
	seedN(t, st, fixtureID, 5)
	ctx := verifiedCtx(t, fixtureID)

	page1, err := svc.List(ctx, ListRequest{SessionID: "session-1", Limit: 2})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}

	// A new turn starts while the caller pages older history.
	if _, err := proj.Append(ctx, fixtureID, turns.Append{
		TurnID:  "turn-new",
		TaskID:  "turn-new",
		Query:   "newest",
		QueryAt: time.Now(),
		Status:  turns.StatusRunning,
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	seen := map[string]bool{}
	for _, r := range page1.Turns {
		seen[string(r.TurnID)] = true
	}
	cur := page1.NextOlderCursor
	for cur != "" {
		page, err := svc.List(ctx, ListRequest{SessionID: "session-1", OlderCursor: cur, Limit: 2})
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		for _, r := range page.Turns {
			if seen[string(r.TurnID)] {
				t.Fatalf("duplicate turn %q while appending", r.TurnID)
			}
			seen[string(r.TurnID)] = true
		}
		if page.HasMore != (page.NextOlderCursor != "") {
			t.Fatalf("HasMore=%v inconsistent with cursor=%q", page.HasMore, page.NextOlderCursor)
		}
		cur = page.NextOlderCursor
	}
	// All 5 original rows seen exactly once — no skip, no duplicate —
	// and the appended turn NEVER satisfies the already-issued cursor
	// (a newly appended turn can never appear on an older page).
	if len(seen) != 5 {
		t.Fatalf("saw %d distinct turns, want 5: %v", len(seen), seen)
	}
	if seen["turn-new"] {
		t.Fatal("appended turn appeared on an already-issued cursor")
	}
	for i := range 5 {
		if !seen[turnIDAt(i)] {
			t.Fatalf("missing turn %q — a skip while appending", turnIDAt(i))
		}
	}
}

// ---- effective-agent gate ----

func TestService_Get_AgentGate_NonOracular(t *testing.T) {
	svc, st, _, _ := newTestService(t)
	mustSeedRow(t, st, fixtureID, fixtureRow("turn-a", turns.StatusComplete, "agent-a"))
	mustSeedRow(t, st, fixtureID, fixtureRow("turn-b", turns.StatusComplete, "agent-b"))
	mustSeedRow(t, st, fixtureID, fixtureRow("turn-nil", turns.StatusComplete, ""))
	ctx := verifiedCtx(t, fixtureID)

	// A caller with reach to agent-a sees turn-a (and the unbound
	// turn-nil) but turn-b answers the SAME typed not-found as an
	// absent turn — non-oracular, never an entitlement oracle.
	ctxA := auth.WithAgentReach(ctx, []string{"agent-a"})
	if _, err := svc.Get(ctxA, GetRequest{SessionID: "session-1", TaskID: "turn-a"}); err != nil {
		t.Fatalf("get turn-a with reach: unexpected err %v", err)
	}
	if _, err := svc.Get(ctxA, GetRequest{SessionID: "session-1", TaskID: "turn-b"}); !errIs(err, ErrTurnNotFound) {
		t.Fatalf("get turn-b without reach: err = %v, want ErrTurnNotFound (non-oracular)", err)
	}
	if _, err := svc.Get(ctxA, GetRequest{SessionID: "session-1", TaskID: "turn-nil"}); err != nil {
		t.Fatalf("get unbound turn: unexpected err %v", err)
	}

	// NO reach claim on ctx: the gate denies named-agent turns (missing
	// context authority fails closed) but serves the unbound turn.
	if _, err := svc.Get(ctx, GetRequest{SessionID: "session-1", TaskID: "turn-a"}); !errIs(err, ErrTurnNotFound) {
		t.Fatalf("get turn-a without reach claim: err = %v, want ErrTurnNotFound", err)
	}
	if _, err := svc.Get(ctx, GetRequest{SessionID: "session-1", TaskID: "turn-nil"}); err != nil {
		t.Fatalf("get unbound turn without reach claim: unexpected err %v", err)
	}
}

func TestService_Get_AgentGate_UnwiredFailsClosed(t *testing.T) {
	// A Service without a wired agent gate fails closed: named-agent
	// turns are not served (never a silent widening), unbound turns are.
	svc, st, _, _ := newTestService(t, WithAgentReachAuthorizer(nil))
	mustSeedRow(t, st, fixtureID, fixtureRow("turn-a", turns.StatusComplete, "agent-a"))
	mustSeedRow(t, st, fixtureID, fixtureRow("turn-nil", turns.StatusComplete, ""))
	ctx := verifiedCtx(t, fixtureID)

	if _, err := svc.Get(ctx, GetRequest{SessionID: "session-1", TaskID: "turn-a"}); !errIs(err, ErrTurnNotFound) {
		t.Fatalf("unwired gate named-agent get: err = %v, want ErrTurnNotFound", err)
	}
	if _, err := svc.Get(ctx, GetRequest{SessionID: "session-1", TaskID: "turn-nil"}); err != nil {
		t.Fatalf("unwired gate unbound get: unexpected err %v", err)
	}
}

func TestService_List_AgentGate_FiltersAndDegradesCount(t *testing.T) {
	svc, st, _, _ := newTestService(t)
	// Oldest row unbound; the two NEWEST rows bound to agent-a — so the
	// first page is entirely gated for a caller with no reach.
	mustSeedRow(t, st, fixtureID, fixtureRow("turn-a", turns.StatusComplete, ""))
	mustSeedRow(t, st, fixtureID, fixtureRow("turn-b", turns.StatusComplete, "agent-a"))
	mustSeedRow(t, st, fixtureID, fixtureRow("turn-c", turns.StatusComplete, "agent-a"))
	ctx := verifiedCtx(t, fixtureID)

	// Caller with NO reach: both agent-a rows are gated out of page 1;
	// the exact remaining count degrades to unknown (it must not leak
	// gated rows); HasMore still leads the caller to the unbound turn.
	resp, err := svc.List(ctx, ListRequest{SessionID: "session-1", Limit: 2})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(resp.Turns) != 0 {
		t.Fatalf("page 1 has %d turns, want 0 (all gated)", len(resp.Turns))
	}
	if resp.CountExact {
		t.Fatal("count_exact must degrade to false when rows were gated")
	}
	if resp.RemainingOlderCount != nil {
		t.Fatalf("remaining must be nil when rows were gated, got %d", *resp.RemainingOlderCount)
	}
	if !resp.HasMore {
		t.Fatal("HasMore must stay true — older rows remain within the retained window")
	}
	page2, err := svc.List(ctx, ListRequest{SessionID: "session-1", OlderCursor: resp.NextOlderCursor, Limit: 2})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page2.Turns) != 1 || string(page2.Turns[0].TurnID) != "turn-a" {
		t.Fatalf("page 2 = %v, want the unbound turn-a", page2.Turns)
	}
}

func TestService_List_AgentGate_ReachAdmitsRows(t *testing.T) {
	svc, st, _, _ := newTestService(t)
	mustSeedRow(t, st, fixtureID, fixtureRow("turn-b", turns.StatusComplete, "agent-b"))
	mustSeedRow(t, st, fixtureID, fixtureRow("turn-a", turns.StatusComplete, "agent-a"))
	ctx := auth.WithAgentReach(verifiedCtx(t, fixtureID), []string{"agent-a", "agent-b"})

	resp, err := svc.List(ctx, ListRequest{SessionID: "session-1"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(resp.Turns) != 2 {
		t.Fatalf("page has %d turns, want 2 (both admitted)", len(resp.Turns))
	}
	if !resp.CountExact {
		t.Fatal("count_exact stays true when no row was gated")
	}
	if resp.RemainingOlderCount == nil || *resp.RemainingOlderCount != 0 {
		t.Fatalf("remaining = %v, want 0", resp.RemainingOlderCount)
	}
}

// ---- erased session ----

func TestService_ErasedSession_NonOracularAndNoResurrection(t *testing.T) {
	svc, st, _, _ := newTestService(t)
	seedN(t, st, fixtureID, 3)
	ctx := verifiedCtx(t, fixtureID)

	if err := st.erase(ctx, fixtureID); err != nil {
		t.Fatalf("erase: %v", err)
	}

	// Get answers the same typed not-found as a never-existing turn.
	if _, err := svc.Get(ctx, GetRequest{SessionID: "session-1", TaskID: "turn-a"}); !errIs(err, ErrTurnNotFound) {
		t.Fatalf("erased get: err = %v, want ErrTurnNotFound", err)
	}
	// List returns an honest empty page with the advanced snapshot —
	// the projection retains nothing, and a rebuild cannot resurrect
	// the rows.
	resp, err := svc.List(ctx, ListRequest{SessionID: "session-1"})
	if err != nil {
		t.Fatalf("erased list: unexpected err %v", err)
	}
	if len(resp.Turns) != 0 {
		t.Fatalf("erased list has %d turns, want 0", len(resp.Turns))
	}
	if resp.Header.SnapshotID != 1 {
		t.Fatalf("erased snapshot = %d, want 1 (erasure advanced the generation)", resp.Header.SnapshotID)
	}
	// The session stays erased — no new rows can be appended (the
	// store-local fence), so the projection cannot resurrect.
	if _, err := st.AppendTurnIf(ctx, fixtureID, fixtureRow("turn-z", turns.StatusRunning, "")); !errIs(err, turns.ErrErasureFenced) {
		t.Fatalf("append after erase: err = %v, want turns.ErrErasureFenced", err)
	}
}

// ---- get: conversation ----

func TestService_Get_Conversation_HappyPath(t *testing.T) {
	svc, st, _, _ := newTestService(t)
	row := mustSeedRow(t, st, fixtureID, fixtureRow("turn-a", turns.StatusComplete, "agent-a"))
	ctx := auth.WithAgentReach(verifiedCtx(t, fixtureID), []string{"agent-a"})

	resp, err := svc.Get(ctx, GetRequest{SessionID: "session-1", TaskID: "turn-a"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.SessionID != "session-1" {
		t.Fatalf("session = %q, want session-1", resp.SessionID)
	}
	if resp.Turn.TurnID != row.TurnID || resp.Turn.Query.Text != "query-turn-a" {
		t.Fatalf("turn mismatch: %+v", resp.Turn)
	}
	if resp.OpsTurn != nil {
		t.Fatal("ops turn must be nil on the conversation lane")
	}
}

// TestService_Get_Conversation_PreservesRichComponents pins that the public
// conversation lane carries the projector's consumer-safe reasoning,
// content-free tool activity, and durable MCP App ref without a second
// reduction or field loss. The App correlation id is metadata for resolving
// already-persisted tool context; it is never callback authority.
func TestService_Get_Conversation_PreservesRichComponents(t *testing.T) {
	svc, st, _, _ := newTestService(t)
	want := fullContentRow("turn-rich")
	want.Status = turns.StatusComplete
	want.Sealed = true
	want.Pause = turns.Pause{Availability: turns.CompletenessUnavailable}
	want.FinishedAt = want.UpdatedAt
	want.Apps[0] = turns.AppRef{
		EffectiveAgentID: "ux-prototype-agent",
		ServerID:         "atrium",
		ResourceURI:      "ui://atrium/dashboard",
		DisplayMode:      "inline",
		ToolCallID:       "call-atrium-01",
		ToolName:         "atrium_create",
		Availability:     turns.AppAvailable,
		Complete:         turns.CompletenessComplete,
	}
	stored := mustSeedRow(t, st, fixtureID, want)
	ctx := auth.WithAgentReach(verifiedCtx(t, fixtureID), []string{"agent-a"})

	resp, err := svc.Get(ctx, GetRequest{SessionID: fixtureID.SessionID, TaskID: "turn-rich"})
	if err != nil {
		t.Fatalf("get rich turn: %v", err)
	}
	if !reflect.DeepEqual(resp.Turn.Reasoning, stored.Reasoning) {
		t.Errorf("reasoning changed: got %+v want %+v", resp.Turn.Reasoning, stored.Reasoning)
	}
	if !reflect.DeepEqual(resp.Turn.Activity, stored.Activity) {
		t.Errorf("activity changed: got %+v want %+v", resp.Turn.Activity, stored.Activity)
	}
	if !reflect.DeepEqual(resp.Turn.Apps, stored.Apps) {
		t.Errorf("apps changed: got %+v want %+v", resp.Turn.Apps, stored.Apps)
	}
}

func TestService_Get_Conversation_NotFoundPassthrough(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := verifiedCtx(t, fixtureID)
	if _, err := svc.Get(ctx, GetRequest{SessionID: "session-1", TaskID: "turn-missing"}); !errIs(err, ErrTurnNotFound) {
		t.Fatalf("missing turn: err = %v, want ErrTurnNotFound", err)
	}
}

// ---- operations lane ----

func TestService_Get_Operations_ScopeDenied(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	// No admin/fleet claim on ctx: the elevated lane refuses.
	ctx := verifiedCtx(t, fixtureID)
	if _, err := svc.Get(ctx, GetRequest{SessionID: "session-1", TaskID: "turn-a", Projection: ProjectionOperations}); !errIs(err, ErrOperationsScopeDenied) {
		t.Fatalf("ops get without claim: err = %v, want ErrOperationsScopeDenied", err)
	}
}

func fullContentRow(turnID string) turns.TurnRow {
	usage := turns.Usage{Model: "model-a"}
	one := int64(100)
	usage.TotalTokens = turns.UsageMeasure{State: turns.UsageExact, Value: &one}
	row := fixtureRow(turnID, turns.StatusPaused, "agent-a")
	row.Answer = turns.Answer{
		State:    turns.AnswerStateInline,
		Inline:   "the answer text",
		Seq:      5,
		Complete: turns.CompletenessComplete,
	}
	row.Reasoning = turns.Reasoning{
		Steps: []turns.ReasoningStep{
			{Index: 0, Kind: turns.ReasoningKindToolCall},
			{Index: 1, Kind: turns.ReasoningKindAwait},
		},
		Complete: turns.CompletenessComplete,
		Dropped:  0,
		Seq:      5,
	}
	row.Activity = turns.Activity{
		Rows: []turns.ActivityRow{
			{Position: 0, Tool: "tool-a", Status: turns.ActivitySucceeded, TerminalClass: turns.ActivityTerminalSucceeded},
		},
		Complete: turns.CompletenessComplete,
		Totals:   turns.ActivityTotals{Succeeded: 1},
	}
	row.Apps = []turns.AppRef{
		{
			EffectiveAgentID: "agent-a",
			ServerID:         "server-1",
			ResourceURI:      "ui://doc",
			DisplayMode:      "inline",
			RawHTMLTrusted:   false,
			ToolCallID:       "call-1",
			ToolName:         "tool-a",
			Availability:     turns.AppAvailable,
			Complete:         turns.CompletenessComplete,
		},
	}
	row.Pause = turns.Pause{
		Class:        turns.PauseClassHitlApproval,
		Reason:       "approve before proceeding",
		Lifecycle:    turns.PauseLifecycleActive,
		Availability: turns.CompletenessComplete,
	}
	row.Inputs = []turns.Attachment{{ID: "artifact-1", Filename: "a.txt", MimeType: "text/plain", SizeBytes: 10, Availability: turns.CompletenessComplete}}
	row.Usage = usage
	return row
}

func TestService_Get_Operations_StructurallyDistinctDTO(t *testing.T) {
	svc, st, _, _ := newTestService(t)
	mustSeedRow(t, st, fixtureID, fullContentRow("turn-a"))

	// The operations DTO is structurally distinct: lifecycle / agent /
	// usage / content-free activity / counts / pause class survive, and
	// the transcript-shaped fields are unreachable.
	for _, sc := range []auth.Scope{auth.ScopeAdmin, auth.ScopeConsoleFleet} {
		ctx := auth.WithScopes(verifiedCtx(t, fixtureID), []auth.Scope{sc})
		resp, err := svc.Get(ctx, GetRequest{SessionID: "session-1", TaskID: "turn-a", Projection: ProjectionOperations})
		if err != nil {
			t.Fatalf("ops get under %q: %v", sc, err)
		}
		if resp.OpsTurn == nil {
			t.Fatalf("ops get under %q: OpsTurn nil", sc)
		}
		ops := *resp.OpsTurn
		if ops.TurnID != "turn-a" || ops.Status != turns.StatusPaused || !ops.Pause.Availability.Valid() {
			t.Fatalf("ops lifecycle/pause mangled: %+v", ops)
		}
		if ops.ReasoningSteps != 2 {
			t.Fatalf("ops reasoning steps count = %d, want 2 (count retained, steps structurally absent)", ops.ReasoningSteps)
		}
		if len(ops.Apps) != 1 || ops.Apps[0].ServerID != "server-1" || ops.Apps[0].EffectiveAgentID != "agent-a" {
			t.Fatalf("ops apps summary mangled: %+v", ops.Apps)
		}
		if ops.Inputs != 1 {
			t.Fatalf("ops inputs count = %d, want 1", ops.Inputs)
		}
		if ops.Activity.Totals.Succeeded != 1 {
			t.Fatalf("ops activity totals mangled: %+v", ops.Activity.Totals)
		}
		if ops.Usage.TotalTokens.State != turns.UsageExact {
			t.Fatalf("ops usage mangled: %+v", ops.Usage)
		}
		if resp.Turn.TurnID != "" {
			t.Fatal("consumer Turn must be empty on the operations lane")
		}
	}
}

func TestService_Get_Operations_WidenedAudit(t *testing.T) {
	svc, st, _, bus := newTestService(t)
	mustSeedRow(t, st, fixtureID, fullContentRow("turn-a"))
	mustSeedRow(t, st, fixtureID, fixtureRow("turn-b", turns.StatusComplete, ""))

	// Reading the caller's OWN session via the operations lane is gated
	// but NOT widened — no audit.
	ctxOwn := auth.WithScopes(verifiedCtx(t, fixtureID), []auth.Scope{auth.ScopeAdmin})
	if _, err := svc.Get(ctxOwn, GetRequest{SessionID: "session-1", TaskID: "turn-a", Projection: ProjectionOperations}); err != nil {
		t.Fatalf("own-session ops get: %v", err)
	}
	if n := len(bus.adminEvents()); n != 0 {
		t.Fatalf("own-session ops read emitted %d admin audits, want 0", n)
	}

	// Reading a SIBLING session (same user, different session) is a
	// WIDENED read — the canonical audit event fires with the verified
	// actor and the read target, never silently.
	other := identity.Identity{TenantID: fixtureID.TenantID, UserID: fixtureID.UserID, SessionID: "session-other"}
	mustSeedRow(t, st, other, fixtureRow("turn-x", turns.StatusComplete, "agent-a"))
	ctxWide := auth.WithScopes(verifiedCtx(t, fixtureID), []auth.Scope{auth.ScopeAdmin})
	if _, err := svc.Get(ctxWide, GetRequest{SessionID: "session-other", TaskID: "turn-x", Projection: ProjectionOperations}); err != nil {
		t.Fatalf("widened ops get: %v", err)
	}
	evs := bus.adminEvents()
	if len(evs) != 1 {
		t.Fatalf("widened ops read emitted %d admin audits, want 1", len(evs))
	}
	payload, ok := evs[0].Payload.(TurnsAdminQueryPayload)
	if !ok {
		t.Fatalf("admin audit payload = %T, want TurnsAdminQueryPayload", evs[0].Payload)
	}
	if payload.Actor != fixtureID {
		t.Fatalf("audit actor = %+v, want %+v", payload.Actor, fixtureID)
	}
	if payload.Target != other {
		t.Fatalf("audit target = %+v, want %+v", payload.Target, other)
	}
	if payload.Method != "sessions.turns.get" {
		t.Fatalf("audit method = %q, want sessions.turns.get", payload.Method)
	}
}

// ---- pause: no token, no projection-derived actionability ----

func TestService_Pause_PassesThroughUnmodified(t *testing.T) {
	svc, st, _, _ := newTestService(t)
	row := mustSeedRow(t, st, fixtureID, fullContentRow("turn-a"))
	ctx := auth.WithAgentReach(verifiedCtx(t, fixtureID), []string{"agent-a"})

	resp, err := svc.Get(ctx, GetRequest{SessionID: "session-1", TaskID: "turn-a"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.Turn.Pause != row.Pause {
		t.Fatalf("pause passed through modified: got %+v want %+v", resp.Turn.Pause, row.Pause)
	}
	if resp.Turn.Pause.Availability != turns.CompletenessComplete {
		t.Fatalf("pause availability lost: %+v", resp.Turn.Pause)
	}
	// Actionability / tokens are NOT stored anywhere on the row — the
	// projection carries class/reason/lifecycle/availability only.
	if resp.Turn.Pause.Reason == "" {
		t.Fatal("consumer-safe pause reason must be present for the owner")
	}
}

// responseTypes walks the exported response DTOs of this package and
// asserts no PAUSE-AUTHORITY field (an action token / receipt / resume
// / approval / control token) rides a read response — the service
// response is structurally unable to leak pause authority, and
// actionability must never be read from the projection. Usage token
// COUNTS (PromptTokens etc.) are legitimate read content and are not
// flagged.
func TestService_NoTokenFields_AnywhereInResponses(t *testing.T) {
	types := []reflect.Type{
		reflect.TypeOf(ListResponse{}),
		reflect.TypeOf(GetResponse{}),
		reflect.TypeOf(SessionHeader{}),
		reflect.TypeOf(turns.TurnRow{}),
		reflect.TypeOf(turns.OpsTurnRow{}),
		reflect.TypeOf(turns.Pause{}),
	}
	for _, typ := range types {
		walkFields(t, typ, typ.Name())
	}
}

func walkFields(t *testing.T, typ reflect.Type, path string) {
	t.Helper()
	if typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return
	}
	for i := range typ.NumField() {
		f := typ.Field(i)
		lower := strings.ToLower(f.Name)
		if strings.Contains(lower, "receipt") ||
			strings.Contains(lower, "actiontoken") ||
			strings.Contains(lower, "resumetoken") ||
			strings.Contains(lower, "approvaltoken") ||
			strings.Contains(lower, "pausetoken") ||
			strings.Contains(lower, "controltoken") ||
			lower == "token" || lower == "action" || lower == "receipt" {
			t.Errorf("%s.%s: pause-authority field %q must never ride a read response (actionability is computed from the verified caller's control tier, never read from the projection)",
				path, typ.Name(), f.Name)
		}
		if f.Type.Kind() == reflect.Struct || f.Type.Kind() == reflect.Slice || f.Type.Kind() == reflect.Pointer {
			walkFields(t, f.Type, path+"."+f.Name)
		}
	}
}

// ---- one projection read per call: no fallback, no per-row reads ----

func TestService_List_OneProjectionRead_NoFallback(t *testing.T) {
	_, st, proj, _ := newTestService(t)
	seedN(t, st, fixtureID, 3)
	rec := &recordingProjector{inner: proj}
	svc2, err := NewService(rec,
		WithAgentReachAuthorizer(auth.NewAgentReachAuthorizer()),
		WithBus(&recordingBus{}),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ctx := verifiedCtx(t, fixtureID)

	if _, err := svc2.List(ctx, ListRequest{SessionID: "session-1", Limit: 2}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if calls := rec.snapshot(); len(calls) != 1 || calls[0] != "List" {
		t.Fatalf("list made calls %v, want exactly [List] — no per-row reads, no fallback", calls)
	}

	// A projector failure surfaces as the error — there is NO fallback
	// to tasks/events/history, and no second call.
	svc3, err := NewService(&failingProjector{},
		WithAgentReachAuthorizer(auth.NewAgentReachAuthorizer()),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := svc3.List(verifiedCtx(t, fixtureID), ListRequest{SessionID: "session-1"}); err == nil {
		t.Fatal("list over a failing projector must fail loud")
	}
	if _, err := svc3.Get(verifiedCtx(t, fixtureID), GetRequest{SessionID: "session-1", TaskID: "turn-a"}); err == nil {
		t.Fatal("get over a failing projector must fail loud")
	}
}

func TestService_Get_OneProjectionRead_NoFallback(t *testing.T) {
	_, st, proj, _ := newTestService(t)
	mustSeedRow(t, st, fixtureID, fixtureRow("turn-a", turns.StatusComplete, ""))
	rec := &recordingProjector{inner: proj}
	svc2, err := NewService(rec,
		WithAgentReachAuthorizer(auth.NewAgentReachAuthorizer()),
		WithBus(&recordingBus{}),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ctx := verifiedCtx(t, fixtureID)

	if _, err := svc2.Get(ctx, GetRequest{SessionID: "session-1", TaskID: "turn-a"}); err != nil {
		t.Fatalf("get: %v", err)
	}
	if calls := rec.snapshot(); len(calls) != 1 || calls[0] != "Get" {
		t.Fatalf("get made calls %v, want exactly [Get]", calls)
	}

	if _, err := svc2.Get(auth.WithScopes(ctx, []auth.Scope{auth.ScopeAdmin}),
		GetRequest{SessionID: "session-1", TaskID: "turn-a", Projection: ProjectionOperations}); err != nil {
		t.Fatalf("ops get: %v", err)
	}
	if calls := rec.snapshot(); len(calls) != 2 || calls[1] != "OpsTurn" {
		t.Fatalf("ops get made calls %v, want [Get OpsTurn]", calls)
	}
}

// failingProjector fails every read loudly — the no-fallback gate.
type failingProjector struct{}

func (failingProjector) List(context.Context, identity.Identity, turns.ListOptions) (turns.Page, error) {
	return turns.Page{}, errors.New("boom")
}

func (failingProjector) Get(context.Context, identity.Identity, turns.TurnID) (turns.TurnRow, error) {
	return turns.TurnRow{}, errors.New("boom")
}

func (failingProjector) OpsTurn(context.Context, identity.Identity, turns.TurnID) (turns.OpsTurnRow, error) {
	return turns.OpsTurnRow{}, errors.New("boom")
}
