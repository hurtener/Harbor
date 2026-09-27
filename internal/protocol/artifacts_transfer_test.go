package protocol_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestArtifactTransferEnforcesExactRefAndUploadBoundsAtSurface(t *testing.T) {
	s := newArtifactsSurface(t, newInMemStore(t), "inmem")
	scope := types.ArtifactScope{Tenant: "tenant-a", User: "owner-a", Session: "thread-a"}
	first := putFixture(t, s, scope, []byte("first"), types.ArtifactsPutOpts{Namespace: "uploads"})
	second := putFixture(t, s, scope, []byte("second"), types.ArtifactsPutOpts{Namespace: "uploads"})
	read := auth.WithArtifactTransfer(context.Background(), auth.ArtifactTransferProof{Mode: "read", ID: first.ID, MaxBytes: 5})
	if _, err := s.Dispatch(read, methods.MethodArtifactsGet, &types.ArtifactsGetRequest{Scope: scope, ID: first.ID}); err != nil {
		t.Fatal("exact ref read refused", err)
	}
	if _, err := s.Dispatch(read, methods.MethodArtifactsGet, &types.ArtifactsGetRequest{Scope: scope, ID: second.ID}); asProtoError(t, err) != "scope_mismatch" {
		t.Fatal("cross-artifact read accepted")
	}
	tooSmall := auth.WithArtifactTransfer(context.Background(), auth.ArtifactTransferProof{Mode: "read", ID: first.ID, MaxBytes: 4})
	if _, err := s.Dispatch(tooSmall, methods.MethodArtifactsGet, &types.ArtifactsGetRequest{Scope: scope, ID: first.ID}); asProtoError(t, err) != "scope_mismatch" {
		t.Fatal("oversize artifact read accepted")
	}
	write := auth.WithArtifactTransfer(context.Background(), auth.ArtifactTransferProof{Mode: "write", ID: "upload-operation", MaxBytes: 4})
	put := types.ArtifactsPutOpts{Namespace: "upload-operation", Source: types.ArtifactSourceUserUpload}
	if _, err := s.Dispatch(write, methods.MethodArtifactsPut, &types.ArtifactsPutRequest{Scope: scope, Bytes: []byte("abcd"), Opts: put}); err != nil {
		t.Fatal("bounded upload refused", err)
	}
	if _, err := s.Dispatch(write, methods.MethodArtifactsPut, &types.ArtifactsPutRequest{Scope: scope, Bytes: []byte("abcde"), Opts: put}); asProtoError(t, err) != "scope_mismatch" {
		t.Fatal("oversize upload accepted")
	}
	if _, err := s.Dispatch(write, methods.MethodArtifactsPut, &types.ArtifactsPutRequest{Scope: types.ArtifactScope{Tenant: scope.Tenant, User: scope.User, Session: scope.Session, Task: "other-task"}, Bytes: []byte("ab"), Opts: put}); asProtoError(t, err) != "scope_mismatch" {
		t.Fatal("task-attributed browser upload accepted")
	}
	wrongNamespace := put
	wrongNamespace.Namespace = "another-operation"
	if _, err := s.Dispatch(write, methods.MethodArtifactsPut, &types.ArtifactsPutRequest{Scope: scope, Bytes: []byte("ab"), Opts: wrongNamespace}); asProtoError(t, err) != "scope_mismatch" {
		t.Fatal("cross-operation browser upload accepted")
	}
}
