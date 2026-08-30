//go:build darwin || linux

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/identity"
	_ "github.com/hurtener/Harbor/internal/llm/mock" // Hermetic real-driver PTY stack.
	protocolclient "github.com/hurtener/Harbor/internal/protocol/client"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tui/app"
	"github.com/hurtener/Harbor/internal/tui/conversation"
	"github.com/hurtener/Harbor/internal/tui/projection"
	"github.com/hurtener/Harbor/internal/tui/ui"
)

const ptyHelperEnv = "HARBOR_TUI_PTY_HELPER"

// ptyWaitTimeout bounds every PTY render/condition wait. It is deliberately
// generous: the 5ms poll ticker returns the instant the expected frame lands,
// so a healthy run is unaffected, but a loaded CI runner's full spawn →
// connect → key-by-key type → re-render pipeline can exceed a tight budget.
// A 5s bound flaked the key-driven search walkthrough on CI (the intermediate
// frames rendered, the final one just missed the window); this headroom keeps
// the gate honest without masking a genuine hang.
const ptyWaitTimeout = 20 * time.Second

type panicModel struct{ app.Model }
type errorModel struct{ app.Model }
type startupFaultModel struct{ app.Model }
type suspendModel struct{ app.Model }

type functionKeyModel struct {
	next int
}

func (functionKeyModel) Init() tea.Cmd { return nil }

func (m functionKeyModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := message.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	functionKeys := [...]rune{tea.KeyF1, tea.KeyF2, tea.KeyF3, tea.KeyF4, tea.KeyF5, tea.KeyF6, tea.KeyF7, tea.KeyF8, tea.KeyF9}
	if m.next == len(functionKeys) {
		if key.Key().Code == 'c' {
			return m, tea.Quit
		}
		return m, nil
	}
	if key.Key().Code < tea.KeyF1 || key.Key().Code > tea.KeyF12 {
		// Bubble Tea's terminal capability probes may also arrive as key
		// messages. They are host negotiation, not operator input.
		return m, nil
	}
	want := functionKeys[m.next]
	if key.Key().Code != want {
		return m, tea.Interrupt
	}
	m.next++
	return m, nil
}

func (m functionKeyModel) View() tea.View {
	if m.next == 9 {
		return tea.NewView("all function keys decoded")
	}
	return tea.NewView(fmt.Sprintf("function keys ready: %d/9", m.next))
}

func (m suspendModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := message.(tea.KeyPressMsg); ok && key.String() == "x" {
		return m, tea.Suspend
	}
	next, cmd := m.Model.Update(message)
	m.Model = next.(app.Model)
	return m, cmd
}

func (m startupFaultModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := message.(tea.WindowSizeMsg); ok {
		panic("injected partial startup failure")
	}
	next, cmd := m.Model.Update(message)
	m.Model = next.(app.Model)
	return m, cmd
}

func (m panicModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := message.(tea.KeyPressMsg); ok && key.String() == "x" {
		panic("injected renderer failure")
	}
	next, cmd := m.Model.Update(message)
	m.Model = next.(app.Model)
	return m, cmd
}

func (m errorModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := message.(tea.KeyPressMsg); ok && key.String() == "x" {
		return m, tea.Interrupt
	}
	next, cmd := m.Model.Update(message)
	m.Model = next.(app.Model)
	return m, cmd
}

func TestE2E_TUITerminalHelper(t *testing.T) {
	mode := os.Getenv(ptyHelperEnv)
	if mode == "" {
		return
	}
	model := app.NewModelFromEnvironment(80, 24, ui.EnvironmentFrom(os.LookupEnv), true, terminalFoundationProjection()).WithState(app.State{CursorHidden: true})
	var candidate tea.Model = model
	switch mode {
	case "panic":
		candidate = panicModel{Model: model}
	case "error":
		candidate = errorModel{Model: model}
	case "startup-fault":
		candidate = startupFaultModel{Model: model}
	case "suspend":
		candidate = suspendModel{Model: model}
	case "function-keys":
		candidate = functionKeyModel{}
	}
	if err := app.Run(context.Background(), os.Stdin, os.Stdout, candidate); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "HOST_ERROR:%v\n", err)
		ptyExit(23)
	}
	ptyExit(0)
}

func ptyExit(code int) { os.Exit(code) }

func terminalFoundationProjection() projection.Projection {
	return projection.Projection{Identity: types.IdentityScope{Tenant: "test", User: "operator", Session: "terminal-foundation"}, Blocks: []projection.Block{{ID: "foundation", Kind: "text", Status: "completed", Text: "Terminal foundation"}}}
}

