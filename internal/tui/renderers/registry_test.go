package renderers

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

func TestRegistry_DispatchFallbackBoundsAndConcurrentReuse(t *testing.T) {
	registry := New(map[string]Renderer{"task": func(v Value) Block { return Block{Kind: v.Kind, ID: v.ID, Title: v.Title, Summary: "known"} }})
	if got := registry.Render(Value{Kind: "task"}); got.Summary != "known" {
		t.Fatalf("known=%#v", got)
	}
	fallback := registry.Render(Value{Kind: "future", Payload: map[string]any{"secret\x1b": strings.Repeat("x", 400)}})
	if !strings.HasPrefix(fallback.Title, "Unknown") || strings.Contains(fallback.Summary, "\x1b") || len([]rune(fallback.Summary)) > defaultSummaryLimit {
		t.Fatalf("fallback=%#v", fallback)
	}
	var wait sync.WaitGroup
	for n := range 128 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			got := registry.Render(Value{Kind: "future", ID: fmt.Sprint(n), Payload: []string{"safe"}})
			if got.ID != fmt.Sprint(n) {
				t.Errorf("identity bleed: %#v", got)
			}
		}()
	}
	wait.Wait()
	if got := registry.Kinds(); len(got) != 1 || got[0] != "task" {
		t.Fatalf("kinds=%v", got)
	}
}

func TestRegistry_BuiltinsAndUnknownFallbackRedactSensitiveMetadata(t *testing.T) {
	registry := Builtins()
	for _, kind := range []string{"event", "tool", "result", "artifact", "future-kind"} {
		block := registry.Render(Value{Kind: kind, Payload: map[string]any{"token": "raw", "nested": map[string]any{"api_key": "raw", "safe": "visible"}}})
		if strings.Contains(block.Summary, "raw") || !strings.Contains(block.Summary, "[REDACTED]") || !strings.Contains(block.Summary, "visible") {
			t.Fatalf("kind %q summary=%q", kind, block.Summary)
		}
	}
}

// The 32768-byte body is retargeted onto the named fold constant
// (phase 213). As a literal it PASSED whether or not the fold tracked
// the LLM-context heavy-output threshold — a coincidence, not a test.
// Sourced on heavyFoldThreshold it is the pin's mutation witness:
// re-aliasing the fold to config.DefaultHeavyOutputThresholdBytes
// (128 KiB) makes this body fall below the bound and the
// "artifact reference required" assertion below FAILS.
func TestRegistry_ArbitraryTypedPayloadRecursiveRedactionAndHeavyRefOnly(t *testing.T) {
	type nested struct {
		Authorization string            `json:"authorization"`
		Labels        map[string]string `json:"labels"`
		Body          string            `json:"body"`
	}
	block := Builtins().Render(Value{Kind: "future\x1b", ID: "id\x00", Title: "title\x1b[31m", Status: "ok\r", Payload: nested{Authorization: "Bearer raw-secret", Labels: map[string]string{"password": "raw-password", "safe": "visible\x1b"}, Body: strings.Repeat("x", heavyFoldThreshold)}})
	for _, forbidden := range []string{"raw-secret", "raw-password", "\x1b", "\x00", strings.Repeat("x", 1000)} {
		if strings.Contains(block.Kind+block.ID+block.Title+block.Status+block.Summary, forbidden) {
			t.Fatalf("unsafe display field survived: %#v", block)
		}
	}
	if !strings.Contains(block.Summary, "[REDACTED]") || !strings.Contains(block.Summary, "artifact reference required") {
		t.Fatalf("normalization=%#v", block)
	}
}

// TestRegistry_HeavyFold_PinnedAwayFromLLMContextThreshold — phase 213
// (D-358). The terminal fold answers "how much can a scrollback
// absorb", not "how many bytes may enter a prompt", so it is pinned at
// 32 KiB while the LLM-context threshold moved to 128 KiB. The 64 KiB
// case is the one that would silently start rendering if the alias
// came back.
func TestRegistry_HeavyFold_PinnedAwayFromLLMContextThreshold(t *testing.T) {
	if heavyFoldThreshold != 32*1024 {
		t.Fatalf("heavyFoldThreshold = %d, want 32768 (pinned)", heavyFoldThreshold)
	}
	if heavyFoldThreshold != config.DefaultConsoleInlinePayloadBytes {
		t.Errorf("heavyFoldThreshold = %d; the terminal fold and the Console inline bound "+
			"both answer a human-facing question and are expected to coincide at %d",
			heavyFoldThreshold, config.DefaultConsoleInlinePayloadBytes)
	}
	if heavyFoldThreshold == config.DefaultHeavyOutputThresholdBytes {
		t.Fatal("the terminal fold must not alias the LLM-context heavy-output threshold")
	}
	for _, size := range []int{heavyFoldThreshold, 64 * 1024} {
		if got := Normalize(strings.Repeat("x", size)); got != "[HEAVY CONTENT OMITTED: artifact reference required]" {
			t.Errorf("a %d-byte string normalized to %.60q, want the fold marker", size, got)
		}
		if got := Normalize([]byte(strings.Repeat("x", size))); got != "[HEAVY CONTENT OMITTED: artifact reference required]" {
			t.Errorf("a %d-byte slice normalized to %.60q, want the fold marker", size, got)
		}
	}
	if got := Normalize(strings.Repeat("x", heavyFoldThreshold-1)); got == "[HEAVY CONTENT OMITTED: artifact reference required]" {
		t.Error("a string one byte under the fold was folded")
	}
}
