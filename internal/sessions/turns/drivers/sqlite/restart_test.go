package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/sessions/turns/drivers/sqlite"
)

// The restart tests prove the durability promise of the file-backed
// SQLite driver: after Close + reopen on the SAME file, the reopened
// store serves IDENTICAL pages (same rows, same order, same minted
// cursors), IDENTICAL gets, IDENTICAL checkpoints — and the durable
// erasure fence / tombstone is STILL in force, so replay can never
// resurrect an erased session.

// openAt opens the driver at dsn and fails the test on error.
func openAt(t *testing.T, dsn string) turns.Store {
	t.Helper()
	s, err := sqlite.New(sqlite.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("sqlite.New(%q): %v", dsn, err)
	}
	return s
}

// openMem opens a fresh in-memory store (isolated per open) and fails
// the test on error.
func openMem(t *testing.T) turns.Store {
	t.Helper()
	s, err := sqlite.New(sqlite.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("sqlite.New(:memory:): %v", err)
	}
	return s
}

func restartID() identity.Identity {
	return identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a"}
}

// richRow builds a row that exercises EVERY persisted component: the
// renderable query, agent binding, attachments, pause, artifact-ref
// answer, per-measure usage with pointer-backed values, derived
// reasoning, content-free activity with exact totals, closed terminal
// enums, and an ordered MCP App collection with availability — the
// full renderable DTO the round-trip must survive byte-for-byte.
func richRow(turnID string, id identity.Identity, seq int) turns.TurnRow {
	prompt := int64(100 + seq)
	total := int64(150 + seq)
	return turns.TurnRow{
		TurnID:              turns.TurnID(turnID),
		SessionID:           id.SessionID,
		Status:              turns.StatusRunning,
		Query:               turns.Query{Text: "hello", At: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), Complete: turns.CompletenessComplete},
		Agent:               turns.Agent{ID: fmt.Sprintf("agent-%d", seq%3), Name: "Agent", BindingSource: turns.AgentBindingExplicit, Complete: turns.CompletenessComplete},
		Inputs:              []turns.Attachment{{ID: "in_1", Filename: "f.txt", Availability: turns.CompletenessComplete}},
		Outputs:             []turns.Attachment{{ID: "out_1", Availability: turns.CompletenessUnavailable}},
		Pause:               turns.Pause{Class: turns.PauseClassHitlApproval, Reason: "approval", Lifecycle: turns.PauseLifecycleActive, Availability: turns.CompletenessComplete},
		Answer:              turns.Answer{State: turns.AnswerStateArtifactRef, Ref: &turns.AnswerRef{ID: "art_1", SizeBytes: 4096}},
		LastAppliedEventSeq: uint64(seq),
		Usage: turns.Usage{
			PromptTokens: turns.UsageMeasure{State: turns.UsageExact, Value: &prompt},
			TotalTokens:  turns.UsageMeasure{State: turns.UsageExact, Value: &total},
			Model:        "model-x",
		},
		Reasoning: turns.Reasoning{
			Steps:    []turns.ReasoningStep{{Index: 0, Kind: turns.ReasoningKindToolCall}, {Index: 2, Kind: turns.ReasoningKindSpawn}},
			Complete: turns.CompletenessComplete,
			Seq:      7,
		},
		Activity: turns.Activity{
			Rows: []turns.ActivityRow{{
				Position: 0, Tool: "t1", Status: turns.ActivitySucceeded,
				TerminalClass: turns.ActivityTerminalSucceeded, Summary: "ok",
			}},
			Complete: turns.CompletenessComplete,
			Totals:   turns.ActivityTotals{Succeeded: 1},
		},
		Apps: []turns.AppRef{
			{EffectiveAgentID: "agent-1", ServerID: "srv-1", ResourceURI: "ui://srv/app", DisplayMode: "inline", RawHTMLTrusted: false, ToolCallID: "tc-1", ToolName: "tool-a", Availability: turns.AppAvailable, Complete: turns.CompletenessComplete},
			{EffectiveAgentID: "", ServerID: "srv-2", ResourceURI: "ui://srv/app2", ToolName: "tool-b", Availability: turns.AppDegraded, Complete: turns.CompletenessComplete},
		},
	}
}

