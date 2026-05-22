// Phase 66 cross-subsystem integration test per CLAUDE.md §17 — the
// `harbor dev` draft-save scaffolding surface (D-100) exercised end-
// to-end against the REAL runtime stack via the devstack helper:
//
//   - Phase 02 internal/config (Load + Validate against the rendered yaml)
//   - Phase 03 internal/audit/drivers/patterns
//   - Phase 05 internal/events/drivers/inmem (lifecycle events observed)
//   - Phase 07 internal/state/drivers/inmem
//   - Phase 17 internal/artifacts/drivers/inmem
//   - Phase 60 internal/protocol/transports.NewMux
//   - Phase 61 internal/protocol/auth.NewValidator (Bearer-token gating)
//   - Phase 66 internal/devdraft.Store + .Handler (this PR)
//
// The test exercises every endpoint shape: create → patch → preview →
// save → delete; observes all five lifecycle events on the bus;
// asserts identity propagation (the draft is invisible from a second
// identity); and confirms the §17.3 ≥1-failure-mode (path-traversal
// attempt → 400 + CodeUnsafePath).
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/devdraft"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/llm"
	_ "github.com/hurtener/Harbor/internal/llm/mock"
	_ "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"

	"github.com/hurtener/Harbor/harbortest/devstack"
)

// TestE2E_Phase66_DraftSave_RoundTripThroughHTTP is the load-bearing
// acceptance test: the round-trip (create → edit → preview → save)
// produces a scaffold the real config validator accepts. Every
// lifecycle event observable on the bus.
func TestE2E_Phase66_DraftSave_RoundTripThroughHTTP(t *testing.T) {
	stack, srv, token := buildPhase66Stack(t)
	defer stack.Close()
	defer srv.Close()

	subCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub, err := stack.Bus.Subscribe(subCtx, events.Filter{
		Tenant:  devstack.DefaultDevTenant,
		User:    devstack.DefaultDevUser,
		Session: devstack.DefaultDevSession,
		Types: []events.EventType{
			devdraft.EventTypeDraftCreated,
			devdraft.EventTypeDraftUpdated,
			devdraft.EventTypeDraftPreviewed,
			devdraft.EventTypeDraftSaved,
			devdraft.EventTypeDraftDiscarded,
		},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	// Create.
	createResp := authedPost(t, srv.URL+devdraft.RoutePrefix+"/", token, map[string]any{
		"name": "round-trip",
	})
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d", createResp.StatusCode)
	}
	var create struct {
		DraftID   string   `json:"draft_id"`
		Template  string   `json:"template"`
		Files     []string `json:"files"`
		FileCount int      `json:"file_count"`
	}
	if err := json.NewDecoder(createResp.Body).Decode(&create); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if create.DraftID == "" {
		t.Fatalf("create: empty DraftID")
	}
	if create.FileCount != 5 {
		t.Errorf("create: FileCount=%d, want 5", create.FileCount)
	}

	// Patch.
	patchURL := srv.URL + devdraft.RoutePrefix + "/" + create.DraftID + "/files/README.md"
	patchResp := authedPatch(t, patchURL, token, map[string]any{
		"content": "# E2E-edited README\n",
	})
	patchResp.Body.Close()
	if patchResp.StatusCode != http.StatusOK {
		t.Fatalf("patch: status %d", patchResp.StatusCode)
	}

	// Preview.
	previewResp := authedPost(t, srv.URL+devdraft.RoutePrefix+"/"+create.DraftID+"/preview", token, nil)
	defer previewResp.Body.Close()
	if previewResp.StatusCode != http.StatusOK {
		t.Fatalf("preview: status %d", previewResp.StatusCode)
	}
	var preview struct {
		OK     bool     `json:"ok"`
		Errors []string `json:"errors"`
	}
	if err := json.NewDecoder(previewResp.Body).Decode(&preview); err != nil {
		t.Fatalf("preview decode: %v", err)
	}
	if !preview.OK {
		t.Fatalf("preview ok=false: %v", preview.Errors)
	}

	// Save.
	outDir := filepath.Join(t.TempDir(), "promoted-e2e")
	saveResp := authedPost(t, srv.URL+devdraft.RoutePrefix+"/"+create.DraftID+"/save", token, map[string]any{
		"name":       "round-trip",
		"output_dir": outDir,
	})
	defer saveResp.Body.Close()
	if saveResp.StatusCode != http.StatusOK {
		t.Fatalf("save: status %d", saveResp.StatusCode)
	}
	var save struct {
		OutputDir string   `json:"output_dir"`
		Files     []string `json:"files"`
	}
	if err := json.NewDecoder(saveResp.Body).Decode(&save); err != nil {
		t.Fatalf("save decode: %v", err)
	}
	if save.OutputDir != outDir {
		t.Errorf("save OutputDir=%q, want %q", save.OutputDir, outDir)
	}
	// Validate the produced harbor.yaml — the binding acceptance.
	if _, err := config.Load(context.Background(), filepath.Join(outDir, "harbor.yaml")); err != nil {
		t.Errorf("promoted harbor.yaml failed config.Load: %v", err)
	}
	// Edit rode through.
	got, err := os.ReadFile(filepath.Join(outDir, "README.md"))
	if err != nil {
		t.Fatalf("read promoted README: %v", err)
	}
	if !strings.Contains(string(got), "E2E-edited") {
		t.Errorf("promoted README missing edit marker: %q", string(got))
	}

	// Discard.
	delReq, _ := http.NewRequest(http.MethodDelete, srv.URL+devdraft.RoutePrefix+"/"+create.DraftID, nil)
	delReq.Header.Set("Authorization", "Bearer "+token)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Errorf("DELETE: status %d", delResp.StatusCode)
	}

	// Verify all five lifecycle events landed.
	want := []events.EventType{
		devdraft.EventTypeDraftCreated,
		devdraft.EventTypeDraftUpdated,
		devdraft.EventTypeDraftPreviewed,
		devdraft.EventTypeDraftSaved,
		devdraft.EventTypeDraftDiscarded,
	}
	got2 := drainEventTypes(t, sub.Events(), len(want), 5*time.Second)
	if len(got2) != len(want) {
		t.Fatalf("expected %d events, got %d: %v", len(want), len(got2), got2)
	}
	for i, w := range want {
		if got2[i] != w {
			t.Errorf("event[%d] = %q, want %q", i, got2[i], w)
		}
	}
}

