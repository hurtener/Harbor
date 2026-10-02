package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/state"
)

type answerRecord struct {
	RequestSHA256 string                              `json:"request_sha256"`
	Response      types.ArtifactsExportAnswerResponse `json:"response"`
}

// MaterializeAnswer stores a runtime-selected final answer as an artifact.
// The selector is an internal trusted source, never request-carried bytes.
// Each request ID is immutable and every write uses the atomic erasure fence.
func (s *Service) MaterializeAnswer(ctx context.Context, r types.ArtifactsExportAnswerRequest, answer []byte, revision uint64) (types.ArtifactsExportAnswerResponse, error) {
	id, ok := identity.FromVerified(ctx)
	if !ok {
		return types.ArtifactsExportAnswerResponse{}, ErrUnauthorized
	}
	e := types.ArtifactTransferEndpoint{Audience: s.cfg.Audience, Epoch: s.cfg.Epoch, Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
	if err := s.scopeOpen(ctx, e); err != nil {
		return types.ArtifactsExportAnswerResponse{}, err
	}
	for _, v := range []string{r.RequestID, r.TaskID, r.TurnID} {
		if v == "" || len(v) > 255 || strings.TrimSpace(v) != v || strings.ContainsAny(v, "\x00\r\n") {
			return types.ArtifactsExportAnswerResponse{}, ErrInvalid
		}
	}
	if r.TurnVersion <= 0 || r.AnswerSequence <= 0 || r.SizeBytes < 0 || r.SizeBytes > s.cfg.MaxBytes || int64(len(answer)) != r.SizeBytes || !utf8.Valid(answer) {
		return types.ArtifactsExportAnswerResponse{}, ErrInvalid
	}
	digest := sha256.Sum256(answer)
	if hex.EncodeToString(digest[:]) != r.SHA256 {
		return types.ArtifactsExportAnswerResponse{}, ErrConflict
	}
	r.Scope = types.ArtifactScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
	encoded, err := json.Marshal(struct {
		Request  types.ArtifactsExportAnswerRequest
		Revision uint64
	}{r, revision})
	if err != nil {
		return types.ArtifactsExportAnswerResponse{}, fmt.Errorf("encode answer selector: %w", err)
	}
	sum := sha256.Sum256(encoded)
	fingerprint := hex.EncodeToString(sum[:])
	q, k := slot(id, "answer", r.RequestID)
	for range 32 {
		if err := s.scopeOpen(ctx, e); err != nil {
			return types.ArtifactsExportAnswerResponse{}, err
		}
		old, err := s.cfg.State.Load(ctx, q, k)
		v := answerRecord{RequestSHA256: fingerprint}
		if err == nil {
			if err = json.Unmarshal(old.Bytes, &v); err != nil {
				return v.Response, fmt.Errorf("decode answer export receipt: %w", err)
			}
			if v.RequestSHA256 != fingerprint {
				return v.Response, ErrConflict
			}
			if v.Response.ArtifactID != "" {
				ref, found, readErr := s.cfg.Artifacts.GetRef(ctx, artifactScope(e), v.Response.ArtifactID)
				if readErr != nil {
					return v.Response, fmt.Errorf("verify answer artifact: %w", readErr)
				}
				if !found {
					return v.Response, ErrNotFound
				}
				if ref.SHA256 != r.SHA256 || ref.SizeBytes != r.SizeBytes {
					return v.Response, ErrConflict
				}
				return v.Response, nil
			}
		} else if !errors.Is(err, state.ErrNotFound) {
			return v.Response, fmt.Errorf("load answer export receipt: %w", err)
		}
		if old.ID == "" {
			b, encodeErr := json.Marshal(v)
			if encodeErr != nil {
				return v.Response, fmt.Errorf("encode answer admission: %w", encodeErr)
			}
			err = s.cfg.State.SaveIf(ctx, []state.SlotExpectation{state.InternalSlotExpectation(q, k, ""), metadataFence(id)}, state.NewInternalRecord(state.NewEventID(), q, k, b))
			if errors.Is(err, state.ErrConditionFailed) {
				continue
			}
			if err != nil {
				return v.Response, fmt.Errorf("admit answer export: %w", err)
			}
			continue
		}
		if auditErr := s.cfg.Bus.Publish(ctx, events.Event{Type: EventTypeAnswerExport, Identity: identity.Quadruple{Identity: id}, OccurredAt: s.cfg.Clock(), Payload: AnswerExportEvent{RequestID: r.RequestID, SelectorSHA256: fingerprint, State: "admitted"}}); auditErr != nil {
			return v.Response, fmt.Errorf("audit final answer export: %w", auditErr)
		}
		ref, err := s.cfg.Artifacts.PutBytes(ctx, artifactScope(e), answer, artifacts.PutOpts{Namespace: "answer-" + fingerprint[:24], MimeType: "text/plain; charset=utf-8", Source: map[string]any{"kind": "sealed_final_answer", "task_id": r.TaskID, "turn_id": r.TurnID, "turn_version": r.TurnVersion, "answer_sequence": r.AnswerSequence, "incorporated_input_revision": revision, "selector_sha256": fingerprint}})
		if err != nil {
			return v.Response, fmt.Errorf("store sealed final answer: %w", err)
		}
		if ref.SHA256 != r.SHA256 || ref.SizeBytes != r.SizeBytes {
			return v.Response, ErrConflict
		}
		v.Response = types.ArtifactsExportAnswerResponse{RequestID: r.RequestID, ArtifactID: ref.ID, SHA256: ref.SHA256, MimeType: ref.MimeType, SizeBytes: ref.SizeBytes, TaskID: r.TaskID, TurnID: r.TurnID, TurnVersion: r.TurnVersion, AnswerSequence: r.AnswerSequence, IncorporatedInputRevision: revision}
		b, err := json.Marshal(v)
		if err != nil {
			return v.Response, fmt.Errorf("encode answer export receipt: %w", err)
		}
		err = s.cfg.State.SaveIf(ctx, []state.SlotExpectation{state.InternalSlotExpectation(q, k, old.ID), metadataFence(id)}, state.NewInternalRecord(state.NewEventID(), q, k, b))
		if errors.Is(err, state.ErrConditionFailed) {
			continue
		}
		if err != nil {
			return v.Response, fmt.Errorf("seal answer export receipt: %w", err)
		}
		return v.Response, nil
	}
	return types.ArtifactsExportAnswerResponse{}, ErrConflict
}

// EventTypeAnswerExport records source-native final-answer materialization.
const EventTypeAnswerExport events.EventType = "artifacts.answer_export"

// AnswerExportEvent records authority only, never final-answer text.
type AnswerExportEvent struct {
	events.SafeSealed
	RequestID      string `json:"request_id"`
	SelectorSHA256 string `json:"selector_sha256"`
	State          string `json:"state"`
}

func init() { events.RegisterEventType(EventTypeAnswerExport) }
