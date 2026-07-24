// Phase 200 — Per-user credential injection for receiver-style MCP servers
// (RFC §6.4; D-341; a controlled pull-then-inject exception to D-271, extending
// the D-278 injection seam).
//
// This test wires the REAL artifacts across the seam — no mocks at the boundary
// (CLAUDE.md §17.3):
//
//   - the same RFC-8693 broker fixture (`newP142Broker`, §17.8 spec-derived)
//     driving the real `tokenexchange` provider — the per-user broker-pull is
//     REUSED unchanged (D-341 sources the credential exactly as the bearer path);
//   - an httptest-hosted MCP fixture on the OFFICIAL go-sdk streamable-HTTP
//     handler, fronted by a recorder that captures EVERY request header + the
//     `_meta` map. NOTE: this is an accept-anything echo receiver; the injected
//     credential FORMS (header name / Basic / `_meta` key) are hand-authored,
//     because no canonical receiver-style credential schema exists to derive
//     from. The §17.8 wrong-field risk does not bite here because the test
//     observes the ACTUAL injected value at the recorder (not a self-consistent
//     pass/fail), proving inject+redact+isolate regardless of what a real
//     receiver would require;
//   - the real MCP driver bound via Config.Injection in each declared form;
//   - the real `audit.Redactor` (patterns driver) on the seam — the captured
//     outbound payload is redacted and asserted to show `***` for every form.
//
// It proves: (a) each declared form injects the per-user pulled value; (b) two
// acting users get DISTINCT injected values (isolation); (c) the redactor holds
// every injected form to `***`; (d) a broker outage fails the call loud with NO
// wire request. `-race` throughout.
package integration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/audit"
	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// p200Capture is one recorded receiver-side observation: the full header set and
// the `_meta` map of a tools/call POST.
type p200Capture struct {
	headers map[string]string
	meta    map[string]any
}

type p200Recorder struct {
	inner http.Handler
	mu    sync.Mutex
	calls []p200Capture
}

func (r *p200Recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodPost {
		body, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err == nil {
			r.record(req.Header, body)
		}
		req.Body = io.NopCloser(strings.NewReader(string(body)))
		req.ContentLength = int64(len(body))
	}
	r.inner.ServeHTTP(w, req)
}

