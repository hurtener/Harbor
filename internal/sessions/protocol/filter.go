package protocol

import (
	"strings"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// filterMatches reports whether row satisfies every axis of f. An empty
// facet slice matches every value on that axis. The filter is applied
// AFTER the identity-scope predicate the Projector enforces — it never
// widens visibility (CLAUDE.md §6).
func filterMatches(f prototypes.SessionFilter, row prototypes.SessionRow) bool {
	if len(f.Statuses) > 0 && !containsStatus(f.Statuses, row.Status) {
		return false
	}
	if len(f.AgentIDs) > 0 && !containsString(f.AgentIDs, row.AgentID) {
		return false
	}
	if len(f.UserIDs) > 0 && !containsString(f.UserIDs, row.UserID) {
		return false
	}
	if len(f.TenantIDs) > 0 && !containsString(f.TenantIDs, row.TenantID) {
		return false
	}
	if f.StartedWindow.From != nil && row.StartedAt.Before(*f.StartedWindow.From) {
		return false
	}
	if f.StartedWindow.To != nil && row.StartedAt.After(*f.StartedWindow.To) {
		return false
	}
	if f.HasIntervention != nil && row.HasPendingIntervention != *f.HasIntervention {
		return false
	}
	if f.HasFailedTask != nil && row.HasFailedTask != *f.HasFailedTask {
		return false
	}
	if f.CostAboveCents != nil && row.TotalCostCents <= *f.CostAboveCents {
		return false
	}
	if q := strings.TrimSpace(f.Query); q != "" && !queryMatches(q, row) {
		return false
	}
	return true
}

// queryMatches reports whether the free-text query substring-matches
// the session id, agent name, agent id, or user. The Service applies
// the query as a post-search refinement — the runtime forwards the
// query to the `search.sessions` index first (D-122 forward-then-filter
// resolution) and this predicate narrows the merged result-set.
func queryMatches(query string, row prototypes.SessionRow) bool {
	q := strings.ToLower(query)
	return strings.Contains(strings.ToLower(row.SessionID), q) ||
		strings.Contains(strings.ToLower(row.AgentName), q) ||
		strings.Contains(strings.ToLower(row.AgentID), q) ||
		strings.Contains(strings.ToLower(row.UserID), q)
}

func containsStatus(set []prototypes.SessionStatus, v prototypes.SessionStatus) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

func containsString(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
