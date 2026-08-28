// Package artifactcontent contains the transport-neutral seam for tool
// results that carry protocol-native binary content. A driver exposes the
// content through tools.ArtifactContentResult; this package persists each
// binary part and returns the driver's metadata-only projection.
package artifactcontent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/tools"
)

const (
	// DefaultMaxBytesPerPart is the defensive upper bound for one
	// protocol-native binary part. It prevents one malformed result from
	// turning the materializer into an unbounded allocation/write path.
	// Deployments may impose a stricter limit in their ArtifactStore.
	DefaultMaxBytesPerPart int64 = 64 << 20

	// DefaultMaxBytesPerResult bounds the aggregate binary payload in one
	// ToolResult. Keeping a separate aggregate bound prevents a result with
	// many individually valid parts from bypassing the per-part ceiling.
	DefaultMaxBytesPerResult int64 = 128 << 20
)

var (
	// ErrStoreUnavailable means a binary result reached a path with no
	// ArtifactStore. Returning the raw value would violate the planner
	// boundary, so callers must fail closed.
	ErrStoreUnavailable = errors.New("artifact content: ArtifactStore is required for binary tool output")
	// ErrTooLarge means a protocol-native result exceeded the materializer's
	// bounded write budget. The result is refused rather than truncated.
	ErrTooLarge = errors.New("artifact content: binary tool output exceeds the materialization limit")
	// ErrInvalidResult means a driver violated the content-result contract,
	// such as returning a different number of refs than candidates or leaving
	// a raw candidate in its projected value.
	ErrInvalidResult = errors.New("artifact content: invalid projected tool result")
	// ErrEmptyPart means a driver returned a binary content block without
	// any bytes. Empty binary blocks are refused instead of creating an
	// apparently valid artifact that cannot be rendered or downloaded.
	ErrEmptyPart = errors.New("artifact content: binary content part is empty")
)

