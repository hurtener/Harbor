// Phase 73f cross-subsystem integration test per CLAUDE.md §17 — the
// seven `tools.*` Protocol methods exercised end-to-end against the
// real wire transport + the real tools.ToolCatalog + the real
// auth.Validator/Middleware (Phase 61), with no mocks at any seam.
//
// Surfaces composed:
//
//   - Phase 26 internal/tools — the real in-memory ToolCatalog whose
//     descriptors the `tools.*` methods project.
//   - Phase 73f internal/tools/protocol — the Tools Protocol Service +
//     CatalogProjector.
//   - Phase 60 internal/protocol/transports — the wire surface the
//     `tools.*` handler is mounted on.
//   - Phase 61 internal/protocol/auth — the JWT validator + middleware
//     gating the `auth.ScopeAdmin` claim on the two admin methods
//     (D-079).
//
// This test ships the §13 primitive-with-consumer discharge for Phase
// 73f's Go-side surface: it is the first end-to-end consumer of the
// `tools.*` wire methods — catalog list + facet filter + drill-down +
// the admin-scope reject / accept paths + identity propagation across
// two tenants.
package integration_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
	"github.com/hurtener/Harbor/internal/tools"
	toolsprotocol "github.com/hurtener/Harbor/internal/tools/protocol"
)

const phase73fKid = "phase73f-kid"

var fixedNowPhase73f = time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

type phase73fDeps struct {
	mux     *http.ServeMux
	priv    *ecdsa.PrivateKey
	cleanup func()
}

func newPhase73fDeps(t *testing.T) *phase73fDeps {
	t.Helper()

	priv, pub := loadES256Phase61(t)
	red := auditpatterns.New()

	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     512,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         512,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("state.Open: %v", err)
	}
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: audit.Redactor(red),
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("tasks.Open: %v", err)
	}
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}

	// Real in-memory ToolCatalog seeded with three tools spanning the
	// transport axis the facet filter branches on.
	cat := tools.NewCatalog()
	for _, spec := range []struct {
		name      string
		transport tools.TransportKind
		se        tools.SideEffect
	}{
		{"calc", tools.TransportInProcess, tools.SideEffectPure},
		{"web_fetch", tools.TransportHTTP, tools.SideEffectExternal},
		{"git_diff", tools.TransportMCP, tools.SideEffectRead},
	} {
		if regErr := cat.Register(tools.ToolDescriptor{
			Tool: tools.Tool{
				Name:      spec.name,
				Transport: spec.transport,
				// SideEffects drives the reliability-tier facet.
				SideEffects: spec.se,
				Loading:     tools.LoadingAlways,
			},
			Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{}, nil
			},
		}); regErr != nil {
			t.Fatalf("catalog Register %q: %v", spec.name, regErr)
		}
	}

	projector, err := toolsprotocol.NewCatalogProjector(cat)
	if err != nil {
		t.Fatalf("NewCatalogProjector: %v", err)
	}
	toolsSvc, err := toolsprotocol.NewService(projector,
		toolsprotocol.WithBus(bus),
		toolsprotocol.WithRedactor(red),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	keys := newES256KeySet(phase73fKid, pub)
	now := func() time.Time { return fixedNowPhase73f }
	v, err := auth.NewValidator(keys, auth.WithClock(now), auth.WithRedactor(red))
	if err != nil {
		t.Fatalf("auth.NewValidator: %v", err)
	}

	mux, err := transports.NewMux(surface, bus,
		transports.WithValidator(v),
		transports.WithToolsService(toolsSvc),
	)
	if err != nil {
		t.Fatalf("transports.NewMux: %v", err)
	}

	return &phase73fDeps{
		mux:  mux,
		priv: priv,
		cleanup: func() {
			_ = taskReg.Close(context.Background())
			_ = store.Close(context.Background())
			_ = bus.Close(context.Background())
		},
	}
}

