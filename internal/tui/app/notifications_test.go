package app

import (
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tui/conversation"
	"github.com/hurtener/Harbor/internal/tui/projection"
	"github.com/hurtener/Harbor/internal/tui/ui"
)

// TestConversational_NotificationBelongsOnChat pins the one runtime-lifecycle
// kind that IS conversational: a notification block is a deliberate,
// operator-addressed wake line, unlike the raw internals (event/session/task
// lifecycle) that stay off-chat.
func TestConversational_NotificationBelongsOnChat(t *testing.T) {
	if !conversational("notification") {
		t.Fatal("notification blocks must be conversational")
	}
	// The raw-internals kinds stay off-chat — the distinction is authorship.
	for _, kind := range []string{"event", "session", "task", "result"} {
		if conversational(kind) {
			t.Errorf("kind %q must NOT be conversational (it is a runtime internal)", kind)
		}
	}
}

// TestLayoutNotification_MutedSingleLine covers the muted-line geometry + style
// vocabulary (CONVENTIONS §10 fixture: a background-completion / group-
// resolution line): one line, the runtime-composed Summary, the muted "·"
// glyph from lifecycleGlyph's vocabulary.
func TestLayoutNotification_MutedSingleLine(t *testing.T) {
	p := projection.Projection{Identity: types.IdentityScope{Tenant: "t", User: "u", Session: "s"}, Blocks: []projection.Block{
		{ID: "notification:1", Kind: "notification", Text: "Background group resolved: 2 of 3 succeeded, 1 failed"},
	}}
	m := NewOperationalModel(100, 24, ui.NewTheme(ui.ModeDark, ui.ProfileMono), true, p)

	lb := m.layoutBlock(p.Blocks[0], 90, false)
	if lb.height != 1 {
		t.Fatalf("notification line height=%d, want 1 (never a card / per-member fan-out)", lb.height)
	}
	if len(lb.ops) != 1 {
		t.Fatalf("notification line ops=%d, want 1", len(lb.ops))
	}
	text := lb.ops[0].text
	if !strings.Contains(text, "2 of 3 succeeded") || !strings.HasPrefix(text, "·") {
		t.Errorf("notification line=%q, want the muted glyph + Summary", text)
	}
	// Style vocabulary: the notification uses the muted lifecycle role.
	if _, role := lifecycleGlyph("notification"); role != ui.RoleMuted {
		t.Errorf("notification glyph role=%v, want RoleMuted", role)
	}
	// And it lands on the conversation surface (Frame, ANSI-stripped).
	m.state.Route, m.state.Connection, m.state.Composer = "session", "live", ComposerFocused
	m.startup = startupHidden
	if !strings.Contains(m.Frame(), "2 of 3 succeeded") {
		t.Error("notification summary missing from the conversation surface")
	}

	// An empty-text notification block renders nothing (the reducer never
	// fabricates a summary, so a blank one costs zero rows).
	empty := m.layoutBlock(projection.Block{ID: "n", Kind: "notification", Text: ""}, 90, false)
	if empty.height != 0 {
		t.Errorf("empty notification block height=%d, want 0", empty.height)
	}
}

