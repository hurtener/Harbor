package turns

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
)

// maxAppCheckPages bounds one SessionHasApp walk. A store holds at most its
// retention bound of rows per session, so the bound is a defence against a
// misbehaving driver, never a truncation of a real session.
const maxAppCheckPages = 1024

// ErrAppCheckUnbounded is returned when a session's pages exceed the walk
// bound; the caller refuses rather than guessing.
var ErrAppCheckUnbounded = errors.New("turns: session app check exceeded its page bound")

// AppChecker answers "did a turn of this session record exactly this App?"
// over the turn projection (D-510). It reads only the caller's own session
// partition and is safe for concurrent use.
type AppChecker struct {
	Store Store
}

// SessionHasApp reports whether a retained turn of id's session carries an
// App reference with exactly (agentID, serverID, resourceURI).
func (c AppChecker) SessionHasApp(ctx context.Context, id identity.Identity, agentID, serverID, resourceURI string) (bool, error) {
	if c.Store == nil {
		return false, fmt.Errorf("turns: app checker has no store")
	}
	want := AppRefKey{EffectiveAgentID: agentID, ServerID: serverID, ResourceURI: resourceURI}
	if agentID == "" || serverID == "" || resourceURI == "" {
		return false, nil
	}
	var before *Cursor
	for range maxAppCheckPages {
		rows, next, _, err := c.Store.ListTurns(ctx, id, before, MaxListLimit)
		if err != nil {
			return false, fmt.Errorf("turns: list session turns: %w", err)
		}
		for _, row := range rows {
			for _, app := range row.Apps {
				if app.Key() == want {
					return true, nil
				}
			}
		}
		if next == nil {
			return false, nil
		}
		before = next
	}
	return false, ErrAppCheckUnbounded
}