// phase73fClaims mints a JWT MapClaims with the test's standard shape.
func phase73fClaims(id identity.Identity, scopes []string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":     "https://idp.test",
		"sub":     id.UserID,
		"exp":     fixedNowPhase73f.Add(15 * time.Minute).Unix(),
		"nbf":     fixedNowPhase73f.Add(-1 * time.Minute).Unix(),
		"tenant":  id.TenantID,
		"user":    id.UserID,
		"session": id.SessionID,
		"scopes":  scopes,
	}
}

// postTools issues a POST /v1/tools/{verb} with the supplied JWT.
func postTools(t *testing.T, srvURL, verb, body, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srvURL+"/v1/tools/"+verb, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// TestE2E_Phase73f_ToolsPage is the §13 primitive-with-consumer binding
// test for the Tools-page Protocol surface. It exercises the catalog
// list + facet filter + drill-down + the admin reject / accept paths +
// cross-tenant identity propagation, all at the wire boundary with real
// drivers.
func TestE2E_Phase73f_ToolsPage(t *testing.T) {
	deps := newPhase73fDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	idA := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-A"}
	idB := identity.Identity{TenantID: "tenant-B", UserID: "u-B", SessionID: "s-B"}
	tokA := signES256Wave10(t, deps.priv, phase73fClaims(idA, nil), phase73fKid)
	tokAdmin := signES256Wave10(t, deps.priv, phase73fClaims(idA, []string{"admin"}), phase73fKid)
	tokB := signES256Wave10(t, deps.priv, phase73fClaims(idB, nil), phase73fKid)

	// (1) tools.list returns the full catalog for tenant A.
	status, body := postTools(t, srv.URL, "list", `{}`, tokA)
	if status != http.StatusOK {
		t.Fatalf("tools.list: status = %d, want 200; body=%s", status, body)
	}
	var listResp prototypes.ToolListResponse
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatalf("decode tools.list: %v", err)
	}
	if listResp.Aggregates.Total != 3 || len(listResp.Tools) != 3 {
		t.Fatalf("tools.list: Total=%d len=%d, want 3/3", listResp.Aggregates.Total, len(listResp.Tools))
	}

	// (2) tools.list with the MCP transport facet narrows to one row.
	status, body = postTools(t, srv.URL, "list", `{"filter":{"transports":["MCP"]}}`, tokA)
	if status != http.StatusOK {
		t.Fatalf("tools.list facet: status = %d, want 200; body=%s", status, body)
	}
	var facetResp prototypes.ToolListResponse
	_ = json.Unmarshal(body, &facetResp)
	if len(facetResp.Tools) != 1 || facetResp.Tools[0].Name != "git_diff" {
		t.Fatalf("MCP facet returned %d rows, want 1 (git_diff)", len(facetResp.Tools))
	}

	// (3) tools.get drill-down on a single tool.
	status, body = postTools(t, srv.URL, "get", `{"id":"web_fetch"}`, tokA)
	if status != http.StatusOK {
		t.Fatalf("tools.get: status = %d, want 200; body=%s", status, body)
	}
	var getResp prototypes.Tool
	_ = json.Unmarshal(body, &getResp)
	if getResp.Transport != prototypes.ToolTransportHTTP {
		t.Errorf("tools.get transport = %q, want HTTP", getResp.Transport)
	}

	// (4) tools.describe / metrics / content_stats round-trip.
	for _, verb := range []string{"describe", "metrics", "content_stats"} {
		status, body = postTools(t, srv.URL, verb, `{"id":"calc"}`, tokA)
		if status != http.StatusOK {
			t.Fatalf("tools.%s: status = %d, want 200; body=%s", verb, status, body)
		}
	}

	// (5) tools.set_approval_policy WITHOUT the admin scope claim → 403.
	status, body = postTools(t, srv.URL, "set_approval_policy",
		`{"id":"calc","policy":"gated"}`, tokA)
	if status != http.StatusForbidden {
		t.Fatalf("set_approval_policy no-admin: status = %d, want 403; body=%s", status, body)
	}
	var perr protoerrors.Error
	if err := json.Unmarshal(body, &perr); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if perr.Code != protoerrors.CodeIdentityScopeRequired {
		t.Errorf("set_approval_policy no-admin: Code = %q, want identity_scope_required", perr.Code)
	}

	// (6) tools.set_approval_policy WITH the admin scope claim → 200,
	// and the change is observable on the next tools.get.
	status, body = postTools(t, srv.URL, "set_approval_policy",
		`{"id":"calc","policy":"gated"}`, tokAdmin)
	if status != http.StatusOK {
		t.Fatalf("set_approval_policy admin: status = %d, want 200; body=%s", status, body)
	}
	status, body = postTools(t, srv.URL, "get", `{"id":"calc"}`, tokAdmin)
	if status != http.StatusOK {
		t.Fatalf("tools.get after policy update: status = %d, want 200", status)
	}
	var updated prototypes.Tool
	_ = json.Unmarshal(body, &updated)
	if updated.ApprovalPolicy != prototypes.ToolApprovalGated {
		t.Errorf("post-update ApprovalPolicy = %q, want gated", updated.ApprovalPolicy)
	}

	// (7) Failure mode — tools.get for an unknown tool → 404.
	status, _ = postTools(t, srv.URL, "get", `{"id":"nonexistent"}`, tokA)
	if status != http.StatusNotFound {
		t.Errorf("tools.get unknown: status = %d, want 404", status)
	}

	// (8) Identity propagation — tenant B sees the same per-runtime
	// catalog (tool descriptors are tenant-agnostic, page-tools.md §8),
	// and an unauthenticated request is rejected at the wire edge.
	status, _ = postTools(t, srv.URL, "list", `{}`, tokB)
	if status != http.StatusOK {
		t.Errorf("tenant B tools.list: status = %d, want 200", status)
	}
	noAuthReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/tools/list", strings.NewReader(`{}`))
	noAuthResp, err := http.DefaultClient.Do(noAuthReq)
	if err != nil {
		t.Fatalf("no-auth request: %v", err)
	}
	_ = noAuthResp.Body.Close()
	if noAuthResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-auth tools.list: status = %d, want 401", noAuthResp.StatusCode)
	}
}

