package protocol

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
)

// proofBase is the documented dummy tuple the proof tests bind — no
// secrets, matching the verified identity the surface dispatches under.
func proofBase() renderAdmissionProof {
	return renderAdmissionProof{
		tenantID:    "t-1",
		userID:      "u-1",
		sessionID:   "s-1",
		agentID:     "agent-a",
		serverID:    "srv-a",
		resourceURI: "ui://srv-a/app.html",
	}
}

// TestCheckRenderAdmissionProof_ExactTupleOnly pins the checker's exact
// contract: a proof minted by this package (the private setter, the ONLY
// mint path) verifies for the exact tuple it binds and for NO other —
// every single dimension (tenant / user / session / agent / server /
// resource URI) must mismatch on its own, and a ctx with no proof must
// refuse. This is the exact re-check the admission-aware accessor
// performs before any resolution or invocation.
func TestCheckRenderAdmissionProof_ExactTupleOnly(t *testing.T) {
	exact := identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"}

	if CheckRenderAdmissionProof(context.Background(), exact, "agent-a", "srv-a", "ui://srv-a/app.html") {
		t.Fatal("a ctx with no proof must refuse")
	}

	okCtx := withRenderAdmissionProof(context.Background(), proofBase())
	if !CheckRenderAdmissionProof(okCtx, exact, "agent-a", "srv-a", "ui://srv-a/app.html") {
		t.Fatal("the exact tuple must verify against the minted proof")
	}

	mismatches := []struct {
		name string
		id   identity.Identity
		args [4]string // agentID, serverID, resourceURI
	}{
		{"foreign tenant", identity.Identity{TenantID: "t-2", UserID: "u-1", SessionID: "s-1"}, [4]string{"agent-a", "srv-a", "ui://srv-a/app.html"}},
		{"foreign user", identity.Identity{TenantID: "t-1", UserID: "u-2", SessionID: "s-1"}, [4]string{"agent-a", "srv-a", "ui://srv-a/app.html"}},
		{"foreign session", identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-2"}, [4]string{"agent-a", "srv-a", "ui://srv-a/app.html"}},
		{"empty identity", identity.Identity{}, [4]string{"agent-a", "srv-a", "ui://srv-a/app.html"}},
		{"foreign agent", exact, [4]string{"agent-b", "srv-a", "ui://srv-a/app.html"}},
		{"empty agent", exact, [4]string{"", "srv-a", "ui://srv-a/app.html"}},
		{"foreign server", exact, [4]string{"agent-a", "srv-b", "ui://srv-a/app.html"}},
		{"empty server", exact, [4]string{"agent-a", "", "ui://srv-a/app.html"}},
		{"foreign resource", exact, [4]string{"agent-a", "srv-a", "ui://srv-b/app.html"}},
		{"empty resource", exact, [4]string{"agent-a", "srv-a", ""}},
	}
	for _, tc := range mismatches {
		t.Run(tc.name, func(t *testing.T) {
			if CheckRenderAdmissionProof(okCtx, tc.id, tc.args[0], tc.args[1], tc.args[2]) {
				t.Errorf("%s: a mismatched tuple must not verify (proof binds %+v)", tc.name, proofBase())
			}
		})
	}
}

// TestCheckRenderAdmissionProof_ExactMatchDoesNotCheckGeneration pins
// the proof's scope honestly: it binds the six tuple components and
// nothing else — no sealed token, no generation, no nonce. The sealed
// admission and the CURRENT generation verification happen upstream
// (verifyRenderAdmission, before the proof is minted); the proof is the
// call-local "this admission was opened and is current" authority, not a
// second generation check.
func TestCheckRenderAdmissionProof_ExactMatchDoesNotCheckGeneration(t *testing.T) {
	exact := identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"}
	ctx := withRenderAdmissionProof(context.Background(), proofBase())
	if !CheckRenderAdmissionProof(ctx, exact, "agent-a", "srv-a", "ui://srv-a/app.html") {
		t.Fatal("exact tuple must verify")
	}
	// A ctx WITHOUT the proof refuses even when every other context value
	// (identity, agent) is present and fully verified — the checker reads
	// ONLY the proof, never falls back to ctx identity or agent state.
	rich, err := identity.With(context.Background(), exact)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if CheckRenderAdmissionProof(rich, exact, "agent-a", "srv-a", "ui://srv-a/app.html") {
		t.Fatal("a fully verified identity/agent context WITHOUT the proof must refuse (method selection is not an authority)")
	}
}
