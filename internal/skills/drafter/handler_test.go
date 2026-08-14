package drafter

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/skills/importer"
	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

// handler_test.go — the ordinary tool handler: exactly ONE immutable
// caller-scoped artifact per success, zero mutation on every failure
// path, caller-scope isolation, replay convergence, bounded review
// metadata, and the round trip through the canonical validate/commit
// ingest.

func newStore(t *testing.T) artifacts.ArtifactStore {
	t.Helper()
	store, err := inmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}

// newRealWriter builds a ScopedWriter over a fresh store under scope.
func newRealWriter(t *testing.T, scope artifacts.ArtifactScope) (*ScopedWriter, artifacts.ArtifactStore) {
	t.Helper()
	store := newStore(t)
	w, err := NewScopedWriter(store, scope)
	if err != nil {
		t.Fatalf("NewScopedWriter: %v", err)
	}
	return w, store
}

func TestCreateDraft_WritesOneImmutableCallerScopedArtifact(t *testing.T) {
	scope := artifacts.ArtifactScope{TenantID: "t", UserID: "u", SessionID: "s", TaskID: "r"}
	writer, store := newRealWriter(t, scope)
	a := newTestAdapter(t, stubClient{content: validDraftContent()})

	result, err := CreateDraft(runCtx(t, testQuad("t", "u", "s", "r")), a, writer, Args{Intent: "triage the inbox"})
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}

	if result.Installed {
		t.Fatal("result.Installed must be false (draft-only lane)")
	}
	if result.State != StateDraft {
		t.Fatalf("state = %q, want %q", result.State, StateDraft)
	}
	if result.Provenance != ProvenanceGeneratedDraft {
		t.Fatalf("provenance = %q, want %q", result.Provenance, ProvenanceGeneratedDraft)
	}
	if result.ArtifactRef == "" || !strings.HasPrefix(result.PackageHash, "v1:") || result.Name == "" {
		t.Fatalf("incomplete result: %+v", result)
	}
	if result.Summary == "" {
		t.Fatal("summary must be populated from the description")
	}

	// The one artifact exists under the caller scope with the exact
	// rendered bytes.
	bytesGot, found, err := store.Get(ctxWithStore(), scope, result.ArtifactRef)
	if err != nil || !found {
		t.Fatalf("artifact not found under caller scope: found=%v err=%v", found, err)
	}
	wantDoc, err := RenderSkillMD(skillpkg.PackageSkill{
		Name:        "triage-inbox",
		Title:       "Triage the shared inbox",
		Description: "Scan the shared inbox and triage every unread message into a queue.",
		Trigger:     "when the user asks to triage the inbox",
		TaskType:    "api",
		Tags:        []string{"triage", "inbox"},
		Steps: []string{
			"Fetch unread messages",
			"Classify each message by priority",
			"Assign each message to the right queue",
		},
		RequiredTools: []string{"inbox.read"},
	})
	if err != nil {
		t.Fatalf("RenderSkillMD(want): %v", err)
	}
	if !bytes.Equal(bytesGot, wantDoc) {
		t.Fatalf("stored bytes do not match the rendered SKILL.md:\n%s", bytesGot)
	}
}

// ctxWithStore returns a background context carrying no identity (used
// only for store reads, where identity is the scope parameter).
func ctxWithStore() context.Context { return context.Background() }

