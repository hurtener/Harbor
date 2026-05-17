package devdraft

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
)

// newTestHandler wires a Store + Handler against a fresh root +
// bus, returns an httptest.Server that injects testIdentity into
// every request's ctx (skipping the auth middleware entirely — the
// production handler is mounted behind auth.Middleware by `harbor
// dev`'s bootDevStack).
func newTestHandler(t *testing.T) (*Handler, *httptest.Server) {
	t.Helper()
	store := newTestStore(t)
	h, err := NewHandler(store, nil)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, err := identity.With(r.Context(), testIdentity)
		if err != nil {
			t.Fatalf("identity.With: %v", err)
		}
		h.ServeHTTP(w, r.WithContext(ctx))
	}))
	t.Cleanup(srv.Close)
	return h, srv
}

// TestHandler_Create_HappyPath pins the POST round-trip.
func TestHandler_Create_HappyPath(t *testing.T) {
	t.Parallel()
	_, srv := newTestHandler(t)
	body := bytes.NewBufferString(`{"name":"http-agent"}`)
	resp, err := http.Post(srv.URL+RoutePrefix+"/", "application/json", body)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var out createResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.DraftID == "" {
		t.Errorf("response missing DraftID")
	}
	if out.FileCount != 5 {
		t.Errorf("response FileCount = %d, want 5", out.FileCount)
	}
}

// TestHandler_MissingIdentity_Returns401 — when the test wraps the
// handler with NO identity middleware, every endpoint MUST 401.
func TestHandler_MissingIdentity_Returns401(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	h, err := NewHandler(store, nil)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := bytes.NewBufferString(`{"name":"agent"}`)
	resp, err := http.Post(srv.URL+RoutePrefix+"/", "application/json", body)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	var env errorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Code != CodeIdentityRequired {
		t.Errorf("error code = %q, want %q", env.Code, CodeIdentityRequired)
	}
}

