package auth

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
)

// dummy IDs — never real credentials per §7 rule 2.
const (
	tDummyTenant  = "tenant-A"
	tDummyUser    = "user-alice"
	tDummySession = "session-001"
	tDummyAgent   = "agent-research-assistant"
	tDummySource  = tools.ToolSourceID("github-mcp")
	tDummyAccess  = "dummy-access-token-not-a-secret"
	tDummyRefresh = "dummy-refresh-token-not-a-secret"
)

func mkIdentity(t *testing.T) identity.Identity {
	t.Helper()
	return identity.Identity{
		TenantID:  tDummyTenant,
		UserID:    tDummyUser,
		SessionID: tDummySession,
	}
}

func mkCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

func mkStore(t *testing.T) state.StateStore {
	t.Helper()
	s, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func mkTokenStore(t *testing.T) (TokenStore, state.StateStore) {
	t.Helper()
	store := mkStore(t)
	sealer, err := NewAESGCMSealer(fixedKEK(t))
	if err != nil {
		t.Fatalf("NewAESGCMSealer: %v", err)
	}
	ts, err := NewTokenStore(store, sealer)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}
	return ts, store
}

func TestTokenStore_PutGet_RoundTrip_UserBound(t *testing.T) {
	t.Parallel()
	ts, _ := mkTokenStore(t)
	id := mkIdentity(t)
	ctx := mkCtx(t, id)

	tok := Token{
		Source:       tDummySource,
		BindingScope: ScopeUser,
		TenantID:     id.TenantID,
		UserID:       id.UserID,
		AccessToken:  tDummyAccess,
		RefreshToken: tDummyRefresh,
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(time.Hour),
		Scopes:       []string{"repo", "read:user"},
	}
	if err := ts.Put(ctx, tok); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ok, err := ts.Get(ctx, ScopeUser, id.UserID, tDummySource)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatalf("Get: ok=false; want true")
	}
	if got.AccessToken != tDummyAccess {
		t.Fatalf("AccessToken: got %q want %q", got.AccessToken, tDummyAccess)
	}
	if got.RefreshToken != tDummyRefresh {
		t.Fatalf("RefreshToken: got %q want %q", got.RefreshToken, tDummyRefresh)
	}
	if got.BindingScope != ScopeUser {
		t.Fatalf("BindingScope: got %s want %s", got.BindingScope, ScopeUser)
	}
}

func TestTokenStore_PutGet_RoundTrip_AgentBound(t *testing.T) {
	t.Parallel()
	ts, _ := mkTokenStore(t)
	id := mkIdentity(t)
	ctx := mkCtx(t, id)

	tok := Token{
		Source:       tools.ToolSourceID("outlook-shared"),
		BindingScope: ScopeAgent,
		TenantID:     id.TenantID,
		AgentID:      tDummyAgent,
		AccessToken:  tDummyAccess + "-agent",
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	if err := ts.Put(ctx, tok); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ok, err := ts.Get(ctx, ScopeAgent, tDummyAgent, "outlook-shared")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatalf("Get: ok=false; want true")
	}
	if got.AccessToken != tDummyAccess+"-agent" {
		t.Fatalf("agent-bound access mismatch")
	}
	if got.AgentID != tDummyAgent {
		t.Fatalf("AgentID: got %q want %q", got.AgentID, tDummyAgent)
	}
}

func TestTokenStore_Get_Miss_ReturnsFalseNilError(t *testing.T) {
	t.Parallel()
	ts, _ := mkTokenStore(t)
	ctx := mkCtx(t, mkIdentity(t))
	tok, ok, err := ts.Get(ctx, ScopeUser, tDummyUser, "nonexistent-source")
	if err != nil {
		t.Fatalf("Get on miss: err=%v want nil", err)
	}
	if ok {
		t.Fatalf("Get on miss: ok=true want false")
	}
	if tok.AccessToken != "" || tok.Source != "" {
		t.Fatalf("Get on miss: token=%+v want zero value", tok)
	}
}

func TestTokenStore_Delete_Idempotent(t *testing.T) {
	t.Parallel()
	ts, _ := mkTokenStore(t)
	ctx := mkCtx(t, mkIdentity(t))
	// Delete missing record — no error.
	if err := ts.Delete(ctx, ScopeUser, tDummyUser, "nonexistent"); err != nil {
		t.Fatalf("Delete on miss: %v", err)
	}
	// Put then delete — gone.
	tok := Token{
		Source:       tDummySource,
		BindingScope: ScopeUser,
		TenantID:     tDummyTenant,
		UserID:       tDummyUser,
		AccessToken:  tDummyAccess,
	}
	if err := ts.Put(ctx, tok); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := ts.Delete(ctx, ScopeUser, tDummyUser, tDummySource); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, ok, err := ts.Get(ctx, ScopeUser, tDummyUser, tDummySource)
	if err != nil {
		t.Fatalf("Get after delete: %v", err)
	}
	if ok {
		t.Fatalf("Get after delete: ok=true; token still present")
	}
}

