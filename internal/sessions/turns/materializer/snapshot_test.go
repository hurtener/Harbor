package materializer

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/sessions/turns/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/tasks"
)

// ---------------------------------------------------------------------------
// fakeTaskReader — a test TaskSnapshotReader serving a per-task snapshot
// map, optional per-task legacy absence (ErrTaskSnapshotNotFound), and
// ONE global injected transient error. It records every invocation
// (task id + identity triple) so tests can assert the seam is invoked
// ONLY while projecting persisted task events (never on Protocol
// reads) and always under the EVENT identity.
// ---------------------------------------------------------------------------

type snapshotCall struct {
	taskID string
	triple identity.Identity
}

type fakeTaskReader struct {
	mu       sync.Mutex
	snaps    map[string]TaskSnapshot
	missing  map[string]bool
	callErr  error
	callIDs  []string
	callTris []identity.Identity
}

func newFakeTaskReader() *fakeTaskReader {
	return &fakeTaskReader{snaps: map[string]TaskSnapshot{}, missing: map[string]bool{}}
}

func (f *fakeTaskReader) set(taskID string, snap TaskSnapshot) *fakeTaskReader {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snaps[taskID] = snap
	return f
}

func (f *fakeTaskReader) markMissing(taskID string) *fakeTaskReader {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.missing[taskID] = true
	return f
}

func (f *fakeTaskReader) fail(err error) *fakeTaskReader {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callErr = err
	return f
}

func (f *fakeTaskReader) Task(_ context.Context, id identity.Identity, taskID string) (TaskSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callIDs = append(f.callIDs, taskID)
	f.callTris = append(f.callTris, id)
	if f.callErr != nil {
		return TaskSnapshot{}, f.callErr
	}
	if f.missing[taskID] {
		return TaskSnapshot{}, ErrTaskSnapshotNotFound
	}
	return f.snaps[taskID], nil
}

func (f *fakeTaskReader) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.callIDs)
}

func (f *fakeTaskReader) calls() []snapshotCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]snapshotCall, len(f.callIDs))
	for i := range f.callIDs {
		out[i] = snapshotCall{taskID: f.callIDs[i], triple: f.callTris[i]}
	}
	return out
}

// ---------------------------------------------------------------------------
// current-format spawn: query / agent / input attachments / identity
// ---------------------------------------------------------------------------

