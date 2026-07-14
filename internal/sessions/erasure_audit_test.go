package sessions_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
)

// erasureLedgerTestKindPrefix mirrors the unexported
// sessions.erasureLedgerKindPrefix's literal value, the same way
// erasureAuditObservabilitySession mirrors erasureAuditSession above —
// tests in this external package pin the literal so a drift in the
// production constant fails these tests loudly instead of silently
// matching nothing.
const erasureLedgerTestKindPrefix = "session.erasure.pending."

// ledgerScopeForTest reconstructs the StateStore identity the erasure
// ledger checkpoint lives under, mirroring the unexported
// sessions.ledgerScope helper.
func ledgerScopeForTest(id identity.Identity) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{
		TenantID:  id.TenantID,
		UserID:    id.UserID,
		SessionID: erasureAuditObservabilitySession,
	}}
}

// ---------------------------------------------------------------------------
// D-286 / #409 / #410 — durable record-of-fact ordering + cumulative counts.
// ---------------------------------------------------------------------------

// erasureAuditObservabilitySession mirrors the unexported
// sessions.erasureAuditSession sentinel's literal value: the reserved
// session slot the ledger checkpoint and the final `session.erased`
// event both ride under (the actor's (tenant, user) with this session).
// Tests in this external test package cannot reference the unexported
// constant directly, so the literal is pinned here; a rename over there
// fails this test loudly rather than silently reading the wrong scope.
const erasureAuditObservabilitySession = "<erasure-audit>"

// flakyMemory wraps a real MemoryStore and fails Flush while its toggle
// is set, forcing a mid-cascade interruption immediately AFTER the
// artifacts step has already durably checkpointed its count — the #410
// fault-injection seam.
type flakyMemory struct {
	memory.MemoryStore
	fail *atomic.Bool
}

func (m *flakyMemory) Flush(ctx context.Context, id identity.Quadruple) error {
	if m.fail.Load() {
		return errors.New("flaky memory store: forced flush failure")
	}
	return m.MemoryStore.Flush(ctx, id)
}

// TestCascadeEraser_LedgerCumulative_InterruptedAfterArtifacts_ReinvokeSumsCounts
// pins #410: a cascade interrupted AFTER the artifacts step (memory.Flush
// fails) durably checkpoints the artifacts count before failing; a
// re-invoke that converges reports the TRUE cumulative total (the first
// attempt's artifacts contribution + the second attempt's memory/state
// contribution) rather than only the second (converging) attempt's own,
// now-zero, artifacts count.
func TestCascadeEraser_LedgerCumulative_InterruptedAfterArtifacts_ReinvokeSumsCounts(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t-410", "u-410", "s-410")
	ictx := ctxFor(id)

	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := f.mem.AddTurn(ictx, identity.Quadruple{Identity: id}, memory.ConversationTurn{
		UserMessage: "hello", AssistantResponse: "world",
	}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	const wantArtifacts = 3
	for i := range wantArtifacts {
		// Distinct content per artifact: the inmem driver content-addresses
		// by sha256(data) within a namespace, so identical bytes would
		// dedupe to a single ref instead of seeding wantArtifacts distinct
		// ones.
		blob := []byte(fmt.Sprintf("blob-%d", i))
		if _, err := f.arts.PutBytes(ctx, scope, blob, artifacts.PutOpts{Namespace: "test"}); err != nil {
			t.Fatalf("PutBytes %d: %v", i, err)
		}
	}

	var fail atomic.Bool
	fail.Store(true)
	flakyMem := &flakyMemory{MemoryStore: f.mem, fail: &fail}
	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: f.reg, State: f.store, Memory: flakyMem, Artifacts: f.arts, Bus: f.bus,
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}

	// First attempt: fails at the memory step, AFTER the artifacts step
	// already ran (and durably checkpointed its count).
	if _, derr := eraser.Erase(ctx, id); derr == nil {
		t.Fatal("interrupted cascade did not surface loudly")
	}
	// Artifacts are already gone (the destructive step ran); the session
	// record survives (DeleteScope never reached).
	refs, lerr := f.arts.List(ctx, scope)
	if lerr != nil {
		t.Fatalf("List after first attempt: %v", lerr)
	}
	if len(refs) != 0 {
		t.Errorf("artifacts survived the first attempt: %d, want 0", len(refs))
	}
	if _, lerr := f.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); lerr != nil {
		t.Fatalf("first attempt deleted the session record: %v", lerr)
	}

	// Clear the fault and re-invoke: the second attempt's OWN artifacts
	// count is 0 (already deleted), but the response must report the
	// TRUE cumulative total from the first attempt's checkpointed count.
	fail.Store(false)
	resp, rerr := eraser.Erase(ctx, id)
	if rerr != nil {
		t.Fatalf("retry after fault cleared failed: %v", rerr)
	}
	if !resp.Deleted {
		t.Fatalf("retry response = %+v, want Deleted", resp)
	}
	if resp.ArtifactsDeleted != wantArtifacts {
		t.Errorf("ArtifactsDeleted = %d, want %d (cumulative across attempts, not just the converging attempt's own 0)", resp.ArtifactsDeleted, wantArtifacts)
	}
	if !resp.MemoryPurged {
		t.Error("MemoryPurged = false, want true")
	}
	if resp.StateRecordsDeleted <= 0 {
		t.Errorf("StateRecordsDeleted = %d, want > 0", resp.StateRecordsDeleted)
	}
	if _, lerr := f.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); !errors.Is(lerr, state.ErrNotFound) {
		t.Errorf("session.lifecycle survived the retry: err=%v", lerr)
	}
}

