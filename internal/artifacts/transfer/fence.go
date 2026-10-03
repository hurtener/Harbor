package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/state"
)

func (s *Service) scopeOpen(ctx context.Context, e types.ArtifactTransferEndpoint) error {
	witness := metadataFence(scope(e))
	if _, err := s.cfg.State.Load(ctx, witness.Identity, witness.Kind); err == nil {
		return ErrRevoked
	} else if !errors.Is(err, state.ErrNotFound) {
		return fmt.Errorf("read transfer erasure witness: %w", err)
	}

	fenced, err := s.fencer.ScopeFenced(ctx, artifactScope(e))
	if err != nil {
		return fmt.Errorf("check artifact scope fence: %w", err)
	}
	if fenced {
		return ErrRevoked
	}
	return nil
}

// FenceSession is the erasure consumer. The artifact driver's own atomic byte
// transaction serializes the tombstone with every Put, including ordinary
// writers. After this returns, no delayed import can resurrect erased bytes.
func (s *Service) FenceSession(ctx context.Context, id identity.Identity) error {
	return (&ErasureFence{store: s.cfg.State, fencer: s.fencer}).FenceSession(ctx, id)
}

// ErasureFence is the storage-only erasure integration. It needs no transfer
// issuer configuration and remains active after transfers are disabled.
type ErasureFence struct {
	store  state.StateStore
	fencer artifacts.ScopeFencer
}

// NewErasureFence binds the metadata witness and atomic byte-store fence.
func NewErasureFence(st state.StateStore, arts artifacts.ArtifactStore) (*ErasureFence, error) {
	f, ok := arts.(artifacts.ScopeFencer)
	if !ok || st == nil {
		return nil, ErrInvalid
	}
	return &ErasureFence{store: st, fencer: f}, nil
}

// FenceSession permanently blocks both receipt resurrection and late bytes.
func (s *ErasureFence) FenceSession(ctx context.Context, id identity.Identity) error {
	if err := identity.Validate(id); err != nil {
		return err
	}

	witness := metadataFence(id)
	// The global, content-free witness survives DeleteScope. Every receipt
	// SaveIf checks its absence in the SAME transaction as its own mutation.
	if _, err := s.store.Load(ctx, witness.Identity, witness.Kind); errors.Is(err, state.ErrNotFound) {
		err = s.store.SaveIf(ctx, []state.SlotExpectation{witness}, state.NewInternalRecord(state.NewEventID(), witness.Identity, witness.Kind, []byte(`{"fenced":true}`)))
		if err != nil && !errors.Is(err, state.ErrConditionFailed) {
			return fmt.Errorf("fence transfer metadata: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("read transfer metadata fence: %w", err)
	}
	if err := s.fencer.FenceScope(ctx, artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}); err != nil {
		return fmt.Errorf("fence artifact transfers before erasure: %w", err)
	}
	return nil
}

func metadataFence(id identity.Identity) state.SlotExpectation {
	raw := fmt.Sprintf("%d:%s%d:%s%d:%s", len(id.TenantID), id.TenantID, len(id.UserID), id.UserID, len(id.SessionID), id.SessionID)
	sum := sha256.Sum256([]byte(raw))
	return state.InternalSlotExpectation(identity.InternalCoordinationQuadruple(), state.InternalKindPrefix+"artifact.transfer.erased/"+hex.EncodeToString(sum[:]), "")
}
