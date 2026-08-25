package stream

import (
	"github.com/hurtener/Harbor/internal/protocol/types"
	turns "github.com/hurtener/Harbor/internal/sessions/turns"
	turnsprotocol "github.com/hurtener/Harbor/internal/sessions/turns/protocol"
)

// session-turns domain→wire projection helpers. The wire shapes
// (internal/protocol/types/session_turns.go) are flat mirrors of the
// turn projection; every converter deep-copies slices and pointer
// fields so the handler never leaks a mutable domain value onto the
// wire or a wire value into the domain.

// projectSessionTurnsList maps the service's list response onto the
// wire. The explicit page-completeness and the counter-status contract
// ride verbatim: RemainingOlderCount is carried ONLY when exact, and
// PageCompleteness is always explicit (never a fabricated empty).
func projectSessionTurnsList(resp turnsprotocol.ListResponse) types.SessionTurnsListResponse {
	turnsOut := make([]types.SessionTurnRow, 0, len(resp.Turns))
	for _, row := range resp.Turns {
		turnsOut = append(turnsOut, projectSessionTurnRow(row))
	}
	return types.SessionTurnsListResponse{
		Header: types.SessionTurnHeader{
			SessionID:  resp.Header.SessionID,
			SnapshotID: resp.Header.SnapshotID,
			AsOf:       resp.Header.AsOf,
		},
		Turns:               turnsOut,
		Order:               string(resp.Order),
		NextOlderCursor:     resp.NextOlderCursor,
		HasMore:             resp.HasMore,
		RemainingOlderCount: resp.RemainingOlderCount,
		CountExact:          resp.CountExact,
		LiveResumeSeq:       resp.LiveResumeSeq,
		PageCompleteness:    string(resp.PageCompleteness),
		PartialReason:       resp.PartialReason,
		ProtocolVersion:     types.ProtocolVersion,
	}
}

// projectSessionTurnsGet maps the service's get response onto the wire:
// exactly one of Turn / OpsTurn / UsageTurn is populated, per the request's
// projection lane.
func projectSessionTurnsGet(resp turnsprotocol.GetResponse) types.SessionTurnsGetResponse {
	out := types.SessionTurnsGetResponse{
		SessionID:       resp.SessionID,
		ProtocolVersion: types.ProtocolVersion,
	}
	switch {
	case resp.OpsTurn != nil:
		ops := projectSessionOpsTurnRow(*resp.OpsTurn)
		out.OpsTurn = &ops
	case resp.UsageTurn != nil:
		usage := projectSessionUsageTurnRow(*resp.UsageTurn)
		out.UsageTurn = &usage
	default:
		row := projectSessionTurnRow(resp.Turn)
		out.Turn = &row
	}
	return out
}

// projectSessionUsageTurnRow maps the structurally content-free consumer
// usage DTO onto the wire. It cannot project conversation/operations content
// because UsageTurnRow has no such fields.
func projectSessionUsageTurnRow(row turnsprotocol.UsageTurnRow) types.SessionUsageTurnRow {
	return types.SessionUsageTurnRow{
		TurnID:              string(row.TurnID),
		TaskID:              row.TaskID,
		SessionID:           row.SessionID,
		AgentID:             row.AgentID,
		Status:              string(row.Status),
		Sealed:              row.Sealed,
		Version:             row.Version,
		LastAppliedEventSeq: row.LastAppliedEventSeq,
		StartedAt:           row.StartedAt,
		UpdatedAt:           row.UpdatedAt,
		FinishedAt:          row.FinishedAt,
		Usage:               projectSessionTurnUsage(row.Usage),
	}
}

