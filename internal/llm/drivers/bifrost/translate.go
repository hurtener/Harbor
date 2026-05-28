// Package bifrost is Harbor's bifrost-backed LLM driver. It wires
// `github.com/maximhq/bifrost/core` behind the `llm.Driver` interface
// settled in Phase 32 (RFC §6.5 / brief 08).
//
// The driver is a thin translation adapter: `llm.CompleteRequest`
// flows into `schemas.BifrostChatRequest`, the response flows back into
// `llm.CompleteResponse`, and the multimodal `ContentPart` sum-type
// (D-021) maps to bifrost's `ChatContentBlock` shapes. Bifrost's
// provider-native tool-calling parameters (the `tools=` request field,
// the `tool_choice=` mode selector, OpenAI's `function_call`,
// Anthropic's `tool_use` blocks) are intentionally NEVER referenced —
// Harbor's runtime owns tool dispatch (RFC §6.4 / brief 07). The
// Phase 32 smoke script's static guard fails on any leak.
//
// Auto-materialization (D-022 / D-039) runs in the Phase 32 safety pass
// upstream; this driver sees post-materialization requests where
// oversize `DataURL`s have already been rewritten as `ArtifactStub`s.
// The driver translates each supply form (URL / DataURL / Artifact)
// faithfully — the safety pass guarantees the inline `DataURL` is
// below the heavy-output threshold.
//
// Concurrent-reuse contract (D-025): the driver itself is stateless
// across calls. The `*bf.Bifrost` it holds is internally synchronized
// by bifrost. The `closed` flag is `atomic.Bool` for the idempotent
// Close path. Safe for N concurrent goroutines after construction.
//
// Cancellation semantics: a streaming Complete cancelled mid-flight
// returns `ctx.Err()` immediately; the driver abandons the bifrost
// chunk reader (brief 08 §"Cancellation caveat"). Bifrost drains its
// upstream HTTP connection on its own goroutine; Harbor never blocks
// waiting for it. The goroutine-leak test pins this.
package bifrost

import (
	"encoding/json"
	"fmt"
	"strings"

	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/llm"
)

// translateRequest builds a `*schemas.BifrostChatRequest` from
// Harbor's `llm.CompleteRequest`. The driver-level fields (`Provider`,
// `Model`) come from the driver's `cfg`; sampler / parameter fields
// come from `req`. Validation has already run upstream (Phase 32
// safety pass); this function trusts its inputs.
//
// Provider-native tool-calling fields are NEVER set — see RFC §6.4
// / brief 07 / the smoke static guard.
func translateRequest(provider bfschemas.ModelProvider, req llm.CompleteRequest) (*bfschemas.BifrostChatRequest, error) {
	messages, err := translateMessages(req.Messages)
	if err != nil {
		return nil, fmt.Errorf("translate messages: %w", err)
	}
	params, err := translateParams(provider, req)
	if err != nil {
		return nil, fmt.Errorf("translate params: %w", err)
	}
	return &bfschemas.BifrostChatRequest{
		Provider: provider,
		Model:    req.Model,
		Input:    messages,
		Params:   params,
	}, nil
}

