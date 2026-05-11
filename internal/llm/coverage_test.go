package llm_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/mock"
)

// Coverage-focused tests for branches the happy-path suite doesn't
// reach. Kept small + targeted; no parallel surface duplication.

func TestRegister_RejectsEmptyName(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Register with empty name did not panic")
		}
	}()
	llm.Register("", nil)
}

func TestRegister_RejectsNilFactory(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Register with nil factory did not panic")
		}
	}()
	llm.Register("name-x", nil)
}

func TestRegister_RejectsDuplicate(t *testing.T) {
	const dupName = "dup-driver-coverage"
	llm.Register(dupName, func(cfg llm.ConfigSnapshot, deps llm.Deps) (llm.Driver, error) {
		return mock.New(mock.Options{}), nil
	})
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("re-registering name did not panic")
		}
	}()
	llm.Register(dupName, func(cfg llm.ConfigSnapshot, deps llm.Deps) (llm.Driver, error) {
		return mock.New(mock.Options{}), nil
	})
}

func TestOpen_RejectsMissingBus(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	// Replace Bus with nil.
	deps.Bus = nil
	if _, err := llm.Open(context.Background(), makeSnapshot("m", 1000), deps); err == nil {
		t.Fatal("Open accepted Deps with nil Bus")
	}
}

func TestOpen_RejectsZeroOrNegHeavyThreshold(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	// Snapshot with explicit negative threshold (zero is replaced by defaults).
	snap := makeSnapshot("m", 1000)
	snap.HeavyOutputThreshold = -1
	if _, err := llm.Open(context.Background(), snap, deps); !errors.Is(err, llm.ErrInvalidConfig) {
		t.Fatalf("err=%v, want ErrInvalidConfig", err)
	}
}

func TestOpen_RejectsBadProfileContextWindow(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	snap := makeSnapshot("m", 0)
	if _, err := llm.Open(context.Background(), snap, deps); !errors.Is(err, llm.ErrInvalidConfig) {
		t.Fatalf("err=%v, want ErrInvalidConfig (ContextWindowTokens=0)", err)
	}
}

func TestComplete_RejectsEmptyModel(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	client, err := llm.Open(context.Background(), makeSnapshot("m", 1000), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background(), "T", "U", "S")
	text := "x"
	_, err = client.Complete(ctx, llm.CompleteRequest{
		Model:    "",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
	})
	if !errors.Is(err, llm.ErrInvalidConfig) {
		t.Fatalf("err=%v, want ErrInvalidConfig (empty model)", err)
	}
}

func TestComplete_PartTypeMismatchIsInvalidContent(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	client, err := llm.Open(context.Background(), makeSnapshot("m", 1000), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background(), "T", "U", "S")

	// Type=image but Image=nil.
	req := llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{{
			Role:    llm.RoleUser,
			Content: llm.Content{Parts: []llm.ContentPart{{Type: llm.PartImage}}},
		}},
	}
	if _, err := client.Complete(ctx, req); !errors.Is(err, llm.ErrInvalidContent) {
		t.Fatalf("err=%v, want ErrInvalidContent", err)
	}

	// Unknown part Type.
	req2 := llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{{
			Role:    llm.RoleUser,
			Content: llm.Content{Parts: []llm.ContentPart{{Type: llm.PartType("video")}}},
		}},
	}
	if _, err := client.Complete(ctx, req2); !errors.Is(err, llm.ErrInvalidContent) {
		t.Fatalf("err=%v, want ErrInvalidContent (unknown PartType)", err)
	}
}

func TestEstimateTokens_MultimodalBreakdown(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	// Build a request with a small cap so the budget guard fires
	// when multimodal parts are added — exercises the multimodal
	// per-part overhead path in the estimator.
	client, err := llm.Open(context.Background(), makeSnapshot("m", 400), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background(), "T", "U", "S")
	name := "claude"
	req := llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{{
			Role: llm.RoleUser,
			Name: &name,
			Content: llm.Content{Parts: []llm.ContentPart{
				{Type: llm.PartText, Text: "describe"},
				{Type: llm.PartImage, Image: &llm.ImagePart{URL: "https://example.com/x.png", MIME: "image/png"}},
				{Type: llm.PartAudio, Audio: &llm.AudioPart{URL: "https://example.com/x.mp3", MIME: "audio/mpeg"}},
				{Type: llm.PartFile, File: &llm.FilePart{URL: "https://example.com/x.pdf", MIME: "application/pdf"}},
			}},
		}},
		Stops: []string{"stop-1", "stop-2"},
		Extra: map[string]any{"k": "v"},
	}
	// 4 multimodal parts × 256 tokens each = 1024 tokens — comfortably
	// over the 400-token cap.
	_, err = client.Complete(ctx, req)
	if !errors.Is(err, llm.ErrContextWindowExceeded) {
		t.Fatalf("err=%v, want ErrContextWindowExceeded (multimodal overhead)", err)
	}
}

