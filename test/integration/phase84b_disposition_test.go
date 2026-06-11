// Phase 84b (D-189) — attachment disposition policy, end-to-end.
//
// Real drivers on every seam (§17.3): real inmem artifact store +
// task registry + bus + steering RunLoop + dispatch executor + real
// ReAct planner, assembled through `harbortest/devstack.Assemble`,
// driven over the REAL wire transport (Phase 61 auth + control
// handler). The scripted LLM is the only stub (it records the
// CompleteRequest so the test asserts what the materializer actually
// emitted) — the Phase 110b parity-test precedent.
//
// Covered:
//
//  1. A wire `start` carrying `input_artifact_dispositions` round-trips:
//     the hint persists onto the task and `tasks.get` reflects it on
//     `input_artifacts[].disposition`.
//  2. The materializer consumed the policy: a `tool:<name>` hint forces
//     `Fetch.Tool` on the emitted ArtifactStub (PartText), instead of
//     the inline/native part the default would have produced.
//  3. The run loop emitted one `task.input_disposition.resolved` event
//     per attachment with the winning layer; degradations (unknown
//     tool, pre-84c provider_native from the agent policy map) ride as
//     typed facts on the event — never a silent drop (§13).
//  4. Failure mode (§17.3 #3): the Protocol edge rejects a non-grammar
//     disposition value with CodeInvalidRequest (HTTP 400).
//  5. Concurrency stress: N≥10 concurrent spawns with per-task hints —
//     each task's first-turn prompt carries ITS OWN artifact ref only
//     (no cross-talk) under -race.
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
)

// phase84bRecordingLLM answers immediately and records every
// CompleteRequest so the test can assert the materialized first-turn
// content parts.
type phase84bRecordingLLM struct {
	mu   sync.Mutex
	reqs []llm.CompleteRequest
}

func (c *phase84bRecordingLLM) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	c.mu.Lock()
	c.reqs = append(c.reqs, req)
	c.mu.Unlock()
	return llm.CompleteResponse{Content: "disposition test answer"}, nil
}

func (c *phase84bRecordingLLM) Close(_ context.Context) error { return nil }

func (c *phase84bRecordingLLM) requests() []llm.CompleteRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]llm.CompleteRequest, len(c.reqs))
	copy(out, c.reqs)
	return out
}

// phase84bUserParts extracts the first RoleUser message's parts from a
// recorded request ("" Text-only user turns return nil).
func phase84bUserParts(req llm.CompleteRequest) []llm.ContentPart {
	for _, m := range req.Messages {
		if m.Role == llm.RoleUser {
			return m.Content.Parts
		}
	}
	return nil
}

func phase84bStack(t *testing.T) (*devstack.DevStack, *phase84bRecordingLLM, identity.Identity, context.Context) {
	t.Helper()
	rec := &phase84bRecordingLLM{}
	cfg := phase110bConfig(t)
	// The agent-policy layer under test: images opt in to
	// provider_native, which MUST degrade to ref (84c not shipped)
	// with a typed fact — the §13 honest-degradation seam.
	cfg.Multimodal = config.MultimodalConfig{
		Disposition: map[string]string{"image/*": "provider_native"},
	}
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		LLMConfigSnapshot: phase110bLLMSnapshot(cfg),
		PlannerOverride:   react.New(rec),
		PreRegisterTools: []tools.ToolDescriptor{{
			Tool: tools.Tool{
				Name:        "pdf.extract",
				Description: "extract text from a pdf (phase 84b test)",
				Transport:   tools.TransportInProcess,
				Source:      "phase84b-test-source",
			},
			Invoke: func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{Value: string(args)}, nil
			},
		}},
	})
	t.Cleanup(stack.Close)

	devID := identity.Identity{
		TenantID:  devstack.DefaultDevTenant,
		UserID:    devstack.DefaultDevUser,
		SessionID: devstack.DefaultDevSession,
	}
	idCtx, err := identity.With(context.Background(), devID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return stack, rec, devID, idCtx
}

func phase84bPut(t *testing.T, store artifacts.ArtifactStore, idCtx context.Context, devID identity.Identity, payload []byte, mime, filename string) string {
	t.Helper()
	scope := artifacts.ArtifactScope{TenantID: devID.TenantID, UserID: devID.UserID, SessionID: devID.SessionID}
	ref, err := store.PutBytes(idCtx, scope, payload, artifacts.PutOpts{MimeType: mime, Filename: filename})
	if err != nil {
		t.Fatalf("PutBytes(%s): %v", filename, err)
	}
	return ref.ID
}