// translateMessages converts Harbor's `[]llm.ChatMessage` into
// bifrost's `[]schemas.ChatMessage`.
func translateMessages(in []llm.ChatMessage) ([]bfschemas.ChatMessage, error) {
	out := make([]bfschemas.ChatMessage, 0, len(in))
	for i, m := range in {
		// An assistant message with `ToolCalls` and no `Content` —
		// the trajectory-replay shape for a prior CallTool step
		// with no preamble — translates with no content field on
		// the wire. OpenAI's spec requires `content: null` (not
		// `""`) when `tool_calls` is present; translateContent
		// would otherwise reject the nil/nil shape.
		var (
			content *bfschemas.ChatMessageContent
			err     error
		)
		isAsstWithToolCalls := m.Role == llm.RoleAssistant && len(m.ToolCalls) > 0
		if isAsstWithToolCalls && m.Content.Text == nil && m.Content.Parts == nil {
			content = nil
		} else {
			content, err = translateContent(m.Content)
			if err != nil {
				return nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
		}
		// Defense in depth: when the renderer set Content.Text to a
		// pointer to "", flatten that to nil on the wire. OpenAI
		// rejects `content: ""` with tool_calls present.
		if isAsstWithToolCalls && content != nil &&
			content.ContentStr != nil && *content.ContentStr == "" &&
			len(content.ContentBlocks) == 0 {
			content = nil
		}
		// Phase 107c / D-167 — native tool-result routing.
		//
		// A RoleTool message that carries a non-nil ToolCallID is a
		// native tool-result thread-back: it MUST map to bifrost's
		// `tool` role with `tool_call_id` set so OpenAI/Anthropic/
		// Gemini recognise the message as a tool-result reply (the
		// brief-07 user-role-observation convention is preserved for
		// RoleTool messages WITHOUT a ToolCallID — those are the
		// legacy prompt-engineered observation strings).
		role := translateRole(m.Role)
		if m.Role == llm.RoleTool && m.ToolCallID != nil {
			role = bfschemas.ChatMessageRoleTool
		}
		msg := bfschemas.ChatMessage{
			Role:    role,
			Content: content,
			Name:    m.Name,
		}
		if m.Role == llm.RoleTool && m.ToolCallID != nil {
			tid := *m.ToolCallID
			msg.ChatToolMessage = &bfschemas.ChatToolMessage{
				ToolCallID: &tid,
			}
		}
		// Phase 107c / D-167 — native tool-call replay on the
		// assistant side. The React planner's trajectory renderer
		// emits a RoleAssistant message with `ToolCalls` set for
		// every prior CallTool step, paired with a RoleTool message
		// carrying the matching `ToolCallID` + observation. Without
		// this branch the provider would see an assistant turn with
		// no `tool_calls` block but a sibling tool-result message
		// referencing a missing call id — every provider rejects
		// that shape.
		if m.Role == llm.RoleAssistant && len(m.ToolCalls) > 0 {
			msg.ChatAssistantMessage = &bfschemas.ChatAssistantMessage{
				ToolCalls: translateAssistantToolCalls(m.ToolCalls),
			}
		}
		out = append(out, msg)
	}
	return out, nil
}

// translateRole maps Harbor's `llm.Role` to bifrost's
// `ChatMessageRole`. Unknown roles default to `user` — Harbor's
// `Content` validation already rejected unknown discriminators at the
// safety pass, so this branch is defensive only.
func translateRole(r llm.Role) bfschemas.ChatMessageRole {
	switch r {
	case llm.RoleSystem:
		return bfschemas.ChatMessageRoleSystem
	case llm.RoleAssistant:
		return bfschemas.ChatMessageRoleAssistant
	case llm.RoleTool:
		// Harbor's RoleTool is the convention for tool-observation
		// renderings; brief 07 §5 says these arrive as user-role
		// messages inside the LLM thread. Bifrost has a separate
		// "tool" role, but we DO NOT use it — to a remote provider, a
		// Harbor tool-observation looks exactly like a user message
		// reciting the observation. Honour that boundary.
		return bfschemas.ChatMessageRoleUser
	default:
		return bfschemas.ChatMessageRoleUser
	}
}

// translateContent maps Harbor's `llm.Content` sum-type to bifrost's
// `*schemas.ChatMessageContent`. The text path uses `ContentStr`; the
// multimodal path uses `ContentBlocks` with one entry per Harbor part.
func translateContent(c llm.Content) (*bfschemas.ChatMessageContent, error) {
	switch {
	case c.Text != nil:
		txt := *c.Text
		return &bfschemas.ChatMessageContent{ContentStr: &txt}, nil
	case c.Parts != nil:
		blocks, err := translateParts(c.Parts)
		if err != nil {
			return nil, err
		}
		return &bfschemas.ChatMessageContent{ContentBlocks: blocks}, nil
	}
	// Defensive — Phase 32's safety pass rejects this case before
	// the driver runs.
	return nil, fmt.Errorf("content has neither Text nor Parts set")
}

// translateParts maps Harbor's `[]llm.ContentPart` to bifrost's
// `[]schemas.ChatContentBlock`. Each Harbor supply form (URL /
// DataURL / Artifact) maps to bifrost's per-block shape:
//
//   - Image URL or DataURL → `ChatInputImage{URL: ...}` (bifrost
//     accepts both raw URLs and data URLs in the same field).
//   - Audio URL or DataURL → `ChatInputAudio{Data: ...}` (bifrost's
//     audio shape is just a string payload).
//   - File URL or DataURL → `ChatInputFile{FileURL: ...}` or
//     `ChatInputFile{FileData: ...}` (file is the most structured).
//   - Artifact → emit an `ArtifactStub` JSON blob as a `text` block
//     with the canonical Harbor JSON shape (RFC §6.5 settled — every
//     provider sees the same stub format regardless of multimodal
//     support).
func translateParts(in []llm.ContentPart) ([]bfschemas.ChatContentBlock, error) {
	out := make([]bfschemas.ChatContentBlock, 0, len(in))
	for i, p := range in {
		switch p.Type {
		case llm.PartText:
			txt := p.Text
			out = append(out, bfschemas.ChatContentBlock{
				Type: bfschemas.ChatContentBlockTypeText,
				Text: &txt,
			})
		case llm.PartImage:
			block, err := translateImagePart(p.Image)
			if err != nil {
				return nil, fmt.Errorf("parts[%d] image: %w", i, err)
			}
			out = append(out, block)
		case llm.PartAudio:
			block, err := translateAudioPart(p.Audio)
			if err != nil {
				return nil, fmt.Errorf("parts[%d] audio: %w", i, err)
			}
			out = append(out, block)
		case llm.PartFile:
			block, err := translateFilePart(p.File)
			if err != nil {
				return nil, fmt.Errorf("parts[%d] file: %w", i, err)
			}
			out = append(out, block)
		default:
			return nil, fmt.Errorf("parts[%d] unknown type %q", i, p.Type)
		}
	}
	return out, nil
}

// translateImagePart resolves the (URL | DataURL | Artifact) sum into
// bifrost's image-block shape. Artifact form renders as the canonical
// `ArtifactStub` JSON inside a text block (D-022 / RFC §6.5) so
// providers without vision still receive a meaningful description.
func translateImagePart(p *llm.ImagePart) (bfschemas.ChatContentBlock, error) {
	if p == nil {
		return bfschemas.ChatContentBlock{}, fmt.Errorf("ImagePart is nil")
	}
	if p.Artifact != nil {
		return artifactStubBlock(p.Artifact)
	}
	url := p.URL
	if url == "" {
		url = p.DataURL
	}
	if url == "" {
		return bfschemas.ChatContentBlock{}, fmt.Errorf("ImagePart has no URL / DataURL / Artifact")
	}
	var detail *string
	if p.Detail != "" {
		d := p.Detail
		detail = &d
	}
	return bfschemas.ChatContentBlock{
		Type: bfschemas.ChatContentBlockTypeImage,
		ImageURLStruct: &bfschemas.ChatInputImage{
			URL:    url,
			Detail: detail,
		},
	}, nil
}

// translateAudioPart resolves the (URL | DataURL | Artifact) sum into
// bifrost's input-audio block. Artifact form renders as the canonical
// `ArtifactStub` JSON inside a text block.
func translateAudioPart(p *llm.AudioPart) (bfschemas.ChatContentBlock, error) {
	if p == nil {
		return bfschemas.ChatContentBlock{}, fmt.Errorf("AudioPart is nil")
	}
	if p.Artifact != nil {
		return artifactStubBlock(p.Artifact)
	}
	data := p.URL
	if data == "" {
		data = p.DataURL
	}
	if data == "" {
		return bfschemas.ChatContentBlock{}, fmt.Errorf("AudioPart has no URL / DataURL / Artifact")
	}
	var format *string
	if p.MIME != "" {
		// MIME `audio/wav` → format hint `wav`. Bifrost's audio shape
		// expects the codec hint as a short string; we strip the
		// MIME prefix when present and leave operator-supplied
		// values untouched otherwise.
		f := stripMIMEPrefix(p.MIME)
		format = &f
	}
	return bfschemas.ChatContentBlock{
		Type: bfschemas.ChatContentBlockTypeInputAudio,
		InputAudio: &bfschemas.ChatInputAudio{
			Data:   data,
			Format: format,
		},
	}, nil
}

// translateFilePart resolves the (URL | DataURL | Artifact) sum into
// bifrost's file-block. Artifact form renders as the canonical
// `ArtifactStub` JSON inside a text block.
func translateFilePart(p *llm.FilePart) (bfschemas.ChatContentBlock, error) {
	if p == nil {
		return bfschemas.ChatContentBlock{}, fmt.Errorf("FilePart is nil")
	}
	if p.Artifact != nil {
		return artifactStubBlock(p.Artifact)
	}
	var fileData *string
	var fileURL *string
	switch {
	case p.URL != "":
		u := p.URL
		fileURL = &u
	case p.DataURL != "":
		// Bifrost's `ChatInputFile.FileData` expects base64-encoded
		// data. The Phase 32 safety pass has already decoded the data
		// URL to bytes when materializing oversize content; sub-
		// threshold content arrives as the raw `data:` URI which we
		// pass straight through — bifrost's provider converters know
		// how to consume it.
		d := p.DataURL
		fileData = &d
	default:
		return bfschemas.ChatContentBlock{}, fmt.Errorf("FilePart has no URL / DataURL / Artifact")
	}
	var fileType *string
	if p.MIME != "" {
		mt := p.MIME
		fileType = &mt
	}
	var filename *string
	if p.Filename != "" {
		fn := p.Filename
		filename = &fn
	}
	return bfschemas.ChatContentBlock{
		Type: bfschemas.ChatContentBlockTypeFile,
		File: &bfschemas.ChatInputFile{
			FileData: fileData,
			FileURL:  fileURL,
			FileType: fileType,
			Filename: filename,
		},
	}, nil
}

// artifactStubBlock renders an `ArtifactStub` as a text block whose
// body is the canonical JSON shape (RFC §6.5). Every provider —
// vision-capable or not — sees the same bytes; the LLM can choose to
// call the named fetch tool if it needs the underlying content.
func artifactStubBlock(stub *llm.ArtifactStub) (bfschemas.ChatContentBlock, error) {
	if stub == nil {
		return bfschemas.ChatContentBlock{}, fmt.Errorf("artifact stub is nil")
	}
	bytes, err := json.Marshal(stub)
	if err != nil {
		return bfschemas.ChatContentBlock{}, fmt.Errorf("marshal ArtifactStub: %w", err)
	}
	body := string(bytes)
	return bfschemas.ChatContentBlock{
		Type: bfschemas.ChatContentBlockTypeText,
		Text: &body,
	}, nil
}

// stripMIMEPrefix turns `audio/wav` into `wav`. Leaves bare strings
// untouched.
func stripMIMEPrefix(mime string) string {
	if i := strings.IndexByte(mime, '/'); i >= 0 {
		return mime[i+1:]
	}
	return mime
}

// translateParams builds bifrost's `*ChatParameters` from Harbor's
// optional sampler fields + ResponseFormat + ReasoningEffort. Returns
// nil when no parameters are set (lets bifrost use its provider
// defaults).
//
// `provider` is consulted for the Anthropic reasoning-budget floor:
// Anthropic's API requires `reasoning.max_tokens >= 1024` and rejects
// lower values. Harbor maps the effort enum to a token budget for the
// Anthropic provider and fails LOUDLY with [ErrReasoningBudgetTooLow]
// when the resulting budget is below the floor — never silently
// clamping (brief 13 §2.6, CLAUDE.md §5 fail-loudly).
func translateParams(provider bfschemas.ModelProvider, req llm.CompleteRequest) (*bfschemas.ChatParameters, error) {
	params := &bfschemas.ChatParameters{}
	used := false

	if req.Temperature != nil {
		t := float64(*req.Temperature)
		params.Temperature = &t
		used = true
	}
	if req.MaxTokens != nil {
		mt := *req.MaxTokens
		params.MaxCompletionTokens = &mt
		used = true
	}
	if len(req.Stops) > 0 {
		params.Stop = append(params.Stop, req.Stops...)
		used = true
	}
	if req.ReasoningEffort != "" {
		eff := translateReasoningEffort(req.ReasoningEffort)
		// Honour explicit "off" by setting Enabled=false; other
		// values pass through as the Effort string.
		params.Reasoning = &bfschemas.ChatReasoning{}
		if req.ReasoningEffort == llm.ReasoningOff {
			off := false
			params.Reasoning.Enabled = &off
		} else {
			params.Reasoning.Effort = &eff
			// Anthropic requires an explicit reasoning.max_tokens that
			// meets a 1024-token floor. Map the effort enum to a
			// budget and validate it BEFORE the request leaves the
			// process. A budget below the floor fails loud rather than
			// silently clamping.
			if provider == bfschemas.Anthropic {
				budget := anthropicReasoningBudget(req.ReasoningEffort)
				if budget < anthropicReasoningMinTokens {
					return nil, fmt.Errorf(
						"%w: provider=anthropic effort=%q maps to %d tokens, floor is %d",
						ErrReasoningBudgetTooLow, req.ReasoningEffort, budget, anthropicReasoningMinTokens)
				}
				params.Reasoning.MaxTokens = &budget
			}
		}
		used = true
	}
	if req.ResponseFormat != nil {
		rf, err := translateResponseFormat(req.ResponseFormat)
		if err != nil {
			return nil, err
		}
		if rf != nil {
			params.ResponseFormat = rf
			used = true
		}
	}

	// Phase 107c / D-167 — native tool-calling params.
	if len(req.Tools) > 0 {
		bftools := make([]bfschemas.ChatTool, 0, len(req.Tools))
		for _, td := range req.Tools {
			ct, err := translateToolDeclaration(td)
			if err != nil {
				return nil, fmt.Errorf("translate tool declaration: %w", err)
			}
			bftools = append(bftools, ct)
		}
		params.Tools = bftools
		used = true
	}
	if req.ToolChoice != "" {
		tc := translateToolChoice(req.ToolChoice)
		params.ToolChoice = &tc
		used = true
	}
	if req.ParallelToolCalls || req.ToolChoice != "" {
		ptc := req.ParallelToolCalls
		params.ParallelToolCalls = &ptc
		used = true
	}

	if !used {
		return nil, nil
	}
	return params, nil
}

// translateReasoningEffort maps Harbor's enum to the lowercase strings
// bifrost forwards into the per-provider reasoning shape.
func translateReasoningEffort(e llm.ReasoningEffort) string {
	switch e {
	case llm.ReasoningLow:
		return "low"
	case llm.ReasoningMedium:
		return "medium"
	case llm.ReasoningHigh:
		return "high"
	}
	// Default — empty / off cases handled by the caller.
	return string(e)
}

// translateResponseFormat builds bifrost's response_format payload
// from Harbor's `ResponseFormat`. Plain text returns nil (no
// constraint). `json_object` returns the simple `{"type":"json_object"}`
// shape. `json_schema` returns the schema-bearing shape.
//
// Bifrost's `ChatParameters.ResponseFormat` is typed as
// `*interface{}` — provider converters interpret the underlying shape.
// We mirror OpenAI's wire format because it's the lingua franca.
func translateResponseFormat(rf *llm.ResponseFormat) (*interface{}, error) {
	if rf == nil {
		return nil, nil
	}
	switch rf.Kind {
	case llm.FormatText, "":
		return nil, nil
	case llm.FormatJSONObject:
		var v interface{} = map[string]any{"type": "json_object"}
		return &v, nil
	case llm.FormatJSONSchema:
		schema := rf.JSONSchema
		if len(schema) == 0 {
			return nil, fmt.Errorf("ResponseFormat.JSONSchema is empty for kind %q", rf.Kind)
		}
		// Wrap the raw schema bytes inside the `{"type":"json_schema",
		// "json_schema": {...}}` envelope. Many providers expect the
		// envelope (`name`, `strict`, `schema` keys); Phase 34's
		// SchemaSanitizer normalizes the shape per provider — Phase 33
		// passes the operator-supplied schema bytes verbatim.
		var schemaObj any
		if err := json.Unmarshal(schema, &schemaObj); err != nil {
			return nil, fmt.Errorf("decode JSONSchema: %w", err)
		}
		var v interface{} = map[string]any{
			"type":        "json_schema",
			"json_schema": schemaObj,
		}
		return &v, nil
	default:
		return nil, fmt.Errorf("unknown ResponseFormat.Kind %q", rf.Kind)
	}
}

// translateResponse builds Harbor's `llm.CompleteResponse` from
// bifrost's non-streaming response. The assistant message's text
// content goes into `Content`; the message's normalised
// `ReasoningDetails` go into `Reasoning` (Phase 83e — closes the
// unary-path reasoning-capture gap); usage and cost flow through.
func translateResponse(resp *bfschemas.BifrostChatResponse) llm.CompleteResponse {
	out := llm.CompleteResponse{}
	if resp == nil {
		return out
	}
	out.Content = extractContent(resp)
	out.ToolCalls = extractToolCalls(resp)
	out.Reasoning = extractReasoning(resp)
	out.Usage, out.Cost = extractUsageAndCost(resp)
	return out
}

// extractReasoning pulls the normalised reasoning trace from the first
// non-streaming choice's assistant message. Bifrost populates
// `reasoning_details[]` on the message for every reasoning-capable
// provider, including the native Gemini path where the per-delta
// `delta.Reasoning` field is nil (brief 13 §2.6). Empty when the
// provider surfaced no reasoning.
func extractReasoning(resp *bfschemas.BifrostChatResponse) string {
	if resp == nil || len(resp.Choices) == 0 {
		return ""
	}
	choice := resp.Choices[0]
	if choice.ChatNonStreamResponseChoice == nil {
		return ""
	}
	return reasoningFromMessage(choice.ChatNonStreamResponseChoice.Message)
}

// extractContent pulls the assistant-message text from the first
// non-streaming choice. Streaming responses return their content via
// the chunk path; the caller accumulates and supplies a non-streaming-
// shaped response to this helper, or constructs one of its own.
func extractContent(resp *bfschemas.BifrostChatResponse) string {
	if len(resp.Choices) == 0 {
		return ""
	}
	choice := resp.Choices[0]
	if choice.ChatNonStreamResponseChoice != nil &&
		choice.ChatNonStreamResponseChoice.Message != nil &&
		choice.ChatNonStreamResponseChoice.Message.Content != nil &&
		choice.ChatNonStreamResponseChoice.Message.Content.ContentStr != nil {
		return *choice.ChatNonStreamResponseChoice.Message.Content.ContentStr
	}
	// Some providers return the content as blocks even for non-
	// streaming responses; concatenate the text-typed blocks.
	if choice.ChatNonStreamResponseChoice != nil &&
		choice.ChatNonStreamResponseChoice.Message != nil &&
		choice.ChatNonStreamResponseChoice.Message.Content != nil &&
		choice.ChatNonStreamResponseChoice.Message.Content.ContentBlocks != nil {
		var sb strings.Builder
		for _, b := range choice.ChatNonStreamResponseChoice.Message.Content.ContentBlocks {
			if b.Type == bfschemas.ChatContentBlockTypeText && b.Text != nil {
				sb.WriteString(*b.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// extractUsageAndCost decodes bifrost's usage shape (which carries
// `*BifrostCost` as a sub-field) into Harbor's `Usage` + `Cost`. A
// nil-usage response yields zero values; Phase 36a's accumulator
// treats zero cost as "no charge for this call" (a deliberate
// no-op).
func extractUsageAndCost(resp *bfschemas.BifrostChatResponse) (llm.Usage, llm.Cost) {
	var usage llm.Usage
	var cost llm.Cost
	if resp == nil || resp.Usage == nil {
		return usage, cost
	}
	usage.PromptTokens = resp.Usage.PromptTokens
	usage.CompletionTokens = resp.Usage.CompletionTokens
	usage.TotalTokens = resp.Usage.TotalTokens
	if resp.Usage.CompletionTokensDetails != nil {
		usage.ReasoningTokens = resp.Usage.CompletionTokensDetails.ReasoningTokens
	}
	usage.LatencyMS = resp.ExtraFields.Latency
	if resp.Usage.Cost != nil {
		cost.InputTokensCost = resp.Usage.Cost.InputTokensCost
		cost.OutputTokensCost = resp.Usage.Cost.OutputTokensCost
		cost.ReasoningTokensCost = resp.Usage.Cost.ReasoningTokensCost
		cost.TotalCost = resp.Usage.Cost.TotalCost
		cost.Currency = "USD"
	}
	return usage, cost
}

// translateError converts bifrost's typed error into a Go error. The
// driver wraps `BifrostError.Error.Message` (or the type field) with a
// short status-code prefix; provider-correction (Phase 34) can match
// on the wrapped message strings.
func translateError(berr *bfschemas.BifrostError, kind string) error {
	if berr == nil {
		return nil
	}
	msg := ""
	if berr.Error != nil && berr.Error.Message != "" {
		msg = berr.Error.Message
	} else if berr.Type != nil {
		msg = *berr.Type
	}
	status := 0
	if berr.StatusCode != nil {
		status = *berr.StatusCode
	}
	if status > 0 {
		return fmt.Errorf("%s: bifrost: status %d: %s", kind, status, msg)
	}
	return fmt.Errorf("%s: bifrost: %s", kind, msg)
}

// Phase 107c / D-167 — native tool-calling translation helpers.

func translateToolDeclaration(td llm.ToolDeclaration) (bfschemas.ChatTool, error) {
	desc := td.Description
	ct := bfschemas.ChatTool{
		Type: bfschemas.ChatToolTypeFunction,
		Function: &bfschemas.ChatToolFunction{
			Name:        td.Name,
			Description: &desc,
		},
	}
	if len(td.Schema) > 0 {
		var tp bfschemas.ToolFunctionParameters
		// Fail loud on a malformed JSON Schema — silent drop here
		// would teach the LLM an undeclared tool shape (CLAUDE.md §5
		// fail-loudly, §13 forbidden-practices).
		if err := json.Unmarshal(td.Schema, &tp); err != nil {
			return bfschemas.ChatTool{}, fmt.Errorf(
				"tool %q: malformed JSON schema: %w", td.Name, err)
		}
		ct.Function.Parameters = &tp
	}
	return ct, nil
}

func translateToolChoice(tc string) bfschemas.ChatToolChoice {
	val := tc
	return bfschemas.ChatToolChoice{ChatToolChoiceStr: &val}
}

// translateAssistantToolCalls projects Harbor's `[]llm.ToolCallStructured`
// onto bifrost's `[]ChatAssistantMessageToolCall`, which the wire
// format renders as the assistant message's `tool_calls` block.
//
// `Type` is set to "function" explicitly. OpenAI's wire spec for
// chat-completions `tool_calls` requires `"type": "function"` on
// every entry; bifrost's struct tags the field `omitempty`, so a
// nil pointer drops it from the wire and the upstream provider
// either rejects the request or silently malforms it.
//
// `Index` is set sequentially. Provider-side parsers accept either
// the sequential or the original index; sequential keeps the
// replay deterministic.
func translateAssistantToolCalls(in []llm.ToolCallStructured) []bfschemas.ChatAssistantMessageToolCall {
	if len(in) == 0 {
		return nil
	}
	functionType := "function"
	out := make([]bfschemas.ChatAssistantMessageToolCall, 0, len(in))
	for i, tc := range in {
		var id *string
		if tc.ID != "" {
			s := tc.ID
			id = &s
		}
		var name *string
		if tc.Name != "" {
			s := tc.Name
			name = &s
		}
		args := ""
		if len(tc.Args) > 0 {
			args = string(tc.Args)
		}
		out = append(out, bfschemas.ChatAssistantMessageToolCall{
			Index: uint16(i), //nolint:gosec // i is bounded by tool-call count per turn (caller-side guards keep it well within uint16)
			Type:  &functionType,
			ID:    id,
			Function: bfschemas.ChatAssistantMessageToolCallFunction{
				Name:      name,
				Arguments: args,
			},
		})
	}
	return out
}

func extractToolCalls(resp *bfschemas.BifrostChatResponse) []llm.ToolCallStructured {
	if resp == nil || len(resp.Choices) == 0 {
		return nil
	}
	choice := resp.Choices[0]
	if choice.ChatNonStreamResponseChoice == nil {
		return nil
	}
	msg := choice.ChatNonStreamResponseChoice.Message
	// bifrost's `ChatMessage` embeds `*ChatAssistantMessage` as a
	// pointer (see bfschemas/chatcompletions.go); `msg.ToolCalls` is
	// promoted through that pointer and nil-derefs when the response
	// did not carry an assistant block at all. Guard explicitly.
	if msg == nil || msg.ChatAssistantMessage == nil {
		return nil
	}
	calls := msg.ChatAssistantMessage.ToolCalls
	if len(calls) == 0 {
		return nil
	}
	out := make([]llm.ToolCallStructured, 0, len(calls))
	for _, tc := range calls {
		var args json.RawMessage
		if tc.Function.Arguments != "" {
			args = json.RawMessage(tc.Function.Arguments)
		}
		callID := ""
		if tc.ID != nil {
			callID = *tc.ID
		}
		name := ""
		if tc.Function.Name != nil {
			name = *tc.Function.Name
		}
		out = append(out, llm.ToolCallStructured{
			ID:   callID,
			Name: name,
			Args: args,
		})
	}
	return out
}
