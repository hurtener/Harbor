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
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tui/app"
	"github.com/hurtener/Harbor/internal/tui/conversation"
	"github.com/hurtener/Harbor/internal/tui/projection"
	"github.com/hurtener/Harbor/internal/tui/ui"
)

const ptyHelperEnv = "HARBOR_TUI_PTY_HELPER"

type panicModel struct{ app.Model }
type errorModel struct{ app.Model }
type startupFaultModel struct{ app.Model }
type suspendModel struct{ app.Model }

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
	session.waitContains(t, "hello")
	session.write(t, "\x1b[1;2D")
	session.key(t, 'o', 1)
	session.key(t, '!', 1)
	session.key(t, '_', 5)
	session.key(t, '_', 3)
	mark := len(session.snapshot())
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
	session.waitContains(t, "o · 0 matches")
	mark = len(session.snapshot())
	session.key(t, 27, 1)
	session.waitContainsAfter(t, mark, "closed")
	session.write(t, "\x1b[5~")
	session.resize(t, 100, 30)
	mark = len(session.snapshot())
	session.command(t, 'c')
	session.waitContainsAfter(t, mark, "Native scrollback on")
	mark = len(session.snapshot())
	session.command(t, 'c')
	session.waitContainsAfter(t, mark, "Native scrollback off")
	mark = len(session.snapshot())
	session.command(t, 'a')
	session.waitContainsAfter(t, mark, "Attach file · path|disposition")
	session.text(t, "missing.txt|context")
	session.key(t, '\r', 1)
	session.waitContains(t, "read attachment")
	if err := os.WriteFile(filepath.Join(temp, "missing.txt"), []byte("retry payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	session.command(t, 'u')
	session.waitContains(t, "attachment · missing.txt")
	session.command(t, 'e')
	session.waitContains(t, "Attachment removed locally")
	mark = len(session.snapshot())
	session.command(t, 'a')
	session.waitContainsAfter(t, mark, "Attach file · path|disposition")
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
	session.waitContains(t, second.Session+"  ·  ctrl+p commands")
	mark = len(session.snapshot())
	session.key(t, 'r', 5)
	session.waitContainsAfter(t, mark, "Rename session")
	session.text(t, "renamed")
	session.key(t, '\r', 1)
	await(t, func() bool {
		response, inspectErr := secondClient.SessionsInspect(t.Context(), types.SessionsInspectRequest{SessionID: second.Session})
		return inspectErr == nil && response.Row.Title == "renamed"
	}, "PTY canonical session rename")

	expiredReplacement := mintExpiredToken(t, stack, second.Session)
	mark = len(session.snapshot())
	session.command(t, 'i')
	session.waitContainsAfter(t, mark, "memory only")
	session.write(t, "\x1b[200~"+expiredReplacement+"\x1b[201~")
	session.key(t, '\r', 1)
	session.waitContains(t, "bearer token expired")
	mark = len(session.snapshot())
	session.key(t, 27, 1)
	session.waitContainsAfter(t, mark, "ctrl+p commands")
	validReplacement := mintPTYToken(t, stack, second.Session)
	mark = len(session.snapshot())
	session.command(t, 'i')
	session.waitContainsAfter(t, mark, "memory only")
	session.write(t, "\x1b[200~"+validReplacement+"\x1b[201~")
	mark = len(session.snapshot())
	session.key(t, '\r', 1)
	session.waitContainsAfter(t, mark, "live")
	mark = len(session.snapshot())
	server.CloseClientConnections()
	session.waitContainsAfter(t, mark, "reconnecting")
	session.waitContainsAfter(t, mark, "live")

	runtimeIdentity := identity.Identity{TenantID: second.Tenant, UserID: second.User, SessionID: second.Session}
	runtimeCtx, identityErr := identity.With(t.Context(), runtimeIdentity)
	if identityErr != nil {
		t.Fatal(identityErr)
	}
	if closeErr := stack.Sessions.Close(runtimeCtx, second.Session, "PTY reopen verification"); closeErr != nil {
		t.Fatal(closeErr)
	}
	session.waitContains(t, "Session closed")
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
	mark = len(session.snapshot())
	holdTask, spawnErr := stack.Tasks.Spawn(runtimeCtx, tasks.SpawnRequest{Identity: quad, Kind: tasks.KindForeground, Description: "PTY follow-up hold", Query: "hold active work", IdempotencyKey: "pty-followup-hold"})
	if spawnErr != nil {
		t.Fatal(spawnErr)
	}
	if runningErr := stack.Tasks.MarkRunning(runtimeCtx, holdTask.ID); runningErr != nil {
		t.Fatal(runningErr)
	}
	session.waitContainsAfter(t, mark, "running")
	session.text(t, "recover queued follow-up")
	session.key(t, '\r', 1)
	session.waitContains(t, "queued locally")
	failNextStart.Store(true)
	if completeErr := stack.Tasks.MarkComplete(runtimeCtx, holdTask.ID, tasks.TaskResult{Value: []byte(`"complete"`)}); completeErr != nil {
		t.Fatal(completeErr)
	}
	session.waitContains(t, "HTTP 503")
	session.command(t, 'j')
	session.command(t, 'j')
	session.waitContains(t, "Retrying failed follow-up")
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
	session.waitContainsAfter(t, mark, "Delete session")
	session.key(t, '\r', 1)
	await(t, func() bool {
		_, inspectErr := secondClient.SessionsInspect(t.Context(), types.SessionsInspectRequest{SessionID: second.Session})
		return inspectErr != nil
	}, "PTY canonical session erase")
	session.waitContains(t, "start fresh required")
	mark = len(session.snapshot())
	session.command(t, 'n')
	session.waitContainsAfter(t, mark, "Start Fresh")
	session.text(t, fresh.Session)
	session.key(t, '\r', 1)
	session.waitContains(t, fresh.Session)
	session.text(t, "fresh turn")
	session.key(t, '\r', 1)
	session.key(t, 'c', 5)
	session.waitExit(t, 0)
	session.assertOperationalRestored(t)
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
	cmd.Env = append(os.Environ(), ptyHelperEnv+"="+mode, "TERM=xterm-256color", "NO_COLOR=")
	cmd.Env = append(cmd.Env, extraEnv...)
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
	s := &ptySession{cmd: cmd, master: master, changed: make(chan struct{}, 1), done: make(chan error, 1), exited: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-s.exited:
		default:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		_ = master.Close()
	})
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, readErr := master.Read(buffer)
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
	}()
	go func() { s.done <- cmd.Wait(); close(s.exited); _ = master.Close() }()
	return s
}

