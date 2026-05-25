package planner_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
)

// stubCatalogView is a minimal ToolCatalogView fixture used by the
// materializer tests. It does NOT pull in the real catalog driver —
// the materializer's contract is a tiny read-only view.
type stubCatalogView struct {
	listed []tools.Tool
}

func (c stubCatalogView) Resolve(name string) (tools.Tool, bool) {
	for _, t := range c.listed {
		if t.Name == name {
			return t, true
		}
	}
	return tools.Tool{}, false
}

func (c stubCatalogView) List() []tools.Tool { return c.listed }

// TestMaterializeInputContent_NoArtifacts pins the text-only baseline.
// When no input artifacts attach, the function returns the same
// `Content{Text: &goal}` shape the planner used pre-F11; the existing
// text-only path is unchanged.
func TestMaterializeInputContent_NoArtifacts(t *testing.T) {
	c := planner.MaterializeInputContent("hello world", nil, nil)
	if c.Text == nil {
		t.Fatalf("Text is nil; want non-nil text-only Content")
	}
	if *c.Text != "hello world" {
		t.Fatalf("Text = %q, want %q", *c.Text, "hello world")
	}
	if c.Parts != nil {
		t.Fatalf("Parts must be nil for the text-only path, got %+v", c.Parts)
	}
}

// TestMaterializeInputContent_ImageInlines confirms the Path 1
// behaviour from D-166: an `image/*` MIME with pre-fetched bytes
// materializes as `llm.ImagePart` with a base64 `DataURL` the bifrost
// driver's existing translation forwards as a native image block.
func TestMaterializeInputContent_ImageInlines(t *testing.T) {
	pixel := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A} // PNG magic
	c := planner.MaterializeInputContent(
		"describe this image",
		[]planner.InputArtifactView{
			{ID: "art_pixel", MIME: "image/png", SizeBytes: int64(len(pixel)), Bytes: pixel, Filename: "pixel.png"},
		},
		nil,
	)
	if c.Text != nil {
		t.Fatalf("Text must be nil for multimodal content; got %q", *c.Text)
	}
	if len(c.Parts) != 2 {
		t.Fatalf("Parts = %d, want 2 (text + image)", len(c.Parts))
	}
	if c.Parts[0].Type != llm.PartText || c.Parts[0].Text != "describe this image" {
		t.Errorf("first part not the goal text: %+v", c.Parts[0])
	}
	if c.Parts[1].Type != llm.PartImage {
		t.Fatalf("second part type = %q, want %q", c.Parts[1].Type, llm.PartImage)
	}
	img := c.Parts[1].Image
	if img == nil {
		t.Fatal("Image is nil")
	}
	if img.MIME != "image/png" {
		t.Errorf("MIME = %q, want image/png", img.MIME)
	}
	if !strings.HasPrefix(img.DataURL, "data:image/png;base64,") {
		t.Errorf("DataURL missing canonical prefix: %s", img.DataURL[:min(40, len(img.DataURL))])
	}
	// Confirm the bytes round-trip — bifrost-side translation will
	// decode the same payload back out of the DataURL.
	want := base64.StdEncoding.EncodeToString(pixel)
	got := strings.TrimPrefix(img.DataURL, "data:image/png;base64,")
	if got != want {
		t.Errorf("base64 mismatch:\n got %q\nwant %q", got, want)
	}
	if img.Artifact != nil {
		t.Errorf("image-inline path must NOT set Artifact ref; got %+v", img.Artifact)
	}
}

// TestMaterializeInputContent_ImageMissingBytesFallsBackToRef — when
// the run loop did not pre-fetch the bytes (e.g. the artifact store
// rejected the read), the materializer degrades to the catch-all
// stub path rather than crashing on an empty DataURL. Defensive: a
// nil/empty Bytes slot must never produce a malformed ImagePart.
func TestMaterializeInputContent_ImageMissingBytesFallsBackToRef(t *testing.T) {
	c := planner.MaterializeInputContent(
		"describe",
		[]planner.InputArtifactView{
			{ID: "art_missing", MIME: "image/png", SizeBytes: 8, Bytes: nil},
		},
		nil,
	)
	if len(c.Parts) != 2 {
		t.Fatalf("Parts = %d, want 2", len(c.Parts))
	}
	if c.Parts[1].Type != llm.PartText {
		t.Fatalf("missing-bytes image must fall back to text-stub; got %q", c.Parts[1].Type)
	}
	if !strings.Contains(c.Parts[1].Text, "art_missing") {
		t.Errorf("fallback text must carry the artifact ref; got %q", c.Parts[1].Text)
	}
}

