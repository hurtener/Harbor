// Multimodal first-turn materialization (Round-7 F11 / D-166).
//
// The Playground composer's chat-attach control uploads files via
// `artifacts.put` and the operator clicks Send. The runtime carries
// the artifact IDs onto the task (`tasks.Task.InputArtifactIDs`); the
// run loop pre-resolves each entry into a `planner.InputArtifactView`
// on the `RunContext` and hands the planner a synchronous, ready-to-
// render slice. `MaterializeInputContent` is the per-MIME dispatcher
// the planner calls when assembling its first-turn user message:
//
//   - `image/*` → `llm.ImagePart{DataURL: data:<mime>;base64,<bytes>}`
//     (Path 1 from D-166: bytes inline at the LLM edge so vision-
//     capable providers actually see the image). The base64 encoding
//     is bounded by the operator's upload (the runtime ArtifactStore
//     itself caps each artifact size; the materializer is a
//     pass-through).
//   - `application/pdf` → `llm.FilePart{Artifact: &llm.ArtifactStub{...}}`
//     by reference. Providers that support PDF native (Anthropic
//     today) consume the ref via the bifrost driver's existing
//     translatation; providers without PDF support see the canonical
//     `ArtifactStub` JSON description (graceful degradation, RFC §6.5).
//   - `audio/*` → `llm.AudioPart{Artifact: &llm.ArtifactStub{...}}`
//     by reference. Same graceful-degradation rule as PDF.
//   - everything else → bare `ArtifactStub` text block on the user
//     message — the LLM sees the ref + MIME + size + (optional)
//     `Fetch.Tool` pointer and routes to a matching tool via the
//     catalog. The operator gets multimodal-as-routing-hint for free
//     (e.g. "I uploaded a CSV, please summarise it").
//
// The optional `Fetch.Tool` pointer on every emitted `ArtifactStub`
// is populated from the supplied `ToolCatalogView`: the first tool
// whose `HandlesMIME` matches the artifact's MIME wins. Operators
// register an audio.transcribe tool with `HandlesMIME: ["audio/*"]`
// once and the LLM gets an explicit "use this tool for this ref"
// hint — no LLM-side guesswork.
package planner

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/llm"
)

// MaterializeInputContent assembles the first-turn LLM `Content` from
// the user goal text plus a slice of pre-resolved input artifacts.
// Returns a text-only `Content` when `artifacts` is empty (the
// existing text-only path is unchanged); otherwise returns a
// `Content{Parts: [...]}` with one PartText for the goal and one part
// per artifact, dispatched by MIME.
//
// `catalog` is consulted for the `Fetch.Tool` annotation on emitted
// `ArtifactStub`s; pass a nil-or-empty view if MIME routing should be
// left to the LLM's catalog discovery.
//
// Pure function — no I/O, no goroutines, no state. The Bytes slot on
// each InputArtifactView is consumed read-only; the materializer
// never mutates the slice or its contents.
func MaterializeInputContent(goal string, artifacts []InputArtifactView, catalog ToolCatalogView) llm.Content {
	if len(artifacts) == 0 {
		s := goal
		return llm.Content{Text: &s}
	}

	parts := make([]llm.ContentPart, 0, len(artifacts)+1)
	if goal != "" {
		parts = append(parts, llm.ContentPart{Type: llm.PartText, Text: goal})
	}
	for _, a := range artifacts {
		parts = append(parts, materializeOne(a, catalog))
	}
	return llm.Content{Parts: parts}
}

// materializeOne dispatches a single InputArtifactView to its
// MIME-specific ContentPart. The dispatch is intentionally narrow —
// the four MIME families that today's vision-capable providers
// actually handle in-context (image, PDF, audio) get typed parts; the
// rest fall through to the catch-all ArtifactStub text block.
func materializeOne(a InputArtifactView, catalog ToolCatalogView) llm.ContentPart {
	switch {
	case strings.HasPrefix(a.MIME, "image/") && len(a.Bytes) > 0:
		return imagePartFromBytes(a)
	case a.MIME == "application/pdf":
		return filePartFromRef(a, catalog)
	case strings.HasPrefix(a.MIME, "audio/"):
		return audioPartFromRef(a, catalog)
	default:
		return stubPartFromRef(a, catalog)
	}
}

// imagePartFromBytes constructs `llm.ImagePart` with `DataURL` inline.
// Path 1 from D-166 — operator-uploaded inputs reach the provider
// inline so vision-capable models actually see the image. The bytes
// were pre-fetched by the run loop; this function is byte-level
// passthrough into base64.
func imagePartFromBytes(a InputArtifactView) llm.ContentPart {
	encoded := base64.StdEncoding.EncodeToString(a.Bytes)
	dataURL := fmt.Sprintf("data:%s;base64,%s", a.MIME, encoded)
	return llm.ContentPart{
		Type: llm.PartImage,
		Image: &llm.ImagePart{
			DataURL: dataURL,
			MIME:    a.MIME,
		},
	}
}

// filePartFromRef constructs `llm.FilePart` with `Artifact` set to a
// canonical `ArtifactStub`. Providers with native file support
// (Anthropic Claude with PDFs) consume the ref via the bifrost
// driver's existing artifact-stub translation; providers without
// receive the stub JSON as a graceful-degradation text description.
func filePartFromRef(a InputArtifactView, catalog ToolCatalogView) llm.ContentPart {
	return llm.ContentPart{
		Type: llm.PartFile,
		File: &llm.FilePart{
			Artifact: artifactStubFor(a, catalog),
			MIME:     a.MIME,
			Filename: a.Filename,
		},
	}
}

