// Package-level: artifact_fetch is the LLM-facing recovery path for
// heavy-content artifacts. The runtime tool-executor materialises
// tool results above the heavy-output threshold to the
// artifact store and projects the LLM-facing observation as a small
// preview + a fetch hint; this builtin lets the LLM pull the full
// bytes of the referenced artifact when the preview doesn't carry
// what it needs.
//
// Identity is mandatory (CLAUDE.md §6 rule 9). Cross-tenant
// rejection is the artifact store's responsibility — the store resolves
// on the isolation triple `(tenant, user, session)` and an out-of-scope
// read returns found-false, which we surface as `{error}` (NOT exposed
// bytes). The builtin's test pins this with a deliberate
// cross-identity write + read.
//
// A sibling run's artifact IS reachable, and deliberately so: the
// session is the innermost isolation scope, so an artifact any run in
// this session produced is in scope for this one. The producing task is
// a provenance annotation on the ref, not a wall.
//
// Concurrent reuse. The builtin is stateless; the only
// shared resource is the `ArtifactStore`, whose concurrent-reuse contract
// lives in its own conformance suite. No per-invocation state
// crosses runs.

package builtin

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// defaultArtifactFetchMaxBytes is the window applied when the caller
// omits `max_bytes`, and hardArtifactFetchMaxBytes is the ceiling a
// caller's own `max_bytes` is clamped to. Both are single-sourced on
// the configuration package so the tool and the Protocol byte read
// answer to the SAME operator policy rather than to two literals that
// can drift apart.
//
// These are the FALLBACKS used when no operator-resolved bound is
// threaded onto the registry context — a standalone catalog in a test,
// or an embedder that registers the builtin without a configuration.
// The production boot path threads the resolved values.
const (
	defaultArtifactFetchMaxBytes = config.DefaultArtifactFetchMaxBytes
	hardArtifactFetchMaxBytes    = config.DefaultArtifactFetchHardMaxBytes
)

// fetchBounds is the resolved read-back policy one artifact_fetch
// registration serves under: the window applied when the model names no
// bound, and the ceiling its own bound is clamped to. Immutable after
// registration — the tool is a compiled artifact and holds no per-run
// state.
type fetchBounds struct {
	defaultMax int
	hardMax    int
}

// resolveFetchBounds normalises an operator's configured pair. A
// non-positive value means "unset" and takes the built-in; a default
// above its own ceiling is clamped DOWN to the ceiling rather than
// silently raising it, because the ceiling is the operator's stated
// upper bound and the default is a convenience inside it.
func resolveFetchBounds(defaultMax, hardMax int) fetchBounds {
	if defaultMax <= 0 {
		defaultMax = defaultArtifactFetchMaxBytes
	}
	if hardMax <= 0 {
		hardMax = hardArtifactFetchMaxBytes
	}
	if defaultMax > hardMax {
		defaultMax = hardMax
	}
	return fetchBounds{defaultMax: defaultMax, hardMax: hardMax}
}

// effectiveMax resolves the window length one fetch may return: the
// model's own bound, the operator's default when it named none, and
// never more than the operator's ceiling.
//
// The result is FLOORED at [utf8.UTFMax] (4). Below that floor a window
// aligned to a rune boundary can be shorter than the first rune it
// contains, so the rune discipline the string-typed `content` channel
// applies trims it to nothing while bytes still remain — and the
// documented paging rule (`offset + returned_bytes`) then yields the
// same offset forever. A boundary-aligned four-byte window always holds
// at least one complete rune, which is what makes the read path's
// strict-progress property provable rather than empirical.
//
// The floor applies to the OPERATOR-RESOLVED bound as well as to a
// caller's own `max_bytes`: a configured default or ceiling of 1–3 is
// raised to 4 by this one path. That is the only bound in the read path
// that clamps UP, and it is deliberate — the alternatives are a response
// that reports `truncated` with an empty window (a lie: bytes remain) or
// a refusal of a legal request (a wall). It is never silent:
// `returned_bytes` reports the truth and the tool description states the
// floor.
func (b fetchBounds) effectiveMax(requested int) int {
	if requested <= 0 {
		requested = b.defaultMax
	}
	if requested > b.hardMax {
		requested = b.hardMax
	}
	if requested < utf8.UTFMax {
		requested = utf8.UTFMax
	}
	return requested
}