func startPTYCommand(t *testing.T, binary string, args []string, workdir string, width, height int) *ptySession {
	t.Helper()
	master, slave, err := openPTY(width, height)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "NO_COLOR=")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err = cmd.Start(); err != nil {
		_ = master.Close()
		_ = slave.Close()
		t.Fatal(err)
	}
	_ = slave.Close()
	s := &ptySession{cmd: cmd, master: master, changed: make(chan struct{}, 1), done: make(chan error, 1), exited: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-s.exited:
		default:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		_ = master.Close()
	})
	go readPTY(s)
	go func() { s.done <- cmd.Wait(); close(s.exited); _ = master.Close() }()
	return s
}

func readPTY(s *ptySession) {
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
	token := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"iss": "harbor-test", "sub": devstack.DefaultDevUser, "aud": "harbor", "exp": time.Now().Add(time.Hour).Unix(), "nbf": time.Now().Add(-time.Minute).Unix(), "iat": time.Now().Unix(), "tenant": devstack.DefaultDevTenant, "user": devstack.DefaultDevUser, "session": session, "scopes": []string{"admin", "console:fleet"}})
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
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		output := s.snapshot()
		if strings.Contains(output, needle) || strings.Contains(ansi.Strip(output), needle) {
			return
		}
		select {
		case <-timer.C:
			t.Fatalf("timeout waiting for PTY output %q; output=%q", needle, ansi.Strip(output))
		case <-ticker.C:
		}
	}
}
func (s *ptySession) waitContainsAfter(t *testing.T, offset int, needle string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		output := s.snapshot()
		if offset > len(output) {
			offset = 0
		}
		recent := output[offset:]
		if strings.Contains(recent, needle) || strings.Contains(ansi.Strip(recent), needle) {
			return
		}
		select {
		case <-timer.C:
			t.Fatalf("timeout waiting for new PTY output %q; recent=%q", needle, ansi.Strip(recent))
		case <-ticker.C:
		}
	}
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
	timer := time.NewTimer(5 * time.Second)
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
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case err := <-ch:
		return err
	case <-timer.C:
		t.Fatalf("timeout waiting for %s", label)
		return nil
	}
}
