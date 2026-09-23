package memory

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
)

const referencePrefix = "memory_source_"

// IsSourceReference identifies the reserved, source-bound memory reference
// namespace. It must never fall through to ordinary blob lookup or presigning.
func IsSourceReference(id string) bool { return strings.HasPrefix(id, referencePrefix) }

// SourceReference names the exact bytes of one committed memory item without
// persisting a second copy. Scope, source key, content and expiry all participate
// in the name. Consumers resolve it through the bounded artifacts.get surface;
// it is not a blob in ArtifactStore and cannot be presigned independently.
func SourceReference(id identity.Quadruple, item Item) (artifacts.ArtifactRef, error) {
	if err := ValidateIdentity(id); err != nil {
		return artifacts.ArtifactRef{}, err
	}
	if item.Key == "" || !json.Valid(item.Value) {
		return artifacts.ArtifactRef{}, ErrInvalidInspection
	}
	digest := sha256.Sum256(item.Value)
	contentHash := hex.EncodeToString(digest[:])
	var binding []byte
	for _, part := range []string{id.TenantID, id.UserID, id.SessionID, item.Key, contentHash, item.ExpiresAt.UTC().Format(time.RFC3339Nano)} {
		binding = binary.AppendUvarint(binding, uint64(len(part)))
		binding = append(binding, part...)
	}
	bound := sha256.Sum256(binding)
	return artifacts.ArtifactRef{
		ID:       referencePrefix + hex.EncodeToString(bound[:]),
		MimeType: "application/json", SizeBytes: int64(len(item.Value)), SHA256: contentHash,
		Scope:     artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID},
		Namespace: "memory_source",
		Source:    map[string]any{"producer": "memory.get", "memory_key": item.Key},
	}, nil
}

// ResolveSourceReference rechecks current committed memory on every read.
// Expiry, erasure, compaction or deletion that removes the source makes the
// reference unavailable; a changed value cannot be served under the old name.
// Read failures remain failures, never an instruction to retry an old action.
func ResolveSourceReference(ctx context.Context, store MemoryStore, id identity.Quadruple, name string) (artifacts.ArtifactRef, []byte, error) {
	if err := ValidateIdentity(id); err != nil {
		return artifacts.ArtifactRef{}, nil, err
	}
	if err := ctx.Err(); err != nil {
		return artifacts.ArtifactRef{}, nil, err
	}
	if !IsSourceReference(name) || len(name) != len(referencePrefix)+sha256.Size*2 || store == nil {
		return artifacts.ArtifactRef{}, nil, ErrNotFound
	}
	view, err := store.Inspect(ctx, id)
	if err != nil {
		return artifacts.ArtifactRef{}, nil, fmt.Errorf("memory: resolve source reference: %w", err)
	}
	for _, item := range view.Items {
		if !item.ExpiresAt.IsZero() && !time.Now().Before(item.ExpiresAt) {
			continue
		}
		ref, err := SourceReference(id, item)
		if err != nil {
			return artifacts.ArtifactRef{}, nil, err
		}
		if ref.ID == name {
			return ref, append([]byte(nil), item.Value...), nil
		}
	}
	return artifacts.ArtifactRef{}, nil, ErrNotFound
}
