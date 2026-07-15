package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tui/conversation"
	"github.com/hurtener/Harbor/internal/tui/projection"
	"github.com/hurtener/Harbor/internal/tui/ui"
)

var updateCaptures = flag.Bool("update-captures", false, "regenerate reviewed-by-orchestrator TUI capture candidates")
var goldenSizes = [][2]int{{40, 12}, {60, 16}, {72, 20}, {79, 24}, {80, 24}, {100, 30}, {120, 30}, {121, 30}, {160, 40}, {240, 60}}

type captureScenario struct {
	name  string
	build func(int, int) Model
}

type operationalCaptureScenario struct {
	name  string
	build func(int, int) RuntimeModel
}

func operationalScenarios() []operationalCaptureScenario {
	base := func(w, h int) RuntimeModel {
		id := types.IdentityScope{Tenant: "tenant-a", User: "operator", Session: "session-alpha"}
		p := projection.Projection{Identity: id, Generation: 1, SessionStatus: string(types.SessionStatusRunning), Blocks: []projection.Block{{ID: "user-1", Kind: "user", Text: "Summarize the deployment status."}, {ID: "answer-1", Kind: "text", Status: "completed", Text: "The deployment completed successfully."}}}
		controller := &recordingController{id: id, projection: p}
		m := NewRuntimeModel(context.Background(), w, h, ui.NewTheme(ui.ModeDark, ui.ProfileMono), controller, nil, RuntimeOptions{ReducedMotion: true, Fingerprint: "runtime-a"})
		m.applyUpdate(conversation.Update{Identity: id, Generation: 1, State: conversation.StateLive, Projection: p})
		return m
	}
	return []operationalCaptureScenario{
		{"operational-live", base},
		{"operational-degraded", func(w, h int) RuntimeModel {
			m := base(w, h)
			p := m.transcript.Projection
			p.HistoryTruncated, p.AggregateTruncated, p.CountersPartial, p.ReconciliationRequired = true, true, true, true
			p.ReplayGap = &projection.ReplayGap{Reason: "stream_overlap", LastSequence: 41, SeenSequence: 44}
			p.Blocks = append(p.Blocks, projection.Block{ID: "unknown-44", Kind: "event", EventType: "extension.notice", Incomplete: true})
			m.applyUpdate(conversation.Update{Identity: p.Identity, Generation: 1, State: conversation.StateReplaying, Projection: p, Overflow: true})
			return m
		}},
		{"operational-session-picker", func(w, h int) RuntimeModel {
			m := base(w, h)
			m.shell = m.shell.WithModal(NewSelect("Sessions", []SelectItem{{ID: "session-alpha", Title: "Current session", Description: "running", Current: true}, {ID: "session-closed", Title: "Incident review", Description: "completed · Resume on next canonical start"}}, "composer"))
			return m
		}},
		{"operational-reauth", func(w, h int) RuntimeModel {
			m := base(w, h)
			m.shell.state.Connection = string(conversation.StateAuthExpired)
			m.shell.state.Composer = ComposerDisabled
			m.shell = m.shell.WithModal(NewSelect("Replace credential · memory only", nil, "composer"))
			return m
		}},
		{"operational-start-fresh", func(w, h int) RuntimeModel {
			m := base(w, h)
			m.shell.state.Erased = true
			m.shell.state.Composer = ComposerDisabled
			m.shell = m.shell.WithModal(NewSelect("Start Fresh", nil, "composer"))
			return m
		}},
		{"operational-attachment", func(w, h int) RuntimeModel {
			m := base(w, h)
			m.shell = m.shell.WithModal(NewSelect("Attach file · path|disposition", nil, "composer"))
			return m
		}},
		{"operational-search", func(w, h int) RuntimeModel {
			m := base(w, h)
			m.searchMode, m.searchQuery = true, "deployment"
			m.transcript = m.transcript.Search(m.searchQuery)
			m.shell.state.Scrolled = true
			m.shell.state.ToastOpen = true
			m.shell.state.Toast = "Search: deployment · 2 matches"
			return m
		}},
		{"operational-followup-failed", func(w, h int) RuntimeModel {
			m := base(w, h)
			m.followups = m.followups.Enqueue("Continue with the rollback analysis.")
			next, entry, _ := m.followups.Begin()
			m.followups = next.Fail(entry.ID, fmt.Errorf("Runtime temporarily unavailable"))
			m.shell.state.HasFollowUp = true
			m.shell.state.ToastOpen = true
			m.shell.state.Toast = "Follow-up failed · retry or discard"
			return m
		}},
	}
}

