package drafter

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
)

// drafter_test.go — the safety-wrapped LLM authoring adapter:
// refusal, malformed output, authority-field injection, unknown
// fields, bounds, timeout, cancellation, and client failure all fail
// loud and produce NO artifact (the adapter has no write path at all).

func newTestAdapter(t *testing.T, c stubClient) *Adapter {
	t.Helper()
	a, err := New(c, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func TestAdapter_Draft_HappyPath(t *testing.T) {
	a := newTestAdapter(t, stubClient{content: validDraftContent()})
	ctx := runCtx(t, testQuad("t", "u", "s", "r"))
	skill, err := a.Draft(ctx, "triage the inbox", "")
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}
	if skill.Name != "triage-inbox" || len(skill.Steps) != 3 || skill.Trigger == "" {
		t.Fatalf("unexpected draft: %+v", skill)
	}
	if err := skill.Validate(); err != nil {
		t.Fatalf("draft failed canonical validation: %v", err)
	}
}

func TestAdapter_Draft_CanonicalizesName(t *testing.T) {
	content := strings.Replace(validDraftContent(), `"triage-inbox"`, `"Triage Inbox"`, 1)
	a := newTestAdapter(t, stubClient{content: content})
	skill, err := a.Draft(runCtx(t, testQuad("t", "u", "s", "r")), "intent", "")
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}
	if skill.Name != "triage inbox" {
		t.Fatalf("name = %q, want canonical %q", skill.Name, "triage inbox")
	}
}

func TestAdapter_Draft_RequiresIdentity(t *testing.T) {
	a := newTestAdapter(t, stubClient{content: validDraftContent()})
	_, err := a.Draft(context.Background(), "intent", "")
	if !errors.Is(err, ErrMissingIdentity) {
		t.Fatalf("err = %v, want ErrMissingIdentity", err)
	}
}

func TestAdapter_Draft_EmptyIntent(t *testing.T) {
	a := newTestAdapter(t, stubClient{content: validDraftContent()})
	_, err := a.Draft(runCtx(t, testQuad("t", "u", "s", "r")), "   ", "")
	if !errors.Is(err, ErrIntentRequired) {
		t.Fatalf("err = %v, want ErrIntentRequired", err)
	}
}

func TestAdapter_Draft_IntentTooLarge(t *testing.T) {
	a := newTestAdapter(t, stubClient{content: validDraftContent()})
	_, err := a.Draft(runCtx(t, testQuad("t", "u", "s", "r")), strings.Repeat("i", MaxIntentRunes+1), "")
	if !errors.Is(err, ErrIntentTooLarge) {
		t.Fatalf("err = %v, want ErrIntentTooLarge", err)
	}
}

func TestAdapter_Draft_FeedbackTooLarge(t *testing.T) {
	a := newTestAdapter(t, stubClient{content: validDraftContent()})
	_, err := a.Draft(runCtx(t, testQuad("t", "u", "s", "r")), "intent", strings.Repeat("f", MaxFeedbackRunes+1))
	if !errors.Is(err, ErrFeedbackTooLarge) {
		t.Fatalf("err = %v, want ErrFeedbackTooLarge", err)
	}
}

