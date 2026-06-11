// Cross-package wiring test (CLAUDE.md §17.1): the Phase 84b
// disposition policy resolves `provider_native`, the planner
// materializer renders the flagged typed part, and THIS driver's
// upload pass closes the seam — real inmem artifact store + real
// inmem event bus on the boundary, identity propagation through every
// layer, plus one failure mode (an artifact ref outside the calling
// identity's scope fails closed).
package bifrost

import (
	"context"
	"strings"
	"testing"
	"time"

	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
)

// TestE2E_ProviderNative_MaterializerToDriver — the full
// policy→materializer→driver path with no run loop:
// EffectiveDisposition honours provider_native, the materializer
// emits the flagged part carrying the ArtifactStub, the driver
// uploads the store's bytes and the wire carries the file_id; the
// upload event lands on the bus under the calling identity.
func TestE2E_ProviderNative_MaterializerToDriver(t *testing.T) {
	bus, busCleanup := openBus(t)
	defer busCleanup()
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{llm.EventTypeProviderFileUploaded},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	store := openArtifactStore(t)
	payload := []byte(strings.Repeat("over-threshold-image-bytes", 2048)) // ~52 KiB
	stub := putTestArtifact(t, store, "s-e2e", "image/png", payload)

	// Phase 84b policy core: a per-agent policy resolves
	// provider_native for image/*; the capability pass honours it.
	policy := planner.DispositionPolicy{
		ByMIME: map[string]planner.AttachmentDisposition{"image/*": planner.DispositionProviderNative},
	}
	resolved, layer := planner.ResolveDisposition("", policy, "image/png")
	if resolved != planner.DispositionProviderNative || layer != planner.DispositionLayerAgentPolicy {
		t.Fatalf("resolved = %q via %q, want provider_native via agent_policy", resolved, layer)
	}
	effective, degradation := planner.EffectiveDisposition(resolved, "image/png", nil)
	if effective != planner.DispositionProviderNative || degradation != nil {
		t.Fatalf("effective = %q degradation=%+v, want honoured provider_native", effective, degradation)
	}

	// Planner materializer renders the flagged part.
	view := planner.InputArtifactView{
		ID: stub.Ref, MIME: "image/png", SizeBytes: stub.SizeBytes, Disposition: effective,
	}
	content := planner.MaterializeInputContent("what is in this image?", []planner.InputArtifactView{view}, nil)

	stubC := newStubClient()
	drv := newDriverWithClientAndStore(stubC, bfschemas.OpenAI, bus, store)
	defer func() { _ = drv.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background(), "s-e2e")
	if _, err := drv.Complete(ctx, llm.CompleteRequest{
		Model:    "m",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: content}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	uploads := stubC.recordedUploads()
	if len(uploads) != 1 || len(uploads[0].File) != len(payload) {
		t.Fatalf("uploads = %d, want 1 carrying the store's %d bytes", len(uploads), len(payload))
	}
	blocks := findFileBlocks(stubC.lastRequest())
	if len(blocks) != 1 || blocks[0].FileID == nil || *blocks[0].FileID == "" {
		t.Fatalf("wire file blocks = %+v, want the uploaded file_id", blocks)
	}
	select {
	case ev := <-sub.Events():
		p := ev.Payload.(llm.ProviderFileUploadedPayload)
		if p.Identity.TenantID != "T" || p.Identity.SessionID != "s-e2e" {
			t.Errorf("event identity = %+v, want the calling triple", p.Identity)
		}
		if p.ArtifactRef != stub.Ref || p.Modality != "image" {
			t.Errorf("event payload = %+v, want ref %q modality image", p, stub.Ref)
		}
	case <-time.After(2 * time.Second):
		t.Error("no llm.provider_file.uploaded within 2s")
	}

	// Failure mode: the same materialized content under a DIFFERENT
	// session's identity fails closed — the artifact is invisible
	// outside its scope and the driver refuses to proceed.
	ctxOther := withIdentity(t, context.Background(), "s-other")
	if _, err := drv.Complete(ctxOther, llm.CompleteRequest{
		Model:    "m",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: content}},
	}); err == nil || !strings.Contains(err.Error(), "not found in scope") {
		t.Fatalf("cross-scope err = %v, want loud not-found-in-scope failure", err)
	}
}