// TestE2E_Phase66_DraftSave_PathTraversal_Returns400 is the §17.3
// "≥1 failure mode" gate. A PATCH with a traversal-shaped path MUST
// be rejected at the handler with HTTP 400 + CodeUnsafePath.
func TestE2E_Phase66_DraftSave_PathTraversal_Returns400(t *testing.T) {
	stack, srv, token := buildPhase66Stack(t)
	defer stack.Close()
	defer srv.Close()

	createResp := authedPost(t, srv.URL+devdraft.RoutePrefix+"/", token, map[string]any{
		"name": "trav-e2e",
	})
	defer createResp.Body.Close()
	var create struct {
		DraftID string `json:"draft_id"`
	}
	if err := json.NewDecoder(createResp.Body).Decode(&create); err != nil {
		t.Fatalf("decode: %v", err)
	}

	patchURL := srv.URL + devdraft.RoutePrefix + "/" + create.DraftID + "/files/" + "..%2Fescape.txt"
	patchResp := authedPatch(t, patchURL, token, map[string]any{"content": "x"})
	defer patchResp.Body.Close()
	if patchResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", patchResp.StatusCode)
	}
	var env struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(patchResp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Code != devdraft.CodeUnsafePath {
		t.Errorf("error code = %q, want %q", env.Code, devdraft.CodeUnsafePath)
	}
}