func TestE2E_TUIConversationPTY_KeyDrivenAuthenticatedWorkflow(t *testing.T) {
	// This is an unconditionally-run release gate. #598's erase ordering defect
	// and the duplicate failed-follow-up command in this workflow both have
	// deterministic app-level regressions. The #604 recurrence exposed
	// protocol-inaccurate function-key input, cell-diff/transient-output oracles,
	// and a same-scope stale-inspection response that could close a newer action
	// modal; each now has a focused regression. Keep the failure-only PTY liveness
	// and SIGQUIT diagnostics below rather than restoring a permanent quarantine
	// or increasing its timeout.
	stack := devstack.Assemble(t, runtimePostureConfig(t), devstack.AssembleOpts{})
	defer stack.Close()
	var failNextStart atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/control/start" && failNextStart.Swap(false) {
			http.Error(w, "forced follow-up failure", http.StatusServiceUnavailable)
			return
		}
		stack.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	first := types.IdentityScope{Tenant: devstack.DefaultDevTenant, User: devstack.DefaultDevUser, Session: devstack.DefaultDevSession}
	second := first
	second.Session = "pty-second"
	fresh := first
	fresh.Session = "pty-fresh"
	for _, scope := range []types.IdentityScope{first, second} {
		id := identity.Identity{TenantID: scope.Tenant, UserID: scope.User, SessionID: scope.Session}
		ctx, err := identity.With(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stack.Sessions.Open(ctx, scope.Session, id); err != nil {
			t.Fatalf("open %s: %v", scope.Session, err)
		}
	}
	tokens := map[string]string{scopeTokenKey(first): stack.Token, scopeTokenKey(second): mintPTYToken(t, stack, second.Session), scopeTokenKey(fresh): mintPTYToken(t, stack, fresh.Session)}
	secondClient, err := protocolclient.New(protocolclient.Connection{BaseURL: server.URL, Token: protocolclient.StaticToken(tokens[scopeTokenKey(second)], second), Identity: second})
	if err != nil {
		t.Fatal(err)
	}
	temp := t.TempDir()
	binary := filepath.Join(temp, "harbor")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/harbor")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("build harbor CLI: %v\n%s", buildErr, output)
	}
	tokenPath := filepath.Join(temp, "tokens.json")
	statePath := filepath.Join(temp, "state.json")
	exportPath := filepath.Join(temp, "exports", first.Session+".md")
	attachmentPath := filepath.Join(temp, "note.txt")
	writePTYTokens(t, tokenPath, tokens)
	if err := os.WriteFile(attachmentPath, []byte("pty attachment"), 0o600); err != nil {
		t.Fatal(err)
	}
	session := startPTYCommand(t, binary, []string{"tui", "--attach", server.URL, "--token-file", tokenPath, "--state-file", statePath, "--session", first.Session}, temp, 80, 24)
	session.waitContains(t, "live")
	session.text(t, "hello")
	awaitPTYDraftPersisted(t, statePath, first, "hello")
	// The renderer emits cell diffs, so five typed characters need not be one
	// contiguous byte substring (observed as "hel" and "lo" around cursor
	// movement). Persistence acknowledges that all five keys reached the model;
	// a resize then asks for a stable full visual frame before editing continues.
	mark := len(session.snapshot())
	session.resize(t, 81, 24)
	session.waitContainsAfter(t, mark, "hello")
	session.write(t, "\x1b[1;2D")
	session.key(t, 'o', 1)
	session.key(t, '!', 1)
	session.key(t, '_', 5)
	session.key(t, '_', 3)
	mark = len(session.snapshot())
	session.key(t, 'x', 5)
	session.waitContainsAfter(t, mark, "Toggle runtime context")
	session.key(t, 'b', 1)
	session.waitContains(t, "Draft stashed locally")
	session.text(t, "/help")
	session.key(t, '\r', 1)
	session.waitContains(t, "Keyboard help")
	session.key(t, 27, 1)
	session.command(t, 'p')
	session.waitContains(t, "Stashed draft restored")
	session.write(t, "\x1b[200~ world\nsecond\x1b[201~")
	session.waitContains(t, "second")
	session.text(t, " @")
	session.waitContains(t, "active canonical session")
	session.key(t, '\t', 1)
	mark = len(session.snapshot())
	session.command(t, 'f')
	session.waitContainsAfter(t, mark, "earch:")
	session.text(t, "hello")
	// Let the query fully register (all five keystrokes processed and the
	// match count recomputed) BEFORE forcing the repaint — otherwise the
	// resize can race the input pipeline on a loaded runner and repaint a
	// stale search state, leaving the assertion text absent (a CI flake).
	session.waitContains(t, "Search: hello")
	// A resize then forces a full repaint: the cell-diff renderer may split
	// the incrementally-updated toast across cursor moves, so the assertion
	// text is only guaranteed contiguous in the byte stream after a fresh
	// paint of the now-settled search state.
	session.resize(t, 100, 30)
	session.waitContains(t, "hello · 0 matches")
	mark = len(session.snapshot())
	session.key(t, 27, 1)
	session.waitContainsAfter(t, mark, "closed")
	session.write(t, "\x1b[5~")
	mark = len(session.snapshot())
	session.command(t, 'a')
	session.waitContainsAfter(t, mark, "File path|disposition")
	session.text(t, "missing.txt|context")
	session.key(t, '\r', 1)
	session.waitContains(t, "read attachment")
	if err := os.WriteFile(filepath.Join(temp, "missing.txt"), []byte("retry payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	session.command(t, 'u')
	session.waitContains(t, "attachment · missing.txt")
	mark = len(session.snapshot())
	session.command(t, 'e')
	// A one-cell suffix update can encode this toast as "Attachment removed
	// locall" plus a separately positioned "y". Force a full frame after the
	// command so the assertion observes the terminal's text rather than requiring
	// one incidental byte boundary from the cell-diff renderer.
	session.resize(t, 99, 30)
	session.waitContainsAfter(t, mark, "Attachment removed locally")
	mark = len(session.snapshot())
	session.command(t, 'a')
	session.waitContainsAfter(t, mark, "File path|disposition")
	session.text(t, "note.txt|ref")
	session.key(t, '\r', 1)
	session.waitContains(t, "attachment · note.txt")
	rotated := mintPTYToken(t, stack, second.Session)
	tokens[scopeTokenKey(second)] = rotated
	writePTYTokens(t, tokenPath, tokens)
	mark = len(session.snapshot())
	session.command(t, 'l')
	session.waitContainsAfter(t, mark, "Sessions")
	session.text(t, second.Session)
	session.waitContains(t, second.Session)
	session.key(t, '\r', 1)
	// The cell-diff renderer rewrites only the changed suffix of the identity
	// row, so wait on the switch acknowledgement toast, which is always emitted
	// whole.
	session.waitContains(t, "Attached session "+second.Session)
	mark = len(session.snapshot())
	session.key(t, 'r', 5)
	session.waitContainsAfter(t, mark, "Rename session")
	session.text(t, "renamed")
	session.key(t, '\r', 1)
	await(t, func() bool {
		response, inspectErr := secondClient.SessionsInspect(t.Context(), types.SessionsInspectRequest{SessionID: second.Session})
		return inspectErr == nil && response.Row.Title == "renamed"
	}, "PTY canonical session rename")
	// SessionsInspect confirms that the server accepted the title before the
	// Bubble Tea command result has necessarily been delivered back to the
	// terminal event loop. Do not send the next shortcut until that result has
	// closed the rename dialog; otherwise its key bytes can be consumed by the
	// still-open title input on a busy runner.
	session.waitContainsAfter(t, mark, "Session renamed: renamed")

	expiredReplacement := mintExpiredToken(t, stack, second.Session)
	mark = len(session.snapshot())
	session.command(t, 'i')
	session.waitContainsAfter(t, mark, "memory only")
	session.write(t, "\x1b[200~"+expiredReplacement+"\x1b[201~")
	session.key(t, '\r', 1)
	session.waitContains(t, "bearer token expired")
	session.key(t, 27, 1)
	// Closing the modal needs no repaint probe: the very next step reopens the
	// credential modal and waits on its title, which fails if this esc did not
	// land. (The inline renderer shifts rows on close without re-emitting
	// unchanged bytes, so there is nothing textual to wait for here.)
	validReplacement := mintPTYToken(t, stack, second.Session)
	session.command(t, 'i')
	// The reopened modal's title occupies the same cells as the one that just
	// closed, so when frames coalesce the cell-diff renderer never re-emits it.
	// Assert the reopen by its OUTCOME instead: the pasted replacement token is
	// accepted through the modal and the stream returns to live.
	session.write(t, "\x1b[200~"+validReplacement+"\x1b[201~")
	mark = len(session.snapshot())
	session.key(t, '\r', 1)
	session.waitContainsAfter(t, mark, "live")
	mark = len(session.snapshot())
	server.CloseClientConnections()
	session.waitContainsAfter(t, mark, "reconnecting")
	session.waitContainsAfter(t, mark, "live")

	for _, route := range []struct {
		key   rune
		label string
	}{
		{tea.KeyF2, "TREE / PROGRESS"},
		{tea.KeyF3, "transport / OAuth"},
		{tea.KeyF4, "metadata only"},
		{tea.KeyF5, "filter / raw safe payload"},
		{tea.KeyF6, "Capabilities:"},
		{tea.KeyF7, "retained by PauseToken"},
		{tea.KeyF8, "retention / capability"},
	} {
		mark = len(session.snapshot())
		session.functionKey(t, route.key)
		session.waitContainsAfter(t, mark, route.label)
	}
	mark = len(session.snapshot())
	session.functionKey(t, tea.KeyF9)
	session.waitContainsAfter(t, mark, "Runtime actions")
	session.key(t, '\r', 1)
	session.waitContains(t, "Unavailable: no artifact selected")
	session.key(t, 27, 1)
	session.functionKey(t, tea.KeyF9)
	session.text(t, "Delete session")
	session.key(t, '\r', 1)
	session.waitContains(t, "Confirm Runtime action")
	session.key(t, 27, 1)
	session.key(t, 27, 1)
	mark = len(session.snapshot())
	session.functionKey(t, tea.KeyF1)
	// F1 returns to the inline conversation surface: leaving the alternate
	// screen repaints the whole managed region, identity row included.
	session.waitContainsAfter(t, mark, "/"+second.Session)

	runtimeIdentity := identity.Identity{TenantID: second.Tenant, UserID: second.User, SessionID: second.Session}
	runtimeCtx, identityErr := identity.With(t.Context(), runtimeIdentity)
	if identityErr != nil {
		t.Fatal(identityErr)
	}
	if closeErr := stack.Sessions.Close(runtimeCtx, second.Session, "PTY reopen verification"); closeErr != nil {
		t.Fatal(closeErr)
	}
	session.waitContains(t, "Session completed")
	session.key(t, '\r', 1)
	await(t, func() bool {
		response, listErr := secondClient.TasksList(t.Context(), types.TaskListRequest{})
		if listErr != nil || len(response.Rows) == 0 {
			return false
		}
		for _, row := range response.Rows {
			if row.Status == types.TaskStatusPending || row.Status == types.TaskStatusRunning {
				return false
			}
		}
		return true
	}, "PTY submitted turn terminal before export")
	quad := identity.Quadruple{Identity: runtimeIdentity}
	// This task is manually controlled by the test. Keep it background-kind so
	// the devstack's foreground-only RunLoopDriver cannot race the explicit
	// MarkRunning/MarkComplete transitions below.
	holdTask, spawnErr := stack.Tasks.Spawn(runtimeCtx, tasks.SpawnRequest{Identity: quad, Kind: tasks.KindBackground, Description: "PTY follow-up hold", Query: "hold active work", IdempotencyKey: "pty-followup-hold"})
	if spawnErr != nil {
		t.Fatal(spawnErr)
	}
	if runningErr := stack.Tasks.MarkRunning(runtimeCtx, holdTask.ID); runningErr != nil {
		t.Fatal(runningErr)
	}
	await(t, func() bool {
		inspected, inspectErr := secondClient.SessionsInspect(t.Context(), types.SessionsInspectRequest{SessionID: second.Session})
		if inspectErr != nil || inspected.Row.Status != types.SessionStatusRunning {
			return false
		}
		response, listErr := secondClient.TasksList(t.Context(), types.TaskListRequest{})
		if listErr != nil {
			return false
		}
		for _, row := range response.Rows {
			if row.ID == string(holdTask.ID) {
				return row.Status == types.TaskStatusRunning
			}
		}
		return false
	}, "PTY reopened session and hold task canonical running state")
	// A running-to-running projection can leave the interrupt hint in unchanged
	// terminal cells; subsequent spinner ticks then emit only the glyph diff. Ask
	// Bubble Tea for a full frame after the canonical running acknowledgement so
	// this visual assertion observes the screen content, not an incidental byte
	// boundary in the diff stream. The in-flight verb varies per run; the
	// interrupt affordance is the stable marker of the working line.
	mark = len(session.snapshot())
	session.resize(t, 101, 30)
	session.waitContainsAfter(t, mark, "esc to interrupt")
	session.text(t, "recover queued follow-up")
	session.key(t, '\r', 1)
	session.waitContains(t, "queued locally")
	failNextStart.Store(true)
	if completeErr := stack.Tasks.MarkComplete(runtimeCtx, holdTask.ID, tasks.TaskResult{Value: []byte(`"complete"`)}); completeErr != nil {
		t.Fatal(completeErr)
	}
	session.waitContains(t, "HTTP 503")
	session.command(t, 'j')
	// One retry transitions the failed entry into an in-flight canonical Start.
	// Sending the chord again while that request is outstanding is not a second
	// retry: it targets no failed entry and only introduces an unrelated local
	// command error into this delivery assertion.
	await(t, func() bool {
		response, listErr := secondClient.TasksList(t.Context(), types.TaskListRequest{})
		if listErr != nil {
			return false
		}
		for _, row := range response.Rows {
			if strings.Contains(row.Query, "recover") {
				return true
			}
		}
		return false
	}, "PTY failed follow-up retry reaches canonical start")
	session.command(t, 'x')
	await(t, func() bool {
		body, err := os.ReadFile(exportPath)
		return err == nil && strings.Contains(string(body), "Harbor session")
	}, "PTY Markdown export")
	mark = len(session.snapshot())
	session.command(t, 'd')
	// Wait on the CONFIRMATION, not on the command's title: the which-key
	// overlay ctrl+x paints lists "ctrl+x d  Delete session" whether or not
	// the command is reachable (it only appends "· unavailable: <reason>"),
	// so waiting on "Delete session" passes identically on the failing path
	// and reports the real problem 20s later at the erase assertion. Only the
	// confirmation dialog proves the chord's command actually ran.
	session.waitContainsAfter(t, mark, "Confirm Delete session")
	session.key(t, '\r', 1)
	await(t, func() bool {
		_, inspectErr := secondClient.SessionsInspect(t.Context(), types.SessionsInspectRequest{SessionID: second.Session})
		return inspectErr != nil
	}, "PTY canonical session erase")
	// The canonical delete may finish before Bubble Tea has emitted a complete
	// visual frame. Cell-diff output can carry only the changed glyphs, which
	// makes a contiguous phrase an invalid acknowledgement of the rendered
	// erased state. Request a repaint after the authoritative deletion and
	// require the stable erased-state prefix; the exact Start Fresh guidance is
	// already pinned by the model golden tests and may be emitted as a suffix-only
	// cell update here.
	mark = len(session.snapshot())
	session.resize(t, 101, 30)
	session.waitContainsAfter(t, mark, "Session erased")
	// Start Fresh mints a random session id. A standalone attach holds only
	// the credentials in --token-file, so the switch must fail HONESTLY and
	// non-destructively — no dead disconnected surface, no dialog trap.
	mark = len(session.snapshot())
	session.command(t, 'n')
	session.waitContainsAfter(t, mark, "Switch failed")
	session.key(t, 'c', 5)
	session.waitExit(t, 0)
	session.assertOperationalRestored(t)
	// The pre-minted fresh session attaches via the explicit --session flag,
	// runs a turn, and its identity persists for the implicit restore below.
	freshPTY := startPTYCommand(t, binary, []string{"tui", "--attach", server.URL, "--token-file", tokenPath, "--state-file", statePath, "--session", fresh.Session}, temp, 80, 24)
	freshPTY.waitContains(t, fresh.Session)
	freshPTY.text(t, "fresh turn")
	freshPTY.key(t, '\r', 1)
	freshPTY.key(t, 'c', 5)
	freshPTY.waitExit(t, 0)
	freshPTY.assertOperationalRestored(t)
	stored := conversation.NewStore(statePath)
	info, infoErr := secondClient.RuntimeInfo(t.Context())
	if infoErr != nil {
		t.Fatal(infoErr)
	}
	fingerprint := info.InstanceID + "@" + info.WireSurfaceDigest
	state, ok, err := stored.Load(fresh, fingerprint)
	if err != nil || !ok {
		t.Fatalf("restored state=%#v ok=%v err=%v", state, ok, err)
	}
	restoredPTY := startPTYCommand(t, binary, []string{"tui", "--attach", server.URL, "--token-file", tokenPath, "--state-file", statePath}, temp, 80, 24)
	restoredPTY.waitContains(t, fresh.Session)
	restoredPTY.key(t, 'c', 5)
	restoredPTY.waitExit(t, 0)
	restoredPTY.assertOperationalRestored(t)
}

// TestE2E_TUIFunctionKeys_KittyCSIU exercises the real PTY decoder after the
// host has negotiated Kitty keyboard mode. The full workflow relies on these
// exact F1-F9 inputs for route changes; legacy SS3/CSI-tilde encodings are not
// a truthful terminal oracle once CSI-u is active.
func TestE2E_TUIFunctionKeys_KittyCSIU(t *testing.T) {
	session := startPTY(t, "function-keys", 80, 24)
	session.waitContains(t, "function keys ready")
	for _, key := range [...]rune{tea.KeyF1, tea.KeyF2, tea.KeyF3, tea.KeyF4, tea.KeyF5, tea.KeyF6, tea.KeyF7, tea.KeyF8, tea.KeyF9} {
		session.functionKey(t, key)
	}
	session.waitContains(t, "all function keys decoded")
	session.key(t, 'c', 1)
	session.waitExit(t, 0)
	// This decoder fixture mounts app.Run directly, not the CLI's RunTerminal
	// wrapper, so it asserts restoration only for modes the fixture enabled.
	session.assertEnabledModesRestored(t)
}

func awaitPTYDraftPersisted(t *testing.T, path string, scope types.IdentityScope, want string) {
	t.Helper()
	await(t, func() bool {
		body, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		var file struct {
			Entries map[string]struct {
				Identity types.IdentityScope `json:"identity"`
				Draft    string              `json:"draft"`
			} `json:"entries"`
		}
		if json.Unmarshal(body, &file) != nil {
			return false
		}
		for _, entry := range file.Entries {
			if entry.Identity == scope && entry.Draft == want {
				return true
			}
		}
		return false
	}, "PTY draft persistence acknowledgement")
}

func TestE2E_TUIRuntimeControlPTY_MultiplePauseTokensAndReconciliation(t *testing.T) {
	stack := devstack.Assemble(t, runtimePostureConfig(t), devstack.AssembleOpts{})
	defer stack.Close()
	var forcedStatus atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status := int(forcedStatus.Load()); status != 0 && r.URL.Path == "/v1/control/approve" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(protoerrors.Error{Code: protoerrors.CodeRuntimeError, Message: "forced PTY operation failure"})
			return
		}
		stack.Handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	scope := types.IdentityScope{Tenant: devstack.DefaultDevTenant, User: devstack.DefaultDevUser, Session: devstack.DefaultDevSession}
	id := identity.Identity{TenantID: scope.Tenant, UserID: scope.User, SessionID: scope.Session}
	ctx, err := identity.With(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	task, err := stack.Tasks.Spawn(ctx, tasks.SpawnRequest{Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground, Description: "PTY controls", Query: "hold", IdempotencyKey: "pty-runtime-controls"})
	if err != nil {
		t.Fatal(err)
	}
	if err = stack.Tasks.MarkRunning(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	quad := identity.Quadruple{Identity: id, RunID: string(task.ID)}
	inbox, err := stack.Steering.Open(quad)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = stack.Tasks.Cancel(ctx, task.ID, "test cleanup"); _ = stack.Steering.Retire(quad) }()
	runCtx, err := identity.WithRun(ctx, id, string(task.ID))
	if err != nil {
		t.Fatal(err)
	}
	request := func(reason pauseresume.Reason, payload map[string]any) pauseresume.Token {
		pause, requestErr := stack.Coordinator.Request(runCtx, pauseresume.PauseRequest{Identity: id, Reason: reason, Payload: payload})
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		return pause.Token
	}
	basePayload := func() map[string]any { return map[string]any{"run_id": string(task.ID)} }
	expired := request(pauseresume.ReasonExternalEvent, map[string]any{"run_id": string(task.ID), "expires_at": time.Now().Add(-time.Minute).Format(time.RFC3339)})
	oauth := request(pauseresume.ReasonExternalEvent, basePayload())
	input := request(pauseresume.ReasonAwaitInput, basePayload())
	reject := request(pauseresume.ReasonApprovalRequired, basePayload())
	approve := request(pauseresume.ReasonApprovalRequired, basePayload())

	temp := t.TempDir()
	binary := filepath.Join(temp, "harbor")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/harbor")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("build harbor CLI: %v\n%s", buildErr, output)
	}
	tokenPath := filepath.Join(temp, "tokens.json")
	writePTYTokens(t, tokenPath, map[string]string{scopeTokenKey(scope): stack.Token})
	runFailure := func(status int) {
		token := request(pauseresume.ReasonApprovalRequired, basePayload())
		forcedStatus.Store(int32(status))
		pty := startPTYCommand(t, binary, []string{"tui", "--attach", server.URL, "--token-file", tokenPath, "--state-file", filepath.Join(temp, fmt.Sprintf("state-error-%d.json", status)), "--session", scope.Session}, temp, 100, 30)
		pty.waitContains(t, "live")
		pty.write(t, "\x1b[18~")
		pty.waitContains(t, string(token))
		pty.write(t, "\x1b[20~")
		pty.text(t, "Approve intervention")
		pty.key(t, '\r', 1)
		pty.waitContains(t, "Confirm Runtime action")
		mark := len(pty.snapshot())
		pty.key(t, '\r', 1)
		pty.waitContainsAfter(t, mark, fmt.Sprintf("HTTP %d", status))
		pty.key(t, 'c', 5)
		pty.waitExit(t, 0)
		forcedStatus.Store(0)
		if resumeErr := stack.Coordinator.Resume(runCtx, token, pauseresume.DecisionReject, nil); resumeErr != nil {
			t.Fatal(resumeErr)
		}
	}
	runFailure(http.StatusConflict)
	runFailure(http.StatusNotImplemented)
	controls := []struct {
		title, input, control string
		token                 pauseresume.Token
		decision              pauseresume.Decision
	}{{"Approve intervention", "", "APPROVE", approve, pauseresume.DecisionApprove}, {"Reject intervention", "unsafe", "REJECT", reject, pauseresume.DecisionReject}, {"Submit required input", "operator input", "RESUME", input, pauseresume.DecisionResume}, {"Submit OAuth intervention", "oauth complete", "RESUME", oauth, pauseresume.DecisionResume}}
	for index, control := range controls {
		pty := startPTYCommand(t, binary, []string{"tui", "--attach", server.URL, "--token-file", tokenPath, "--state-file", filepath.Join(temp, fmt.Sprintf("state-%d.json", index)), "--session", scope.Session}, temp, 100, 30)
		pty.waitContains(t, "live")
		pty.write(t, "\x1b[18~")
		pty.waitContains(t, string(control.token))
		pty.write(t, "\x1b[20~")
		pty.waitContains(t, "Runtime actions")
		pty.text(t, control.title)
		pty.key(t, '\r', 1)
		if control.input != "" {
			pty.waitContains(t, "Action input")
			pty.text(t, control.input)
			pty.key(t, '\r', 1)
		}
		pty.waitContains(t, "Confirm Runtime action")
		mark := len(pty.snapshot())
		pty.key(t, '\r', 1)
		var event steering.ControlEvent
		await(t, func() bool {
			values, drainErr := inbox.Drain()
			if drainErr != nil {
				t.Fatal(drainErr)
			}
			if len(values) == 0 {
				return false
			}
			event = values[len(values)-1]
			return true
		}, "PTY canonical "+control.control)
		if string(event.Type) != control.control || event.Payload["token"] != string(control.token) {
			t.Fatalf("control=%#v", event)
		}
		// Runtime routes render on the alternate screen with a plain route header.
		pty.waitContainsAfter(t, mark, "INTERVENTION INBOX")
		if err = stack.Coordinator.Resume(runCtx, control.token, control.decision, nil); err != nil {
			t.Fatal(err)
		}
		status, statusErr := stack.Coordinator.Status(runCtx, control.token)
		if statusErr != nil || status.State != pauseresume.StatusResumed || status.Decision != control.decision {
			t.Fatalf("resolved status=%#v err=%v", status, statusErr)
		}
		pty.key(t, 'c', 5)
		pty.waitExit(t, 0)
		pty.assertOperationalRestored(t)
	}
	pty := startPTYCommand(t, binary, []string{"tui", "--attach", server.URL, "--token-file", tokenPath, "--state-file", filepath.Join(temp, "state-expired.json"), "--session", scope.Session}, temp, 100, 30)
	pty.waitContains(t, "live")
	pty.write(t, "\x1b[18~")
	pty.waitContains(t, string(expired))
	pty.waitContains(t, "expired")
	pty.key(t, 'c', 5)
	pty.waitExit(t, 0)
	pty.assertOperationalRestored(t)
}

