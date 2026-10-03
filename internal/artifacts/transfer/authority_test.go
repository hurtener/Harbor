package transfer_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/transfer"
)

func TestTransfer_BindingSubstitutionAndPolicyEpoch(t *testing.T) {
	f := newFixture(t)
	g, data := f.grant(t, 55)
	if _, err := f.target.Prepare(owner(t, g.Destination), g); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"audience", "tenant", "user", "session", "digest", "mime", "size", "epoch", "source"} {
		t.Run(field, func(t *testing.T) {
			bad := g
			switch field {
			case "audience":
				bad.Destination.Audience = "other"
			case "tenant":
				bad.Destination.Tenant = "other"
			case "user":
				bad.Destination.User = "other"
			case "session":
				bad.Destination.Session = "other"
			case "digest":
				bad.SHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			case "mime":
				bad.MimeType = "text/plain"
			case "size":
				bad.SizeBytes++
			case "epoch":
				bad.Destination.Epoch++
			case "source":
				bad.Source.Session = "other"
			}
			signed, err := transfer.Sign(bad, f.key)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.target.Import(context.Background(), signed, data); err == nil {
				t.Fatal("substitution admitted")
			}
		})
	}
	bad := g
	bad.Purpose = "tampered"
	if _, err := f.target.Prepare(owner(t, g.Destination), bad); !errors.Is(err, transfer.ErrUnauthorized) {
		t.Fatalf("tampered signature %v", err)
	}
	rotated := f.targetCfg
	rotated.Epoch++
	svc, err := transfer.New(rotated)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Import(t.Context(), g, data); !errors.Is(err, transfer.ErrUnauthorized) {
		t.Fatalf("old epoch accepted %v", err)
	}
	refs, err := f.targetCfg.Artifacts.List(t.Context(), artifacts.ArtifactScope{TenantID: g.Destination.Tenant})
	if err != nil || len(refs) != 0 {
		t.Fatalf("unauthorized bytes visible %d %v", len(refs), err)
	}
}

func TestTransfer_DeletedSourceAndDeclaredLimitFailBeforeDelivery(t *testing.T) {
	f := newFixture(t)
	g, _ := f.grant(t, 56)
	if _, err := f.target.Prepare(owner(t, g.Destination), g); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sourceCfg.Artifacts.Delete(t.Context(), artifacts.ArtifactScope{TenantID: g.Source.Tenant, UserID: g.Source.User, SessionID: g.Source.Session}, g.ArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.source.Transfer(owner(t, g.Source), g); !errors.Is(err, transfer.ErrNotFound) {
		t.Fatalf("deleted source %v", err)
	}
	bad := g
	bad.TransferID = "oversize"
	bad.SizeBytes = f.targetCfg.MaxBytes + 1
	bad, err := transfer.Sign(bad, f.key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.target.Prepare(owner(t, bad.Destination), bad); !errors.Is(err, transfer.ErrInvalid) {
		t.Fatalf("oversize admission %v", err)
	}
	refs, err := f.targetCfg.Artifacts.List(t.Context(), artifacts.ArtifactScope{TenantID: g.Destination.Tenant})
	if err != nil || len(refs) != 0 {
		t.Fatalf("bytes delivered %d %v", len(refs), err)
	}
}
