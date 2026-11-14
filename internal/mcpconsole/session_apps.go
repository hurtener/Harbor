package mcpconsole

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// sessionAppKindPrefix keys the D-510 session App ledger records. Each
// record is the bare fact "this session rendered App (agent, server,
// resource)"; it carries no content and lives in the session's own state
// partition, so session erasure removes it with the rest of the session.
const sessionAppKindPrefix = "mcp.apps.session_app"

var _ mcp.SessionAppRecorder = (*ToolContextStore)(nil)

func sessionAppKind(agentID, serverID, resourceURI string) string {
	sum := sha256.Sum256([]byte(agentID + "\x00" + serverID + "\x00" + resourceURI))
	return sessionAppKindPrefix + "/" + hex.EncodeToString(sum[:])
}

// RecordSessionApp records that the ctx identity's session rendered the App.
// The provider calls it before publishing the render event; the Apps surface
// calls it for an App returned by an admitted App callback (nested App).
func (s *ToolContextStore) RecordSessionApp(ctx context.Context, agentID, serverID, resourceURI string) error {
	id, ok := identity.From(ctx)
	if !ok {
		return fmt.Errorf("mcpconsole: record session app: %w", mcp.ErrIdentityMissing)
	}
	if agentID == "" || serverID == "" || resourceURI == "" {
		return nil
	}
	rec := state.StateRecord{
		ID:        state.NewEventID(),
		Identity:  identity.Quadruple{Identity: id},
		Kind:      sessionAppKind(agentID, serverID, resourceURI),
		Bytes:     []byte("{}"),
		UpdatedAt: s.clock(),
	}
	if err := s.state.Save(ctx, rec); err != nil {
		return fmt.Errorf("mcpconsole: persist session app: %w", err)
	}
	return nil
}

// SessionHasApp reports whether id's session recorded exactly this App.
func (s *ToolContextStore) SessionHasApp(ctx context.Context, id identity.Identity, agentID, serverID, resourceURI string) (bool, error) {
	if agentID == "" || serverID == "" || resourceURI == "" {
		return false, nil
	}
	_, err := s.state.Load(ctx, identity.Quadruple{Identity: id}, sessionAppKind(agentID, serverID, resourceURI))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, state.ErrNotFound) {
		return false, nil
	}
	return false, fmt.Errorf("mcpconsole: load session app: %w", err)
}