// registerArtifactFetch attaches `artifact_fetch` to the catalog. The
// LLM-facing description is the load-bearing surface — it's what the
// model reads in `<available_tools>` and what teaches it when to call
// the tool. Keep the language concrete and example-shaped (the model
// follows examples better than directives).
func registerArtifactFetch(cat tools.ToolCatalog, store artifacts.ArtifactStore, bounds fetchBounds) error {
	return inproc.RegisterFunc[ArtifactFetchArgs, ArtifactFetchOut](
		cat, "artifact_fetch",
		func(ctx context.Context, args ArtifactFetchArgs) (ArtifactFetchOut, error) {
			return artifactFetch(ctx, store, bounds, args)
		},
		tools.WithDescription(
			"Fetch the content of a previously-surfaced artifact "+
				"reference. When a prior tool result was too large to "+
				"return inline, the runtime stored the full bytes and "+
				"showed you only the head. Call this tool with that "+
				"reference (the runtime quoted it in square brackets "+
				"after the inlined preview) to read the payload. "+
				"Use `max_bytes` to bound the response and `offset` to "+
				"start the window at a byte position — that is how you "+
				"page a large file: read from offset 0, then re-call "+
				"with `offset` set to the previous "+
				"`offset + returned_bytes` while `truncated` is true. "+
				"Windows are byte ranges, not lines or rows: split the "+
				"returned text yourself, and expect a window to start or "+
				"end mid-line. Every response reports `total_size_bytes`, "+
				"`returned_bytes` and `truncated`, so you can always tell "+
				"a bounded read from a complete one. A `max_bytes` above "+
				"the deployment's ceiling is served AT the ceiling and "+
				"reported through those same fields — it is not an error, "+
				"and a `max_bytes` below 4 is served at 4 for the same "+
				"reason (a shorter window cannot hold a whole character). "+
				"This tool returns TEXT. Check the reference's `mime` "+
				"before calling: a non-text artifact (a PDF, an image, a "+
				"zip) is refused with an error naming its MIME and the "+
				"byte offset that failed, because delivering it as text "+
				"would corrupt it. To act on a non-text artifact, call a "+
				"tool that declares an artifact-reference parameter — it "+
				"receives the bytes directly, without routing them "+
				"through your context."),
		tools.WithSideEffect(tools.SideEffectRead),
		tools.WithLoading(tools.LoadingAlways),
		tools.WithTags("builtin", "meta", "artifact"),
	)
}

// ArtifactFetchArgs is the typed input shape (the inproc deriver
// generates a JSON Schema the LLM sees from these tags).
type ArtifactFetchArgs struct {
	// Ref is the artifact identifier surfaced by a prior tool result's
	// fetch footer.
	Ref string `json:"ref"`
	// MaxBytes bounds the returned window. Zero or negative applies the
	// deployment's configured default; a value above its configured
	// ceiling is SERVED AT THE CEILING and reported through the same
	// Truncated / TotalSizeBytes / ReturnedBytes fields as any other
	// bound — never refused, never silently shortened.
	MaxBytes int `json:"max_bytes,omitempty"`
	// Offset is the zero-based BYTE index the returned window starts at.
	// It is how a model pages a large artifact: read at 0, then re-call
	// with Offset set to the previous Offset + ReturnedBytes while
	// Truncated is true.
	//
	// The window is a byte range and is MIME-agnostic — it knows nothing
	// about lines, rows or records, so a window may begin and end
	// mid-line and the caller splits the text itself. That is deliberate:
	// a stored MIME is not revisable on a content-addressed store, so
	// keying read behaviour on one would turn a wrong stamp into a
	// permanent refusal.
	//
	// An offset at or beyond the artifact's size is not an error — the
	// content is empty and Truncated is false, because nothing follows
	// the window. A negative offset is a soft error rather than a guess.
	Offset int `json:"offset,omitempty"`
}

