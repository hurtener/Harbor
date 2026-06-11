package bifrost

import (
	"fmt"
	"sync"
	"sync/atomic"

	bfschemas "github.com/maximhq/bifrost/core/schemas"
)

// stubClient is a deterministic in-test implementation of
// `bifrostClient`. It records every request and returns operator-
// supplied responses / errors. Safe for N concurrent goroutines —
// the recorder uses a mutex; per-call response selection is
// stateless.
type stubClient struct {
	mu       sync.Mutex
	requests []*bfschemas.BifrostChatRequest

	// hook overrides — set by tests for specific scenarios.
	chatHandler   func(req *bfschemas.BifrostChatRequest) (*bfschemas.BifrostChatResponse, *bfschemas.BifrostError)
	streamHandler func(req *bfschemas.BifrostChatRequest) (chan *bfschemas.BifrostStreamChunk, *bfschemas.BifrostError)

	// provider-native file surface recorders / hooks.
	fileUploads   []*bfschemas.BifrostFileUploadRequest
	fileDeletes   []*bfschemas.BifrostFileDeleteRequest
	uploadHandler func(req *bfschemas.BifrostFileUploadRequest) (*bfschemas.BifrostFileUploadResponse, *bfschemas.BifrostError)
	deleteHandler func(req *bfschemas.BifrostFileDeleteRequest) (*bfschemas.BifrostFileDeleteResponse, *bfschemas.BifrostError)

	calls       atomic.Int64
	uploadCalls atomic.Int64
}

func newStubClient() *stubClient {
	return &stubClient{}
}

// ChatCompletionRequest records the request and delegates to the
// configured handler (or returns a benign default).
func (s *stubClient) ChatCompletionRequest(_ *bfschemas.BifrostContext, req *bfschemas.BifrostChatRequest) (*bfschemas.BifrostChatResponse, *bfschemas.BifrostError) {
	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.mu.Unlock()
	s.calls.Add(1)
	if s.chatHandler != nil {
		return s.chatHandler(req)
	}
	return defaultChatResponse(req), nil
}

// ChatCompletionStreamRequest records the request and delegates to
// the configured handler.
func (s *stubClient) ChatCompletionStreamRequest(_ *bfschemas.BifrostContext, req *bfschemas.BifrostChatRequest) (chan *bfschemas.BifrostStreamChunk, *bfschemas.BifrostError) {
	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.mu.Unlock()
	s.calls.Add(1)
	if s.streamHandler != nil {
		return s.streamHandler(req)
	}
	return defaultStreamResponse(req), nil
}

// FileUploadRequest records the upload and delegates to the
// configured handler (or returns a deterministic per-call file id).
func (s *stubClient) FileUploadRequest(_ *bfschemas.BifrostContext, req *bfschemas.BifrostFileUploadRequest) (*bfschemas.BifrostFileUploadResponse, *bfschemas.BifrostError) {
	s.mu.Lock()
	s.fileUploads = append(s.fileUploads, req)
	s.mu.Unlock()
	n := s.uploadCalls.Add(1)
	if s.uploadHandler != nil {
		return s.uploadHandler(req)
	}
	return &bfschemas.BifrostFileUploadResponse{
		ID:       fmt.Sprintf("file-stub-%d", n),
		Bytes:    int64(len(req.File)),
		Filename: req.Filename,
		Purpose:  req.Purpose,
	}, nil
}

// FileDeleteRequest records the delete and delegates to the
// configured handler (or acknowledges).
func (s *stubClient) FileDeleteRequest(_ *bfschemas.BifrostContext, req *bfschemas.BifrostFileDeleteRequest) (*bfschemas.BifrostFileDeleteResponse, *bfschemas.BifrostError) {
	s.mu.Lock()
	s.fileDeletes = append(s.fileDeletes, req)
	s.mu.Unlock()
	if s.deleteHandler != nil {
		return s.deleteHandler(req)
	}
	return &bfschemas.BifrostFileDeleteResponse{ID: req.FileID, Deleted: true}, nil
}

// uploadedFilenames returns the recorded upload filenames (test
// observability).
func (s *stubClient) recordedUploads() []*bfschemas.BifrostFileUploadRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*bfschemas.BifrostFileUploadRequest, len(s.fileUploads))
	copy(out, s.fileUploads)
	return out
}

