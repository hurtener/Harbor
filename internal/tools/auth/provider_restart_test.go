package auth

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/state"
)

func restartBuildConfig(server *fakeAuthServer, names ...string) config.ToolsConfig {
	providers := make([]config.ToolOAuthProviderConfig, 0, len(names))
	for _, name := range names {
		p := bpProviderCfg("bp-test")
		p.Name = name
		p.AuthURL = server.BaseURL() + "/authorize"
		p.TokenURL = server.BaseURL() + "/token"
		providers = append(providers, p)
	}
	return config.ToolsConfig{OAuthTokenKEKEnv: "HARBOR_BP_TEST_KEK", OAuthProviders: providers}
}

func restartBuildDeps(t *testing.T, raw state.StateStore, coord pauseresume.Coordinator) BuildDeps {
	t.Helper()
	red := mkRedactor(t)
	return BuildDeps{State: raw, Bus: mkBus(t, red), Redactor: red, Coordinator: coord}
}

func TestBuildProviders_RestartCompletesDurablePKCEFlowAndUnifiedPause(t *testing.T) {
	registerBPTestDriver(t)
	t.Setenv("HARBOR_BP_TEST_KEK", dummyKEKHex)
	t.Setenv("HARBOR_BP_TEST_CLIENT_ID", "dummy-client-id")
	t.Setenv("HARBOR_BP_TEST_CLIENT_SECRET", "dummy-client-secret")
	server := newFakeAuthServer(t)
	raw := mkStore(t)
	id := mkIdentity(t)
	ctx := mkCtx(t, id)
	cfg := restartBuildConfig(server, "restart-source")

	coord1 := pauseresume.New(pauseresume.WithCheckpointStore(raw))
	first, err := BuildProviders(ctx, cfg, restartBuildDeps(t, raw, coord1))
	if err != nil {
		t.Fatalf("BuildProviders first: %v", err)
	}
	flow, err := first["restart-source"].InitiateFlow(ctx, "restart-source")
	if err != nil {
		t.Fatalf("InitiateFlow: %v", err)
	}
	code, _, err := server.VisitAuthorizeURL(flow.AuthorizeURL)
	if err != nil {
		t.Fatalf("VisitAuthorizeURL: %v", err)
	}
	rawFlow, err := raw.LoadByEventID(ctx, state.EventID(flow.State))
	if err != nil {
		t.Fatalf("durable flow lookup: %v", err)
	}
	for _, forbidden := range []string{"dummy-client-secret", flow.PauseToken, id.UserID} {
		if bytes.Contains(rawFlow.Bytes, []byte(forbidden)) {
			t.Fatalf("pending flow leaked plaintext %q", forbidden)
		}
	}
	if err := first["restart-source"].Close(ctx); err != nil {
		t.Fatalf("Close first provider: %v", err)
	}

	coord2 := pauseresume.New(pauseresume.WithCheckpointStore(raw))
	second, err := BuildProviders(ctx, cfg, restartBuildDeps(t, raw, coord2))
	if err != nil {
		t.Fatalf("BuildProviders restart: %v", err)
	}
	if _, ok, err := second["restart-source"].PendingFlow(ctx, flow.State); err != nil || !ok {
		t.Fatalf("PendingFlow after restart = ok:%v err:%v", ok, err)
	}
	if _, err := second["restart-source"].CompleteFlow(ctx, flow.State, code); err != nil {
		t.Fatalf("CompleteFlow after restart: %v", err)
	}
	status, err := coord2.Status(ctx, pauseresume.Token(flow.PauseToken))
	if err != nil || status.State != pauseresume.StatusResumed {
		t.Fatalf("pause after restart completion = %+v err=%v", status, err)
	}
	if got := server.TokenCalls(); got != 1 {
		t.Fatalf("token exchanges=%d want=1", got)
	}
}

