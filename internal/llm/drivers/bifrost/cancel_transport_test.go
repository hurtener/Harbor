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

// Exercise the pinned Bifrost SDK and its actual socket, not the stub stream.
// Stopping Harbor's reader alone cannot satisfy this assertion.
func TestDriver_CancellationClosesUpstreamStream(t *testing.T) {
	// Synthetic local fixture credential, never sent outside httptest.
	t.Setenv("HARBOR_CANCEL_FIXTURE_KEY", "synthetic-key")
	disconnected := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"started\"}}]}\n\n"); err != nil {
			t.Error(err)
			return
		}
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(disconnected)
		case <-release:
		}
	}))
	driver, err := New(llm.ConfigSnapshot{Provider: "openai", Model: "m", APIKey: "env.HARBOR_CANCEL_FIXTURE_KEY", BaseURL: server.URL, Timeout: 10 * time.Second}, llm.Deps{})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		close(release)
		_ = driver.Close(context.Background())
		server.Close()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ctx = withIdentity(t, ctx, "socket-cancel")
	text := "fixture"
	_, err = driver.Complete(ctx, llm.CompleteRequest{Model: "m", Stream: true,
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
		OnContent: func(chunk string, _ bool) {
			if chunk != "" {
				cancel()
			}
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("completion cancellation = %v", err)
	}
	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("Harbor returned but the upstream provider connection stayed open")
	}
}