// flakyPublishBus wraps a real Fencer-capable bus and forces Publish to
// fail while its toggle is set — the #409 fault-injection seam for the
// FINAL record-of-fact emit. Delegation only: Subscribe/Close promote via
// the embedded EventBus; Fence/Unfence delegate explicitly to the
// captured real Fencer (mirroring erasure_fence_test.go's
// flakyFencerBus) so the fence step keeps working normally while only
// Publish is forced to fail.
type flakyPublishBus struct {
	events.EventBus
	fencer events.Fencer
	fail   *atomic.Bool
}

func (b *flakyPublishBus) Publish(ctx context.Context, ev events.Event) error {
	if b.fail.Load() {
		return errors.New("flaky bus: forced publish failure")
	}
	return b.EventBus.Publish(ctx, ev)
}

func (b *flakyPublishBus) Fence(ctx context.Context, id identity.Identity) error {
	return b.fencer.Fence(ctx, id)
}

func (b *flakyPublishBus) Unfence(ctx context.Context, id identity.Identity) error {
	return b.fencer.Unfence(ctx, id)
}

// countSessionErasedEvents scans the actor's observability scope for
// `session.erased` events whose payload SessionID equals sessionID,
// returning the count and the last matching event's Identity (for the
// identity-propagation assertion). Enumerates durable heads
// maintenance-scoped rather than hardcoding the reserved session slot's
// literal directly against the bus, so the assertion still exercises the
// real read surface (HistoryReplayer.Window) the same way an operator's
// tooling would.
func countSessionErasedEvents(t *testing.T, bus events.EventBus, tenant, user, sessionID string) (int, identity.Quadruple) {
	t.Helper()
	hr, ok := bus.(events.HistoryReplayer)
	if !ok {
		t.Fatalf("bus must implement events.HistoryReplayer")
	}
	filter := events.Filter{
		Tenant:  tenant,
		User:    user,
		Session: erasureAuditObservabilitySession,
		Types:   []events.EventType{sessions.EventTypeSessionErased},
	}
	evs, err := hr.Window(context.Background(), 0, 1000, filter)
	if err != nil {
		t.Fatalf("Window: %v", err)
	}
	count := 0
	var lastIdentity identity.Quadruple
	for _, ev := range evs {
		// The durable driver rehydrates every replayed payload as a
		// generic events.RedactedMap (internal/events/drivers/durable
		// record.go) — the concrete SessionErasedPayload type is never
		// reconstructed on replay, by design (persistedEvent's doc
		// comment). Read the SessionID field out of the map instead.
		redacted, ok := ev.Payload.(events.RedactedMap)
		if !ok {
			continue
		}
		got, _ := redacted.Data["SessionID"].(string)
		if got == sessionID {
			count++
			lastIdentity = ev.Identity
		}
	}
	return count, lastIdentity
}

