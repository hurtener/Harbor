// cmd/harbor/cmd_composition_preview_test.go — unit + golden tests
// for `harbor composition-preview` (D-415, the HA-66 effective-
// composition preview consumer).
//
// The command body is driven against an httptest.Server serving the
// canonical `agent_config.composition.preview` wire shape (no real
// Runtime needed); goldens lock both human-mode and --json output
// shapes. Flag-validation + auth paths are asserted via the cobra
// root like the inspect-* suites.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// runCompositionPreviewTest builds a fresh root, sets args, captures
// stdout/stderr, returns (stdout, stderr, err). Mirrors
// runInspectTopologyTest for the composition-preview path.
func runCompositionPreviewTest(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	root := NewRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	for _, child := range root.Commands() {
		if child.Name() == "composition-preview" {
			child.SetOut(&out)
			child.SetErr(&errBuf)
		}
	}
	root.SetArgs(append([]string{"composition-preview"}, args...))
	err := root.Execute()
	return out.String(), errBuf.String(), err
}

// canonicalPreviewFixture is the deterministic `available` wire
// response the golden tests serve: two items with stable names,
// sources, hashes, and skill summaries. The shape matches the
// `agent_config.composition.preview` response verbatim (strict decode
// disallows unknown fields on the client side, so the fixture must
// mirror the wire type exactly).
const canonicalPreviewFixture = `{
  "outcome": "available",
  "boot_pack_set_hash": "bootpackset000000000000000000000000000000000000000000000000000001",
  "combined_hash": "combinedhash00000000000000000000000000000000000000000000000000000001",
  "revision_hash": "revisionhash00000000000000000000000000000000000000000000000000000001",
  "revision_id": "rev-2",
  "content_hash": "contenthash00000000000000000000000000000000000000000000000000000001",
  "items": [
    {
      "name": "boot-pack-skill",
      "semantic_hash": "sema00000000000000000000000000000000000000000000000000000000000001",
      "source": "boot",
      "skill": {
        "name": "boot-pack-skill",
        "title": "Boot pack skill",
        "trigger": "when asked to pack",
        "origin": "boot",
        "scope": "operator",
        "content_hash": "skill-content-hash-1",
        "updated_at": "2026-08-14T00:00:00Z"
      }
    },
    {
      "name": "recap",
      "semantic_hash": "sema00000000000000000000000000000000000000000000000000000000000002",
      "source": "both",
      "skill": {
        "name": "recap",
        "title": "Summarise a thread",
        "trigger": "recap the thread",
        "origin": "generated",
        "scope": "project",
        "content_hash": "skill-content-hash-2",
        "updated_at": "2026-08-14T00:00:00Z"
      }
    }
  ],
  "widened": false,
  "protocol_version": "0.1.0"
}`

// capturedPreviewRequest is the synchronized view of the last
// composition-preview request the fixture server handled. The handler
// sends it over a channel BEFORE writing the response, so a test that
// awaits the response (via the client returning) observes it with a
// proper happens-before edge.
type capturedPreviewRequest struct {
	req  *http.Request
	body []byte
}

// previewFixtureServer serves one canned response per request path
// (the composition-preview path) and captures the request's headers +
// body through a channel so the test can assert the identity headers +
// body shape without racing the handler goroutine.
func previewFixtureServer(t *testing.T, responses map[string]string) (*httptest.Server, <-chan capturedPreviewRequest) {
	t.Helper()
	captured := make(chan capturedPreviewRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body) //nolint:errcheck // test fixture; a body read failure surfaces as an empty-body assertion
		}
		select {
		case captured <- capturedPreviewRequest{req: r.Clone(r.Context()), body: body}:
		default:
		}
		resp, ok := responses[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}