// ArtifactFetchOut is the typed return shape. `Error` is a soft-error
// channel (e.g. unknown ref, cross-tenant rejection) the LLM reads as
// observation text without the runtime aborting the run — same shape
// as the discovery meta-tools.
type ArtifactFetchOut struct {
	Ref       string `json:"ref"`
	MIME      string `json:"mime,omitempty"`
	SizeBytes int64  `json:"size_bytes"`
	// Content is the admitted window as text. It is exactly
	// `blob[Offset : Offset+ReturnedBytes]` — the requested range trimmed
	// to whole runes at both ends — so a model paging by the documented
	// rule reassembles the artifact with no gaps and no duplicates.
	//
	// A window that is not valid UTF-8 after that trimming is REFUSED
	// rather than delivered: `Content` is empty and `Error` names the
	// stored MIME, the failing byte offset and the by-reference route
	// those bytes can travel. The refusal exists because this field is a
	// string on its way through `encoding/json`, which rewrites every
	// invalid byte to U+FFFD — so a binary artifact would arrive
	// corrupted, at a different length than `ReturnedBytes` reported.
	Content string `json:"content,omitempty"`
	// Offset is the byte index the returned window ACTUALLY starts at.
	// When the requested offset landed mid-rune it is the requested
	// offset advanced past the continuation bytes that were trimmed, not
	// the requested offset echoed back — a paging model computes its next
	// call as `Offset + ReturnedBytes`, and echoing a stale offset would
	// drop the trimmed bytes with no signal.
	Offset int64 `json:"offset"`
	// ReturnedBytes is the length of `Content` in bytes.
	ReturnedBytes int64 `json:"returned_bytes"`
	// TotalSizeBytes is the artifact's full stored size — the
	// denominator a bounded read is bounded against. It duplicates
	// SizeBytes, which predates this field and is kept because shipped
	// prompts and transcripts name it; both are the same number.
	TotalSizeBytes int64 `json:"total_size_bytes"`
	// Truncated reports whether bytes remain AFTER the returned window
	// (`offset + returned_bytes < total_size_bytes`). It is true
	// whichever bound applied — the caller's `max_bytes`, the
	// deployment's default, or the deployment's ceiling — so the model
	// never has to know WHICH bound applied to know that one did. Re-call
	// with `offset` advanced to read on.
	Truncated bool `json:"truncated,omitempty"`
	// Error carries soft failures. When set, `Content` is empty and
	// the LLM sees the error message as the observation text.
	Error string `json:"error,omitempty"`
}

