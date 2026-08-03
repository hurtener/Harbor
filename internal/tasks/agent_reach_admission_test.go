package tasks

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
)

func TestAgentReachAdmission_SealedCaptureRestoreAndTamperDenial(t *testing.T) {
	id := identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}
	sealed := WithAgentReachAdmission(context.Background(), id, "agent-a")
	receipt := CaptureAgentReachAdmission(sealed, id, "agent-a")
	if receipt == nil {
		t.Fatal("exact sealed admission was not captured")
	}
	task := &Task{Identity: identity.Quadruple{Identity: id}, AgentID: "agent-a", AgentReachAdmission: receipt}
	raw, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	var restarted Task
	if err := json.Unmarshal(raw, &restarted); err != nil {
		t.Fatal(err)
	}
	if _, got, admitted := RestoreAgentReachAdmission(context.Background(), &restarted); !admitted || got != "agent-a" {
		t.Fatalf("restart restore = (%q, %v), want (agent-a, true)", got, admitted)
	}

	for _, mutate := range []func(*Task){
		func(v *Task) { v.AgentID = "agent-b" },
		func(v *Task) { v.Identity.UserID = "other-user" },
		func(v *Task) { v.AgentReachAdmission.Schema = "unknown" },
		func(v *Task) { v.AgentReachAdmission.TenantID = "other-tenant" },
		func(v *Task) { v.AgentReachAdmission.EffectiveAgentID = "agent-b" },
	} {
		candidate := restarted
		receiptCopy := *restarted.AgentReachAdmission
		candidate.AgentReachAdmission = &receiptCopy
		mutate(&candidate)
		if _, _, admitted := RestoreAgentReachAdmission(context.Background(), &candidate); admitted {
			t.Fatalf("tampered admission restored: task=%+v receipt=%+v", candidate, candidate.AgentReachAdmission)
		}
	}
	other := id
	other.SessionID = "other-session"
	if got := CaptureAgentReachAdmission(sealed, other, "agent-a"); got != nil {
		t.Fatalf("cross-identity capture returned %+v", got)
	}
	if got := CaptureAgentReachAdmission(sealed, id, "agent-b"); got != nil {
		t.Fatalf("wrong-agent capture returned %+v", got)
	}
}

func TestAgentReachAdmission_ConcurrentCaptureNoBleed(t *testing.T) {
	const n = 128
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := identity.Identity{TenantID: "tenant", UserID: "user-" + strconv.Itoa(i), SessionID: "session-" + strconv.Itoa(i)}
			agent := "agent-" + strconv.Itoa(i)
			ctx := WithAgentReachAdmission(context.Background(), id, agent)
			receipt := CaptureAgentReachAdmission(ctx, id, agent)
			if receipt == nil || receipt.UserID != id.UserID || receipt.SessionID != id.SessionID || receipt.EffectiveAgentID != agent {
				errs <- agent
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for agent := range errs {
		t.Fatalf("concurrent admission bled at %s", agent)
	}
}
