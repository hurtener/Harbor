package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/hurtener/Harbor/internal/artifacts/transfer"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

// SelectedFinalAnswer is internal materialization input, never a wire response.
// The runtime selector independently verifies the exact sealed answer source.
type SelectedFinalAnswer struct {
	bytes    []byte
	revision uint64
}

// FinalAnswerSelector reads an owner-authorized exact sealed final answer only.
type FinalAnswerSelector interface {
	SelectFinalAnswer(context.Context, types.ArtifactsExportAnswerRequest) (SelectedFinalAnswer, error)
}

// NewSelectedFinalAnswer freezes selected bytes and consumed-input provenance.
func NewSelectedFinalAnswer(b []byte, revision uint64) SelectedFinalAnswer {
	return SelectedFinalAnswer{bytes: append([]byte(nil), b...), revision: revision}
}

// Bytes returns a detached copy for runtime-native artifact materialization.
func (s SelectedFinalAnswer) Bytes() []byte { return append([]byte(nil), s.bytes...) }

// IncorporatedInputRevision is the exact sealed output's consumed revision.
func (s SelectedFinalAnswer) IncorporatedInputRevision() uint64 { return s.revision }

func (s *ArtifactsSurface) handleExportAnswer(ctx context.Context, in any) (any, error) {
	if s.transfer == nil || s.finalAnswer == nil {
		return nil, protoerrors.New(protoerrors.CodeUnknownMethod, "sealed final answer export is not configured")
	}
	r, ok := in.(*types.ArtifactsExportAnswerRequest)
	if !ok || r == nil {
		return nil, protoerrors.New(protoerrors.CodeInvalidRequest, "invalid final answer selector")
	}
	selected, err := s.finalAnswer.SelectFinalAnswer(ctx, *r)
	if err != nil {
		var typed *protoerrors.Error
		if errors.As(err, &typed) {
			return nil, typed
		}
		return nil, protoerrors.New(protoerrors.CodeNotFound, "exact sealed final answer unavailable")
	}
	result, err := s.transfer.MaterializeAnswer(ctx, *r, selected.Bytes(), selected.IncorporatedInputRevision())
	if err != nil {
		code := protoerrors.CodeRuntimeError
		if errors.Is(err, transfer.ErrConflict) {
			code = protoerrors.CodeArtifactTransferConflict
		}
		if errors.Is(err, transfer.ErrUnauthorized) || errors.Is(err, transfer.ErrRevoked) {
			code = protoerrors.CodeScopeMismatch
		}
		if errors.Is(err, transfer.ErrInvalid) {
			code = protoerrors.CodeInvalidRequest
		}
		if errors.Is(err, transfer.ErrNotFound) {
			code = protoerrors.CodeNotFound
		}
		return nil, protoerrors.New(code, "final answer export refused or unconfirmed; reuse exact request identity")
	}
	return &result, nil
}

// String projects the selected answer to content-free diagnostic metadata.
func (s SelectedFinalAnswer) String() string {
	return fmt.Sprintf("selected final answer (%d bytes, input revision %d)", len(s.bytes), s.revision)
}

// GoString keeps %#v diagnostics content-free too.
func (s SelectedFinalAnswer) GoString() string { return s.String() }

// LogValue prevents structured diagnostics from exposing selected text.
func (s SelectedFinalAnswer) LogValue() slog.Value { return slog.StringValue(s.String()) }

// MarshalJSON exposes only metadata; selected bytes have no JSON wire route.
func (s SelectedFinalAnswer) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		SizeBytes int    `json:"size_bytes"`
		Revision  uint64 `json:"incorporated_input_revision"`
	}{len(s.bytes), s.revision})
}