// artifactFetch is the registered tool body. Hard-error returns
// (operator misconfiguration: nil store; missing identity) abort the
// invoke; soft-error returns (unknown ref, cross-tenant rejection)
// land on `Error` so the planner can repair without the run failing.
func artifactFetch(ctx context.Context, store artifacts.ArtifactStore, bounds fetchBounds, args ArtifactFetchArgs) (ArtifactFetchOut, error) {
	if store == nil {
		// Operator misconfiguration — the wiring didn't pass an
		// ArtifactStore. Fail loud per CLAUDE.md §13 (silent
		// degradation is forbidden).
		return ArtifactFetchOut{}, errors.New("artifact_fetch: backing ArtifactStore is nil (operator misconfiguration: builtin.RegistryContext.ArtifactStore was not threaded)")
	}
	id, err := requireIdentity(ctx)
	if err != nil {
		return ArtifactFetchOut{}, err
	}
	args.Ref = strTrim(args.Ref)
	if args.Ref == "" {
		return ArtifactFetchOut{Error: "ref field is empty"}, nil
	}
	if args.Offset < 0 {
		// A negative offset is not an omission — a model that meant "from
		// the end" is asking for something this tool does not offer, and
		// reinterpreting it would be a guess the model could not detect.
		return ArtifactFetchOut{
			Ref:   args.Ref,
			Error: fmt.Sprintf("artifact_fetch: offset %d must not be negative", args.Offset),
		}, nil
	}
	maxBytes := bounds.effectiveMax(args.MaxBytes)
	scope := artifacts.ArtifactScope{
		TenantID:  id.TenantID,
		UserID:    id.UserID,
		SessionID: id.SessionID,
		// TaskID intentionally left empty, and it no longer has to
		// match anything: the store's read key IS the isolation triple
		// and `TaskID` is a provenance annotation the drivers ignore
		// when resolving. This is what makes the session-artifact
		// manifest's invitation true — the manifest lists on the triple,
		// so it can enumerate an artifact a heavy tool result stamped
		// without a task AND one the LLM materialiser stamped with the
		// producing run, and this fetch resolves both.
	}
	ref, found, err := store.GetRef(ctx, scope, args.Ref)
	if err != nil {
		return ArtifactFetchOut{Error: fmt.Sprintf("artifact_fetch: GetRef failed: %v", err)}, nil
	}
	if !found {
		// Either the ref does not exist OR the cross-identity scope
		// boundary rejected the read. The store does NOT distinguish
		// these cases (cross-identity = found-false by construction),
		// so we surface a single message that holds for both. The
		// security-regression test pins this shape.
		return ArtifactFetchOut{
			Ref:   args.Ref,
			Error: fmt.Sprintf("artifact_fetch: ref %q not found in this session's scope", args.Ref),
		}, nil
	}
	bytes, found, err := store.Get(ctx, scope, args.Ref)
	if err != nil {
		return ArtifactFetchOut{Error: fmt.Sprintf("artifact_fetch: Get failed: %v", err)}, nil
	}
	if !found {
		// GetRef succeeded but Get returned found-false — a race
		// (e.g. concurrent Delete) or driver inconsistency. Surface
		// the same shape as the unknown-ref case.
		return ArtifactFetchOut{
			Ref:   args.Ref,
			Error: fmt.Sprintf("artifact_fetch: ref %q disappeared during fetch", args.Ref),
		}, nil
	}
	// The window is taken from the whole blob the store returned. That is
	// a CONTRACT, not a cost claim: `ArtifactStore.Get` has no range
	// parameter and every driver materialises the blob, so reading at an
	// offset costs a full materialisation. Making the read cheap is a
	// range-aware store method with five-driver conformance parity — its
	// own change, deliberately not implied here.
	total := int64(len(bytes))
	offset := int64(args.Offset)
	win := admissibleWindow(bytes, offset, int64(maxBytes))
	if win.invalidAt >= 0 {
		// Inadmissible: the window is not valid UTF-8 after trimming the
		// rune fragments windowing itself created, so it cannot travel
		// through the string-typed `content` field. Returning it anyway
		// is what this refusal replaces: `encoding/json` rewrites every
		// invalid byte to U+FFFD on the way to the model, so the caller
		// would receive DIFFERENT bytes at a DIFFERENT length than
		// `returned_bytes` reported — the silent-degradation shape
		// CLAUDE.md §13 forbids.
		//
		// The answer is facts, not only a wall: the identity, MIME and
		// size fields are populated so a model that followed a fetch
		// hint learns WHAT it holds, and the message names the route
		// those bytes can travel. The windowing fields stay zero-valued
		// on purpose — a refusal reporting `truncated: true` would
		// invite a model to page into the same wall forever.
		return ArtifactFetchOut{
			Ref:            ref.ID,
			MIME:           ref.MimeType,
			SizeBytes:      ref.SizeBytes,
			TotalSizeBytes: total,
			Error: fmt.Sprintf(
				"artifact_fetch: artifact %q (mime %q) is not readable as text: "+
					"byte %d is not valid UTF-8, so returning this window would corrupt it. "+
					"To act on these bytes, call a tool that declares an artifact-reference "+
					"parameter and pass it %q — the runtime hands that tool the bytes directly, "+
					"without routing them through your context.",
				ref.ID, ref.MimeType, win.invalidAt, ref.ID),
		}, nil
	}
	return ArtifactFetchOut{
		Ref:  ref.ID,
		MIME: ref.MimeType,
		// SizeBytes and TotalSizeBytes are the same number; the former
		// predates the truthful-bound field set and is kept for the
		// prompts and transcripts that already name it.
		SizeBytes:      ref.SizeBytes,
		Content:        string(win.content),
		Offset:         win.start,
		ReturnedBytes:  int64(len(win.content)),
		TotalSizeBytes: total,
		Truncated:      win.truncated,
	}, nil
}