// TestMaterializeInputContent_PDFIsFilePart confirms PDFs route to
// `llm.FilePart{Artifact}` so providers with native PDF input
// (Anthropic) consume them as a typed block; providers without get
// the canonical ArtifactStub-JSON description.
func TestMaterializeInputContent_PDFIsFilePart(t *testing.T) {
	c := planner.MaterializeInputContent(
		"summarise this PDF",
		[]planner.InputArtifactView{
			{ID: "art_doc", MIME: "application/pdf", SizeBytes: 4096, Filename: "report.pdf"},
		},
		nil,
	)
	if len(c.Parts) != 2 || c.Parts[1].Type != llm.PartFile {
		t.Fatalf("expected PartFile for pdf; got Parts=%+v", c.Parts)
	}
	if c.Parts[1].File == nil || c.Parts[1].File.Artifact == nil {
		t.Fatal("PartFile.Artifact missing")
	}
	if c.Parts[1].File.Artifact.Ref != "art_doc" {
		t.Errorf("Ref = %q, want art_doc", c.Parts[1].File.Artifact.Ref)
	}
	if c.Parts[1].File.Filename != "report.pdf" {
		t.Errorf("Filename = %q, want report.pdf", c.Parts[1].File.Filename)
	}
}

// TestMaterializeInputContent_AudioIsAudioPart confirms audio MIMEs
// route to AudioPart{Artifact} — same graceful-degradation contract.
func TestMaterializeInputContent_AudioIsAudioPart(t *testing.T) {
	c := planner.MaterializeInputContent(
		"transcribe this",
		[]planner.InputArtifactView{
			{ID: "art_audio", MIME: "audio/wav", SizeBytes: 8192},
		},
		nil,
	)
	if len(c.Parts) != 2 || c.Parts[1].Type != llm.PartAudio {
		t.Fatalf("expected PartAudio for audio/wav; got Parts=%+v", c.Parts)
	}
	if c.Parts[1].Audio == nil || c.Parts[1].Audio.Artifact == nil {
		t.Fatal("PartAudio.Artifact missing")
	}
}

// TestMaterializeInputContent_UnknownMIMEIsStubText confirms the
// catch-all: a MIME the dispatcher doesn't recognise (text/csv,
// application/json, etc.) emits an ArtifactStub-as-text-part. The
// LLM reads the stub JSON and routes via the catalog.
func TestMaterializeInputContent_UnknownMIMEIsStubText(t *testing.T) {
	c := planner.MaterializeInputContent(
		"summarise the CSV",
		[]planner.InputArtifactView{
			{ID: "art_csv", MIME: "text/csv", SizeBytes: 2048, Filename: "data.csv"},
		},
		nil,
	)
	if len(c.Parts) != 2 || c.Parts[1].Type != llm.PartText {
		t.Fatalf("expected PartText for catch-all; got Parts=%+v", c.Parts)
	}
	stubText := c.Parts[1].Text
	if !strings.Contains(stubText, "art_csv") {
		t.Errorf("stub text missing artifact ref: %q", stubText)
	}
	if !strings.Contains(stubText, "text/csv") {
		t.Errorf("stub text missing MIME: %q", stubText)
	}
	if !strings.Contains(stubText, "data.csv") {
		t.Errorf("stub text missing filename: %q", stubText)
	}
}