// Materialize stores every binary candidate in value under scope and returns
// the driver's metadata-only projection. Non-implementing values pass through
// unchanged. The function is deliberately independent of MCP or any concrete
// tool driver; MCP, A2A, and future protocol adapters can implement the same
// tools.ArtifactContentResult seam.
//
// Ordering is deterministic: candidates are walked in the value's declared
// order, receive a stable content_index, and are handed back to the driver in
// that same order. One timestamp is used for the whole result so a session
// manifest never depends on sub-millisecond Put timing. Resource links and
// other references are not candidates and are never fetched.
//
// Identity is checked before the first write. The ArtifactStore scope is the
// session's (tenant, user, session) triple; scope.TaskID is provenance only.
// Cancellation is checked before each write and before projection. A storage
// error is returned as-is with context, never replaced by a truncated or raw
// fallback. ArtifactStore has no transaction/rollback contract; if a later
// part fails, earlier content-addressed writes may remain in the session
// manifest. They are identity-scoped and deduplicated on retry.
func Materialize(ctx context.Context, store artifacts.ArtifactStore, scope artifacts.ArtifactScope, value any, provenance string) (projected any, err error) {
	// Preserve the underlying sentinel (store, cancellation, validation, or
	// projection) while marking every materialization failure terminal for the
	// reliability shell. A remote tool call can have completed before this
	// local phase fails; reissuing it is not a safe recovery strategy.
	defer func() {
		if err != nil && !errors.Is(err, tools.ErrToolResultMaterialization) {
			err = fmt.Errorf("%w: %w", tools.ErrToolResultMaterialization, err)
		}
	}()
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrInvalidResult)
	}
	result, ok := value.(tools.ArtifactContentResult)
	if !ok {
		return value, nil
	}
	parts := result.ArtifactContentParts()
	if len(parts) == 0 {
		return value, nil
	}
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidResult, err)
	}
	if store == nil {
		return nil, ErrStoreUnavailable
	}

	var total int64
	mimeTypes := make([]string, len(parts))
	mimeBases := make([]string, len(parts))
	filenames := make([]string, len(parts))
	for i, part := range parts {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("artifact content: before part %d: %w", i, err)
		}
		size := int64(len(part.Data))
		if size == 0 {
			return nil, fmt.Errorf("%w: part %d", ErrEmptyPart, i)
		}
		if size > DefaultMaxBytesPerPart {
			return nil, fmt.Errorf("%w: part %d is %d bytes (limit %d)", ErrTooLarge, i, size, DefaultMaxBytesPerPart)
		}
		if size > DefaultMaxBytesPerResult-total {
			return nil, fmt.Errorf("%w: result exceeds %d bytes at part %d", ErrTooLarge, DefaultMaxBytesPerResult, i)
		}
		mimeType, mimeBase, err := normalizeMIME(part.MIMEType)
		if err != nil {
			return nil, fmt.Errorf("artifact content: part %d MIME type: %w", i, err)
		}
		mimeTypes[i] = mimeType
		mimeBases[i] = mimeBase
		filenames[i] = safeFilename(i, mimeType, part.Filename)
		total += size
	}

	name := strings.TrimSpace(provenance)
	if name == "" {
		name = "tool"
	}
	createdAt := time.Now().UTC()
	refs := make([]tools.ArtifactContentRef, len(parts))
	for i, part := range parts {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("artifact content: before part %d: %w", i, err)
		}
		mimeType := mimeTypes[i]
		filename := filenames[i]
		source := map[string]any{
			// ToolResult content is a tool-produced artifact regardless of
			// which protocol adapter supplied it. Keep the canonical source
			// enum stable for Protocol filters; the detailed producer below
			// carries the adapter/descriptor provenance.
			"source":        "tool",
			"producer":      name,
			"content_kind":  part.Kind,
			"content_index": i,
			"created_at":    createdAt,
			"task_id":       scope.TaskID,
		}
		if sourceURI := strings.TrimSpace(part.SourceURI); sourceURI != "" {
			source["source_uri"] = sourceURI
		}
		ref, err := store.PutBytes(ctx, scope, part.Data, artifacts.PutOpts{
			MimeType:  mimeType,
			Filename:  filename,
			Namespace: namespaceForMIME(mimeBases[i]),
			Source:    source,
		})
		if err != nil {
			return nil, fmt.Errorf("artifact content: store part %d: %w", i, err)
		}
		if err := validateRef(ref, scope, mimeType, mimeBases[i], filename, part.Data); err != nil {
			return nil, fmt.Errorf("artifact content: store part %d returned invalid ref: %w", i, err)
		}
		refs[i] = tools.ArtifactContentRef{
			ID:           ref.ID,
			MIMEType:     ref.MimeType,
			SizeBytes:    ref.SizeBytes,
			Filename:     ref.Filename,
			SHA256:       ref.SHA256,
			Provenance:   name,
			ContentIndex: i,
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("artifact content: before projection: %w", err)
	}
	projectedResult, err := result.WithArtifactContentRefs(refs)
	if err != nil {
		return nil, fmt.Errorf("%w: driver projection: %w", ErrInvalidResult, err)
	}
	if projectedResult == nil {
		return nil, fmt.Errorf("%w: driver returned nil projection", ErrInvalidResult)
	}
	if remaining := projectedResult.ArtifactContentParts(); len(remaining) != 0 {
		return nil, fmt.Errorf("%w: %d binary candidate(s) remain after projection", ErrInvalidResult, len(remaining))
	}
	projected = projectedResult
	return projected, nil
}

func normalizeMIME(raw string) (canonical, base string, err error) {
	mimeType := strings.TrimSpace(raw)
	if mimeType == "" {
		return "application/octet-stream", "application/octet-stream", nil
	}
	base, params, err := mime.ParseMediaType(mimeType)
	if err != nil {
		return "", "", fmt.Errorf("%q is not a valid media type: %w", mimeType, err)
	}
	canonical = mime.FormatMediaType(base, params)
	if canonical == "" {
		return "", "", fmt.Errorf("%q cannot be represented as a canonical media type", mimeType)
	}
	// Artifact routing and content-addressed namespaces use the canonical base
	// media type. Parameters are retained in the ref's MIME metadata because a
	// consumer may need charset/profile semantics, while the namespace ignores
	// them so harmless parameter/casing variants do not split one binary kind
	// across distinct content-addressed buckets.
	return canonical, base, nil
}

