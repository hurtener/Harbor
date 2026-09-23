package summarizer_test

import (
	"context"
	"sync"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
)

// stubClient is a test-local llm.LLMClient that records every Complete
// call's request + identity, optionally forces an error, and returns a
// canned response. It is intentionally NOT registered through the
// production llm.Register seam — it lives in this _test.go and never
// reaches a production binary path.
type stubClient struct {
	mu       sync.Mutex
	calls    []recordedCall
	response llm.CompleteResponse
	err      error
}

type recordedCall struct {
	id      identity.Identity
	req     llm.CompleteRequest
	content string
}

func (s *stubClient) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	id, _ := identity.From(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	rc := recordedCall{id: id, req: req}
	for _, m := range req.Messages {
		if m.Content.Text != nil {
			rc.content += *m.Content.Text + "\n"
		}
	}
	s.calls = append(s.calls, rc)
	if s.err != nil {
		return llm.CompleteResponse{}, s.err
	}
	return s.response, nil
}

func (s *stubClient) Close(_ context.Context) error { return nil }

func (s *stubClient) seenCalls() []recordedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]recordedCall, len(s.calls))
	copy(out, s.calls)
	return out
}

func newStubClient() *stubClient {
	return &stubClient{
		response: llm.CompleteResponse{Content: goodSummaryJSON},
	}
}