func phase84bWaitTerminal(t *testing.T, stack *devstack.DevStack, idCtx context.Context, id tasks.TaskID) *tasks.Task {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		got, err := stack.Tasks.Get(idCtx, id)
		if err == nil && (got.Status == tasks.StatusComplete || got.Status == tasks.StatusFailed) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s never reached a terminal status", id)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestE2E_Phase84b_DispositionHint_RoundTrip_ForcedTool_Degradations
// is the headline gate — one wire-started task with three attachments
// exercising the caller-hint layer (known tool), the unknown-tool
// degradation, and the agent-policy provider_native degradation.
func TestE2E_Phase84b_DispositionHint_RoundTrip_ForcedTool_Degradations(t *testing.T) {
	t.Parallel()
	stack, rec, devID, idCtx := phase84bStack(t)
	if stack.Handler == nil || stack.Token == "" {
		t.Fatal("stack.Handler / stack.Token missing — the wire surface is not assembled")
	}
	srv := httptest.NewServer(stack.Handler)
	defer srv.Close()

	pdfA := phase84bPut(t, stack.Artifacts, idCtx, devID, []byte("%PDF-1.4 A"), "application/pdf", "a.pdf")
	pdfB := phase84bPut(t, stack.Artifacts, idCtx, devID, []byte("%PDF-1.4 B"), "application/pdf", "b.pdf")
	img := phase84bPut(t, stack.Artifacts, idCtx, devID, []byte{0x89, 0x50, 0x4E, 0x47}, "image/png", "c.png")

	// Subscribe BEFORE starting so no resolution event is missed.
	// Triple-scoped (not admin) — seeing the events under the dev
	// triple IS the identity-propagation assertion.
	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	sub, err := stack.Bus.Subscribe(subCtx, events.Filter{
		Tenant:  devID.TenantID,
		User:    devID.UserID,
		Session: devID.SessionID,
		Types:   []events.EventType{runctx.EventTypeInputDispositionResolved},
	})
	if err != nil {
		t.Fatalf("Bus.Subscribe: %v", err)
	}

	wireCall := func(path string, body string) (*http.Response, string) {
		req, rErr := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+path, bytes.NewBufferString(body))
		if rErr != nil {
			t.Fatalf("NewRequest(%s): %v", path, rErr)
		}
		req.Header.Set("Authorization", "Bearer "+stack.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, dErr := srv.Client().Do(req)
		if dErr != nil {
			t.Fatalf("POST %s: %v", path, dErr)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp, string(raw)
	}

	// 1. Wire start with the three hints/policy combinations.
	startBody := fmt.Sprintf(`{
		"identity": {"tenant": %q, "user": %q, "session": %q},
		"query": "summarise the attachments",
		"input_artifact_ids": [%q, %q, %q],
		"input_artifact_dispositions": {%q: "tool:pdf.extract", %q: "tool:no.such.tool"}
	}`, devID.TenantID, devID.UserID, devID.SessionID, pdfA, pdfB, img, pdfA, pdfB)
	resp, raw := wireCall("/v1/control/start", startBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start status = %d, body: %s", resp.StatusCode, raw)
	}
	var startResp struct {
		TaskID string `json:"task_id"`
	}
	if uErr := json.Unmarshal([]byte(raw), &startResp); uErr != nil || startResp.TaskID == "" {
		t.Fatalf("start response unparsable: %v / %s", uErr, raw)
	}
	taskID := tasks.TaskID(startResp.TaskID)
	task := phase84bWaitTerminal(t, stack, idCtx, taskID)
	if task.Status != tasks.StatusComplete {
		t.Fatalf("task status = %s, want complete (err=%+v)", task.Status, task.Error)
	}

	// 2. The materializer consumed the policy. Inspect the FIRST
	//    recorded LLM request's user-message parts.
	reqs := rec.requests()
	if len(reqs) == 0 {
		t.Fatal("the scripted LLM recorded no requests")
	}
	parts := phase84bUserParts(reqs[0])
	if len(parts) != 4 { // goal text + 3 attachments
		t.Fatalf("first-turn user parts = %d, want 4: %+v", len(parts), parts)
	}
	// pdfA — forced tool: a PartText ArtifactStub carrying Fetch.Tool.
	if parts[1].Type != llm.PartText || !strings.Contains(parts[1].Text, `"tool":"pdf.extract"`) ||
		!strings.Contains(parts[1].Text, fmt.Sprintf(`"artifact_ref":%q`, pdfA)) {
		t.Fatalf("pdfA part is not the forced-tool stub: %+v", parts[1])
	}
	// pdfB — unknown tool degraded to ref: the typed FilePart stub
	// WITHOUT the bogus tool name anywhere.
	if parts[2].Type != llm.PartFile || parts[2].File == nil || parts[2].File.Artifact == nil {
		t.Fatalf("pdfB part is not the ref FilePart degradation: %+v", parts[2])
	}
	if parts[2].File.Artifact.Fetch != nil && parts[2].File.Artifact.Fetch.Tool == "no.such.tool" {
		t.Fatal("unknown tool name leaked onto the degraded stub")
	}
	// img — agent-policy provider_native degraded to ref (84c not
	// shipped): a stub-text part, NOT an inline ImagePart.
	if parts[3].Type != llm.PartText || !strings.Contains(parts[3].Text, fmt.Sprintf(`"artifact_ref":%q`, img)) {
		t.Fatalf("image part is not the ref stub degradation: %+v", parts[3])
	}

	// 3. The resolution events: one per attachment, with the winning
	//    layer + typed degradation facts.
	got := map[string]runctx.InputDispositionResolvedPayload{}
	deadline := time.After(10 * time.Second)
	for len(got) < 3 {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription closed before all resolution events arrived")
			}
			payload, pOK := ev.Payload.(runctx.InputDispositionResolvedPayload)
			if !pOK {
				t.Fatalf("payload type = %T", ev.Payload)
			}
			if payload.TaskID == string(taskID) {
				got[payload.ArtifactID] = payload
			}
		case <-deadline:
			t.Fatalf("timed out waiting for resolution events; got %d/3", len(got))
		}
	}
	a := got[pdfA]
	if a.Disposition != "tool:pdf.extract" || a.Layer != string(planner.DispositionLayerCallerHint) || a.Degraded {
		t.Fatalf("pdfA resolution = %+v, want honoured caller_hint tool", a)
	}
	b := got[pdfB]
	if b.Disposition != "ref" || !b.Degraded || b.DegradationReason != string(planner.DegradationUnknownTool) || b.Tool != "no.such.tool" {
		t.Fatalf("pdfB resolution = %+v, want unknown_tool degradation to ref", b)
	}
	c := got[img]
	if c.Disposition != "ref" || c.Layer != string(planner.DispositionLayerAgentPolicy) || !c.Degraded ||
		c.DegradationReason != string(planner.DegradationProviderNativeUnavailable) {
		t.Fatalf("img resolution = %+v, want provider_native_unavailable degradation at agent_policy", c)
	}

	// 4. tasks.get reflects the hints on the wire.
	getBody := fmt.Sprintf(`{"identity": {"tenant": %q, "user": %q, "session": %q}, "id": %q}`,
		devID.TenantID, devID.UserID, devID.SessionID, taskID)
	resp, raw = wireCall("/v1/tasks/get", getBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tasks.get status = %d, body: %s", resp.StatusCode, raw)
	}
	var detail struct {
		InputArtifacts []struct {
			ID          string `json:"id"`
			Disposition string `json:"disposition"`
		} `json:"input_artifacts"`
	}
	if uErr := json.Unmarshal([]byte(raw), &detail); uErr != nil {
		t.Fatalf("tasks.get response unparsable: %v / %s", uErr, raw)
	}
	if len(detail.InputArtifacts) != 3 {
		t.Fatalf("tasks.get input_artifacts = %d entries, want 3: %s", len(detail.InputArtifacts), raw)
	}
	wantDisp := map[string]string{pdfA: "tool:pdf.extract", pdfB: "tool:no.such.tool", img: ""}
	for i, ia := range detail.InputArtifacts {
		if want, known := wantDisp[ia.ID]; !known || ia.Disposition != want {
			t.Fatalf("input_artifacts[%d] = %+v, want disposition %q", i, ia, wantDisp[ia.ID])
		}
	}

	// 5. Failure mode (§17.3 #3): a non-grammar value fails closed.
	badBody := fmt.Sprintf(`{
		"identity": {"tenant": %q, "user": %q, "session": %q},
		"query": "bad hint",
		"input_artifact_ids": [%q],
		"input_artifact_dispositions": {%q: "bogus"}
	}`, devID.TenantID, devID.UserID, devID.SessionID, pdfA, pdfA)
	resp, raw = wireCall("/v1/control/start", badBody)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bogus disposition start status = %d (want 400), body: %s", resp.StatusCode, raw)
	}
	if !strings.Contains(raw, "invalid_request") {
		t.Fatalf("bogus disposition error body missing invalid_request: %s", raw)
	}
}

