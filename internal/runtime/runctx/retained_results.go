package runctx

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/planner"
)

const (
	maxRetainedResultRefs          = 64
	maxRetainedResultMetadataBytes = 16 * 1024
)

// Recover only the existing dispatcher envelope, never identifiers generated
// by a summary or inferred from prose. This is an ephemeral projection of the
// bounded window, not a new persistent index or domain-specific registry.
func retainedResultReferences(ctx context.Context, rc planner.RunContext, store artifacts.ArtifactStore) ([]planner.ArtifactManifestEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if rc.Trajectory == nil {
		return nil, ErrRetainedContextUnavailable
	}
	ids := make(map[string]struct{})
	remaining := 2 * maxRetainedContextBytes // retained window plus current run
	var walk func(any, int) error
	walk = func(v any, depth int) error {
		if depth > 64 {
			return ErrRetainedContextCapacity
		}
		switch value := v.(type) {
		case map[string]any:
			truncated, hasTruncated := value["truncated"].(bool)
			ref, hasRef := value["artifact_ref"].(string)
			_, hasPreview := value["preview"].(string)
			tool, hasTool := value["tool"].(string)
			size, hasSize := value["size_bytes"].(json.Number)
			if hasTruncated && truncated && hasRef && hasPreview && hasTool && tool != "" && hasSize {
				sizeValue, err := size.Int64()
				if err != nil || sizeValue < 0 || ref == "" || len(ref) > 256 || strings.TrimSpace(ref) != ref || !utf8.ValidString(ref) {
					return ErrRetainedContextUnavailable
				}
				ids[ref] = struct{}{}
				if len(ids) > maxRetainedResultRefs {
					return ErrRetainedContextCapacity
				}
			}
			for _, child := range value {
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range value {
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, entry := range rc.Trajectory.Steps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		step := entry
		if step.Historical != nil {
			var err error
			step, err = planner.ReadHistoricalStep(step)
			if err != nil {
				return nil, ErrRetainedContextUnavailable
			}
		}
		observation := step.LLMObservation
		if observation == nil {
			observation = step.Observation
		}
		if observation == nil {
			continue
		}
		encoded, err := json.Marshal(observation)
		if err != nil {
			return nil, ErrRetainedContextUnavailable
		}
		if len(encoded) > remaining {
			return nil, ErrRetainedContextCapacity
		}
		remaining -= len(encoded)
		var value any
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return nil, ErrRetainedContextUnavailable
		}
		if err := walk(value, 0); err != nil {
			return nil, err
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if store == nil {
		return nil, ErrRetainedContextUnavailable
	}
	scope := artifacts.ArtifactScope{TenantID: rc.Quadruple.TenantID, UserID: rc.Quadruple.UserID, SessionID: rc.Quadruple.SessionID}
	names := make([]string, 0, len(ids))
	for id := range ids {
		names = append(names, id)
	}
	sort.Strings(names)
	refs := make([]planner.ArtifactManifestEntry, 0, len(names))
	for _, id := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ref, found, err := store.GetRef(ctx, scope, id)
		if err != nil || !found || ref == nil || ref.ID != id || ref.SizeBytes < 0 || ref.Scope.TenantID != scope.TenantID || ref.Scope.UserID != scope.UserID || ref.Scope.SessionID != scope.SessionID {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, ErrRetainedContextUnavailable
		}
		refs = append(refs, planner.ArtifactManifestEntry{Ref: id, Filename: ref.Filename, MIME: ref.MimeType, SizeBytes: ref.SizeBytes, Provenance: "retained tool result"})
	}
	encoded, err := json.Marshal(refs)
	if err != nil || len(encoded) > maxRetainedResultMetadataBytes {
		return nil, ErrRetainedContextCapacity
	}
	return refs, nil
}
