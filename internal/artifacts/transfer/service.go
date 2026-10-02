package transfer

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/state"
)

// EventType records content-free transfer admission, dispatch and completion.
const EventType events.EventType = "artifacts.transfer"

func init() { events.RegisterEventType(EventType) }

// ArtifactTransferEvent is content-free by construction. Purpose and filenames are not emitted.
type ArtifactTransferEvent struct {
	events.SafeSealed
	TransferID  string `json:"transfer_id"`
	GrantSHA256 string `json:"grant_sha256"`
	State       string `json:"state"`
}

// Sender moves bytes only to a boot-trusted destination, or probes its receipt.
// Probe never creates, imports, refreshes, or extends authority.
type Sender interface {
	Send(context.Context, types.ArtifactTransferGrant, []byte) (types.ArtifactTransferReceipt, error)
	Probe(context.Context, types.ArtifactTransferGrant) (types.ArtifactTransferReceipt, error)
}

// Config supplies immutable operator authority and bounded storage seams.
type Config struct {
	// LegacyWritersDrained is required: old shared-store writers cannot honor fences.
	LegacyWritersDrained bool
	Audience             string
	Epoch                uint64
	MaxBytes             int64
	Keys                 map[string]ed25519.PublicKey
	Artifacts            artifacts.ArtifactStore
	State                state.StateStore
	Bus                  events.EventBus
	Sender               Sender
	Clock                func() time.Time
}

// Service uses conditional durable records rather than process-local locks.
// Every instance can safely share its configured stores with other instances.
type Service struct {
	cfg    Config
	fencer artifacts.ScopeFencer
}

type record struct {
	Grant   types.ArtifactTransferGrant   `json:"grant"`
	Receipt types.ArtifactTransferReceipt `json:"receipt"`
}

// New constructs a transfer service; missing authority never becomes a default.
func New(c Config) (*Service, error) {
	if !c.LegacyWritersDrained || c.Audience == "" || c.Epoch == 0 || c.MaxBytes <= 0 || c.MaxBytes > 64<<20 || len(c.Keys) == 0 || c.Artifacts == nil || c.State == nil || c.Bus == nil || c.Clock == nil {
		return nil, ErrInvalid
	}
	keys := make(map[string]ed25519.PublicKey, len(c.Keys))
	for id, key := range c.Keys {
		if id == "" || len(key) != ed25519.PublicKeySize {
			return nil, ErrInvalid
		}
		keys[id] = append(ed25519.PublicKey(nil), key...)
	}
	c.Keys = keys
	fencer, ok := c.Artifacts.(artifacts.ScopeFencer)
	if !ok {
		return nil, fmt.Errorf("%w: artifact driver lacks atomic scope fencing", ErrInvalid)
	}
	return &Service{cfg: c, fencer: fencer}, nil
}