// TestCascadeEraser_FinalEmitPublishFailure_FailsLoud_ReinvokeConverges
// pins #409's core fix: the destructive steps complete (data is
// irrevocably gone), the final `session.erased` publish fails — Erase
// MUST fail the whole call loud (ErrErasureRecordFailed), never report
// success with a missing audit trail. A re-invoke after the transient
// bus fault clears converges: it re-attempts ONLY the record-of-fact
// (no destructive step re-runs) and succeeds, leaving EXACTLY ONE
// durable `session.erased` record — never zero (the bug this closes),
// never two (the re-invoke must not double-emit).
func TestCascadeEraser_FinalEmitPublishFailure_FailsLoud_ReinvokeConverges(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t-409", "u-409", "s-409")
	ictx := ctxFor(id)

	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := f.mem.AddTurn(ictx, identity.Quadruple{Identity: id}, memory.ConversationTurn{
		UserMessage: "hello", AssistantResponse: "world",
	}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	if _, err := f.arts.PutBytes(ctx, scope, []byte("blob"), artifacts.PutOpts{Namespace: "test"}); err != nil {
		t.Fatalf("PutBytes: %v", err)
	}

	realFencer, ok := f.bus.(events.Fencer)
	if !ok {
		t.Fatalf("fixture bus must implement events.Fencer")
	}
	var fail atomic.Bool
	fail.Store(true)
	flaky := &flakyPublishBus{EventBus: f.bus, fencer: realFencer, fail: &fail}

	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: f.reg, State: f.store, Memory: f.mem, Artifacts: f.arts, Bus: flaky,
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}

	// First attempt: the destructive steps all succeed, but the final
	// publish fails → the WHOLE call fails loud with the typed sentinel.
	_, ferr := eraser.Erase(ctx, id)
	if ferr == nil {
		t.Fatal("forced publish failure did not surface loudly")
	}
	if !errors.Is(ferr, sessions.ErrErasureRecordFailed) {
		t.Errorf("err = %v, want errors.Is(err, ErrErasureRecordFailed)", ferr)
	}
	// The data really is gone already (D-286's "success requires a
	// record" invariant is about SUCCESS, not about the destructive
	// steps — those already ran).
	refs, lerr := f.arts.List(ctx, scope)
	if lerr != nil {
		t.Fatalf("List after failed emit: %v", lerr)
	}
	if len(refs) != 0 {
		t.Errorf("artifacts survived: %d, want 0 (the destructive steps already completed)", len(refs))
	}
	if _, lerr := f.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); !errors.Is(lerr, state.ErrNotFound) {
		t.Errorf("session.lifecycle survived: err=%v, want ErrNotFound (DeleteScope already ran)", lerr)
	}
	// No durable record exists yet — the publish failed.
	if count, _ := countSessionErasedEvents(t, f.bus, id.TenantID, id.UserID, id.SessionID); count != 0 {
		t.Errorf("session.erased count after failed emit = %d, want 0", count)
	}

	// Clear the transient bus fault and re-invoke: converges via the
	// ledger checkpoint — no destructive step re-runs (there is nothing
	// left to delete), only the record-of-fact completes.
	fail.Store(false)
	resp, rerr := eraser.Erase(ctx, id)
	if rerr != nil {
		t.Fatalf("retry after the bus recovers failed: %v", rerr)
	}
	if !resp.Deleted || resp.ArtifactsDeleted != 1 || !resp.MemoryPurged || resp.StateRecordsDeleted <= 0 {
		t.Errorf("converged response = %+v, want Deleted+ArtifactsDeleted=1+MemoryPurged+StateRecordsDeleted>0", resp)
	}

	// EXACTLY ONE durable session.erased record — not lost, not doubled.
	count, gotIdentity := countSessionErasedEvents(t, f.bus, id.TenantID, id.UserID, id.SessionID)
	if count != 1 {
		t.Fatalf("session.erased count after convergence = %d, want exactly 1", count)
	}
	// Identity propagation: the record's (tenant, user) match the actor's
	// verified identity (the session component is deliberately the
	// reserved observability slot, not the erased session — documented
	// design, asserted by countSessionErasedEvents' own filter).
	if gotIdentity.TenantID != id.TenantID || gotIdentity.UserID != id.UserID {
		t.Errorf("session.erased Identity = %+v, want TenantID=%q UserID=%q", gotIdentity, id.TenantID, id.UserID)
	}
}

// flakyRedactor wraps a real audit.Redactor and forces Redact to fail
// while its toggle is set — the OTHER #409 fault-injection seam
// (redactor refusal must take the same loud path as a bus failure, not
// an `Error`-log-and-continue).
type flakyRedactor struct {
	inner audit.Redactor
	fail  *atomic.Bool
}

func (r *flakyRedactor) Redact(ctx context.Context, payload any) (any, error) {
	if r.fail.Load() {
		return nil, errors.New("flaky redactor: forced refusal")
	}
	return r.inner.Redact(ctx, payload)
}

// TestCascadeEraser_RedactorRefusal_FailsLoud_ReinvokeConverges pins the
// other half of #409: a redactor refusal at the final emit follows the
// SAME loud path as a bus-publish failure (no `Error`-log-and-continue) —
// Erase fails with ErrErasureRecordFailed, the destructive steps already
// completed, and a re-invoke after the redactor heals converges with
// exactly one durable record.
func TestCascadeEraser_RedactorRefusal_FailsLoud_ReinvokeConverges(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t-409r", "u-409r", "s-409r")
	ictx := ctxFor(id)

	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	if _, err := f.arts.PutBytes(ctx, scope, []byte("blob"), artifacts.PutOpts{Namespace: "test"}); err != nil {
		t.Fatalf("PutBytes: %v", err)
	}

	var fail atomic.Bool
	fail.Store(true)
	flaky := &flakyRedactor{inner: passthroughRedactor{}, fail: &fail}
	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: f.reg, State: f.store, Memory: f.mem, Artifacts: f.arts, Bus: f.bus, Redactor: flaky,
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}

	_, ferr := eraser.Erase(ctx, id)
	if ferr == nil {
		t.Fatal("forced redactor refusal did not surface loudly")
	}
	if !errors.Is(ferr, sessions.ErrErasureRecordFailed) {
		t.Errorf("err = %v, want errors.Is(err, ErrErasureRecordFailed)", ferr)
	}
	if _, lerr := f.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); !errors.Is(lerr, state.ErrNotFound) {
		t.Errorf("session.lifecycle survived: err=%v, want ErrNotFound", lerr)
	}

	fail.Store(false)
	resp, rerr := eraser.Erase(ctx, id)
	if rerr != nil {
		t.Fatalf("retry after the redactor heals failed: %v", rerr)
	}
	if !resp.Deleted || resp.ArtifactsDeleted != 1 {
		t.Errorf("converged response = %+v, want Deleted+ArtifactsDeleted=1", resp)
	}
	if count, _ := countSessionErasedEvents(t, f.bus, id.TenantID, id.UserID, id.SessionID); count != 1 {
		t.Errorf("session.erased count after convergence = %d, want exactly 1", count)
	}
}

