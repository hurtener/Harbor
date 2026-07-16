package app

import (
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tui/projection"
	"github.com/hurtener/Harbor/internal/tui/ui"
)

// TestConversationSurface_RuntimeInternalsNeverLeakIntoChat pins the boundary
// between the conversation and Runtime diagnostics. A real session interleaves
// audit records, cost accounting, planner decisions and session/task lifecycle
// with the answer; none of them belong on the chat surface (they stay on the
// events and diagnostics routes), and none of them may light the conversation's
// honesty banners — fallback event blocks are incomplete by construction.
func TestConversationSurface_RuntimeInternalsNeverLeakIntoChat(t *testing.T) {
	now := time.Now()
	internals := []projection.Block{
		{ID: "e1", Kind: "event", EventType: "audit.admin_scope_used", Incomplete: true, At: now},
		{ID: "e2", Kind: "event", EventType: "session.opened", Incomplete: true, At: now},
		{ID: "e3", Kind: "event", EventType: "llm.cost.recorded", Incomplete: true, At: now},
		{ID: "e4", Kind: "event", EventType: "planner.decision", Incomplete: true, At: now},
		{ID: "s1", Kind: "session", Status: "running", At: now},
	}
	for _, b := range internals {
		if conversational(b.Kind) {
			t.Fatalf("kind %q must not be conversational", b.Kind)
		}
	}

	visible := []projection.Block{
		{ID: "u1", Kind: "user", Text: "hey there!", At: now},
		{ID: "a1", Kind: "text", Text: "Hey there! How can I help you today?", At: now},
	}
	p := projection.Projection{Identity: types.IdentityScope{Tenant: "dev", User: "dev", Session: "dev"}, Blocks: visible}
	m := NewOperationalModel(120, 26, ui.NewTheme(ui.ModeDark, ui.ProfileMono), true, p)
	m.state.Route, m.state.Connection, m.state.Composer = "session", "live", ComposerFocused
	m.state.TurnStatus = "z-ai/glm-5.2  ·  7.0s"
	m.startup = startupHidden
	frame := m.Frame()

	for _, noise := range []string{"audit.admin_scope_used", "llm.cost.recorded", "planner.decision", "session.opened", "Some blocks are incomplete"} {
		if strings.Contains(frame, noise) {
			t.Errorf("runtime internal leaked into the conversation: %q", noise)
		}
	}
	// The canonical per-turn anchor replaces that noise.
	if !strings.Contains(frame, "z-ai/glm-5.2") || !strings.Contains(frame, "7.0s") {
		t.Error("per-turn status anchor missing from the answer")
	}
}

// TestComposerStatus_ReportsNoLocalElapsed pins that the working indicator never
// derives a duration from a local clock: the only honest elapsed is the
// Runtime's own (TaskRow.StartedAt → UpdatedAt), rendered on the turn anchor
// once the run is terminal. A local timer would count the operator's typing.
func TestComposerStatus_ReportsNoLocalElapsed(t *testing.T) {
	p := projection.Projection{Blocks: []projection.Block{
		{ID: "a1", Kind: "text", Text: "streaming", Incomplete: true, At: time.Now().Add(-90 * time.Second)},
	}}
	m := NewOperationalModel(100, 24, ui.NewTheme(ui.ModeDark, ui.ProfileMono), true, p)
	m.state.Composer = ComposerRunning
	status := m.composerStatus()
	if !strings.Contains(status, "Working") || !strings.Contains(status, "esc interrupt") {
		t.Fatalf("working status missing progress/interrupt affordance: %q", status)
	}
	if strings.Contains(status, "90") || strings.Contains(status, "s ·") {
		t.Fatalf("composer status must not report a locally-derived elapsed: %q", status)
	}

	// An incomplete metadata-only fallback block must never pin "Working" on.
	idle := NewOperationalModel(100, 24, ui.NewTheme(ui.ModeDark, ui.ProfileMono), true, projection.Projection{Blocks: []projection.Block{
		{ID: "e1", Kind: "event", EventType: "llm.cost.recorded", Incomplete: true},
	}})
	idle.state.Composer = ComposerFocused
	if idle.hasActiveTurn() {
		t.Error("a metadata-only fallback event must not make a finished turn look active")
	}
}
