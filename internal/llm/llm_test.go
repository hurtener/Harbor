package llm_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	artifactsinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	_ "github.com/hurtener/Harbor/internal/llm/mock"
)

// makeDeps returns a fully-wired llm.Deps backed by real in-memory
// drivers + a real patterns redactor. The bus is closed via the
// returned cleanup func; the artifact store is closed by the
// cleanup too.
//
// AGENTS.md §17.3: integration shapes use REAL drivers on the seam.
// No mocks at the boundary.
func makeDeps(t *testing.T) (llm.Deps, func()) {
	t.Helper()

	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
		ReplayBufferSize:         100,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}

	store, err := artifactsinmem.New(config.ArtifactsConfig{
		Driver:                    "inmem",
		HeavyOutputThresholdBytes: 32 * 1024,
	})
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("artifacts.New: %v", err)
	}

	cleanup := func() {
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
	}
	return llm.Deps{Artifacts: store, Bus: bus}, cleanup
}

// makeSnapshot returns a llm.ConfigSnapshot wired for the mock
// driver with a single model profile sized for tests.
func makeSnapshot(model string, ctxTokens int) llm.ConfigSnapshot {
	return llm.ConfigSnapshot{
		Driver:               "mock",
		ContextWindowReserve: 0.05,
		HeavyOutputThreshold: 32 * 1024,
		ModelProfiles: map[string]llm.ModelProfile{
			model: {
				ContextWindowTokens: ctxTokens,
				TokenEstimator:      "chars_div_4",
			},
		},
	}
}

// withIdentity attaches the deterministic test identity (T/U/S) to ctx.
func withIdentity(t *testing.T, ctx context.Context) context.Context {
	t.Helper()
	c, err := identity.With(ctx, identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return c
}

func TestOpen_RejectsMissingDeps(t *testing.T) {
	if _, err := llm.Open(context.Background(), makeSnapshot("m", 1000), llm.Deps{}); err == nil {
		t.Fatalf("Open accepted Deps with nil Artifacts")
	}
}

func TestOpen_RejectsUnknownDriver(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	snap := makeSnapshot("m", 1000)
	snap.Driver = "no-such-driver"
	_, err := llm.Open(context.Background(), snap, deps)
	if !errors.Is(err, llm.ErrUnknownDriver) {
		t.Fatalf("err=%v, want ErrUnknownDriver", err)
	}
	if !strings.Contains(err.Error(), "registered") {
		t.Errorf("err=%q should list registered drivers", err.Error())
	}
}

func TestOpen_RejectsBadReserve(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	snap := makeSnapshot("m", 1000)
	snap.ContextWindowReserve = 1.5
	if _, err := llm.Open(context.Background(), snap, deps); !errors.Is(err, llm.ErrInvalidConfig) {
		t.Fatalf("err=%v, want ErrInvalidConfig", err)
	}
}

func TestComplete_RejectsMissingIdentity(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	client, err := llm.Open(context.Background(), makeSnapshot("m", 1000), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	text := "hi"
	req := llm.CompleteRequest{
		Model:    "m",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
	}
	_, err = client.Complete(context.Background(), req)
	if !errors.Is(err, llm.ErrIdentityMissing) {
		t.Fatalf("err=%v, want ErrIdentityMissing", err)
	}
}

func TestComplete_TextRoundTrip(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	client, err := llm.Open(context.Background(), makeSnapshot("m", 1000), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background())
	text := "hello world"
	resp, err := client.Complete(ctx, llm.CompleteRequest{
		Model:    "m",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !strings.Contains(resp.Content, "hello world") {
		t.Errorf("resp.Content=%q does not echo input", resp.Content)
	}
	if resp.Usage.TotalTokens <= 0 {
		t.Errorf("Usage.TotalTokens=%d, want > 0", resp.Usage.TotalTokens)
	}
}

func TestComplete_RejectsUnsupportedModel(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	client, err := llm.Open(context.Background(), makeSnapshot("known-model", 1000), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background())
	text := "x"
	_, err = client.Complete(ctx, llm.CompleteRequest{
		Model:    "unknown-model",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
	})
	if !errors.Is(err, llm.ErrUnsupportedModel) {
		t.Fatalf("err=%v, want ErrUnsupportedModel", err)
	}
}

func TestComplete_RejectsInvalidContent(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	client, err := llm.Open(context.Background(), makeSnapshot("m", 1000), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background())

	// Both Text and Parts set → invalid.
	text := "x"
	bad := llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{{
			Role: llm.RoleUser,
			Content: llm.Content{
				Text:  &text,
				Parts: []llm.ContentPart{{Type: llm.PartText, Text: "y"}},
			},
		}},
	}
	if _, err := client.Complete(ctx, bad); !errors.Is(err, llm.ErrInvalidContent) {
		t.Fatalf("err=%v, want ErrInvalidContent (both Text+Parts)", err)
	}

	// Neither Text nor Parts → invalid.
	empty := llm.CompleteRequest{
		Model:    "m",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{}}},
	}
	if _, err := client.Complete(ctx, empty); !errors.Is(err, llm.ErrInvalidContent) {
		t.Fatalf("err=%v, want ErrInvalidContent (neither set)", err)
	}
}

func TestComplete_CloseIdempotentAndPostClose(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	client, err := llm.Open(context.Background(), makeSnapshot("m", 1000), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("Close 1: %v", err)
	}
	if err := client.Close(context.Background()); err != nil {
		t.Errorf("Close 2 (idempotent): %v", err)
	}
	ctx := withIdentity(t, context.Background())
	text := "x"
	_, err = client.Complete(ctx, llm.CompleteRequest{
		Model:    "m",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
	})
	if !errors.Is(err, llm.ErrClientClosed) {
		t.Errorf("err=%v, want ErrClientClosed after Close", err)
	}
}

// TestComplete_RejectsOrphanToolCall asserts the safety-pass orphan
// validator: every assistant message with ToolCalls MUST be
// followed by RoleTool messages whose ToolCallIDs match each
// ToolCalls[i].ID per OpenAI's wire spec. The safety pass rejects
// loudly with ErrOrphanToolCall so any producer-side regression
// fails at the LLM-client edge.
func TestComplete_RejectsOrphanToolCall(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	client, err := llm.Open(context.Background(), makeSnapshot("m", 1000), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()
	ctx := withIdentity(t, context.Background())

	userText := "find a video"
	asstContent := llm.Content{} // zero-value (already-valid for the with-tool_calls carve-out)

	// Case 1: assistant tool_call with NO following RoleTool message.
	orphan := llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{
			{Role: llm.RoleUser, Content: llm.Content{Text: &userText}},
			{
				Role:      llm.RoleAssistant,
				Content:   asstContent,
				ToolCalls: []llm.ToolCallStructured{{ID: "call_x1", Name: "foo"}},
			},
			// MISSING: tool message with ToolCallID="call_x1"
		},
	}
	if _, err := client.Complete(ctx, orphan); !errors.Is(err, llm.ErrOrphanToolCall) {
		t.Fatalf("orphan tool_call: err=%v, want ErrOrphanToolCall", err)
	}

	// Case 2: tool message ToolCallID does not match assistant's.
	mismatchID := "call_other"
	mismatchToolText := "result"
	mismatch := llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{
			{Role: llm.RoleUser, Content: llm.Content{Text: &userText}},
			{
				Role:      llm.RoleAssistant,
				Content:   asstContent,
				ToolCalls: []llm.ToolCallStructured{{ID: "call_x2", Name: "foo"}},
			},
			{
				Role:       llm.RoleTool,
				Content:    llm.Content{Text: &mismatchToolText},
				ToolCallID: &mismatchID,
			},
		},
	}
	if _, err := client.Complete(ctx, mismatch); !errors.Is(err, llm.ErrOrphanToolCall) {
		t.Fatalf("mismatched ToolCallID: err=%v, want ErrOrphanToolCall", err)
	}

	// Case 3: RoleTool message with missing ToolCallID.
	toolText := "r"
	missingID := llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{
			{Role: llm.RoleUser, Content: llm.Content{Text: &userText}},
			{
				Role:      llm.RoleAssistant,
				Content:   asstContent,
				ToolCalls: []llm.ToolCallStructured{{ID: "call_x3", Name: "foo"}},
			},
			{Role: llm.RoleTool, Content: llm.Content{Text: &toolText}}, // ToolCallID nil
		},
	}
	if _, err := client.Complete(ctx, missingID); !errors.Is(err, llm.ErrOrphanToolCall) {
		t.Fatalf("missing ToolCallID: err=%v, want ErrOrphanToolCall", err)
	}
}

