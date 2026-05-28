// Package-level: artifact_fetch is the LLM-facing recovery path for
// heavy-content artifacts. The runtime tool-executor materialises
// tool results above the heavy-output threshold (D-026) to the
// artifact store and projects the LLM-facing observation as a small
// preview + a fetch hint; this builtin lets the LLM pull the full
// bytes of the referenced artifact when the preview doesn't carry
// what it needs.
//
// Identity is mandatory (CLAUDE.md §6 rule 9). Cross-tenant
// rejection is the artifact store's responsibility — the store keys
// storage on `(tenant,user,session,task)` and an out-of-scope read
// returns found-false, which we surface as `{error}` (NOT exposed
// bytes). The builtin's test pins this with a deliberate
// cross-identity write + read.
//
// Concurrent reuse (D-025). The builtin is stateless; the only
// shared resource is the `ArtifactStore`, whose D-025 contract
// lives in its own conformance suite. No per-invocation state
// crosses runs.

package builtin

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// defaultArtifactFetchMaxBytes is the conservative ceiling applied
// when the caller omits `max_bytes`. 64 KiB is large enough for the
// typical YouTube-metadata-style payload (~1.5 KiB structured JSON)
// while small enough to keep an accidental fetch on a multi-megabyte
// artifact from blowing the LLM's context window.
const defaultArtifactFetchMaxBytes = 64 * 1024

// hardArtifactFetchMaxBytes caps `max_bytes` at 1 MiB. The runtime's
// LLM-edge safety pass (D-026) will still reject content above the
// configured heavy-output threshold, but this hard ceiling stops a
// caller from requesting hundreds of MB of bytes in one fetch.
const hardArtifactFetchMaxBytes = 1 * 1024 * 1024

// registerArtifactFetch attaches `artifact_fetch` to the catalog. The
// LLM-facing description is the load-bearing surface — it's what the
// model reads in `<available_tools>` and what teaches it when to call
// the tool. Keep the language concrete and example-shaped (the model
// follows examples better than directives).
func registerArtifactFetch(cat tools.ToolCatalog, store artifacts.ArtifactStore) error {
	return inproc.RegisterFunc[ArtifactFetchArgs, ArtifactFetchOut](
		cat, "artifact_fetch",
		func(ctx context.Context, args ArtifactFetchArgs) (ArtifactFetchOut, error) {
			return artifactFetch(ctx, store, args)
		},
		tools.WithDescription(
			"Fetch the full content of a previously-surfaced artifact "+
				"reference. When a prior tool result was too large to "+
				"return inline, the runtime stored the full bytes and "+
				"showed you only the head. Call this tool with that "+
				"reference (the runtime quoted it in square brackets "+
				"after the inlined preview) to read the full payload. "+
				"Use `max_bytes` to bound the response (default 64 KiB; "+
				"max 1 MiB). Returns the bytes as text; sets "+
				"`truncated: true` when the artifact is larger than "+
				"`max_bytes`."),
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
	// MaxBytes bounds the returned content. Zero / negative defaults to
	// 64 KiB; values above 1 MiB are clamped to 1 MiB.
	MaxBytes int `json:"max_bytes,omitempty"`
}

// ArtifactFetchOut is the typed return shape. `Error` is a soft-error
// channel (e.g. unknown ref, cross-tenant rejection) the LLM reads as
// observation text without the runtime aborting the run — same shape
// as the discovery meta-tools.
type ArtifactFetchOut struct {
	Ref       string `json:"ref"`
	MIME      string `json:"mime,omitempty"`
	SizeBytes int64  `json:"size_bytes"`
	Content   string `json:"content,omitempty"`
	// Truncated is set when the artifact's full size exceeds the
	// caller's `max_bytes` request — `Content` carries the head bytes
	// only. The LLM can re-call with a larger `max_bytes` if it
	// needs more (subject to the 1 MiB hard cap).
	Truncated bool `json:"truncated,omitempty"`
	// Error carries soft failures. When set, `Content` is empty and
	// the LLM sees the error message as the observation text.
	Error string `json:"error,omitempty"`
}

// artifactFetch is the registered tool body. Hard-error returns
// (operator misconfiguration: nil store; missing identity) abort the
// invoke; soft-error returns (unknown ref, cross-tenant rejection)
// land on `Error` so the planner can repair without the run failing.
func artifactFetch(ctx context.Context, store artifacts.ArtifactStore, args ArtifactFetchArgs) (ArtifactFetchOut, error) {
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
	maxBytes := args.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultArtifactFetchMaxBytes
	}
	if maxBytes > hardArtifactFetchMaxBytes {
		maxBytes = hardArtifactFetchMaxBytes
	}
	scope := artifacts.ArtifactScope{
		TenantID:  id.TenantID,
		UserID:    id.UserID,
		SessionID: id.SessionID,
		// TaskID intentionally left empty — the runtime tool-executor
		// stamps artifacts WITHOUT a TaskID (it derives the scope from
		// `rc.Quadruple` minus the RunID; see
		// `cmd/harbor/cmd_dev_executor.go::projectForLLM`). Reading
		// back with the same shape avoids a scope mismatch.
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
	out := ArtifactFetchOut{
		Ref:       ref.ID,
		MIME:      ref.MimeType,
		SizeBytes: ref.SizeBytes,
	}
	if int64(len(bytes)) > int64(maxBytes) {
		out.Content = string(bytes[:maxBytes])
		out.Truncated = true
	} else {
		out.Content = string(bytes)
	}
	return out, nil
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