func TestCreateDraft_ReplayConvergesToSameArtifact(t *testing.T) {
	scope := artifacts.ArtifactScope{TenantID: "t", UserID: "u", SessionID: "s", TaskID: "r"}
	writer, store := newRealWriter(t, scope)
	a := newTestAdapter(t, stubClient{content: validDraftContent()})
	ctx := runCtx(t, testQuad("t", "u", "s", "r"))

	first, err := CreateDraft(ctx, a, writer, Args{Intent: "triage the inbox"})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := CreateDraft(ctx, a, writer, Args{Intent: "triage the inbox"})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	// Content addressing: the same caller scope + same bytes converge
	// to the SAME ref and hash — no duplicate mutable state.
	if first.ArtifactRef != second.ArtifactRef {
		t.Fatalf("replay refs differ: %q vs %q", first.ArtifactRef, second.ArtifactRef)
	}
	if first.PackageHash != second.PackageHash {
		t.Fatalf("replay hashes differ: %q vs %q", first.PackageHash, second.PackageHash)
	}
	refs, err := store.List(ctxWithStore(), artifacts.ArtifactScope{TenantID: "t", UserID: "u", SessionID: "s"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("expected exactly one stored artifact after replay, got %d", len(refs))
	}
}

func TestCreateDraft_ChangedFeedbackDistinctDraft(t *testing.T) {
	writer, _ := newRealWriter(t, artifacts.ArtifactScope{TenantID: "t", UserID: "u", SessionID: "s", TaskID: "r"})
	base := validDraftContent()
	revised := strings.Replace(base, "Classify each message by priority", "Classify each message by urgency", 1)
	client := stubClient{contentFor: func(ctx context.Context, req llm.CompleteRequest) string {
		for _, m := range req.Messages {
			if m.Role == llm.RoleUser && m.Content.Text != nil && strings.Contains(*m.Content.Text, "second pass") {
				return revised
			}
		}
		return base
	}}
	a := newTestAdapter(t, client)
	ctx := runCtx(t, testQuad("t", "u", "s", "r"))

	first, err := CreateDraft(ctx, a, writer, Args{Intent: "intent", Feedback: "first pass"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := CreateDraft(ctx, a, writer, Args{Intent: "intent", Feedback: "second pass"})
	if err != nil {
		t.Fatal(err)
	}
	if first.PackageHash == second.PackageHash {
		t.Fatal("changed feedback must produce a distinct package hash")
	}
}

func TestCreateDraft_TwoUserIsolation(t *testing.T) {
	store := newStore(t)
	scopeA := artifacts.ArtifactScope{TenantID: "t", UserID: "a", SessionID: "s", TaskID: "r"}
	scopeB := artifacts.ArtifactScope{TenantID: "t", UserID: "b", SessionID: "s", TaskID: "r"}
	writerA, err := NewScopedWriter(store, scopeA)
	if err != nil {
		t.Fatal(err)
	}
	writerB, err := NewScopedWriter(store, scopeB)
	if err != nil {
		t.Fatal(err)
	}
	// Each user's draft carries DIFFERENT bytes (a different name), so
	// the content-addressed refs differ too.
	contentA := strings.Replace(validDraftContent(), "triage-inbox", "user-a-skill", 1)
	contentB := strings.Replace(validDraftContent(), "triage-inbox", "user-b-skill", 1)
	client := stubClient{contentFor: func(ctx context.Context, req llm.CompleteRequest) string {
		q, _ := identity.QuadrupleFrom(ctx)
		if q.UserID == "b" {
			return contentB
		}
		return contentA
	}}
	a := newTestAdapter(t, client)

	resA, err := CreateDraft(runCtx(t, testQuad("t", "a", "s", "r")), a, writerA, Args{Intent: "intent"})
	if err != nil {
		t.Fatal(err)
	}
	resB, err := CreateDraft(runCtx(t, testQuad("t", "b", "s", "r")), a, writerB, Args{Intent: "intent"})
	if err != nil {
		t.Fatal(err)
	}
	if resA.ArtifactRef == resB.ArtifactRef {
		t.Fatal("two users' drafts must be distinct artifacts (isolation triple)")
	}

	// Cross-user reads fail as not-found without existence disclosure.
	if _, found, err := store.Get(ctxWithStore(), scopeB, resA.ArtifactRef); err != nil || found {
		t.Fatalf("user B read user A's draft: found=%v err=%v", found, err)
	}
	if _, found, err := store.Get(ctxWithStore(), scopeA, resB.ArtifactRef); err != nil || found {
		t.Fatalf("user A read user B's draft: found=%v err=%v", found, err)
	}
	// Each user sees exactly their own artifact in their own session.
	listA, err := store.List(ctxWithStore(), artifacts.ArtifactScope{TenantID: "t", UserID: "a", SessionID: "s"})
	if err != nil || len(listA) != 1 || listA[0].ID != resA.ArtifactRef {
		t.Fatalf("user A list = %+v err=%v", listA, err)
	}
	listB, err := store.List(ctxWithStore(), artifacts.ArtifactScope{TenantID: "t", UserID: "b", SessionID: "s"})
	if err != nil || len(listB) != 1 || listB[0].ID != resB.ArtifactRef {
		t.Fatalf("user B list = %+v err=%v", listB, err)
	}
}

func TestCreateDraft_ZeroMutationOnEveryFailurePath(t *testing.T) {
	// The spy writer records every Write call: any non-zero count on a
	// failure path is a mutation leak. The spy is write-only, so a
	// handler regression that reached a SkillStore / membership /
	// pack / capability seam could not even express itself through
	// it — the seam contract itself is the non-vacuity guard.
	writer := &spyWriter{}
	ctx := runCtx(t, testQuad("t", "u", "s", "r"))

	cases := []struct {
		name    string
		client  stubClient
		args    Args
		wantErr error
	}{
		{"refusal", stubClient{content: `{"refusal":"no"}`}, Args{Intent: "i"}, ErrModelRefused},
		{"malformed", stubClient{content: "not json"}, Args{Intent: "i"}, ErrMalformedModelOutput},
		{"authority field", stubClient{content: `{"name":"x","trigger":"t","steps":["s"],"scope":{}}`}, Args{Intent: "i"}, ErrForbiddenAuthorityField},
		{"client error", stubClient{err: errors.New("down")}, Args{Intent: "i"}, nil},
		{"empty intent", stubClient{content: validDraftContent()}, Args{Intent: "  "}, ErrIntentRequired},
		{"intent too large", stubClient{content: validDraftContent()}, Args{Intent: strings.Repeat("i", MaxIntentRunes+1)}, ErrIntentTooLarge},
		{"unrenderable", stubClient{content: `{"name":"x","trigger":"t","steps":["a\nb"]}`}, Args{Intent: "i"}, ErrUnrenderableSkill},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAdapter(t, tc.client)
			if _, err := CreateDraft(ctx, a, writer, tc.args); err == nil {
				t.Fatal("expected an error")
			}
			if n := writer.writeCount(); n != 0 {
				t.Fatalf("failure path made %d artifact writes (want 0)", n)
			}
		})
	}

	// Cancellation path: no write.
	t.Run("cancellation", func(t *testing.T) {
		ctx2, cancel := context.WithCancel(ctx)
		cancel()
		a := newTestAdapter(t, stubClient{content: validDraftContent()})
		if _, err := CreateDraft(ctx2, a, writer, Args{Intent: "i"}); err == nil {
			t.Fatal("expected cancellation error")
		}
		if n := writer.writeCount(); n != 0 {
			t.Fatalf("cancellation path made %d artifact writes (want 0)", n)
		}
	})
}

func TestCreateDraft_WriteFailureFailsLoudNoPartialMutation(t *testing.T) {
	writer := &spyWriter{err: errors.New("store full")}
	a := newTestAdapter(t, stubClient{content: validDraftContent()})
	_, err := CreateDraft(runCtx(t, testQuad("t", "u", "s", "r")), a, writer, Args{Intent: "i"})
	if err == nil {
		t.Fatal("expected write failure")
	}
	if !strings.Contains(err.Error(), "persist draft artifact") {
		t.Fatalf("err = %v, want wrapped persist failure", err)
	}
	if n := writer.writeCount(); n != 1 {
		t.Fatalf("write failure path made %d attempts (want exactly 1)", n)
	}
}

func TestCreateDraft_MissingIdentity(t *testing.T) {
	writer := &spyWriter{}
	a := newTestAdapter(t, stubClient{content: validDraftContent()})
	_, err := CreateDraft(context.Background(), a, writer, Args{Intent: "i"})
	if !errors.Is(err, ErrMissingIdentity) {
		t.Fatalf("err = %v, want ErrMissingIdentity", err)
	}
	if n := writer.writeCount(); n != 0 {
		t.Fatalf("missing-identity path made %d artifact writes (want 0)", n)
	}
}

func TestCreateDraft_NilWriter(t *testing.T) {
	a := newTestAdapter(t, stubClient{content: validDraftContent()})
	_, err := CreateDraft(runCtx(t, testQuad("t", "u", "s", "r")), a, nil, Args{Intent: "i"})
	if !errors.Is(err, ErrWriterRequired) {
		t.Fatalf("err = %v, want ErrWriterRequired", err)
	}
}

func TestCreateDraft_NilAdapter(t *testing.T) {
	writer := &spyWriter{}
	_, err := CreateDraft(runCtx(t, testQuad("t", "u", "s", "r")), nil, writer, Args{Intent: "i"})
	if err == nil {
		t.Fatal("expected error for nil adapter")
	}
	if n := writer.writeCount(); n != 0 {
		t.Fatalf("nil-adapter path made %d artifact writes (want 0)", n)
	}
}

func TestCreateDraft_SummaryBounded(t *testing.T) {
	// A description far longer than the summary bound must not leak
	// into the result; the artifact carries the full body.
	longDesc := strings.Repeat("lorem ipsum dolor ", 100) // 1800 runes
	content := strings.Replace(validDraftContent(), "Scan the shared inbox and triage every unread message into a queue.", longDesc, 1)
	a := newTestAdapter(t, stubClient{content: content})
	writer, _ := newRealWriter(t, artifacts.ArtifactScope{TenantID: "t", UserID: "u", SessionID: "s", TaskID: "r"})
	result, err := CreateDraft(runCtx(t, testQuad("t", "u", "s", "r")), a, writer, Args{Intent: "i"})
	if err != nil {
		t.Fatal(err)
	}
	if rl := len([]rune(result.Summary)); rl > MaxSummaryRunes {
		t.Fatalf("summary is %d runes (limit %d)", rl, MaxSummaryRunes)
	}
	if result.Summary == longDesc {
		t.Fatal("result leaked the full description")
	}
	if !strings.Contains(result.Summary, "lorem ipsum") {
		t.Fatalf("summary does not contain the excerpt: %q", result.Summary)
	}
}

func TestCreateDraft_WarningsForRequiredTools(t *testing.T) {
	a := newTestAdapter(t, stubClient{content: validDraftContent()})
	writer, _ := newRealWriter(t, artifacts.ArtifactScope{TenantID: "t", UserID: "u", SessionID: "s", TaskID: "r"})
	result, err := CreateDraft(runCtx(t, testQuad("t", "u", "s", "r")), a, writer, Args{Intent: "i"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "metadata only") && strings.Contains(w, "never treated as capability grants") {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings = %v, want the metadata-only required-tools warning", result.Warnings)
	}
}

// TestCreateDraft_ArtifactPassesValidateIngest is the canonical
// round-trip through the handler: the artifact bytes this tool writes
// are accepted by the canonical single-document ingest (the same
// ingest the validate/commit workflow runs) with the identical
// package hash and normalized representation.
func TestCreateDraft_ArtifactPassesValidateIngest(t *testing.T) {
	scope := artifacts.ArtifactScope{TenantID: "t", UserID: "u", SessionID: "s", TaskID: "r"}
	writer, store := newRealWriter(t, scope)
	a := newTestAdapter(t, stubClient{content: validDraftContent()})
	result, err := CreateDraft(runCtx(t, testQuad("t", "u", "s", "r")), a, writer, Args{Intent: "i"})
	if err != nil {
		t.Fatal(err)
	}

	doc, found, err := store.Get(ctxWithStore(), scope, result.ArtifactRef)
	if err != nil || !found {
		t.Fatalf("artifact read-back: found=%v err=%v", found, err)
	}
	imp := newPackageImporter(t)
	ingest, err := imp.ImportPackageMarkdown(context.Background(), importer.PackageMarkdownSource{
		Markdown: doc,
		PathHint: "draft.md",
	})
	if err != nil {
		t.Fatalf("validate/commit ingest rejected the draft artifact: %v", err)
	}
	if ingest.Hash != result.PackageHash {
		t.Fatalf("ingest hash %q != returned hash %q", ingest.Hash, result.PackageHash)
	}
	if ingest.Package.Skill.Name != result.Name {
		t.Fatalf("ingested name %q != returned name %q", ingest.Package.Skill.Name, result.Name)
	}
	if len(ingest.Package.Supports) != 0 {
		t.Fatalf("draft artifact must be resource-free, got %d support files", len(ingest.Package.Supports))
	}
}