func TestTokenStore_MissingIdentity_FailsLoud(t *testing.T) {
	t.Parallel()
	ts, _ := mkTokenStore(t)
	noIDCtx := context.Background()
	_, _, err := ts.Get(noIDCtx, ScopeUser, tDummyUser, tDummySource)
	if !errors.Is(err, ErrIdentityRequired) {
		t.Fatalf("Get no-identity: want ErrIdentityRequired, got %v", err)
	}
	tok := Token{
		Source: tDummySource, BindingScope: ScopeUser, TenantID: tDummyTenant,
		UserID: tDummyUser, AccessToken: tDummyAccess,
	}
	if err := ts.Put(noIDCtx, tok); !errors.Is(err, ErrIdentityRequired) {
		t.Fatalf("Put no-identity: want ErrIdentityRequired, got %v", err)
	}
}

func TestTokenStore_CrossTenantIsolation(t *testing.T) {
	t.Parallel()
	ts, _ := mkTokenStore(t)

	idA := identity.Identity{TenantID: "tenantA", UserID: "ua", SessionID: "sa"}
	idB := identity.Identity{TenantID: "tenantB", UserID: "ub", SessionID: "sb"}

	ctxA := mkCtx(t, idA)
	ctxB := mkCtx(t, idB)

	tokA := Token{
		Source:       tDummySource,
		BindingScope: ScopeUser,
		TenantID:     idA.TenantID,
		UserID:       idA.UserID,
		AccessToken:  "secretA",
	}
	tokB := Token{
		Source:       tDummySource,
		BindingScope: ScopeUser,
		TenantID:     idB.TenantID,
		UserID:       idB.UserID,
		AccessToken:  "secretB",
	}
	if err := ts.Put(ctxA, tokA); err != nil {
		t.Fatalf("Put A: %v", err)
	}
	if err := ts.Put(ctxB, tokB); err != nil {
		t.Fatalf("Put B: %v", err)
	}
	// A reads its own → secretA.
	gotA, _, err := ts.Get(ctxA, ScopeUser, idA.UserID, tDummySource)
	if err != nil {
		t.Fatalf("Get A: %v", err)
	}
	if gotA.AccessToken != "secretA" {
		t.Fatalf("A read: got %q want secretA", gotA.AccessToken)
	}
	// B reads B → secretB. Crucially, B reading via UserID 'ua'
	// (A's user) still keys by ctxB's tenant — A's record is
	// unreachable.
	gotBImpostor, ok, err := ts.Get(ctxB, ScopeUser, idA.UserID, tDummySource)
	if err != nil {
		t.Fatalf("Get B-impostor: %v", err)
	}
	if ok {
		t.Fatalf("B reading with A's userID via ctxB returned %+v — cross-tenant leak", gotBImpostor)
	}
}

func TestTokenStore_CrossAgentIsolation(t *testing.T) {
	t.Parallel()
	ts, _ := mkTokenStore(t)
	id := mkIdentity(t)
	ctx := mkCtx(t, id)

	tokAlpha := Token{
		Source:       tDummySource,
		BindingScope: ScopeAgent,
		TenantID:     id.TenantID,
		AgentID:      "agent-alpha",
		AccessToken:  "secret-alpha",
	}
	tokBeta := Token{
		Source:       tDummySource,
		BindingScope: ScopeAgent,
		TenantID:     id.TenantID,
		AgentID:      "agent-beta",
		AccessToken:  "secret-beta",
	}
	if err := ts.Put(ctx, tokAlpha); err != nil {
		t.Fatalf("Put alpha: %v", err)
	}
	if err := ts.Put(ctx, tokBeta); err != nil {
		t.Fatalf("Put beta: %v", err)
	}
	gotAlpha, _, err := ts.Get(ctx, ScopeAgent, "agent-alpha", tDummySource)
	if err != nil {
		t.Fatalf("Get alpha: %v", err)
	}
	if gotAlpha.AccessToken != "secret-alpha" {
		t.Fatalf("alpha read leaked: %q", gotAlpha.AccessToken)
	}
	gotBeta, _, err := ts.Get(ctx, ScopeAgent, "agent-beta", tDummySource)
	if err != nil {
		t.Fatalf("Get beta: %v", err)
	}
	if gotBeta.AccessToken != "secret-beta" {
		t.Fatalf("beta read leaked: %q", gotBeta.AccessToken)
	}
}