func scope(e types.ArtifactTransferEndpoint) identity.Identity {
	return identity.Identity{TenantID: e.Tenant, UserID: e.User, SessionID: e.Session}
}
func artifactScope(e types.ArtifactTransferEndpoint) artifacts.ArtifactScope {
	return artifacts.ArtifactScope{TenantID: e.Tenant, UserID: e.User, SessionID: e.Session}
}
func slot(id identity.Identity, direction, transferID string) (identity.Quadruple, string) {
	h := sha256.Sum256([]byte(transferID))
	return identity.Quadruple{Identity: id}, state.InternalKindPrefix + "artifact.transfer." + direction + "." + hex.EncodeToString(h[:])
}
func (s *Service) owner(ctx context.Context, e types.ArtifactTransferEndpoint) error {
	id, ok := identity.FromVerified(ctx)
	if !ok || id != scope(e) || e.Audience != s.cfg.Audience || e.Epoch != s.cfg.Epoch {
		return ErrUnauthorized
	}
	return s.scopeOpen(ctx, e)
}
func (s *Service) valid(g types.ArtifactTransferGrant) error {
	if err := verify(g, s.cfg.Keys); err != nil {
		return err
	}
	if g.SizeBytes > s.cfg.MaxBytes {
		return ErrInvalid
	}
	return nil
}
func (s *Service) live(g types.ArtifactTransferGrant) error {
	now := s.cfg.Clock()
	if now.Before(g.IssuedAt) || !now.Before(g.ExpiresAt) {
		return ErrExpired
	}
	return nil
}
func (s *Service) load(ctx context.Context, id identity.Identity, direction, transferID string) (record, state.StateRecord, error) {
	q, k := slot(id, direction, transferID)
	r, err := s.cfg.State.Load(ctx, q, k)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return record{}, r, ErrNotFound
		}
		return record{}, r, fmt.Errorf("load transfer: %w", err)
	}
	var v record
	if err = json.Unmarshal(r.Bytes, &v); err != nil {
		return v, r, fmt.Errorf("decode transfer: %w", err)
	}
	return v, r, nil
}
func (s *Service) save(ctx context.Context, id identity.Identity, direction string, v record, old state.StateRecord) error {
	q, k := slot(id, direction, v.Grant.TransferID)
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode transfer receipt: %w", err)
	}
	err = s.cfg.State.SaveIf(ctx, []state.SlotExpectation{state.InternalSlotExpectation(q, k, old.ID), metadataFence(id)}, state.NewInternalRecord(state.NewEventID(), q, k, b))
	if err != nil {
		return fmt.Errorf("save transfer receipt: %w", err)
	}
	return nil
}
func initial(g types.ArtifactTransferGrant) record {
	return record{Grant: g, Receipt: types.ArtifactTransferReceipt{TransferID: g.TransferID, GrantSHA256: grantHash(g), State: "admitted", Source: g.Source, Destination: g.Destination, SourceArtifactID: g.ArtifactID, SHA256: g.SHA256, MimeType: g.MimeType, SizeBytes: g.SizeBytes, ExpiresAt: g.ExpiresAt}}
}
func matches(v record, g types.ArtifactTransferGrant) error {
	if v.Receipt.GrantSHA256 != grantHash(g) {
		return ErrConflict
	}
	if v.Receipt.State == "revoked" {
		return ErrRevoked
	}
	return nil
}
func (s *Service) audit(ctx context.Context, id identity.Identity, v record, action string) error {
	err := s.cfg.Bus.Publish(ctx, events.Event{Type: EventType, Identity: identity.Quadruple{Identity: id}, OccurredAt: s.cfg.Clock(), Payload: ArtifactTransferEvent{TransferID: v.Grant.TransferID, GrantSHA256: v.Receipt.GrantSHA256, State: action}})
	if err != nil {
		return fmt.Errorf("record transfer audit: %w", err)
	}
	return nil
}

// Prepare ratifies an exact signed import under the current destination owner.
func (s *Service) Prepare(ctx context.Context, g types.ArtifactTransferGrant) (types.ArtifactTransferReceipt, error) {
	if err := s.valid(g); err != nil {
		return types.ArtifactTransferReceipt{}, err
	}
	if err := s.owner(ctx, g.Destination); err != nil {
		return types.ArtifactTransferReceipt{}, err
	}
	id := scope(g.Destination)
	for range 16 {
		v, _, err := s.load(ctx, id, "import", g.TransferID)
		if err == nil {
			if err = matches(v, g); err != nil {
				return v.Receipt, err
			}
			if v.Receipt.State != "completed" {
				if err = s.live(g); err != nil {
					return v.Receipt, err
				}
			}
			return v.Receipt, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return types.ArtifactTransferReceipt{}, err
		}
		if err = s.live(g); err != nil {
			return types.ArtifactTransferReceipt{}, err
		}
		v = initial(g)
		if err = s.audit(ctx, id, v, "admitted"); err != nil {
			return v.Receipt, err
		}
		err = s.save(ctx, id, "import", v, state.StateRecord{})
		if errors.Is(err, state.ErrConditionFailed) {
			continue
		}
		return v.Receipt, err
	}
	return types.ArtifactTransferReceipt{}, ErrConflict
}

