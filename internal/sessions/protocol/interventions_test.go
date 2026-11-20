package protocol

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/sessions"
)

type recordingInterventions struct {
	seen []identity.Identity
	err  error
}

func (r *recordingInterventions) RecentInterventions(_ context.Context, id identity.Identity) ([]prototypes.InterventionSummary, error) {
	r.seen = append(r.seen, id)
	if r.err != nil {
		return nil, r.err
	}
	return []prototypes.InterventionSummary{{
		Type: "tool_approval", Outcome: "approved", Tool: "delete_doc",
		DecidedBy: &prototypes.InterventionDecider{TenantID: id.TenantID, UserID: "approver"},
		DecidedAt: time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC),
	}}, nil
}

// sessions.inspect fills recent interventions from the wired reader,
// scoped to the INSPECTED session's own identity; the lifecycle-only
// projection never reads it; a reader failure fails the inspect.
func TestInspect_RecentInterventionsFromReader(t *testing.T) {
	snap := sessions.SessionSnapshot{Session: sessions.Session{
		ID:       "sess-x",
		Identity: identity.Identity{TenantID: "tenant-1", UserID: "user-1", SessionID: "sess-x"},
		OpenedAt: time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC),
	}}
	reader := &recordingInterventions{}
	projector, err := NewListerProjector(catalogLister{snapshots: []sessions.SessionSnapshot{snap}}, WithInterventions(reader))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := projector.InspectSession(context.Background(), lifecycleCaller(), "sess-x", false)
	if err != nil {
		t.Fatalf("InspectSession: %v", err)
	}
	if len(resp.RecentInterventions) != 1 || resp.RecentInterventions[0].DecidedBy.UserID != "approver" {
		t.Fatalf("RecentInterventions = %+v", resp.RecentInterventions)
	}
	if len(reader.seen) != 1 || reader.seen[0] != snap.Identity {
		t.Fatalf("reader scoped to %+v, want the inspected session %+v", reader.seen, snap.Identity)
	}

	lc, err := projector.inspectLifecycleOnly(context.Background(), lifecycleCaller(), "sess-x", false)
	if err != nil || len(lc.RecentInterventions) != 0 || len(reader.seen) != 1 {
		t.Fatalf("lifecycle projection read interventions: %+v, %v", lc.RecentInterventions, err)
	}

	// A bearer whose signed session_reach excludes the inspected session
	// reads no decisions from it.
	narrowed := auth.WithSessionReach(context.Background(), []string{"another-session"})
	before := len(reader.seen)
	resp, err = projector.InspectSession(narrowed, lifecycleCaller(), "sess-x", false)
	if err != nil || len(resp.RecentInterventions) != 0 || len(reader.seen) != before {
		t.Fatalf("out-of-reach inspect read decisions: %+v, %v", resp.RecentInterventions, err)
	}

	reader.err = errors.New("store down")
	if _, err := projector.InspectSession(context.Background(), lifecycleCaller(), "sess-x", false); err == nil {
		t.Fatal("a reader failure must fail the inspect, not return an empty card")
	}
}
