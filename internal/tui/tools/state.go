// Package tools derives terminal tool inspection state from Protocol values.
package tools

import (
	"strings"

	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tui/surface"
)

// Detail contains catalog, schema, policy, OAuth, analytics, and content posture.
type Detail struct {
	Tool        types.Tool
	Manifest    *types.ToolManifest
	Metrics     *types.ToolMetrics
	Content     *types.ToolContentStats
	BestEffort  bool
	Annotations bool
	Unavailable string
}

// State is one identity-scoped tool catalog projection.
type State struct {
	Surface           surface.State
	Rows              []types.Tool
	Aggregates        types.ToolAggregates
	AggregatesPartial bool
	Selected          *Detail
	SelectedID        string
	Filter            string
	Page, PageSize    int
}

// Derive copies canonical rows and preserves annotation partiality.
func Derive(response types.ToolListResponse, annotations bool) State {
	status := surface.Loaded
	if response.AggregatesPartial {
		status = surface.Partial
	}
	return State{Surface: surface.State{Status: status}, Rows: append([]types.Tool(nil), response.Tools...), Aggregates: response.Aggregates, AggregatesPartial: response.AggregatesPartial, Page: max(1, response.Page), PageSize: max(1, response.PageSize)}
}

// Visible returns the filterable current page.
func (s State) Visible() []types.Tool {
	query := strings.ToLower(strings.TrimSpace(s.Filter))
	rows := make([]types.Tool, 0, len(s.Rows))
	for _, row := range s.Rows {
		if query == "" || strings.Contains(strings.ToLower(row.ID+" "+row.Name+" "+string(row.Transport)+" "+string(row.OAuthStatus)+" "+string(row.ApprovalPolicy)), query) {
			rows = append(rows, row)
		}
	}
	return rows
}
