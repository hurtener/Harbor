package control_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/transports/control"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

type countingPreparer struct{ calls, admitted atomic.Int64 }

func (c *countingPreparer) PrepareSession(ctx context.Context, _ protocol.SessionPrepareRequest) (types.PrepareState, error) {
	c.calls.Add(1)
	if _, ok := tasks.AgentReachAdmissionSubjectFrom(ctx); ok {
		c.admitted.Add(1)
	}
	return types.PrepareStateScheduled, nil
}

func newPrepareHandler(t *testing.T, preparer protocol.SessionPreparer) http.Handler {
	t.Helper()
	// Documented dummy (all-zero) KEK for the reach-admission authority.
	sealer, err := toolauth.NewAESGCMSealer(make([]byte, toolauth.KEKSizeBytes))
	if err != nil {
		t.Fatal(err)
	}
	admissions, err := tasks.NewAgentReachAdmissionAuthority(sealer)
	if err != nil {
		t.Fatal(err)
	}
	extra := []protocol.Option{protocol.WithAgentReachAdmissionAuthority(admissions)}
	if preparer != nil {
		extra = append(extra, protocol.WithSessionPreparer(preparer))
	}
	surface, cleanup := newTestSurface(t, extra...)
	t.Cleanup(cleanup)
	h, err := control.NewHandler(surface)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// TestServeHTTP_Prepare_Accepted202 — `POST /v1/control/prepare` answers 202
// with the acknowledgement body and never spawns a task.
func TestServeHTTP_Prepare_Accepted202(t *testing.T) {
	preparer := &countingPreparer{}
	h := newPrepareHandler(t, preparer)
	rec := do(t, h, "/v1/control/prepare", `{"identity":{"tenant":"t1","user":"u1","session":"s1"}}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var resp types.PrepareResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.State != types.PrepareStateScheduled || resp.ProtocolVersion != types.ProtocolVersion || preparer.calls.Load() != 1 || preparer.admitted.Load() != 1 {
		t.Fatalf("resp=%+v calls=%d", resp, preparer.calls.Load())
	}
}

func TestServeHTTP_Prepare_Refusals(t *testing.T) {
	preparer := &countingPreparer{}
	h := newPrepareHandler(t, preparer)
	cases := []struct {
		name, body string
		want       int
	}{
		{"foreign body identity", `{"identity":{"tenant":"t1","user":"u2","session":"s1"}}`, http.StatusUnauthorized},
		{"impersonation", `{"identity":{"tenant":"t1","user":"u1","session":"s1","impersonating":{"tenant":"t1","user":"u1","session":"s1"}}}`, http.StatusBadRequest},
		{"unknown member", `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"query":"hi"}`, http.StatusBadRequest},
		{"unreachable agent", `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"someone-else"}`, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := do(t, h, "/v1/control/prepare", tc.body); rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	if preparer.calls.Load() != 0 {
		t.Fatalf("refused prepares reached the preparer %d times", preparer.calls.Load())
	}
	// A Runtime without a preparer reads the route as absent (smoke SKIP).
	if rec := do(t, newPrepareHandler(t, nil), "/v1/control/prepare", `{"identity":{"tenant":"t1","user":"u1","session":"s1"}}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unwired status = %d, want 404", rec.Code)
	}
}
