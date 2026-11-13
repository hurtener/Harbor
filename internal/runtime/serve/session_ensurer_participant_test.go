package serve

import (
	"context"
	"errors"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
)

// A participant may take part only in a session that already exists in the
// owner's partition; it never creates one there.
func TestSessionEnsurerAdapter_ParticipantRequiresExistingSession(t *testing.T) {
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	reg, err := sessions.New(st, config.SessionsConfig{IdleTTL: 24 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: 15 * time.Minute}, mkDriverTestBus(t, auditpatterns.New()))
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.CloseRegistry(context.Background()) })
	ad := NewSessionEnsurerAdapter(reg)

	shared := identity.Identity{TenantID: "t", UserID: "owner", SessionID: "shared"}
	guest, err := identity.WithParticipant(context.Background(), identity.Participant{UserID: "writer"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ad.EnsureSession(guest, shared); !errors.Is(err, protocol.ErrSessionParticipantNoSession) {
		t.Fatalf("guest on a missing session = %v, want ErrSessionParticipantNoSession", err)
	}
	if err := reg.RequireExisting(context.Background(), shared); !errors.Is(err, sessions.ErrSessionNotFound) {
		t.Fatalf("the refused guest start must not have created the session: %v", err)
	}
	if err := ad.EnsureSession(context.Background(), shared); err != nil {
		t.Fatalf("owner create: %v", err)
	}
	if err := ad.EnsureSession(guest, shared); err != nil {
		t.Fatalf("guest on the existing session: %v", err)
	}
}
