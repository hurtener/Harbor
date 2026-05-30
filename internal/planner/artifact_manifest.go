package planner

import (
	"sort"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
)

// BuildArtifactManifest maps a session's listed artifact refs onto the
// metadata-only [ArtifactManifestEntry] slice the planner renders into
// the read-only `<session_artifacts>` prompt block (Phase 107f — D-176).
//
// It is the SINGLE source of the run-loop ↔ devstack manifest build so
// the production dev run loop and the test harness cannot diverge
// (CLAUDE.md §17.6 parity). Both call sites list `ArtifactStore.List`
// scoped to the run's `(tenant, user, session)` triple and hand the
// returned refs here.
//
// Ordering is stable newest-first: the artifact's `created_at`
// provenance stamp (when present) primary, descending; the content-
// addressed `ID` secondary, ascending, as a deterministic tiebreaker so
// the map-iteration-order non-determinism of `ArtifactStore.List` never
// leaks into the prompt (a stable prefix preserves KV-cache windows).
//
// The FULL slice is returned; the renderer ([renderSessionArtifacts] in
// the react package) caps the rendered rows and appends an explicit
// "+K more" line on overflow (AC-6) — never a silent truncation. A nil /
// empty input returns nil so the planner omits the block entirely.
func BuildArtifactManifest(refs []artifacts.ArtifactRef) []ArtifactManifestEntry {
	if len(refs) == 0 {
		return nil
	}

	ordered := make([]artifacts.ArtifactRef, len(refs))
	copy(ordered, refs)
	sort.SliceStable(ordered, func(i, j int) bool {
		ti := artifactManifestCreatedAt(ordered[i].Source)
		tj := artifactManifestCreatedAt(ordered[j].Source)
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return ordered[i].ID < ordered[j].ID
	})

	out := make([]ArtifactManifestEntry, 0, len(ordered))
	for _, ref := range ordered {
		out = append(out, ArtifactManifestEntry{
			Ref:        ref.ID,
			Filename:   ref.Filename,
			MIME:       ref.MimeType,
			SizeBytes:  ref.SizeBytes,
			Provenance: ResolveProvenance(ref.Source),
		})
	}
	return out
}

// ResolveProvenance derives the human-readable origin string the
// manifest shows for an artifact, from its opaque `Source` map (Phase
// 107f — D-176). Resolution order:
//
//  1. The canonical `source` key when it is a non-empty string
//     (e.g. "user_upload", "tool", "flow").
//  2. Otherwise a `tool` name key → "tool: <name>".
//  3. Otherwise a `flow` name key → "flow: <name>".
//  4. Otherwise a `producer` key → that value verbatim.
//  5. Otherwise "unknown".
//
// The else-chain keeps EXISTING artifacts (put before Phase 107f, so
// carrying no canonical `source` key) resolving to a real provenance
// instead of "unknown" — no back-fill migration needed (D-176).
func ResolveProvenance(src map[string]any) string {
	if src == nil {
		return "unknown"
	}
	if s, ok := src["source"].(string); ok && s != "" {
		return s
	}
	if t, ok := src["tool"].(string); ok && t != "" {
		return "tool: " + t
	}
	if f, ok := src["flow"].(string); ok && f != "" {
		return "flow: " + f
	}
	if p, ok := src["producer"].(string); ok && p != "" {
		return p
	}
	return "unknown"
}

// artifactManifestCreatedAt coerces the storage ref's opaque
// `created_at` value into a time.Time for the newest-first ordering. It
// tolerates the shapes a JSON round-trip can produce (time.Time, an
// RFC-3339 string, a numeric Unix-seconds value). A missing / unparsable
// stamp yields the zero time, which sorts last (oldest).
func artifactManifestCreatedAt(src map[string]any) time.Time {
	if src == nil {
		return time.Time{}
	}
	switch t := src["created_at"].(type) {
	case time.Time:
		return t
	case string:
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			return parsed
		}
	case float64:
		return time.Unix(int64(t), 0).UTC()
	case int64:
		return time.Unix(t, 0).UTC()
	}
	return time.Time{}
}
