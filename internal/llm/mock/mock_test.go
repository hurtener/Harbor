package mock_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/mock"
)

func TestMock_TextRoundTrip(t *testing.T) {
	drv := mock.New(mock.Options{})
	defer func() { _ = drv.Close(context.Background()) }()

	ctx, _ := identity.With(context.Background(), identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"})
	text := "hello"
	resp, err := drv.Complete(ctx, llm.CompleteRequest{
		Model:    "m",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !strings.Contains(resp.Content, "hello") {
		t.Errorf("resp.Content=%q does not echo input", resp.Content)
	}
}

func TestMock_MultimodalRoundTrip(t *testing.T) {
	drv := mock.New(mock.Options{})
	defer func() { _ = drv.Close(context.Background()) }()

	ctx, _ := identity.With(context.Background(), identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"})
	resp, err := drv.Complete(ctx, llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{{
			Role: llm.RoleUser,
			Content: llm.Content{Parts: []llm.ContentPart{
				{Type: llm.PartText, Text: "describe this image"},
				{Type: llm.PartImage, Image: &llm.ImagePart{URL: "https://example.com/x.png", MIME: "image/png"}},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content == "" {
		t.Errorf("multimodal resp.Content empty")
	}
}

func TestMock_StreamingCallbacks(t *testing.T) {
	drv := mock.New(mock.Options{StreamChunks: 5})
	defer func() { _ = drv.Close(context.Background()) }()

	ctx, _ := identity.With(context.Background(), identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"})
	var (
		contentSeen   strings.Builder
		reasoningSeen strings.Builder
		contentDone   bool
	)
	text := "abcdefghijklmnopqrst"
	_, err := drv.Complete(ctx, llm.CompleteRequest{
		Model:    "m",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
		Stream:   true,
		OnContent: func(delta string, done bool) {
			contentSeen.WriteString(delta)
			if done {
				contentDone = true
			}
		},
		OnReasoning: func(delta string, done bool) {
			reasoningSeen.WriteString(delta)
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !contentDone {
		t.Errorf("content callback never fired done=true")
	}
	if contentSeen.Len() == 0 {
		t.Errorf("content callback never observed any delta")
	}
	if reasoningSeen.Len() == 0 {
		t.Errorf("reasoning callback never observed any delta")
	}
}

func TestMock_CancellationDuringStream(t *testing.T) {
	drv := mock.New(mock.Options{
		StreamChunks:   8,
		PreStreamDelay: 50 * time.Millisecond,
	})
	defer func() { _ = drv.Close(context.Background()) }()

	ctx, cancel := context.WithCancel(context.Background())
	ctx, _ = identity.With(ctx, identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"})

	// Pre-cancel ctx so the stream loop's ctx.Err() guard (or the
	// PreStreamDelay select's ctx.Done() branch) fires
	// deterministically. AGENTS.md §11: no time.Sleep for
	// synchronisation. Both code paths return ctx.Err() — what we're
	// testing is the surface contract (cancelled ctx ⇒
	// context.Canceled), not which guard observes it first.
	cancel()

	text := "abcdefghijklmnopqrst"
	_, err := drv.Complete(ctx, llm.CompleteRequest{
		Model:     "m",
		Messages:  []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
		Stream:    true,
		OnContent: func(string, bool) {},
	})
	if err == nil {
		t.Fatal("Complete returned nil during cancellation, want ctx.Err()")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err=%v, want context.Canceled", err)
	}
}

func TestMock_ForcedError(t *testing.T) {
	sentinel := errors.New("test: synthetic failure")
	drv := mock.New(mock.Options{ForcedError: sentinel})
	defer func() { _ = drv.Close(context.Background()) }()

	ctx, _ := identity.With(context.Background(), identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"})
	text := "x"
	_, err := drv.Complete(ctx, llm.CompleteRequest{
		Model:    "m",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("err=%v, want forced %v", err, sentinel)
	}
}

func TestMock_PostCloseReturnsErrClosed(t *testing.T) {
	drv := mock.New(mock.Options{})
	if err := drv.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ctx, _ := identity.With(context.Background(), identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"})
	text := "x"
	_, err := drv.Complete(ctx, llm.CompleteRequest{
		Model:    "m",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
	})
	if !errors.Is(err, llm.ErrClientClosed) {
		t.Errorf("err=%v, want ErrClientClosed", err)
	}
}

func TestMock_SeenIdentityChannel(t *testing.T) {
	seen := make(chan identity.Quadruple, 1)
	drv := mock.New(mock.Options{SeenIdentity: seen})
	defer func() { _ = drv.Close(context.Background()) }()

	ctx, _ := identity.With(context.Background(), identity.Identity{TenantID: "T2", UserID: "U2", SessionID: "S2"})
	text := "x"
	if _, err := drv.Complete(ctx, llm.CompleteRequest{
		Model:    "m",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	select {
	case got := <-seen:
		if got.TenantID != "T2" {
			t.Errorf("got.TenantID=%q, want T2", got.TenantID)
		}
	case <-time.After(time.Second):
		t.Fatal("SeenIdentity channel did not receive")
	}
}