// Status retrieves a receipt only for the caller's currently verified owner.
func (s *Service) Status(ctx context.Context, direction, transferID string) (types.ArtifactTransferReceipt, error) {
	if direction != "import" && direction != "export" {
		return types.ArtifactTransferReceipt{}, ErrInvalid
	}
	id, ok := identity.FromVerified(ctx)
	if !ok {
		return types.ArtifactTransferReceipt{}, ErrUnauthorized
	}
	for range 16 {
		v, old, err := s.load(ctx, id, direction, transferID)
		if err == nil && direction == "import" {
			if fenceErr := s.scopeOpen(ctx, v.Grant.Destination); fenceErr != nil {
				return v.Receipt, fenceErr
			}
		}
		if err != nil || direction != "import" || v.Receipt.State != "importing" {
			return v.Receipt, err
		}
		ref, found, err := s.cfg.Artifacts.GetRef(ctx, artifactScope(v.Grant.Destination), destinationID(v.Grant))
		if err != nil {
			return v.Receipt, fmt.Errorf("recover transfer status: %w", err)
		}
		if !found {
			return v.Receipt, nil
		}
		if ref.SHA256 != v.Grant.SHA256 || ref.SizeBytes != v.Grant.SizeBytes || ref.MimeType != v.Grant.MimeType {
			return v.Receipt, ErrConflict
		}
		v.Receipt.State = "completed"
		v.Receipt.DestinationArtifactID = ref.ID
		if err = s.audit(ctx, id, v, "recovered"); err != nil {
			return v.Receipt, err
		}
		err = s.save(ctx, id, direction, v, old)
		if errors.Is(err, state.ErrConditionFailed) {
			continue
		}
		return v.Receipt, err
	}
	return types.ArtifactTransferReceipt{}, ErrConflict
}

// Revoke prevents an undispatched operation. A dispatched operation returns
// ErrTooLate: delivery may already have happened and is never claimed undone.
func (s *Service) Revoke(ctx context.Context, direction, transferID string) (types.ArtifactTransferReceipt, error) {
	if direction != "import" && direction != "export" {
		return types.ArtifactTransferReceipt{}, ErrInvalid
	}
	id, ok := identity.FromVerified(ctx)
	if !ok {
		return types.ArtifactTransferReceipt{}, ErrUnauthorized
	}
	for range 16 {
		v, old, err := s.load(ctx, id, direction, transferID)
		if err != nil {
			return v.Receipt, err
		}
		if v.Receipt.State == "revoked" {
			return v.Receipt, nil
		}
		if v.Receipt.State != "admitted" {
			return v.Receipt, ErrTooLate
		}
		if err = s.audit(ctx, id, v, "revoked"); err != nil {
			return v.Receipt, err
		}
		v.Receipt.State = "revoked"
		err = s.save(ctx, id, direction, v, old)
		if errors.Is(err, state.ErrConditionFailed) {
			continue
		}
		return v.Receipt, err
	}
	return types.ArtifactTransferReceipt{}, ErrConflict
}

// ImportAdmission authenticates the immutable grant and recipient's prior
// ratification BEFORE an HTTP adapter reads the bounded byte body.
func (s *Service) ImportAdmission(ctx context.Context, g types.ArtifactTransferGrant, probe bool) (types.ArtifactTransferReceipt, error) {
	if err := s.valid(g); err != nil {
		return types.ArtifactTransferReceipt{}, err
	}
	if g.Destination.Audience != s.cfg.Audience || g.Destination.Epoch != s.cfg.Epoch {
		return types.ArtifactTransferReceipt{}, ErrUnauthorized
	}
	if err := s.scopeOpen(ctx, g.Destination); err != nil {
		return types.ArtifactTransferReceipt{}, err
	}
	v, _, err := s.load(ctx, scope(g.Destination), "import", g.TransferID)
	if err != nil {
		return v.Receipt, err
	}
	if err = matches(v, g); err != nil {
		return v.Receipt, err
	}
	if !probe && v.Receipt.State != "completed" && v.Receipt.State != "importing" {
		err = s.live(g)
	}
	return v.Receipt, err
}

func destinationID(g types.ArtifactTransferGrant) string {
	return "transfer-" + grantHash(g)[:24] + "_" + g.SHA256[:12]
}