// passthroughRedactor is a trivial audit.Redactor stand-in: it returns
// the payload unchanged. The SessionErasedPayload under test is already
// content-free (bounded ids/counts/timestamp), so no real redaction
// rule is exercised by this seam-focused test.
type passthroughRedactor struct{}

func (passthroughRedactor) Redact(_ context.Context, payload any) (any, error) { return payload, nil }

// TestCascadeEraser_ConcurrentSameSession_OneWins_NeverDoubleEvent pins
// the concurrency contract for two callers racing `sessions.delete` on
// the SAME session: the striped per-session lock (lockSession) ensures
// exactly one goroutine ever runs the cascade for this session, so
// EXACTLY one call succeeds (Deleted: true) and the other observes the
// genuine ErrSessionNotFound (the session and its ledger are already
// gone by the time it acquires the lock) — never a double event, never
// both failing. Run under -race (the package gate).
func TestCascadeEraser_ConcurrentSameSession_OneWins_NeverDoubleEvent(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t-race", "u-race", "s-race")
	ictx := ctxFor(id)

	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	if _, err := f.arts.PutBytes(ctx, scope, []byte("blob"), artifacts.PutOpts{Namespace: "test"}); err != nil {
		t.Fatalf("PutBytes: %v", err)
	}

	type outcome struct {
		deleted bool
		err     error
	}
	results := make([]outcome, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	for i := range 2 {
		go func(i int) {
			defer wg.Done()
			resp, err := f.eraser.Erase(ctx, id)
			results[i] = outcome{deleted: resp.Deleted, err: err}
		}(i)
	}
	wg.Wait()

	successes, notFounds := 0, 0
	for _, r := range results {
		switch {
		case r.err == nil && r.deleted:
			successes++
		case errors.Is(r.err, sessions.ErrSessionNotFound):
			notFounds++
		default:
			t.Errorf("unexpected outcome: deleted=%v err=%v", r.deleted, r.err)
		}
	}
	if successes != 1 {
		t.Errorf("successes = %d, want exactly 1", successes)
	}
	if notFounds != 1 {
		t.Errorf("not-found outcomes = %d, want exactly 1", notFounds)
	}

	count, _ := countSessionErasedEvents(t, f.bus, id.TenantID, id.UserID, id.SessionID)
	if count != 1 {
		t.Errorf("session.erased count = %d, want exactly 1 (never a double event)", count)
	}
}

// TestCascadeEraser_StaleLedger_SessionIDReuse_CountsOnlyNewLifecycle is
// the adversarial-review repro for stale-ledger count inflation across
// session-id reuse: lifecycle 1 completes its destructive steps but
// fails at the final emit (ErrErasureRecordFailed, never converged — 5
// artifacts checkpointed in the ledger); the SAME session id is then
// reopened as a brand-new lifecycle with 2 artifacts and erased cleanly.
// Without the lifecycle stamp the leftover ledger would accumulate and
// report ArtifactsDeleted=7; with it, the stale checkpoint is discarded
// and the clean erasure reports exactly lifecycle 2's own count.
// TestCascadeEraser_FailedErase_ReopenBlocked_ReinvokeConverges pins the
// D-312 interaction with the erasure ledger: after an erasure whose
// destructive steps succeeded but whose record-of-fact emit FAILED (a pending
// ledger survives, NO tombstone yet), the ONLY correct recovery is to RE-INVOKE
// Erase (convergence) — a reopen of the same id is TERMINAL (the pending ledger
// makes isErased fire → ErrReopenAfterErase), NEVER a fresh reused lifecycle.
// The converging re-invoke completes from the ledger's cumulative counts and
// then writes the terminal tombstone. (Pre-D-312 this scenario reopened the id
// as a brand-new lifecycle; the amended §6.9 forbids reusing an erased id.)
func TestCascadeEraser_FailedErase_ReopenBlocked_ReinvokeConverges(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t-reuse2", "u-reuse2", "s-reuse2")
	ictx := ctxFor(id)

	// --- 5 artifacts, destructive steps succeed, the final emit fails ---
	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	for i := range 5 {
		if _, err := f.arts.PutBytes(ctx, scope, []byte(fmt.Sprintf("l1-blob-%d", i)), artifacts.PutOpts{Namespace: "test"}); err != nil {
			t.Fatalf("PutBytes %d: %v", i, err)
		}
	}
	realFencer, ok := f.bus.(events.Fencer)
	if !ok {
		t.Fatalf("fixture bus must implement events.Fencer")
	}
	var fail atomic.Bool
	fail.Store(true)
	flaky := &flakyPublishBus{EventBus: f.bus, fencer: realFencer, fail: &fail}
	failEraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: f.reg, State: f.store, Memory: f.mem, Artifacts: f.arts, Bus: flaky,
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}
	if _, ferr := failEraser.Erase(ctx, id); !errors.Is(ferr, sessions.ErrErasureRecordFailed) {
		t.Fatalf("erase err=%v, want ErrErasureRecordFailed", ferr)
	}

	// Reopen the SAME id is TERMINAL — the pending ledger (in-flight /
	// interrupted erasure) makes isErased fire even though the record is gone.
	if _, oerr := f.reg.Open(ictx, id.SessionID, id); !errors.Is(oerr, sessions.ErrReopenAfterErase) {
		t.Fatalf("reopen after failed erase = %v, want ErrReopenAfterErase", oerr)
	}

	// The correct recovery: RE-INVOKE Erase (fault cleared) → converges from
	// the ledger's cumulative counts (5 artifacts), writes the tombstone.
	resp, rerr := f.eraser.Erase(ctx, id)
	if rerr != nil {
		t.Fatalf("converging re-invoke: %v", rerr)
	}
	if resp.ArtifactsDeleted != 5 {
		t.Errorf("ArtifactsDeleted = %d, want 5 (cumulative from the checkpointed ledger, not the converging attempt's own 0)", resp.ArtifactsDeleted)
	}
	if !resp.Deleted || !resp.MemoryPurged {
		t.Errorf("converged response = %+v, want Deleted && MemoryPurged", resp)
	}
	// Exactly one durable record-of-fact for the erased lifecycle.
	if count, _ := countSessionErasedEvents(t, f.bus, id.TenantID, id.UserID, id.SessionID); count != 1 {
		t.Errorf("session.erased count = %d, want exactly 1", count)
	}
	// The pending ledger slot is gone (deleted on the clean convergence).
	if _, lerr := f.store.Load(ctx, ledgerScopeForTest(id), erasureLedgerTestKindPrefix+id.SessionID); !errors.Is(lerr, state.ErrNotFound) {
		t.Errorf("ledger checkpoint survived the converged erasure: err=%v", lerr)
	}
	// And the id stays terminal: reopen still fails loud (now via the tombstone).
	if _, oerr := f.reg.Open(ictx, id.SessionID, id); !errors.Is(oerr, sessions.ErrReopenAfterErase) {
		t.Fatalf("reopen after converged erase = %v, want ErrReopenAfterErase", oerr)
	}
}