func TestCallbackHandler_RestartRoutesStateOnlyToOwningProvider(t *testing.T) {
	registerBPTestDriver(t)
	t.Setenv("HARBOR_BP_TEST_KEK", dummyKEKHex)
	t.Setenv("HARBOR_BP_TEST_CLIENT_ID", "dummy-client-id")
	t.Setenv("HARBOR_BP_TEST_CLIENT_SECRET", "dummy-client-secret")
	server := newFakeAuthServer(t)
	raw := mkStore(t)
	ctx := mkCtx(t, mkIdentity(t))
	cfg := restartBuildConfig(server, "provider-a", "provider-b")
	coord := pauseresume.New(pauseresume.WithCheckpointStore(raw))
	first, err := BuildProviders(ctx, cfg, restartBuildDeps(t, raw, coord))
	if err != nil {
		t.Fatal(err)
	}
	flow, err := first["provider-b"].InitiateFlow(ctx, "provider-b")
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := server.VisitAuthorizeURL(flow.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := BuildProviders(ctx, cfg, restartBuildDeps(t, raw, pauseresume.New(pauseresume.WithCheckpointStore(raw))))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := rebuilt["provider-a"].PendingFlow(ctx, flow.State); err != nil || ok {
		t.Fatalf("non-owner PendingFlow = ok:%v err:%v", ok, err)
	}
	if _, ok, err := rebuilt["provider-b"].PendingFlow(ctx, flow.State); err != nil || !ok {
		t.Fatalf("owner PendingFlow = ok:%v err:%v", ok, err)
	}
	req := httptest.NewRequest("GET", CallbackPath+"?state="+flow.State+"&code="+code, nil)
	rec := httptest.NewRecorder()
	CallbackHandler(rebuilt).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("callback status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := server.TokenCalls(); got != 1 {
		t.Fatalf("token exchanges=%d want=1", got)
	}
}

type failFinishOnceStore struct {
	FlowStore
	failed atomic.Bool
}

type finishAckLostOnceStore struct {
	FlowStore
	failed atomic.Bool
}

type pendingDeleteFailOnceStateStore struct {
	state.StateStore
	failed atomic.Bool
}

func (s *pendingDeleteFailOnceStateStore) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	return s.StateStore.SaveIf(ctx, expectations, next)
}

func (s *pendingDeleteFailOnceStateStore) Delete(ctx context.Context, id identity.Quadruple, kind string) error {
	isPending := strings.HasPrefix(kind, flowKindPrefix) &&
		!strings.HasPrefix(kind, flowClaimKindPrefix) &&
		!strings.HasPrefix(kind, flowTerminalKindPrefix) &&
		!strings.HasPrefix(kind, flowCompletedKindPrefix)
	if isPending && s.failed.CompareAndSwap(false, true) {
		return errors.New("injected pending-flow Delete failure")
	}
	return s.StateStore.Delete(ctx, id, kind)
}

type failResumeOnceCoordinator struct {
	pauseresume.Coordinator
	failed atomic.Bool
}

func (c *failResumeOnceCoordinator) Resume(ctx context.Context, token pauseresume.Token, decision pauseresume.Decision, result map[string]any) error {
	if c.failed.CompareAndSwap(false, true) {
		return errors.New("injected Resume failure")
	}
	return c.Coordinator.Resume(ctx, token, decision, result)
}

func (s *failFinishOnceStore) Finish(ctx context.Context, claim FlowClaim) error {
	if s.failed.CompareAndSwap(false, true) {
		return errors.New("injected Finish failure")
	}
	return s.FlowStore.Finish(ctx, claim)
}

func (s *finishAckLostOnceStore) Finish(ctx context.Context, claim FlowClaim) error {
	if err := s.FlowStore.Finish(ctx, claim); err != nil {
		return err
	}
	if s.failed.CompareAndSwap(false, true) {
		return errors.New("injected Finish acknowledgement loss")
	}
	return nil
}

