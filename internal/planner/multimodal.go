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
// ContentPart. Since Phase 84b (D-189) the materializer is a policy
// CONSUMER, not the policy author: a non-zero `a.Disposition` (the
// resolved attachment disposition — caller hint > agent policy >
// runtime default) selects the branch; the historical per-MIME switch
// survives verbatim as the zero-disposition fallback (see
// [materializeDefault]) so consumers that never set the field are
// byte-for-byte unchanged.
func materializeOne(a InputArtifactView, catalog ToolCatalogView) llm.ContentPart {
	switch {
	case a.Disposition.IsZero():
		return materializeDefault(a, catalog)
	case a.Disposition == DispositionInline:
		// The sub-threshold image fast path. Missing bytes fall back
		// to the by-reference rendering — the same degradation the
		// pre-84b dispatch applied to a bytes-less image.
		if strings.HasPrefix(a.MIME, "image/") && len(a.Bytes) > 0 {
			return imagePartFromBytes(a)
		}
		return refPart(a, catalog)
	case a.Disposition == DispositionProviderNative:
		return providerNativePart(a, catalog)
	default:
		if tool, ok := a.Disposition.ToolName(); ok {
			return stubPartForcedTool(a, tool)
		}
		// DispositionRef — plus the defensive landing for a
		// non-grammar value that bypassed [EffectiveDisposition]:
		// the `ArtifactStub` universal degradation (RFC §6.5),
		// never a silent drop.
		return refPart(a, catalog)
	}
}

// providerNativePart renders the `provider_native` disposition: a
// typed part carrying the canonical `ArtifactStub` reference plus the
// `ProviderNative` flag the LLM driver keys on. The driver uploads
// the artifact's bytes to the provider's file surface inside
// `Complete` and rewrites the part to the returned `file_id`; a
// provider without upload support for the modality keeps the stub —
// the universal degradation (RFC §6.5). The stub keeps its
// `Fetch.Tool` hint so the degraded rendering stays actionable.
//
// Modality dispatch: `image/*` → ImagePart, `audio/*` → AudioPart,
// everything else (PDF, video, documents) → FilePart with
// `DocumentType` set for structured documents.
func providerNativePart(a InputArtifactView, catalog ToolCatalogView) llm.ContentPart {
	switch {
	case strings.HasPrefix(a.MIME, "image/"):
		return llm.ContentPart{
			Type: llm.PartImage,
			Image: &llm.ImagePart{
				Artifact:       artifactStubFor(a, catalog),
				MIME:           a.MIME,
				ProviderNative: true,
			},
		}
	case strings.HasPrefix(a.MIME, "audio/"):
		return llm.ContentPart{
			Type: llm.PartAudio,
			Audio: &llm.AudioPart{
				Artifact:       artifactStubFor(a, catalog),
				MIME:           a.MIME,
				ProviderNative: true,
			},
		}
	default:
		return llm.ContentPart{
			Type: llm.PartFile,
			File: &llm.FilePart{
				Artifact:       artifactStubFor(a, catalog),
				MIME:           a.MIME,
				Filename:       a.Filename,
				ProviderNative: true,
				DocumentType:   documentTypeFor(a.MIME),
			},
		}
	}
}

// documentTypeFor derives the short `DocumentType` token for
// structured documents — the MIME subtype for `application/*` and
// `text/*` content (`application/pdf` → "pdf", `text/csv` → "csv").
// Empty for everything else (video and other non-document MIMEs the
// FilePart carries without a document hint).
func documentTypeFor(mime string) string {
	family, sub, found := strings.Cut(mime, "/")
	if !found || sub == "" {
		return ""
	}
	if family != "application" && family != "text" {
		return ""
	}
	// Trim media-type parameters (`text/csv; charset=utf-8` → "csv");
	// the subtype token is otherwise passed through verbatim.
	if i := strings.IndexByte(sub, ';'); i >= 0 {
		sub = strings.TrimSpace(sub[:i])
	}
	return sub
}

// materializeDefault is the pre-84b per-MIME dispatch, kept verbatim
// as the zero-disposition default map (the default-parity golden test
// pins it byte-for-byte). The four MIME families that today's
// vision-capable providers actually handle in-context (image, PDF,
// audio) get typed parts; the rest fall through to the catch-all
// ArtifactStub text block.
func materializeDefault(a InputArtifactView, catalog ToolCatalogView) llm.ContentPart {
	switch {
	case strings.HasPrefix(a.MIME, "image/") && len(a.Bytes) > 0:
		return imagePartFromBytes(a)
	default:
		return refPart(a, catalog)
	}
}

// refPart renders the by-reference (`ref`) disposition: the typed
// `FilePart` / `AudioPart` stub carriers for the MIME families the
// bifrost driver translates natively for capable providers, and the
// catch-all `ArtifactStub` text block for everything else (including
// images explicitly declared `ref`, and images whose inline bytes
// went missing).
func refPart(a InputArtifactView, catalog ToolCatalogView) llm.ContentPart {
	switch {
	case a.MIME == "application/pdf":
		return filePartFromRef(a, catalog)
	case strings.HasPrefix(a.MIME, "audio/"):
		return audioPartFromRef(a, catalog)
	default:
		return stubPartFromRef(a, catalog)
	}
}

// stubPartForcedTool renders the `tool:<name>` disposition: the
// catch-all `ArtifactStub` text block with `Fetch.Tool` FORCED to the
// declared tool, overriding the catalog's `HandlesMIME` discovery.
// Unknown names were already degraded to `ref` by
// [EffectiveDisposition] when the caller supplied a catalog view; a
// name that reaches this branch is emitted verbatim (the stub's fetch
// pointer is an explicit instruction to the LLM).
func stubPartForcedTool(a InputArtifactView, tool string) llm.ContentPart {
	stub := &llm.ArtifactStub{
		Ref:       a.ID,
		MIME:      a.MIME,
		SizeBytes: a.SizeBytes,
		Summary:   stubSummary(a),
		Fetch:     &llm.StubFetch{Tool: tool, ID: a.ID},
	}
	return llm.ContentPart{
		Type: llm.PartText,
		Text: stubAsText(stub, a.Filename),
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