// awaitPreviewRequest drains the capture channel. The server handles
// exactly one request per test, so the first value is the one.
func awaitPreviewRequest(t *testing.T, captured <-chan capturedPreviewRequest) capturedPreviewRequest {
	t.Helper()
	select {
	case got := <-captured:
		return got
	default:
		t.Fatal("fixture server never received a request")
		return capturedPreviewRequest{}
	}
}

// TestCompositionPreview_MissingIdentity asserts the incomplete-triple
// path surfaces CodeIdentityIncomplete at the CLI edge (before any
// network call).
func TestCompositionPreview_MissingIdentity(t *testing.T) {
	t.Parallel()
	_, _, err := runCompositionPreviewTest(t, "--agent", "harbor-dev-agent")
	if err == nil {
		t.Fatal("expected error for missing identity triple")
	}
	var cli CLIError
	if !errors.As(err, &cli) {
		t.Fatalf("error is not a CLIError: %T %v", err, err)
	}
	if cli.Code != CodeIdentityIncomplete {
		t.Errorf("CLIError.Code: got %q, want %q", cli.Code, CodeIdentityIncomplete)
	}
}

// TestCompositionPreview_MissingAgent asserts the missing --agent path
// surfaces CodeCompositionPreviewAgentMissing.
func TestCompositionPreview_MissingAgent(t *testing.T) {
	t.Parallel()
	_, _, err := runCompositionPreviewTest(t, "--tenant", "t", "--user", "u", "--session", "s")
	if err == nil {
		t.Fatal("expected error for missing --agent")
	}
	var cli CLIError
	if !errors.As(err, &cli) {
		t.Fatalf("error is not a CLIError: %T %v", err, err)
	}
	if cli.Code != CodeCompositionPreviewAgentMissing {
		t.Errorf("CLIError.Code: got %q, want %q", cli.Code, CodeCompositionPreviewAgentMissing)
	}
}

// TestCompositionPreview_MissingToken asserts the auth-missing error
// fires when HARBOR_TOKEN is unset AND no ~/.harbor/token exists.
func TestCompositionPreview_MissingToken(t *testing.T) {
	t.Setenv(envHarborToken, "")
	t.Setenv("HOME", t.TempDir())
	_, _, err := runCompositionPreviewTest(t,
		"--tenant", "t", "--user", "u", "--session", "s", "--agent", "harbor-dev-agent")
	if err == nil {
		t.Fatal("expected error for missing token")
	}
	var cli CLIError
	if !errors.As(err, &cli) {
		t.Fatalf("error is not a CLIError: %T %v", err, err)
	}
	if cli.Code != CodeAuthRequired {
		t.Errorf("CLIError.Code: got %q, want %q", cli.Code, CodeAuthRequired)
	}
}

// TestCompositionPreview_BadBind asserts --bind validation surfaces
// CodeBindInvalid. Cannot run t.Parallel — it mutates env.
func TestCompositionPreview_BadBind(t *testing.T) {
	t.Setenv(envHarborToken, "test-bearer-token")
	_, _, err := runCompositionPreviewTest(t,
		"--tenant", "t", "--user", "u", "--session", "s", "--agent", "a", "--bind", "http://exa mple")
	if err == nil {
		t.Fatal("expected error for bad --bind")
	}
	var cli CLIError
	if !errors.As(err, &cli) {
		t.Fatalf("error is not a CLIError: %T %v", err, err)
	}
	if cli.Code != CodeBindInvalid {
		t.Errorf("CLIError.Code: got %q, want %q", cli.Code, CodeBindInvalid)
	}
}