func scenarios() []captureScenario {
	state := func(name string, value State) captureScenario {
		return captureScenario{name, func(w, h int) Model {
			return NewModel(w, h, ui.NewTheme(ui.ModeDark, ui.ProfileMono), name == "reduced-motion", FixtureProjection()).WithState(value)
		}}
	}
	return []captureScenario{
		state("home", State{Route: "home", Composer: ComposerIdle}), state("session", State{Route: "session", Composer: ComposerIdle}),
		state("composer-focused", State{Composer: ComposerFocused}), state("composer-disabled", State{Composer: ComposerDisabled}), state("composer-running", State{Composer: ComposerRunning}), state("composer-retry", State{Composer: ComposerRetry}), state("composer-attachment", State{Composer: ComposerAttachment}),
		state("active-stream", State{Active: true, Composer: ComposerRunning}), state("scrolled-away", State{Scrolled: true}), state("reconnect", State{Connection: "retrying · attempt 2 in 4s"}), state("replay-gap", State{ReplayGap: true}), state("dropped-event", State{Dropped: true}), state("closed", State{Closed: true}), state("erased", State{Erased: true}), state("intervention", State{Intervention: true}), state("unknown-fallback", State{Unknown: true}), state("sidebar-overlay-or-joined", State{SidebarOpen: true, ToastOpen: true, Toast: "Sidebar context visible"}), state("autocomplete", State{AutocompleteOpen: true}), state("reduced-motion", State{Startup: true}),
		{"palette", func(w, h int) Model {
			return NewModel(w, h, ui.NewTheme(ui.ModeDark, ui.ProfileMono), true, FixtureProjection()).WithState(State{}).OpenPalette()
		}},
		{"which-key", func(w, h int) Model {
			m := NewModel(w, h, ui.NewTheme(ui.ModeDark, ui.ProfileMono), true, FixtureProjection()).WithState(State{})
			next, _ := m.Update(keyMsg('x', tea.ModCtrl))
			return next.(Model)
		}},
		{"modal-filter", func(w, h int) Model {
			m := NewModel(w, h, ui.NewTheme(ui.ModeDark, ui.ProfileMono), true, FixtureProjection()).WithState(State{}).OpenPalette()
			for _, r := range "theme" {
				next, _ := m.Update(keyMsg(r, 0))
				m = next.(Model)
			}
			return m
		}},
		{"nested-modal", func(w, h int) Model {
			m := NewModel(w, h, ui.NewTheme(ui.ModeDark, ui.ProfileMono), true, FixtureProjection()).WithState(State{}).OpenPalette()
			return m.WithModal(NewSelect("Themes", []SelectItem{{ID: "dark", Title: "Dark"}, {ID: "light", Title: "Light"}}, "modal"))
		}},
		{"paste", func(w, h int) Model {
			m := NewModel(w, h, ui.NewTheme(ui.ModeDark, ui.ProfileMono), true, FixtureProjection()).WithState(State{})
			next, _ := m.Update(tea.PasteMsg{Content: "one\ntwo\nthree"})
			return next.(Model)
		}},
		{"resize", func(w, h int) Model {
			m := NewModel(100, 30, ui.NewTheme(ui.ModeDark, ui.ProfileMono), true, FixtureProjection()).WithState(State{ToastOpen: true, Toast: "Resize applied"})
			next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
			return next.(Model)
		}},
		{"focus-restore", func(w, h int) Model {
			m := NewModel(w, h, ui.NewTheme(ui.ModeDark, ui.ProfileMono), true, FixtureProjection()).WithState(State{}).OpenPalette()
			next, _ := m.Update(keyMsg(tea.KeyEscape, 0))
			m = next.(Model)
			next, _ = m.Update(tea.FocusMsg{})
			return next.(Model)
		}},
	}
}

type captureEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type captureManifest struct {
	Status          string            `json:"status"`
	GeneratedOn     string            `json:"generated_on"`
	ReviewDate      string            `json:"review_date"`
	ReviewScope     string            `json:"review_scope"`
	Reviewed        bool              `json:"reviewed"`
	Reproduce       string            `json:"reproduce"`
	TerminalProfile map[string]string `json:"terminal_profile"`
	Reference       map[string]any    `json:"reference_measurements"`
	Deviations      []string          `json:"known_deviations"`
	Candidates      []captureEntry    `json:"candidates"`
}

