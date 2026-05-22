package bifrost

import (
	"encoding/json"
	"strings"
	"testing"

	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/llm"
)

// TestTranslateRequest_TextOnly — the common text-only path.
func TestTranslateRequest_TextOnly(t *testing.T) {
	text := "hello"
	req := llm.CompleteRequest{
		Model: "openai/gpt-5.3-chat",
		Messages: []llm.ChatMessage{
			{Role: llm.RoleUser, Content: llm.Content{Text: &text}},
		},
	}
	bfReq, err := translateRequest(bfschemas.OpenRouter, req)
	if err != nil {
		t.Fatalf("translateRequest: %v", err)
	}
	if bfReq.Provider != bfschemas.OpenRouter {
		t.Errorf("Provider = %q want %q", bfReq.Provider, bfschemas.OpenRouter)
	}
	if bfReq.Model != req.Model {
		t.Errorf("Model = %q want %q", bfReq.Model, req.Model)
	}
	if len(bfReq.Input) != 1 {
		t.Fatalf("Input len = %d want 1", len(bfReq.Input))
	}
	msg := bfReq.Input[0]
	if msg.Role != bfschemas.ChatMessageRoleUser {
		t.Errorf("Role = %q want %q", msg.Role, bfschemas.ChatMessageRoleUser)
	}
	if msg.Content == nil || msg.Content.ContentStr == nil || *msg.Content.ContentStr != text {
		t.Errorf("Content text = %v want %q", msg.Content, text)
	}
	if bfReq.Params != nil {
		t.Errorf("Params should be nil when no parameters set, got %+v", bfReq.Params)
	}
}

// TestTranslateRequest_RoleMapping — system, user, assistant roles.
// RoleTool deliberately maps to user (Harbor's tool-observation
// convention; brief 07 §5).
func TestTranslateRequest_RoleMapping(t *testing.T) {
	cases := []struct {
		harbor llm.Role
		want   bfschemas.ChatMessageRole
	}{
		{llm.RoleSystem, bfschemas.ChatMessageRoleSystem},
		{llm.RoleUser, bfschemas.ChatMessageRoleUser},
		{llm.RoleAssistant, bfschemas.ChatMessageRoleAssistant},
		{llm.RoleTool, bfschemas.ChatMessageRoleUser}, // intentional
	}
	txt := "x"
	for _, tc := range cases {
		t.Run(string(tc.harbor), func(t *testing.T) {
			req := llm.CompleteRequest{
				Model:    "m",
				Messages: []llm.ChatMessage{{Role: tc.harbor, Content: llm.Content{Text: &txt}}},
			}
			bfReq, err := translateRequest(bfschemas.OpenAI, req)
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			if got := bfReq.Input[0].Role; got != tc.want {
				t.Errorf("Role %q → %q, want %q", tc.harbor, got, tc.want)
			}
		})
	}
}

// TestTranslateRequest_Multimodal_ImageURL — image part via URL.
func TestTranslateRequest_Multimodal_ImageURL(t *testing.T) {
	req := llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{
			{
				Role: llm.RoleUser,
				Content: llm.Content{
					Parts: []llm.ContentPart{
						{Type: llm.PartText, Text: "describe this"},
						{Type: llm.PartImage, Image: &llm.ImagePart{
							URL:    "https://example.com/cat.png",
							MIME:   "image/png",
							Detail: "high",
						}},
					},
				},
			},
		},
	}
	bfReq, err := translateRequest(bfschemas.OpenAI, req)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	blocks := bfReq.Input[0].Content.ContentBlocks
	if len(blocks) != 2 {
		t.Fatalf("Blocks len = %d want 2", len(blocks))
	}
	if blocks[0].Type != bfschemas.ChatContentBlockTypeText {
		t.Errorf("block 0 Type = %q want text", blocks[0].Type)
	}
	if blocks[1].Type != bfschemas.ChatContentBlockTypeImage {
		t.Errorf("block 1 Type = %q want image_url", blocks[1].Type)
	}
	if blocks[1].ImageURLStruct == nil || blocks[1].ImageURLStruct.URL != "https://example.com/cat.png" {
		t.Errorf("image URL not propagated: %+v", blocks[1].ImageURLStruct)
	}
	if blocks[1].ImageURLStruct.Detail == nil || *blocks[1].ImageURLStruct.Detail != "high" {
		t.Errorf("image Detail = %v want high", blocks[1].ImageURLStruct.Detail)
	}
}

