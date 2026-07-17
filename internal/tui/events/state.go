// Package events derives retained/live event diagnostics from Protocol values.
package events

import (
	"strings"

	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tui/surface"
)

// State holds a bounded event page and aggregate completeness posture.
type State struct {
	Surface              surface.State
	Rows                 []types.StateEvent
	Buckets              []types.EventBucket
	HasMore, Truncated   bool
	Stream, Reconnecting string
	SelectedSequence     uint64
	Filter               string
	Page, PageSize       int
}

// Derive copies canonical event list and aggregate responses.
func Derive(list types.EventsListResponse, aggregate types.EventAggregateResponse) State {
	status := surface.Loaded
	if list.Truncated || aggregate.Truncated {
		status = surface.Partial
	}
	return State{Surface: surface.State{Status: status}, Rows: append([]types.StateEvent(nil), list.Events...), Buckets: append([]types.EventBucket(nil), aggregate.Buckets...), HasMore: list.HasMore, Truncated: list.Truncated || aggregate.Truncated, Page: 1, PageSize: 8}
}

// Visible returns rows matching type, run, and safe textual metadata.
func (s State) Visible() []types.StateEvent {
	query := strings.ToLower(strings.TrimSpace(s.Filter))
	rows := make([]types.StateEvent, 0, len(s.Rows))
	for _, row := range s.Rows {
		if query == "" || strings.Contains(strings.ToLower(row.Type+" "+row.Run), query) {
			rows = append(rows, row)
		}
	}
	start := min(max(0, s.Page-1)*max(1, s.PageSize), len(rows))
	return rows[start:min(len(rows), start+max(1, s.PageSize))]
}
