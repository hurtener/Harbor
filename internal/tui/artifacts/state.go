// Package artifacts derives safe terminal artifact metadata and preview posture.
package artifacts

import (
	"strings"

	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tui/surface"
)

// Preview is safe metadata or a time-bounded external reference, never bytes.
type Preview struct {
	Ref         types.ArtifactRef
	URL         string
	Unavailable string
}

// State is one session's metadata-only artifact catalog.
type State struct {
	Surface      surface.State
	Rows         []types.ArtifactRow
	TotalMatched int
	Selected     *Preview
	SelectedID   string
	Filter       string
	Page         int
	PageSize     int
}

// Derive copies a canonical list response.
func Derive(response types.ArtifactsListResponse) State {
	return State{Surface: surface.State{Status: surface.Loaded}, Rows: append([]types.ArtifactRow(nil), response.Rows...), TotalMatched: response.TotalMatched, Page: 1, PageSize: 8}
}

// Visible returns metadata rows matching the local bounded filter.
func (s State) Visible() []types.ArtifactRow {
	query := strings.ToLower(strings.TrimSpace(s.Filter))
	rows := make([]types.ArtifactRow, 0, len(s.Rows))
	for _, row := range s.Rows {
		if query == "" || strings.Contains(strings.ToLower(row.Ref.ID+" "+row.Ref.Filename+" "+row.Ref.MimeType+" "+string(row.Source)), query) {
			rows = append(rows, row)
		}
	}
	start := min(max(0, s.Page-1)*max(1, s.PageSize), len(rows))
	return rows[start:min(len(rows), start+max(1, s.PageSize))]
}

// Previewable reports terminal-safe textual/image metadata candidates. Actual
// bytes remain behind artifacts.get_ref and are never read by this package.
func Previewable(ref types.ArtifactRef) bool {
	return strings.HasPrefix(ref.MimeType, "text/") || strings.HasPrefix(ref.MimeType, "image/") || ref.MimeType == "application/json"
}
