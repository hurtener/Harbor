package stream_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
)

func seedWritten(t *testing.T, coord pauseresume.Coordinator, id identity.Identity, runID, writer string) {
	t.Helper()
	ctx, err := identity.WithRun(context.Background(), id, runID)
	if err != nil {
		t.Fatal(err)
	}
	if writer != "" {
		if ctx, err = identity.WithParticipant(ctx, identity.Participant{UserID: writer}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := coord.Request(ctx, pauseresume.PauseRequest{Identity: id, Reason: pauseresume.ReasonApprovalRequired,
		Payload: map[string]any{"tool": "t", "args_summary": map[string]any{"args": map[string]any{"id": "x"}}}}); err != nil {
		t.Fatal(err)
	}
}

func listAs(t *testing.T, h http.Handler, id identity.Identity, participant *identity.Participant) prototypes.PauseListResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/pause/list", strings.NewReader(`{}`))
	ctx, err := identity.WithVerified(req.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if participant != nil {
		if ctx, err = identity.WithParticipant(ctx, *participant); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp prototypes.PauseListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// Snapshots carry their run and task; a session participant sees only
// pauses of runs it wrote in that session; the owner's view is unchanged.
func TestPauseList_RunAndParticipantScope(t *testing.T) {
	coord := pauseresume.New()
	h, err := stream.NewPauseListHandler(coord, newArtifactStore(t), heavyThresholdBytes)
	if err != nil {
		t.Fatal(err)
	}
	owner := identity.Identity{TenantID: "t-pp", UserID: "owner", SessionID: "shared"}
	other := identity.Identity{TenantID: "t-pp", UserID: "owner", SessionID: "private"}
	seedWritten(t, coord, owner, "run-guest", "guest")
	seedWritten(t, coord, owner, "run-owner", "")
	seedWritten(t, coord, other, "run-private", "")

	all := listAs(t, h, owner, nil)
	if len(all.Snapshots) != 3 {
		t.Fatalf("owner sees %d pauses, want 3", len(all.Snapshots))
	}
	for _, s := range all.Snapshots {
		if s.Identity.Run == "" || s.TaskID != s.Identity.Run {
			t.Fatalf("snapshot run/task = %q/%q", s.Identity.Run, s.TaskID)
		}
		if _, ok := s.Payload["args_summary"]; !ok || s.Payload["tool"] != "t" {
			t.Fatalf("snapshot payload = %+v", s.Payload)
		}
	}
	guest := listAs(t, h, owner, &identity.Participant{UserID: "guest"})
	if len(guest.Snapshots) != 1 || guest.Snapshots[0].Identity.Run != "run-guest" {
		t.Fatalf("guest sees %+v, want only run-guest", guest.Snapshots)
	}
}
