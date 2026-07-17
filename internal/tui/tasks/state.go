// Package tasks derives terminal task inspection state from Protocol values.
package tasks

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tui/surface"
)

// State is the bounded task list/tree/progress projection for one session.
type State struct {
	Surface    surface.State
	Rows       []types.TaskRow
	Aggregates types.TaskListAggregates
	Selected   *types.TaskDetail
	SelectedID string
	Filter     string
	Page       int
	PageSize   int
	NextCursor types.TaskListCursor
}

// Derive copies and deterministically orders canonical rows.
func Derive(response types.TaskListResponse, detail *types.TaskDetail) State {
	rows := append([]types.TaskRow(nil), response.Rows...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].StartedAt.After(rows[j].StartedAt) })
	selectedID := ""
	if detail != nil {
		selectedID = detail.Task.ID
	}
	return State{Surface: surface.State{Status: surface.Loaded}, Rows: rows, Aggregates: response.Aggregates, Selected: detail, SelectedID: selectedID, Page: 1, PageSize: max(1, len(response.Rows)), NextCursor: response.Cursor}
}

// Visible returns the filtered canonical page without changing selection.
func (s State) Visible() []types.TaskRow {
	query := strings.ToLower(strings.TrimSpace(s.Filter))
	rows := make([]types.TaskRow, 0, len(s.Rows))
	for _, row := range s.Rows {
		if query == "" || strings.Contains(strings.ToLower(row.ID+" "+row.Query+" "+string(row.Status)+" "+string(row.Kind)+" "+row.ParentTaskID+" "+row.GroupID), query) {
			rows = append(rows, row)
		}
	}
	pageSize := max(1, s.PageSize)
	return rows[:min(len(rows), pageSize)]
}

// Summary returns bounded operational text without claiming authoritative cost.
func (s State) Summary(row types.TaskRow) string {
	progress := "progress indeterminate"
	if row.Progress != nil {
		progress = fmt.Sprintf("progress %.0f%%", *row.Progress*100)
	}
	line := fmt.Sprintf("%s · %s · %s · %dms", row.Status, row.Kind, progress, row.DurationMS)
	if row.ParentTaskID != "" {
		line += " · child of " + row.ParentTaskID
	}
	if row.GroupID != "" {
		line += " · group " + row.GroupID
	}
	if row.LastActivityAt.IsZero() {
		line += " · activity latency unavailable"
	}
	return line
}