// Import verifies every byte before its first visible store write. A crash
// after PutBytes is recovered from the exact deterministic destination ref;
// recovery after expiry may seal an existing copy but never create a new one.
func (s *Service) Import(ctx context.Context, g types.ArtifactTransferGrant, data []byte) (types.ArtifactTransferReceipt, error) {
	if _, err := s.ImportAdmission(ctx, g, false); err != nil {
		return types.ArtifactTransferReceipt{}, err
	}
	h := sha256.Sum256(data)
	if int64(len(data)) != g.SizeBytes || hex.EncodeToString(h[:]) != g.SHA256 {
		return types.ArtifactTransferReceipt{}, ErrInvalid
	}
	id := scope(g.Destination)
	for range 32 {
		v, old, err := s.load(ctx, id, "import", g.TransferID)
		if err != nil {
			return v.Receipt, err
		}
		if err = matches(v, g); err != nil {
			return v.Receipt, err
		}
		if v.Receipt.State == "completed" {
			return v.Receipt, nil
		}
		if v.Receipt.State == "admitted" {
			if err = s.live(g); err != nil {
				return v.Receipt, err
			}
			if err = s.audit(ctx, id, v, "importing"); err != nil {
				return v.Receipt, err
			}
			v.Receipt.State = "importing"
			err = s.save(ctx, id, "import", v, old)
			if errors.Is(err, state.ErrConditionFailed) {
				continue
			}
			if err != nil {
				return v.Receipt, err
			}
			continue
		}
		if v.Receipt.State != "importing" {
			return v.Receipt, ErrConflict
		}
		ref, found, err := s.cfg.Artifacts.GetRef(ctx, artifactScope(g.Destination), destinationID(g))
		if err != nil {
			return v.Receipt, fmt.Errorf("recover imported ref: %w", err)
		}
		if !found {
			if err = s.live(g); err != nil {
				return v.Receipt, err
			}
			created, e := s.cfg.Artifacts.PutBytes(ctx, artifactScope(g.Destination), data, artifacts.PutOpts{Namespace: "transfer-" + grantHash(g)[:24], MimeType: g.MimeType, Source: map[string]any{"transfer_id": g.TransferID, "grant_sha256": grantHash(g), "source_artifact_id": g.ArtifactID, "source_sha256": g.SHA256}})
			if e != nil {
				return v.Receipt, fmt.Errorf("store imported artifact: %w", e)
			}
			ref = &created
		}
		if ref.ID != destinationID(g) || ref.SHA256 != g.SHA256 || ref.SizeBytes != g.SizeBytes || ref.MimeType != g.MimeType {
			return v.Receipt, ErrConflict
		}
		v.Receipt.State = "completed"
		v.Receipt.DestinationArtifactID = ref.ID
		if err = s.audit(ctx, id, v, "completed"); err != nil {
			return v.Receipt, err
		}
		err = s.save(ctx, id, "import", v, old)
		if errors.Is(err, state.ErrConditionFailed) {
			continue
		}
		return v.Receipt, err
	}
	return types.ArtifactTransferReceipt{}, ErrConflict
}