// ---------------------------------------------------------------------------
// Ledger-checkpoint fault injection: the StateStore seam the erasure
// ledger itself is persisted through.
// ---------------------------------------------------------------------------

// flakyLedgerStore wraps a real StateStore and selectively fails
// Save/Load/Delete for erasure-ledger-kind keys (or DeleteScope
// entirely) while the matching toggle is set — the seam for exercising
// the erasure ledger's OWN persistence failure modes, distinct from a
// failure in the destructive stores (artifacts/memory) already covered
// elsewhere.
type flakyLedgerStore struct {
	state.StateStore
	saveFail        *atomic.Bool
	loadFail        *atomic.Bool
	deleteFail      *atomic.Bool
	deleteScopeFail *atomic.Bool
	// saveSkip, when non-nil, lets the first saveSkip ledger-kind Saves
	// through before the saveFail toggle takes effect — used to target a
	// LATER ledger checkpoint (e.g. the artifacts-step checkpoint) while
	// letting the erase-INTENT checkpoint (the first ledger Save) land, so a
	// test can exercise the artifacts-checkpoint residual gap specifically.
	saveSkip *atomic.Int32
}

func (s *flakyLedgerStore) Save(ctx context.Context, r state.StateRecord) error {
	if s.saveFail != nil && s.saveFail.Load() && strings.HasPrefix(r.Kind, erasureLedgerTestKindPrefix) {
		if s.saveSkip != nil && s.saveSkip.Add(-1) >= 0 {
			return s.StateStore.Save(ctx, r)
		}
		return errors.New("flaky state store: forced ledger save failure")
	}
	return s.StateStore.Save(ctx, r)
}

func (s *flakyLedgerStore) Load(ctx context.Context, id identity.Quadruple, kind string) (state.StateRecord, error) {
	if s.loadFail != nil && s.loadFail.Load() && strings.HasPrefix(kind, erasureLedgerTestKindPrefix) {
		return state.StateRecord{}, errors.New("flaky state store: forced ledger load failure")
	}
	return s.StateStore.Load(ctx, id, kind)
}

func (s *flakyLedgerStore) Delete(ctx context.Context, id identity.Quadruple, kind string) error {
	if s.deleteFail != nil && s.deleteFail.Load() && strings.HasPrefix(kind, erasureLedgerTestKindPrefix) {
		return errors.New("flaky state store: forced ledger delete failure")
	}
	return s.StateStore.Delete(ctx, id, kind)
}

func (s *flakyLedgerStore) DeleteScope(ctx context.Context, id identity.Identity) (int, error) {
	if s.deleteScopeFail != nil && s.deleteScopeFail.Load() {
		return 0, errors.New("flaky state store: forced DeleteScope failure")
	}
	return s.StateStore.DeleteScope(ctx, id)
}