func TestE2E_TUITerminalPTY_RestoreResizeSuspendSignalsAndPanic(t *testing.T) {
	baseline := runtime.NumGoroutine()
	t.Run("normal-resize-suspend", func(t *testing.T) {
		session := startPTY(t, "normal", 80, 24)
		session.waitContains(t, "Terminal foundation")
		session.resize(t, 100, 30)
		session.waitContains(t, "test/operator/terminal-foundation")
		if err := syscall.Kill(-session.cmd.Process.Pid, syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		await(t, func() bool { return processStopped(session.cmd.Process.Pid) }, "PTY child enters stopped state")
		if err := syscall.Kill(-session.cmd.Process.Pid, syscall.SIGCONT); err != nil {
			t.Fatal(err)
		}
		session.resize(t, 101, 30)
		session.write(t, "q")
		session.waitExit(t, 0)
		session.assertRestored(t)
	})
	for _, signal := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(signal.String(), func(t *testing.T) {
			session := startPTY(t, "normal", 80, 24)
			session.waitContains(t, "\x1b[?1049h")
			if err := session.cmd.Process.Signal(signal); err != nil {
				t.Fatal(err)
			}
			session.waitExitAny(t)
			session.assertRestored(t)
		})
	}
	t.Run("panic", func(t *testing.T) {
		session := startPTY(t, "panic", 80, 24)
		session.waitContains(t, "Terminal foundation")
		session.write(t, "x")
		session.waitExit(t, 23)
		session.waitContains(t, "HOST_ERROR:")
		session.assertRestored(t)
	})
	t.Run("program-error", func(t *testing.T) {
		session := startPTY(t, "error", 80, 24)
		session.waitContains(t, "Terminal foundation")
		session.write(t, "x")
		session.waitExit(t, 23)
		session.waitContains(t, "HOST_ERROR:")
		session.assertRestored(t)
	})
	t.Run("partial-startup-fault", func(t *testing.T) {
		session := startPTY(t, "startup-fault", 80, 24)
		session.waitExit(t, 23)
		session.waitContains(t, "HOST_ERROR:")
		session.assertEnabledModesRestored(t)
	})
	for i := range 5 {
		session := startPTY(t, "normal", 40+i, 12)
		session.waitContains(t, "\x1b[?1049h")
		session.write(t, "q")
		session.waitExit(t, 0)
		session.assertRestored(t)
	}
	await(t, func() bool { return runtime.NumGoroutine() <= baseline+2 }, "PTY reader goroutines return to baseline")
}