// textWindow is the outcome of admitting a byte window onto the
// string-typed `content` channel.
type textWindow struct {
	// content is the admitted window: a copy of the requested byte range
	// trimmed to whole runes at both ends. Nil when invalidAt >= 0.
	content []byte
	// start is the absolute byte index content actually begins at — the
	// requested offset advanced past any front-trimmed rune
	// continuation. It is what the response reports as `offset`, and
	// what makes `offset + returned_bytes` a truthful next-page cursor.
	start int64
	// truncated reports whether bytes remain after content.
	truncated bool
	// invalidAt is the absolute byte index at which admissibility
	// failed, or -1 when the window is admissible.
	invalidAt int64
}

// admissibleWindow takes the requested byte range and decides whether it
// can travel through the model-facing string channel.
//
// Two trims, each justified by WHERE the fragment came from:
//
//   - LEADING rune continuations are dropped when the requested offset
//     is non-zero. A caller paging by byte offset can land mid-rune, and
//     that split is an artefact of windowing, not of the stored content.
//     At offset zero there is no split to blame, so a leading
//     continuation byte is content and is refused.
//   - A TRAILING incomplete rune is dropped only when bytes remain after
//     the window, for the same reason read the other way: the rune's
//     missing bytes are in the artifact, just past the bound. When the
//     window reaches the end of the artifact there is nothing past it,
//     so an incomplete trailing rune is content and is refused.
//
// What survives both trims must be valid UTF-8. A single bad byte in the
// middle of otherwise-valid text is refused, not dropped.
func admissibleWindow(blob []byte, offset, maxBytes int64) textWindow {
	window, truncated := artifactWindow(blob, offset, maxBytes)
	start := offset
	if len(window) == 0 {
		return textWindow{content: window, start: start, truncated: truncated, invalidAt: -1}
	}
	if offset > 0 {
		if k := leadingContinuationBytes(window); k > 0 {
			window = window[k:]
			start += int64(k)
		}
	}
	if truncated {
		if k := incompleteRuneSuffix(window); k > 0 {
			window = window[:len(window)-k]
		}
	}
	if bad := firstInvalidByteIndex(window); bad >= 0 {
		return textWindow{start: start, invalidAt: start + int64(bad)}
	}
	return textWindow{content: window, start: start, truncated: truncated, invalidAt: -1}
}

// leadingContinuationBytes counts the leading UTF-8 continuation bytes —
// the tail of a rune whose leading byte lies BEFORE the window. Capped
// at utf8.UTFMax-1: a longer run cannot be one rune's tail, so the
// remainder is left in place for the validity check to refuse.
func leadingContinuationBytes(b []byte) int {
	n := 0
	for n < len(b) && n < utf8.UTFMax-1 && !utf8.RuneStart(b[n]) {
		n++
	}
	return n
}