// recordedDeletes returns the recorded delete requests.
func (s *stubClient) recordedDeletes() []*bfschemas.BifrostFileDeleteRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*bfschemas.BifrostFileDeleteRequest, len(s.fileDeletes))
	copy(out, s.fileDeletes)
	return out
}

// requestsSnapshot returns a copy of every recorded chat request.
func (s *stubClient) requestsSnapshot() []*bfschemas.BifrostChatRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*bfschemas.BifrostChatRequest, len(s.requests))
	copy(out, s.requests)
	return out
}

// lastRequest returns the most recent recorded request (nil if none).
func (s *stubClient) lastRequest() *bfschemas.BifrostChatRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		return nil
	}
	return s.requests[len(s.requests)-1]
}

// defaultChatResponse synthesizes a benign non-streaming response.
// Used by tests that don't care about the response shape — they only
// assert on translation correctness via the recorded request.
func defaultChatResponse(req *bfschemas.BifrostChatRequest) *bfschemas.BifrostChatResponse {
	content := "stub:ok"
	if req != nil && req.Model != "" {
		content = "stub:" + req.Model
	}
	return &bfschemas.BifrostChatResponse{
		ID:     "stub-id",
		Model:  req.Model,
		Object: "chat.completion",
		Choices: []bfschemas.BifrostResponseChoice{
			{
				Index: 0,
				ChatNonStreamResponseChoice: &bfschemas.ChatNonStreamResponseChoice{
					Message: &bfschemas.ChatMessage{
						Role: bfschemas.ChatMessageRoleAssistant,
						Content: &bfschemas.ChatMessageContent{
							ContentStr: &content,
						},
					},
				},
			},
		},
		Usage: &bfschemas.BifrostLLMUsage{
			PromptTokens:     10,
			CompletionTokens: 5,
			TotalTokens:      15,
			Cost: &bfschemas.BifrostCost{
				InputTokensCost:  0.001,
				OutputTokensCost: 0.002,
				TotalCost:        0.003,
			},
		},
	}
}

// defaultStreamResponse returns a small pre-built stream:
// three content chunks and a final usage chunk. Each chunk is a
// non-nil `BifrostStreamChunk` whose `BifrostChatResponse` carries a
// `ChatStreamResponseChoice`. The channel is closed after the final
// chunk.
func defaultStreamResponse(req *bfschemas.BifrostChatRequest) chan *bfschemas.BifrostStreamChunk {
	ch := make(chan *bfschemas.BifrostStreamChunk, 4)
	go func() {
		defer close(ch)
		for _, piece := range []string{"hel", "lo ", "wor"} {
			s := piece
			ch <- &bfschemas.BifrostStreamChunk{
				BifrostChatResponse: &bfschemas.BifrostChatResponse{
					Model: req.Model,
					Choices: []bfschemas.BifrostResponseChoice{
						{
							ChatStreamResponseChoice: &bfschemas.ChatStreamResponseChoice{
								Delta: &bfschemas.ChatStreamResponseChoiceDelta{
									Content: &s,
								},
							},
						},
					},
				},
			}
		}
		// Final usage-bearing chunk (no content).
		ch <- &bfschemas.BifrostStreamChunk{
			BifrostChatResponse: &bfschemas.BifrostChatResponse{
				Model: req.Model,
				Usage: &bfschemas.BifrostLLMUsage{
					PromptTokens:     8,
					CompletionTokens: 9,
					TotalTokens:      17,
					Cost: &bfschemas.BifrostCost{
						InputTokensCost:  0.0008,
						OutputTokensCost: 0.0009,
						TotalCost:        0.0017,
					},
				},
			},
		}
	}()
	return ch
}

// blockingStreamResponse returns a channel that emits one chunk, then
// blocks forever (or until the channel is closed externally). Used by
// the cancellation-isolation test to assert the driver returns
// `ctx.Err()` promptly without waiting for the upstream.
func blockingStreamResponse() chan *bfschemas.BifrostStreamChunk {
	ch := make(chan *bfschemas.BifrostStreamChunk, 1)
	piece := "first"
	ch <- &bfschemas.BifrostStreamChunk{
		BifrostChatResponse: &bfschemas.BifrostChatResponse{
			Model: "stub",
			Choices: []bfschemas.BifrostResponseChoice{
				{
					ChatStreamResponseChoice: &bfschemas.ChatStreamResponseChoice{
						Delta: &bfschemas.ChatStreamResponseChoiceDelta{
							Content: &piece,
						},
					},
				},
			},
		},
	}
	return ch
}
