// Package sessionpicker implements generation-fenced session selection.
package sessionpicker

import (
	"sort"
	"strings"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

// Request identifies one async picker query. Results apply only to the exact generation and scope.
type Request struct {
	Generation uint64
	Identity   types.IdentityScope
	Query      string
	Cursor     string
}

// Model is a value-updated one-active-session selector.
type Model struct {
	identity   types.IdentityScope
	query      string
	rows       []types.SessionRow
	current    string
	generation uint64
	nextCursor string
	truncated  bool
}

// New constructs a picker for one tenant/user authority.
func New(identity types.IdentityScope, current string) Model {
	return Model{identity: identity, current: current}
}

// Search begins a fenced request.
func (m Model) Search(query string) (Model, Request) {
	m.query = strings.TrimSpace(query)
	m.generation++
	return m, Request{Generation: m.generation, Identity: m.identity, Query: m.query}
}

// Apply ignores stale or cross-scope responses and merges the current row deterministically.
func (m Model) Apply(request Request, response types.SessionsListResponse) Model {
	if !m.IsCurrent(request) {
		return m
	}
	m.rows = append([]types.SessionRow(nil), response.Rows...)
	sort.SliceStable(m.rows, func(i, j int) bool { return m.rows[i].LastActivityAt.After(m.rows[j].LastActivityAt) })
	m.nextCursor = response.NextCursor
	m.truncated = response.Truncated
	return m
}

// IsCurrent reports whether an async response may update both picker data and dialog state.
func (m Model) IsCurrent(request Request) bool {
	return request.Generation == m.generation && request.Query == m.query && request.Identity.Tenant == m.identity.Tenant && request.Identity.User == m.identity.User
}

// Rows returns isolated visible session metadata.
func (m Model) Rows() []types.SessionRow { return append([]types.SessionRow(nil), m.rows...) }

// Select returns a complete target triple without mutating active state.
func (m Model) Select(session string) (types.IdentityScope, bool) {
	for _, row := range m.rows {
		if row.SessionID == session {
			return types.IdentityScope{Tenant: m.identity.Tenant, User: m.identity.User, Session: session}, true
		}
	}
	return types.IdentityScope{}, false
}

// SetCurrent is called only after stream drain, target auth, hydrate, and attach complete.
func (m Model) SetCurrent(session string) Model { m.current = session; return m }