// Transfer exports only the current source owner's exact immutable artifact.
// The sender cannot choose URLs, expand a grant, or copy a different artifact.
func (s *Service) Transfer(ctx context.Context, g types.ArtifactTransferGrant) (types.ArtifactTransferReceipt, error) {
	if err := s.valid(g); err != nil {
		return types.ArtifactTransferReceipt{}, err
	}
	if err := s.owner(ctx, g.Source); err != nil {
		return types.ArtifactTransferReceipt{}, err
	}
	if s.cfg.Sender == nil {
		return types.ArtifactTransferReceipt{}, ErrUnauthorized
	}
	id := scope(g.Source)
	for range 32 {
		v, old, err := s.load(ctx, id, "export", g.TransferID)
		if errors.Is(err, ErrNotFound) {
			if err = s.live(g); err != nil {
				return v.Receipt, err
			}
			v = initial(g)
			err = s.save(ctx, id, "export", v, state.StateRecord{})
			if errors.Is(err, state.ErrConditionFailed) {
				continue
			}
			if err != nil {
				return v.Receipt, err
			}
			continue
		}
		if err != nil {
			return v.Receipt, err
		}
		if err = matches(v, g); err != nil {
			return v.Receipt, err
		}
		if v.Receipt.State == "completed" {
			return v.Receipt, nil
		}
		remote, probeErr := s.cfg.Sender.Probe(ctx, g)
		if probeErr != nil {
			return v.Receipt, fmt.Errorf("probe admitted destination: %w", probeErr)
		}
		if remote.GrantSHA256 != grantHash(g) {
			return v.Receipt, ErrConflict
		}
		if remote.State == "completed" {
			if remote.DestinationArtifactID != destinationID(g) || remote.SHA256 != g.SHA256 || remote.Source != g.Source || remote.Destination != g.Destination || remote.SizeBytes != g.SizeBytes || remote.MimeType != g.MimeType || remote.TransferID != g.TransferID || remote.SourceArtifactID != g.ArtifactID || !remote.ExpiresAt.Equal(g.ExpiresAt) {
				return v.Receipt, ErrConflict
			}
			v.Receipt = remote
			err = s.save(ctx, id, "export", v, old)
			if errors.Is(err, state.ErrConditionFailed) {
				continue
			}
			return v.Receipt, err
		}
		if remote.State != "admitted" && remote.State != "importing" {
			return v.Receipt, ErrRevoked
		}
		if err = s.live(g); err != nil {
			return v.Receipt, err
		}
		ref, found, err := s.cfg.Artifacts.GetRef(ctx, artifactScope(g.Source), g.ArtifactID)
		if err != nil {
			return v.Receipt, fmt.Errorf("read source ref: %w", err)
		}
		if !found {
			return v.Receipt, ErrNotFound
		}
		if ref.SHA256 != g.SHA256 || ref.SizeBytes != g.SizeBytes || ref.MimeType != g.MimeType {
			return v.Receipt, ErrConflict
		}
		data, found, err := s.cfg.Artifacts.Get(ctx, artifactScope(g.Source), g.ArtifactID)
		if err != nil {
			return v.Receipt, fmt.Errorf("read source artifact: %w", err)
		}
		if !found {
			return v.Receipt, ErrNotFound
		}
		h := sha256.Sum256(data)
		if int64(len(data)) != g.SizeBytes || hex.EncodeToString(h[:]) != g.SHA256 {
			return v.Receipt, ErrConflict
		}
		if v.Receipt.State == "admitted" {
			v.Receipt.State = "exporting"
			err = s.save(ctx, id, "export", v, old)
			if errors.Is(err, state.ErrConditionFailed) {
				continue
			}
			if err != nil {
				return v.Receipt, err
			}
			continue
		}
		if v.Receipt.State != "exporting" {
			return v.Receipt, ErrConflict
		}
		if err = s.audit(ctx, id, v, "exporting"); err != nil {
			return v.Receipt, err
		}
		if err = s.live(g); err != nil {
			return v.Receipt, err
		}
		remote, err = s.cfg.Sender.Send(ctx, g, data)
		if err != nil {
			return v.Receipt, fmt.Errorf("send artifact directly: %w", err)
		}
		if remote.State != "completed" || remote.GrantSHA256 != grantHash(g) || remote.DestinationArtifactID != destinationID(g) || remote.SHA256 != g.SHA256 || remote.Source != g.Source || remote.Destination != g.Destination || remote.SizeBytes != g.SizeBytes || remote.MimeType != g.MimeType || remote.TransferID != g.TransferID || remote.SourceArtifactID != g.ArtifactID || !remote.ExpiresAt.Equal(g.ExpiresAt) {
			return v.Receipt, ErrConflict
		}
		v.Receipt = remote
		err = s.save(ctx, id, "export", v, old)
		if errors.Is(err, state.ErrConditionFailed) {
			continue
		}
		return v.Receipt, err
	}
	return types.ArtifactTransferReceipt{}, ErrConflict
}