// TestLayoutNotification_GroupCancelledRollup is the CONVENTIONS §10
// fixture for the cancelled-group rollup line: an unprompted
// (fail-fast / cascade) group cancel renders as ONE muted lifecycle line
// (muted "·" glyph + the runtime-composed Summary), never a card or a
// per-member fan-out. The suppressed operator-initiated cancel is proven
// at the mapper (it never synthesises a notification), so at the
// transcript layer the suppressed case is simply the absence of any such
// block — covered here by the empty-text no-row assertion.
func TestLayoutNotification_GroupCancelledRollup(t *testing.T) {
	p := projection.Projection{Identity: types.IdentityScope{Tenant: "t", User: "u", Session: "s"}, Blocks: []projection.Block{
		{ID: "notification:4", Kind: "notification", Text: "Background group cancelled (fail-fast): 2 cancelled of 3, 1 failed"},
	}}
	m := NewOperationalModel(100, 24, ui.NewTheme(ui.ModeDark, ui.ProfileMono), true, p)

	lb := m.layoutBlock(p.Blocks[0], 90, false)
	if lb.height != 1 {
		t.Fatalf("cancelled-group line height=%d, want 1 (never a card / per-member fan-out)", lb.height)
	}
	if len(lb.ops) != 1 {
		t.Fatalf("cancelled-group line ops=%d, want 1", len(lb.ops))
	}
	text := lb.ops[0].text
	if !strings.Contains(text, "2 cancelled of 3") || !strings.HasPrefix(text, "·") {
		t.Errorf("cancelled-group line=%q, want the muted glyph + Summary", text)
	}
	if _, role := lifecycleGlyph("notification"); role != ui.RoleMuted {
		t.Errorf("notification glyph role=%v, want RoleMuted", role)
	}
	// Lands on the conversation surface (Frame, ANSI-stripped).
	m.state.Route, m.state.Connection, m.state.Composer = "session", "live", ComposerFocused
	m.startup = startupHidden
	if !strings.Contains(m.Frame(), "2 cancelled of 3") {
		t.Error("cancelled-group summary missing from the conversation surface")
	}
	// The suppressed operator-cancel case: no notification block is minted
	// upstream, so a blank one costs zero rows here.
	empty := m.layoutBlock(projection.Block{ID: "n", Kind: "notification", Text: ""}, 90, false)
	if empty.height != 0 {
		t.Errorf("suppressed (empty) cancel block height=%d, want 0", empty.height)
	}
}

// TestStatusStrip_TurnFailedWithoutCode covers the no-error-code branch of the
// turn-failure line (a bounded fallback when the Runtime reported no code).
func TestStatusStrip_TurnFailedWithoutCode(t *testing.T) {
	m := NewOperationalModel(100, 24, ui.NewTheme(ui.ModeDark, ui.ProfileMono), true, projection.Projection{})
	m.state.TurnFailed = true
	m.state.TurnFailedCode = ""
	m.state.Composer = ComposerFocused
	line, role, show := m.statusStrip()
	if !show || role != ui.RoleError || line != "×  Turn failed" {
		t.Fatalf("no-code turn-failure line=%q role=%v show=%v", line, role, show)
	}
	// While a fresh turn is running, the line is suppressed.
	m.state.Composer = ComposerRunning
	if _, _, show := m.statusStrip(); show {
		t.Error("turn-failure line must be suppressed while a fresh turn is running")
	}
}

// TestProjectionActive_NotificationIsNotInFlightWork proves a settled
// notification block never holds the composer in the running state.
func TestProjectionActive_NotificationIsNotInFlightWork(t *testing.T) {
	m, _, _ := operationalModel(t)
	id := m.controller.Identity()
	p := projection.Projection{Identity: id, LastSequence: 1, Blocks: []projection.Block{
		{ID: "notification:1", Kind: "notification", Text: "Background task done"},
	}}
	m.applyUpdate(conversation.Update{Identity: id, Generation: 2, State: conversation.StateLive, Projection: p})
	if m.projectionActive() {
		t.Error("a notification block must not register as in-flight work")
	}
}

// TestStatusStrip_TurnFailedLineAppearsThenClearsOnNextSubmit is the turn-
// failure honesty gate (CONVENTIONS §10 fixture: present, then cleared on next
// submit). A FAILED foreground turn renders "× Turn failed · <code>"; a fresh
// turn (the newest foreground turn now in flight) clears it.
func TestStatusStrip_TurnFailedLineAppearsThenClearsOnNextSubmit(t *testing.T) {
	m, _, _ := operationalModel(t)
	id := m.controller.Identity()
	now := time.Now()

	// A failed foreground turn: a user turn block + its terminal failed task
	// block carrying the bounded error code.
	failed := projection.Projection{Identity: id, LastSequence: 2, Blocks: []projection.Block{
		{ID: "user:r1", Kind: "user", RunID: "r1", Text: "do the thing", At: now},
		{ID: "task:r1", Kind: "task", RunID: "r1", Status: "failed", ErrorCode: "planner_rejected", At: now},
	}}
	m.applyUpdate(conversation.Update{Identity: id, Generation: 2, State: conversation.StateLive, Projection: failed})

	if !m.shell.state.TurnFailed || m.shell.state.TurnFailedCode != "planner_rejected" {
		t.Fatalf("turn-failure state not derived: %+v", m.shell.state)
	}
	line, role, show := m.shell.statusStrip()
	if !show || role != ui.RoleError || !strings.Contains(line, "Turn failed") || !strings.Contains(line, "planner_rejected") {
		t.Fatalf("status strip did not show the turn-failure line: %q role=%v show=%v", line, role, show)
	}

	// A fresh turn is submitted: r2's user + running task blocks become the
	// newest foreground turn, which is not failed → the line clears.
	fresh := projection.Projection{Identity: id, LastSequence: 4, Blocks: []projection.Block{
		{ID: "user:r1", Kind: "user", RunID: "r1", Text: "do the thing", At: now},
		{ID: "task:r1", Kind: "task", RunID: "r1", Status: "failed", ErrorCode: "planner_rejected", At: now},
		{ID: "user:r2", Kind: "user", RunID: "r2", Text: "try again", At: now.Add(time.Second)},
		{ID: "task:r2", Kind: "task", RunID: "r2", Status: "running", At: now.Add(time.Second)},
	}}
	m.applyUpdate(conversation.Update{Identity: id, Generation: 3, State: conversation.StateLive, Projection: fresh})
	if m.shell.state.TurnFailed {
		t.Error("turn-failure line must clear once a fresh turn becomes the newest foreground turn")
	}
	if _, _, show := m.shell.statusStrip(); show {
		if line, _, _ := m.shell.statusStrip(); strings.Contains(line, "Turn failed") {
			t.Errorf("stale turn-failure line survived a fresh submit: %q", line)
		}
	}
}