// TestTranslateRequest_Multimodal_ImageDataURL — image part via DataURL.
// (Sub-threshold data URLs survive auto-materialize and reach the
// driver verbatim.)
func TestTranslateRequest_Multimodal_ImageDataURL(t *testing.T) {
	const tinyDataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAUA"
	req := llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{
			{Role: llm.RoleUser, Content: llm.Content{Parts: []llm.ContentPart{
				{Type: llm.PartImage, Image: &llm.ImagePart{DataURL: tinyDataURL, MIME: "image/png"}},
			}}},
		},
	}
	bfReq, err := translateRequest(bfschemas.OpenAI, req)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	block := bfReq.Input[0].Content.ContentBlocks[0]
	if block.Type != bfschemas.ChatContentBlockTypeImage {
		t.Errorf("Type = %q want image_url", block.Type)
	}
	if block.ImageURLStruct == nil || block.ImageURLStruct.URL != tinyDataURL {
		t.Errorf("DataURL not propagated: %+v", block.ImageURLStruct)
	}
}

// TestTranslateRequest_Multimodal_ImageArtifact — image part via
// ArtifactStub. The stub renders as a text block with canonical JSON.
func TestTranslateRequest_Multimodal_ImageArtifact(t *testing.T) {
	stub := &llm.ArtifactStub{
		Ref:       "art-123",
		MIME:      "image/png",
		SizeBytes: 65536,
		Hash:      "sha256:abc",
		Summary:   "screenshot",
		Fetch:     &llm.StubFetch{Tool: "artifact.fetch", ID: "art-123"},
	}
	req := llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{
			{Role: llm.RoleUser, Content: llm.Content{Parts: []llm.ContentPart{
				{Type: llm.PartImage, Image: &llm.ImagePart{Artifact: stub, MIME: "image/png"}},
			}}},
		},
	}
	bfReq, err := translateRequest(bfschemas.OpenAI, req)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	block := bfReq.Input[0].Content.ContentBlocks[0]
	if block.Type != bfschemas.ChatContentBlockTypeText {
		t.Errorf("Type = %q want text (Artifact renders as stub JSON)", block.Type)
	}
	if block.Text == nil {
		t.Fatalf("Text is nil")
	}
	var parsed llm.ArtifactStub
	if err := json.Unmarshal([]byte(*block.Text), &parsed); err != nil {
		t.Fatalf("decode stub JSON: %v", err)
	}
	if parsed.Ref != stub.Ref {
		t.Errorf("Ref roundtrip: got %q want %q", parsed.Ref, stub.Ref)
	}
	if parsed.SizeBytes != stub.SizeBytes {
		t.Errorf("SizeBytes roundtrip: got %d want %d", parsed.SizeBytes, stub.SizeBytes)
	}
}

// TestTranslateRequest_Multimodal_AudioVariants — audio supply forms.
func TestTranslateRequest_Multimodal_AudioVariants(t *testing.T) {
	cases := map[string]llm.AudioPart{
		"URL":      {URL: "https://example.com/clip.mp3", MIME: "audio/mpeg"},
		"DataURL":  {DataURL: "data:audio/wav;base64,UklGR", MIME: "audio/wav"},
		"Artifact": {Artifact: &llm.ArtifactStub{Ref: "audio-1", MIME: "audio/mpeg", SizeBytes: 4096}, MIME: "audio/mpeg"},
	}
	for name, part := range cases {
		t.Run(name, func(t *testing.T) {
			p := part
			req := llm.CompleteRequest{
				Model: "m",
				Messages: []llm.ChatMessage{
					{Role: llm.RoleUser, Content: llm.Content{Parts: []llm.ContentPart{
						{Type: llm.PartAudio, Audio: &p},
					}}},
				},
			}
			bfReq, err := translateRequest(bfschemas.OpenAI, req)
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			block := bfReq.Input[0].Content.ContentBlocks[0]
			if part.Artifact != nil {
				if block.Type != bfschemas.ChatContentBlockTypeText {
					t.Errorf("Artifact variant: Type = %q want text", block.Type)
				}
			} else {
				if block.Type != bfschemas.ChatContentBlockTypeInputAudio {
					t.Errorf("Type = %q want input_audio", block.Type)
				}
				if block.InputAudio == nil || block.InputAudio.Data == "" {
					t.Errorf("InputAudio.Data missing: %+v", block.InputAudio)
				}
			}
		})
	}
}