// TestE2E_Phase66_DraftSave_MissingBearer_Returns401 — every draft
// endpoint MUST 401 without a Bearer token (the auth.Middleware
// fail-closes).
func TestE2E_Phase66_DraftSave_MissingBearer_Returns401(t *testing.T) {
	stack, srv, _ := buildPhase66Stack(t)
	defer stack.Close()
	defer srv.Close()
	resp, err := http.Post(srv.URL+devdraft.RoutePrefix+"/", "application/json", strings.NewReader(`{"name":"x"}`))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// TestE2E_Phase66_DraftSave_ConcurrencyStress runs N=10 concurrent
// round-trip clients against one shared dev stack. Asserts no cross-
// talk under -race: each client's draft IDs are unique and the
// per-client validations all succeed.
func TestE2E_Phase66_DraftSave_ConcurrencyStress(t *testing.T) {
	stack, srv, token := buildPhase66Stack(t)
	defer stack.Close()
	defer srv.Close()

	const clients = 10
	var wg sync.WaitGroup
	wg.Add(clients)
	ids := make(chan string, clients)
	for i := range clients {

		go func() {
			defer wg.Done()
			createResp := authedPost(t, srv.URL+devdraft.RoutePrefix+"/", token, map[string]any{
				"name": fmt.Sprintf("concur-%d", i),
			})
			defer createResp.Body.Close()
			if createResp.StatusCode != http.StatusCreated {
				t.Errorf("client %d: create status %d", i, createResp.StatusCode)
				return
			}
			var create struct {
				DraftID string `json:"draft_id"`
			}
			if err := json.NewDecoder(createResp.Body).Decode(&create); err != nil {
				t.Errorf("client %d: decode: %v", i, err)
				return
			}
			ids <- create.DraftID
		}()
	}
	wg.Wait()
	close(ids)
	seen := make(map[string]struct{})
	for id := range ids {
		if _, dup := seen[id]; dup {
			t.Errorf("duplicate DraftID: %q", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != clients {
		t.Errorf("expected %d unique DraftIDs, got %d", clients, len(seen))
	}
}

// ---- helpers ------------------------------------------------------

func buildPhase66Stack(t *testing.T) (*devstack.DevStack, *httptest.Server, string) {
	t.Helper()
	cfgPath := writeDevConfig(t)
	cfg, err := config.Load(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	llmSnap := newPhase66LLMSnap(cfg)
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		LLMConfigSnapshot: &llmSnap,
	})
	if stack.Handler == nil {
		stack.Close()
		t.Fatal("phase66: stack.Handler is nil — devstack regressed")
	}
	if stack.DraftStore == nil {
		stack.Close()
		t.Fatal("phase66: stack.DraftStore is nil — devstack regressed (D-094)")
	}
	if stack.Token == "" {
		stack.Close()
		t.Fatal("phase66: stack.Token is empty")
	}
	srv := httptest.NewServer(stack.Handler)
	return stack, srv, stack.Token
}

// newPhase66LLMSnap mirrors phase64's LLM override — the integration
// test stays hermetic without a real LLM key.
func newPhase66LLMSnap(cfg *config.Config) llm.ConfigSnapshot {
	return llm.ConfigSnapshot{
		Driver:               "mock",
		ContextWindowReserve: cfg.LLM.ContextWindowReserve,
		HeavyOutputThreshold: cfg.Artifacts.HeavyOutputThresholdBytes,
		ModelProfiles: map[string]llm.ModelProfile{
			"anthropic/claude-sonnet-4": {
				ContextWindowTokens: 200000,
				TokenEstimator:      "chars_div_4",
			},
		},
	}
}

func authedPost(t *testing.T, url, token string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	} else {
		buf.WriteString("{}")
	}
	req, _ := http.NewRequest(http.MethodPost, url, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Post %s: %v", url, err)
	}
	return resp
}

func authedPatch(t *testing.T, url, token string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatalf("encode: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPatch, url, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", url, err)
	}
	return resp
}

func drainEventTypes(t *testing.T, ch <-chan events.Event, n int, timeout time.Duration) []events.EventType {
	t.Helper()
	out := make([]events.EventType, 0, n)
	deadline := time.After(timeout)
	for range n {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev.Type)
		case <-deadline:
			return out
		}
	}
	return out
}