// TestTurnFailure_BackgroundTaskFailureNeverCountsAsTurnFailure proves the
// dedup keys on the foreground-turn signal: a failed task with NO user block
// (a background task) never lights the turn-failure line.
func TestTurnFailure_BackgroundTaskFailureNeverCountsAsTurnFailure(t *testing.T) {
	m, _, _ := operationalModel(t)
	id := m.controller.Identity()
	p := projection.Projection{Identity: id, LastSequence: 1, Blocks: []projection.Block{
		{ID: "task:bg", Kind: "task", RunID: "bg", Status: "failed", ErrorCode: "timeout", At: time.Now()},
	}}
	m.applyUpdate(conversation.Update{Identity: id, Generation: 2, State: conversation.StateLive, Projection: p})
	if m.shell.state.TurnFailed {
		t.Error("a background task failure (no user block) must not light the turn-failure line")
	}
}

// TestTurnFailure_HydratedRowsClassifyByKindNotQuery proves the end-to-end
// hydration path: a failed BACKGROUND task row carrying a non-empty Query does
// NOT light the turn-failure line (it has no user block), while a failed
// FOREGROUND row — even one with an empty Query — DOES. This closes the
// misclassification where background tasks (which also carry a Query) produced
// a spurious "Turn failed" line.
func TestTurnFailure_HydratedRowsClassifyByKindNotQuery(t *testing.T) {
	m, _, _ := operationalModel(t)
	id := m.controller.Identity()
	now := time.Unix(1, 0).UTC()

	background := mustHydrate(t, id, types.TaskRow{
		ID: "bg", Kind: types.TaskKindBackground, IsBackground: true,
		Status: types.TaskStatusFailed, ErrorClass: "timeout",
		Identity: id, ParentSessionID: id.Session, Query: "spawned query", StartedAt: now,
	})
	m.applyUpdate(conversation.Update{Identity: id, Generation: 2, State: conversation.StateLive, Projection: background})
	if m.shell.state.TurnFailed {
		t.Error("a failed background row with a Query must not light the turn-failure line")
	}

	foreground := mustHydrate(t, id, types.TaskRow{
		ID: "fg", Kind: types.TaskKindForeground,
		Status: types.TaskStatusFailed, ErrorClass: "planner_rejected",
		Identity: id, ParentSessionID: id.Session, Description: "no query here", StartedAt: now,
	})
	m.applyUpdate(conversation.Update{Identity: id, Generation: 3, State: conversation.StateLive, Projection: foreground})
	if !m.shell.state.TurnFailed {
		t.Error("a failed foreground row (even with an empty Query) must light the turn-failure line")
	}
}

func mustHydrate(t *testing.T, id types.IdentityScope, rows ...types.TaskRow) projection.Projection {
	t.Helper()
	p, err := (&projection.Reducer{}).Hydrate(projection.SnapshotBundle{
		Generation: 1, Identity: id,
		Tasks: types.TaskListResponse{Rows: rows},
	})
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	return p
}
