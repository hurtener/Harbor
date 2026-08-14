package protocol_test

// composition_preview_concurrency_test.go — the concurrent-reuse gate for the
// read-only composition preview service (D-025 shape): N>=100 concurrent
// invocations against ONE shared service + ONE shared registry + ONE shared
// frozen boot index, mixing ordinary own previews from two users, elevated
// widened previews, foreign targets, cross-tenant targets, and a retired
// agent. Run under -race. Asserts deterministic per-case output (byte-identical
// to serial baselines — no context bleed), no data races, an audited widen
// per elevated invocation, and a restored goroutine baseline.

import (
	"fmt"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/protocol/auth"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

func TestCompositionPreview_ConcurrentMixedIsolation_NoCrossTalk(t *testing.T) {
	fx := newPreviewFixture(t, standardBootIndex())
	bus := &recordingPreviewBus{}
	reg := previewRegistry(t)
	preview, err := agentcfgprotocol.NewCompositionPreviewService(reg, fx.bootIdx,
		agentcfgprotocol.WithPreviewSessionReach(auth.NewSessionReachAuthorizer()),
		agentcfgprotocol.WithPreviewAgentReach(auth.NewAgentReachAuthorizer()),
		agentcfgprotocol.WithPreviewBus(bus),
	)
	if err != nil {
		t.Fatalf("NewCompositionPreviewService: %v", err)
	}
	adminSvc, err := agentcfgprotocol.NewService(reg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	seedPackItem(t, adminSvc, packItemWire("gamma", "Gamma", "gamma trigger", []string{"gamma step"}))
	retirePreviewAgent(t, reg, "agent-retired")

	// Serial baselines: every concurrent case must reproduce its own baseline
	// byte-for-byte — no cross-talk, no determinism drift.
	ownA, err := preview.CompositionPreview(
		previewCtx(t, previewUserA, nil, nil, []string{testAgentID, "agent-retired"}),
		previewReq("ua", "sa", testAgentID),
	)
	if err != nil {
		t.Fatalf("baseline ownA: %v", err)
	}
	ownB, err := preview.CompositionPreview(
		previewCtx(t, previewUserB, nil, nil, []string{testAgentID, "agent-retired"}),
		previewReq("ub", "sb", testAgentID),
	)
	if err != nil {
		t.Fatalf("baseline ownB: %v", err)
	}
	widened, err := preview.CompositionPreview(
		previewCtx(t, previewAdmin, []auth.Scope{auth.ScopeAdmin}, nil, []string{testAgentID, "agent-retired"}),
		previewReq("ua", "sa", testAgentID),
	)
	if err != nil {
		t.Fatalf("baseline widened: %v", err)
	}
	unavailable, err := preview.CompositionPreview(
		previewCtx(t, previewUserA, nil, nil, []string{testAgentID, "agent-retired"}),
		previewReq("u-other", "s-other", testAgentID),
	)
	if err != nil {
		t.Fatalf("baseline unavailable: %v", err)
	}
	crossTenant, err := preview.CompositionPreview(
		previewCtx(t, previewAdmin, []auth.Scope{auth.ScopeAdmin}, nil, []string{testAgentID, "agent-retired"}),
		agentcfgprotocol.CompositionPreviewRequest{TenantID: "other-tenant", UserID: "ua", SessionID: "sa", AgentID: testAgentID},
	)
	if err != nil {
		t.Fatalf("baseline cross-tenant: %v", err)
	}
	retired, err := preview.CompositionPreview(
		previewCtx(t, previewUserA, nil, nil, []string{testAgentID, "agent-retired"}),
		previewReq("ua", "sa", "agent-retired"),
	)
	if err != nil {
		t.Fatalf("baseline retired: %v", err)
	}

	// Every elevated widened invocation (case 2: n/6 of them) must have been
	// audited exactly once — the widened audit is never dropped under load.
	// The serial baselines above already emitted one audit for the widened
	// baseline; capture that BEFORE the goroutines spawn so the concurrent
	// delta is counted without racing the workers.
	auditBaseline := len(bus.adminEvents())

	baseline := runtime.NumGoroutine()
	const n = 120
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var resp agentcfgprotocol.CompositionPreviewResponse
			var err error
			switch i % 6 {
			case 0: // own preview, user A
				resp, err = preview.CompositionPreview(
					previewCtx(t, previewUserA, nil, nil, []string{testAgentID, "agent-retired"}),
					previewReq("ua", "sa", testAgentID),
				)
				if err == nil && !reflect.DeepEqual(resp, ownA) {
					err = fmt.Errorf("case 0 context bleed: ownA baseline %+v got %+v", ownA, resp)
				}
			case 1: // own preview, user B — distinct user, same agent
				resp, err = preview.CompositionPreview(
					previewCtx(t, previewUserB, nil, nil, []string{testAgentID, "agent-retired"}),
					previewReq("ub", "sb", testAgentID),
				)
				if err == nil && !reflect.DeepEqual(resp, ownB) {
					err = fmt.Errorf("case 1 context bleed: ownB baseline %+v got %+v", ownB, resp)
				}
			case 2: // elevated widened preview of user A
				resp, err = preview.CompositionPreview(
					previewCtx(t, previewAdmin, []auth.Scope{auth.ScopeAdmin}, nil, []string{testAgentID, "agent-retired"}),
					previewReq("ua", "sa", testAgentID),
				)
				if err == nil && !reflect.DeepEqual(resp, widened) {
					err = fmt.Errorf("case 2 context bleed: widened baseline %+v got %+v", widened, resp)
				}
			case 3: // foreign triple — non-oracular unavailable
				resp, err = preview.CompositionPreview(
					previewCtx(t, previewUserA, nil, nil, []string{testAgentID, "agent-retired"}),
					previewReq("u-other", "s-other", testAgentID),
				)
				if err == nil && !reflect.DeepEqual(resp, unavailable) {
					err = fmt.Errorf("case 3 context bleed: unavailable baseline %+v got %+v", unavailable, resp)
				}
			case 4: // cross-tenant target under an elevated caller
				resp, err = preview.CompositionPreview(
					previewCtx(t, previewAdmin, []auth.Scope{auth.ScopeAdmin}, nil, []string{testAgentID, "agent-retired"}),
					agentcfgprotocol.CompositionPreviewRequest{TenantID: "other-tenant", UserID: "ua", SessionID: "sa", AgentID: testAgentID},
				)
				if err == nil && !reflect.DeepEqual(resp, crossTenant) {
					err = fmt.Errorf("case 4 context bleed: cross-tenant baseline %+v got %+v", crossTenant, resp)
				}
			case 5: // retired effective agent — typed retired outcome
				resp, err = preview.CompositionPreview(
					previewCtx(t, previewUserA, nil, nil, []string{testAgentID, "agent-retired"}),
					previewReq("ua", "sa", "agent-retired"),
				)
				if err == nil && !reflect.DeepEqual(resp, retired) {
					err = fmt.Errorf("case 5 context bleed: retired baseline %+v got %+v", retired, resp)
				}
			}
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent preview failed: %v", err)
	}
	if want := auditBaseline + n/6; len(bus.adminEvents()) != want {
		t.Fatalf("admin audits=%d want %d (baseline %d + one per elevated widened preview)",
			len(bus.adminEvents()), want, auditBaseline)
	}

	// Goroutine baseline restored — the preview path spawns no goroutines.
	var after int
	for range 50 {
		after = runtime.NumGoroutine()
		if after <= baseline+2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if after > baseline+2 {
		t.Errorf("goroutine leak: baseline=%d after=%d", baseline, after)
	}
}
