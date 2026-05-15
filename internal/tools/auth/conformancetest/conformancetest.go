// Package conformancetest exposes the shared TokenStore + Sealer
// conformance suite Phase 30 ships. The same suite runs against every
// V1 state.StateStore driver (in-mem / SQLite / Postgres) — driver
// pluralism for the TokenStore is inherited from the
// state.StateStore §4.4 seam (D-027 + D-067 pattern; Phase 30 follows
// the same approach Phase 53a took, see phase-30-tool-oauth.md
// §"Findings I'm departing from").
//
// Test surface:
//
//   - Put → Get round-trip (user-bound)
//   - Put → Get round-trip (agent-bound)
//   - cross-tenant isolation
//   - cross-user isolation
//   - cross-agent isolation
//   - mixed-scope coexistence (same source, both scopes coexist)
//   - encryption at rest — raw stored bytes never contain plaintext
//   - Delete idempotency
//   - missing-identity fail-loud
//
// The driver under test is constructed by the caller via a Factory
// callback so SQLite / Postgres legs can pass their own DSN / KEK
// setup. See internal/tools/auth/conformance_test.go and
// test/integration/phase30_tool_oauth_test.go for the call sites.
package conformancetest

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// Factory builds a fresh (TokenStore, raw StateStore) pair the suite
// drives. Implementations MUST open a clean store (no carry-over from
// previous test runs); t.Cleanup closes any resources the factory
// allocates.
type Factory func(t *testing.T) (store auth.TokenStore, raw state.StateStore, sealer auth.Sealer)

// Run drives every conformance subtest against factory. Each subtest
// gets a fresh factory invocation so they cannot pollute one another.
func Run(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("PutGet_RoundTrip_UserBound", func(t *testing.T) {
		st, _, _ := factory(t)
		runPutGetUserBound(t, st)
	})
	t.Run("PutGet_RoundTrip_AgentBound", func(t *testing.T) {
		st, _, _ := factory(t)
		runPutGetAgentBound(t, st)
	})
	t.Run("CrossTenantIsolation", func(t *testing.T) {
		st, _, _ := factory(t)
		runCrossTenantIsolation(t, st)
	})
	t.Run("CrossUserIsolation", func(t *testing.T) {
		st, _, _ := factory(t)
		runCrossUserIsolation(t, st)
	})
	t.Run("CrossAgentIsolation", func(t *testing.T) {
		st, _, _ := factory(t)
		runCrossAgentIsolation(t, st)
	})
	t.Run("MixedScopeCoexistence", func(t *testing.T) {
		st, _, _ := factory(t)
		runMixedScope(t, st)
	})
	t.Run("EncryptionAtRest_CiphertextNotPlaintext", func(t *testing.T) {
		st, raw, _ := factory(t)
		runEncryptionAtRest(t, st, raw)
	})
	t.Run("Delete_Idempotent", func(t *testing.T) {
		st, _, _ := factory(t)
		runDeleteIdempotent(t, st)
	})
	t.Run("MissingIdentity_FailsLoud", func(t *testing.T) {
		st, _, _ := factory(t)
		runMissingIdentity(t, st)
	})
	t.Run("Sealer_TamperedCiphertext_FailsLoud", func(t *testing.T) {
		_, _, sealer := factory(t)
		runSealerTamperRejection(t, sealer)
	})
}

