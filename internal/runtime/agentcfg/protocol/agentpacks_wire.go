package protocol

// agentpacks_wire.go is the narrow adapter between the runtime's domain
// inspection/copy port and the versioned Protocol wire shapes. Authorization
// remains in internal/protocol: this adapter only projects already-verified
// requests and maps runtime sentinels to Protocol's closed error classes.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/hurtener/Harbor/internal/agentcfg"
	harborprotocol "github.com/hurtener/Harbor/internal/protocol"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

const (
	emptyBootPackSetHashEnvelope = "boot-pack-set-v1\x00"
	emptyCompositionHashEnvelope = "operator-tier-v1\x00"
)

// Inspect implements harborprotocol.AgentPacksAdminPort. The context is
// expected to carry the verified identity established by the Protocol surface;
// the request's identity is intentionally not used as an authority source.
func (s *Service) Inspect(ctx context.Context, req prototypes.AgentConfigAgentPacksInspectRequest) (prototypes.AgentConfigAgentPacksInspectResponse, error) {
	view, err := s.InspectEffective(ctx, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigAgentPacksInspectResponse{}, err
	}
	return agentPackInspectionToWire(view), nil
}

// Copy implements harborprotocol.AgentPacksAdminPort. The runtime owns the
// durable receipt and target CAS; the adapter preserves the wire request's
// mandatory composition expectations and returns one outcome per selection.
func (s *Service) Copy(ctx context.Context, req prototypes.AgentConfigAgentPacksCopyRequest) (prototypes.AgentConfigAgentPacksCopyResponse, error) {
	result, err := s.CopySelected(ctx, AgentPackCopyRequest{
		SourceAgentID:                 req.SourceAgentID,
		TargetAgentID:                 req.TargetAgentID,
		PackIDs:                       append([]string(nil), req.PackIDs...),
		ExpectedSourceCompositionHash: coreCompositionHash(req.ExpectedSourceCompositionHash),
		ExpectedTargetCompositionHash: coreCompositionHash(req.ExpectedTargetCompositionHash),
		IdempotencyKey:                req.IdempotencyKey,
	})
	if err != nil {
		return prototypes.AgentConfigAgentPacksCopyResponse{}, err
	}
	out := prototypes.AgentConfigAgentPacksCopyResponse{
		SourceAgentID:   result.SourceAgentID,
		TargetAgentID:   result.TargetAgentID,
		CompositionHash: wireCompositionHash(result.TargetCompositionHash, result.Target.Items),
		BootPackSetHash: wireBootPackSetHash(result.Target.BootPackSetHash, result.Target.BootItems),
		ProtocolVersion: prototypes.ProtocolVersion,
		Outcomes:        make([]prototypes.AgentConfigAgentPackCopyOutcome, 0, len(result.Outcomes)),
	}
	for _, outcome := range result.Outcomes {
		out.Outcomes = append(out.Outcomes, prototypes.AgentConfigAgentPackCopyOutcome{
			PackID:  outcome.PackID,
			Outcome: string(outcome.Outcome),
		})
	}
	return out, nil
}

// ClassifyAgentPackError maps the runtime's stable sentinels to the closed
// classes consumed by internal/protocol.AgentPacksSurface. Keep this mapping
// here, next to the runtime sentinels, so the Protocol package never imports a
// concrete storage or service implementation.
func ClassifyAgentPackError(err error) harborprotocol.AgentPacksErrorCode {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrAgentPackCopyIdempotencyConflict):
		return harborprotocol.AgentPacksErrorIdempotencyConflict
	case errors.Is(err, ErrAgentPackCopyCollision), errors.Is(err, ErrBootPackOwned):
		return harborprotocol.AgentPacksErrorConflict
	case errors.Is(err, ErrAgentPackCopyExpectation), errors.Is(err, agentcfg.ErrRevisionConflict), errors.Is(err, ErrAgentPackCopyReplay):
		return harborprotocol.AgentPacksErrorStale
	case errors.Is(err, ErrAgentPackNotFound), errors.Is(err, agentcfg.ErrRevisionNotFound):
		return harborprotocol.AgentPacksErrorNotFound
	case errors.Is(err, ErrAgentPackInspectionIdentityRequired), errors.Is(err, ErrAgentPackCopyInvalid), errors.Is(err, ErrAgentPacksInvalid), errors.Is(err, agentcfg.ErrInvalidPayload):
		return harborprotocol.AgentPacksErrorInvalid
	default:
		return harborprotocol.AgentPacksErrorUnavailable
	}
}

var _ harborprotocol.AgentPacksAdminPort = (*Service)(nil)

func agentPackInspectionToWire(view AgentPackInspection) prototypes.AgentConfigAgentPacksInspectResponse {
	out := prototypes.AgentConfigAgentPacksInspectResponse{
		AgentID:         view.AgentID,
		BootPackSetHash: wireBootPackSetHash(view.BootPackSetHash, view.BootItems),
		CompositionHash: wireCompositionHash(view.CompositionHash, view.Items),
		ProtocolVersion: prototypes.ProtocolVersion,
		BootPacks:       make([]prototypes.AgentConfigAgentPackInspection, 0, len(view.BootItems)),
		RevisionPacks:   make([]prototypes.AgentConfigAgentPackInspection, 0, len(view.RevisionItems)),
		EffectivePacks:  make([]prototypes.AgentConfigAgentPackInspection, 0, len(view.Items)),
	}
	for _, item := range view.BootItems {
		out.BootPacks = append(out.BootPacks, agentPackLayerToWire(item, "boot"))
	}
	for _, item := range view.RevisionItems {
		out.RevisionPacks = append(out.RevisionPacks, agentPackLayerToWire(item, "revision"))
	}
	for _, item := range view.Items {
		out.EffectivePacks = append(out.EffectivePacks, prototypes.AgentConfigAgentPackInspection{
			PackID:       item.PackID,
			Pack:         agentPackItemToWire(item.Item),
			Source:       string(item.Source),
			SemanticHash: item.SemanticHash,
			Editable:     item.Editable,
		})
	}
	return out
}

func agentPackLayerToWire(item AgentPackLayerItem, source string) prototypes.AgentConfigAgentPackInspection {
	return prototypes.AgentConfigAgentPackInspection{
		PackID:       item.PackID,
		Pack:         agentPackItemToWire(item.Item),
		Source:       source,
		SemanticHash: item.SemanticHash,
		Editable:     source == "revision",
	}
}

func coreCompositionHash(value string) string {
	if value == emptyHash(emptyCompositionHashEnvelope) {
		return ""
	}
	return value
}

func wireCompositionHash(value string, items []AgentPackEffectiveItem) string {
	if value != "" {
		return value
	}
	if len(items) == 0 {
		return emptyHash(emptyCompositionHashEnvelope)
	}
	return value
}

func wireBootPackSetHash(value string, items []AgentPackLayerItem) string {
	if value != "" {
		return value
	}
	if len(items) == 0 {
		return emptyHash(emptyBootPackSetHashEnvelope)
	}
	return value
}

func emptyHash(envelope string) string {
	sum := sha256.Sum256([]byte(envelope))
	return hex.EncodeToString(sum[:])
}