// walkPages pages the ENTIRE session newest-first and returns the full
// rows in walk order plus every minted cursor (for cross-restart
// cursor-continuation proofs).
func walkPages(t *testing.T, s turns.Store, id identity.Identity, limit int) ([]turns.TurnRow, []*turns.Cursor) {
	t.Helper()
	ctx := context.Background()
	var rows []turns.TurnRow
	var cursors []*turns.Cursor
	var c *turns.Cursor
	for {
		page, next, _, err := s.ListTurns(ctx, id, c, limit)
		if err != nil {
			t.Fatalf("ListTurns: %v", err)
		}
		rows = append(rows, page...)
		if next == nil {
			return rows, cursors
		}
		cursors = append(cursors, next)
		c = next
	}
}

// TestRestart_IdenticalPagesGetAndCheckpoint proves the durability
// promise end-to-end: 30 rich rows (with every persisted component),
// some updated and sealed, page them ALL, read one by get, save a
// checkpoint — then Close, reopen, and prove the reopened store serves
// IDENTICAL pages (rows, order, cursors), an IDENTICAL get, and an
// IDENTICAL checkpoint.
func TestRestart_IdenticalPagesGetAndCheckpoint(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "turns.sqlite")
	id := restartID()

	s1 := openAt(t, dsn)
	ctx := context.Background()
	const n = 30
	appended := make([]turns.TurnRow, 0, n)
	for i := 1; i <= n; i++ {
		row, err := s1.AppendTurnIf(ctx, id, richRow(fmt.Sprintf("run-%02d", i), id, i))
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		appended = append(appended, row)
	}
	// Update + seal a few rows so the reopened store must reproduce the
	// versioned + sealed forms too.
	for _, tid := range []string{"run-05", "run-12", "run-20"} {
		cur, err := s1.GetTurn(ctx, id, turns.TurnID(tid))
		if err != nil {
			t.Fatalf("get %s: %v", tid, err)
		}
		next := cur
		next.Answer = turns.Answer{State: turns.AnswerStateInline, Inline: "updated"}
		got, err := s1.UpdateTurnIf(ctx, id, cur.TurnID, cur.Version, next)
		if err != nil {
			t.Fatalf("update %s: %v", tid, err)
		}
		sealed := got
		sealed.Status = turns.StatusComplete
		sealed.Sealed = true
		sealed.FinishReason = turns.FinishGoal
		sealed.FinishMessage = "goal reached"
		if _, err := s1.SealTurnIf(ctx, id, got.TurnID, got.Version, sealed); err != nil {
			t.Fatalf("seal %s: %v", tid, err)
		}
	}
	if err := s1.SaveCheckpoint(ctx, id, 4242); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	// Record the pre-restart view.
	preRows, preCursors := walkPages(t, s1, id, 7)
	preGet, err := s1.GetTurn(ctx, id, "run-01")
	if err != nil {
		t.Fatalf("pre get: %v", err)
	}
	preCheckpoint, err := s1.LoadCheckpoint(ctx, id)
	if err != nil {
		t.Fatalf("pre checkpoint: %v", err)
	}
	if len(preRows) != n {
		t.Fatalf("pre-restart walk has %d rows, want %d", len(preRows), n)
	}
	if len(preCursors) != 4 { // 30 rows / 7 per page → 5 pages, 4 intermediate cursors
		t.Fatalf("pre-restart cursors=%d, want 4", len(preCursors))
	}
	if preCheckpoint != 4242 {
		t.Fatalf("pre checkpoint=%d, want 4242", preCheckpoint)
	}
	if err := s1.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen the SAME file — the restart.
	s2 := openAt(t, dsn)
	defer func() { _ = s2.Close(ctx) }()
	if !s2.Durable() {
		t.Fatal("reopened file-backed store must report Durable() == true")
	}

	// Identical pages: same rows (full DTO equality), same order, same
	// minted cursors.
	postRows, postCursors := walkPages(t, s2, id, 7)
	if len(postRows) != len(preRows) || len(postCursors) != len(preCursors) {
		t.Fatalf("restart walk: %d rows %d cursors, want %d rows %d cursors",
			len(postRows), len(postCursors), len(preRows), len(preCursors))
	}
	for i := range preRows {
		if !reflect.DeepEqual(postRows[i], preRows[i]) {
			t.Errorf("restart row %d drifted:\n got: %+v\nwant: %+v", i, postRows[i], preRows[i])
		}
	}
	for i := range preCursors {
		if !reflect.DeepEqual(postCursors[i], preCursors[i]) {
			t.Errorf("restart cursor %d drifted: got %+v want %+v", i, postCursors[i], preCursors[i])
		}
	}
	// A pre-restart cursor still pages identically against the reopened
	// store (the snapshot generation survived the restart).
	page2, next2, _, err := s2.ListTurns(ctx, id, preCursors[0], 7)
	if err != nil {
		t.Fatalf("restart cursor continuation: %v", err)
	}
	if next2 == nil || len(page2) != 7 {
		t.Fatalf("restart cursor continuation page: %d rows next=%v, want 7/non-nil", len(page2), next2 != nil)
	}
	if !reflect.DeepEqual(page2[0], preRows[7]) {
		t.Errorf("restart cursor continuation row 0 drifted: got %+v want %+v", page2[0], preRows[7])
	}

	// Identical get.
	postGet, err := s2.GetTurn(ctx, id, "run-01")
	if err != nil {
		t.Fatalf("post get: %v", err)
	}
	if !reflect.DeepEqual(postGet, preGet) {
		t.Errorf("restart get drifted:\n got: %+v\nwant: %+v", postGet, preGet)
	}

	// Identical checkpoint.
	postCheckpoint, err := s2.LoadCheckpoint(ctx, id)
	if err != nil {
		t.Fatalf("post checkpoint: %v", err)
	}
	if postCheckpoint != preCheckpoint {
		t.Errorf("restart checkpoint=%d, want %d", postCheckpoint, preCheckpoint)
	}
}