// projectSessionTurnRow maps one consumer turn row onto the wire. Every
// slice and pointer is deep-copied.
func projectSessionTurnRow(row turns.TurnRow) types.SessionTurnRow {
	inputs := make([]types.SessionTurnAttachment, 0, len(row.Inputs))
	for _, a := range row.Inputs {
		inputs = append(inputs, projectSessionTurnAttachment(a))
	}
	outputs := make([]types.SessionTurnAttachment, 0, len(row.Outputs))
	for _, a := range row.Outputs {
		outputs = append(outputs, projectSessionTurnAttachment(a))
	}
	apps := make([]types.SessionTurnAppRef, 0, len(row.Apps))
	for _, a := range row.Apps {
		apps = append(apps, projectSessionTurnAppRef(a))
	}
	return types.SessionTurnRow{
		TurnID:              string(row.TurnID),
		TaskID:              row.TaskID,
		RunID:               row.RunID,
		SessionID:           row.SessionID,
		Sequence:            int64(row.Sequence),
		TieBreaker:          string(row.TieBreaker),
		Status:              string(row.Status),
		Sealed:              row.Sealed,
		Version:             row.Version,
		LastAppliedEventSeq: row.LastAppliedEventSeq,
		StartedAt:           row.StartedAt,
		UpdatedAt:           row.UpdatedAt,
		FinishedAt:          row.FinishedAt,
		FinishReason:        string(row.FinishReason),
		ErrorClass:          string(row.ErrorClass),
		FinishMessage:       row.FinishMessage,
		ErrorMessage:        row.ErrorMessage,
		Agent:               projectSessionTurnAgent(row.Agent),
		Query:               projectSessionTurnQuery(row.Query),
		Answer:              projectSessionTurnAnswer(row.Answer),
		Pause:               projectSessionTurnPause(row.Pause),
		Inputs:              inputs,
		Outputs:             outputs,
		Usage:               projectSessionTurnUsage(row.Usage),
		Reasoning:           projectSessionTurnReasoning(row.Reasoning),
		Activity:            projectSessionTurnActivity(row.Activity),
		Apps:                apps,
	}
}

func projectSessionTurnAgent(a turns.Agent) types.SessionTurnAgent {
	return types.SessionTurnAgent{
		ID:            a.ID,
		Name:          a.Name,
		BindingSource: string(a.BindingSource),
		Complete:      string(a.Complete),
	}
}

func projectSessionTurnQuery(q turns.Query) types.SessionTurnQuery {
	return types.SessionTurnQuery{
		Text:     q.Text,
		At:       q.At,
		Complete: string(q.Complete),
	}
}

func projectSessionTurnAnswer(a turns.Answer) types.SessionTurnAnswer {
	out := types.SessionTurnAnswer{
		State:    string(a.State),
		Inline:   a.Inline,
		Seq:      a.Seq,
		Complete: string(a.Complete),
	}
	if a.Ref != nil {
		out.Ref = &types.SessionTurnAnswerRef{
			ID:        a.Ref.ID,
			MimeType:  a.Ref.MimeType,
			SizeBytes: a.Ref.SizeBytes,
			Filename:  a.Ref.Filename,
			SHA256:    a.Ref.SHA256,
		}
	}
	return out
}

func projectSessionTurnPause(p turns.Pause) types.SessionTurnPause {
	return types.SessionTurnPause{
		Class:        string(p.Class),
		Reason:       p.Reason,
		Lifecycle:    string(p.Lifecycle),
		Availability: string(p.Availability),
	}
}

func projectSessionTurnAttachment(a turns.Attachment) types.SessionTurnAttachment {
	return types.SessionTurnAttachment{
		ID:           a.ID,
		Filename:     a.Filename,
		MimeType:     a.MimeType,
		SizeBytes:    a.SizeBytes,
		SHA256:       a.SHA256,
		Disposition:  a.Disposition,
		Availability: string(a.Availability),
	}
}

func projectSessionTurnUsage(u turns.Usage) types.SessionTurnUsage {
	return types.SessionTurnUsage{
		PromptTokens:     projectSessionTurnUsageMeasure(u.PromptTokens),
		CompletionTokens: projectSessionTurnUsageMeasure(u.CompletionTokens),
		ReasoningTokens:  projectSessionTurnUsageMeasure(u.ReasoningTokens),
		CacheReadTokens:  projectSessionTurnUsageMeasure(u.CacheReadTokens),
		CacheWriteTokens: projectSessionTurnUsageMeasure(u.CacheWriteTokens),
		TotalTokens:      projectSessionTurnUsageMeasure(u.TotalTokens),
		CostMicroUSD:     projectSessionTurnUsageMeasure(u.CostMicroUSD),
		LatencyNS:        projectSessionTurnUsageMeasure(u.LatencyNS),
		Model:            u.Model,
	}
}

func projectSessionTurnUsageMeasure(m turns.UsageMeasure) types.SessionTurnUsageMeasure {
	out := types.SessionTurnUsageMeasure{State: string(m.State)}
	if m.Value != nil {
		v := *m.Value
		out.Value = &v
	}
	return out
}

