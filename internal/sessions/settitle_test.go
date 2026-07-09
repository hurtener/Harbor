package sessions_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// titleTestWiring mirrors testWiring but also returns the underlying
// state.StateStore so a test can write a deliberately-corrupted record
// directly (bypassing SetTitle) to exercise the stored-identity
// defence-in-depth branch (ErrIdentityMismatch), which is otherwise
// unreachable through the public Registry API — the StateStore key
// already scopes every load to the requested (tenant, user, id).
func titleTestWiring(t *testing.T) (*sessions.Registry, state.StateStore, events.EventBus) {
	t.Helper()
	cfg := &config.Config{
		Events: config.EventsConfig{
			Driver:                   "inmem",
			MaxSubscribersPerSession: 16,
			SubscriberBufferSize:     64,
			IdleTimeout:              60 * time.Second,
			DropWindow:               1 * time.Second,
		},
		State:    config.StateConfig{Driver: "inmem"},
		Sessions: config.SessionsConfig{IdleTTL: 24 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: 1 * time.Hour},
	}
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), cfg.Events, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	store, err := state.Open(context.Background(), cfg.State)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	reg, err := sessions.New(store, cfg.Sessions, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.CloseRegistry(context.Background()) })
	return reg, store, bus
}

// --- SetTitle semantics table ---