func TestTokenStore_MixedScopeCoexistence(t *testing.T) {
	t.Parallel()
	ts, _ := mkTokenStore(t)
	id := mkIdentity(t)
	ctx := mkCtx(t, id)

	tokUser := Token{
		Source:       tDummySource,
		BindingScope: ScopeUser,
		TenantID:     id.TenantID,
		UserID:       id.UserID,
		AccessToken:  "user-token",
	}
	tokAgent := Token{
		Source:       tDummySource,
		BindingScope: ScopeAgent,
		TenantID:     id.TenantID,
		AgentID:      tDummyAgent,
		AccessToken:  "agent-token",
	}
	if err := ts.Put(ctx, tokUser); err != nil {
		t.Fatalf("Put user: %v", err)
	}
	if err := ts.Put(ctx, tokAgent); err != nil {
		t.Fatalf("Put agent: %v", err)
	}
	gotUser, _, _ := ts.Get(ctx, ScopeUser, id.UserID, tDummySource)
	gotAgent, _, _ := ts.Get(ctx, ScopeAgent, tDummyAgent, tDummySource)
	if gotUser.AccessToken != "user-token" {
		t.Fatalf("user-scope readback: %q", gotUser.AccessToken)
	}
	if gotAgent.AccessToken != "agent-token" {
		t.Fatalf("agent-scope readback: %q", gotAgent.AccessToken)
	}
	// Verify scope discriminator on the readback.
	if gotUser.BindingScope != ScopeUser || gotAgent.BindingScope != ScopeAgent {
		t.Fatalf("scope discriminator drift on readback: user=%s agent=%s",
			gotUser.BindingScope, gotAgent.BindingScope)
	}
}

func TestTokenStore_EncryptionAtRest_CiphertextNotPlaintext(t *testing.T) {
	t.Parallel()
	// Open the store ourselves so we can inspect the raw stored bytes.
	store := mkStore(t)
	sealer, err := NewAESGCMSealer(fixedKEK(t))
	if err != nil {
		t.Fatalf("NewAESGCMSealer: %v", err)
	}
	ts, err := NewTokenStore(store, sealer)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	id := mkIdentity(t)
	ctx := mkCtx(t, id)
	const verySensitiveAccess = "ACCESS-TOKEN-MARKER-PLAINTEXT-XYZ"
	const verySensitiveRefresh = "REFRESH-TOKEN-MARKER-PLAINTEXT-ABC"
	tok := Token{
		Source:       tDummySource,
		BindingScope: ScopeUser,
		TenantID:     id.TenantID,
		UserID:       id.UserID,
		AccessToken:  verySensitiveAccess,
		RefreshToken: verySensitiveRefresh,
	}
	if err := ts.Put(ctx, tok); err != nil {
		t.Fatalf("Put: %v", err)
	}

	q := identity.Quadruple{Identity: id}
	rec, err := store.Load(ctx, q, tokenKind(ScopeUser, id.UserID, tDummySource))
	if err != nil {
		t.Fatalf("raw Load: %v", err)
	}
	if bytes.Contains(rec.Bytes, []byte(verySensitiveAccess)) {
		t.Fatalf("AT-REST LEAK: raw StateStore bytes contain access-token plaintext.")
	}
	if bytes.Contains(rec.Bytes, []byte(verySensitiveRefresh)) {
		t.Fatalf("AT-REST LEAK: raw StateStore bytes contain refresh-token plaintext.")
	}
	if !strings.HasPrefix(string(rec.Bytes), "{") {
		t.Fatalf("envelope not JSON-shaped; got prefix %q", string(rec.Bytes[:min(16, len(rec.Bytes))]))
	}

	// Refresh sibling record — same check on the refresh-only blob.
	refRec, err := store.Load(ctx, q, refreshKind(ScopeUser, id.UserID, tDummySource))
	if err != nil {
		t.Fatalf("raw Load refresh: %v", err)
	}
	if bytes.Contains(refRec.Bytes, []byte(verySensitiveRefresh)) {
		t.Fatalf("AT-REST LEAK: refresh sibling contains refresh plaintext.")
	}
}

func TestTokenStore_NilStore_FailsLoud(t *testing.T) {
	t.Parallel()
	sealer, _ := NewAESGCMSealer(fixedKEK(t))
	_, err := NewTokenStore(nil, sealer)
	if err == nil {
		t.Fatalf("nil store should fail loud")
	}
}

func TestTokenStore_NilSealer_FailsLoud(t *testing.T) {
	t.Parallel()
	store := mkStore(t)
	_, err := NewTokenStore(store, nil)
	if err == nil {
		t.Fatalf("nil sealer should fail loud — encryption-at-rest is mandatory")
	}
}

func TestTokenStore_KindBelongsToToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind string
		want bool
	}{
		{tokenKind(ScopeUser, "u", "s"), true},
		{refreshKind(ScopeAgent, "a", "s"), true},
		{"sessions.lifecycle", false},
		{"agent.record.xyz", false},
		{"", false},
	}
	for _, c := range cases {
		if got := kindBelongsToToken(c.kind); got != c.want {
			t.Errorf("kindBelongsToToken(%q) = %v, want %v", c.kind, got, c.want)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