// TestTranslateRequest_Multimodal_FileVariants — file supply forms.
func TestTranslateRequest_Multimodal_FileVariants(t *testing.T) {
	cases := map[string]llm.FilePart{
		"URL":      {URL: "https://example.com/doc.pdf", MIME: "application/pdf", Filename: "doc.pdf"},
		"DataURL":  {DataURL: "data:application/pdf;base64,JVBERi0x", MIME: "application/pdf", Filename: "inline.pdf"},
		"Artifact": {Artifact: &llm.ArtifactStub{Ref: "doc-1", MIME: "application/pdf", SizeBytes: 8192}, MIME: "application/pdf"},
	}
	for name, part := range cases {
		t.Run(name, func(t *testing.T) {
			p := part
			req := llm.CompleteRequest{
				Model: "m",
				Messages: []llm.ChatMessage{
					{Role: llm.RoleUser, Content: llm.Content{Parts: []llm.ContentPart{
						{Type: llm.PartFile, File: &p},
					}}},
				},
			}
			bfReq, err := translateRequest(bfschemas.OpenAI, req)
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			block := bfReq.Input[0].Content.ContentBlocks[0]
			if part.Artifact != nil {
				if block.Type != bfschemas.ChatContentBlockTypeText {
					t.Errorf("Artifact variant: Type = %q want text", block.Type)
				}
				return
			}
			if block.Type != bfschemas.ChatContentBlockTypeFile {
				t.Errorf("Type = %q want file", block.Type)
			}
			if block.File == nil {
				t.Fatalf("File is nil")
			}
			if part.URL != "" {
				if block.File.FileURL == nil || *block.File.FileURL != part.URL {
					t.Errorf("FileURL = %v want %q", block.File.FileURL, part.URL)
				}
			}
			if part.DataURL != "" {
				if block.File.FileData == nil || *block.File.FileData != part.DataURL {
					t.Errorf("FileData = %v want DataURL value", block.File.FileData)
				}
			}
		})
	}
}

// TestTranslateRequest_ResponseFormat — Text / JSONObject / JSONSchema.
func TestTranslateRequest_ResponseFormat(t *testing.T) {
	schemaBytes := json.RawMessage(`{"name":"reply","schema":{"type":"object"}}`)
	txt := "hello"
	cases := []struct { //nolint:govet // fieldalignment on a test-only struct; field order kept for readability
		name      string
		rf        *llm.ResponseFormat
		wantParam bool
	}{
		{"nil", nil, false},
		{"text", &llm.ResponseFormat{Kind: llm.FormatText}, false},
		{"json_object", &llm.ResponseFormat{Kind: llm.FormatJSONObject}, true},
		{"json_schema", &llm.ResponseFormat{Kind: llm.FormatJSONSchema, JSONSchema: schemaBytes}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := llm.CompleteRequest{
				Model:    "m",
				Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &txt}}},
			}
			req.ResponseFormat = tc.rf
			bfReq, err := translateRequest(bfschemas.OpenAI, req)
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			got := bfReq.Params != nil && bfReq.Params.ResponseFormat != nil
			if got != tc.wantParam {
				t.Errorf("ResponseFormat set: got %v want %v", got, tc.wantParam)
			}
		})
	}
}

// TestTranslateRequest_ReasoningEffort — maps levels + handles "off".
func TestTranslateRequest_ReasoningEffort(t *testing.T) {
	txt := "x"
	cases := []struct { //nolint:govet // fieldalignment on a test-only struct; field order kept for readability
		eff           llm.ReasoningEffort
		wantParam     bool
		wantEffort    string
		wantEnabled   bool
		wantEnabledOK bool
	}{
		{llm.ReasoningEffort(""), false, "", false, false},
		{llm.ReasoningOff, true, "", false, true},
		{llm.ReasoningLow, true, "low", false, false},
		{llm.ReasoningMedium, true, "medium", false, false},
		{llm.ReasoningHigh, true, "high", false, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.eff), func(t *testing.T) {
			req := llm.CompleteRequest{
				Model:    "m",
				Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &txt}}},
			}
			req.ReasoningEffort = tc.eff
			bfReq, err := translateRequest(bfschemas.OpenAI, req)
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			gotParam := bfReq.Params != nil && bfReq.Params.Reasoning != nil
			if gotParam != tc.wantParam {
				t.Errorf("Reasoning set: got %v want %v", gotParam, tc.wantParam)
				return
			}
			if !tc.wantParam {
				return
			}
			r := bfReq.Params.Reasoning
			if tc.wantEffort != "" {
				if r.Effort == nil || *r.Effort != tc.wantEffort {
					t.Errorf("Effort = %v want %q", r.Effort, tc.wantEffort)
				}
			}
			if tc.wantEnabledOK {
				if r.Enabled == nil || *r.Enabled != tc.wantEnabled {
					t.Errorf("Enabled = %v want %v", r.Enabled, tc.wantEnabled)
				}
			}
		})
	}
}