type ptySession struct {
	cmd     *exec.Cmd
	master  *os.File
	mu      sync.Mutex
	output  bytes.Buffer
	changed chan struct{}
	done    chan error
	exited  chan struct{}
	// readerDone closes only after the master reader has appended the final
	// PTY frame; process exit can happen before that read completes.
	readerDone chan struct{}
	// exitErr is written before exited closes, so readers that observe exited
	// can report it without consuming done (waitExit owns that channel).
	exitErr error
}

func startPTY(t *testing.T, mode string, width, height int) *ptySession {
	return startPTYEnv(t, mode, width, height, nil)
}
func startPTYEnv(t *testing.T, mode string, width, height int, extraEnv []string) *ptySession {
	t.Helper()
	master, slave, err := openPTY(width, height)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestE2E_TUITerminalHelper$", "-test.v=false")
	// Hermetic HOME for the whole PTY class (see ptyHermeticEnv): today this
	// helper drives an in-process model that keeps no state, but a mode that
	// reaches entry.Run would otherwise silently start writing the operator's
	// real ~/.harbor.
	cmd.Env = ptyHermeticEnv(t, append([]string{ptyHelperEnv + "=" + mode}, extraEnv...))
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err = cmd.Start(); err != nil {
		_ = master.Close()
		_ = slave.Close()
		t.Fatal(err)
	}
	_ = slave.Close()
	s := &ptySession{cmd: cmd, master: master, changed: make(chan struct{}, 1), done: make(chan error, 1), exited: make(chan struct{}), readerDone: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-s.exited:
		default:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		_ = master.Close()
	})
	go readPTY(s)
	go waitPTYCommand(s)
	return s
}

