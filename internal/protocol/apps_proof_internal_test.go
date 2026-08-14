package protocol

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
)

// proofBase is the documented dummy tuple the proof tests bind — no
// secrets, matching the verified identity the surface dispatches under.
// The generation field mirrors the exact current provider/catalog
// generation the surface verified the sealed admission against.
func proofBase() renderAdmissionProof {
	return renderAdmissionProof{
		tenantID:    "t-1",
		userID:      "u-1",
		sessionID:   "s-1",
		agentID:     "agent-a",
		serverID:    "srv-a",
		resourceURI: "ui://srv-a/app.html",
		generation:  "gen-1",
	}
}

// TestCheckRenderAdmissionProof_ExactTupleOnly pins the checker's exact
// contract: a proof minted by this package (the private setter, the ONLY
// mint path) verifies for the exact tuple it binds and for NO other —
// every single dimension (tenant / user / session / agent / server /
// resource URI / generation) must mismatch on its own, and a ctx with no
// proof must refuse. This is the exact re-check the admission-aware
// accessor performs before any resolution or invocation.
func TestCheckRenderAdmissionProof_ExactTupleOnly(t *testing.T) {
	exact := identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"}

	if CheckRenderAdmissionProof(context.Background(), exact, "agent-a", "srv-a", "ui://srv-a/app.html", "gen-1") {
		t.Fatal("a ctx with no proof must refuse")
	}

	okCtx := withRenderAdmissionProof(context.Background(), proofBase())
	if !CheckRenderAdmissionProof(okCtx, exact, "agent-a", "srv-a", "ui://srv-a/app.html", "gen-1") {
		t.Fatal("the exact tuple must verify against the minted proof")
	}

	mismatches := []struct {
		name string
		id   identity.Identity
		args [5]string // agentID, serverID, resourceURI, generation
	}{
		{"foreign tenant", identity.Identity{TenantID: "t-2", UserID: "u-1", SessionID: "s-1"}, [5]string{"agent-a", "srv-a", "ui://srv-a/app.html", "gen-1"}},
		{"foreign user", identity.Identity{TenantID: "t-1", UserID: "u-2", SessionID: "s-1"}, [5]string{"agent-a", "srv-a", "ui://srv-a/app.html", "gen-1"}},
		{"foreign session", identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-2"}, [5]string{"agent-a", "srv-a", "ui://srv-a/app.html", "gen-1"}},
		{"empty identity", identity.Identity{}, [5]string{"agent-a", "srv-a", "ui://srv-a/app.html", "gen-1"}},
		{"foreign agent", exact, [5]string{"agent-b", "srv-a", "ui://srv-a/app.html", "gen-1"}},
		{"empty agent", exact, [5]string{"", "srv-a", "ui://srv-a/app.html", "gen-1"}},
		{"foreign server", exact, [5]string{"agent-a", "srv-b", "ui://srv-a/app.html", "gen-1"}},
		{"empty server", exact, [5]string{"agent-a", "", "ui://srv-a/app.html", "gen-1"}},
		{"foreign resource", exact, [5]string{"agent-a", "srv-a", "ui://srv-b/app.html", "gen-1"}},
		{"empty resource", exact, [5]string{"agent-a", "srv-a", "", "gen-1"}},
		{"foreign generation", exact, [5]string{"agent-a", "srv-a", "ui://srv-a/app.html", "gen-2"}},
		{"empty generation", exact, [5]string{"agent-a", "srv-a", "ui://srv-a/app.html", ""}},
	}
	for _, tc := range mismatches {
		t.Run(tc.name, func(t *testing.T) {
			if CheckRenderAdmissionProof(okCtx, tc.id, tc.args[0], tc.args[1], tc.args[2], tc.args[3]) {
				t.Errorf("%s: a mismatched tuple must not verify (proof binds %+v)", tc.name, proofBase())
			}
		})
	}
}

// TestCheckRenderAdmissionProof_ExactMatchIncludesGeneration pins the
// HA-56 TOCTOU correction: the proof binds the exact CURRENT
// provider/catalog generation the surface verified, so a checker that
// does not name the exact same generation refuses — a proof minted
// against generation G1 can never authorize a call under G2 (the
// refresh/replacement race), and an empty generation never verifies. The
// checker reads ONLY the proof — it never falls back to ctx identity or
// agent state, however fully verified that context is.
func TestCheckRenderAdmissionProof_ExactMatchIncludesGeneration(t *testing.T) {
	exact := identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"}
	ctx := withRenderAdmissionProof(context.Background(), proofBase())
	if !CheckRenderAdmissionProof(ctx, exact, "agent-a", "srv-a", "ui://srv-a/app.html", "gen-1") {
		t.Fatal("exact tuple + exact generation must verify")
	}
	// The generation is part of the exact tuple: a refresh/replacement
	// that moved the current generation from gen-1 to gen-2 makes the
	// stale proof refuse — the accessor passes the CURRENT generation it
	// just read, so the mismatch is caught here, before any resolution.
	if CheckRenderAdmissionProof(ctx, exact, "agent-a", "srv-a", "ui://srv-a/app.html", "gen-2") {
		t.Fatal("a proof minted against gen-1 must not verify under gen-2 (refresh/replacement race)")
	}
	if CheckRenderAdmissionProof(ctx, exact, "agent-a", "srv-a", "ui://srv-a/app.html", "") {
		t.Fatal("an empty generation must never verify")
	}
	// A ctx WITHOUT the proof refuses even when every other context value
	// (identity, agent) is present and fully verified — the checker reads
	// ONLY the proof, never falls back to ctx identity or agent state.
	rich, err := identity.With(context.Background(), exact)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if CheckRenderAdmissionProof(rich, exact, "agent-a", "srv-a", "ui://srv-a/app.html", "gen-1") {
		t.Fatal("a fully verified identity/agent context WITHOUT the proof must refuse (method selection is not an authority)")
	}
}