// TestTranslateRequest_Sampler — Temperature / MaxTokens / Stops.
func TestTranslateRequest_Sampler(t *testing.T) {
	txt := "x"
	temp := float32(0.42)
	max := 256
	req := llm.CompleteRequest{
		Model:       "m",
		Messages:    []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &txt}}},
		Temperature: &temp,
		MaxTokens:   &max,
		Stops:       []string{"###", "<END>"},
	}
	bfReq, err := translateRequest(bfschemas.OpenAI, req)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if bfReq.Params == nil {
		t.Fatalf("Params is nil")
	}
	if bfReq.Params.Temperature == nil || *bfReq.Params.Temperature != 0.41999998688697815 && *bfReq.Params.Temperature != float64(temp) {
		// The float32→float64 conversion may not be byte-equal; allow
		// either the literal float64 or the float32-converted value.
		t.Errorf("Temperature = %v want approximately %v", bfReq.Params.Temperature, temp)
	}
	if bfReq.Params.MaxCompletionTokens == nil || *bfReq.Params.MaxCompletionTokens != 256 {
		t.Errorf("MaxCompletionTokens = %v want 256", bfReq.Params.MaxCompletionTokens)
	}
	if len(bfReq.Params.Stop) != 2 {
		t.Errorf("Stops len = %d want 2", len(bfReq.Params.Stop))
	}
}

// TestTranslateResponse_TextContent — non-streaming response is
// extracted from `ChatNonStreamResponseChoice.Message.Content.ContentStr`.
func TestTranslateResponse_TextContent(t *testing.T) {
	text := "hello, world"
	resp := &bfschemas.BifrostChatResponse{
		Choices: []bfschemas.BifrostResponseChoice{
			{
				ChatNonStreamResponseChoice: &bfschemas.ChatNonStreamResponseChoice{
					Message: &bfschemas.ChatMessage{
						Role: bfschemas.ChatMessageRoleAssistant,
						Content: &bfschemas.ChatMessageContent{
							ContentStr: &text,
						},
					},
				},
			},
		},
		Usage: &bfschemas.BifrostLLMUsage{
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
			Cost: &bfschemas.BifrostCost{
				InputTokensCost:  0.001,
				OutputTokensCost: 0.002,
				TotalCost:        0.003,
			},
		},
	}
	out := translateResponse(resp)
	if out.Content != text {
		t.Errorf("Content = %q want %q", out.Content, text)
	}
	if out.Usage.PromptTokens != 10 || out.Usage.CompletionTokens != 20 || out.Usage.TotalTokens != 30 {
		t.Errorf("Usage = %+v", out.Usage)
	}
	if out.Cost.TotalCost != 0.003 || out.Cost.InputTokensCost != 0.001 || out.Cost.OutputTokensCost != 0.002 {
		t.Errorf("Cost = %+v", out.Cost)
	}
	if out.Cost.Currency != "USD" {
		t.Errorf("Currency = %q want USD", out.Cost.Currency)
	}
}

// TestTranslateResponse_BlockContent — non-streaming response with
// content blocks (Anthropic-style). Concatenates text blocks.
func TestTranslateResponse_BlockContent(t *testing.T) {
	a := "hello "
	b := "world"
	resp := &bfschemas.BifrostChatResponse{
		Choices: []bfschemas.BifrostResponseChoice{
			{
				ChatNonStreamResponseChoice: &bfschemas.ChatNonStreamResponseChoice{
					Message: &bfschemas.ChatMessage{
						Content: &bfschemas.ChatMessageContent{
							ContentBlocks: []bfschemas.ChatContentBlock{
								{Type: bfschemas.ChatContentBlockTypeText, Text: &a},
								{Type: bfschemas.ChatContentBlockTypeText, Text: &b},
							},
						},
					},
				},
			},
		},
	}
	out := translateResponse(resp)
	if out.Content != "hello world" {
		t.Errorf("Content = %q want %q", out.Content, "hello world")
	}
}

// TestTranslateResponse_NoUsage — defaults to zero values cleanly.
func TestTranslateResponse_NoUsage(t *testing.T) {
	resp := &bfschemas.BifrostChatResponse{
		Choices: []bfschemas.BifrostResponseChoice{},
	}
	out := translateResponse(resp)
	if out.Usage.TotalTokens != 0 || out.Cost.TotalCost != 0 {
		t.Errorf("zero-value defaults broken: %+v %+v", out.Usage, out.Cost)
	}
}

// TestTranslateError — wraps bifrost error with status code prefix.
func TestTranslateError(t *testing.T) {
	status := 503
	msg := "upstream temporarily unavailable"
	berr := &bfschemas.BifrostError{
		StatusCode: &status,
		Error:      &bfschemas.ErrorField{Message: msg},
	}
	err := translateError(berr, "ChatCompletionRequest")
	if err == nil {
		t.Fatalf("err is nil")
	}
	if !strings.Contains(err.Error(), "status 503") {
		t.Errorf("err missing status: %q", err.Error())
	}
	if !strings.Contains(err.Error(), msg) {
		t.Errorf("err missing message: %q", err.Error())
	}
}
