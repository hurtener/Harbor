//go:build darwin || linux

package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/hurtener/Harbor/internal/tui/app"
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
	model := app.NewModelFromEnvironment(80, 24, ui.EnvironmentFrom(os.LookupEnv), true, app.FixtureProjection()).WithState(app.State{CursorHidden: true})
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
		os.Exit(23)
	}
	os.Exit(0)
}

func TestE2E_TUITerminalPTY_RestoreResizeSuspendSignalsAndPanic(t *testing.T) {
	baseline := runtime.NumGoroutine()
	t.Run("normal-resize-suspend", func(t *testing.T) {
		session := startPTY(t, "normal", 80, 24)
		session.waitContains(t, "terminal size 80x24")
		session.resize(t, 100, 30)
		session.waitContains(t, "Harbor fixture 100x30")
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
		session.waitContains(t, "terminal size 80x24")
		session.write(t, "x")
		session.waitExit(t, 23)
		session.waitContains(t, "HOST_ERROR:")
		session.assertRestored(t)
	})
	t.Run("program-error", func(t *testing.T) {
		session := startPTY(t, "error", 80, 24)
		session.waitContains(t, "terminal size 80x24")
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
}

func startPTY(t *testing.T, mode string, width, height int) *ptySession {
	t.Helper()
	master, slave, err := openPTY(width, height)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestE2E_TUITerminalHelper$", "-test.v=false")
	cmd.Env = append(os.Environ(), ptyHelperEnv+"="+mode, "TERM=xterm-256color", "NO_COLOR=")
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
	s := &ptySession{cmd: cmd, master: master, changed: make(chan struct{}, 1), done: make(chan error, 1)}
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
					s.done <- readErr
				}
				return
			}
		}
	}()
	go func() { s.done <- cmd.Wait(); _ = master.Close() }()
	return s
}
func (s *ptySession) write(t *testing.T, value string) {
	t.Helper()
	if _, err := io.WriteString(s.master, value); err != nil {
		t.Fatal(err)
	}
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
	await(t, func() bool {
		output := s.snapshot()
		return strings.Contains(output, needle) || strings.Contains(ansi.Strip(output), needle)
	}, "PTY output contains "+fmt.Sprintf("%q", needle))
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