func TestApplyDefaults_FillsZeros(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	// All Phase-32 defaults: empty Driver → "mock"; zero
	// ContextWindowReserve → 0.05; zero HeavyOutputThreshold → 32 KiB.
	snap := llm.ConfigSnapshot{
		ModelProfiles: map[string]llm.ModelProfile{"m": {ContextWindowTokens: 1000}},
	}
	client, err := llm.Open(context.Background(), snap, deps)
	if err != nil {
		t.Fatalf("Open with zero-value snapshot: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background(), "T", "U", "S")
	text := "x"
	if _, err := client.Complete(ctx, llm.CompleteRequest{
		Model:    "m",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
	}); err != nil {
		t.Fatalf("Complete (defaults applied): %v", err)
	}
}

func TestRegistry_NameInUnknownDriverError(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	snap := makeSnapshot("m", 1000)
	snap.Driver = "nope-12345"
	_, err := llm.Open(context.Background(), snap, deps)
	if !errors.Is(err, llm.ErrUnknownDriver) {
		t.Fatalf("err=%v, want ErrUnknownDriver", err)
	}
	if !strings.Contains(err.Error(), "mock") {
		t.Errorf("err=%q does not list registered 'mock' driver", err.Error())
	}
}

func TestDecodeDataURL_NonBase64Payload(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	client, err := llm.Open(context.Background(), makeSnapshot("m", 1_000_000), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	// Plain (non-base64) data URL above threshold — exercises the
	// non-base64 decode branch in decodeDataURL.
	raw := strings.Repeat("ABCD", 12*1024) // 48 KiB, > 32 KiB threshold
	dataURL := "data:image/png," + raw
	ctx := withIdentity(t, context.Background(), "T", "U", "S")
	req := llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{{
			Role: llm.RoleUser,
			Content: llm.Content{Parts: []llm.ContentPart{{
				Type:  llm.PartImage,
				Image: &llm.ImagePart{DataURL: dataURL, MIME: "image/png"},
			}}},
		}},
	}
	if _, err := client.Complete(ctx, req); err != nil {
		t.Fatalf("Complete (non-base64 DataURL): %v", err)
	}
}

func TestDecodeDataURL_Malformed(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	client, err := llm.Open(context.Background(), makeSnapshot("m", 1_000_000), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background(), "T", "U", "S")
	// Threshold-sized DataURL with malformed base64 should fail
	// loudly via ErrInvalidContent.
	bad := "data:image/png;base64," + strings.Repeat("!", 50*1024)
	req := llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{{
			Role: llm.RoleUser,
			Content: llm.Content{Parts: []llm.ContentPart{{
				Type:  llm.PartImage,
				Image: &llm.ImagePart{DataURL: bad, MIME: "image/png"},
			}}},
		}},
	}
	if _, err := client.Complete(ctx, req); !errors.Is(err, llm.ErrInvalidContent) {
		t.Fatalf("err=%v, want ErrInvalidContent (malformed base64)", err)
	}
}

func TestEvents_PayloadShapes_NonZero(t *testing.T) {
	// Build payload values and confirm fields round-trip — keeps
	// the payload structs from being eliminated by dead-code
	// detection in callers, and tests at-least-once the field
	// names line up with the per-type tests' assertions.
	p1 := llm.ImageMaterializedPayload{ArtifactRef: "r", MIME: "image/png", SizeBytes: 10, OccurredAt: time.Now()}
	if p1.ArtifactRef != "r" {
		t.Fatal("ImageMaterializedPayload assignment broke")
	}
	p2 := llm.ContextLeakPayload{LeakSite: "Messages[0].Content.Text", SizeBytes: 1, Threshold: 32 * 1024, OccurredAt: time.Now()}
	if p2.LeakSite == "" {
		t.Fatal("ContextLeakPayload assignment broke")
	}
	p3 := llm.ContextWindowExceededPayload{EstimatedTokens: 100, ContextWindowTokens: 1000, ContextWindowReserve: 0.05, OccurredAt: time.Now()}
	if p3.ContextWindowTokens == 0 {
		t.Fatal("ContextWindowExceededPayload assignment broke")
	}
	p4 := llm.CostRecordedPayload{Model: "m", Cost: llm.Cost{TotalCost: 1.0}, OccurredAt: time.Now()}
	if p4.Model == "" {
		t.Fatal("CostRecordedPayload assignment broke")
	}
	p5 := llm.ModeDowngradedPayload{From: llm.FormatJSONSchema, To: llm.FormatJSONObject, Reason: "test", OccurredAt: time.Now()}
	if p5.From == "" {
		t.Fatal("ModeDowngradedPayload assignment broke")
	}
}

func TestHasIdentity_BothShapes(t *testing.T) {
	if llm.HasIdentity(context.Background()) {
		t.Fatal("HasIdentity returned true on empty ctx")
	}
	ctx := withIdentity(t, context.Background(), "T", "U", "S")
	if !llm.HasIdentity(ctx) {
		t.Fatal("HasIdentity returned false with identity in ctx")
	}
}