func ctxFor(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

func runPutGetUserBound(t *testing.T, st auth.TokenStore) {
	id := identity.Identity{TenantID: "tA", UserID: "uA", SessionID: "sA"}
	ctx := ctxFor(t, id)
	tok := auth.Token{
		Source:       tools.ToolSourceID("src-X"),
		BindingScope: auth.ScopeUser,
		TenantID:     id.TenantID,
		UserID:       id.UserID,
		AccessToken:  "dummy-access-userA",
		RefreshToken: "dummy-refresh-userA",
		TokenType:    "Bearer",
		Scopes:       []string{"repo"},
	}
	if err := st.Put(ctx, tok); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok, err := st.Get(ctx, auth.ScopeUser, id.UserID, "src-X")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok || got.AccessToken != tok.AccessToken {
		t.Fatalf("readback mismatch: got %+v", got)
	}
}

func runPutGetAgentBound(t *testing.T, st auth.TokenStore) {
	id := identity.Identity{TenantID: "tA", UserID: "uA", SessionID: "sA"}
	ctx := ctxFor(t, id)
	tok := auth.Token{
		Source:       tools.ToolSourceID("src-Y"),
		BindingScope: auth.ScopeAgent,
		TenantID:     id.TenantID,
		AgentID:      "agent-1",
		AccessToken:  "dummy-access-agent1",
	}
	if err := st.Put(ctx, tok); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok, err := st.Get(ctx, auth.ScopeAgent, "agent-1", "src-Y")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok || got.AccessToken != tok.AccessToken {
		t.Fatalf("readback mismatch: got %+v", got)
	}
	if got.UserID != "" {
		t.Fatalf("agent-bound token's UserID must be empty; got %q", got.UserID)
	}
	if got.AgentID != "agent-1" {
		t.Fatalf("AgentID: got %q", got.AgentID)
	}
}

func runCrossTenantIsolation(t *testing.T, st auth.TokenStore) {
	idA := identity.Identity{TenantID: "tA", UserID: "u", SessionID: "s"}
	idB := identity.Identity{TenantID: "tB", UserID: "u", SessionID: "s"}
	ctxA := ctxFor(t, idA)
	ctxB := ctxFor(t, idB)
	if err := st.Put(ctxA, auth.Token{
		Source: "src-X", BindingScope: auth.ScopeUser,
		TenantID: idA.TenantID, UserID: idA.UserID,
		AccessToken: "secretA",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctxB, auth.Token{
		Source: "src-X", BindingScope: auth.ScopeUser,
		TenantID: idB.TenantID, UserID: idB.UserID,
		AccessToken: "secretB",
	}); err != nil {
		t.Fatal(err)
	}
	// A's ctx + B's user is unreachable — different tenant.
	_, ok, err := st.Get(ctxA, auth.ScopeUser, idA.UserID, "src-X")
	if err != nil || !ok {
		t.Fatalf("A self-read: ok=%v err=%v", ok, err)
	}
	gotA, _, _ := st.Get(ctxA, auth.ScopeUser, idA.UserID, "src-X")
	if gotA.AccessToken != "secretA" {
		t.Fatalf("A read leaked: %q", gotA.AccessToken)
	}
	gotB, _, _ := st.Get(ctxB, auth.ScopeUser, idB.UserID, "src-X")
	if gotB.AccessToken != "secretB" {
		t.Fatalf("B read leaked: %q", gotB.AccessToken)
	}
}

func runCrossUserIsolation(t *testing.T, st auth.TokenStore) {
	id := identity.Identity{TenantID: "tA", UserID: "alice", SessionID: "s"}
	ctxAlice := ctxFor(t, id)
	idB := id
	idB.UserID = "bob"
	ctxBob := ctxFor(t, idB)
	if err := st.Put(ctxAlice, auth.Token{
		Source: "src", BindingScope: auth.ScopeUser,
		TenantID: id.TenantID, UserID: "alice",
		AccessToken: "alice-secret",
	}); err != nil {
		t.Fatal(err)
	}
	// Bob cannot read Alice's token even with knowledge of her UserID.
	_, ok, _ := st.Get(ctxBob, auth.ScopeUser, "alice", "src")
	if ok {
		t.Fatalf("bob reading via alice's user id leaked")
	}
}

func runCrossAgentIsolation(t *testing.T, st auth.TokenStore) {
	id := identity.Identity{TenantID: "tA", UserID: "u", SessionID: "s"}
	ctx := ctxFor(t, id)
	if err := st.Put(ctx, auth.Token{
		Source: "src", BindingScope: auth.ScopeAgent,
		TenantID: id.TenantID, AgentID: "alpha", AccessToken: "alpha-secret",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, auth.Token{
		Source: "src", BindingScope: auth.ScopeAgent,
		TenantID: id.TenantID, AgentID: "beta", AccessToken: "beta-secret",
	}); err != nil {
		t.Fatal(err)
	}
	gotAlpha, _, _ := st.Get(ctx, auth.ScopeAgent, "alpha", "src")
	if gotAlpha.AccessToken != "alpha-secret" {
		t.Fatalf("alpha leaked")
	}
	gotBeta, _, _ := st.Get(ctx, auth.ScopeAgent, "beta", "src")
	if gotBeta.AccessToken != "beta-secret" {
		t.Fatalf("beta leaked")
	}
}

func runMixedScope(t *testing.T, st auth.TokenStore) {
	id := identity.Identity{TenantID: "tA", UserID: "u", SessionID: "s"}
	ctx := ctxFor(t, id)
	if err := st.Put(ctx, auth.Token{
		Source: "src", BindingScope: auth.ScopeUser,
		TenantID: id.TenantID, UserID: id.UserID, AccessToken: "user-tok",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, auth.Token{
		Source: "src", BindingScope: auth.ScopeAgent,
		TenantID: id.TenantID, AgentID: "ag", AccessToken: "agent-tok",
	}); err != nil {
		t.Fatal(err)
	}
	gotUser, _, _ := st.Get(ctx, auth.ScopeUser, id.UserID, "src")
	gotAgent, _, _ := st.Get(ctx, auth.ScopeAgent, "ag", "src")
	if gotUser.AccessToken != "user-tok" || gotAgent.AccessToken != "agent-tok" {
		t.Fatalf("mixed-scope readback drift: user=%q agent=%q",
			gotUser.AccessToken, gotAgent.AccessToken)
	}
}

func runEncryptionAtRest(t *testing.T, st auth.TokenStore, raw state.StateStore) {
	id := identity.Identity{TenantID: "tA", UserID: "u", SessionID: "s"}
	ctx := ctxFor(t, id)
	const marker = "ENCRYPTION-AT-REST-MARKER-XYZ"
	if err := st.Put(ctx, auth.Token{
		Source: "src", BindingScope: auth.ScopeUser,
		TenantID: id.TenantID, UserID: id.UserID,
		AccessToken: marker,
	}); err != nil {
		t.Fatal(err)
	}
	q := identity.Quadruple{Identity: id}
	// The wrapper hides the Kind; we know the layout (and the
	// in-package kindBelongsToToken would have already been
	// asserted) — we ask the store directly via the access-token
	// Kind for the (scope, subject, source) shape this Put used.
	// We reach in through the auth package's exported helpers via
	// the suite's caller; here we Load every record and search.
	rec, err := raw.Load(ctx, q, "tools.auth.access.user."+id.UserID+".src")
	if err != nil {
		t.Fatalf("raw Load: %v", err)
	}
	if bytes.Contains(rec.Bytes, []byte(marker)) {
		t.Fatalf("ENCRYPTION-AT-REST LEAK: raw bytes contain plaintext marker")
	}
}

func runDeleteIdempotent(t *testing.T, st auth.TokenStore) {
	id := identity.Identity{TenantID: "tA", UserID: "u", SessionID: "s"}
	ctx := ctxFor(t, id)
	// Delete-missing → no error.
	if err := st.Delete(ctx, auth.ScopeUser, "u", "src"); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
	// Put then delete → gone.
	_ = st.Put(ctx, auth.Token{
		Source: "src", BindingScope: auth.ScopeUser,
		TenantID: id.TenantID, UserID: id.UserID, AccessToken: "x",
	})
	if err := st.Delete(ctx, auth.ScopeUser, id.UserID, "src"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, ok, _ := st.Get(ctx, auth.ScopeUser, id.UserID, "src")
	if ok {
		t.Fatalf("Get after Delete: still present")
	}
}

func runMissingIdentity(t *testing.T, st auth.TokenStore) {
	noID := context.Background()
	_, _, err := st.Get(noID, auth.ScopeUser, "u", "src")
	if !errors.Is(err, auth.ErrIdentityRequired) {
		t.Fatalf("Get noID: want ErrIdentityRequired, got %v", err)
	}
}

func runSealerTamperRejection(t *testing.T, sealer auth.Sealer) {
	if sealer == nil {
		t.Skip("factory did not return a Sealer")
	}
	ct, err := sealer.Seal([]byte("payload"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Flip the last byte (tag).
	bad := append([]byte(nil), ct...)
	bad[len(bad)-1] ^= 0xFF
	_, err = sealer.Open(bad)
	if !auth.IsCipherCorrupt(err) {
		t.Fatalf("tampered Open: want ErrTokenCipherCorrupt, got %v", err)
	}
}