// TestE2E_Phase84b_ConcurrentDispositionResolution_NoCrossTalk runs
// N=12 concurrent spawns, each with its own artifact + forced-tool
// hint, and asserts every task's first-turn prompt carries ITS OWN
// artifact ref with the forced tool — no context bleed across runs
// (D-025 at the policy seam), under -race.
func TestE2E_Phase84b_ConcurrentDispositionResolution_NoCrossTalk(t *testing.T) {
	t.Parallel()
	const taskCount = 12
	stack, rec, devID, idCtx := phase84bStack(t)

	artByQuery := make(map[string]string, taskCount)
	ids := make([]tasks.TaskID, 0, taskCount)
	for i := range taskCount {
		query := fmt.Sprintf("disposition stress <%d>", i)
		artID := phase84bPut(t, stack.Artifacts, idCtx, devID,
			[]byte(fmt.Sprintf("%%PDF-1.4 stress %d", i)), "application/pdf", fmt.Sprintf("s%d.pdf", i))
		artByQuery[query] = artID
		h, err := stack.Tasks.Spawn(idCtx, tasks.SpawnRequest{
			Identity:                  identity.Quadruple{Identity: devID},
			Kind:                      tasks.KindForeground,
			Query:                     query,
			InputArtifactIDs:          []string{artID},
			InputArtifactDispositions: map[string]string{artID: "tool:pdf.extract"},
		})
		if err != nil {
			t.Fatalf("Spawn(%d): %v", i, err)
		}
		ids = append(ids, h.ID)
	}
	for _, id := range ids {
		task := phase84bWaitTerminal(t, stack, idCtx, id)
		if task.Status != tasks.StatusComplete {
			t.Fatalf("task %s status = %s (err=%+v)", id, task.Status, task.Error)
		}
	}

	// Pair each recorded request back to its task via the unique query
	// text and assert the prompt's stub names that task's artifact —
	// and ONLY that task's artifact.
	matched := 0
	for _, req := range rec.requests() {
		parts := phase84bUserParts(req)
		if len(parts) != 2 {
			continue // not a first-turn multimodal prompt
		}
		// The prompt builder prefixes the goal ("User goal: <query>");
		// match by containment against the unique query strings.
		query := ""
		for q := range artByQuery {
			if strings.Contains(parts[0].Text, q) {
				query = q
				break
			}
		}
		if query == "" {
			t.Fatalf("recorded request carries unknown query text %q", parts[0].Text)
		}
		wantArt := artByQuery[query]
		stubText := parts[1].Text
		if !strings.Contains(stubText, fmt.Sprintf(`"artifact_ref":%q`, wantArt)) ||
			!strings.Contains(stubText, `"tool":"pdf.extract"`) {
			t.Fatalf("task %q prompt stub drifted: %s", query, stubText)
		}
		for otherQuery, otherArt := range artByQuery {
			if otherQuery != query && strings.Contains(stubText, otherArt) {
				t.Fatalf("cross-talk: task %q prompt carries task %q's artifact", query, otherQuery)
			}
		}
		matched++
	}
	if matched < taskCount {
		t.Fatalf("matched %d first-turn prompts, want ≥ %d", matched, taskCount)
	}
}

