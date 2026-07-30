// safety_default_threshold_client_test.go — phase 213 (D-358), the
// end-to-end half of the LLM-edge arm. The white-box file next to this
// one pins findContextLeak; this one drives a REAL client whose
// snapshot resolves the DEFAULT threshold, so the pairing the raise
// rests on is asserted where it actually runs:
//
//   - a 64 KiB DataURL — inside the newly-inlined band — still
//     auto-materializes to an ArtifactStub carrying the artifact_fetch
//     hint, and the call SUCCEEDS;
//   - a 128 KiB RoleTool observation still fails loudly with
//     ErrContextLeak and emits llm.context_leak carrying the site and
//     the size.

package llm_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/llm"
)

// defaultThresholdSnapshot is makeSnapshot with the heavy-output
// threshold left at its RESOLVED DEFAULT rather than the 32 KiB literal
// the shared helper pins — the whole point of these arms.
func defaultThresholdSnapshot(model string, ctxTokens int) llm.ConfigSnapshot {
	snap := makeSnapshot(model, ctxTokens)
	snap.HeavyOutputThreshold = config.DefaultHeavyOutputThresholdBytes
	return snap
}

// TestSafety_DefaultThreshold_InlinedBandDataURL_MaterializesAndSucceeds
// — the counterpart half. At the raised threshold a 64 KiB attachment
// is no longer flagged, and the materialization pass still converts it
// to a stub, so no raw bytes reach the driver.
func TestSafety_DefaultThreshold_InlinedBandDataURL_MaterializesAndSucceeds(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	client, err := llm.Open(context.Background(), defaultThresholdSnapshot("m", 1_000_000), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background())
	if _, err := client.Complete(ctx, llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{{
			Role: llm.RoleUser,
			Content: llm.Content{Parts: []llm.ContentPart{{
				Type:  llm.PartImage,
				Image: &llm.ImagePart{DataURL: makeImageDataURL(64 * 1024), MIME: "image/png"},
			}}},
		}},
	}); err != nil {
		t.Fatalf("Complete: %v — a 64 KiB attachment must materialize, not leak", err)
	}
}

// TestSafety_DefaultThreshold_AboveBandToolText_StillLeaksLoudly — the
// raise moved the boundary, it did not disarm the guard. The emitted
// event carries the leak site and a size at or above the threshold, so
// an operator can correlate the failure with their configuration.
func TestSafety_DefaultThreshold_AboveBandToolText_StillLeaksLoudly(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	client, err := llm.Open(context.Background(), defaultThresholdSnapshot("m", 100_000_000), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	sub := subscribeBus(t, deps.Bus, llm.EventTypeContextLeak)

	ctx := withIdentity(t, context.Background())
	leaky := strings.Repeat("X", config.DefaultHeavyOutputThresholdBytes)
	callID := "call_default_leak"
	_, err = client.Complete(ctx, llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCallStructured{{ID: callID, Name: "probe"}}},
			{Role: llm.RoleTool, ToolCallID: &callID, Content: llm.Content{Text: &leaky}},
		},
	})
	if !errors.Is(err, llm.ErrContextLeak) {
		t.Fatalf("err = %v, want ErrContextLeak at %d bytes", err, len(leaky))
	}

	select {
	case ev := <-sub.Events():
		p, ok := ev.Payload.(llm.ContextLeakPayload)
		if !ok {
			t.Fatalf("event payload type = %T, want llm.ContextLeakPayload", ev.Payload)
		}
		if !strings.Contains(p.LeakSite, "Messages[1].Content.Text") {
			t.Errorf("payload.LeakSite = %q does not name the leak site", p.LeakSite)
		}
		if p.SizeBytes < int64(config.DefaultHeavyOutputThresholdBytes) {
			t.Errorf("payload.SizeBytes = %d, want >= %d", p.SizeBytes, config.DefaultHeavyOutputThresholdBytes)
		}
		if p.Threshold != config.DefaultHeavyOutputThresholdBytes {
			t.Errorf("payload.Threshold = %d, want the resolved default %d",
				p.Threshold, config.DefaultHeavyOutputThresholdBytes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not observe llm.context_leak within 2s")
	}
}