func projectSessionTurnReasoning(r turns.Reasoning) types.SessionTurnReasoning {
	steps := make([]types.SessionTurnReasoningStep, 0, len(r.Steps))
	for _, s := range r.Steps {
		steps = append(steps, types.SessionTurnReasoningStep{Index: s.Index, Kind: string(s.Kind)})
	}
	return types.SessionTurnReasoning{
		Steps:    steps,
		Complete: string(r.Complete),
		Dropped:  r.Dropped,
		Seq:      r.Seq,
	}
}

func projectSessionTurnActivity(a turns.Activity) types.SessionTurnActivity {
	rows := make([]types.SessionTurnActivityRow, 0, len(a.Rows))
	for _, r := range a.Rows {
		rows = append(rows, projectSessionTurnActivityRow(r))
	}
	return types.SessionTurnActivity{
		Rows:     rows,
		Complete: string(a.Complete),
		More:     a.More,
		Dropped:  a.Dropped,
		Totals: types.SessionTurnActivityTotals{
			Invoked:         a.Totals.Invoked,
			Succeeded:       a.Totals.Succeeded,
			Failed:          a.Totals.Failed,
			Cancelled:       a.Totals.Cancelled,
			Retried:         a.Totals.Retried,
			PolicyExhausted: a.Totals.PolicyExhausted,
		},
	}
}

func projectSessionTurnActivityRow(r turns.ActivityRow) types.SessionTurnActivityRow {
	return types.SessionTurnActivityRow{
		Position:        r.Position,
		InvocationID:    r.InvocationID,
		Tool:            r.Tool,
		StepSequence:    r.StepSequence,
		BatchID:         r.BatchID,
		Status:          string(r.Status),
		TerminalClass:   string(r.TerminalClass),
		StartedAt:       r.StartedAt,
		FinishedAt:      r.FinishedAt,
		Duration:        r.Duration,
		AttemptCount:    r.AttemptCount,
		Retryable:       r.Retryable,
		PolicyExhausted: r.PolicyExhausted,
		Summary:         r.Summary,
	}
}

func projectSessionTurnAppRef(a turns.AppRef) types.SessionTurnAppRef {
	return types.SessionTurnAppRef{
		EffectiveAgentID: a.EffectiveAgentID,
		ServerID:         a.ServerID,
		ResourceURI:      a.ResourceURI,
		DisplayMode:      a.DisplayMode,
		RawHTMLTrusted:   a.RawHTMLTrusted,
		ToolCallID:       a.ToolCallID,
		ToolName:         a.ToolName,
		Availability:     string(a.Availability),
		Complete:         string(a.Complete),
	}
}

// projectSessionOpsTurnRow maps one operations-safe DTO row onto the
// wire. The structurally distinct shape deliberately carries NO query /
// answer / reasoning summaries / App URI / tool_call_id / App context /
// pause tokens.
func projectSessionOpsTurnRow(row turns.OpsTurnRow) types.SessionOpsTurnRow {
	apps := make([]types.SessionOpsAppRef, 0, len(row.Apps))
	for _, a := range row.Apps {
		apps = append(apps, types.SessionOpsAppRef{
			EffectiveAgentID: a.EffectiveAgentID,
			ServerID:         a.ServerID,
			ToolName:         a.ToolName,
			Availability:     string(a.Availability),
		})
	}
	return types.SessionOpsTurnRow{
		TurnID:              string(row.TurnID),
		TaskID:              row.TaskID,
		RunID:               row.RunID,
		SessionID:           row.SessionID,
		Sequence:            int64(row.Sequence),
		TieBreaker:          string(row.TieBreaker),
		Status:              string(row.Status),
		Sealed:              row.Sealed,
		Version:             row.Version,
		StartedAt:           row.StartedAt,
		UpdatedAt:           row.UpdatedAt,
		FinishedAt:          row.FinishedAt,
		FinishReason:        string(row.FinishReason),
		ErrorClass:          string(row.ErrorClass),
		FinishMessage:       row.FinishMessage,
		ErrorMessage:        row.ErrorMessage,
		AgentID:             row.AgentID,
		AgentName:           row.AgentName,
		AgentBindingSource:  string(row.AgentBindingSource),
		Usage:               projectSessionTurnUsage(row.Usage),
		Activity:            projectSessionTurnActivity(row.Activity),
		ReasoningSteps:      row.ReasoningSteps,
		Inputs:              row.Inputs,
		Outputs:             row.Outputs,
		Apps:                apps,
		Pause:               projectSessionTurnPause(row.Pause),
		LastAppliedEventSeq: row.LastAppliedEventSeq,
	}
}