// TestRestart_FenceSurvivesRestart proves the durable erasure
// tombstone: a session fenced before Close is STILL fenced after
// reopen — every write path refuses with ErrErasureFenced and the
// checkpoint stays readable.
func TestRestart_FenceSurvivesRestart(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "turns.sqlite")
	id := restartID()
	ctx := context.Background()

	s1 := openAt(t, dsn)
	if _, err := s1.AppendTurnIf(ctx, id, richRow("run-1", id, 1)); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s1.FenceSession(ctx, id); err != nil {
		t.Fatalf("FenceSession: %v", err)
	}
	if err := s1.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2 := openAt(t, dsn)
	defer func() { _ = s2.Close(ctx) }()

	if _, err := s2.AppendTurnIf(ctx, id, richRow("run-2", id, 2)); !errors.Is(err, turns.ErrErasureFenced) {
		t.Errorf("post-restart append error=%v, want ErrErasureFenced (fence survives restart)", err)
	}
	if _, err := s2.GetTurn(ctx, id, "run-1"); err != nil {
		t.Errorf("pre-erase row must survive a restart with the fence: %v", err)
	}
	// Reads are never fenced.
	if _, err := s2.LoadCheckpoint(ctx, id); err != nil {
		t.Errorf("post-restart checkpoint read: %v", err)
	}
	if err := s2.FenceSession(ctx, id); err != nil {
		t.Errorf("re-fence after restart error=%v, want nil (idempotent)", err)
	}
}

