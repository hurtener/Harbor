package bifrost

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/llm"
)

// Cancellation must close the socket even before the
// provider has emitted headers. No model or external service is involved.
func TestDriver_CancellationBeforeHeadersClosesUpstream(t *testing.T) {
	for _, provider := range []string{"openai", "openrouter"} {
		t.Run(provider, func(t *testing.T) {
			t.Run("stream", func(t *testing.T) { cancellationBeforeHeaders(t, provider, true) })
			t.Run("unary", func(t *testing.T) { cancellationBeforeHeaders(t, provider, false) })
		})
	}
}

func cancellationBeforeHeaders(t *testing.T, provider string, stream bool) {
	t.Helper()
	// Synthetic local fixture credential, never sent outside httptest.
	t.Setenv("HARBOR_CANCEL_HEADERS_KEY", "synthetic-fixture-key")
	started, disconnected, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
			close(disconnected)
		case <-release:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	driver, err := New(llm.ConfigSnapshot{Provider: provider, Model: "m", APIKey: "env.HARBOR_CANCEL_HEADERS_KEY", BaseURL: server.URL, Timeout: 10 * time.Second}, llm.Deps{})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		close(release)
		_ = driver.Close(context.Background())
		server.Close()
	})
	ctx, cancel := context.WithTimeout(withIdentity(t, t.Context(), "headers-cancel"), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		text := "fixture"
		_, err := driver.Complete(ctx, llm.CompleteRequest{Model: "m", Stream: stream, Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}}})
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("provider request did not start")
	}
	cancel()
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Error("cancelled provider socket remained open before response headers")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("completion cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Error("completion still waiting after cancellation")
	}
}