// TestCompositionPreview_Human_Golden drives the testable core against
// an httptest server serving the canonical fixture and pins the
// human-mode output against
// testdata/golden/composition-preview-human.txt.
func TestCompositionPreview_Human_Golden(t *testing.T) {
	srv, captured := previewFixtureServer(t, map[string]string{
		"/v1/agent_config/composition/preview": canonicalPreviewFixture,
	})

	var out bytes.Buffer
	err := runCompositionPreviewAgainst(context.Background(), &out, compositionPreviewOpts{
		Endpoint: srv.URL,
		Identity: prototypes.IdentityScope{Tenant: "t1", User: "u1", Session: "s1"},
		AgentID:  "harbor-dev-agent",
		Auth:     inspectAuth{Token: "test-bearer-token"},
		JSON:     false,
		Client:   srv.Client(),
	}, func(cli CLIError) error { return cli })
	if err != nil {
		t.Fatalf("runCompositionPreviewAgainst: %v", err)
	}

	lastReq := awaitPreviewRequest(t, captured)

	// The request rides the verified caller identity: Bearer header +
	// the identity triple headers; the body carries ONLY the effective
	// agent id (never an invented user/session).
	if got := lastReq.req.Header.Get("Authorization"); got != "Bearer test-bearer-token" {
		t.Errorf("Authorization = %q, want Bearer test-bearer-token", got)
	}
	if got := lastReq.req.Header.Get("X-Harbor-Tenant"); got != "t1" {
		t.Errorf("X-Harbor-Tenant = %q, want t1", got)
	}
	var body map[string]any
	if err := json.Unmarshal(lastReq.body, &body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if body["agent_id"] != "harbor-dev-agent" {
		t.Errorf("request agent_id = %v, want harbor-dev-agent", body["agent_id"])
	}
	if _, ok := body["user_id"]; ok {
		t.Errorf("request must not carry an invented user_id")
	}
	if _, ok := body["session_id"]; ok {
		t.Errorf("request must not carry an invented session_id")
	}

	assertGolden(t, "composition-preview-human.txt", out.String())
}

// TestCompositionPreview_JSON_Golden pins the --json output against
// testdata/golden/composition-preview-json.txt (the canonical wire
// shape re-encoded).
func TestCompositionPreview_JSON_Golden(t *testing.T) {
	srv, _ := previewFixtureServer(t, map[string]string{
		"/v1/agent_config/composition/preview": canonicalPreviewFixture,
	})

	var out bytes.Buffer
	err := runCompositionPreviewAgainst(context.Background(), &out, compositionPreviewOpts{
		Endpoint: srv.URL,
		Identity: prototypes.IdentityScope{Tenant: "t1", User: "u1", Session: "s1"},
		AgentID:  "harbor-dev-agent",
		Auth:     inspectAuth{Token: "test-bearer-token"},
		JSON:     true,
		Client:   srv.Client(),
	}, func(cli CLIError) error { return cli })
	if err != nil {
		t.Fatalf("runCompositionPreviewAgainst: %v", err)
	}
	assertGolden(t, "composition-preview-json.txt", out.String())
}

// TestCompositionPreview_TypedOutcomes asserts every typed outcome
// (available | unavailable | conflict | retired) renders in human mode
// as a SUCCESSFUL preview — never a fabricated error and never a blank
// state (D-311).
func TestCompositionPreview_TypedOutcomes(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
		want    []string
	}{
		{
			name:    "unavailable",
			fixture: `{"outcome":"unavailable","widened":false,"protocol_version":"0.1.0"}`,
			want:    []string{"outcome: unavailable", "non-oracular"},
		},
		{
			name:    "conflict",
			fixture: `{"outcome":"conflict","conflict_name":"recap","widened":false,"protocol_version":"0.1.0"}`,
			want:    []string{"outcome: conflict", "conflict_name: recap", "never a silent overwrite"},
		},
		{
			name:    "retired",
			fixture: `{"outcome":"retired","widened":false,"protocol_version":"0.1.0"}`,
			want:    []string{"outcome: retired", "tombstone"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := previewFixtureServer(t, map[string]string{
				"/v1/agent_config/composition/preview": tc.fixture,
			})
			var out bytes.Buffer
			err := runCompositionPreviewAgainst(context.Background(), &out, compositionPreviewOpts{
				Endpoint: srv.URL,
				Identity: prototypes.IdentityScope{Tenant: "t", User: "u", Session: "s"},
				AgentID:  "a",
				Auth:     inspectAuth{Token: "j"},
				JSON:     false,
				Client:   srv.Client(),
			}, func(cli CLIError) error { return cli })
			if err != nil {
				t.Fatalf("typed outcome %q must NOT be an error: %v", tc.name, err)
			}
			got := out.String()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("human output missing %q:\n%s", want, got)
				}
			}
		})
	}
}