// TestE2E_Phase84b_DefaultImageInline_ReachesLLMRequest pins the
// operator-reported small-image happy path end-to-end (the parked
// live bug's Go-side chain): a sub-threshold image attached with NO
// hint and NO policy must arrive at the LLM driver as an inline
// `ImagePart.DataURL` — vision-capable providers actually see it.
func TestE2E_Phase84b_DefaultImageInline_ReachesLLMRequest(t *testing.T) {
	t.Parallel()
	rec := &phase84bRecordingLLM{}
	cfg := phase110bConfig(t) // NO multimodal block — the runtime default
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		LLMConfigSnapshot: phase110bLLMSnapshot(cfg),
		PlannerOverride:   react.New(rec),
	})
	t.Cleanup(stack.Close)
	devID := identity.Identity{
		TenantID:  devstack.DefaultDevTenant,
		UserID:    devstack.DefaultDevUser,
		SessionID: devstack.DefaultDevSession,
	}
	idCtx, err := identity.With(context.Background(), devID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	imgID := phase84bPut(t, stack.Artifacts, idCtx, devID,
		[]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}, "image/png", "tiny.png")
	h, err := stack.Tasks.Spawn(idCtx, tasks.SpawnRequest{
		Identity:         identity.Quadruple{Identity: devID},
		Kind:             tasks.KindForeground,
		Query:            "what is in this image?",
		InputArtifactIDs: []string{imgID},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	task := phase84bWaitTerminal(t, stack, idCtx, h.ID)
	if task.Status != tasks.StatusComplete {
		t.Fatalf("task status = %s (err=%+v)", task.Status, task.Error)
	}
	reqs := rec.requests()
	if len(reqs) == 0 {
		t.Fatal("no LLM request recorded")
	}
	parts := phase84bUserParts(reqs[0])
	if len(parts) != 2 {
		t.Fatalf("first-turn user parts = %d, want 2 (goal + image): %+v", len(parts), parts)
	}
	if parts[1].Type != llm.PartImage || parts[1].Image == nil ||
		!strings.HasPrefix(parts[1].Image.DataURL, "data:image/png;base64,") {
		t.Fatalf("image did not reach the LLM request inline: %+v", parts[1])
	}
}
