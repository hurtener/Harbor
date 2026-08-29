// Behaviour-parity golden tests for the promoted RunContext-population
// helpers (Phase 110b — D-195; Phase 111d — D-201 deleted the
// `ExtractSkillKeywords` shaper per its deprecation notice and added
// the Directory-view projection). These tables pin the shapes the
// planner renders so a refactor cannot drift the prompt shape
// (memory / skills projections).

package runctx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	artinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	"github.com/hurtener/Harbor/internal/skills"
)

// TestEmitterAndPublisher_MatchRunContextCallbackShapes is the
// compile-shaped assertion the phase plan requires from "a package
// that may import both": `events.IdentityStampingEmitter`'s return
// satisfies `planner.RunContext.Emit`, and `llm.NewChunkPublisher`'s
// return adapts onto `planner.RunContext.OnChunk` with the documented
// one-line kind wrapper.
func TestEmitterAndPublisher_MatchRunContextCallbackShapes(t *testing.T) {
	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
		RunID:    "r",
	}
	bus := &captureBus{}
	pub := llm.NewChunkPublisher(bus, q, "task-1", slog.Default())
	rc := planner.RunContext{
		Emit: events.IdentityStampingEmitter(bus, q, slog.Default()),
		OnChunk: func(delta string, done bool, kind planner.ChunkKind) {
			pub(delta, done, string(kind))
		},
	}
	if rc.Emit == nil || rc.OnChunk == nil {
		t.Fatal("constructor returns did not assign onto RunContext callbacks")
	}
}

// captureBus is a minimal EventBus double for the compile-shape test
// above; the behavioural emitter/publisher tests live next to their
// constructors (internal/events, internal/llm).
type captureBus struct{ events.EventBus }

func (b *captureBus) Publish(context.Context, events.Event) error { return nil }
func TestProjectMemoryBlocks_EmptyPatchReturnsNil(t *testing.T) {
	if got := runctx.ProjectMemoryBlocks(memory.LLMContextPatch{}); got != nil {
		t.Errorf("empty patch: got %v, want nil (wrapper omitted)", got)
	}
}

// TestProjectMemoryBlocks_GoldenShape pins the conversation-tier map
// shape byte-for-byte against what the pre-110b cmd helper produced.
func TestProjectMemoryBlocks_GoldenShape(t *testing.T) {
	patch := memory.LLMContextPatch{
		Strategy: memory.StrategyTruncation,
		Summary:  "rolling summary",
		RecentTurns: []memory.ConversationTurn{
			{UserMessage: "q1", AssistantResponse: "a1"},
			{UserMessage: "q2", AssistantResponse: "a2"},
		},
	}
	got := runctx.ProjectMemoryBlocks(patch)
	if got == nil {
		t.Fatal("non-empty patch projected to nil")
	}
	want := map[string]any{
		"strategy": string(memory.StrategyTruncation),
		"recent_turns": []map[string]any{
			{"user": "q1", "assistant": "a1"},
			{"user": "q2", "assistant": "a2"},
		},
		"summary": "rolling summary",
	}
	if !reflect.DeepEqual(got.Conversation, want) {
		t.Errorf("conversation block = %#v, want %#v", got.Conversation, want)
	}
	if got.External != nil {
		t.Errorf("External tier = %v, want nil (V1.1 ships Conversation only)", got.External)
	}
}

// TestProjectMemoryBlocks_NoSummaryOmitsKey — the `summary` key is
// only present when the patch carries one.
func TestProjectMemoryBlocks_NoSummaryOmitsKey(t *testing.T) {
	got := runctx.ProjectMemoryBlocks(memory.LLMContextPatch{
		RecentTurns: []memory.ConversationTurn{{UserMessage: "q", AssistantResponse: "a"}},
	})
	if got == nil {
		t.Fatal("patch with turns projected to nil")
	}
	conv, ok := got.Conversation.(map[string]any)
	if !ok {
		t.Fatalf("Conversation tier is %T, want map[string]any", got.Conversation)
	}
	if _, present := conv["summary"]; present {
		t.Error("summary key present on a summary-less patch")
	}
}

func TestProjectSkillsContext_EmptyReturnsNil(t *testing.T) {
	if got := runctx.ProjectSkillsContext(nil); got != nil {
		t.Errorf("nil ranked: got %v, want nil", got)
	}
	if got := runctx.ProjectSkillsContext([]skills.RankedSkill{}); got != nil {
		t.Errorf("empty ranked: got %v, want nil", got)
	}
}

