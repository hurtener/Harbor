package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Ordinary provider responses are scripted; request construction, Bifrost,
// retention, SQLite, compaction, tool dispatch and exact edits are production.
func TestSampleSeparateInvocations(t *testing.T) {
	t.Setenv("PORTABLE_CONTEXT_API_KEY", "synthetic-only")
	var mu sync.Mutex
	decisions, summaries := 0, 0
	var observed []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 256*1024))
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		msg := map[string]any{"role": "assistant", "content": "The requested work is recorded."}
		reason := "stop"
		if strings.Contains(string(body), "You summarize historical agent execution") {
			summaries++
			msg["content"] = `{"goals":["edit sample-document"],"facts":["The complete read observed version 7; preserve the footer"],"pending":["continue the requested edit"],"last_output_digest":"document read complete","note":"fixture summary"}`
		} else {
			observed = append(observed, string(body))
			decisions++
			name, args := "", ""
			switch decisions {
			case 1:
				name, args = "document_read", `{"resource_id":"sample-document"}`
			case 3:
				name, args = "document_replace", `{"resource_id":"sample-document","expected_version":7,"old":"<h1>Draft</h1>","new":"<h1>Approved</h1>"}`
			}
			if name != "" {
				msg["content"] = nil
				msg["tool_calls"] = []any{map[string]any{"id": fmt.Sprintf("call-%d", decisions), "type": "function", "function": map[string]any{"name": name, "arguments": args}}}
				reason = "tool_calls"
			}
		}
		var request struct {
			Stream bool `json:"stream"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Error(err)
			return
		}
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			if calls, ok := msg["tool_calls"].([]any); ok {
				for i, raw := range calls {
					call, ok := raw.(map[string]any)
					if !ok {
						t.Error("invalid fixture call")
						return
					}
					call["index"] = i
				}
			}
			for _, choice := range []map[string]any{
				{"index": 0, "delta": msg},
				{"index": 0, "delta": map[string]any{}, "finish_reason": reason},
			} {
				data, err := json.Marshal(map[string]any{"id": "fixture", "object": "chat.completion.chunk", "model": "sample-model", "choices": []any{choice}})
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
					t.Error(err)
					return
				}
			}
			if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
				t.Error(err)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "object": "chat.completion", "model": "sample-model", "choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": reason}}, "usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 30, "total_tokens": 130}}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	for _, prompt := range []string{"Read sample-document completely; do not change it.", "Rename its heading to Approved; preserve the footer."} {
		var out, diagnostics bytes.Buffer
		if err := command(t.Context(), []string{"-provider", "openai", "-model", "sample-model", "-context-window", "32768", "-token-budget", "1800", "-base-url", server.URL, "-data-dir", dir, "-prompt", prompt}, &out, &diagnostics); err != nil {
			t.Fatalf("sample run: %v", err)
		}
		var result sampleResult
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.DiagnosticsAvailable || result.DiagnosticsTruncated || len(result.Context) == 0 {
			t.Fatalf("missing answer or diagnostics: %v", err)
		}
		for _, preparation := range result.Context {
			if preparation.Identity.RunID != result.RunID || (preparation.MaintenanceOrdinal > 0 && preparation.History != nil) {
				t.Fatal("diagnostics mixed runs or inherited maintenance history")
			}
		}
	}
	var output bytes.Buffer
	if err := command(t.Context(), []string{"-data-dir", dir, "-inspect"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	var doc document
	if err := json.Unmarshal(output.Bytes(), &doc); err != nil || doc.Version != 8 || !strings.Contains(doc.Source, "<h1>Approved</h1>") || !strings.Contains(doc.Source, "Keep this footer") {
		t.Fatalf("actual persisted edit failed: version=%d err=%v", doc.Version, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if decisions != 4 || summaries == 0 || !strings.Contains(observed[2], "sample-document") || !strings.Contains(observed[2], "version 7") {
		t.Fatalf("continuation or compaction missing: decisions=%d summaries=%d", decisions, summaries)
	}
	// The next invocation receives a source-bound checkpoint rather than
	// relying on the intentionally uninformative final assistant answer.
	if strings.Contains(observed[2], strings.Repeat("x", 1000)) {
		t.Fatal("covered historical body was not compacted")
	}
}

func TestSampleInputValidation(t *testing.T) {
	for _, args := range [][]string{nil, {"-inspect", "-prompt", "write"}, {"-provider", "openai", "-prompt", "go"}, {"unexpected"}} {
		if err := command(t.Context(), args, io.Discard, io.Discard); err == nil {
			t.Errorf("invalid args accepted: %v", args)
		}
	}
}
