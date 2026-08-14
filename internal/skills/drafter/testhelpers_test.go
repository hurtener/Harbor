package drafter

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
)

// testhelpers_test.go — shared test fixtures for the drafter lane.
//
// The stub LLM client is a test-grade fake standing in for the
// assembly's composed client: it returns a scripted response, can
// force an error, can sink the per-call identity for no-context-bleed
// assertions, and can block until ctx cancellation to exercise the
// cancellation path. The spy writer records every Write so the
// non-mutation guarantees are asserted with real call counts, not by
// inspection.

// validDraftContent returns a canonical, renderable model response.
func validDraftContent() string {
	b, _ := json.Marshal(map[string]any{
		"name":        "triage-inbox",
		"title":       "Triage the shared inbox",
		"description": "Scan the shared inbox and triage every unread message into a queue.",
		"trigger":     "when the user asks to triage the inbox",
		"task_type":   "api",
		"tags":        []string{"triage", "inbox"},
		"steps": []string{
			"Fetch unread messages",
			"Classify each message by priority",
			"Assign each message to the right queue",
		},
		"required_tools": []string{"inbox.read"},
	})
	return string(b)
}

// stubClient is a scripted llm.LLMClient.
type stubClient struct {
	// content is the scripted response body.
	content string
	// contentFor, when set, overrides content per request (e.g. to
	// model feedback-dependence or identity-dependence).
	contentFor func(ctx context.Context, req llm.CompleteRequest) string
	// err forces every Complete to fail with this error.
	err error
	// sink, when set, receives the identity each Complete observes
	// (buffered by the caller).
	sink chan identity.Quadruple
	// beforeComplete runs inside Complete before the response is
	// returned (used to block or cancel).
	beforeComplete func(ctx context.Context, req llm.CompleteRequest)
}

func (c stubClient) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	if c.err != nil {
		return llm.CompleteResponse{}, c.err
	}
	if err := ctx.Err(); err != nil {
		return llm.CompleteResponse{}, err
	}
	if q, ok := identity.QuadrupleFrom(ctx); ok && c.sink != nil {
		select {
		case c.sink <- q:
		default:
		}
	}
	if c.beforeComplete != nil {
		c.beforeComplete(ctx, req)
	}
	// Re-check after the hook: a mid-call cancellation (or a hook that
	// cancels the parent ctx) must surface loudly, mirroring the
	// composed client's behaviour.
	if err := ctx.Err(); err != nil {
		return llm.CompleteResponse{}, err
	}
	content := c.content
	if c.contentFor != nil {
		content = c.contentFor(ctx, req)
	}
	return llm.CompleteResponse{Content: content}, nil
}

func (stubClient) Close(context.Context) error { return nil }

var _ llm.LLMClient = stubClient{}

// blockingClient blocks inside Complete until ctx is cancelled, then
// returns ctx.Err() — the shape a real provider surfaces on
// cancellation/timeout.
type blockingClient struct{}

func (blockingClient) Complete(ctx context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	<-ctx.Done()
	return llm.CompleteResponse{}, ctx.Err()
}

func (blockingClient) Close(context.Context) error { return nil }

var _ llm.LLMClient = blockingClient{}

// testQuad builds a complete identity quadruple.
func testQuad(tenant, user, session, run string) identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: tenant, UserID: user, SessionID: session},
		RunID:    run,
	}
}

// runCtx attaches the quadruple to a fresh context.
func runCtx(t testing.TB, q identity.Quadruple) context.Context {
	t.Helper()
	ctx, err := identity.WithRun(context.Background(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

// spyWriter records every Write (and its data) and fails them with
// `err` when set. It can optionally persist through a real store so
// isolation/replay tests can read the artifact back.
type spyWriter struct {
	mu    sync.Mutex
	store artifacts.ArtifactStore
	scope artifacts.ArtifactScope
	err   error

	data  [][]byte
	opts  []artifacts.PutOpts
	refs  []artifacts.ArtifactRef
	calls int
}

func (w *spyWriter) Write(ctx context.Context, data []byte, opts artifacts.PutOpts) (artifacts.ArtifactRef, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	w.data = append(w.data, append([]byte(nil), data...))
	w.opts = append(w.opts, opts)
	if w.err != nil {
		return artifacts.ArtifactRef{}, w.err
	}
	if w.store == nil {
		// Pure recording spy: hand back a deterministic ref.
		ref := artifacts.ArtifactRef{ID: "skill-draft_fixture", SizeBytes: int64(len(data))}
		w.refs = append(w.refs, ref)
		return ref, nil
	}
	ref, err := w.store.PutBytes(ctx, w.scope, data, opts)
	if err == nil {
		w.refs = append(w.refs, ref)
	}
	return ref, err
}

// writeCount returns how many Write calls happened.
func (w *spyWriter) writeCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

// writtenData returns the bytes of the i-th Write.
func (w *spyWriter) writtenData(i int) []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	if i < 0 || i >= len(w.data) {
		return nil
	}
	return append([]byte(nil), w.data[i]...)
}

// writtenOpts returns the opts of the i-th Write.
func (w *spyWriter) writtenOpts(i int) artifacts.PutOpts {
	w.mu.Lock()
	defer w.mu.Unlock()
	if i < 0 || i >= len(w.opts) {
		return artifacts.PutOpts{}
	}
	return w.opts[i]
}

// writtenRef returns the ref of the i-th successful Write.
func (w *spyWriter) writtenRef(i int) artifacts.ArtifactRef {
	w.mu.Lock()
	defer w.mu.Unlock()
	if i < 0 || i >= len(w.refs) {
		return artifacts.ArtifactRef{}
	}
	return w.refs[i]
}