// ptyHermeticEnv builds the environment for a PTY-launched Harbor binary.
// It pins HOME to a fresh per-launch directory so everything the TUI keeps
// under ~/.harbor — the interaction-state file and the transcript export
// directory — lands inside the test's temp tree instead of the operator's
// real home (CLAUDE.md §11: tests never write outside the tree).
//
// This is load-bearing, not hygiene. Co-launch modes (`harbor serve --tui`
// and a scaffolded --with-tui binary) expose no --state-file flag: the TUI
// resolves $HOME/.harbor/tui-state.json. Inheriting the real HOME makes the
// suite self-poisoning — a run persists the composer draft it typed, and the
// NEXT run restores that draft into the composer, so the placeholder never
// renders and every "Ask Harbor" assertion times out from the second run
// onward, permanently, on any machine that has run the suite once.
//
// os/exec keeps the LAST value for a duplicated key, so a caller that needs
// to observe the state file supplies its own HOME in extraEnv and wins.
func ptyHermeticEnv(t *testing.T, extraEnv []string) []string {
	t.Helper()
	env := append(os.Environ(), "TERM=xterm-256color", "NO_COLOR=", "HOME="+t.TempDir())
	return append(env, extraEnv...)
}

func startPTYCommand(t *testing.T, binary string, args []string, workdir string, width, height int) *ptySession {
	t.Helper()
	master, slave, err := openPTY(width, height)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = workdir
	cmd.Env = ptyHermeticEnv(t, nil)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err = cmd.Start(); err != nil {
		_ = master.Close()
		_ = slave.Close()
		t.Fatal(err)
	}
	_ = slave.Close()
	s := &ptySession{
		cmd:        cmd,
		master:     master,
		changed:    make(chan struct{}, 1),
		done:       make(chan error, 1),
		exited:     make(chan struct{}),
		readerDone: make(chan struct{}),
	}
	t.Cleanup(func() {
		select {
		case <-s.exited:
		default:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		_ = master.Close()
	})
	go readPTY(s)
	go waitPTYCommand(s)
	return s
}

func waitPTYCommand(s *ptySession) {
	err := s.cmd.Wait()
	s.exitErr = err
	close(s.exited)
	// The child closing its PTY slave makes the master reader return after it
	// has consumed the queued terminal cleanup frame. Do not close the master
	// here first: that races the final read and can discard those bytes. A test
	// timeout still closes the master from Cleanup, which releases this wait.
	<-s.readerDone
	_ = s.master.Close()
	s.done <- err
}

func readPTY(s *ptySession) {
	defer close(s.readerDone)
	buffer := make([]byte, 4096)
	for {
		n, readErr := s.master.Read(buffer)
		if n > 0 {
			s.mu.Lock()
			_, _ = s.output.Write(buffer[:n])
			s.mu.Unlock()
			select {
			case s.changed <- struct{}{}:
			default:
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) && !strings.Contains(readErr.Error(), "input/output error") {
				s.mu.Lock()
				_, _ = fmt.Fprintf(&s.output, "\nPTY_READ_ERROR:%v", readErr)
				s.mu.Unlock()
			}
			return
		}
	}
}

func scopeTokenKey(scope types.IdentityScope) string {
	return scope.Tenant + "/" + scope.User + "/" + scope.Session
}
func writePTYTokens(t *testing.T, path string, tokens map[string]string) {
	t.Helper()
	body, err := json.Marshal(tokens)
	if err != nil {
		t.Fatal(err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}
func mintPTYToken(t *testing.T, stack *devstack.DevStack, session string) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"iss": "harbor-test", "sub": devstack.DefaultDevUser, "aud": "harbor", "exp": time.Now().Add(time.Hour).Unix(), "nbf": time.Now().Add(-time.Minute).Unix(), "iat": time.Now().Unix(), "tenant": devstack.DefaultDevTenant, "user": devstack.DefaultDevUser, "session": session, "scopes": []string{"admin", "console:fleet"}, "agent_reach": []string{stack.AgentConfigID}})
	token.Header["kid"] = stack.KID
	signed, err := token.SignedString(stack.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}
func (s *ptySession) write(t *testing.T, value string) {
	t.Helper()
	if _, err := io.WriteString(s.master, value); err != nil {
		t.Fatal(err)
	}
}
func (s *ptySession) key(t *testing.T, code rune, modifier int) {
	t.Helper()
	if modifier <= 1 {
		s.write(t, fmt.Sprintf("\x1b[%du", code))
		return
	}
	s.write(t, fmt.Sprintf("\x1b[%d;%du", code, modifier))
}

func (s *ptySession) functionKey(t *testing.T, code rune) {
	t.Helper()
	if code < tea.KeyF1 || code > tea.KeyF12 {
		t.Fatalf("function key code %d is outside F1-F12", code)
	}
	// tea.KeyF* values are Bubble Tea's internal key enum. Kitty CSI-u does
	// not put that enum on the wire: the protocol assigns F1 the Unicode
	// functional-key codepoint 57364 and the remaining function keys follow
	// contiguously. Sending the internal enum produces an unrelated control
	// code that the decoder may surface as U+FFFD instead of a function key.
	const kittyF1 = 57364
	s.key(t, kittyF1+(code-tea.KeyF1), 1)
}
func (s *ptySession) text(t *testing.T, value string) {
	t.Helper()
	s.write(t, value)
}
func (s *ptySession) command(t *testing.T, key rune) {
	t.Helper()
	s.key(t, 'x', 5)
	s.key(t, key, 1)
}
func (s *ptySession) resize(t *testing.T, width, height int) {
	t.Helper()
	before := len(s.snapshot())
	if err := resizePTY(s.master, width, height); err != nil {
		t.Fatal(err)
	}
	actualWidth, actualHeight, err := ptySize(s.master)
	if err != nil || actualWidth != width || actualHeight != height {
		t.Fatalf("PTY size=%dx%d want=%dx%d err=%v", actualWidth, actualHeight, width, height, err)
	}
	await(t, func() bool { return len(s.snapshot()) > before }, fmt.Sprintf("resize render %dx%d", width, height))
}
func (s *ptySession) snapshot() string { s.mu.Lock(); defer s.mu.Unlock(); return s.output.String() }
func (s *ptySession) waitContains(t *testing.T, needle string) {
	t.Helper()
	timer := time.NewTimer(ptyWaitTimeout)
	defer timer.Stop()
	for {
		output := s.snapshot()
		if strings.Contains(output, needle) || strings.Contains(normalizePTY(output), needle) {
			return
		}
		select {
		case <-timer.C:
			s.failWait(t, needle, output, false)
		case <-s.changed:
		case <-s.exited:
			s.awaitReaderDrain()
			output = s.snapshot()
			if strings.Contains(output, needle) || strings.Contains(normalizePTY(output), needle) {
				return
			}
			s.failWait(t, needle, output, false)
		}
	}
}
func (s *ptySession) waitContainsAfter(t *testing.T, offset int, needle string) {
	t.Helper()
	timer := time.NewTimer(ptyWaitTimeout)
	defer timer.Stop()
	for {
		output := s.snapshot()
		if offset > len(output) {
			offset = 0
		}
		recent := output[offset:]
		if strings.Contains(recent, needle) || strings.Contains(normalizePTY(recent), needle) {
			return
		}
		select {
		case <-timer.C:
			s.failWait(t, needle, recent, true)
		case <-s.changed:
		case <-s.exited:
			s.awaitReaderDrain()
			output = s.snapshot()
			if offset > len(output) {
				offset = 0
			}
			recent = output[offset:]
			if strings.Contains(recent, needle) || strings.Contains(normalizePTY(recent), needle) {
				return
			}
			s.failWait(t, needle, recent, true)
		}
	}
}

// awaitReaderDrain gives the PTY reader one bounded chance to append the
// child's final frame after cmd.Wait reports exit. PTYs can report process
// exit before their master has yielded the final bytes; failing immediately
// would turn that ordering into a false test failure. This drains once only —
// it is not a retry or a longer assertion timeout.
func (s *ptySession) awaitReaderDrain() {
	select {
	case <-s.readerDone:
	case <-time.After(250 * time.Millisecond):
	}
}

// failWait turns the previously opaque PTY timeout into a liveness diagnosis.
// If the child is still alive, SIGQUIT asks the Go runtime for every goroutine
// stack before the test fails; if it already exited, the exit result and PTY
// tail distinguish a dead child from a stalled renderer. This is failure-only
// instrumentation, never a retry or a larger timeout.
func (s *ptySession) failWait(t *testing.T, needle, observed string, recent bool) {
	t.Helper()
	live := true
	select {
	case <-s.exited:
		live = false
	default:
	}
	diagnosticOffset := len(s.snapshot())
	if live && s.cmd.Process != nil {
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGQUIT)
		select {
		case <-s.changed:
		case <-time.After(250 * time.Millisecond):
		}
	}
	select {
	case <-s.exited:
		s.awaitReaderDrain()
		live = false
	default:
	}
	output := s.snapshot()
	diagnostic := ""
	if diagnosticOffset < len(output) {
		diagnostic = output[diagnosticOffset:]
		const diagnosticBytes = 65536
		if len(diagnostic) > diagnosticBytes {
			diagnostic = diagnostic[:diagnosticBytes]
		}
	}
	tail := output
	const tailBytes = 8192
	if len(tail) > tailBytes {
		tail = tail[len(tail)-tailBytes:]
	}
	exit := "still-running"
	if !live {
		exit = fmt.Sprintf("exited: %v", s.exitErr)
	}
	if recent {
		t.Fatalf("timeout waiting for new PTY output %q; child=%s total_bytes=%d observed=%q tail=%q sigquit=%q", needle, exit, len(output), ansi.Strip(observed), ansi.Strip(tail), ansi.Strip(diagnostic))
	}
	t.Fatalf("timeout waiting for PTY output %q; child=%s total_bytes=%d tail=%q sigquit=%q", needle, exit, len(output), ansi.Strip(tail), ansi.Strip(diagnostic))
}
func (s *ptySession) waitExit(t *testing.T, want int) {
	t.Helper()
	err := waitChannel(t, s.done, "PTY process exit")
	var exit *exec.ExitError
	got := 0
	if err != nil {
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
		got = exit.ExitCode()
	}
	if got != want {
		t.Fatalf("exit=%d want=%d err=%v output=%q", got, want, err, s.snapshot())
	}
}

