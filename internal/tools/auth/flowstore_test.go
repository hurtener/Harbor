package auth

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/state"
)

func testPendingFlow(stateToken string) PendingFlowRecord {
	now := time.Now().UTC()
	return PendingFlowRecord{
		State: stateToken, Source: "source-a", BindingScope: ScopeUser,
		SubjectID: "user-a", Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a"},
		Verifier: "pkce-verifier-material", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
		PauseToken: pauseresume.Token("pause-token-material"), ClientID: "client-a",
		ClientSecret: "client-secret-material", TokenURL: "https://auth.example/token", RedirectURI: "https://harbor.example/callback",
	}
}

func testCompletedFlow(flow PendingFlowRecord) CompletedFlowRecord {
	return CompletedFlowRecord{
		State:            flow.State,
		TokenMarker:      flow.State,
		Source:           flow.Source,
		BindingScope:     flow.BindingScope,
		SubjectID:        flow.SubjectID,
		Identity:         flow.Identity,
		PauseToken:       flow.PauseToken,
		ExpectedDecision: pauseresume.DecisionResume,
		ExpiresAt:        flow.ExpiresAt,
	}
}

type completionSaveAckLostStore struct {
	state.StateStore
	failed atomic.Bool
}

func (s *completionSaveAckLostStore) Save(ctx context.Context, rec state.StateRecord) error {
	if err := s.StateStore.Save(ctx, rec); err != nil {
		return err
	}
	if strings.HasPrefix(rec.Kind, flowCompletedKindPrefix) && s.failed.CompareAndSwap(false, true) {
		return errors.New("injected completed marker acknowledgement loss")
	}
	return nil
}

func (s *completionSaveAckLostStore) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	if err := s.StateStore.SaveIf(ctx, expectations, next); err != nil {
		return err
	}
	if strings.HasPrefix(next.Kind, flowCompletedKindPrefix) && s.failed.CompareAndSwap(false, true) {
		return errors.New("injected completed marker acknowledgement loss")
	}
	return nil
}

func newFlowStoreFixture(t *testing.T) (state.StateStore, FlowStore, Sealer) {
	t.Helper()
	raw := mkStore(t)
	sealer, err := NewAESGCMSealer(fixedKEK(t))
	if err != nil {
		t.Fatal(err)
	}
	flows, err := NewFlowStore(raw, sealer)
	if err != nil {
		t.Fatal(err)
	}
	return raw, flows, sealer
}

func TestFlowStore_RestartLookup_SealsWholeEnvelopeAndRejectsCollision(t *testing.T) {
	raw, flows, sealer := newFlowStoreFixture(t)
	ctx := mkCtx(t, testPendingFlow("state-restart").Identity)
	flow := testPendingFlow("state-restart")
	if err := flows.Put(ctx, flow); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rec, err := raw.LoadByEventID(ctx, state.EventID(flow.State))
	if err != nil {
		t.Fatalf("LoadByEventID: %v", err)
	}
	for _, plaintext := range []string{flow.Verifier, flow.ClientSecret, string(flow.PauseToken), flow.Identity.UserID} {
		if bytes.Contains(rec.Bytes, []byte(plaintext)) {
			t.Fatalf("durable flow leaked plaintext %q: %q", plaintext, rec.Bytes)
		}
	}
	rebuilt, err := NewFlowStore(raw, sealer)
	if err != nil {
		t.Fatalf("NewFlowStore restart: %v", err)
	}
	got, ok, err := rebuilt.Get(ctx, flow.State)
	if err != nil || !ok || got.Verifier != flow.Verifier || got.PauseToken != flow.PauseToken {
		t.Fatalf("restart Get = (%+v,%v,%v)", got, ok, err)
	}

	other := testPendingFlow("state-collision")
	collision := state.StateRecord{ID: state.EventID(other.State), Identity: identity.Quadruple{Identity: other.Identity}, Kind: "other.kind", Bytes: []byte("not-a-flow")}
	if err := raw.Save(ctx, collision); err != nil {
		t.Fatalf("save collision: %v", err)
	}
	if _, _, err := rebuilt.Get(ctx, other.State); err == nil {
		t.Fatal("mismatched-kind EventID collision did not fail loud")
	}
}

func TestFlowStore_ConcurrentReconstructedClaim_ExactlyOneWinner(t *testing.T) {
	raw, first, sealer := newFlowStoreFixture(t)
	second, err := NewFlowStore(raw, sealer)
	if err != nil {
		t.Fatal(err)
	}
	flow := testPendingFlow("state-one-winner")
	ctx := mkCtx(t, flow.Identity)
	if err := first.Put(ctx, flow); err != nil {
		t.Fatal(err)
	}
	const n = 128
	var winners atomic.Int64
	var winnerMu sync.Mutex
	var winnerStore FlowStore
	var winnerClaim FlowClaim
	errCh := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			candidate := first
			if i%2 == 1 {
				candidate = second
			}
			_, claim, ok, err := candidate.Claim(ctx, flow.State)
			if err != nil {
				errCh <- err
				return
			}
			if ok {
				winners.Add(1)
				winnerMu.Lock()
				winnerStore, winnerClaim = candidate, claim
				winnerMu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("Claim: %v", err)
	}
	if got := winners.Load(); got != 1 {
		t.Fatalf("claim winners=%d want=1", got)
	}
	if err := winnerStore.Release(ctx, winnerClaim); err != nil {
		t.Fatalf("Release winner: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := first.Get(cancelled, flow.State); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Get err=%v", err)
	}
}