// TestCascadeEraser_LedgerLoadFailure_FailsLoud_TouchesNothing pins the
// very first fail-loud gate: if the ledger LOAD itself errors (a real
// store failure, not ErrNotFound), Erase fails immediately, before the
// refuse-if-running pre-flight ever runs — no store is touched.
func TestCascadeEraser_LedgerLoadFailure_FailsLoud_TouchesNothing(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t-ledgerload", "u-ledgerload", "s-ledgerload")
	ictx := ctxFor(id)
	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	if _, err := f.arts.PutBytes(ctx, scope, []byte("blob"), artifacts.PutOpts{Namespace: "test"}); err != nil {
		t.Fatalf("PutBytes: %v", err)
	}

	var fail atomic.Bool
	fail.Store(true)
	flaky := &flakyLedgerStore{StateStore: f.store, loadFail: &fail}
	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: f.reg, State: flaky, Memory: f.mem, Artifacts: f.arts, Bus: f.bus,
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}

	if _, derr := eraser.Erase(ctx, id); derr == nil {
		t.Fatal("forced ledger load failure did not surface loudly")
	}
	if _, lerr := f.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); lerr != nil {
		t.Errorf("ledger load failure touched the session record: %v", lerr)
	}
	refs, lerr := f.arts.List(ctx, scope)
	if lerr != nil {
		t.Fatalf("List: %v", lerr)
	}
	if len(refs) != 1 {
		t.Errorf("ledger load failure touched artifacts: %d, want 1", len(refs))
	}

	fail.Store(false)
	resp, rerr := eraser.Erase(ctx, id)
	if rerr != nil {
		t.Fatalf("retry after fault cleared failed: %v", rerr)
	}
	if !resp.Deleted {
		t.Errorf("retry response = %+v, want Deleted", resp)
	}
}

// TestCascadeEraser_LedgerSaveFailure_LoudAndRetrySafe pins a checkpoint
// SAVE failure (right after the artifacts step): the destructive
// artifacts delete already ran, but the checkpoint write failed, so
// Erase fails loud before memory/state ever run. This is a DOCUMENTED
// RESIDUAL GAP (see erasure.go's package godoc + CascadeEraser.Erase):
// because there is no ACID transaction spanning the artifact store and
// the StateStore (D-262/D-286's explicit non-goal — no generalized
// transactional-outbox subsystem), a failure in the narrow window
// between a destructive delete succeeding and its OWN checkpoint save
// committing is not covered by the cumulative-counts guarantee — the
// checkpoint never durably recorded that attempt's contribution, so a
// converging retry's own (now zero, since already deleted) count is all
// that gets reported. This test PINS that narrow, accepted gap
// explicitly (rather than leaving it silently untested) and asserts the
// stronger guarantees that DO hold regardless: the failure is loud, no
// LATER step runs, and the retry still converges to Deleted:true.
func TestCascadeEraser_LedgerSaveFailure_LoudAndRetrySafe(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t-ledgersave", "u-ledgersave", "s-ledgersave")
	ictx := ctxFor(id)
	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	if _, err := f.arts.PutBytes(ctx, scope, []byte("blob"), artifacts.PutOpts{Namespace: "test"}); err != nil {
		t.Fatalf("PutBytes: %v", err)
	}

	var fail atomic.Bool
	fail.Store(true)
	// saveSkip=1 lets the erase-INTENT checkpoint (the first ledger Save, added
	// to close the fence-lift race) land, and fails the NEXT ledger checkpoint
	// (the artifacts step) — targeting the documented artifacts-checkpoint
	// residual gap specifically, exactly as before the intent checkpoint existed.
	var skip atomic.Int32
	skip.Store(1)
	flaky := &flakyLedgerStore{StateStore: f.store, saveFail: &fail, saveSkip: &skip}
	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: f.reg, State: flaky, Memory: f.mem, Artifacts: f.arts, Bus: f.bus,
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}

	if _, derr := eraser.Erase(ctx, id); derr == nil {
		t.Fatal("forced ledger save failure did not surface loudly")
	}
	// The artifact delete already ran (irrevocable); the session record
	// survives (DeleteScope never reached).
	refs, lerr := f.arts.List(ctx, scope)
	if lerr != nil {
		t.Fatalf("List: %v", lerr)
	}
	if len(refs) != 0 {
		t.Errorf("artifacts survived: %d, want 0", len(refs))
	}
	if _, lerr := f.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); lerr != nil {
		t.Errorf("ledger save failure deleted the session record: %v", lerr)
	}

	fail.Store(false)
	resp, rerr := eraser.Erase(ctx, id)
	if rerr != nil {
		t.Fatalf("retry after fault cleared failed: %v", rerr)
	}
	// ArtifactsDeleted is 0 here — the documented residual gap above, NOT
	// a regression of #410's guarantee (which covers a failure at the
	// NEXT cascade step, once a checkpoint has already durably landed;
	// see TestCascadeEraser_LedgerCumulative_InterruptedAfterArtifacts_ReinvokeSumsCounts).
	if !resp.Deleted {
		t.Errorf("retry response = %+v, want Deleted", resp)
	}
}