// TestProjectSkillsContext_GoldenShape pins the per-skill entry map
// shape: name + title always; description + steps only when present.
func TestProjectSkillsContext_GoldenShape(t *testing.T) {
	ranked := []skills.RankedSkill{
		{Skill: skills.Skill{
			Name:        "full",
			Title:       "Full Skill",
			Description: "does things",
			Steps:       []string{"step1", "step2"},
		}},
		{Skill: skills.Skill{Name: "bare", Title: "Bare Skill"}},
	}
	got := runctx.ProjectSkillsContext(ranked)
	want := []any{
		map[string]any{
			"name":        "full",
			"title":       "Full Skill",
			"description": "does things",
			"steps":       []string{"step1", "step2"},
		},
		map[string]any{
			"name":  "bare",
			"title": "Bare Skill",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("skills context = %#v, want %#v", got, want)
	}
}

// TestProjectSkillsDirectory pins the Phase 111d (D-201) projection:
// each `skills.SkillView` rides verbatim as one []any element (the
// planner JSON-encodes the slice into the `<skills_context>` wrapper);
// an empty input returns nil so the wrapper is omitted entirely.
func TestProjectSkillsDirectory(t *testing.T) {
	if got := runctx.ProjectSkillsDirectory(nil); got != nil {
		t.Errorf("empty views: got %#v, want nil", got)
	}
	views := []skills.SkillView{
		{Name: "triage-incident", Title: "Triage an incident", Trigger: "when a ticket arrives", TaskType: "triage", Pinned: true},
		{Name: "summarise-paper", Title: "Summarise a paper"},
	}
	got := runctx.ProjectSkillsDirectory(views)
	want := []any{views[0], views[1]}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("skills directory projection = %#v, want %#v", got, want)
	}
	// The projected block must JSON-encode with the SkillView wire
	// keys — this is the prompt-block shape the planner renders
	// (the 111d prompt-delta golden, asserted at the projection seam).
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal projected views: %v", err)
	}
	const wantJSON = `[{"name":"triage-incident","title":"Triage an incident","trigger":"when a ticket arrives","task_type":"triage","pinned":true},{"name":"summarise-paper","title":"Summarise a paper","pinned":false}]`
	if string(raw) != wantJSON {
		t.Errorf("projected JSON = %s, want %s", raw, wantJSON)
	}
}