// TestMaterializeInputContent_FetchToolPopulated confirms the
// `Fetch.Tool` annotation on emitted ArtifactStubs — when the catalog
// has a tool whose HandlesMIME covers the artifact's MIME, the stub
// carries an explicit pointer to that tool, giving the LLM a routing
// hint without forcing it to discover the binding from catalog
// descriptions.
func TestMaterializeInputContent_FetchToolPopulated(t *testing.T) {
	cat := stubCatalogView{
		listed: []tools.Tool{
			{Name: "tool.unrelated", HandlesMIME: []string{"video/*"}},
			{Name: "audio.transcribe", HandlesMIME: []string{"audio/*"}},
			{Name: "audio.other", HandlesMIME: []string{"audio/wav"}},
		},
	}
	c := planner.MaterializeInputContent(
		"transcribe",
		[]planner.InputArtifactView{
			{ID: "art_audio", MIME: "audio/wav", SizeBytes: 1024},
		},
		cat,
	)
	if len(c.Parts) != 2 {
		t.Fatalf("Parts = %d, want 2", len(c.Parts))
	}
	audio := c.Parts[1].Audio
	if audio == nil || audio.Artifact == nil {
		t.Fatal("audio.Artifact nil")
	}
	// First match wins — audio.transcribe lists `audio/*` and comes
	// before audio.other, so the wildcard match should be selected.
	if audio.Artifact.Fetch == nil || audio.Artifact.Fetch.Tool != "audio.transcribe" {
		t.Fatalf("Fetch = %+v, want Tool=audio.transcribe", audio.Artifact.Fetch)
	}
	if audio.Artifact.Fetch.ID != "art_audio" {
		t.Errorf("Fetch.ID = %q, want art_audio", audio.Artifact.Fetch.ID)
	}
}

// TestMaterializeInputContent_FetchToolNilWithoutCatalog — when the
// catalog is nil, no Fetch annotation is populated. The LLM still
// routes through the catalog by description (the fallback path);
// nil-catalog must NOT panic.
func TestMaterializeInputContent_FetchToolNilWithoutCatalog(t *testing.T) {
	c := planner.MaterializeInputContent(
		"transcribe",
		[]planner.InputArtifactView{
			{ID: "art_audio", MIME: "audio/wav", SizeBytes: 1024},
		},
		nil,
	)
	audio := c.Parts[1].Audio
	if audio.Artifact.Fetch != nil {
		t.Errorf("Fetch = %+v, want nil with nil catalog", audio.Artifact.Fetch)
	}
}

// TestMaterializeInputContent_MixedAttachments confirms the
// per-MIME dispatcher fans out correctly across heterogeneous
// attachments in a single turn: image + pdf + audio + csv all
// materialize to their respective parts on the same Content.
func TestMaterializeInputContent_MixedAttachments(t *testing.T) {
	c := planner.MaterializeInputContent(
		"process all of these",
		[]planner.InputArtifactView{
			{ID: "p", MIME: "image/png", Bytes: []byte{1, 2, 3}, SizeBytes: 3},
			{ID: "d", MIME: "application/pdf", SizeBytes: 100},
			{ID: "a", MIME: "audio/mp3", SizeBytes: 50},
			{ID: "c", MIME: "text/csv", SizeBytes: 20, Filename: "x.csv"},
		},
		nil,
	)
	wantTypes := []llm.PartType{llm.PartText, llm.PartImage, llm.PartFile, llm.PartAudio, llm.PartText}
	if len(c.Parts) != len(wantTypes) {
		t.Fatalf("Parts len = %d, want %d", len(c.Parts), len(wantTypes))
	}
	for i, want := range wantTypes {
		if c.Parts[i].Type != want {
			t.Errorf("Parts[%d].Type = %q, want %q", i, c.Parts[i].Type, want)
		}
	}
}

// TestMaterializeInputContent_EmptyGoalDropsTextPart — when the
// operator sends only an attachment with no message body, the goal-
// text part is elided (so the LLM sees just the attachment), not
// inserted as an empty `PartText`. A subtle UX hygiene rule: a
// "(no goal supplied)" placeholder belongs in `buildUserContent`,
// not in the multimodal materializer.
func TestMaterializeInputContent_EmptyGoalDropsTextPart(t *testing.T) {
	c := planner.MaterializeInputContent(
		"",
		[]planner.InputArtifactView{
			{ID: "p", MIME: "image/png", Bytes: []byte{1, 2, 3}, SizeBytes: 3},
		},
		nil,
	)
	if len(c.Parts) != 1 {
		t.Fatalf("Parts = %d, want 1 (goal elided)", len(c.Parts))
	}
	if c.Parts[0].Type != llm.PartImage {
		t.Errorf("only part should be the image; got %q", c.Parts[0].Type)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
