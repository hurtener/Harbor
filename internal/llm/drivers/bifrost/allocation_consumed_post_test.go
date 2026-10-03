package bifrost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/allocation"
	"github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

type consumedPOSTConnectionKey struct{}

// This uses the real native OpenAI factory and allocation wrapper. The loopback
// server counts complete request bodies before losing headers, independently of
// SDK attempt metadata. The retry case composes transport replay with an SDK 500
// retry; only the final physical request supplies measured response usage.
func TestAllocation_ConsumedPOSTReplayKeepsUnknownEnvelope(t *testing.T) {
	for _, logicalRetry := range []bool{false, true} {
		t.Run(fmt.Sprintf("logical_retry_%t", logicalRetry), func(t *testing.T) {
			var consumed atomic.Int64
			var mu sync.Mutex
			warmed := map[net.Conn]bool{}
			ready := make(chan struct{}, 3)
			release := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			response := `{"id":"fixture","object":"chat.completion","created":1,"model":"fixture","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":5,"total_tokens":35}}`
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
				if err != nil {
					t.Error(err)
					return
				}
				_ = r.Body.Close()
				var body struct {
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
					MaxTokens           *int `json:"max_tokens"`
					MaxCompletionTokens *int `json:"max_completion_tokens"`
				}
				if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || json.Unmarshal(raw, &body) != nil || len(body.Messages) != 1 {
					t.Error("unexpected fixture request")
					w.WriteHeader(400)
					return
				}
				maximum := body.MaxCompletionTokens
				if maximum == nil {
					maximum = body.MaxTokens
				}
				if maximum == nil || *maximum != 100 {
					t.Error("request-specific output cap changed")
					w.WriteHeader(400)
					return
				}
				conn := r.Context().Value(consumedPOSTConnectionKey{}).(net.Conn)
				if body.Messages[0].Content == "warm" {
					mu.Lock()
					warmed[conn] = true
					mu.Unlock()
					ready <- struct{}{}
					<-release
				} else {
					if body.Messages[0].Content != "inference" {
						t.Error("changed request body")
						w.WriteHeader(400)
						return
					}
					n := consumed.Add(1)
					mu.Lock()
					wasWarm := warmed[conn]
					mu.Unlock()
					if wasWarm {
						hijacked, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = hijacked.Close()
						return
					}
					if logicalRetry && n == 4 {
						// Keep this newly established socket pooled after the logical 500,
						// then drop its next fully consumed POST to exercise both layers.
						mu.Lock()
						warmed[conn] = true
						mu.Unlock()
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(500)
						_, _ = io.WriteString(w, `{"error":{"message":"synthetic retry"}}`)
						return
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, response)
			}))
			server.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
				return context.WithValue(ctx, consumedPOSTConnectionKey{}, c)
			}
			server.Start()
			defer server.Close()
			cfg := llm.ConfigSnapshot{Driver: "bifrost", Provider: "openai", Model: "fixture", APIKey: "test-only-not-a-real-key", BaseURL: server.URL, Timeout: 5 * time.Second, NetworkDefaults: llm.NetworkDefaults{MaxRetries: 1, RetryBackoffInitial: time.Millisecond, RetryBackoffMax: time.Millisecond}, DisableCorrections: true, DisableRetry: true, DisableDowngrade: true, DisableGovernance: true, ModelProfiles: map[string]llm.ModelProfile{"fixture": {ContextWindowTokens: 1000}}}
			st, err := inmem.New(config.StateConfig{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close(context.Background()) }()
			mgr := allocation.New(st)
			deps, closeDeps := makeCustomProviderTestDeps(t)
			defer closeDeps()
			deps.Allocations = mgr
			client, err := llm.Open(t.Context(), cfg, deps)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close(context.Background()) }()
			q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "physical"}
			ctx, _ := identity.WithRun(t.Context(), q.Identity, q.RunID)
			request := func(content string) llm.CompleteRequest {
				return llm.CompleteRequest{Model: "fixture", MaxTokens: new(100), Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: new(content)}}}}
			}
			results := make(chan error, 3)
			for range 3 {
				go func() { _, err := client.Complete(ctx, request("warm")); results <- err }()
			}
			for range 3 {
				select {
				case <-ready:
				case <-time.After(6 * time.Second):
					releaseOnce.Do(func() { close(release) })
					t.Fatal("native factory did not warm three sockets")
				}
			}
			releaseOnce.Do(func() { close(release) })
			for range 3 {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
			const envelope int64 = 15 * 1100 // five physical sends times (one configured retry + two).
			a := llm.InferenceAllocation{AllocationID: "fund", Revision: 1, MaxTotalTokens: envelope}
			funded := llm.WithInferenceAllocation(ctx, &a)
			got, err := client.Complete(funded, request("inference"))
			if err != nil || !got.Usage.ReportPresent || got.Usage.Estimated || got.Usage.TotalTokens != 35 {
				t.Fatalf("final response usage: %+v %v", got.Usage, err)
			}
			wantConsumed := int64(4)
			if logicalRetry {
				wantConsumed = 6
			}
			if consumed.Load() != wantConsumed {
				t.Fatalf("consumed POSTs=%d want %d", consumed.Load(), wantConsumed)
			}
			snap, err := mgr.Snapshot(ctx, q, a)
			if err != nil || snap.AttemptCount != 1 || snap.SettledTokens != 35 || snap.ReservedTokens != envelope-35 || snap.UnknownTokens != envelope-35 || snap.BoundBreached {
				t.Fatalf("unknown physical liability: %+v %v", snap, err)
			}
			if _, err = client.Complete(funded, request("inference")); !errors.Is(err, llm.ErrAllocationExhausted) || consumed.Load() != wantConsumed {
				t.Fatalf("exhausted allocation reached transport: %v / %d", err, consumed.Load())
			}
			t.Logf("consumed POSTs=%d; final measured usage=35; full multiplied envelope=%d; retained unknown=%d", consumed.Load(), envelope, snap.UnknownTokens)
		})
	}
}
