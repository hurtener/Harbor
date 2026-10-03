package sessions_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/transfer"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions"
)

func TestCascadeErasure_FencesPausedArtifactWriter(t *testing.T) {
	f := newErasureFixture(t, nil)
	id := identity.Identity{TenantID: "transfer-tenant", UserID: "owner", SessionID: "destination"}
	ctx, err := identity.WithVerified(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.reg.Open(ctx, id.SessionID, id); err != nil {
		t.Fatal(err)
	}
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := transfer.New(transfer.Config{LegacyWritersDrained: true, Audience: "recipient", Epoch: 1, MaxBytes: 1024, Keys: map[string]ed25519.PublicKey{"fixture": public}, Artifacts: f.arts, State: f.store, Bus: f.bus, Clock: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	e, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{Registry: f.reg, State: f.store, Artifacts: f.arts, Skills: f.skills, Bus: f.bus, ArtifactTransfers: svc})
	if err != nil {
		t.Fatal(err)
	}
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	if _, err = f.arts.PutText(ctx, scope, "before deletion", artifacts.PutOpts{}); err != nil {
		t.Fatal(err)
	}
	ready, release, result := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		close(ready)
		<-release
		_, err := f.arts.PutText(context.Background(), scope, "late import", artifacts.PutOpts{})
		result <- err
	}()
	<-ready
	if _, err = e.Erase(ctx, id); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err = <-result; !errors.Is(err, artifacts.ErrScopeFenced) {
		t.Fatalf("late writer escaped erasure %v", err)
	}
	refs, err := f.arts.List(ctx, scope)
	if err != nil || len(refs) != 0 {
		t.Fatalf("erased bytes resurrected %d %v", len(refs), err)
	}
	// Reconstructing the transfer service does not release the store tombstone.
	svc, err = transfer.New(transfer.Config{LegacyWritersDrained: true, Audience: "recipient", Epoch: 1, MaxBytes: 1024, Keys: map[string]ed25519.PublicKey{"fixture": public}, Artifacts: f.arts, State: f.store, Bus: f.bus, Clock: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.FenceSession(ctx, id); err != nil {
		t.Fatal(err)
	}
}