// incompleteRuneSuffix returns the number of trailing bytes that form
// the PREFIX of a rune whose remaining bytes lie beyond the window, and
// 0 when the window ends on a rune boundary or on genuinely invalid
// content. Distinguishing those two is the whole job: "cut in half by
// the bound" is trimmable, "malformed in the stored bytes" is not.
func incompleteRuneSuffix(b []byte) int {
	limit := utf8.UTFMax - 1
	if len(b) < limit {
		limit = len(b)
	}
	for k := 1; k <= limit; k++ {
		lead := b[len(b)-k]
		if !utf8.RuneStart(lead) {
			continue
		}
		if n := encodedRuneLen(lead); n > k {
			return k
		}
		return 0
	}
	return 0
}

// encodedRuneLen reports how many bytes the rune introduced by this
// leading byte occupies, or 0 when the byte cannot lead a rune.
func encodedRuneLen(lead byte) int {
	switch {
	case lead < 0x80:
		return 1
	case lead&0xE0 == 0xC0:
		return 2
	case lead&0xF0 == 0xE0:
		return 3
	case lead&0xF8 == 0xF0:
		return 4
	default:
		return 0
	}
}

// firstInvalidByteIndex returns the index of the first byte that begins
// an invalid UTF-8 sequence, or -1 when b is entirely valid. A correctly
// encoded U+FFFD is valid and reports -1; the size check is what tells
// it apart from a decode failure, which also yields RuneError.
func firstInvalidByteIndex(b []byte) int {
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size <= 1 {
			return i
		}
		i += size
	}
	return -1
}

// artifactWindow returns blob[offset : offset+maxBytes] clipped to the
// blob, plus whether bytes remain AFTER the window. An offset at or
// beyond the blob yields an empty window and truncated=false — nothing
// follows it, which is the honest answer rather than an error.
//
// The returned slice is a COPY. The window outlives the store call that
// produced the blob, and whether a store's returned buffer is the
// caller's to keep is a per-driver property, not a contract every driver
// states — so the read side does not depend on the answer.
//
// # What this helper shares with the Protocol byte read, and what it
// # deliberately does not
//
// The Protocol's own window helper (`boundedWindow`, in
// internal/protocol) is byte-identical to this one and MUST stay so: the
// copy-not-alias invariant is the shared property, and a window-slicing
// helper that aliases beside one that copies is the same helper
// disagreeing with itself — a disagreement only ever discovered by the
// driver that stops cloning.
//
// The rune discipline and the four-byte floor are NOT shared, and a
// future reader who finds that divergence should find this argument
// rather than "restore consistency". They belong to [admissibleWindow]
// and to [fetchBounds.effectiveMax], one layer above this helper,
// because they are properties of the MODEL-FACING STRING CHANNEL: the
// tool returns `content` as a Go string that is JSON-encoded on its way
// to the model, and `encoding/json` rewrites every invalid byte to
// U+FFFD. The Protocol response carries `[]byte`, which is base64 over
// the wire and therefore byte-exact for every MIME. Propagating rune
// trimming into the Protocol path would SHORT-READ an operator's PDF —
// silently, and at a length the response misreports.
//
// So: same helper, same invariant, two callers with two different
// admissibility rules above it. The Protocol side's binary-bytes-exact
// test is the trip-wire that catches the "consistency" refactor.
func artifactWindow(blob []byte, offset, maxBytes int64) (window []byte, truncated bool) {
	total := int64(len(blob))
	if offset >= total {
		return nil, false
	}
	end := offset + maxBytes
	if end > total {
		end = total
	}
	out := make([]byte, end-offset)
	copy(out, blob[offset:end])
	return out, end < total
}

// strTrim is a small helper to keep the body compact; the standard
// library import surface stays tiny.
func strTrim(s string) string {
	// Inline a minimal trim of leading/trailing ASCII whitespace —
	// strings.TrimSpace would also work but pulling in the package
	// just for this is overkill in a tightly-scoped builtin.
	start := 0
	end := len(s)
	for start < end && isSpace(s[start]) {
		start++
	}
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r':
		return true
	}
	return false
}
