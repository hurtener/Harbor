package protocol_test

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// A participant's direct upload is pinned to its one session and the
// artifacts.uploaded audit names the participant as uploader.
func TestArtifactsPut_ParticipantPinnedAndAudited(t *testing.T) {
	bus := newArtifactsBus(t)
	s, err := protocol.NewArtifactsSurface(protocol.ArtifactsDeps{
		Store: newInMemStore(t), Redactor: patterns.New(), Bus: bus, Clock: artifactsTestClock, DriverName: "inmem",
		MaxBodyBytes: 1 << 20, FetchDefaultMaxBytes: config.DefaultArtifactFetchMaxBytes, FetchHardMaxBytes: config.DefaultArtifactFetchHardMaxBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	partition := identity.Identity{TenantID: "t", UserID: "owner", SessionID: "shared"}
	ctx, err := identity.WithVerified(context.Background(), partition)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = identity.WithParticipant(ctx, identity.Participant{UserID: "writer"})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := bus.Subscribe(context.Background(), events.Filter{Tenant: "t", User: "owner", Session: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()

	put := func(session string) error {
		_, err := s.Dispatch(ctx, methods.MethodArtifactsPut, &types.ArtifactsPutRequest{
			Scope: types.ArtifactScope{Tenant: "t", User: "owner", Session: session}, Bytes: []byte("hello"),
		})
		return err
	}
	if err := put("other-session"); asProtoError(t, err) != protoerrors.CodeScopeMismatch {
		t.Fatalf("participant upload into another session = %v", err)
	}
	if err := put("shared"); err != nil {
		t.Fatalf("participant upload: %v", err)
	}
	select {
	case ev := <-sub.Events():
		payload, ok := ev.Payload.(protocol.ArtifactUploadedPayload)
		if !ok || payload.ParticipantUserID != "writer" {
			t.Fatalf("uploaded audit = %+v", ev.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no artifacts.uploaded event")
	}
}
