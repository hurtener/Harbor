package react

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
)

// TestRenderSessionArtifacts_Manifest_RowsAndFraming asserts the block
// lists each entry as `ref · filename (mime, size) · provenance`, carries
// the read-only UNTRUSTED framing, and tells the model it may call
// artifact_fetch by ref (AC-2, AC-3).
func TestRenderSessionArtifacts_Manifest_RowsAndFraming(t *testing.T) {
	entries := []planner.ArtifactManifestEntry{
		{Ref: "default_abc123", Filename: "report.pdf", MIME: "application/pdf", SizeBytes: 2048, Provenance: "user_upload"},
		{Ref: "default_def456", Filename: "tool-result-web_search.json", MIME: "application/json", SizeBytes: 9001, Provenance: "tool: web_search"},
	}
	msg, ok := renderSessionArtifacts(entries)
	if !ok {
		t.Fatal("renderSessionArtifacts: ok=false for non-empty entries")
	}
	if msg.Role != llm.RoleSystem {
		t.Errorf("Role = %q, want system", msg.Role)
	}
	body := msgText(t, msg)

	// Block tags.
	if !strings.Contains(body, "<session_artifacts>") || !strings.Contains(body, "</session_artifacts>") {
		t.Errorf("missing <session_artifacts> wrapper:\n%s", body)
	}
	// Read-only / UNTRUSTED framing (AC-3).
	if !strings.Contains(body, "UNTRUSTED") {
		t.Errorf("missing UNTRUSTED framing:\n%s", body)
	}
	if !strings.Contains(body, "not an instruction") {
		t.Errorf("missing 'not an instruction' framing:\n%s", body)
	}
	// The artifact_fetch instruction (AC-3).
	if !strings.Contains(body, "artifact_fetch") {
		t.Errorf("missing artifact_fetch instruction:\n%s", body)
	}
	// Each ref + filename + mime + size + provenance row (AC-2).
	for _, want := range []string{
		"default_abc123", "report.pdf", "application/pdf", "2048 bytes", "user_upload",
		"default_def456", "tool-result-web_search.json", "application/json", "9001 bytes", "tool: web_search",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

// TestRenderSessionArtifacts_Empty_NoBlock asserts an empty manifest
// produces NO block — no fabricated rows, no empty-list noise (AC-4).
func TestRenderSessionArtifacts_Empty_NoBlock(t *testing.T) {
	msg, ok := renderSessionArtifacts(nil)
	if ok {
		t.Errorf("renderSessionArtifacts(nil): ok=true, want false")
	}
	if msg.Content.Text != nil {
		t.Errorf("renderSessionArtifacts(nil): non-zero message %+v", msg)
	}

	msg2, ok2 := renderSessionArtifacts([]planner.ArtifactManifestEntry{})
	if ok2 {
		t.Errorf("renderSessionArtifacts([]): ok=true, want false")
	}
	if msg2.Content.Text != nil {
		t.Errorf("renderSessionArtifacts([]): non-zero message %+v", msg2)
	}
}

// TestRenderSessionArtifacts_Cap_AppendsMore asserts the block caps at
// sessionArtifactsCap rows and appends an explicit "+K more" line on
// overflow — never a silent drop (AC-6).
func TestRenderSessionArtifacts_Cap_AppendsMore(t *testing.T) {
	const total = sessionArtifactsCap + 5
	entries := make([]planner.ArtifactManifestEntry, 0, total)
	for i := range total {
		entries = append(entries, planner.ArtifactManifestEntry{
			Ref:        "ref-" + string(rune('A'+i%26)) + strings.Repeat("x", i),
			Filename:   "f.txt",
			MIME:       "text/plain",
			SizeBytes:  int64(i),
			Provenance: "user_upload",
		})
	}
	msg, ok := renderSessionArtifacts(entries)
	if !ok {
		t.Fatal("renderSessionArtifacts: ok=false")
	}
	body := msgText(t, msg)

	// Count the rendered list rows by isolating the
	// <session_artifacts_list> block (the rule lines also start with
	// "- ", so a whole-body count would over-count).
	start := strings.Index(body, "<session_artifacts_list>")
	end := strings.Index(body, "</session_artifacts_list>")
	if start < 0 || end < 0 || end < start {
		t.Fatalf("malformed list block:\n%s", body)
	}
	listBody := body[start+len("<session_artifacts_list>") : end]
	gotRows := 0
	for _, line := range strings.Split(strings.Trim(listBody, "\n"), "\n") {
		if strings.HasPrefix(line, "- ") {
			gotRows++
		}
	}
	if gotRows != sessionArtifactsCap {
		t.Errorf("rendered rows = %d, want cap %d", gotRows, sessionArtifactsCap)
	}
	// The overflow line names the exact remainder (AC-6).
	if !strings.Contains(body, "+5 more (use artifact_fetch by ref)") {
		t.Errorf("missing '+5 more' overflow line:\n%s", body)
	}
}

// TestRenderSessionArtifacts_BlankFields_Defaulted asserts a row with a
// blank MIME / provenance / filename still renders sensibly (no panic, no
// empty parens) rather than fabricating data.
func TestRenderSessionArtifacts_BlankFields_Defaulted(t *testing.T) {
	entries := []planner.ArtifactManifestEntry{
		{Ref: "default_zzz", SizeBytes: 10},
	}
	msg, ok := renderSessionArtifacts(entries)
	if !ok {
		t.Fatal("renderSessionArtifacts: ok=false")
	}
	body := msgText(t, msg)
	if !strings.Contains(body, "default_zzz") {
		t.Errorf("missing ref:\n%s", body)
	}
	if !strings.Contains(body, "application/octet-stream") {
		t.Errorf("blank MIME not defaulted to octet-stream:\n%s", body)
	}
	if !strings.Contains(body, "unknown") {
		t.Errorf("blank provenance not defaulted to unknown:\n%s", body)
	}
}

// TestRenderInjectionMessages_SessionArtifacts_LastBlock asserts the
// session-artifact block flows through renderInjectionMessages and lands
// AFTER the memory / skills blocks (the documented stable ordering),
// while an empty manifest contributes nothing.
func TestRenderInjectionMessages_SessionArtifacts_LastBlock(t *testing.T) {
	rc := planner.RunContext{
		SkillsContext: []any{map[string]any{"name": "billing"}},
		SessionArtifacts: []planner.ArtifactManifestEntry{
			{Ref: "default_abc", Filename: "x.txt", MIME: "text/plain", SizeBytes: 1, Provenance: "user_upload"},
		},
	}
	msgs, err := renderInjectionMessages(rc)
	if err != nil {
		t.Fatalf("renderInjectionMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d injection messages, want 2 (skills + artifacts)", len(msgs))
	}
	if !strings.Contains(msgText(t, msgs[0]), "<skills_context>") {
		t.Errorf("first injection block is not skills_context")
	}
	if !strings.Contains(msgText(t, msgs[1]), "<session_artifacts>") {
		t.Errorf("session_artifacts is not the last injection block")
	}

	// Empty manifest → no artifact block.
	rcEmpty := planner.RunContext{SkillsContext: []any{map[string]any{"name": "billing"}}}
	msgsEmpty, err := renderInjectionMessages(rcEmpty)
	if err != nil {
		t.Fatalf("renderInjectionMessages(empty): %v", err)
	}
	for _, m := range msgsEmpty {
		if strings.Contains(msgText(t, m), "<session_artifacts>") {
			t.Errorf("empty manifest still rendered a <session_artifacts> block")
		}
	}
}