// normalizePTY reduces a raw PTY byte stream to comparable text: ANSI escapes
// are stripped and backspace is applied (dropping the previous character), so
// the inline renderer's space+backspace cursor parking does not fragment words
// across the stream.
func normalizePTY(raw string) string {
	stripped := ansi.Strip(raw)
	out := make([]rune, 0, len(stripped))
	for _, r := range stripped {
		if r == '\b' {
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
			continue
		}
		out = append(out, r)
	}
	return string(out)
}

func (s *ptySession) waitExitAny(t *testing.T) {
	t.Helper()
	_ = waitChannel(t, s.done, "signalled PTY process exit")
}
func (s *ptySession) assertRestored(t *testing.T) {
	t.Helper()
	output := s.snapshot()
	for _, sequence := range []string{"\x1b[?1049h", "\x1b[?25l", "\x1b[?25h", "\x1b[?1049l", "\x1b[?2004h", "\x1b[?2004l", "\x1b[?1004h", "\x1b[?1004l", "\x1b[>4;2m", "\x1b[>4m", "\x1b[=1;1u", "\x1b[=0;1u"} {
		if !strings.Contains(output, sequence) {
			t.Errorf("terminal sequence %q missing from %q", sequence, output)
		}
	}
}

func (s *ptySession) assertOperationalRestored(t *testing.T) {
	t.Helper()
	output := s.snapshot()
	for _, sequence := range []string{"\x1b[?1049h", "\x1b[?25h", "\x1b[?1049l", "\x1b[?2004h", "\x1b[?2004l", "\x1b[?1004h", "\x1b[?1004l", "\x1b[>4;2m", "\x1b[>4m", "\x1b[=1;1u", "\x1b[=0;1u"} {
		if !strings.Contains(output, sequence) {
			t.Errorf("operational terminal sequence %q missing from %q", sequence, output)
		}
	}
}