func safeFilename(index int, mimeType, requested string) string {
	base, _, err := mime.ParseMediaType(mimeType)
	ext := ".bin"
	if err == nil {
		extensions, extErr := mime.ExtensionsByType(base)
		if extErr == nil {
			for _, candidate := range extensions {
				if isSafeExtension(candidate) {
					ext = candidate
					break
				}
			}
		}
	}
	fallback := fmt.Sprintf("content-%03d%s", index+1, ext)
	return sanitizeFilename(requested, fallback)
}

func sanitizeFilename(requested, fallback string) string {
	name := strings.TrimSpace(requested)
	if name == "" {
		return fallback
	}
	// Treat both slash forms as separators before taking the final path
	// component. Filename is metadata only, but stripping traversal here
	// keeps every ArtifactRef safe for UI/download consumers too.
	name = strings.TrimSpace(strings.TrimSuffix(strings.ReplaceAll(name, "\\", "/"), "/"))
	if slash := strings.LastIndexByte(name, '/'); slash >= 0 {
		name = name[slash+1:]
	}
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f:
			return '_'
		case r == '/' || r == '\\':
			return '_'
		default:
			return r
		}
	}, name)
	name = strings.TrimSpace(strings.Trim(name, "."))
	if name == "" || name == "." || name == ".." {
		return fallback
	}
	// Keep metadata bounded without splitting a UTF-8 code point.
	runes := []rune(name)
	if len(runes) > 255 {
		name = string(runes[:255])
	}
	return name
}

func isSafeExtension(ext string) bool {
	if len(ext) < 2 || len(ext) > 16 || ext[0] != '.' {
		return false
	}
	for _, r := range ext[1:] {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func namespaceForMIME(mimeType string) string {
	digest := sha256.Sum256([]byte(mimeType))
	return "artifact.content." + hex.EncodeToString(digest[:4])
}

func validateRef(ref artifacts.ArtifactRef, scope artifacts.ArtifactScope, mimeType, mimeBase, filename string, data []byte) error {
	if ref.ID == "" {
		return errors.New("empty id")
	}
	if !ref.Scope.EqualTriple(scope) {
		return fmt.Errorf("scope %q/%q/%q, want %q/%q/%q", ref.Scope.TenantID, ref.Scope.UserID, ref.Scope.SessionID, scope.TenantID, scope.UserID, scope.SessionID)
	}
	_, refBase, err := normalizeMIME(ref.MimeType)
	if err != nil {
		return fmt.Errorf("MIME type %q is invalid: %w", ref.MimeType, err)
	}
	if refBase != mimeBase {
		return fmt.Errorf("MIME base type %q, want %q (canonical request %q)", refBase, mimeBase, mimeType)
	}
	size := int64(len(data))
	if ref.SizeBytes != size {
		return fmt.Errorf("size %d, want %d", ref.SizeBytes, size)
	}
	if ref.Filename == "" {
		return fmt.Errorf("empty filename (generated fallback was %q)", filename)
	}
	if strings.ContainsAny(ref.Filename, `/\\`) || ref.Filename == "." || ref.Filename == ".." {
		return fmt.Errorf("unsafe filename %q", ref.Filename)
	}
	if strings.TrimSpace(ref.Filename) == "" || len([]rune(ref.Filename)) > 255 {
		return fmt.Errorf("unsafe filename %q", ref.Filename)
	}
	for _, r := range ref.Filename {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("unsafe filename %q", ref.Filename)
		}
	}
	if len(ref.SHA256) != sha256.Size*2 {
		return fmt.Errorf("SHA256 length %d, want %d", len(ref.SHA256), sha256.Size*2)
	}
	if _, err := hex.DecodeString(ref.SHA256); err != nil {
		return fmt.Errorf("invalid SHA256: %w", err)
	}
	wantHash := sha256.Sum256(data)
	if ref.SHA256 != hex.EncodeToString(wantHash[:]) {
		return fmt.Errorf("SHA256 %q does not match stored bytes", ref.SHA256)
	}
	return nil
}