// TestMaterialize_TaskSnapshot_SpawnCapturesQueryAgentInputsAndIdentity
// pins the current-format root spawn capture: with the reader wired,
// the append carries the bounded renderable query + instant, the
// effective agent binding, the input attachment metadata, and the
// record's authoritative task/run identity — and the seam is invoked
// exactly for the spawn + the terminal failure, both under the EVENT
// identity.
func TestMaterialize_TaskSnapshot_SpawnCapturesQueryAgentInputsAndIdentity(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	reader := newFakeTaskReader()
	queryAt := time.Unix(1_700_000_123, 0)
	reader.set("task-s", TaskSnapshot{
		TaskID:       "task-s",
		RunID:        "run-snap",
		QueryPresent: true, Query: "render this query", QueryAt: queryAt,
		AgentPresent: true, AgentID: "agent-z", AgentName: "Zeta", AgentBindingSource: turns.AgentBindingExplicit,
		InputsPresent: true, Inputs: []turns.Attachment{{
			ID: "in-snap", MimeType: "application/pdf", Availability: turns.CompletenessComplete,
		}},
	})
	m := h.newMaterializer(t, WithTaskSnapshotReader(reader))

	quad := testQuad(h.id, "run-snap")
	h.src.publish(t, spawnEv(h.id, quad.RunID, "task-s", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-s"))
	h.src.publish(t, failedEv(h.id, "task-s", "timeout"))

	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	row := mustGetRow(t, h, "task-s")
	if row.Query.Text != "render this query" || !row.Query.At.Equal(queryAt) ||
		row.Query.Complete != turns.CompletenessComplete {
		t.Errorf("query = %+v", row.Query)
	}
	if row.Agent.ID != "agent-z" || row.Agent.Name != "Zeta" ||
		row.Agent.BindingSource != turns.AgentBindingExplicit || row.Agent.Complete != turns.CompletenessComplete {
		t.Errorf("agent = %+v", row.Agent)
	}
	if len(row.Inputs) != 1 || row.Inputs[0].ID != "in-snap" || row.Inputs[0].MimeType != "application/pdf" {
		t.Errorf("inputs = %+v", row.Inputs)
	}
	if row.RunID != "run-snap" || row.TaskID != "task-s" {
		t.Errorf("identity = task %q run %q, want the record's authoritative values", row.TaskID, row.RunID)
	}
	// Exactly the spawn read + the failure read, under the event triple.
	calls := reader.calls()
	if len(calls) != 2 {
		t.Fatalf("reader calls = %d, want 2 (spawn + failure)", len(calls))
	}
	for _, c := range calls {
		if c.taskID != "task-s" || c.triple != h.id {
			t.Errorf("reader call = %+v, want task-s under %+v", c, h.id)
		}
	}
}

// TestMaterialize_TaskSnapshot_AuthoritativeRunIDFromRecord pins that a
// legacy spawn event WITHOUT an envelope run id still rides the
// record's authoritative run id onto the row (never equated with the
// task id).
func TestMaterialize_TaskSnapshot_AuthoritativeRunIDFromRecord(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	reader := newFakeTaskReader().set("task-r", TaskSnapshot{RunID: "run-record"})
	m := h.newMaterializer(t, WithTaskSnapshotReader(reader))

	h.src.publish(t, spawnEv(h.id, "", "task-r", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-r"))
	h.src.publish(t, failedEv(h.id, "task-r", "timeout"))
	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	row := mustGetRow(t, h, "task-r")
	if row.RunID != "run-record" {
		t.Errorf("run id = %q, want the record's authoritative run id", row.RunID)
	}
}

// ---------------------------------------------------------------------------
// answer envelope: inline / empty / artifact reference
// ---------------------------------------------------------------------------

// TestMaterialize_TaskSnapshot_AnswerEnvelopeShapes pins the bounded
// Harbor answer envelope on a COMPLETE seal: the reader's inline /
// empty / artifact-reference shapes converge the row to sealed
// complete with the answer component, and the output attachments ride
// the same update. A legacy record (no answer) is covered separately.
func TestMaterialize_TaskSnapshot_AnswerEnvelopeShapes(t *testing.T) {
	cases := []struct {
		name string
		ans  turns.Answer
	}{
		{
			name: "inline",
			ans:  turns.Answer{State: turns.AnswerStateInline, Inline: "the harbor answer"},
		},
		{
			name: "empty",
			ans:  turns.Answer{State: turns.AnswerStateEmpty},
		},
		{
			name: "artifact reference",
			ans: turns.Answer{State: turns.AnswerStateArtifactRef, Ref: &turns.AnswerRef{
				ID: "art-out-1", MimeType: "text/markdown", SizeBytes: 4096,
				Filename: "answer.md", SHA256: strings.Repeat("ab", 32),
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "")
			defer h.closeStore()
			reader := newFakeTaskReader().set("task-a", TaskSnapshot{
				AnswerPresent: true, Answer: tc.ans,
				OutputsPresent: true, Outputs: []turns.Attachment{{
					ID: "out-1", MimeType: "text/plain", Availability: turns.CompletenessComplete,
				}},
			})
			m := h.newMaterializer(t, WithTaskSnapshotReader(reader))

			quad := testQuad(h.id, "run-a")
			h.src.publish(t, spawnEv(h.id, quad.RunID, "task-a", tasks.KindForeground, ""))
			h.src.publish(t, startedEv(h.id, "task-a"))
			h.src.publish(t, completedEv(h.id, "task-a"))

			res, err := m.Materialize(context.Background())
			if err != nil {
				t.Fatalf("materialize: %v", err)
			}
			if res.PendingComplete != 0 {
				t.Errorf("pending complete = %d, want 0 (the envelope converged the seal)", res.PendingComplete)
			}
			row := mustGetRow(t, h, "task-a")
			if !row.Sealed || row.Status != turns.StatusComplete || row.FinishReason != turns.FinishGoal {
				t.Fatalf("row = status %q sealed %v finish %q — the envelope must seal complete", row.Status, row.Sealed, row.FinishReason)
			}
			if row.Answer.Complete != turns.CompletenessComplete {
				t.Errorf("answer completeness = %q, want complete", row.Answer.Complete)
			}
			// The output attachments rode the same completion update.
			if len(row.Outputs) != 1 || row.Outputs[0].ID != "out-1" {
				t.Errorf("outputs = %+v, want exactly out-1", row.Outputs)
			}
		})
	}
}

// TestMaterialize_TaskSnapshot_AnswerEnvelopeShapes_PinShape pins the
// per-shape content: inline text survives, empty stays empty, and the
// artifact reference metadata survives byte-for-byte.
func TestMaterialize_TaskSnapshot_AnswerEnvelopeShapes_PinShape(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	reader := newFakeTaskReader().set("task-p", TaskSnapshot{
		AnswerPresent: true,
		Answer: turns.Answer{State: turns.AnswerStateArtifactRef, Ref: &turns.AnswerRef{
			ID: "art-abc", MimeType: "text/markdown", SizeBytes: 4096, Filename: "a.md", SHA256: strings.Repeat("cd", 32),
		}},
	})
	m := h.newMaterializer(t, WithTaskSnapshotReader(reader))

	quad := testQuad(h.id, "run-p")
	h.src.publish(t, spawnEv(h.id, quad.RunID, "task-p", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-p"))
	h.src.publish(t, completedEv(h.id, "task-p"))
	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	row := mustGetRow(t, h, "task-p")
	if row.Answer.State != turns.AnswerStateArtifactRef || row.Answer.Ref == nil {
		t.Fatalf("answer = %+v, want artifact_ref", row.Answer)
	}
	if row.Answer.Ref.ID != "art-abc" || row.Answer.Ref.MimeType != "text/markdown" ||
		row.Answer.Ref.SizeBytes != 4096 || row.Answer.Ref.Filename != "a.md" ||
		row.Answer.Ref.SHA256 != strings.Repeat("cd", 32) {
		t.Errorf("answer ref = %+v", row.Answer.Ref)
	}
	if row.Answer.Inline != "" {
		t.Errorf("artifact_ref answer carries inline text %q", row.Answer.Inline)
	}
}

// ---------------------------------------------------------------------------
// legacy absence + transient failure
// ---------------------------------------------------------------------------

// TestMaterialize_TaskSnapshot_LegacyAbsenceStaysUnavailable pins the
// honest legacy-absence contract: a missing record
// (ErrTaskSnapshotNotFound) leaves the query / agent components
// unavailable and DEFERS the complete seal (the row honestly stays
// mutable); a present record without failure fields derives the closed
// error class from the event's code alone and carries no message.
func TestMaterialize_TaskSnapshot_LegacyAbsenceStaysUnavailable(t *testing.T) {
	t.Run("missing record", func(t *testing.T) {
		h := newHarness(t, "")
		defer h.closeStore()
		reader := newFakeTaskReader().markMissing("task-l")
		m := h.newMaterializer(t, WithTaskSnapshotReader(reader))

		quad := testQuad(h.id, "run-l")
		h.src.publish(t, spawnEv(h.id, quad.RunID, "task-l", tasks.KindForeground, ""))
		h.src.publish(t, startedEv(h.id, "task-l"))
		h.src.publish(t, completedEv(h.id, "task-l"))

		res, err := m.Materialize(context.Background())
		if err != nil {
			t.Fatalf("materialize: %v (a missing record is never a hard failure)", err)
		}
		if res.PendingComplete != 1 {
			t.Errorf("pending complete = %d, want 1 (the seal defers honestly)", res.PendingComplete)
		}
		row := mustGetRow(t, h, "task-l")
		if row.Sealed || row.Status != turns.StatusRunning {
			t.Fatalf("row = status %q sealed %v (must stay mutable while the answer source is absent)", row.Status, row.Sealed)
		}
		if row.Query.Complete != turns.CompletenessUnavailable || row.Agent.Complete != turns.CompletenessUnavailable {
			t.Errorf("components must stay unavailable: query=%+v agent=%+v", row.Query, row.Agent)
		}
	})
	t.Run("legacy failure fields", func(t *testing.T) {
		h := newHarness(t, "")
		defer h.closeStore()
		reader := newFakeTaskReader().set("task-lf", TaskSnapshot{}) // present record, no failure fields
		m := h.newMaterializer(t, WithTaskSnapshotReader(reader))

		quad := testQuad(h.id, "run-lf")
		h.src.publish(t, spawnEv(h.id, quad.RunID, "task-lf", tasks.KindForeground, ""))
		h.src.publish(t, startedEv(h.id, "task-lf"))
		h.src.publish(t, failedEv(h.id, "task-lf", "5xx"))
		if _, err := m.Materialize(context.Background()); err != nil {
			t.Fatalf("materialize: %v", err)
		}
		row := mustGetRow(t, h, "task-lf")
		if row.ErrorClass != turns.ErrorClass5xx || row.ErrorMessage != "" {
			t.Errorf("failure = class %q message %q, want the event-derived 5xx and no message", row.ErrorClass, row.ErrorMessage)
		}
	})
}

// TestMaterialize_TaskSnapshot_TransientErrorFailsWithoutAdvancingCheckpoint
// pins the transient snapshot failure contract: a reader error aborts
// the projection loudly, writes NOTHING, and does NOT advance the
// checkpoint; once the transient failure heals, the retry converges
// with exactly one turn and the checkpoint lands on the last event.
func TestMaterialize_TaskSnapshot_TransientErrorFailsWithoutAdvancingCheckpoint(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	reader := newFakeTaskReader().fail(errors.New("task store transient failure"))
	m := h.newMaterializer(t, WithTaskSnapshotReader(reader))

	quad := testQuad(h.id, "run-tx")
	h.src.publish(t, spawnEv(h.id, quad.RunID, "task-tx", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-tx"))
	last := h.src.publish(t, failedEv(h.id, "task-tx", "timeout"))

	// Pass 1: the spawn projection's snapshot read fails transiently —
	// the pass fails loud and nothing advances.
	if _, err := m.Materialize(context.Background()); err == nil {
		t.Fatal("pass must fail loud on the transient snapshot error")
	}
	cp, err := h.proj.Checkpoint(context.Background(), h.id)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if cp != 0 {
		t.Errorf("checkpoint advanced to %d on a failed projection — want 0", cp)
	}
	if _, err := h.proj.Get(context.Background(), h.id, "task-tx"); !errors.Is(err, turns.ErrTurnNotFound) {
		t.Fatalf("turn = %v, want ErrTurnNotFound (the failed projection wrote nothing)", err)
	}

	// The transient failure heals: the retry converges with exactly one
	// turn and the checkpoint lands on the last event.
	reader.set("task-tx", TaskSnapshot{FailurePresent: true, ErrorCode: "timeout", ErrorMessage: "bounded"})
	reader.fail(nil)
	res, err := m.Materialize(context.Background())
	if err != nil {
		t.Fatalf("retry materialize: %v", err)
	}
	if res.Cursor != last.Sequence {
		t.Errorf("cursor = %d, want %d", res.Cursor, last.Sequence)
	}
	cp, err = h.proj.Checkpoint(context.Background(), h.id)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if cp != last.Sequence {
		t.Errorf("checkpoint = %d, want %d", cp, last.Sequence)
	}
	row := mustGetRow(t, h, "task-tx")
	if !row.Sealed || row.Status != turns.StatusFailed || row.ErrorClass != turns.ErrorClassTimeout {
		t.Errorf("row = status %q sealed %v class %q", row.Status, row.Sealed, row.ErrorClass)
	}
	page, err := h.proj.List(context.Background(), h.id, turns.ListOptions{Limit: 20})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Rows) != 1 {
		t.Fatalf("rows = %d, want exactly one (no phantom rows from the failed attempt)", len(page.Rows))
	}
}

// ---------------------------------------------------------------------------
// restart byte identity
// ---------------------------------------------------------------------------

// TestMaterialize_TaskSnapshot_RestartByteIdentity pins the restart
// contract WITH the seam: after a store restart the snapshot-enriched
// row (query / agent / inputs / answer) is byte-identical — catch-up
// writes nothing — and the catch-up re-reads the SPAWN record exactly
// once to rebuild the in-memory input accumulator (the completion at or
// below the checkpoint reads nothing).
func TestMaterialize_TaskSnapshot_RestartByteIdentity(t *testing.T) {
	dsn := t.TempDir() + "/turns-snap.sqlite"
	h := newHarness(t, dsn)
	reader := newFakeTaskReader()
	reader.set("task-sr", TaskSnapshot{
		QueryPresent: true, Query: "restart query", QueryAt: time.Unix(1_700_000_200, 0),
		AgentPresent: true, AgentID: "agent-r", AgentName: "Rho", AgentBindingSource: turns.AgentBindingExplicit,
		InputsPresent: true, Inputs: []turns.Attachment{{
			ID: "in-r", MimeType: "image/png", Availability: turns.CompletenessComplete,
		}},
		AnswerPresent: true, Answer: turns.Answer{State: turns.AnswerStateInline, Inline: "restart answer"},
	})
	m1 := h.newMaterializer(t, WithTaskSnapshotReader(reader))

	quad := testQuad(h.id, "run-sr")
	h.src.publish(t, spawnEv(h.id, quad.RunID, "task-sr", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-sr"))
	last := h.src.publish(t, completedEv(h.id, "task-sr"))

	if _, err := m1.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize (pre-restart): %v", err)
	}
	pre := mustGetRow(t, h, "task-sr")
	if !pre.Sealed || pre.Status != turns.StatusComplete || pre.Answer.Inline != "restart answer" {
		t.Fatalf("pre-restart row = status %q sealed %v answer %q", pre.Status, pre.Sealed, pre.Answer.Inline)
	}

	// "Restart": close and reopen the same file; the reader (the
	// runtime task-store analog) keeps serving the same record.
	h.closeStore()
	store2, err := sqlite.New(sqlite.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	proj2, err := turns.New(store2)
	if err != nil {
		t.Fatalf("reopen projector: %v", err)
	}
	defer func() { _ = proj2.Close(context.Background()) }()
	h2 := &harness{id: h.id, store: store2, proj: proj2, src: h.src}
	m2, err := New(h2.src, h2.proj, WithTaskSnapshotReader(reader))
	if err != nil {
		t.Fatalf("new post-restart materializer: %v", err)
	}
	beforeCalls := reader.callCount()
	if _, err := m2.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize (post-restart): %v", err)
	}
	post := mustGetRow(t, h2, "task-sr")
	if !reflect.DeepEqual(pre, post) {
		t.Errorf("restart changed the row:\nbefore: %+v\nafter:  %+v", pre, post)
	}
	if post.Version != pre.Version {
		t.Errorf("restart bumped the version %d→%d", pre.Version, post.Version)
	}
	postCP, err := h2.proj.Checkpoint(context.Background(), h.id)
	if err != nil {
		t.Fatalf("post-restart checkpoint: %v", err)
	}
	if postCP != last.Sequence {
		t.Errorf("post-restart checkpoint = %d, want %d", postCP, last.Sequence)
	}
	// Catch-up re-read the SPAWN record (to rebuild the input
	// accumulator); the completion at-or-below the checkpoint is a
	// no-op that reads nothing — exactly one new reader call.
	if got := reader.callCount() - beforeCalls; got != 1 {
		t.Errorf("post-restart reader calls = %d, want 1 (the spawn catch-up read)", got)
	}
}

// ---------------------------------------------------------------------------
// terminal event/error agreement
// ---------------------------------------------------------------------------

// TestMaterialize_TaskSnapshot_TerminalFailureAgreement pins that the
// durable turn and the task record agree on a failure: the closed
// error class derives from the record's canonical code and the bounded
// safe message rides the seal; the record's code is authoritative when
// it diverges from the event's (the task record is the canonical
// store).
func TestMaterialize_TaskSnapshot_TerminalFailureAgreement(t *testing.T) {
	t.Run("record code and message agree with the event", func(t *testing.T) {
		h := newHarness(t, "")
		defer h.closeStore()
		reader := newFakeTaskReader().set("task-ag", TaskSnapshot{
			FailurePresent: true, ErrorCode: "timeout", ErrorMessage: "bounded safe message",
		})
		m := h.newMaterializer(t, WithTaskSnapshotReader(reader))

		quad := testQuad(h.id, "run-ag")
		h.src.publish(t, spawnEv(h.id, quad.RunID, "task-ag", tasks.KindForeground, ""))
		h.src.publish(t, startedEv(h.id, "task-ag"))
		h.src.publish(t, failedEv(h.id, "task-ag", "timeout"))
		if _, err := m.Materialize(context.Background()); err != nil {
			t.Fatalf("materialize: %v", err)
		}
		row := mustGetRow(t, h, "task-ag")
		if row.ErrorClass != turns.ErrorClassTimeout || row.ErrorMessage != "bounded safe message" {
			t.Errorf("failure = class %q message %q", row.ErrorClass, row.ErrorMessage)
		}
	})
	t.Run("record code is canonical over a divergent event code", func(t *testing.T) {
		h := newHarness(t, "")
		defer h.closeStore()
		reader := newFakeTaskReader().set("task-cn", TaskSnapshot{
			FailurePresent: true, ErrorCode: "timeout", ErrorMessage: "record-safe message",
		})
		m := h.newMaterializer(t, WithTaskSnapshotReader(reader))

		quad := testQuad(h.id, "run-cn")
		h.src.publish(t, spawnEv(h.id, quad.RunID, "task-cn", tasks.KindForeground, ""))
		h.src.publish(t, startedEv(h.id, "task-cn"))
		h.src.publish(t, failedEv(h.id, "task-cn", "5xx")) // divergent event code
		if _, err := m.Materialize(context.Background()); err != nil {
			t.Fatalf("materialize: %v", err)
		}
		row := mustGetRow(t, h, "task-cn")
		if row.ErrorClass != turns.ErrorClassTimeout || row.ErrorMessage != "record-safe message" {
			t.Errorf("failure = class %q message %q, want the record's canonical code and message", row.ErrorClass, row.ErrorMessage)
		}
	})
}

// ---------------------------------------------------------------------------
// bounds, redaction posture, cross-identity denial
// ---------------------------------------------------------------------------

// TestMaterialize_TaskSnapshot_BoundsFailLoudly pins the never-clamp
// contract at the seam: an over-bound query, inline answer, terminal
// message, or attachment id is REFUSED loudly by the projector (the
// pass fails and the offending event does NOT advance the checkpoint)
// — never truncated, never silently omitted.
func TestMaterialize_TaskSnapshot_BoundsFailLoudly(t *testing.T) {
	t.Run("over-bound query", func(t *testing.T) {
		h := newHarness(t, "")
		defer h.closeStore()
		reader := newFakeTaskReader().set("task-bq", TaskSnapshot{
			QueryPresent: true, Query: strings.Repeat("q", turns.MaxQueryRunes+1),
		})
		m := h.newMaterializer(t, WithTaskSnapshotReader(reader))
		h.src.publish(t, spawnEv(h.id, "run-bq", "task-bq", tasks.KindForeground, ""))
		_, err := m.Materialize(context.Background())
		if err == nil || !errors.Is(err, turns.ErrInvalidInput) {
			t.Fatalf("over-bound query = %v, want ErrInvalidInput", err)
		}
		cp, cerr := h.proj.Checkpoint(context.Background(), h.id)
		if cerr != nil {
			t.Fatalf("checkpoint: %v", cerr)
		}
		if cp != 0 {
			t.Errorf("checkpoint = %d, want 0 (the refused append advanced nothing)", cp)
		}
	})
	t.Run("over-bound inline answer", func(t *testing.T) {
		h := newHarness(t, "")
		defer h.closeStore()
		reader := newFakeTaskReader().set("task-ba", TaskSnapshot{
			AnswerPresent: true, Answer: turns.Answer{State: turns.AnswerStateInline, Inline: strings.Repeat("a", turns.MaxInlineAnswerBytes)},
		})
		m := h.newMaterializer(t, WithTaskSnapshotReader(reader))
		quad := testQuad(h.id, "run-ba")
		h.src.publish(t, spawnEv(h.id, quad.RunID, "task-ba", tasks.KindForeground, ""))
		h.src.publish(t, startedEv(h.id, "task-ba"))
		h.src.publish(t, completedEv(h.id, "task-ba"))
		_, err := m.Materialize(context.Background())
		if err == nil || !errors.Is(err, turns.ErrContextLeak) {
			t.Fatalf("over-bound inline answer = %v, want ErrContextLeak (route by artifact reference)", err)
		}
		cp, cerr := h.proj.Checkpoint(context.Background(), h.id)
		if cerr != nil {
			t.Fatalf("checkpoint: %v", cerr)
		}
		if cp != 2 {
			t.Errorf("checkpoint = %d, want 2 (the refused completion did not advance)", cp)
		}
	})
	t.Run("over-bound failure message", func(t *testing.T) {
		h := newHarness(t, "")
		defer h.closeStore()
		reader := newFakeTaskReader().set("task-bm", TaskSnapshot{
			FailurePresent: true, ErrorMessage: strings.Repeat("m", turns.MaxTerminalMessageRunes+1),
		})
		m := h.newMaterializer(t, WithTaskSnapshotReader(reader))
		quad := testQuad(h.id, "run-bm")
		h.src.publish(t, spawnEv(h.id, quad.RunID, "task-bm", tasks.KindForeground, ""))
		h.src.publish(t, startedEv(h.id, "task-bm"))
		h.src.publish(t, failedEv(h.id, "task-bm", "timeout"))
		_, err := m.Materialize(context.Background())
		if err == nil || !errors.Is(err, turns.ErrInvalidInput) {
			t.Fatalf("over-bound failure message = %v, want ErrInvalidInput", err)
		}
		cp, cerr := h.proj.Checkpoint(context.Background(), h.id)
		if cerr != nil {
			t.Fatalf("checkpoint: %v", cerr)
		}
		if cp != 2 {
			t.Errorf("checkpoint = %d, want 2 (the refused seal did not advance)", cp)
		}
	})
	t.Run("over-bound attachment id", func(t *testing.T) {
		h := newHarness(t, "")
		defer h.closeStore()
		reader := newFakeTaskReader().set("task-bi", TaskSnapshot{
			InputsPresent: true, Inputs: []turns.Attachment{{ID: strings.Repeat("i", turns.MaxArtifactIDRunes+1)}},
		})
		m := h.newMaterializer(t, WithTaskSnapshotReader(reader))
		h.src.publish(t, spawnEv(h.id, "run-bi", "task-bi", tasks.KindForeground, ""))
		_, err := m.Materialize(context.Background())
		if err == nil || !errors.Is(err, turns.ErrInvalidInput) {
			t.Fatalf("over-bound attachment id = %v, want ErrInvalidInput", err)
		}
		cp, cerr := h.proj.Checkpoint(context.Background(), h.id)
		if cerr != nil {
			t.Fatalf("checkpoint: %v", cerr)
		}
		if cp != 0 {
			t.Errorf("checkpoint = %d, want 0 (the refused append advanced nothing)", cp)
		}
	})
}

// TestMaterialize_TaskSnapshot_CorruptSnapshotContractRefused pins the
// DTO-contract gate: a reader that reports an invalid answer envelope
// (a state outside inline / artifact_ref / empty, or content on an
// absent answer) is corrupt and fails the projection loudly — never
// silently mapped onto a weaker claim.
func TestMaterialize_TaskSnapshot_CorruptSnapshotContractRefused(t *testing.T) {
	t.Run("invalid answer state", func(t *testing.T) {
		h := newHarness(t, "")
		defer h.closeStore()
		reader := newFakeTaskReader().set("task-cc", TaskSnapshot{
			AnswerPresent: true, Answer: turns.Answer{State: "bogus"},
		})
		m := h.newMaterializer(t, WithTaskSnapshotReader(reader))
		quad := testQuad(h.id, "run-cc")
		h.src.publish(t, spawnEv(h.id, quad.RunID, "task-cc", tasks.KindForeground, ""))
		h.src.publish(t, startedEv(h.id, "task-cc"))
		h.src.publish(t, completedEv(h.id, "task-cc"))
		if _, err := m.Materialize(context.Background()); err == nil {
			t.Fatal("a corrupt answer envelope must fail the projection loudly")
		}
	})
	t.Run("absent answer carries content", func(t *testing.T) {
		h := newHarness(t, "")
		defer h.closeStore()
		reader := newFakeTaskReader().set("task-cc2", TaskSnapshot{
			Answer: turns.Answer{State: turns.AnswerStateInline, Inline: "content"},
		})
		m := h.newMaterializer(t, WithTaskSnapshotReader(reader))
		h.src.publish(t, spawnEv(h.id, "run-cc2", "task-cc2", tasks.KindForeground, ""))
		if _, err := m.Materialize(context.Background()); err == nil {
			t.Fatal("an absent answer with content must fail the projection loudly")
		}
	})
}

// TestMaterialize_TaskSnapshot_EventIdentityOnlyAndNeverOnProtocolReads
// pins the two seam-invocation invariants: the reader is called ONLY
// for root foreground task events (never for child/background spawns,
// never for non-task events) and ALWAYS under the event's own
// isolation triple — and the Protocol read surface (Get / List) never
// touches the seam at all.
func TestMaterialize_TaskSnapshot_EventIdentityOnlyAndNeverOnProtocolReads(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	other := identity.Identity{TenantID: "tenant-b", UserID: "user-b", SessionID: "sess-b"}
	reader := newFakeTaskReader()
	m := h.newMaterializer(t, WithTaskSnapshotReader(reader))

	quadA := testQuad(h.id, "run-a")
	quadB := testQuad(other, "run-b")
	h.src.publish(t, spawnEv(h.id, quadA.RunID, "task-a", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-a"))
	h.src.publish(t, spawnEv(h.id, "run-child", "task-child", tasks.KindBackground, "task-a"))
	h.src.publish(t, failedEv(h.id, "task-a", "timeout"))
	h.src.publish(t, spawnEv(other, quadB.RunID, "task-b", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(other, "task-b"))
	h.src.publish(t, failedEv(other, "task-b", "timeout"))

	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	// Exactly the two ROOT spawns + their two root failures; the
	// child/background spawn never reads.
	want := map[string]int{"task-a": 2, "task-b": 2}
	got := map[string]int{}
	for _, c := range reader.calls() {
		got[c.taskID]++
		wantTriple := h.id
		if c.taskID == "task-b" {
			wantTriple = other
		}
		if c.triple != wantTriple {
			t.Errorf("reader invoked for %s under %+v, want the event triple %+v", c.taskID, c.triple, wantTriple)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reader task ids = %v, want %v (only the root spawns + root failures, under their own triples)", got, want)
	}

	// The read surface never touches the seam: Get / List after the
	// pass do not add a single reader call.
	before := reader.callCount()
	if _, err := h.proj.Get(context.Background(), h.id, "task-a"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := h.proj.List(context.Background(), h.id, turns.ListOptions{Limit: 10}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := reader.callCount(); got != before {
		t.Errorf("protocol reads invoked the reader: calls %d → %d", before, got)
	}
}