func TestFlowStore_CompletedMarker_SaveAckLossReconcilesAndSurvivesFinish(t *testing.T) {
	raw := mkStore(t)
	wrapped := &completionSaveAckLostStore{StateStore: raw}
	sealer, err := NewAESGCMSealer(fixedKEK(t))
	if err != nil {
		t.Fatal(err)
	}
	flows, err := NewFlowStore(wrapped, sealer)
	if err != nil {
		t.Fatal(err)
	}
	flow := testPendingFlow("state-completed-ack-loss")
	ctx := mkCtx(t, flow.Identity)
	if err := flows.Put(ctx, flow); err != nil {
		t.Fatal(err)
	}
	_, claim, ok, err := flows.Claim(ctx, flow.State)
	if err != nil || !ok {
		t.Fatalf("Claim = ok:%v err:%v", ok, err)
	}
	completed := testCompletedFlow(flow)
	if err := flows.MarkCompleted(ctx, claim, completed); err != nil {
		t.Fatalf("MarkCompleted did not reconcile landed Save: %v", err)
	}
	marker, err := raw.LoadByEventID(ctx, flowCompletedEventID(flow.State))
	if err != nil {
		t.Fatal(err)
	}
	for _, plaintext := range []string{flow.SubjectID, flow.Identity.UserID, string(flow.PauseToken), string(flow.Source)} {
		if bytes.Contains(marker.Bytes, []byte(plaintext)) {
			t.Fatalf("completed marker leaked plaintext %q", plaintext)
		}
	}
	if err := flows.Finish(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := flows.Get(ctx, flow.State); err != nil || ok {
		t.Fatalf("pending flow after Finish = ok:%v err:%v", ok, err)
	}
	got, ok, err := flows.GetCompleted(ctx, flow.State)
	if err != nil || !ok || got != completed {
		t.Fatalf("completion tombstone after Finish = (%+v,%v,%v)", got, ok, err)
	}
}

func TestFlowStore_PutPrunesOnlyExpiredCompletedTombstonesForExactIdentity(t *testing.T) {
	raw, flows, _ := newFlowStoreFixture(t)
	concrete := flows.(*stateStoreFlowStore)
	now := time.Now().UTC()
	concrete.now = func() time.Time { return now }

	idA := testPendingFlow("identity-template-a").Identity
	idB := identity.Identity{TenantID: idA.TenantID, UserID: "user-b", SessionID: "session-b"}
	ctxA := mkCtx(t, idA)
	ctxB := mkCtx(t, idB)
	stageCompletion := func(ctx context.Context, flow PendingFlowRecord) {
		t.Helper()
		if err := flows.Put(ctx, flow); err != nil {
			t.Fatalf("Put %s: %v", flow.State, err)
		}
		_, claim, ok, err := flows.Claim(ctx, flow.State)
		if err != nil || !ok {
			t.Fatalf("Claim %s = ok:%v err:%v", flow.State, ok, err)
		}
		if err := flows.MarkCompleted(ctx, claim, testCompletedFlow(flow)); err != nil {
			t.Fatalf("MarkCompleted %s: %v", flow.State, err)
		}
		if err := flows.Finish(ctx, claim); err != nil {
			t.Fatalf("Finish %s: %v", flow.State, err)
		}
	}
	flowFor := func(stateToken string, id identity.Identity, expires time.Time) PendingFlowRecord {
		flow := testPendingFlow(stateToken)
		flow.Identity = id
		flow.SubjectID = id.UserID
		flow.CreatedAt = now
		flow.ExpiresAt = expires
		flow.PauseToken = pauseresume.Token("pause-" + stateToken)
		return flow
	}

	expiredA := flowFor("completed-expired-a", idA, now.Add(time.Minute))
	liveA := flowFor("completed-live-a", idA, now.Add(10*time.Minute))
	expiredB := flowFor("completed-expired-b", idB, now.Add(time.Minute))
	stageCompletion(ctxA, expiredA)
	stageCompletion(ctxA, liveA)
	stageCompletion(ctxB, expiredB)

	now = now.Add(2 * time.Minute)
	newA := flowFor("new-flow-a", idA, now.Add(10*time.Minute))
	if err := flows.Put(ctxA, newA); err != nil {
		t.Fatalf("Put new flow: %v", err)
	}
	if _, ok, err := flows.GetCompleted(ctxA, expiredA.State); err != nil || ok {
		t.Fatalf("expired identity-A tombstone retained: ok=%v err=%v", ok, err)
	}
	if _, ok, err := flows.GetCompleted(ctxA, liveA.State); err != nil || !ok {
		t.Fatalf("unexpired identity-A tombstone pruned: ok=%v err=%v", ok, err)
	}
	if _, ok, err := flows.GetCompleted(ctxB, expiredB.State); err != nil || !ok {
		t.Fatalf("identity-B tombstone touched by identity-A Put: ok=%v err=%v", ok, err)
	}
	recordsB, err := raw.ListKindForIdentity(ctxB, identity.Quadruple{Identity: idB}, flowCompletedKindPrefix)
	if err != nil || len(recordsB) != 1 {
		t.Fatalf("identity-B completed population = %d err=%v want=1", len(recordsB), err)
	}
}