// TestRestart_EraseFenceSurvivesRestart_ReplayCannotResurrect proves
// the full erasure story across a restart: FenceSession + DeleteScope
// + Close + reopen leaves the session EMPTY (no rows, checkpoint 0)
// yet STILL FENCED — a replay from sequence zero cannot resurrect the
// erased session.
func TestRestart_EraseFenceSurvivesRestart_ReplayCannotResurrect(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "turns.sqlite")
	id := restartID()
	ctx := context.Background()

	s1 := openAt(t, dsn)
	for i := 1; i <= 5; i++ {
		if _, err := s1.AppendTurnIf(ctx, id, richRow(fmt.Sprintf("run-%d", i), id, i)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if err := s1.SaveCheckpoint(ctx, id, 77); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	// The erasure cascade: fence FIRST, then delete the projections.
	if err := s1.FenceSession(ctx, id); err != nil {
		t.Fatalf("FenceSession: %v", err)
	}
	if n, err := s1.DeleteScope(ctx, id); err != nil || n < 6 {
		t.Fatalf("DeleteScope = (%d, %v), want (>=6, nil)", n, err)
	}
	if err := s1.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen and REPLAY the same observations — they must be refused.
	s2 := openAt(t, dsn)
	defer func() { _ = s2.Close(ctx) }()

	if _, err := s2.GetTurn(ctx, id, "run-1"); !errors.Is(err, turns.ErrTurnNotFound) {
		t.Errorf("post-restart get error=%v, want ErrTurnNotFound (projection erased)", err)
	}
	if seq, err := s2.LoadCheckpoint(ctx, id); err != nil || seq != 0 {
		t.Errorf("post-restart checkpoint=%d err=%v, want 0 nil", seq, err)
	}
	rows, next, info, err := s2.ListTurns(ctx, id, nil, 10)
	if err != nil {
		t.Fatalf("post-restart list: %v", err)
	}
	if len(rows) != 0 || next != nil || info.Truncated {
		t.Errorf("post-restart page: %d rows next=%v truncated=%v, want empty", len(rows), next != nil, info.Truncated)
	}
	if info.Snapshot != 1 {
		t.Errorf("post-restart snapshot=%d, want 1 (the erase advanced the generation)", info.Snapshot)
	}
	// Replay cannot resurrect: every write path stays fenced.
	if _, err := s2.AppendTurnIf(ctx, id, richRow("run-1", id, 1)); !errors.Is(err, turns.ErrErasureFenced) {
		t.Errorf("replay append error=%v, want ErrErasureFenced (no resurrection)", err)
	}
	if err := s2.SaveCheckpoint(ctx, id, 78); !errors.Is(err, turns.ErrErasureFenced) {
		t.Errorf("replay checkpoint error=%v, want ErrErasureFenced", err)
	}
}

// TestRestart_SnapshotBindingSurvivesRestart proves the projection
// snapshot generation is durable across a restart: a cursor minted
// before the restart pages IDENTICALLY after it (same generation), and
// a cursor minted before an ERASE is rejected as stale after the erase
// + restart.
func TestRestart_SnapshotBindingSurvivesRestart(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "turns.sqlite")
	id := restartID()
	ctx := context.Background()

	s1 := openAt(t, dsn)
	for i := 1; i <= 6; i++ {
		if _, err := s1.AppendTurnIf(ctx, id, richRow(fmt.Sprintf("run-%d", i), id, i)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	page, next, info, err := s1.ListTurns(ctx, id, nil, 2)
	if err != nil || next == nil || len(page) != 2 || info.Snapshot != 0 {
		t.Fatalf("page 1: %d rows next=%v snapshot=%d err=%v", len(page), next != nil, info.Snapshot, err)
	}
	if err := s1.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen: the pre-restart cursor still pages — the snapshot
	// generation survived the restart.
	s2 := openAt(t, dsn)
	defer func() { _ = s2.Close(ctx) }()
	page2, next2, info2, err := s2.ListTurns(ctx, id, next, 2)
	if err != nil {
		t.Fatalf("post-restart cursor continuation: %v", err)
	}
	if len(page2) != 2 || next2 == nil || info2.Snapshot != next.Snapshot {
		t.Errorf("post-restart cursor page: %d rows next=%v snapshot=%d, want 2/non-nil/%d",
			len(page2), next2 != nil, info2.Snapshot, next.Snapshot)
	}

	// Erase, then restart again: the pre-erase cursor is now stale.
	if err := s2.FenceSession(ctx, id); err != nil {
		t.Fatalf("FenceSession: %v", err)
	}
	if _, err := s2.DeleteScope(ctx, id); err != nil {
		t.Fatalf("DeleteScope: %v", err)
	}
	if err := s2.Close(ctx); err != nil {
		t.Fatalf("close 2: %v", err)
	}
	s3 := openAt(t, dsn)
	defer func() { _ = s3.Close(ctx) }()
	if _, _, _, err := s3.ListTurns(ctx, id, next, 2); !errors.Is(err, turns.ErrCursorSnapshotStale) {
		t.Errorf("post-erase-restart cursor error=%v, want ErrCursorSnapshotStale", err)
	}
}