func TestCaptureMatrix_AllApplicableFoundationStates(t *testing.T) {
	dir := filepath.Join("..", "testdata", "golden")
	if *updateCaptures {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".txt") || strings.HasSuffix(entry.Name(), ".ansi") {
				if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	var captures []captureEntry
	for _, size := range goldenSizes {
		seen := map[string]string{}
		for _, scenario := range scenarios() {
			name := fmt.Sprintf("%dx%d-%s.txt", size[0], size[1], scenario.name)
			t.Run(name, func(t *testing.T) {
				frame := ansi.Strip(scenario.build(size[0], size[1]).Frame())
				assertFrameGeometry(t, frame, size[0], size[1])
				capture := normalizeCapture(frame)
				if prior, exists := seen[hash(capture)]; exists {
					t.Fatalf("scenario %s duplicates %s at %dx%d", scenario.name, prior, size[0], size[1])
				}
				seen[hash(capture)] = scenario.name
				path := filepath.Join(dir, name)
				compareOrWrite(t, path, capture)
				captures = append(captures, captureEntry{Path: name, SHA256: hash(capture)})
			})
		}
		for _, scenario := range operationalScenarios() {
			name := fmt.Sprintf("%dx%d-%s.txt", size[0], size[1], scenario.name)
			t.Run(name, func(t *testing.T) {
				frame := ansi.Strip(scenario.build(size[0], size[1]).shell.Frame())
				assertFrameGeometry(t, frame, size[0], size[1])
				capture := normalizeCapture(frame)
				if prior, exists := seen[hash(capture)]; exists {
					t.Fatalf("scenario %s duplicates %s at %dx%d", scenario.name, prior, size[0], size[1])
				}
				seen[hash(capture)] = scenario.name
				path := filepath.Join(dir, name)
				compareOrWrite(t, path, capture)
				captures = append(captures, captureEntry{Path: name, SHA256: hash(capture)})
			})
		}
	}
	profiles := []struct {
		name  string
		theme ui.Theme
	}{{"truecolor-dark", ui.NewTheme(ui.ModeDark, ui.ProfileTrueColor)}, {"truecolor-light", ui.NewTheme(ui.ModeLight, ui.ProfileTrueColor)}, {"256-dark", ui.NewTheme(ui.ModeDark, ui.ProfileANSI256)}, {"256-light", ui.NewTheme(ui.ModeLight, ui.ProfileANSI256)}, {"16-dark", ui.NewTheme(ui.ModeDark, ui.ProfileANSI16)}, {"mono", ui.NewTheme(ui.ModeDark, ui.ProfileMono)}}
	for _, profile := range profiles {
		for _, scenario := range []State{{Intervention: true}, {ReplayGap: true}} {
			label := "intervention"
			if scenario.ReplayGap {
				label = "replay-gap"
			}
			name := profile.name + "-" + label + ".ansi"
			frame := NewModel(100, 30, profile.theme, profile.name == "mono", FixtureProjection()).WithState(scenario).Frame()
			assertFrameGeometry(t, ansi.Strip(frame), 100, 30)
			capture := normalizeCapture(frame)
			compareOrWrite(t, filepath.Join(dir, name), capture)
			captures = append(captures, captureEntry{Path: name, SHA256: hash(capture)})
		}
	}
	if *updateCaptures {
		sort.Slice(captures, func(i, j int) bool { return captures[i].Path < captures[j].Path })
		manifest := captureManifest{Status: "candidate-generated-pending-orchestrator-review", GeneratedOn: "2026-07-15", ReviewDate: "", ReviewScope: "Phase-182 operational RuntimeModel candidates: live, degraded/reconciliation, session picker, reauthentication, and failed follow-up states; Phase-181 foundation fixtures remain in their own scenarios; orchestrator review pending.", Reviewed: false, Reproduce: "go test -race ./internal/tui/app -run TestCaptureMatrix -update-captures", TerminalProfile: map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor", "unicode_width": "github.com/charmbracelet/x/ansi v0.11.7"}, Reference: map[string]any{"source": "docs/research/tui-investigation/05-opencode-visual-grammar.md lines 25-48, 88-104, 184-263, 373-422", "outer_padding_cells": 2, "sidebar_columns": 42, "actions": "79 stacked; 80 horizontal", "sidebar": "120 overlay; 121 joined", "composer": "75 columns or 70% wide", "dialogs": []int{60, 88, 116}, "autocomplete_rows": 10, "spinner_ms": []int{80, 40}}, Deviations: []string{"Terminal scrims use semantic faint/dim styling because alpha compositing is unavailable.", "Operational captures use canonical test Protocol data through RuntimeModel; authenticated transport behavior is separately verified by the built-CLI PTY integration.", "PTY behavior varies by terminal; xterm-256color is the pinned capture profile."}, Candidates: captures}
		data, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, '\n')
		if err = os.WriteFile(filepath.Join(dir, "capture-manifest.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func compareOrWrite(t *testing.T, path, frame string) {
	t.Helper()
	if *updateCaptures {
		if err := os.WriteFile(path, []byte(frame), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if frame != string(want) {
		t.Fatalf("capture drifted from %s; inspect then regenerate intentionally", path)
	}
}

func normalizeCapture(frame string) string {
	lines := strings.Split(frame, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.Join(lines, "\n")
}

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func assertFrameGeometry(t *testing.T, frame string, width, height int) {
	t.Helper()
	lines := strings.Split(frame, "\n")
	if len(lines) != height {
		t.Fatalf("rows=%d want=%d", len(lines), height)
	}
	for row, line := range lines {
		if got := ui.Width(line); got != width {
			t.Fatalf("row %d width=%d want=%d: %q", row, got, width, line)
		}
	}
}
func keyMsg(code rune, mod tea.KeyMod) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: code, Mod: mod})
}
