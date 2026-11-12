package builtin

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/tools"
)

type noteBus struct {
	mu        sync.Mutex
	published []events.Event
}

func (b *noteBus) Publish(_ context.Context, ev events.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.published = append(b.published, ev)
	return nil
}
func (b *noteBus) Subscribe(context.Context, events.Filter) (events.Subscription, error) {
	return nil, nil
}
func (b *noteBus) Close(context.Context) error { return nil }

func putToolResult(t *testing.T, store artifacts.ArtifactStore, session, tool, filename, mime, body string, at time.Time) artifacts.ArtifactRef {
	t.Helper()
	ref, err := store.PutText(t.Context(), artifacts.ArtifactScope{TenantID: "t", UserID: "u", SessionID: session, TaskID: "r"}, body,
		artifacts.PutOpts{MimeType: mime, Filename: filename, Source: map[string]any{"source": "tool", "tool": tool, "created_at": at}})
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func searchDoc() string {
	var b strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&b, "row %02d value=%d\n", i, i*10)
	}
	b.WriteString("Revenue grew in Q3\nrevenue table follows\n")
	return b.String()
}

func TestArtifactSearch_LiteralRegexCaseAndContext(t *testing.T) {
	store := artifactFetchTestStore(t)
	ctx := artifactFetchTestCtx(t, "t", "u", "s", "r")
	ref := putToolResult(t, store, "s", "web_search", "result.txt", "text/plain; charset=utf-8", searchDoc(), time.Now())

	out, err := artifactSearch(ctx, store, ArtifactSearchArgs{Ref: ref.ID, Pattern: "Revenue"})
	if err != nil || out.Error != "" || len(out.Matches) != 1 || out.Matches[0].Line != 51 {
		t.Fatalf("literal search = %+v %v", out, err)
	}
	m := out.Matches[0]
	body := searchDoc()
	if body[m.ByteOffset:m.ByteOffset+int64(len("Revenue"))] != "Revenue" {
		t.Fatalf("byte_offset %d does not point at the match line", m.ByteOffset)
	}
	if len(m.Before) != 2 || m.Before[1] != "row 50 value=500" || len(m.After) != 1 || m.After[0] != "revenue table follows" {
		t.Fatalf("context = before %q after %q", m.Before, m.After)
	}
	out, _ = artifactSearch(ctx, store, ArtifactSearchArgs{Ref: ref.ID, Pattern: "revenue", CaseInsensitive: true})
	if len(out.Matches) != 2 {
		t.Fatalf("case-insensitive matches = %d", len(out.Matches))
	}
	zero := 0
	out, _ = artifactSearch(ctx, store, ArtifactSearchArgs{Ref: ref.ID, Pattern: `value=4\d0$`, Regex: true, ContextLines: &zero})
	if len(out.Matches) != 10 || out.Matches[0].Line != 40 || len(out.Matches[0].Before) != 0 {
		t.Fatalf("regex matches = %+v", out.Matches)
	}
	out, _ = artifactSearch(ctx, store, ArtifactSearchArgs{Ref: ref.ID, Pattern: "(unclosed", Regex: true})
	if !strings.Contains(out.Error, "invalid RE2 pattern") {
		t.Fatalf("invalid regex error = %q", out.Error)
	}
	out, _ = artifactSearch(ctx, store, ArtifactSearchArgs{Ref: ref.ID, Pattern: strings.Repeat("x", searchMaxPatternBytes+1)})
	if !strings.Contains(out.Error, "at most") {
		t.Fatalf("oversized pattern error = %q", out.Error)
	}
}

func TestArtifactSearch_TruncationContinuesFromNextOffset(t *testing.T) {
	store := artifactFetchTestStore(t)
	ctx := artifactFetchTestCtx(t, "t", "u", "s", "r")
	ref := putToolResult(t, store, "s", "web_search", "result.txt", "text/plain", searchDoc(), time.Now())
	first, _ := artifactSearch(ctx, store, ArtifactSearchArgs{Ref: ref.ID, Pattern: "row", MaxMatches: 30})
	if len(first.Matches) != 30 || !first.Truncated || first.NextOffset == 0 {
		t.Fatalf("first page = %d truncated=%v next=%d", len(first.Matches), first.Truncated, first.NextOffset)
	}
	second, _ := artifactSearch(ctx, store, ArtifactSearchArgs{Ref: ref.ID, Pattern: "row", MaxMatches: 30, Offset: first.NextOffset})
	if len(second.Matches) != 20 || second.Truncated || second.Matches[0].Line != 31 {
		t.Fatalf("continuation = %d truncated=%v first line %d", len(second.Matches), second.Truncated, second.Matches[0].Line)
	}
}