func (r *p200Recorder) record(h http.Header, body []byte) {
	var msg struct {
		Method string `json:"method"`
		Params struct {
			Meta map[string]any `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &msg); err != nil || msg.Method != "tools/call" {
		return
	}
	headers := map[string]string{}
	for k := range h {
		headers[k] = h.Get(k)
	}
	r.mu.Lock()
	r.calls = append(r.calls, p200Capture{headers: headers, meta: msg.Params.Meta})
	r.mu.Unlock()
}

func (r *p200Recorder) snapshot() []p200Capture {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]p200Capture, len(r.calls))
	copy(out, r.calls)
	return out
}

func p200MCPServer(t *testing.T) (*httptest.Server, *p200Recorder) {
	t.Helper()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "harbor-p200-fixture", Version: "v0"}, nil)
	mcpsdk.AddTool(srv,
		&mcpsdk.Tool{
			Name:        "echo",
			Description: "echo",
			InputSchema: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"text": map[string]any{"type": "string"}},
				"additionalProperties": false,
			},
		},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, in struct {
			Text string `json:"text"`
		}) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: in.Text}}}, nil, nil
		},
	)
	rec := &p200Recorder{inner: mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil)}
	hs := httptest.NewServer(rec)
	t.Cleanup(hs.Close)
	return hs, rec
}

// newP200Echo builds a receiver-style connection bound to the given injection
// mapping and returns the discovered echo descriptor + the recorder.
func newP200Echo(t *testing.T, inj *mcpdrv.CredentialInjection) (tools.ToolDescriptor, *p200Recorder) {
	t.Helper()
	broker := newP142Broker(t)
	hs, rec := p200MCPServer(t)
	prov, bus := newP148ProviderAndBus(t, broker, hs.URL)
	inj.Provider = prov

	p, err := mcpdrv.New(mcpdrv.Config{
		Name:            "recv",
		TransportMode:   mcpdrv.TransportStreamableHTTP,
		URL:             hs.URL,
		Bus:             bus,
		DefaultIdentity: identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"},
		Injection:       inj,
	})
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	descs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, d := range descs {
		if d.Tool.Name == "recv_echo" {
			return d, rec
		}
	}
	t.Fatal("recv_echo not discovered")
	return tools.ToolDescriptor{}, nil
}

// redactedPayload runs the captured request (headers + _meta) through the REAL
// audit redactor and returns the redacted map.
func redactedPayload(t *testing.T, c p200Capture) map[string]any {
	t.Helper()
	red := patternsAudit.New()
	var _ audit.Redactor = red
	out, err := red.Redact(context.Background(), map[string]any{
		"headers": c.headers,
		"_meta":   c.meta,
	})
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	return out.(map[string]any)
}

func TestE2E_Phase200_HeaderForm_InjectsAndRedacts(t *testing.T) {
	t.Parallel()
	echo, rec := newP200Echo(t, &mcpdrv.CredentialInjection{
		Form:   mcpdrv.InjectionFormHeader,
		Header: "x-vendor-api-key",
	})
	id := identity.Identity{TenantID: "t1", UserID: "alice", SessionID: "s1"}
	if _, err := echo.Invoke(p148Ctx(t, id, "agent-1"), json.RawMessage(`{"text":"hi"}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("server saw %d calls, want 1", len(calls))
	}
	if got := calls[0].headers["X-Vendor-Api-Key"]; got != "brokered-t1-alice" {
		t.Fatalf("injected header = %q, want brokered-t1-alice", got)
	}
	// Redaction: the captured header must not survive to an audit payload.
	red := redactedPayload(t, calls[0])
	if v := red["headers"].(map[string]any)["X-Vendor-Api-Key"]; v != "***" {
		t.Fatalf("injected header not redacted: %v", v)
	}
}

func TestE2E_Phase200_BasicForm_InjectsAndRedacts(t *testing.T) {
	t.Parallel()
	echo, rec := newP200Echo(t, &mcpdrv.CredentialInjection{
		Form:          mcpdrv.InjectionFormBasic,
		BasicUsername: "svc",
	})
	id := identity.Identity{TenantID: "t1", UserID: "bob", SessionID: "s1"}
	if _, err := echo.Invoke(p148Ctx(t, id, "agent-1"), json.RawMessage(`{"text":"hi"}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	calls := rec.snapshot()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("svc:brokered-t1-bob"))
	if got := calls[0].headers["Authorization"]; got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
	red := redactedPayload(t, calls[0])
	if v := red["headers"].(map[string]any)["Authorization"]; v != "***" {
		t.Fatalf("Basic Authorization not redacted: %v", v)
	}
}

func TestE2E_Phase200_MetaForm_InjectsAndRedacts(t *testing.T) {
	t.Parallel()
	echo, rec := newP200Echo(t, &mcpdrv.CredentialInjection{
		Form:    mcpdrv.InjectionFormMeta,
		MetaKey: []string{"vendor", "api_key"},
	})
	id := identity.Identity{TenantID: "t1", UserID: "carol", SessionID: "s1"}
	if _, err := echo.Invoke(p148Ctx(t, id, "agent-1"), json.RawMessage(`{"text":"hi"}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	calls := rec.snapshot()
	vendor, ok := calls[0].meta["vendor"].(map[string]any)
	if !ok {
		t.Fatalf("_meta.vendor missing: %+v", calls[0].meta)
	}
	if vendor["api_key"] != "brokered-t1-carol" {
		t.Fatalf("_meta credential = %v, want brokered-t1-carol", vendor["api_key"])
	}
	// The triple is still stamped and unshadowed.
	if calls[0].meta["tenant"] != "t1" || calls[0].meta["user"] != "carol" {
		t.Fatalf("triple missing/shadowed: %+v", calls[0].meta)
	}
	red := redactedPayload(t, calls[0])
	redVendor := red["_meta"].(map[string]any)["vendor"].(map[string]any)
	if redVendor["api_key"] != "***" {
		t.Fatalf("_meta credential not redacted: %v", redVendor["api_key"])
	}
}

func TestE2E_Phase200_PerUserIsolation(t *testing.T) {
	t.Parallel()
	echo, rec := newP200Echo(t, &mcpdrv.CredentialInjection{
		Form:   mcpdrv.InjectionFormHeader,
		Header: "x-vendor-api-key",
	})
	for _, id := range []identity.Identity{
		{TenantID: "t-a", UserID: "amy", SessionID: "s"},
		{TenantID: "t-b", UserID: "bob", SessionID: "s"},
	} {
		if _, err := echo.Invoke(p148Ctx(t, id, "agent-x"), json.RawMessage(`{"text":"hi"}`)); err != nil {
			t.Fatalf("Invoke(%s): %v", id.UserID, err)
		}
	}
	want := map[string]string{"amy": "brokered-t-a-amy", "bob": "brokered-t-b-bob"}
	for _, c := range rec.snapshot() {
		user, _ := c.meta["user"].(string)
		exp, ok := want[user]
		if !ok {
			t.Fatalf("unexpected user in _meta: %+v", c.meta)
		}
		if got := c.headers["X-Vendor-Api-Key"]; got != exp {
			t.Fatalf("user %s: injected header %q, want %q (cross-user bleed)", user, got, exp)
		}
	}
}

func TestE2E_Phase200_BrokerRefusal_FailsLoud_NoWireCall(t *testing.T) {
	t.Parallel()
	broker := newP142Broker(t)
	hs, rec := p200MCPServer(t)
	prov, bus := newP148ProviderAndBus(t, broker, hs.URL)
	p, err := mcpdrv.New(mcpdrv.Config{
		Name:            "recv",
		TransportMode:   mcpdrv.TransportStreamableHTTP,
		URL:             hs.URL,
		Bus:             bus,
		DefaultIdentity: identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"},
		Injection:       &mcpdrv.CredentialInjection{Provider: prov, Form: mcpdrv.InjectionFormHeader, Header: "x-vendor-api-key"},
	})
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	descs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var echo tools.ToolDescriptor
	for _, d := range descs {
		if d.Tool.Name == "recv_echo" {
			echo = d
		}
	}
	broker.setPosture("error500")
	id := identity.Identity{TenantID: "t1", UserID: "dave", SessionID: "s1"}
	if _, err := echo.Invoke(p148Ctx(t, id, "agent-x"), json.RawMessage(`{"text":"hi"}`)); err == nil {
		t.Fatal("want error on broker outage, got nil (an unauthenticated call would leak)")
	}
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Fatalf("broker refused but the server saw %d tools/call — an unauthenticated call leaked", len(calls))
	}
}
