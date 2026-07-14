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
	// f.AgentIDs is rejected loud at the Service edge (protocol.go — the
	// session→agent binding is unpopulated in V1), so it never reaches
	// filterMatches. No AgentID predicate here.
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
// query to the `search.sessions` index first (forward-then-filter
// resolution) and this predicate narrows the merged result-set.
//
// WARN-4: the agent sub-fields are nullable and unpopulated in V1. A nil
// AgentID / AgentName honestly NEVER-matches the query term (nil is not a
// substring of anything) — the whole query is NEVER failed loud for
// touching them; it still matches the populated session_id / user_id so
// working id / user search is preserved.
func queryMatches(query string, row prototypes.SessionRow) bool {
	q := strings.ToLower(query)
	if strings.Contains(strings.ToLower(row.SessionID), q) ||
		strings.Contains(strings.ToLower(row.UserID), q) {
		return true
	}
	if row.AgentName != nil && strings.Contains(strings.ToLower(*row.AgentName), q) {
		return true
	}
	if row.AgentID != nil && strings.Contains(strings.ToLower(*row.AgentID), q) {
		return true
	}
	return false
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