// TestCascadeEraser_CorruptLedger_FailsLoud pins the ledger-integrity
// gate: a checkpoint record that fails to unmarshal (corrupt bytes,
// never expected in production but defence-in-depth) fails Erase loud
// rather than silently treating it as absent (which would under-report
// cumulative counts) or panicking.
func TestCascadeEraser_CorruptLedger_FailsLoud(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t-corrupt", "u-corrupt", "s-corrupt")
	ictx := ctxFor(id)
	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := f.store.Save(ctx, state.StateRecord{
		ID:       state.NewEventID(),
		Identity: ledgerScopeForTest(id),
		Kind:     erasureLedgerTestKindPrefix + id.SessionID,
		Bytes:    []byte("not valid json"),
	}); err != nil {
		t.Fatalf("seed corrupt ledger: %v", err)
	}

	if _, derr := f.eraser.Erase(ctx, id); derr == nil {
		t.Fatal("corrupt ledger did not surface loudly")
	}
	// Nothing touched — the corrupt-ledger check runs before any
	// destructive step.
	if _, lerr := f.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); lerr != nil {
		t.Errorf("corrupt-ledger failure touched the session record: %v", lerr)
	}
}

// TestCascadeEraser_DeleteScopeFailure_LoudAndRetrySafe pins a failure at
// the irreversible-clear step itself: artifacts + memory already
// completed (and checkpointed), state.DeleteScope errors, Erase fails
// loud, and a retry converges with the cumulative counts intact.
func TestCascadeEraser_DeleteScopeFailure_LoudAndRetrySafe(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t-statefail", "u-statefail", "s-statefail")
	ictx := ctxFor(id)
	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := f.mem.AddTurn(ictx, identity.Quadruple{Identity: id}, memory.ConversationTurn{
		UserMessage: "hello", AssistantResponse: "world",
	}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	if _, err := f.arts.PutBytes(ctx, scope, []byte("blob"), artifacts.PutOpts{Namespace: "test"}); err != nil {
		t.Fatalf("PutBytes: %v", err)
	}

	var fail atomic.Bool
	fail.Store(true)
	flaky := &flakyLedgerStore{StateStore: f.store, deleteScopeFail: &fail}
	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: f.reg, State: flaky, Memory: f.mem, Artifacts: f.arts, Bus: f.bus,
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}

	if _, derr := eraser.Erase(ctx, id); derr == nil {
		t.Fatal("forced DeleteScope failure did not surface loudly")
	}
	// Artifacts + memory already gone; the session record survives
	// (DeleteScope itself failed, so it never removed session.lifecycle).
	if _, lerr := f.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); lerr != nil {
		t.Errorf("DeleteScope failure removed the session record before DeleteScope itself succeeded: %v", lerr)
	}

	fail.Store(false)
	resp, rerr := eraser.Erase(ctx, id)
	if rerr != nil {
		t.Fatalf("retry after fault cleared failed: %v", rerr)
	}
	if !resp.Deleted || resp.ArtifactsDeleted != 1 || !resp.MemoryPurged || resp.StateRecordsDeleted <= 0 {
		t.Errorf("retry response = %+v, want Deleted+ArtifactsDeleted=1+MemoryPurged+StateRecordsDeleted>0", resp)
	}
	if _, lerr := f.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); !errors.Is(lerr, state.ErrNotFound) {
		t.Errorf("session.lifecycle survived the retry: err=%v", lerr)
	}
}

// TestCascadeEraser_LedgerCleanupFailure_SucceedsWithWarn pins
// completeErasure's deliberate asymmetry: a failure removing the
// now-superfluous ledger checkpoint AFTER a successful record-of-fact
// publish does NOT fail an already-successful erasure (the invariant
// this cascade defends — a durable record before success — is already
// satisfied). The only residual effect is a stray durable ledger record.
func TestCascadeEraser_LedgerCleanupFailure_SucceedsWithWarn(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t-ledgerclean", "u-ledgerclean", "s-ledgerclean")
	ictx := ctxFor(id)
	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	if _, err := f.arts.PutBytes(ctx, scope, []byte("blob"), artifacts.PutOpts{Namespace: "test"}); err != nil {
		t.Fatalf("PutBytes: %v", err)
	}

	var fail atomic.Bool
	fail.Store(true)
	flaky := &flakyLedgerStore{StateStore: f.store, deleteFail: &fail}
	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: f.reg, State: flaky, Memory: f.mem, Artifacts: f.arts, Bus: f.bus,
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}

	resp, err := eraser.Erase(ctx, id)
	if err != nil {
		t.Fatalf("Erase with a failing ledger cleanup should still succeed: %v", err)
	}
	if !resp.Deleted || resp.ArtifactsDeleted != 1 {
		t.Errorf("response = %+v, want Deleted+ArtifactsDeleted=1", resp)
	}
	// The record-of-fact is durably published regardless.
	if count, _ := countSessionErasedEvents(t, f.bus, id.TenantID, id.UserID, id.SessionID); count != 1 {
		t.Errorf("session.erased count = %d, want exactly 1", count)
	}
	// The stray ledger record remains (the only residual effect).
	if _, lerr := f.store.Load(ctx, ledgerScopeForTest(id), erasureLedgerTestKindPrefix+id.SessionID); lerr != nil {
		t.Errorf("expected the stray ledger record to remain after a cleanup failure, Load err=%v", lerr)
	}
}