// audioPartFromRef constructs `llm.AudioPart` with `Artifact` set —
// same graceful-degradation rule as file/pdf inputs.
func audioPartFromRef(a InputArtifactView, catalog ToolCatalogView) llm.ContentPart {
	return llm.ContentPart{
		Type: llm.PartAudio,
		Audio: &llm.AudioPart{
			Artifact: artifactStubFor(a, catalog),
			MIME:     a.MIME,
		},
	}
}

// stubPartFromRef is the catch-all for MIMEs the dispatcher doesn't
// recognise. The LLM sees an `ArtifactStub` text block (per the
// bifrost driver's existing `translateImagePart` artifact branch,
// translateAudioPart, etc.) wrapped in a `PartText` — the
// `ArtifactStub.MarshalJSON` shape is the canonical reference
// description (RFC §6.5 / D-022). The LLM routes to a matching tool
// via the catalog, optionally hinted by `Fetch.Tool` when the
// catalog advertises a `HandlesMIME` match.
func stubPartFromRef(a InputArtifactView, catalog ToolCatalogView) llm.ContentPart {
	stub := artifactStubFor(a, catalog)
	// Emit the stub as a text part — non-image / non-pdf / non-audio
	// MIMEs aren't handled by the provider's multimodal sum-type
	// natively, so we render the stub JSON as the user's text turn
	// for the LLM to read and act on.
	text := stubAsText(stub, a.Filename)
	return llm.ContentPart{
		Type: llm.PartText,
		Text: text,
	}
}

// artifactStubFor builds the canonical `llm.ArtifactStub` from the
// pre-resolved view. The `Summary` is operator-friendly text the LLM
// reads; `Fetch.Tool` is populated when the catalog advertises a
// `HandlesMIME` match for the artifact's MIME, giving the LLM an
// explicit pointer to the right tool. Empty `Fetch` falls back to
// catalog discovery — the LLM still finds the binding via the
// catalog by description.
func artifactStubFor(a InputArtifactView, catalog ToolCatalogView) *llm.ArtifactStub {
	stub := &llm.ArtifactStub{
		Ref:       a.ID,
		MIME:      a.MIME,
		SizeBytes: a.SizeBytes,
		Summary:   stubSummary(a),
	}
	if catalog != nil {
		if toolName := firstHandlerForMIME(catalog, a.MIME); toolName != "" {
			stub.Fetch = &llm.StubFetch{Tool: toolName, ID: a.ID}
		}
	}
	return stub
}

// firstHandlerForMIME walks the catalog in natural order and returns
// the first tool whose HandlesMIME matches `mime`. Returns the empty
// string when no tool advertises the MIME — the LLM then finds the
// binding through catalog descriptions, the V1 fallback. The walk is
// deterministic because the catalog's `List` order is stable
// (registration order, per `internal/tools/catalog.go`).
func firstHandlerForMIME(catalog ToolCatalogView, mime string) string {
	if mime == "" {
		return ""
	}
	for _, t := range catalog.List() {
		if t.MatchesMIME(mime) {
			return t.Name
		}
	}
	return ""
}

// stubSummary builds the operator-friendly description embedded on
// every emitted `ArtifactStub`. Format: `User-uploaded <type> input
// (<size>) [filename]` where the bracketed segment elides when no
// filename is supplied. Kept short — the stub JSON itself carries
// the precise MIME / size / ref the LLM reads programmatically; the
// summary is for human-readable trace.
func stubSummary(a InputArtifactView) string {
	var b strings.Builder
	b.WriteString("User-uploaded ")
	if a.MIME != "" {
		b.WriteString(a.MIME)
	} else {
		b.WriteString("artifact")
	}
	b.WriteString(" input")
	if a.SizeBytes > 0 {
		fmt.Fprintf(&b, " (%d bytes)", a.SizeBytes)
	}
	if a.Filename != "" {
		b.WriteString(" [")
		b.WriteString(a.Filename)
		b.WriteString("]")
	}
	return b.String()
}

// stubAsText renders an `ArtifactStub` as a single user-visible text
// part for the catch-all dispatcher branch. The output sandwiches the
// canonical stub JSON (the same shape `ArtifactStub.MarshalJSON`
// emits) between an "Attachment:" header and a blank-line separator
// so the LLM can recognise it as a reference, not free text.
func stubAsText(stub *llm.ArtifactStub, filename string) string {
	stubBytes, err := stub.MarshalJSON()
	if err != nil {
		// MarshalJSON on ArtifactStub is well-behaved (the canonical
		// shape is byte-stable per D-022). Fail loudly enough to
		// surface in tests but degrade gracefully at runtime — the
		// LLM still sees the ref + filename and can route from there.
		return fmt.Sprintf("Attachment (ref=%s, mime=%s): %s", stub.Ref, stub.MIME, filename)
	}
	var b strings.Builder
	b.WriteString("Attachment")
	if filename != "" {
		b.WriteString(" — ")
		b.WriteString(filename)
	}
	b.WriteString(":\n")
	b.Write(stubBytes)
	return b.String()
}