func TestArtifactSearch_RefusesBinaryAndOtherSessions(t *testing.T) {
	store := artifactFetchTestStore(t)
	ctx := artifactFetchTestCtx(t, "t", "u", "s", "r")
	pdf := putToolResult(t, store, "s", "fetch", "doc.pdf", "application/pdf", "%PDF-1.7 Revenue", time.Now())
	out, err := artifactSearch(ctx, store, ArtifactSearchArgs{Ref: pdf.ID, Pattern: "Revenue"})
	if err != nil || !strings.Contains(out.Error, "not text") || !strings.Contains(out.Error, "artifact-reference parameter") {
		t.Fatalf("binary artifact = %+v %v", out, err)
	}
	other := putToolResult(t, store, "other-session", "web_search", "r.txt", "text/plain", "Revenue", time.Now())
	out, _ = artifactSearch(ctx, store, ArtifactSearchArgs{Ref: other.ID, Pattern: "Revenue"})
	if !strings.Contains(out.Error, "not found in this session's scope") || len(out.Matches) != 0 {
		t.Fatalf("cross-session search = %+v", out)
	}
	if _, err := artifactSearch(context.Background(), store, ArtifactSearchArgs{Ref: other.ID, Pattern: "x"}); err == nil {
		t.Fatal("missing identity accepted")
	}
}