func (s *ptySession) assertEnabledModesRestored(t *testing.T) {
	t.Helper()
	output := s.snapshot()
	pairs := [][2]string{{"\x1b[?1049h", "\x1b[?1049l"}, {"\x1b[?25l", "\x1b[?25h"}, {"\x1b[?2004h", "\x1b[?2004l"}, {"\x1b[?1004h", "\x1b[?1004l"}, {"\x1b[>4;2m", "\x1b[>4m"}, {"\x1b[=1;1u", "\x1b[=0;1u"}}
	for _, pair := range pairs {
		if strings.Contains(output, pair[0]) && !strings.Contains(output, pair[1]) {
			t.Errorf("enabled terminal mode %q not restored by %q", pair[0], pair[1])
		}
	}
}

func processStopped(pid int) bool {
	output, err := exec.Command("ps", "-o", "state=", "-p", fmt.Sprint(pid)).Output()
	return err == nil && strings.Contains(string(output), "T")
}
func await(t *testing.T, condition func() bool, label string) {
	t.Helper()
	timer := time.NewTimer(ptyWaitTimeout)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-timer.C:
			t.Fatalf("timeout waiting for %s", label)
		case <-ticker.C:
		}
	}
}
func waitChannel(t *testing.T, ch <-chan error, label string) error {
	t.Helper()
	timer := time.NewTimer(ptyWaitTimeout)
	defer timer.Stop()
	select {
	case err := <-ch:
		return err
	case <-timer.C:
		t.Fatalf("timeout waiting for %s", label)
		return nil
	}
}