// TestCascadeEraser_ReinvokeAfterCleanupFailure_ExactlyOneEmit pins the
// idempotent re-emit guard: after a successful publish whose LEDGER
// CLEANUP failed (the benign-retry double-emit window), a re-invoke for
// the same session converges via the ledger, recognizes the existing
// session.erased record for this session+lifecycle in the observability
// history, SKIPS the publish, still removes the ledger, and returns
// success with the checkpointed counts — exactly ONE durable record
// across both calls, never two.
func TestCascadeEraser_ReinvokeAfterCleanupFailure_ExactlyOneEmit(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t-oneemit", "u-oneemit", "s-oneemit")
	ictx := ctxFor(id)
	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	if _, err := f.arts.PutBytes(ctx, scope, []byte("blob"), artifacts.PutOpts{Namespace: "test"}); err != nil {
		t.Fatalf("PutBytes: %v", err)
	}

	var fail atomic.Bool
	fail.Store(true)
	flaky := &flakyLedgerStore{StateStore: f.store, deleteFail: &fail}
	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: f.reg, State: flaky, Memory: f.mem, Artifacts: f.arts, Bus: f.bus,
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}

	// First call: full success (publish landed), ledger cleanup failed.
	first, err := eraser.Erase(ctx, id)
	if err != nil {
		t.Fatalf("first Erase: %v", err)
	}
	if !first.Deleted || first.ArtifactsDeleted != 1 {
		t.Fatalf("first response = %+v, want Deleted+ArtifactsDeleted=1", first)
	}

	// Heal the cleanup fault and re-invoke: the converging path must NOT
	// re-publish (the record already exists for this session+lifecycle).
	fail.Store(false)
	second, err := eraser.Erase(ctx, id)
	if err != nil {
		t.Fatalf("re-invoke after cleanup failure: %v", err)
	}
	if !second.Deleted || second.ArtifactsDeleted != first.ArtifactsDeleted ||
		second.StateRecordsDeleted != first.StateRecordsDeleted || !second.MemoryPurged {
		t.Errorf("re-invoke response = %+v, want convergence to the first call's telemetry %+v", second, first)
	}

	// EXACTLY one durable record across both calls.
	if count, _ := countSessionErasedEvents(t, f.bus, id.TenantID, id.UserID, id.SessionID); count != 1 {
		t.Errorf("session.erased count = %d, want exactly 1 (the re-invoke must not double-emit)", count)
	}
	// The ledger is now cleaned up.
	if _, lerr := f.store.Load(ctx, ledgerScopeForTest(id), erasureLedgerTestKindPrefix+id.SessionID); !errors.Is(lerr, state.ErrNotFound) {
		t.Errorf("ledger checkpoint survived the converged re-invoke: err=%v", lerr)
	}
}

// windowErrBus wraps a real Fencer+HistoryReplayer bus but forces the
// HistoryReplayer.Window scan to fail — the seam for the re-emit
// guard's cannot-verify branch: a scan failure must degrade in the SAFE
// direction (emit anyway; a duplicate record beats a lost one), never
// block the erasure.
type windowErrBus struct {
	events.EventBus
	fencer events.Fencer
	hr     events.HistoryReplayer
}

func (b *windowErrBus) Fence(ctx context.Context, id identity.Identity) error {
	return b.fencer.Fence(ctx, id)
}

func (b *windowErrBus) Unfence(ctx context.Context, id identity.Identity) error {
	return b.fencer.Unfence(ctx, id)
}

func (b *windowErrBus) Bounds(ctx context.Context, f events.Filter) (uint64, uint64, bool, error) {
	return b.hr.Bounds(ctx, f)
}

func (b *windowErrBus) Window(context.Context, uint64, int, events.Filter) ([]events.Event, error) {
	return nil, errors.New("window error bus: forced scan failure")
}

func (b *windowErrBus) ListWindow(ctx context.Context, q events.EventListQuery) (events.EventListPage, error) {
	return b.hr.ListWindow(ctx, q)
}

// TestCascadeEraser_ReemitGuardScanFailure_EmitsAnyway pins the re-emit
// guard's safe degradation: when the observability-history scan errors,
// the guard reports "not emitted" and completeErasure publishes — the
// erasure still succeeds with exactly one durable record.
func TestCascadeEraser_ReemitGuardScanFailure_EmitsAnyway(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t-scanfail", "u-scanfail", "s-scanfail")
	ictx := ctxFor(id)
	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}

	fencer, ok := f.bus.(events.Fencer)
	if !ok {
		t.Fatalf("fixture bus must implement events.Fencer")
	}
	hr, ok := f.bus.(events.HistoryReplayer)
	if !ok {
		t.Fatalf("fixture bus must implement events.HistoryReplayer")
	}
	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: f.reg, State: f.store, Memory: f.mem, Artifacts: f.arts,
		Bus: &windowErrBus{EventBus: f.bus, fencer: fencer, hr: hr},
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}

	resp, err := eraser.Erase(ctx, id)
	if err != nil {
		t.Fatalf("Erase with a failing re-emit-guard scan should still succeed: %v", err)
	}
	if !resp.Deleted {
		t.Errorf("response = %+v, want Deleted", resp)
	}
	if count, _ := countSessionErasedEvents(t, f.bus, id.TenantID, id.UserID, id.SessionID); count != 1 {
		t.Errorf("session.erased count = %d, want exactly 1", count)
	}
}
