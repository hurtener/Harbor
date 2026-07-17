package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tui/conversation"
)

func TestTUICmd_HelpStatesProtocolOnlyOperationalSurface(t *testing.T) {
	stdout, stderr, err := runRoot(t, []string{"tui", "--help"})
	if err != nil || stderr != "" {
		t.Fatalf("help err=%v stderr=%q", err, stderr)
	}
	for _, text := range []string{"--attach", "authenticated Harbor Protocol REST", "never starts, embeds, or directly accesses a Runtime", "--session"} {
		if !strings.Contains(stdout, text) {
			t.Errorf("help missing %q:\n%s", text, stdout)
		}
	}
	root := NewRootCmd()
	command, _, findErr := root.Find([]string{"tui"})
	if findErr != nil || command.Flags().Lookup("attach") == nil {
		t.Fatalf("operational attach flag missing: %v", findErr)
	}
}

func TestTUICmd_AttachRunsOperationalModelAndQuitsFromKeyboard(t *testing.T) {
	id := types.IdentityScope{Tenant: "t", User: "u", Session: "s"}
	token := tuiTestToken(id)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		write := func(value any) { _ = json.NewEncoder(w).Encode(value) }
		switch r.URL.Path {
		case "/v1/control/runtime.info":
			write(types.RuntimeInfo{InstanceID: "cmd-test", BuildVersion: "test", BuildCommit: "test", BuildGoVersion: "go", ProtocolVersion: types.ProtocolVersion, WireSurfaceDigest: "digest", Capabilities: []types.Capability{types.CapTaskControl, types.CapEventsSubscribe, types.CapStateSnapshots}})
		case "/v1/state/history":
			write(types.StateHistoryResponse{})
		case "/v1/tasks/list":
			write(types.TaskListResponse{})
		case "/v1/sessions/inspect":
			write(types.SessionsInspectResponse{Row: types.SessionRow{SessionID: id.Session, Identity: id, Status: types.SessionStatusRunning}})
		case "/v1/pause/list":
			write(types.PauseListResponse{Page: 1, PageCount: 1})
		case "/v1/events":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	store := conversation.NewStore(filepath.Join(dir, "state.json"))
	if err := store.Save(conversation.InteractionState{Identity: id, RuntimeFingerprint: "cmd-test@digest", Draft: "restored", Compact: true, Theme: "light", ReducedMotion: true}); err != nil {
		t.Fatal(err)
	}
	cmd := newTUICmd()
	statePath := filepath.Join(dir, "state.json")
	cmd.SetArgs([]string{"--attach", server.URL, "--token-file", tokenPath, "--state-file", statePath, "--session", id.Session, "--compact"})
	cmd.SetIn(bytes.NewBufferString("\x03"))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	restored := newTUICmd()
	restored.SetArgs([]string{"--attach", server.URL, "--token-file", tokenPath, "--state-file", statePath})
	restored.SetIn(bytes.NewBufferString("\x03"))
	restored.SetOut(&bytes.Buffer{})
	restored.SetErr(&bytes.Buffer{})
	if err := restored.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	invalidURL := newTUICmd()
	invalidURL.SetArgs([]string{"--attach", "://bad", "--token-file", tokenPath, "--state-file", filepath.Join(dir, "invalid-url.json"), "--session", id.Session})
	invalidURL.SetOut(&bytes.Buffer{})
	invalidURL.SetErr(&bytes.Buffer{})
	if err := invalidURL.ExecuteContext(t.Context()); err == nil {
		t.Fatal("malformed Runtime URL accepted")
	}
	badTokenPath := filepath.Join(dir, "bad-token")
	if err := os.WriteFile(badTokenPath, []byte("malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	badToken := newTUICmd()
	badToken.SetArgs([]string{"--attach", server.URL, "--token-file", badTokenPath, "--state-file", filepath.Join(dir, "bad-token-state.json"), "--session", id.Session})
	badToken.SetOut(&bytes.Buffer{})
	badToken.SetErr(&bytes.Buffer{})
	if err := badToken.ExecuteContext(t.Context()); err == nil {
		t.Fatal("malformed token accepted")
	}
	if err := os.WriteFile(statePath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	brokenState := newTUICmd()
	brokenState.SetArgs([]string{"--attach", server.URL, "--token-file", tokenPath, "--state-file", statePath, "--session", id.Session})
	brokenState.SetIn(bytes.NewBufferString("\x03"))
	brokenState.SetOut(&bytes.Buffer{})
	brokenState.SetErr(&bytes.Buffer{})
	if err := brokenState.ExecuteContext(t.Context()); err == nil {
		t.Fatal("malformed state did not fail runTUI")
	}
	server.Close()
	offline := newTUICmd()
	offline.SetArgs([]string{"--attach", server.URL, "--token-file", tokenPath, "--state-file", filepath.Join(dir, "offline.json"), "--session", id.Session})
	offline.SetIn(bytes.NewBufferString("\x03"))
	offline.SetOut(&bytes.Buffer{})
	offline.SetErr(&bytes.Buffer{})
	if err := offline.ExecuteContext(t.Context()); err == nil {
		t.Fatal("unreachable Runtime did not fail runTUI")
	}
}

func tuiTestToken(id types.IdentityScope) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"tenant":"%s","user":"%s","session":"%s","exp":%d,"scopes":["admin"]}`, id.Tenant, id.User, id.Session, time.Now().Add(time.Hour).Unix())))
	return "e30." + payload + ".signature"
}

func TestTUICmd_ExecutionFailsLoudlyWithoutAttach(t *testing.T) {
	_, stderr, err := runRoot(t, []string{"tui"})
	var cli CLIError
	if err == nil || !errors.As(err, &cli) || cli.Code != CodeBindInvalid || !strings.Contains(stderr, "--attach is required") {
		t.Fatalf("err=%v stderr=%q", err, stderr)
	}
}

func TestResolveTUIAuth_ExplicitRotatingFileOverridesEnvironmentPolicy(t *testing.T) {
	token := tuiTestToken(types.IdentityScope{Tenant: "t", User: "u", Session: "s"})
	path := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(path, []byte(`{"t/u/s":"`+token+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	auth, source, err := resolveTUIAuth(path, true, "s")
	if err != nil || auth.Source != "file" || source != path || !strings.Contains(auth.Token, token) {
		t.Fatalf("auth=%#v source=%q err=%v", auth, source, err)
	}
	if _, _, err = resolveTUIAuth(filepath.Join(t.TempDir(), "missing"), true, "s"); err == nil {
		t.Fatal("missing explicit token file was ignored")
	}
}

func TestTUIAuthAndErrorBranchesFailLoudly(t *testing.T) {
	id := types.IdentityScope{Tenant: "t", User: "u", Session: "s"}
	token := tuiTestToken(id)
	t.Setenv(envHarborToken, token)
	auth, source, err := resolveTUIAuth(filepath.Join(t.TempDir(), "unused"), false, id.Session)
	if err != nil || auth.Source != "env" || source != "" {
		t.Fatalf("env auth=%#v source=%q err=%v", auth, source, err)
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if err = os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = resolveTUIAuth(empty, true, id.Session); err == nil {
		t.Fatal("empty explicit token file accepted")
	}
	if _, err = resolveTUIInitialToken("{", id.Session); err == nil {
		t.Fatal("malformed token map accepted")
	}
	other := types.IdentityScope{Tenant: "t", User: "u", Session: "other"}
	if _, err = resolveTUIInitialToken(`{"t/u/other":"`+tuiTestToken(other)+`"}`, id.Session); !errors.Is(err, conversation.ErrTokenUnavailable) {
		t.Fatalf("wrong target token=%v", err)
	}
	if got := tuiCLIError(conversation.ErrTokenExpired); got.Code != CodeAuthRequired {
		t.Fatalf("auth error=%#v", got)
	}
	if got := tuiCLIError(errors.New("network")); got.Code != "tui_failed" {
		t.Fatalf("generic error=%#v", got)
	}
}
