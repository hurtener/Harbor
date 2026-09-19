package bifrost

import (
	"context"
	"testing"

	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/llm"
)

func TestFinishReason_UnarySelectedChoiceOnly(t *testing.T) {
	t.Parallel()
	stop, length := "stop", "length"
	response := &bfschemas.BifrostChatResponse{Choices: []bfschemas.BifrostResponseChoice{
		{Index: 1, FinishReason: &stop, ChatNonStreamResponseChoice: &bfschemas.ChatNonStreamResponseChoice{}},
		{Index: 0, FinishReason: &length, ChatNonStreamResponseChoice: &bfschemas.ChatNonStreamResponseChoice{}},
	}}
	if got := translateResponse(response).FinishReason; got != "length" {
		t.Fatalf("reason=%q", got)
	}
	response.Choices = response.Choices[:1]
	if got := translateResponse(response).FinishReason; got != "" {
		t.Fatalf("missing choice zero became %q", got)
	}
	if got := translateResponse(nil).FinishReason; got != "" {
		t.Fatal("nil response invented stop")
	}
}

func TestFinishReason_StreamTerminalSurvivesUsageChunk(t *testing.T) {
	t.Parallel()
	stub := newStubClient()
	stub.streamHandler = func(*bfschemas.BifrostChatRequest) (chan *bfschemas.BifrostStreamChunk, *bfschemas.BifrostError) {
		ch := make(chan *bfschemas.BifrostStreamChunk, 3)
		length, stop := "length", "stop"
		ch <- &bfschemas.BifrostStreamChunk{BifrostChatResponse: &bfschemas.BifrostChatResponse{Choices: []bfschemas.BifrostResponseChoice{{Index: 1, FinishReason: &stop}, {Index: 0, FinishReason: &length}}}}
		ch <- &bfschemas.BifrostStreamChunk{BifrostChatResponse: &bfschemas.BifrostChatResponse{}}
		close(ch)
		return ch, nil
	}
	driver := newDriverWithClient(stub, bfschemas.OpenAI, nil)
	defer func() { _ = driver.Close(context.Background()) }()
	text := "go"
	response, err := driver.Complete(withIdentity(t, context.Background(), "finish"), llm.CompleteRequest{Model: "m", Stream: true, Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}}})
	if err != nil {
		t.Fatal(err)
	}
	if response.FinishReason != "length" {
		t.Fatalf("reason=%q", response.FinishReason)
	}
}