// TestCompositionPreview_HTTPStatus_FailsLoud asserts a non-2xx
// Runtime response (e.g. the preview not wired → 501) surfaces
// CodeCompositionPreviewHTTPStatus with a hint naming the cause.
func TestCompositionPreview_HTTPStatus_FailsLoud(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"code":"unknown_method","message":"composition preview is not wired on this runtime"}`))
	}))
	defer srv.Close()

	var captured CLIError
	emit := func(cli CLIError) error {
		captured = cli
		return cli
	}
	err := runCompositionPreviewAgainst(context.Background(), &bytes.Buffer{}, compositionPreviewOpts{
		Endpoint: srv.URL,
		Identity: prototypes.IdentityScope{Tenant: "t", User: "u", Session: "s"},
		AgentID:  "a",
		Auth:     inspectAuth{Token: "j"},
		Client:   srv.Client(),
	}, emit)
	if err == nil {
		t.Fatal("expected error for 501 response")
	}
	if captured.Code != CodeCompositionPreviewHTTPStatus {
		t.Errorf("captured.Code = %q, want %q", captured.Code, CodeCompositionPreviewHTTPStatus)
	}
	if captured.Hint == "" {
		t.Error("captured.Hint empty — the 501 path must name the un-wired preview")
	}
}

// TestCompositionPreview_EmptyItems renders the available outcome with
// an empty tier (deterministic hashes only) — never a blank state.
func TestCompositionPreview_EmptyItems(t *testing.T) {
	srv, _ := previewFixtureServer(t, map[string]string{
		"/v1/agent_config/composition/preview": `{
			"outcome":"available","boot_pack_set_hash":"b","combined_hash":"c",
			"widened":false,"protocol_version":"0.1.0"}`,
	})
	var out bytes.Buffer
	err := runCompositionPreviewAgainst(context.Background(), &out, compositionPreviewOpts{
		Endpoint: srv.URL,
		Identity: prototypes.IdentityScope{Tenant: "t", User: "u", Session: "s"},
		AgentID:  "a",
		Auth:     inspectAuth{Token: "j"},
		JSON:     false,
		Client:   srv.Client(),
	}, func(cli CLIError) error { return cli })
	if err != nil {
		t.Fatalf("runCompositionPreviewAgainst: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "outcome: available") || !strings.Contains(got, "items: none") {
		t.Errorf("empty-tier render missing outcome/items: none:\n%s", got)
	}
}

// TestPreviewBaseURL accepts both bare host:port and full URLs, and
// fails loud on an empty bind.
func TestPreviewBaseURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		bind string
		want string
		err  bool
	}{
		{"127.0.0.1:18080", "http://127.0.0.1:18080", false},
		{"http://127.0.0.1:18080", "http://127.0.0.1:18080", false},
		{"https://runtime.example.com/base/", "https://runtime.example.com/base", false},
		{"", "", true},
	}
	for _, tc := range cases {
		got, err := previewBaseURL(tc.bind)
		if tc.err {
			if err == nil {
				t.Errorf("previewBaseURL(%q): expected error", tc.bind)
			}
			continue
		}
		if err != nil {
			t.Errorf("previewBaseURL(%q): %v", tc.bind, err)
			continue
		}
		if got != tc.want {
			t.Errorf("previewBaseURL(%q) = %q, want %q", tc.bind, got, tc.want)
		}
	}
}