func TestProvider_CompleteFlow_FinishFailureRetryDoesNotReexchange(t *testing.T) {
	h := newProviderHarness(t)
	provider, err := NewProvider([]OAuthConfig{h.userCfg, h.agentCfg}, ProviderDeps{
		Store: h.store, Flows: &failFinishOnceStore{FlowStore: h.flows}, Bus: h.bus,
		Redactor: h.redactor, Coordinator: h.coordinator, HTTPClient: &http.Client{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := mkCtx(t, mkIdentity(t))
	_, err = provider.Token(ctx, h.userCfg.Source)
	var required *ErrAuthRequired
	if !errors.As(err, &required) {
		t.Fatalf("Token: %v", err)
	}
	code, _, err := h.server.VisitAuthorizeURL(required.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.CompleteFlow(ctx, required.State, code); err == nil || err.Error() != "injected Finish failure" {
		t.Fatalf("first CompleteFlow err=%v", err)
	}
	if _, err := provider.CompleteFlow(ctx, required.State, code); err != nil {
		t.Fatalf("retry CompleteFlow: %v", err)
	}
	if got := h.server.TokenCalls(); got != 1 {
		t.Fatalf("token exchanges=%d want=1", got)
	}
	if _, ok, err := provider.PendingFlow(ctx, required.State); err != nil || !ok {
		t.Fatalf("completion tombstone missing after cleanup retry: ok=%v err=%v", ok, err)
	}
	if _, ok, err := h.flows.Get(ctx, required.State); err != nil || ok {
		t.Fatalf("pending secret-bearing flow retained after cleanup retry: ok=%v err=%v", ok, err)
	}
}

func TestProvider_CompleteFlow_ConcurrentReplacementCannotErasePerFlowCompletion(t *testing.T) {
	h := newProviderHarness(t)
	flows := &failFinishOnceStore{FlowStore: h.flows}
	provider, err := NewProvider([]OAuthConfig{h.userCfg, h.agentCfg}, ProviderDeps{
		Store: h.store, Flows: flows, Bus: h.bus, Redactor: h.redactor,
		Coordinator: h.coordinator, HTTPClient: &http.Client{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := mkCtx(t, mkIdentity(t))
	flowA, err := provider.InitiateFlow(ctx, h.userCfg.Source)
	if err != nil {
		t.Fatal(err)
	}
	flowB, err := provider.InitiateFlow(ctx, h.userCfg.Source)
	if err != nil {
		t.Fatal(err)
	}
	codeA, _, err := h.server.VisitAuthorizeURL(flowA.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	codeB, _, err := h.server.VisitAuthorizeURL(flowB.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.CompleteFlow(ctx, flowA.State, codeA); err == nil || err.Error() != "injected Finish failure" {
		t.Fatalf("flow A first completion err=%v", err)
	}
	if _, err := provider.CompleteFlow(ctx, flowB.State, codeB); err != nil {
		t.Fatalf("flow B completion: %v", err)
	}
	current, ok, err := h.store.Get(ctx, ScopeUser, mkIdentity(t).UserID, h.userCfg.Source)
	if err != nil || !ok || current.completedFlowState != flowB.State {
		t.Fatalf("current token marker after flow B = %q ok=%v err=%v", current.completedFlowState, ok, err)
	}
	retried, err := provider.CompleteFlow(ctx, flowA.State, codeA)
	if err != nil {
		t.Fatalf("flow A tombstone retry: %v", err)
	}
	if retried.completedFlowState != flowB.State {
		t.Fatalf("flow A retry returned marker %q want current flow B marker %q", retried.completedFlowState, flowB.State)
	}
	if got := h.server.TokenCalls(); got != 2 {
		t.Fatalf("token exchanges=%d want exactly A+B=2", got)
	}
	if _, ok, err := h.flows.Get(ctx, flowA.State); err != nil || ok {
		t.Fatalf("flow A pending record retained after tombstone retry: ok=%v err=%v", ok, err)
	}
	for _, flow := range []FlowInitiation{flowA, flowB} {
		status, statusErr := h.coordinator.Status(ctx, pauseresume.Token(flow.PauseToken))
		if statusErr != nil || status.State != pauseresume.StatusResumed || status.Decision != pauseresume.DecisionResume {
			t.Fatalf("pause %s = %+v err=%v", flow.State, status, statusErr)
		}
	}
}

func TestCallbackHandler_CompletedTombstoneRoutesAfterFinishDeleteAckLoss(t *testing.T) {
	h := newProviderHarness(t)
	flows := &finishAckLostOnceStore{FlowStore: h.flows}
	provider, err := NewProvider([]OAuthConfig{h.userCfg, h.agentCfg}, ProviderDeps{
		Store: h.store, Flows: flows, Bus: h.bus, Redactor: h.redactor,
		Coordinator: h.coordinator, HTTPClient: &http.Client{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := mkCtx(t, mkIdentity(t))
	flow, err := provider.InitiateFlow(ctx, h.userCfg.Source)
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := h.server.VisitAuthorizeURL(flow.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.CompleteFlow(ctx, flow.State, code); err == nil ||
		!strings.Contains(err.Error(), "injected Finish acknowledgement loss") {
		t.Fatalf("first completion err=%v", err)
	}
	if _, ok, err := h.flows.Get(ctx, flow.State); err != nil || ok {
		t.Fatalf("pending flow survived landed Finish: ok=%v err=%v", ok, err)
	}
	if _, ok, err := provider.PendingFlow(context.Background(), flow.State); err != nil || !ok {
		t.Fatalf("callback could not route completion tombstone: ok=%v err=%v", ok, err)
	}

	handler := CallbackHandler(map[string]OAuthProvider{"provider": provider})
	req := httptest.NewRequest(http.MethodGet, CallbackPath+"?state="+flow.State+"&code="+code, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry callback status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := h.server.TokenCalls(); got != 1 {
		t.Fatalf("token exchanges=%d want=1", got)
	}
}

func TestProvider_CompleteFlow_TombstoneRetryCleansPartialFinishPendingRecord(t *testing.T) {
	h := newProviderHarness(t)
	raw := mkStore(t)
	flaky := &pendingDeleteFailOnceStateStore{StateStore: raw}
	sealer, err := NewAESGCMSealer(fixedKEK(t))
	if err != nil {
		t.Fatal(err)
	}
	flows, err := NewFlowStore(flaky, sealer)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := NewProvider([]OAuthConfig{h.userCfg, h.agentCfg}, ProviderDeps{
		Store: h.store, Flows: flows, Bus: h.bus, Redactor: h.redactor,
		Coordinator: h.coordinator, HTTPClient: &http.Client{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := mkCtx(t, mkIdentity(t))
	flow, err := provider.InitiateFlow(ctx, h.userCfg.Source)
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := h.server.VisitAuthorizeURL(flow.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.CompleteFlow(ctx, flow.State, code); err == nil ||
		!strings.Contains(err.Error(), "injected pending-flow Delete failure") {
		t.Fatalf("first completion err=%v", err)
	}
	if _, ok, err := flows.Get(ctx, flow.State); err != nil || !ok {
		t.Fatalf("partial Finish did not retain pending record: ok=%v err=%v", ok, err)
	}
	if _, err := provider.CompleteFlow(ctx, flow.State, code); err != nil {
		t.Fatalf("tombstone cleanup retry: %v", err)
	}
	if _, ok, err := flows.Get(ctx, flow.State); err != nil || ok {
		t.Fatalf("pending record survived tombstone cleanup retry: ok=%v err=%v", ok, err)
	}
	if got := h.server.TokenCalls(); got != 1 {
		t.Fatalf("token exchanges=%d want=1", got)
	}
}

func TestProvider_CompleteFlow_ExpiredCompletionTombstoneIsForgotten(t *testing.T) {
	h := newProviderHarness(t)
	now := time.Now()
	provider, err := NewProvider([]OAuthConfig{h.userCfg, h.agentCfg}, ProviderDeps{
		Store: h.store, Flows: h.flows, Bus: h.bus, Redactor: h.redactor,
		Coordinator: h.coordinator, HTTPClient: &http.Client{}, Clock: func() time.Time { return now }, FlowTTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := mkCtx(t, mkIdentity(t))
	flow, err := provider.InitiateFlow(ctx, h.userCfg.Source)
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := h.server.VisitAuthorizeURL(flow.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.CompleteFlow(ctx, flow.State, code); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := provider.CompleteFlow(ctx, flow.State, code); !errors.Is(err, ErrFlowExpired) {
		t.Fatalf("expired tombstone retry err=%v want ErrFlowExpired", err)
	}
	if _, ok, err := h.flows.GetCompleted(ctx, flow.State); err != nil || ok {
		t.Fatalf("expired completion tombstone retained: ok=%v err=%v", ok, err)
	}
	if got := h.server.TokenCalls(); got != 1 {
		t.Fatalf("token exchanges=%d want=1", got)
	}
}

func TestProvider_CompleteFlow_UnrelatedNewerTokenCannotCompleteFlow(t *testing.T) {
	h := newProviderHarness(t)
	ctx := mkCtx(t, mkIdentity(t))
	_, err := h.provider.Token(ctx, h.userCfg.Source)
	var required *ErrAuthRequired
	if !errors.As(err, &required) {
		t.Fatalf("Token: %v", err)
	}
	code, _, err := h.server.VisitAuthorizeURL(required.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	id := mkIdentity(t)
	if err := h.store.Put(ctx, Token{
		Source: h.userCfg.Source, BindingScope: ScopeUser,
		TenantID: id.TenantID, UserID: id.UserID,
		AccessToken: "unrelated-concurrent-token", LastRefreshedAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	tok, err := h.provider.CompleteFlow(ctx, required.State, code)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken == "unrelated-concurrent-token" {
		t.Fatal("unrelated newer token falsely completed the pending OAuth flow")
	}
	if got := h.server.TokenCalls(); got != 1 {
		t.Fatalf("token exchanges=%d want=1", got)
	}
}

func TestProvider_CompleteFlow_TokenPutAndResumeFailureRetainsTerminalRetry(t *testing.T) {
	h := newProviderHarness(t)
	coord := &failResumeOnceCoordinator{Coordinator: h.coordinator}
	provider, err := NewProvider([]OAuthConfig{h.userCfg, h.agentCfg}, ProviderDeps{
		Store: &failPutOnceTokenStore{TokenStore: h.store}, Flows: h.flows, Bus: h.bus,
		Redactor: h.redactor, Coordinator: coord, HTTPClient: &http.Client{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := mkCtx(t, mkIdentity(t))
	_, err = provider.Token(ctx, h.userCfg.Source)
	var required *ErrAuthRequired
	if !errors.As(err, &required) {
		t.Fatalf("Token: %v", err)
	}
	code, _, err := h.server.VisitAuthorizeURL(required.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.CompleteFlow(ctx, required.State, code); err == nil {
		t.Fatal("first CompleteFlow unexpectedly succeeded")
	}
	if _, ok, err := provider.PendingFlow(ctx, required.State); err != nil || !ok {
		t.Fatalf("terminal flow not retained: ok=%v err=%v", ok, err)
	}
	if _, err := provider.CompleteFlow(ctx, required.State, code); !errors.Is(err, ErrExchangeFailed) {
		t.Fatalf("terminal retry err=%v want ErrExchangeFailed", err)
	}
	status, err := h.coordinator.Status(ctx, pauseresume.Token(required.PauseToken))
	if err != nil || status.State != pauseresume.StatusResumed || status.Decision != pauseresume.DecisionReject {
		t.Fatalf("terminal retry pause=%+v err=%v", status, err)
	}
	if _, ok, err := provider.PendingFlow(ctx, required.State); err != nil || ok {
		t.Fatalf("terminal flow retained after retry: ok=%v err=%v", ok, err)
	}
	if got := h.server.TokenCalls(); got != 1 {
		t.Fatalf("token exchanges=%d want=1", got)
	}
}

func TestProvider_CompleteFlow_RefreshPreservesExactCleanupMarker(t *testing.T) {
	h := newProviderHarness(t)
	now := time.Now()
	coord := &failResumeOnceCoordinator{Coordinator: h.coordinator}
	provider, err := NewProvider([]OAuthConfig{h.userCfg, h.agentCfg}, ProviderDeps{
		Store: h.store, Flows: h.flows, Bus: h.bus, Redactor: h.redactor,
		Coordinator: coord, HTTPClient: &http.Client{}, Clock: func() time.Time { return now }, FlowTTL: 3 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := mkCtx(t, mkIdentity(t))
	_, err = provider.Token(ctx, h.userCfg.Source)
	var required *ErrAuthRequired
	if !errors.As(err, &required) {
		t.Fatalf("Token: %v", err)
	}
	code, _, err := h.server.VisitAuthorizeURL(required.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.CompleteFlow(ctx, required.State, code); err == nil {
		t.Fatal("first CompleteFlow unexpectedly succeeded")
	}
	now = now.Add(2 * time.Hour)
	if _, err := provider.Token(ctx, h.userCfg.Source); err != nil {
		t.Fatalf("refresh token: %v", err)
	}
	if _, err := provider.CompleteFlow(ctx, required.State, code); err != nil {
		t.Fatalf("cleanup retry after refresh: %v", err)
	}
	if got := h.server.TokenCalls(); got != 2 {
		t.Fatalf("token endpoint calls=%d want exchange+refresh=2", got)
	}
}

func TestProvider_CompleteFlow_CompetingRejectCannotReportCompletion(t *testing.T) {
	h := newProviderHarness(t)
	ctx := mkCtx(t, mkIdentity(t))
	_, err := h.provider.Token(ctx, h.userCfg.Source)
	var required *ErrAuthRequired
	if !errors.As(err, &required) {
		t.Fatalf("Token: %v", err)
	}
	code, _, err := h.server.VisitAuthorizeURL(required.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.coordinator.Resume(ctx, pauseresume.Token(required.PauseToken), pauseresume.DecisionReject, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.provider.CompleteFlow(ctx, required.State, code); err == nil {
		t.Fatal("OAuth completion succeeded after a competing reject decision")
	}
}

func TestProvider_DenyFlow_FinishFailureRetryConverges(t *testing.T) {
	h := newProviderHarness(t)
	provider, err := NewProvider([]OAuthConfig{h.userCfg, h.agentCfg}, ProviderDeps{
		Store: h.store, Flows: &failFinishOnceStore{FlowStore: h.flows}, Bus: h.bus,
		Redactor: h.redactor, Coordinator: h.coordinator, HTTPClient: &http.Client{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := mkCtx(t, mkIdentity(t))
	flow, err := provider.InitiateFlow(ctx, h.userCfg.Source)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.DenyFlow(ctx, flow.State, "access_denied"); err == nil {
		t.Fatal("first DenyFlow unexpectedly succeeded")
	}
	if err := provider.DenyFlow(ctx, flow.State, "access_denied"); err != nil {
		t.Fatalf("retry DenyFlow: %v", err)
	}
	if _, ok, err := provider.PendingFlow(ctx, flow.State); err != nil || ok {
		t.Fatalf("denied flow retained after retry: ok=%v err=%v", ok, err)
	}
}

func TestProvider_CompleteFlow_ExchangeFailureReleasesClaimAndKeepsPauseRetryable(t *testing.T) {
	h := newProviderHarness(t)
	ctx := mkCtx(t, mkIdentity(t))
	_, err := h.provider.Token(ctx, h.userCfg.Source)
	var required *ErrAuthRequired
	if !errors.As(err, &required) {
		t.Fatalf("Token: %v", err)
	}
	code, _, err := h.server.VisitAuthorizeURL(required.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.provider.CompleteFlow(ctx, required.State, "invalid-code"); !errors.Is(err, ErrExchangeFailed) {
		t.Fatalf("invalid exchange err=%v", err)
	}
	status, err := h.coordinator.Status(ctx, pauseresume.Token(required.PauseToken))
	if err != nil || status.State != pauseresume.StatusPaused {
		t.Fatalf("pause after retryable exchange failure = %+v err=%v", status, err)
	}
	if _, ok, err := h.provider.PendingFlow(ctx, required.State); err != nil || !ok {
		t.Fatalf("flow after retryable exchange failure = ok:%v err:%v", ok, err)
	}
	if _, err := h.provider.CompleteFlow(ctx, required.State, code); err != nil {
		t.Fatalf("valid retry: %v", err)
	}
}

type failPutOnceTokenStore struct {
	TokenStore
	failed atomic.Bool
}

type cancelPutTokenStore struct {
	TokenStore
	cancel context.CancelFunc
}

func (s *cancelPutTokenStore) Put(ctx context.Context, _ Token) error {
	s.cancel()
	return ctx.Err()
}

func (s *failPutOnceTokenStore) Put(ctx context.Context, token Token) error {
	if s.failed.CompareAndSwap(false, true) {
		return errors.New("injected token Put failure")
	}
	return s.TokenStore.Put(ctx, token)
}

func TestProvider_CompleteFlow_TokenPutFailureRejectsPauseAndConsumesSpentCode(t *testing.T) {
	h := newProviderHarness(t)
	provider, err := NewProvider([]OAuthConfig{h.userCfg, h.agentCfg}, ProviderDeps{
		Store: &failPutOnceTokenStore{TokenStore: h.store}, Flows: h.flows, Bus: h.bus,
		Redactor: h.redactor, Coordinator: h.coordinator, HTTPClient: &http.Client{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := mkCtx(t, mkIdentity(t))
	_, err = provider.Token(ctx, h.userCfg.Source)
	var required *ErrAuthRequired
	if !errors.As(err, &required) {
		t.Fatalf("Token: %v", err)
	}
	code, _, err := h.server.VisitAuthorizeURL(required.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.CompleteFlow(ctx, required.State, code); err == nil || !bytes.Contains([]byte(err.Error()), []byte("injected token Put failure")) {
		t.Fatalf("CompleteFlow err=%v", err)
	}
	status, err := h.coordinator.Status(ctx, pauseresume.Token(required.PauseToken))
	if err != nil || status.State != pauseresume.StatusResumed || status.Decision != pauseresume.DecisionReject {
		t.Fatalf("pause after token Put failure = %+v err=%v", status, err)
	}
	if _, ok, err := provider.PendingFlow(ctx, required.State); err != nil || ok {
		t.Fatalf("spent flow retained: ok=%v err=%v", ok, err)
	}
}

func TestProvider_CompleteFlow_CancelledTokenPutStillRejectsAndCleansUp(t *testing.T) {
	h := newProviderHarness(t)
	baseCtx := mkCtx(t, mkIdentity(t))
	ctx, cancel := context.WithCancel(baseCtx)
	provider, err := NewProvider([]OAuthConfig{h.userCfg, h.agentCfg}, ProviderDeps{
		Store: &cancelPutTokenStore{TokenStore: h.store, cancel: cancel}, Flows: h.flows, Bus: h.bus,
		Redactor: h.redactor, Coordinator: h.coordinator, HTTPClient: &http.Client{},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Token(ctx, h.userCfg.Source)
	var required *ErrAuthRequired
	if !errors.As(err, &required) {
		t.Fatalf("Token: %v", err)
	}
	code, _, err := h.server.VisitAuthorizeURL(required.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.CompleteFlow(ctx, required.State, code); !errors.Is(err, context.Canceled) {
		t.Fatalf("CompleteFlow err=%v want context.Canceled", err)
	}
	status, err := h.coordinator.Status(baseCtx, pauseresume.Token(required.PauseToken))
	if err != nil || status.State != pauseresume.StatusResumed || status.Decision != pauseresume.DecisionReject {
		t.Fatalf("pause after canceled Put = %+v err=%v", status, err)
	}
	if _, ok, err := provider.PendingFlow(baseCtx, required.State); err != nil || ok {
		t.Fatalf("spent flow retained after canceled Put: ok=%v err=%v", ok, err)
	}
}

type cancelPutFlowStore struct {
	FlowStore
	cancel context.CancelFunc
}

func (s *cancelPutFlowStore) Put(ctx context.Context, _ PendingFlowRecord) error {
	s.cancel()
	return ctx.Err()
}

type recordingCoordinator struct {
	pauseresume.Coordinator
	mu    sync.Mutex
	token pauseresume.Token
}

func (c *recordingCoordinator) Request(ctx context.Context, req pauseresume.PauseRequest) (pauseresume.Pause, error) {
	pause, err := c.Coordinator.Request(ctx, req)
	if err == nil {
		c.mu.Lock()
		c.token = pause.Token
		c.mu.Unlock()
	}
	return pause, err
}

func (c *recordingCoordinator) requestedToken() pauseresume.Token {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

func TestProvider_PendingFlowPutCancellationRejectsAllocatedPause(t *testing.T) {
	for _, initiate := range []bool{false, true} {
		t.Run(map[bool]string{false: "Token", true: "InitiateFlow"}[initiate], func(t *testing.T) {
			h := newProviderHarness(t)
			baseCtx := mkCtx(t, mkIdentity(t))
			ctx, cancel := context.WithCancel(baseCtx)
			coord := &recordingCoordinator{Coordinator: h.coordinator}
			flows := &cancelPutFlowStore{FlowStore: h.flows, cancel: cancel}
			provider, err := NewProvider([]OAuthConfig{h.userCfg, h.agentCfg}, ProviderDeps{
				Store: h.store, Flows: flows, Bus: h.bus, Redactor: h.redactor,
				Coordinator: coord, HTTPClient: &http.Client{},
			})
			if err != nil {
				t.Fatal(err)
			}
			if initiate {
				_, err = provider.InitiateFlow(ctx, h.userCfg.Source)
			} else {
				_, err = provider.Token(ctx, h.userCfg.Source)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("operation err=%v want context.Canceled", err)
			}
			status, statusErr := coord.Status(baseCtx, coord.requestedToken())
			if statusErr != nil || status.State != pauseresume.StatusResumed || status.Decision != pauseresume.DecisionReject {
				t.Fatalf("pause after canceled flow Put = %+v err=%v", status, statusErr)
			}
		})
	}
}