func TestRegistry_SetTitle_SetsManualTitle(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := reg.SetTitle(ctxFor(id), id.SessionID, id, "My first title"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	snap, err := reg.Inspect(ctxFor(id), id.SessionID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if snap.Title != "My first title" || snap.TitleSource != sessions.TitleSourceManual {
		t.Errorf("title = %q/%q, want \"My first title\"/manual", snap.Title, snap.TitleSource)
	}
}

func TestRegistry_SetTitle_ReSetOverwrites(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := reg.SetTitle(ctxFor(id), id.SessionID, id, "First"); err != nil {
		t.Fatalf("SetTitle #1: %v", err)
	}
	if err := reg.SetTitle(ctxFor(id), id.SessionID, id, "Second"); err != nil {
		t.Fatalf("SetTitle #2: %v", err)
	}
	snap, err := reg.Inspect(ctxFor(id), id.SessionID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if snap.Title != "Second" {
		t.Errorf("title = %q, want %q", snap.Title, "Second")
	}
}

func TestRegistry_SetTitle_EmptyClears(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := reg.SetTitle(ctxFor(id), id.SessionID, id, "Something"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	if err := reg.SetTitle(ctxFor(id), id.SessionID, id, ""); err != nil {
		t.Fatalf("SetTitle (clear): %v", err)
	}
	snap, err := reg.Inspect(ctxFor(id), id.SessionID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if snap.Title != "" || snap.TitleSource != sessions.TitleSourceUnset {
		t.Errorf("after clear title = %q/%q, want \"\"/unset", snap.Title, snap.TitleSource)
	}
}

func TestRegistry_SetTitle_TrimsWhitespace(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := reg.SetTitle(ctxFor(id), id.SessionID, id, "   padded title   "); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	snap, err := reg.Inspect(ctxFor(id), id.SessionID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if snap.Title != "padded title" {
		t.Errorf("title = %q, want %q", snap.Title, "padded title")
	}
	// Whitespace-only input clears (trims to empty).
	if err := reg.SetTitle(ctxFor(id), id.SessionID, id, "   \t  "); err != nil {
		t.Fatalf("SetTitle (whitespace-only): %v", err)
	}
	snap, err = reg.Inspect(ctxFor(id), id.SessionID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if snap.Title != "" || snap.TitleSource != sessions.TitleSourceUnset {
		t.Errorf("after whitespace-only title = %q/%q, want \"\"/unset", snap.Title, snap.TitleSource)
	}
}

func TestRegistry_SetTitle_OversizeRejected(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	over := strings.Repeat("x", sessions.MaxSessionTitleLen+1)
	err := reg.SetTitle(ctxFor(id), id.SessionID, id, over)
	if !errors.Is(err, sessions.ErrInvalidTitle) {
		t.Fatalf("err = %v, want ErrInvalidTitle", err)
	}
	// The record is untouched — no silent clamp (CLAUDE.md §13).
	snap, gerr := reg.Inspect(ctxFor(id), id.SessionID)
	if gerr != nil {
		t.Fatalf("Inspect: %v", gerr)
	}
	if snap.Title != "" {
		t.Errorf("title = %q after a rejected oversize SetTitle, want unchanged \"\"", snap.Title)
	}
}

func TestRegistry_SetTitle_ExactlyMaxLenAccepted(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	exact := strings.Repeat("y", sessions.MaxSessionTitleLen)
	if err := reg.SetTitle(ctxFor(id), id.SessionID, id, exact); err != nil {
		t.Fatalf("SetTitle at exactly MaxSessionTitleLen: %v", err)
	}
}

func TestRegistry_SetTitle_ControlCharsRejected(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, bad := range []string{"line one\nline two", "tab\ttab", "cr\rreturn"} {
		err := reg.SetTitle(ctxFor(id), id.SessionID, id, bad)
		if !errors.Is(err, sessions.ErrInvalidTitle) {
			t.Errorf("title %q: err = %v, want ErrInvalidTitle", bad, err)
		}
	}
}

// TestRegistry_SetTitle_InvisibleOnlyRejected pins the Unicode Cf
// posture: a title made ONLY of format characters (zero-width space
// U+200B etc.) and whitespace survives TrimSpace non-empty but renders
// as nothing — it is rejected as ErrInvalidTitle (invalid, NOT treated
// as a clear), while a title that merely CONTAINS format characters
// alongside visible text stays valid (emoji ZWJ sequences depend on
// U+200D).
func TestRegistry_SetTitle_InvisibleOnlyRejected(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := reg.SetTitle(ctxFor(id), id.SessionID, id, "visible before"); err != nil {
		t.Fatalf("SetTitle (seed): %v", err)
	}
	// Invisible-only inputs — all rejected, never a silent clear.
	for _, invisible := range []string{
		"\u200b",             // zero-width space
		"\u200b\u200b\u200b", // several
		"\u200d",             // zero-width joiner alone
		"\u200e\u200f",       // directional marks
		" \u200b \u2060 ",    // format chars mixed with whitespace
		"\ufeff",             // BOM / zero-width no-break space
	} {
		err := reg.SetTitle(ctxFor(id), id.SessionID, id, invisible)
		if !errors.Is(err, sessions.ErrInvalidTitle) {
			t.Errorf("invisible-only title %q: err = %v, want ErrInvalidTitle", invisible, err)
		}
	}
	// The pre-existing title is untouched (rejected ≠ cleared).
	snap, err := reg.Inspect(ctxFor(id), id.SessionID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if snap.Title != "visible before" {
		t.Errorf("title = %q after rejected invisible-only inputs, want %q untouched", snap.Title, "visible before")
	}
	// Format characters MIXED with visible text stay valid — the
	// family-emoji ZWJ sequence is the canonical legitimate use.
	if err := reg.SetTitle(ctxFor(id), id.SessionID, id, "family \U0001F468\u200d\U0001F469\u200d\U0001F467 chat"); err != nil {
		t.Fatalf("ZWJ-containing title should be valid: %v", err)
	}
}

func TestRegistry_SetTitle_UnknownID_NotFound(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)
	id := ident("t1", "u1", "s1")
	err := reg.SetTitle(ctxFor(id), "does-not-exist", id, "title")
	if !errors.Is(err, sessions.ErrSessionNotFound) {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
}

func TestRegistry_SetTitle_IncompleteIdentity_Rejected(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	incomplete := identity.Identity{TenantID: "t1", UserID: "", SessionID: "s1"}
	err := reg.SetTitle(ctxFor(id), id.SessionID, incomplete, "title")
	if !errors.Is(err, identity.ErrIdentityIncomplete) {
		t.Fatalf("err = %v, want ErrIdentityIncomplete", err)
	}
}

func TestRegistry_SetTitle_OnClosedRegistry_Rejected(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := reg.CloseRegistry(context.Background()); err != nil {
		t.Fatalf("CloseRegistry: %v", err)
	}
	err := reg.SetTitle(ctxFor(id), id.SessionID, id, "title")
	if !errors.Is(err, sessions.ErrRegistryClosed) {
		t.Fatalf("err = %v, want ErrRegistryClosed", err)
	}
}

func TestRegistry_SetTitle_ClosedSessionAllowed(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := reg.Close(ctxFor(id), id.SessionID, "done"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := reg.SetTitle(ctxFor(id), id.SessionID, id, "renamed after close"); err != nil {
		t.Fatalf("SetTitle on a CLOSED session should be allowed: %v", err)
	}
	snap, err := reg.Inspect(ctxFor(id), id.SessionID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if snap.Title != "renamed after close" {
		t.Errorf("title = %q, want %q", snap.Title, "renamed after close")
	}
	if !snap.Closed {
		t.Error("SetTitle must not reopen a closed session")
	}
}

func TestRegistry_SetTitle_SiblingSession_SameOwnerAllowed(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)
	caller := ident("t1", "u1", "s-caller")
	sibling := ident("t1", "u1", "s-sibling")
	if _, err := reg.Open(ctxFor(caller), caller.SessionID, caller); err != nil {
		t.Fatalf("Open caller: %v", err)
	}
	if _, err := reg.Open(ctxFor(sibling), sibling.SessionID, sibling); err != nil {
		t.Fatalf("Open sibling: %v", err)
	}
	// The caller's OWN ctx identity (SessionID = s-caller) renames the
	// SIBLING session s-sibling — the (tenant, user) write scope (D-288).
	if err := reg.SetTitle(ctxFor(caller), sibling.SessionID, caller, "Renamed sibling"); err != nil {
		t.Fatalf("SetTitle on a sibling session of the same (tenant, user): %v", err)
	}
	snap, err := reg.Inspect(ctxFor(sibling), sibling.SessionID)
	if err != nil {
		t.Fatalf("Inspect sibling: %v", err)
	}
	if snap.Title != "Renamed sibling" {
		t.Errorf("sibling title = %q, want %q", snap.Title, "Renamed sibling")
	}
}

func TestRegistry_SetTitle_CrossUserTarget_NotFound(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)
	victim := ident("t1", "u-victim", "s-victim")
	attacker := ident("t1", "u-attacker", "s-attacker")
	if _, err := reg.Open(ctxFor(victim), victim.SessionID, victim); err != nil {
		t.Fatalf("Open victim: %v", err)
	}
	if _, err := reg.Open(ctxFor(attacker), attacker.SessionID, attacker); err != nil {
		t.Fatalf("Open attacker: %v", err)
	}
	// The attacker names the victim's session id under the ATTACKER's own
	// (tenant, user) — the StateStore key (t1, u-attacker, s-victim)
	// simply doesn't exist, so this is indistinguishable from an unknown
	// id (existence is never revealed across identities).
	err := reg.SetTitle(ctxFor(attacker), victim.SessionID, attacker, "pwned")
	if !errors.Is(err, sessions.ErrSessionNotFound) {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
	// The victim's title is untouched.
	snap, gerr := reg.Inspect(ctxFor(victim), victim.SessionID)
	if gerr != nil {
		t.Fatalf("Inspect victim: %v", gerr)
	}
	if snap.Title != "" {
		t.Errorf("victim title = %q, want untouched \"\"", snap.Title)
	}
}

func TestRegistry_SetTitle_CrossTenantTarget_NotFound(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)
	victim := ident("t-victim", "u1", "s-victim")
	attacker := ident("t-attacker", "u1", "s-attacker")
	if _, err := reg.Open(ctxFor(victim), victim.SessionID, victim); err != nil {
		t.Fatalf("Open victim: %v", err)
	}
	if _, err := reg.Open(ctxFor(attacker), attacker.SessionID, attacker); err != nil {
		t.Fatalf("Open attacker: %v", err)
	}
	err := reg.SetTitle(ctxFor(attacker), victim.SessionID, attacker, "pwned")
	if !errors.Is(err, sessions.ErrSessionNotFound) {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
}

// TestRegistry_SetTitle_IdentityMismatch_Defensive exercises the
// stored-identity (tenant, user) mismatch branch, which is unreachable
// through the public Registry API (the StateStore key already scopes
// every load to the requested (tenant, user, id) pair) — it guards
// against a future driver bug, mirroring Touch/Close's identical
// defence-in-depth check. To trigger it, the test writes a
// deliberately-corrupted record directly to the shared store: a record
// SAVED under key (t1, u1, s1) whose EMBEDDED Session.Identity claims a
// different (tenant, user).
func TestRegistry_SetTitle_IdentityMismatch_Defensive(t *testing.T) {
	t.Parallel()
	reg, store, _ := titleTestWiring(t)
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Overwrite the record at the SAME StateStore key with a payload
	// whose embedded Identity disagrees — simulating a corrupt record a
	// buggy driver might return.
	corrupt := struct {
		ID       string
		Identity identity.Identity
	}{ID: id.SessionID, Identity: identity.Identity{TenantID: "t-OTHER", UserID: "u-OTHER", SessionID: id.SessionID}}
	raw, err := json.Marshal(corrupt)
	if err != nil {
		t.Fatalf("marshal corrupt record: %v", err)
	}
	if err := store.Save(context.Background(), state.StateRecord{
		ID:       state.NewEventID(),
		Identity: identity.Quadruple{Identity: id},
		Kind:     "session.lifecycle",
		Bytes:    raw,
	}); err != nil {
		t.Fatalf("Save corrupt record: %v", err)
	}
	err = reg.SetTitle(ctxFor(id), id.SessionID, id, "title")
	if !errors.Is(err, sessions.ErrIdentityMismatch) {
		t.Fatalf("err = %v, want ErrIdentityMismatch", err)
	}
}

// TestRegistry_SetTitle_EmitsContentFreeEvent pins D-288's content-free
// contract: session.title_changed carries the SessionID + Source only —
// the marshaled payload bytes never contain the title substring.
func TestRegistry_SetTitle_EmitsContentFreeEvent(t *testing.T) {
	t.Parallel()
	reg, _, bus := titleTestWiring(t)
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}

	const secretTitle = "top-secret-conversation-name-xyz"
	received := make(chan events.Event, 4)
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: "t1", User: "u1", Session: "s1",
		Types: []events.EventType{sessions.EventTypeSessionTitleChanged},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()
	go func() {
		for ev := range sub.Events() {
			received <- ev
		}
	}()

	if err := reg.SetTitle(ctxFor(id), id.SessionID, id, secretTitle); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}

	select {
	case ev := <-received:
		payload, ok := ev.Payload.(sessions.SessionTitleChangedPayload)
		if !ok {
			t.Fatalf("payload type = %T, want SessionTitleChangedPayload", ev.Payload)
		}
		if payload.SessionID != id.SessionID {
			t.Errorf("SessionID = %q, want %q", payload.SessionID, id.SessionID)
		}
		if payload.Source != string(sessions.TitleSourceManual) {
			t.Errorf("Source = %q, want %q", payload.Source, sessions.TitleSourceManual)
		}
		// The content-free assertion: marshal the payload and grep the
		// raw bytes for the title substring — it must be ABSENT.
		raw, merr := json.Marshal(payload)
		if merr != nil {
			t.Fatalf("marshal payload: %v", merr)
		}
		if strings.Contains(string(raw), secretTitle) {
			t.Fatalf("session.title_changed payload leaked the title content: %s", raw)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for session.title_changed")
	}
}

// TestRegistry_SetTitle_ClearEmitsUnsetSource pins that a clearing call
// (empty title) reports Source = "" (TitleSourceUnset), never "manual".
func TestRegistry_SetTitle_ClearEmitsUnsetSource(t *testing.T) {
	t.Parallel()
	reg, _, bus := titleTestWiring(t)
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := reg.SetTitle(ctxFor(id), id.SessionID, id, "will be cleared"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}

	received := make(chan events.Event, 4)
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: "t1", User: "u1", Session: "s1",
		Types: []events.EventType{sessions.EventTypeSessionTitleChanged},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()
	go func() {
		for ev := range sub.Events() {
			received <- ev
		}
	}()

	if err := reg.SetTitle(ctxFor(id), id.SessionID, id, ""); err != nil {
		t.Fatalf("SetTitle (clear): %v", err)
	}
	select {
	case ev := <-received:
		payload, ok := ev.Payload.(sessions.SessionTitleChangedPayload)
		if !ok {
			t.Fatalf("payload type = %T, want SessionTitleChangedPayload", ev.Payload)
		}
		if payload.Source != string(sessions.TitleSourceUnset) {
			t.Errorf("Source = %q, want unset (\"\")", payload.Source)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for session.title_changed")
	}
}

// TestRegistry_SetTitle_NoOpWriteEmitsNoEvent pins FIX-6(a): a SetTitle that
// changes nothing — clearing an already-unset title, or an identical manual
// re-set — persists nothing and emits NO session.title_changed (a content-free
// event on a no-change write is a spurious wake-up for every subscriber). A
// genuine change still emits exactly one event.
func TestRegistry_SetTitle_NoOpWriteEmitsNoEvent(t *testing.T) {
	t.Parallel()
	reg, _, bus := titleTestWiring(t)
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}

	received := make(chan events.Event, 8)
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: "t1", User: "u1", Session: "s1",
		Types: []events.EventType{sessions.EventTypeSessionTitleChanged},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()
	go func() {
		for ev := range sub.Events() {
			received <- ev
		}
	}()

	// Clearing an already-unset title is a no-op — no event.
	if err := reg.SetTitle(ctxFor(id), id.SessionID, id, ""); err != nil {
		t.Fatalf("SetTitle (clear on unset): %v", err)
	}
	// A first real set — exactly one event.
	if err := reg.SetTitle(ctxFor(id), id.SessionID, id, "A title"); err != nil {
		t.Fatalf("SetTitle (set): %v", err)
	}
	// An identical manual re-set is a no-op — no event.
	if err := reg.SetTitle(ctxFor(id), id.SessionID, id, "A title"); err != nil {
		t.Fatalf("SetTitle (identical re-set): %v", err)
	}

	// Collect events for a bounded window; exactly one (the real set) must
	// arrive, and no more.
	var got []events.Event
	deadline := time.After(500 * time.Millisecond)
collect:
	for {
		select {
		case ev := <-received:
			got = append(got, ev)
		case <-deadline:
			break collect
		}
	}
	if len(got) != 1 {
		t.Fatalf("got %d session.title_changed events, want exactly 1 (the two no-op writes must emit nothing)", len(got))
	}
	payload, ok := got[0].Payload.(sessions.SessionTitleChangedPayload)
	if !ok || payload.Source != string(sessions.TitleSourceManual) {
		t.Fatalf("the one event = %+v, want Source=manual", got[0].Payload)
	}
}

// TestRegistry_SetTitle_OldBlob_AdditiveRoundTrip proves a session
// record persisted BEFORE this phase (no title/title_source keys in the
// JSON at all) decodes with both zero-valued — TitleSourceUnset — with
// no migration.
func TestRegistry_SetTitle_OldBlob_AdditiveRoundTrip(t *testing.T) {
	t.Parallel()
	_, store, _ := titleTestWiring(t)
	id := ident("t1", "u1", "s1")

	// A pre-Phase-157 session.lifecycle blob: no "Title" / "TitleSource"
	// keys at all (captured shape, not round-tripped through the new
	// struct).
	oldBlob := fmt.Sprintf(`{
		"ID": %q,
		"Identity": {"TenantID": %q, "UserID": %q, "SessionID": %q},
		"OpenedAt": "2026-01-01T00:00:00Z",
		"LastSeen": "2026-01-01T00:00:00Z",
		"Closed": false,
		"ClosedAt": "0001-01-01T00:00:00Z",
		"ClosedReason": "",
		"Limits": {},
		"Context": {}
	}`, id.SessionID, id.TenantID, id.UserID, id.SessionID)

	if err := store.Save(context.Background(), state.StateRecord{
		ID:       state.NewEventID(),
		Identity: identity.Quadruple{Identity: id},
		Kind:     "session.lifecycle",
		Bytes:    []byte(oldBlob),
	}); err != nil {
		t.Fatalf("Save old blob: %v", err)
	}

	var decoded sessions.Session
	rec, err := store.Load(context.Background(), identity.Quadruple{Identity: id}, "session.lifecycle")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := json.Unmarshal(rec.Bytes, &decoded); err != nil {
		t.Fatalf("Unmarshal old blob: %v", err)
	}
	if decoded.Title != "" || decoded.TitleSource != sessions.TitleSourceUnset {
		t.Fatalf("old-blob decode: Title=%q TitleSource=%q, want both zero-valued", decoded.Title, decoded.TitleSource)
	}
}

// --- Concurrent-reuse stress (D-025 extension) ---

// TestRegistry_ConcurrentReuse_SetTitle_NoBleed extends the registry's
// D-025 stress contract: N≥100 concurrent SetTitle + ListSnapshots +
// Touch calls against ONE shared Registry, asserting distinct sessions
// never bleed titles into one another.
func TestRegistry_ConcurrentReuse_SetTitle_NoBleed(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)

	const n = 120
	var wg sync.WaitGroup
	errCount := atomic.Int64{}
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := ident(fmt.Sprintf("t-%d", i%8), fmt.Sprintf("u-%d", i%8), fmt.Sprintf("s-%d", i))
			if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
				errCount.Add(1)
				t.Errorf("Open: %v", err)
				return
			}
			title := fmt.Sprintf("title-%d", i)
			if err := reg.SetTitle(ctxFor(id), id.SessionID, id, title); err != nil {
				errCount.Add(1)
				t.Errorf("SetTitle: %v", err)
				return
			}
			if err := reg.Touch(ctxFor(id), id.SessionID); err != nil {
				errCount.Add(1)
				t.Errorf("Touch: %v", err)
				return
			}
			snaps, err := reg.ListSnapshots(ctxFor(id), sessions.SessionListFilter{
				TenantIDs: []string{id.TenantID}, UserIDs: []string{id.UserID}, IncludeClosed: true,
			})
			if err != nil {
				errCount.Add(1)
				t.Errorf("ListSnapshots: %v", err)
				return
			}
			for _, s := range snaps {
				if s.ID != id.SessionID {
					continue
				}
				if s.Title != title {
					errCount.Add(1)
					t.Errorf("title bleed: session %q has title %q, want %q", s.ID, s.Title, title)
				}
			}
		}(i)
	}
	wg.Wait()
	if errCount.Load() != 0 {
		t.Fatalf("concurrent SetTitle stress observed %d errors", errCount.Load())
	}
}

// TestRegistry_ConcurrentReuse_SetTitle_SameSession_Serializes fires
// N≥100 concurrent SetTitle calls at ONE session and asserts the
// result is always one of the attempted titles (last-write-wins) —
// never a torn/corrupt record (each writer marshals+saves the FULL
// Session struct, so no interleaving can produce a mixed record).
func TestRegistry_ConcurrentReuse_SetTitle_SameSession_Serializes(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}

	const n = 150
	valid := make(map[string]struct{}, n)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range n {
		title := fmt.Sprintf("concurrent-title-%d", i)
		mu.Lock()
		valid[title] = struct{}{}
		mu.Unlock()
		wg.Add(1)
		go func(title string) {
			defer wg.Done()
			if err := reg.SetTitle(ctxFor(id), id.SessionID, id, title); err != nil {
				t.Errorf("SetTitle: %v", err)
			}
		}(title)
	}
	wg.Wait()

	snap, err := reg.Inspect(ctxFor(id), id.SessionID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if snap.TitleSource != sessions.TitleSourceManual {
		t.Fatalf("TitleSource = %q, want manual (torn record?)", snap.TitleSource)
	}
	mu.Lock()
	_, ok := valid[snap.Title]
	mu.Unlock()
	if !ok {
		t.Fatalf("final title %q is not one of the %d attempted titles (torn record)", snap.Title, n)
	}
}