func TestAdapter_Draft_ModelOutputTooLarge(t *testing.T) {
	a, err := New(stubClient{content: validDraftContent()}, Options{MaxModelOutputBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Draft(runCtx(t, testQuad("t", "u", "s", "r")), "intent", "")
	if !errors.Is(err, ErrModelOutputTooLarge) {
		t.Fatalf("err = %v, want ErrModelOutputTooLarge", err)
	}
}

func TestAdapter_Draft_RejectsAuthorityFields(t *testing.T) {
	base := `{"name":"x","trigger":"t","steps":["s"]`
	for _, field := range []string{
		"origin", "origin_ref", "content_hash", "scope", "tenant", "tenant_id",
		"user", "user_id", "session", "session_id", "agent", "agent_id", "model",
		"owner", "audience", "membership", "capabilities", "policy", "policy_hash",
		"permissions", "provenance", "persist", "replace", "publish", "publication",
		"grant", "grants", "tool_visibility", "oauth", "approval", "authority",
	} {
		t.Run(field, func(t *testing.T) {
			content := base + `,"` + field + `":null}`
			a := newTestAdapter(t, stubClient{content: content})
			_, err := a.Draft(runCtx(t, testQuad("t", "u", "s", "r")), "intent", "")
			if !errors.Is(err, ErrForbiddenAuthorityField) {
				t.Fatalf("err = %v, want ErrForbiddenAuthorityField", err)
			}
			if !strings.Contains(err.Error(), field) {
				t.Fatalf("err %q does not name the field", err)
			}
		})
	}
}

func TestAdapter_Draft_RejectsUnknownFields(t *testing.T) {
	content := `{"name":"x","trigger":"t","steps":["s"],"bogus":1}`
	a := newTestAdapter(t, stubClient{content: content})
	_, err := a.Draft(runCtx(t, testQuad("t", "u", "s", "r")), "intent", "")
	if !errors.Is(err, ErrMalformedModelOutput) {
		t.Fatalf("err = %v, want ErrMalformedModelOutput", err)
	}
}

func TestAdapter_Draft_Refusal(t *testing.T) {
	for _, content := range []string{
		`{"refusal":"I cannot do that","name":"x","trigger":"t","steps":["s"]}`,
		`{"error":"model error","name":"x","trigger":"t","steps":["s"]}`,
	} {
		a := newTestAdapter(t, stubClient{content: content})
		_, err := a.Draft(runCtx(t, testQuad("t", "u", "s", "r")), "intent", "")
		if !errors.Is(err, ErrModelRefused) {
			t.Fatalf("content %q: err = %v, want ErrModelRefused", content, err)
		}
	}
}

func TestAdapter_Draft_Malformed(t *testing.T) {
	cases := map[string]string{
		"not JSON":       "this is not json",
		"empty":          "",
		"trailing JSON":  `{"name":"x","trigger":"t","steps":["s"]}{}`,
		"schema invalid": `{"name":"x","trigger":"t"}`,
		"empty step":     `{"name":"x","trigger":"t","steps":["  "]}`,
		"name empty":     `{"name":" ","trigger":"t","steps":["s"]}`,
		"trigger absent": `{"name":"x","steps":["s"]}`,
		"trailing text":  `{"name":"x","trigger":"t","steps":["s"]} trailing`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			a := newTestAdapter(t, stubClient{content: content})
			_, err := a.Draft(runCtx(t, testQuad("t", "u", "s", "r")), "intent", "")
			if !errors.Is(err, ErrMalformedModelOutput) {
				t.Fatalf("err = %v, want ErrMalformedModelOutput", err)
			}
			// Limit errors must not carry raw model output.
			if strings.Contains(err.Error(), "this is not json") {
				t.Fatalf("error leaks raw model output: %v", err)
			}
		})
	}
}

func TestAdapter_Draft_ClientError(t *testing.T) {
	boom := errors.New("provider down")
	a := newTestAdapter(t, stubClient{err: boom})
	_, err := a.Draft(runCtx(t, testQuad("t", "u", "s", "r")), "intent", "")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped %v", err, boom)
	}
}

func TestAdapter_Draft_PreCancelledCtx(t *testing.T) {
	a := newTestAdapter(t, stubClient{content: validDraftContent()})
	ctx, cancel := context.WithCancel(runCtx(t, testQuad("t", "u", "s", "r")))
	cancel()
	_, err := a.Draft(ctx, "intent", "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestAdapter_Draft_CancellationMidCall(t *testing.T) {
	ctx, cancel := context.WithCancel(runCtx(t, testQuad("t", "u", "s", "r")))
	a := newTestAdapter(t, stubClient{
		content: validDraftContent(),
		beforeComplete: func(c context.Context, _ llm.CompleteRequest) {
			cancel()
		},
	})
	if _, err := a.Draft(ctx, "intent", ""); err == nil {
		t.Fatal("expected a loud failure after mid-call cancellation, got nil")
	}
}

func TestAdapter_Draft_ExpiredDeadline(t *testing.T) {
	a := newTestAdapter(t, stubClient{content: validDraftContent()})
	ctx, cancel := context.WithDeadline(runCtx(t, testQuad("t", "u", "s", "r")), time.Now().Add(-time.Second))
	defer cancel()
	_, err := a.Draft(ctx, "intent", "")
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a context deadline/cancel error", err)
	}
}

func TestAdapter_Draft_BlockedClientHonoursCancellation(t *testing.T) {
	a, err := New(blockingClient{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(runCtx(t, testQuad("t", "u", "s", "r")))
	done := make(chan error, 1)
	go func() {
		_, err := a.Draft(ctx, "intent", "")
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Draft did not return after cancellation (goroutine leak)")
	}
}

func TestAdapter_Draft_EmptyModelResponse(t *testing.T) {
	a := newTestAdapter(t, stubClient{content: "   "})
	_, err := a.Draft(runCtx(t, testQuad("t", "u", "s", "r")), "intent", "")
	if !errors.Is(err, ErrMalformedModelOutput) {
		t.Fatalf("err = %v, want ErrMalformedModelOutput", err)
	}
}

// The adapter passes the run identity through to the client; the
// concurrency test asserts the full no-context-bleed property.
func TestAdapter_Draft_IdentityFlowsToClient(t *testing.T) {
	sink := make(chan identity.Quadruple, 1)
	a := newTestAdapter(t, stubClient{content: validDraftContent(), sink: sink})
	want := testQuad("t1", "u1", "s1", "r1")
	if _, err := a.Draft(runCtx(t, want), "intent", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-sink:
		if got != want {
			t.Fatalf("client saw identity %+v, want %+v", got, want)
		}
	default:
		t.Fatal("client never observed the run identity")
	}
}