func TestArtifactList_FiltersPaginationAndNotes(t *testing.T) {
	store := artifactFetchTestStore(t)
	ctx := artifactFetchTestCtx(t, "t", "u", "s", "r")
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i := range 25 {
		putToolResult(t, store, "s", []string{"web_search", "knowledge_read"}[i%2], fmt.Sprintf("tool-result-%02d.json", i), "application/json", fmt.Sprint(`{"i":`, i, `}`), base.Add(time.Duration(i)*time.Minute))
	}
	upload, err := store.PutText(t.Context(), artifacts.ArtifactScope{TenantID: "t", UserID: "u", SessionID: "s"}, "brief",
		artifacts.PutOpts{Filename: "Brief.docx", Source: map[string]any{"source": "user_upload", "created_at": base.Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	putToolResult(t, store, "other-session", "web_search", "foreign.json", "application/json", "{}", base)

	page, err := artifactList(ctx, store, ArtifactListArgs{})
	if err != nil || page.Total != 26 || len(page.Artifacts) != 20 || page.NextCursor != "20" || page.Artifacts[0].Ref != upload.ID {
		t.Fatalf("first page total=%d len=%d next=%q first=%+v err=%v", page.Total, len(page.Artifacts), page.NextCursor, page.Artifacts[0], err)
	}
	rest, _ := artifactList(ctx, store, ArtifactListArgs{Cursor: page.NextCursor})
	if len(rest.Artifacts) != 6 || rest.NextCursor != "" {
		t.Fatalf("second page len=%d next=%q", len(rest.Artifacts), rest.NextCursor)
	}
	tools, _ := artifactList(ctx, store, ArtifactListArgs{Source: "tool", Tool: "web_search", Limit: 50})
	if tools.Total != 13 {
		t.Fatalf("tool filter total = %d", tools.Total)
	}
	for _, a := range tools.Artifacts {
		if a.Source != "tool" || a.Tool != "web_search" || a.Filename == "foreign.json" {
			t.Fatalf("filter leaked %+v", a)
		}
	}
	uploads, _ := artifactList(ctx, store, ArtifactListArgs{Source: "user_upload", Filename: "brief"})
	if uploads.Total != 1 || uploads.Artifacts[0].Ref != upload.ID {
		t.Fatalf("upload filter = %+v", uploads)
	}
	if _, _, err := store.SetNote(t.Context(), artifacts.ArtifactScope{TenantID: "t", UserID: "u", SessionID: "s"}, upload.ID, "client brief, v2"); err != nil {
		t.Fatal(err)
	}
	noted, _ := artifactList(ctx, store, ArtifactListArgs{Source: "user_upload"})
	if noted.Artifacts[0].Note != "client brief, v2" {
		t.Fatalf("note missing from listing: %+v", noted.Artifacts[0])
	}
	bad, _ := artifactList(ctx, store, ArtifactListArgs{Source: "bogus"})
	if bad.Error == "" {
		t.Fatal("invalid source accepted")
	}
}

func TestArtifactNote_SetReplaceClearAndRecord(t *testing.T) {
	store := artifactFetchTestStore(t)
	ctx := artifactFetchTestCtx(t, "t", "u", "s", "r")
	ref := putToolResult(t, store, "s", "web_search", "r.json", "application/json", `{"a":1}`, time.Now())
	bus := &noteBus{}
	out, err := artifactNote(ctx, store, bus, ArtifactNoteArgs{Ref: ref.ID, Note: "pricing table for vendor A"})
	if err != nil || out.Error != "" || out.Note != "pricing table for vendor A" {
		t.Fatalf("set = %+v %v", out, err)
	}
	out, _ = artifactNote(ctx, store, bus, ArtifactNoteArgs{Ref: ref.ID, Note: "replaced"})
	if out.Note != "replaced" {
		t.Fatalf("replace = %+v", out)
	}
	body, _, _ := store.Get(t.Context(), artifacts.ArtifactScope{TenantID: "t", UserID: "u", SessionID: "s"}, ref.ID)
	if string(body) != `{"a":1}` {
		t.Fatal("note changed the artifact bytes")
	}
	out, _ = artifactNote(ctx, store, bus, ArtifactNoteArgs{Ref: ref.ID, Note: ""})
	if out.Note != "" || out.Error != "" {
		t.Fatalf("clear = %+v", out)
	}
	if len(bus.published) != 3 {
		t.Fatalf("events = %d, want one per change", len(bus.published))
	}
	last := bus.published[2].Payload.(artifacts.ArtifactNoteSetPayload)
	if bus.published[0].Type != artifacts.EventTypeArtifactNoteSet || !last.Cleared || last.ArtifactID != ref.ID || bus.published[0].Identity.SessionID != "s" {
		t.Fatalf("event = %+v", bus.published)
	}
	out, _ = artifactNote(ctx, store, bus, ArtifactNoteArgs{Ref: ref.ID, Note: strings.Repeat("n", artifacts.MaxNoteBytes+1)})
	if !strings.Contains(out.Error, "at most") || len(bus.published) != 3 {
		t.Fatalf("oversized note = %+v", out)
	}
	other := putToolResult(t, store, "other-session", "web_search", "x.json", "application/json", `{}`, time.Now())
	out, _ = artifactNote(ctx, store, bus, ArtifactNoteArgs{Ref: other.ID, Note: "nope"})
	if !strings.Contains(out.Error, "not found") {
		t.Fatalf("cross-session note = %+v", out)
	}
}

func TestArtifactTools_RegistrationRequiresWiring(t *testing.T) {
	if err := RegisterWith(RegistryContext{Catalog: tools.NewCatalog()}, []string{"artifact_note"}); err == nil {
		t.Fatal("artifact_note registered without a bus")
	}
	cat := tools.NewCatalog()
	if err := RegisterWith(RegistryContext{Catalog: cat, Bus: &noteBus{}}, []string{"artifact_search", "artifact_list", "artifact_note"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"artifact_search", "artifact_list", "artifact_note"} {
		if _, ok := cat.Resolve(name); !ok {
			t.Fatalf("%s not registered", name)
		}
	}
}

// One shared store serves 128 concurrent searches, listings and notes across
// sessions without cross-talk.
func TestArtifactTools_ConcurrentReuseIsolated(t *testing.T) {
	store := artifactFetchTestStore(t)
	bus := &noteBus{}
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session := fmt.Sprint("s-", i)
			ctx := artifactFetchTestCtx(t, "t", "u", session, "r")
			ref := putToolResult(t, store, session, "web_search", "r.txt", "text/plain", fmt.Sprintf("marker-%d\nother\n", i), time.Now())
			found, _ := artifactSearch(ctx, store, ArtifactSearchArgs{Ref: ref.ID, Pattern: "marker-"})
			if len(found.Matches) != 1 || found.Matches[0].Text != fmt.Sprintf("marker-%d", i) {
				t.Errorf("session %d search = %+v", i, found)
			}
			if out, _ := artifactNote(ctx, store, bus, ArtifactNoteArgs{Ref: ref.ID, Note: session}); out.Note != session {
				t.Errorf("session %d note = %+v", i, out)
			}
			listed, _ := artifactList(ctx, store, ArtifactListArgs{})
			if listed.Total != 1 || listed.Artifacts[0].Note != session {
				t.Errorf("session %d listing = %+v", i, listed)
			}
		}()
	}
	wg.Wait()
}
