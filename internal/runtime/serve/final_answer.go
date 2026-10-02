package serve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/protocol"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	turnsprotocol "github.com/hurtener/Harbor/internal/sessions/turns/protocol"
	"github.com/hurtener/Harbor/internal/tasks"
)

// finalAnswerSelector composes two canonical read authorities. It has no model,
// tool, transcript, raw-event, or artifact-write dependency. Per-call selection
// is local, and both shared dependencies provide synchronized reads.
type finalAnswerSelector struct {
	turns *turnsprotocol.Service
	tasks tasks.TaskRegistry
}

// NewFinalAnswerSelector builds the runtime-native exact sealed-answer reader.
// Artifact materialization, erasure fencing, and request replay belong to the
// caller's existing artifact surface; this helper never creates execution.
func NewFinalAnswerSelector(turnsService *turnsprotocol.Service, taskRegistry tasks.TaskRegistry) (protocol.FinalAnswerSelector, error) {
	if turnsService == nil || taskRegistry == nil {
		return nil, errors.New("serve: final answer selector requires turn and task readers")
	}
	return &finalAnswerSelector{turns: turnsService, tasks: taskRegistry}, nil
}

func (s *finalAnswerSelector) SelectFinalAnswer(ctx context.Context, request types.ArtifactsExportAnswerRequest) (protocol.SelectedFinalAnswer, error) {
	if err := ctx.Err(); err != nil {
		return protocol.SelectedFinalAnswer{}, err
	}
	invalid := func() (protocol.SelectedFinalAnswer, error) {
		return protocol.SelectedFinalAnswer{}, protoerrors.New(protoerrors.CodeInvalidRequest, "invalid sealed final answer selector")
	}
	unavailable := func() (protocol.SelectedFinalAnswer, error) {
		return protocol.SelectedFinalAnswer{}, protoerrors.New(protoerrors.CodeNotFound, "exact sealed final answer unavailable")
	}
	mismatch := func() (protocol.SelectedFinalAnswer, error) {
		return protocol.SelectedFinalAnswer{}, protoerrors.New(protoerrors.CodeRevisionConflict, "sealed final answer selector no longer matches")
	}
	for _, value := range []string{request.RequestID, request.TaskID, request.TurnID} {
		if value == "" || len(value) > 255 || !utf8.ValidString(value) || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n") {
			return invalid()
		}
	}
	if request.TurnVersion <= 0 || request.AnswerSequence <= 0 || request.SizeBytes < 0 || request.SizeBytes > turns.MaxInlineAnswerBytes || len(request.SHA256) != 64 || strings.ToLower(request.SHA256) != request.SHA256 {
		return invalid()
	}
	if _, err := hex.DecodeString(request.SHA256); err != nil {
		return invalid()
	}
	if request.Scope.Task != "" && request.Scope.Task != request.TaskID {
		return invalid()
	}
	id, ok := identity.FromVerified(ctx)
	if !ok || identity.Validate(id) != nil || request.Scope.Tenant != id.TenantID || request.Scope.User != id.UserID || request.Scope.Session != id.SessionID {
		return unavailable()
	}

	// This is the existing consumer lane, with exact-session, signed session
	// reach and effective-agent reach. Never substitute raw state or transcript
	// when retention, erasure, or current authority makes the turn unavailable.
	response, err := s.turns.Get(ctx, turnsprotocol.GetRequest{SessionID: id.SessionID, TaskID: request.TaskID, Projection: turnsprotocol.ProjectionConversation})
	if err != nil {
		return unavailable()
	}
	row := response.Turn
	if response.SessionID != id.SessionID || row.SessionID != id.SessionID || row.TaskID != request.TaskID || !row.Sealed || row.Status != turns.StatusComplete || row.Answer.State != turns.AnswerStateInline || row.Answer.Complete != turns.CompletenessComplete || row.Answer.Ref != nil {
		return unavailable()
	}
	if string(row.TurnID) != request.TurnID || row.Version <= 0 || int64(row.Version) != request.TurnVersion || row.Answer.Seq != uint64(request.AnswerSequence) {
		return mismatch()
	}

	taskCtx, err := identity.With(ctx, id)
	if err != nil {
		return unavailable()
	}
	task, err := s.tasks.Get(taskCtx, tasks.TaskID(request.TaskID))
	if err != nil || task == nil || string(task.ID) != request.TaskID || task.Identity.Identity != id || task.Kind != tasks.KindForeground || task.ParentTaskID != nil || task.Status != tasks.StatusComplete || task.Result == nil {
		return unavailable()
	}
	// Task.Identity.RunID is spawn context, not the executed-run authority.
	// The exact Task.ID and owner triple above bind the canonical final result.
	var envelope planner.AnswerEnvelope
	var fields map[string]json.RawMessage
	if json.Unmarshal(task.Result.Value, &envelope) != nil || json.Unmarshal(task.Result.Value, &fields) != nil || len(fields["answer"]) == 0 || string(fields["answer"]) == "null" || envelope.FinishReason != string(planner.FinishGoal) || envelope.Answer != row.Answer.Inline || envelope.IncorporatedInputRevision != task.Result.IncorporatedInputRevision {
		return unavailable()
	}
	answer := []byte(envelope.Answer)
	if !utf8.Valid(answer) {
		return unavailable()
	}
	digest := sha256.Sum256(answer)
	if int64(len(answer)) != request.SizeBytes || hex.EncodeToString(digest[:]) != request.SHA256 {
		return mismatch()
	}
	if err := ctx.Err(); err != nil {
		return protocol.SelectedFinalAnswer{}, err
	}
	return protocol.NewSelectedFinalAnswer(answer, task.Result.IncorporatedInputRevision), nil
}

var _ protocol.FinalAnswerSelector = (*finalAnswerSelector)(nil)