// TestHandler_Patch_RoundTrip pins the PATCH path.
func TestHandler_Patch_RoundTrip(t *testing.T) {
	t.Parallel()
	_, srv := newTestHandler(t)

	// Create.
	create := postJSON(t, srv, RoutePrefix+"/", map[string]any{"name": "patch-agent"})
	var cr createResponse
	if err := json.Unmarshal(create, &cr); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Patch.
	patchURL := srv.URL + RoutePrefix + "/" + cr.DraftID + "/files/README.md"
	req, err := http.NewRequest(http.MethodPatch, patchURL, bytes.NewBufferString(`{"content":"hi"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200", resp.StatusCode)
	}
	var pr patchResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pr.Size != 2 {
		t.Errorf("Size = %d, want 2", pr.Size)
	}
}

// TestHandler_Patch_RejectsTraversal pins the §7 rule 5 surface on
// the wire — the handler MUST return 400 with CodeUnsafePath.
func TestHandler_Patch_RejectsTraversal(t *testing.T) {
	t.Parallel()
	_, srv := newTestHandler(t)

	create := postJSON(t, srv, RoutePrefix+"/", map[string]any{"name": "trav-agent"})
	var cr createResponse
	if err := json.Unmarshal(create, &cr); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Try to PATCH an escape path. The URL is encoded so the
	// handler routes correctly; the engine's resolveSafe catches it.
	patchURL := srv.URL + RoutePrefix + "/" + cr.DraftID + "/files/" + "..%2Fescape.txt"
	req, _ := http.NewRequest(http.MethodPatch, patchURL, bytes.NewBufferString(`{"content":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var env errorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Code != CodeUnsafePath {
		t.Errorf("error code = %q, want %q (env=%+v)", env.Code, CodeUnsafePath, env)
	}
}

// TestHandler_FullRoundTrip wires every endpoint end-to-end —
// create → patch → preview → save → discard. Mirrors the acceptance
// criterion at the handler layer.
func TestHandler_FullRoundTrip(t *testing.T) {
	t.Parallel()
	_, srv := newTestHandler(t)
	// Create.
	create := postJSON(t, srv, RoutePrefix+"/", map[string]any{"name": "rt-agent"})
	var cr createResponse
	if err := json.Unmarshal(create, &cr); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Patch README.
	patchURL := srv.URL + RoutePrefix + "/" + cr.DraftID + "/files/README.md"
	patchReq, _ := http.NewRequest(http.MethodPatch, patchURL, bytes.NewBufferString(`{"content":"# edited"}`))
	patchReq.Header.Set("Content-Type", "application/json")
	patchResp, err := http.DefaultClient.Do(patchReq)
	if err != nil {
		t.Fatalf("PATCH: %v", err)
	}
	patchResp.Body.Close()
	if patchResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH status = %d", patchResp.StatusCode)
	}

	// Preview.
	preview := postJSON(t, srv, RoutePrefix+"/"+cr.DraftID+"/preview", nil)
	var pv previewResponse
	if err := json.Unmarshal(preview, &pv); err != nil {
		t.Fatalf("preview decode: %v", err)
	}
	if !pv.OK {
		t.Fatalf("preview not ok: %v", pv.Errors)
	}

	// Save.
	outDir := filepath.Join(t.TempDir(), "promoted-handler")
	save := postJSON(t, srv, RoutePrefix+"/"+cr.DraftID+"/save", map[string]any{
		"name":       "rt-agent",
		"output_dir": outDir,
	})
	var sv saveResponse
	if err := json.Unmarshal(save, &sv); err != nil {
		t.Fatalf("save decode: %v", err)
	}
	if sv.OutputDir != outDir {
		t.Errorf("save OutputDir = %q, want %q", sv.OutputDir, outDir)
	}
	got, err := os.ReadFile(filepath.Join(outDir, "README.md"))
	if err != nil {
		t.Fatalf("read promoted README: %v", err)
	}
	if !strings.Contains(string(got), "edited") {
		t.Errorf("promoted README missing edit marker: %q", string(got))
	}

	// Discard.
	delURL := srv.URL + RoutePrefix + "/" + cr.DraftID
	delReq, _ := http.NewRequest(http.MethodDelete, delURL, nil)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Errorf("DELETE status = %d, want 200", delResp.StatusCode)
	}

	// GET after discard → 404.
	getResp, err := http.Get(srv.URL + RoutePrefix + "/" + cr.DraftID)
	if err != nil {
		t.Fatalf("GET after discard: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusNotFound {
		t.Errorf("GET after discard status = %d, want 404", getResp.StatusCode)
	}
}

// TestHandler_Save_InvalidYAML_Returns400_WithCodeValidationFailed
// pins the wire mapping for the validation-failure path.
func TestHandler_Save_InvalidYAML_Returns400_WithCodeValidationFailed(t *testing.T) {
	t.Parallel()
	_, srv := newTestHandler(t)
	create := postJSON(t, srv, RoutePrefix+"/", map[string]any{"name": "bad-agent"})
	var cr createResponse
	if err := json.Unmarshal(create, &cr); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Break the yaml.
	patchURL := srv.URL + RoutePrefix + "/" + cr.DraftID + "/files/harbor.yaml"
	req, _ := http.NewRequest(http.MethodPatch, patchURL, bytes.NewBufferString(`{"content":"not: a:\n   yaml::"}`))
	req.Header.Set("Content-Type", "application/json")
	pr, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH: %v", err)
	}
	pr.Body.Close()

	saveResp := mustPost(t, srv, RoutePrefix+"/"+cr.DraftID+"/save", map[string]any{
		"name":       "bad-agent",
		"output_dir": filepath.Join(t.TempDir(), "never"),
	})
	defer saveResp.Body.Close()
	if saveResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", saveResp.StatusCode)
	}
	var env errorEnvelope
	if err := json.NewDecoder(saveResp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Code != CodeValidationFailed {
		t.Errorf("error code = %q, want %q", env.Code, CodeValidationFailed)
	}
}

// ---- helpers ------------------------------------------------------

func postJSON(t *testing.T, srv *httptest.Server, path string, body any) []byte {
	t.Helper()
	resp := mustPost(t, srv, path, body)
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("postJSON %s: status %d", path, resp.StatusCode)
	}
	out := new(bytes.Buffer)
	if _, err := out.ReadFrom(resp.Body); err != nil {
		t.Fatalf("postJSON %s: read: %v", path, err)
	}
	return out.Bytes()
}

func mustPost(t *testing.T, srv *httptest.Server, path string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	} else {
		buf.WriteString("{}")
	}
	resp, err := http.Post(srv.URL+path, "application/json", &buf)
	if err != nil {
		t.Fatalf("Post %s: %v", path, err)
	}
	return resp
}

// Silence unused-import lint guard for context (kept for future
// helpers that need explicit cancellation).
var _ = context.Background