// TestComplete_AcceptsPairedToolCall confirms the canonical paired
// sequence passes validation: assistant tool_call followed by
// matching RoleTool message → safety pass goes through.
func TestComplete_AcceptsPairedToolCall(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	client, err := llm.Open(context.Background(), makeSnapshot("m", 1000), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()
	ctx := withIdentity(t, context.Background())

	userText := "find a video"
	toolText := "result"
	callID := "call_paired"
	paired := llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{
			{Role: llm.RoleUser, Content: llm.Content{Text: &userText}},
			{
				Role:      llm.RoleAssistant,
				Content:   llm.Content{}, // empty content allowed when ToolCalls present
				ToolCalls: []llm.ToolCallStructured{{ID: callID, Name: "foo"}},
			},
			{
				Role:       llm.RoleTool,
				Content:    llm.Content{Text: &toolText},
				ToolCallID: &callID,
			},
		},
	}
	// The mock driver echoes; we only care that the safety pass does
	// NOT reject with ErrOrphanToolCall.
	if _, err := client.Complete(ctx, paired); errors.Is(err, llm.ErrOrphanToolCall) {
		t.Fatalf("paired tool_call rejected as orphan: %v", err)
	}
	_ = strings.TrimSpace // keep import in case the file is trimmed
}

func TestRegisteredDrivers_IncludesMock(t *testing.T) {
	found := false
	for _, n := range llm.RegisteredDrivers() {
		if n == "mock" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("RegisteredDrivers()=%v missing 'mock'", llm.RegisteredDrivers())
	}
}

// Compile-time guard — the package boundary holds (no leak of
// concrete artifact-store types into llm.Deps).
var _ artifacts.ArtifactStore = (artifacts.ArtifactStore)(nil)