// TestE2E_Phase73f_ToolsConcurrencyStress runs N concurrent tools.list
// requests across multiple tenants against a single shared mux,
// asserting no cross-talk and no goroutine leak after teardown (CLAUDE.md
// §17.3 cross-package concurrency stress).
func TestE2E_Phase73f_ToolsConcurrencyStress(t *testing.T) {
	deps := newPhase73fDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	const workers = 24
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	wg.Add(workers)
	errCh := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func(n int) {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  "tenant-" + string(rune('A'+(n%5))),
				UserID:    "u",
				SessionID: "s",
			}
			tok := signES256Wave10(t, deps.priv, phase73fClaims(id, nil), phase73fKid)
			status, body := postTools(t, srv.URL, "list", `{}`, tok)
			if status != http.StatusOK {
				errCh <- &stressErr{n, status, string(body)}
				return
			}
			var resp prototypes.ToolListResponse
			if err := json.Unmarshal(body, &resp); err != nil {
				errCh <- &stressErr{n, status, "decode: " + err.Error()}
				return
			}
			if len(resp.Tools) != 3 {
				errCh <- &stressErr{n, status, "row-count cross-talk"}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	// Close keep-alive connections so the httptest server's per-conn
	// goroutines are reaped before the leak measurement.
	http.DefaultClient.CloseIdleConnections()
	for attempt := 0; attempt < 50; attempt++ {
		if runtime.NumGoroutine() <= baseline+8 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutine leak: NumGoroutine=%d, baseline=%d", runtime.NumGoroutine(), baseline)
}

// stressErr carries a concurrency-stress failure for deferred reporting.
type stressErr struct {
	worker int
	status int
	detail string
}

func (e *stressErr) Error() string {
	return "worker " + string(rune('0'+e.worker%10)) + ": status " +
		http.StatusText(e.status) + " — " + e.detail
}