// TestExtractAssistantAnswer pins every fallback branch: string
// payload verbatim; map with a string "answer"; map with a non-string
// "answer" → Sprintf of the map; map without "answer" → Sprintf; nil
// payload → string(Reason); arbitrary payload → Sprintf. Silent
// "nothing extracted" is forbidden (CLAUDE.md §5).
func TestExtractAssistantAnswer(t *testing.T) {
	cases := []struct {
		name string
		fin  planner.Finish
		want string
	}{
		{
			name: "string_payload_verbatim",
			fin:  planner.Finish{Reason: planner.FinishGoal, Payload: "the answer"},
			want: "the answer",
		},
		{
			name: "map_with_answer_key",
			fin: planner.Finish{Reason: planner.FinishGoal,
				Payload: map[string]any{"answer": "from map"}},
			want: "from map",
		},
		{
			name: "map_with_non_string_answer_falls_back_to_sprintf",
			fin: planner.Finish{Reason: planner.FinishGoal,
				Payload: map[string]any{"answer": 42}},
			want: "map[answer:42]",
		},
		{
			name: "nil_payload_falls_back_to_reason",
			fin:  planner.Finish{Reason: planner.FinishGoal, Payload: nil},
			want: "goal",
		},
		{
			name: "other_payload_sprintf",
			fin:  planner.Finish{Reason: planner.FinishGoal, Payload: 7},
			want: "7",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runctx.ExtractAssistantAnswer(tc.fin); got != tc.want {
				t.Errorf("ExtractAssistantAnswer = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- ResolveInputArtifacts -------------------------------------------------

var runctxTestQ = identity.Quadruple{
	Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-a"},
	RunID:    "run-1",
}

func newRunctxArtifactStore(t *testing.T) artifacts.ArtifactStore {
	t.Helper()
	store, err := artinmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatalf("artifacts inmem: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}

func runctxScope() artifacts.ArtifactScope {
	return artifacts.ArtifactScope{
		TenantID:  runctxTestQ.TenantID,
		UserID:    runctxTestQ.UserID,
		SessionID: runctxTestQ.SessionID,
	}
}

func TestResolveInputArtifacts_EmptyIDsReturnsNil(t *testing.T) {
	got := runctx.ResolveInputArtifacts(context.Background(), nil, runctxTestQ, nil, slog.Default(), runctx.InputArtifactOptions{})
	if got != nil {
		t.Errorf("empty ids: got %v, want nil", got)
	}
}

// TestResolveInputArtifacts_NilStoreWarnsAndReturnsNil — the D-166
// bounded-failure contract: a nil store with non-empty IDs degrades
// to a text-only prompt with a LOUD Warn, never a panic or a silent
// drop.
func TestResolveInputArtifacts_NilStoreWarnsAndReturnsNil(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	got := runctx.ResolveInputArtifacts(context.Background(), nil, runctxTestQ, []string{"art-1"}, logger, runctx.InputArtifactOptions{})
	if got != nil {
		t.Errorf("nil store: got %v, want nil", got)
	}
	if !strings.Contains(buf.String(), "no artifact store wired") {
		t.Errorf("expected loud Warn about missing store; log: %s", buf.String())
	}
}

// TestResolveInputArtifacts_TextRefOnly_ImageBytesInlined — the happy
// path over the REAL inmem driver: a text artifact resolves
// metadata-only (Bytes nil — the materializer renders an ArtifactStub
// ref); an image artifact gets its bytes inlined (Path 1 — DataURL).
func TestResolveInputArtifacts_TextRefOnly_ImageBytesInlined(t *testing.T) {
	store := newRunctxArtifactStore(t)
	ctx := context.Background()
	scope := runctxScope()

	textRef, err := store.PutText(ctx, scope, "hello world", artifacts.PutOpts{
		MimeType: "text/plain",
		Filename: "notes.txt",
	})
	if err != nil {
		t.Fatalf("PutText: %v", err)
	}
	imgBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x01, 0x02}
	imgRef, err := store.PutBytes(ctx, scope, imgBytes, artifacts.PutOpts{
		MimeType: "image/png",
		Filename: "shot.png",
	})
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}

	got := runctx.ResolveInputArtifacts(ctx, store, runctxTestQ,
		[]string{textRef.ID, imgRef.ID}, slog.Default(), runctx.InputArtifactOptions{})
	if len(got) != 2 {
		t.Fatalf("resolved %d views, want 2: %#v", len(got), got)
	}
	text, img := got[0], got[1]
	if text.ID != textRef.ID || text.MIME != "text/plain" || text.Filename != "notes.txt" {
		t.Errorf("text view metadata mismatch: %#v", text)
	}
	if text.Bytes != nil {
		t.Errorf("text view carried inline bytes; non-image MIMEs stay ref-only")
	}
	if img.ID != imgRef.ID || img.MIME != "image/png" {
		t.Errorf("image view metadata mismatch: %#v", img)
	}
	if !bytes.Equal(img.Bytes, imgBytes) {
		t.Errorf("image bytes not inlined: got %v, want %v", img.Bytes, imgBytes)
	}
}

// TestResolveInputArtifacts_MissingArtifactSkippedWithWarn — a GC'd /
// never-existed ID is skipped with a Warn; the rest of the slice
// survives.
func TestResolveInputArtifacts_MissingArtifactSkippedWithWarn(t *testing.T) {
	store := newRunctxArtifactStore(t)
	ctx := context.Background()
	ref, err := store.PutText(ctx, runctxScope(), "kept", artifacts.PutOpts{MimeType: "text/plain"})
	if err != nil {
		t.Fatalf("PutText: %v", err)
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	got := runctx.ResolveInputArtifacts(ctx, store, runctxTestQ,
		[]string{"no-such-artifact", ref.ID}, logger, runctx.InputArtifactOptions{})
	if len(got) != 1 || got[0].ID != ref.ID {
		t.Fatalf("resolved %#v, want only %q", got, ref.ID)
	}
	if !strings.Contains(buf.String(), "artifact not found") {
		t.Errorf("expected Warn for the missing artifact; log: %s", buf.String())
	}
}

// erroringStore wraps a real store, forcing errors on GetRef (for a
// chosen ID) and on Get (bytes fetch), so the two error branches of
// the resolver can be pinned. A boundary double is acceptable in a
// unit test; the integration test drives the real driver end-to-end.
type erroringStore struct {
	artifacts.ArtifactStore
	failRefID string
	failGet   bool
}

func (s *erroringStore) GetRef(ctx context.Context, scope artifacts.ArtifactScope, id string) (*artifacts.ArtifactRef, bool, error) {
	if id == s.failRefID {
		return nil, false, errors.New("forced GetRef failure")
	}
	return s.ArtifactStore.GetRef(ctx, scope, id)
}

func (s *erroringStore) Get(ctx context.Context, scope artifacts.ArtifactScope, id string) ([]byte, bool, error) {
	if s.failGet {
		return nil, false, errors.New("forced Get failure")
	}
	return s.ArtifactStore.Get(ctx, scope, id)
}

// TestResolveInputArtifacts_StoreErrorSkippedWithWarn — a GetRef
// error skips that ID loudly; the rest of the slice survives.
func TestResolveInputArtifacts_StoreErrorSkippedWithWarn(t *testing.T) {
	store := newRunctxArtifactStore(t)
	ctx := context.Background()
	ref, err := store.PutText(ctx, runctxScope(), "survives", artifacts.PutOpts{MimeType: "text/plain"})
	if err != nil {
		t.Fatalf("PutText: %v", err)
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	failing := &erroringStore{ArtifactStore: store, failRefID: "exploding-id"}
	got := runctx.ResolveInputArtifacts(ctx, failing, runctxTestQ,
		[]string{"exploding-id", ref.ID}, logger, runctx.InputArtifactOptions{})
	if len(got) != 1 || got[0].ID != ref.ID {
		t.Fatalf("resolved %#v, want only %q", got, ref.ID)
	}
	if !strings.Contains(buf.String(), "GetRef failed") {
		t.Errorf("expected Warn for the GetRef error; log: %s", buf.String())
	}
}

// TestResolveInputArtifacts_ImageBytesFetchError_RefOnlyFallback — a
// bytes-fetch failure on an image keeps the metadata entry with nil
// Bytes (the materializer renders a stub-text part), with a Warn.
func TestResolveInputArtifacts_ImageBytesFetchError_RefOnlyFallback(t *testing.T) {
	store := newRunctxArtifactStore(t)
	ctx := context.Background()
	imgRef, err := store.PutBytes(ctx, runctxScope(), []byte{0x01, 0x02}, artifacts.PutOpts{
		MimeType: "image/png",
	})
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	failing := &erroringStore{ArtifactStore: store, failGet: true}
	got := runctx.ResolveInputArtifacts(ctx, failing, runctxTestQ,
		[]string{imgRef.ID}, logger, runctx.InputArtifactOptions{})
	if len(got) != 1 {
		t.Fatalf("resolved %d views, want 1 (ref-only fallback keeps the entry)", len(got))
	}
	if got[0].Bytes != nil {
		t.Errorf("Bytes = %v, want nil on bytes-fetch failure", got[0].Bytes)
	}
	if !strings.Contains(buf.String(), "inline artifact bytes missing") {
		t.Errorf("expected Warn for the bytes fallback; log: %s", buf.String())
	}
}

// TestResolveInputArtifacts_IdentityScoped — the store enforces
// tenant isolation on read: an artifact Put under tenant B never
// resolves for tenant A's run (CLAUDE.md §6).
func TestResolveInputArtifacts_IdentityScoped(t *testing.T) {
	store := newRunctxArtifactStore(t)
	ctx := context.Background()
	otherScope := artifacts.ArtifactScope{TenantID: "tenant-b", UserID: "user-b", SessionID: "sess-b"}
	ref, err := store.PutText(ctx, otherScope, "secret", artifacts.PutOpts{MimeType: "text/plain"})
	if err != nil {
		t.Fatalf("PutText: %v", err)
	}
	got := runctx.ResolveInputArtifacts(ctx, store, runctxTestQ, []string{ref.ID}, slog.Default(), runctx.InputArtifactOptions{})
	if len(got) != 0 {
		t.Fatalf("cross-tenant artifact resolved: %#v — isolation breach", got)
	}
}
