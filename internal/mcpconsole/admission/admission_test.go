package admission

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// fixedKEK returns a deterministic dev KEK. NEVER used outside tests.
// The pattern (a KEK built from a known seed in a test helper) mirrors
// the documented-dummy-values testdata-fixture convention.
func fixedKEK(t *testing.T) []byte {
	t.Helper()
	kek := make([]byte, auth.KEKSizeBytes)
	for i := range kek {
		kek[i] = byte(i)
	}
	return kek
}

// freshKEK returns a random KEK for tests that need an independent
// sealing context (the "different sealer fails" assertions).
func freshKEK(t *testing.T) []byte {
	t.Helper()
	kek := make([]byte, auth.KEKSizeBytes)
	if _, err := rand.Read(kek); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return kek
}

// testSealer returns the shared-sealer fixture used by most tests.
func testSealer(t *testing.T) auth.Sealer {
	t.Helper()
	s, err := auth.NewAESGCMSealer(fixedKEK(t))
	if err != nil {
		t.Fatalf("NewAESGCMSealer: %v", err)
	}
	return s
}

// fixedNow is the deterministic clock anchor for time-bound tests.
var fixedNow = time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

// clockAt returns a clock pinned to now.
func clockAt(now time.Time) func() time.Time {
	return func() time.Time { return now }
}

// testTuple returns a render tuple unique per i: every dimension
// varies with i, so two tuples for different i never collide.
func testTuple(i int) RenderTuple {
	return RenderTuple{
		Identity: identity.Identity{
			TenantID:  fmt.Sprintf("tenant-%03d", i),
			UserID:    fmt.Sprintf("user-%03d", i),
			SessionID: fmt.Sprintf("session-%03d", i),
		},
		AgentID:               fmt.Sprintf("agent-%03d", i),
		ServerID:              fmt.Sprintf("server-%03d", i),
		ResourceURI:           fmt.Sprintf("ui://app/render-%03d.html", i),
		DescriptorFingerprint: fmt.Sprintf("gen-%03d", i),
	}
}

// handClaims returns a well-formed claim set for structural tests.
func handClaims(now time.Time) Claims {
	return Claims{
		Schema:               Schema,
		Version:              SchemaVersion,
		TenantID:             "tenant-hand",
		UserID:               "user-hand",
		SessionID:            "session-hand",
		AgentID:              "agent-hand",
		ServerID:             "server-hand",
		ResourceURI:          "ui://app/hand.html",
		DescriptorGeneration: "gen-hand",
		IssuedAt:             now.Unix(),
		ExpiresAt:            now.Add(15 * time.Minute).Unix(),
		Nonce:                base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xAB}, NonceSize)),
	}
}

// handTuple returns the RenderTuple matching handClaims.
func handTuple() RenderTuple {
	return RenderTuple{
		Identity: identity.Identity{
			TenantID:  "tenant-hand",
			UserID:    "user-hand",
			SessionID: "session-hand",
		},
		AgentID:               "agent-hand",
		ServerID:              "server-hand",
		ResourceURI:           "ui://app/hand.html",
		DescriptorFingerprint: "gen-hand",
	}
}

// sealedToken canonical-encodes and seals claims, returning the opaque
// base64url token — the same shape Mint produces.
func sealedToken(t *testing.T, sealer auth.Sealer, c Claims) string {
	t.Helper()
	pt, err := canonicalJSON(c)
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	return sealedPlaintextToken(t, sealer, pt)
}

// sealedPlaintextToken seals raw claims plaintext (used by the
// structural-invalid tests that need a non-canonical or malformed
// claim body).
func sealedPlaintextToken(t *testing.T, sealer auth.Sealer, pt []byte) string {
	t.Helper()
	sealed, err := sealer.Seal(pt)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(sealed)
}

func TestNew_NilSealer_FailsLoud(t *testing.T) {
	t.Parallel()
	_, err := New(nil)
	if !errors.Is(err, ErrNilSealer) {
		t.Fatalf("New(nil) error = %v, want wrapped ErrNilSealer", err)
	}
}

func TestNew_WithClock_Nil_FailsLoud(t *testing.T) {
	t.Parallel()
	_, err := New(testSealer(t), WithClock(nil))
	if err == nil {
		t.Fatal("New(WithClock(nil)) succeeded, want loud failure")
	}
}

func TestNew_WithTTL_Bounds(t *testing.T) {
	t.Parallel()
	bad := []time.Duration{0, -time.Second, MaxTTL + time.Hour}
	for _, ttl := range bad {
		if _, err := New(testSealer(t), WithTTL(ttl)); err == nil {
			t.Fatalf("New(WithTTL(%s)) succeeded, want error", ttl)
		}
	}
	if _, err := New(testSealer(t), WithTTL(2*time.Minute)); err != nil {
		t.Fatalf("New(WithTTL(2m)): %v", err)
	}
	if _, err := New(testSealer(t), WithTTL(MaxTTL)); err != nil {
		t.Fatalf("New(WithTTL(MaxTTL)): %v", err)
	}
}

func TestMint_InvalidTuple_FailsLoud(t *testing.T) {
	t.Parallel()
	a, err := New(testSealer(t), WithClock(clockAt(fixedNow)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	base := testTuple(0)
	cases := map[string]RenderTuple{
		"empty tenant":  mutate(base, func(rt *RenderTuple) { rt.Identity.TenantID = "" }),
		"empty user":    mutate(base, func(rt *RenderTuple) { rt.Identity.UserID = "" }),
		"empty session": mutate(base, func(rt *RenderTuple) { rt.Identity.SessionID = "" }),
		"empty agent":   mutate(base, func(rt *RenderTuple) { rt.AgentID = "" }),
		"empty server":  mutate(base, func(rt *RenderTuple) { rt.ServerID = "" }),
		"empty resource": mutate(base, func(rt *RenderTuple) {
			rt.ResourceURI = ""
		}),
		"empty fingerprint": mutate(base, func(rt *RenderTuple) {
			rt.DescriptorFingerprint = ""
		}),
		"non-ui resource": mutate(base, func(rt *RenderTuple) {
			rt.ResourceURI = "https://example.com/app.html"
		}),
		"nul byte": mutate(base, func(rt *RenderTuple) { rt.AgentID = "ag\x00ent" }),
		"invalid utf-8": mutate(base, func(rt *RenderTuple) {
			rt.AgentID = "agent-\xff-broken"
		}),
		"byte bound": mutate(base, func(rt *RenderTuple) {
			rt.ServerID = strings.Repeat("s", MaxClaimStringBytes+1)
		}),
		"rune bound": mutate(base, func(rt *RenderTuple) {
			rt.ResourceURI = "ui://" + strings.Repeat("a", MaxClaimStringRunes)
		}),
	}
	for name, tuple := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := a.Mint(ctx, tuple); !errors.Is(err, ErrInvalidMintInput) {
				t.Fatalf("Mint error = %v, want wrapped ErrInvalidMintInput", err)
			}
		})
	}
}

// mutate copies rt, applies fn, and returns the copy.
func mutate(rt RenderTuple, fn func(*RenderTuple)) RenderTuple {
	cp := rt
	fn(&cp)
	return cp
}

func TestMint_DefaultTTL_15Minutes(t *testing.T) {
	t.Parallel()
	a, err := New(testSealer(t), WithClock(clockAt(fixedNow)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tok, err := a.Mint(context.Background(), testTuple(0))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if got := tok.ExpiresAt.Sub(tok.IssuedAt); got != DefaultTTL {
		t.Fatalf("lifetime = %s, want %s", got, DefaultTTL)
	}
	if !tok.IssuedAt.Equal(fixedNow) {
		t.Fatalf("IssuedAt = %s, want %s", tok.IssuedAt, fixedNow)
	}
}

func TestMint_WithTTL_Applied(t *testing.T) {
	t.Parallel()
	a, err := New(testSealer(t), WithClock(clockAt(fixedNow)), WithTTL(2*time.Minute))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tok, err := a.Mint(context.Background(), testTuple(0))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if got := tok.ExpiresAt.Sub(tok.IssuedAt); got != 2*time.Minute {
		t.Fatalf("lifetime = %s, want 2m", got)
	}
}

func TestMint_Verify_RoundTrip(t *testing.T) {
	t.Parallel()
	a, err := New(testSealer(t), WithClock(clockAt(fixedNow)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tuple := testTuple(7)
	tok, err := a.Mint(context.Background(), tuple)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	claims, err := a.Verify(context.Background(), tuple, tok.Value)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Schema != Schema || claims.Version != SchemaVersion {
		t.Fatalf("claims schema/version = %q/%d, want %q/%d",
			claims.Schema, claims.Version, Schema, SchemaVersion)
	}
	if claims.TenantID != tuple.Identity.TenantID ||
		claims.UserID != tuple.Identity.UserID ||
		claims.SessionID != tuple.Identity.SessionID {
		t.Fatalf("claims identity mismatch: %+v", claims)
	}
	if claims.AgentID != tuple.AgentID ||
		claims.ServerID != tuple.ServerID ||
		claims.ResourceURI != tuple.ResourceURI ||
		claims.DescriptorGeneration != tuple.DescriptorFingerprint {
		t.Fatalf("claims tuple mismatch: %+v", claims)
	}
	if !claims.IssuedAtTime().Equal(tok.IssuedAt) || !claims.ExpiresAtTime().Equal(tok.ExpiresAt) {
		t.Fatalf("claims instants differ from token metadata: %+v vs %+v", claims, tok)
	}
	nonce, err := base64.RawURLEncoding.DecodeString(claims.Nonce)
	if err != nil {
		t.Fatalf("nonce decode: %v", err)
	}
	if len(nonce) != NonceSize {
		t.Fatalf("nonce is %d bytes, want %d", len(nonce), NonceSize)
	}
}

func TestMint_Claims_ExactCardinality(t *testing.T) {
	t.Parallel()
	sealer := testSealer(t)
	a, err := New(sealer, WithClock(clockAt(fixedNow)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tok, err := a.Mint(context.Background(), testTuple(0))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	sealed, err := base64.RawURLEncoding.DecodeString(tok.Value)
	if err != nil {
		t.Fatalf("token decode: %v", err)
	}
	pt, err := sealer.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(pt, &m); err != nil {
		t.Fatalf("claims unmarshal: %v", err)
	}
	want := []string{
		"schema", "version",
		"tenant_id", "user_id", "session_id",
		"agent_id", "server_id", "resource_uri", "descriptor_generation",
		"issued_at", "expires_at", "nonce",
	}
	if len(m) != len(want) {
		t.Fatalf("claim set has %d keys, want %d (%v)", len(m), len(want), keys(m))
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Fatalf("claim set missing key %q; got %v", k, keys(m))
		}
	}
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestMint_SameTuple_TokensDistinct(t *testing.T) {
	t.Parallel()
	a, err := New(testSealer(t), WithClock(clockAt(fixedNow)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tuple := testTuple(3)
	tok1, err := a.Mint(context.Background(), tuple)
	if err != nil {
		t.Fatalf("Mint 1: %v", err)
	}
	tok2, err := a.Mint(context.Background(), tuple)
	if err != nil {
		t.Fatalf("Mint 2: %v", err)
	}
	if tok1.Value == tok2.Value {
		t.Fatal("two mints of the identical tuple produced the identical token")
	}
	if !tok1.IssuedAt.Equal(tok2.IssuedAt) || !tok1.ExpiresAt.Equal(tok2.ExpiresAt) {
		t.Fatalf("metadata drifted across identical-tuple mints: %+v vs %+v", tok1, tok2)
	}
	if _, err := a.Verify(context.Background(), tuple, tok1.Value); err != nil {
		t.Fatalf("Verify tok1: %v", err)
	}
	if _, err := a.Verify(context.Background(), tuple, tok2.Value); err != nil {
		t.Fatalf("Verify tok2: %v", err)
	}
}

func TestVerify_Missing(t *testing.T) {
	t.Parallel()
	a, err := New(testSealer(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.Verify(context.Background(), testTuple(0), ""); !errors.Is(err, ErrTokenMissing) {
		t.Fatalf("Verify(\"\") error = %v, want ErrTokenMissing", err)
	}
}

func TestVerify_Unavailable_GarbageToken(t *testing.T) {
	t.Parallel()
	a, err := New(testSealer(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, tok := range []string{"%%%not-base64%%%", "abc", "!!!"} {
		if _, err := a.Verify(context.Background(), testTuple(0), tok); !errors.Is(err, ErrTokenUnavailable) {
			t.Fatalf("Verify(%q) error = %v, want ErrTokenUnavailable", tok, err)
		}
	}
}

func TestVerify_Unavailable_OversizedToken(t *testing.T) {
	t.Parallel()
	a, err := New(testSealer(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	big := strings.Repeat("A", MaxTokenBytes+1)
	if _, err := a.Verify(context.Background(), testTuple(0), big); !errors.Is(err, ErrTokenUnavailable) {
		t.Fatalf("Verify(oversized) error = %v, want ErrTokenUnavailable", err)
	}
}

func TestVerify_Unavailable_TamperedToken(t *testing.T) {
	t.Parallel()
	a, err := New(testSealer(t), WithClock(clockAt(fixedNow)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tok, err := a.Mint(context.Background(), testTuple(0))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok.Value)
	if err != nil {
		t.Fatalf("token decode: %v", err)
	}
	// Flip one byte in the middle of the sealed envelope: AES-GCM
	// authentication must fail closed.
	raw[len(raw)/2] ^= 0x01
	tampered := base64.RawURLEncoding.EncodeToString(raw)
	if _, err := a.Verify(context.Background(), testTuple(0), tampered); !errors.Is(err, ErrTokenUnavailable) {
		t.Fatalf("Verify(tampered) error = %v, want ErrTokenUnavailable", err)
	}
	// Truncated envelope: too short to be a sealed admission.
	truncated := base64.RawURLEncoding.EncodeToString(raw[:len(raw)-4])
	if _, err := a.Verify(context.Background(), testTuple(0), truncated); !errors.Is(err, ErrTokenUnavailable) {
		t.Fatalf("Verify(truncated) error = %v, want ErrTokenUnavailable", err)
	}
}

func TestVerify_Unavailable_WrongSealer(t *testing.T) {
	t.Parallel()
	a1, err := New(testSealer(t), WithClock(clockAt(fixedNow)))
	if err != nil {
		t.Fatalf("New a1: %v", err)
	}
	other, err := auth.NewAESGCMSealer(freshKEK(t))
	if err != nil {
		t.Fatalf("NewAESGCMSealer(other): %v", err)
	}
	a2, err := New(other, WithClock(clockAt(fixedNow)))
	if err != nil {
		t.Fatalf("New a2: %v", err)
	}
	tok, err := a1.Mint(context.Background(), testTuple(0))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := a2.Verify(context.Background(), testTuple(0), tok.Value); !errors.Is(err, ErrTokenUnavailable) {
		t.Fatalf("Verify(wrong sealer) error = %v, want ErrTokenUnavailable", err)
	}
}

func TestRestart_SameSealer_VerifiesAcrossInstances(t *testing.T) {
	t.Parallel()
	sealer := testSealer(t)
	a1, err := New(sealer, WithClock(clockAt(fixedNow)))
	if err != nil {
		t.Fatalf("New a1: %v", err)
	}
	tuple := testTuple(11)
	tok, err := a1.Mint(context.Background(), tuple)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// "Restart": a fresh Authority sharing the same sealer must verify
	// the token minted before restart. Even a drifted clock within skew
	// stays admissible.
	a2, err := New(sealer, WithClock(clockAt(fixedNow.Add(2*time.Minute))))
	if err != nil {
		t.Fatalf("New a2: %v", err)
	}
	if _, err := a2.Verify(context.Background(), tuple, tok.Value); err != nil {
		t.Fatalf("Verify across restart: %v", err)
	}
}

func TestVerify_Invalid_GarbagePlaintext(t *testing.T) {
	t.Parallel()
	sealer := testSealer(t)
	a, err := New(sealer, WithClock(clockAt(fixedNow)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tok := sealedPlaintextToken(t, sealer, []byte("{definitely not canonical claims"))
	if _, err := a.Verify(context.Background(), handTuple(), tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("Verify(garbage plaintext) error = %v, want ErrTokenInvalid", err)
	}
}

func TestVerify_Invalid_UnknownVersion(t *testing.T) {
	t.Parallel()
	sealer := testSealer(t)
	a, err := New(sealer, WithClock(clockAt(fixedNow)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := handClaims(fixedNow)
	c.Version = SchemaVersion + 1
	tok := sealedToken(t, sealer, c)
	if _, err := a.Verify(context.Background(), handTuple(), tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("Verify(unknown version) error = %v, want ErrTokenInvalid", err)
	}
}

func TestVerify_Invalid_UnknownSchema(t *testing.T) {
	t.Parallel()
	sealer := testSealer(t)
	a, err := New(sealer, WithClock(clockAt(fixedNow)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := handClaims(fixedNow)
	c.Schema = "harbor.other.admission"
	tok := sealedToken(t, sealer, c)
	if _, err := a.Verify(context.Background(), handTuple(), tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("Verify(unknown schema) error = %v, want ErrTokenInvalid", err)
	}
}

func TestVerify_Invalid_UnknownField(t *testing.T) {
	t.Parallel()
	sealer := testSealer(t)
	a, err := New(sealer, WithClock(clockAt(fixedNow)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tok := claimsWithExtraKey(t, sealer, handClaims(fixedNow), "callback_name", "onToolCall")
	if _, err := a.Verify(context.Background(), handTuple(), tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("Verify(unknown field) error = %v, want ErrTokenInvalid (DisallowUnknownFields)", err)
	}
}

func TestVerify_Invalid_TrailingData(t *testing.T) {
	t.Parallel()
	sealer := testSealer(t)
	a, err := New(sealer, WithClock(clockAt(fixedNow)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pt, err := canonicalJSON(handClaims(fixedNow))
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	pt = append(pt, []byte(" 42")...)
	tok := sealedPlaintextToken(t, sealer, pt)
	if _, err := a.Verify(context.Background(), handTuple(), tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("Verify(trailing data) error = %v, want ErrTokenInvalid", err)
	}
}

func TestVerify_Invalid_RawInvalidUTF8(t *testing.T) {
	t.Parallel()
	sealer := testSealer(t)
	a, err := New(sealer, WithClock(clockAt(fixedNow)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pt, err := canonicalJSON(handClaims(fixedNow))
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	pt = append(pt, 0xff)
	tok := sealedPlaintextToken(t, sealer, pt)
	if _, err := a.Verify(context.Background(), handTuple(), tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("Verify(raw invalid utf-8) error = %v, want ErrTokenInvalid", err)
	}
}

func TestVerify_Invalid_NonUIResource(t *testing.T) {
	t.Parallel()
	sealer := testSealer(t)
	a, err := New(sealer, WithClock(clockAt(fixedNow)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := handClaims(fixedNow)
	c.ResourceURI = "https://example.com/app.html"
	tok := sealedToken(t, sealer, c)
	if _, err := a.Verify(context.Background(), handTuple(), tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("Verify(non-ui resource) error = %v, want ErrTokenInvalid", err)
	}
}

func TestVerify_Invalid_BadNonce(t *testing.T) {
	t.Parallel()
	sealer := testSealer(t)
	a, err := New(sealer, WithClock(clockAt(fixedNow)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := handClaims(fixedNow)
	c.Nonce = base64.RawURLEncoding.EncodeToString([]byte{0x01}) // 1 byte
	tok := sealedToken(t, sealer, c)
	if _, err := a.Verify(context.Background(), handTuple(), tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("Verify(short nonce) error = %v, want ErrTokenInvalid", err)
	}
	c.Nonce = "!!!not-base64!!!"
	tok = sealedToken(t, sealer, c)
	if _, err := a.Verify(context.Background(), handTuple(), tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("Verify(malformed nonce) error = %v, want ErrTokenInvalid", err)
	}
}

func TestVerify_Invalid_EmptyClaimField(t *testing.T) {
	t.Parallel()
	sealer := testSealer(t)
	a, err := New(sealer, WithClock(clockAt(fixedNow)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := handClaims(fixedNow)
	c.TenantID = ""
	tok := sealedToken(t, sealer, c)
	if _, err := a.Verify(context.Background(), handTuple(), tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("Verify(empty claim field) error = %v, want ErrTokenInvalid", err)
	}
}

func TestVerify_Invalid_AbsurdLifetime(t *testing.T) {
	t.Parallel()
	sealer := testSealer(t)
	a, err := New(sealer, WithClock(clockAt(fixedNow)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Lifetime beyond MaxTTL.
	c := handClaims(fixedNow)
	c.ExpiresAt = c.IssuedAt + int64(MaxTTL/time.Second) + 3600
	tok := sealedToken(t, sealer, c)
	if _, err := a.Verify(context.Background(), handTuple(), tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("Verify(oversized lifetime) error = %v, want ErrTokenInvalid", err)
	}
	// Zero lifetime.
	c = handClaims(fixedNow)
	c.ExpiresAt = c.IssuedAt
	tok = sealedToken(t, sealer, c)
	if _, err := a.Verify(context.Background(), handTuple(), tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("Verify(zero lifetime) error = %v, want ErrTokenInvalid", err)
	}
	// Negative lifetime.
	c = handClaims(fixedNow)
	c.ExpiresAt = c.IssuedAt - 60
	tok = sealedToken(t, sealer, c)
	if _, err := a.Verify(context.Background(), handTuple(), tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("Verify(negative lifetime) error = %v, want ErrTokenInvalid", err)
	}
}

func TestVerify_Invalid_FutureIssuance(t *testing.T) {
	t.Parallel()
	sealer := testSealer(t)
	mint, err := New(sealer, WithClock(clockAt(fixedNow)))
	if err != nil {
		t.Fatalf("New mint: %v", err)
	}
	tuple := testTuple(0)
	tok, err := mint.Mint(context.Background(), tuple)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// A verifier whose clock is more than MaxClockSkew BEHIND the
	// issuer sees an admission issued in the future.
	past := fixedNow.Add(-10 * time.Minute)
	verifier, err := New(sealer, WithClock(clockAt(past)))
	if err != nil {
		t.Fatalf("New verifier: %v", err)
	}
	if _, err := verifier.Verify(context.Background(), tuple, tok.Value); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("Verify(future issuance) error = %v, want ErrTokenInvalid", err)
	}
}

func TestVerify_Expired(t *testing.T) {
	t.Parallel()
	sealer := testSealer(t)
	mint, err := New(sealer, WithClock(clockAt(fixedNow)))
	if err != nil {
		t.Fatalf("New mint: %v", err)
	}
	tuple := testTuple(0)
	tok, err := mint.Mint(context.Background(), tuple)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Past TTL + skew.
	late := fixedNow.Add(DefaultTTL + MaxClockSkew + time.Second)
	verifier, err := New(sealer, WithClock(clockAt(late)))
	if err != nil {
		t.Fatalf("New verifier: %v", err)
	}
	if _, err := verifier.Verify(context.Background(), tuple, tok.Value); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("Verify(expired) error = %v, want ErrTokenExpired", err)
	}
}

func TestVerify_WithinClockSkew_Boundaries(t *testing.T) {
	t.Parallel()
	sealer := testSealer(t)
	mint, err := New(sealer, WithClock(clockAt(fixedNow)))
	if err != nil {
		t.Fatalf("New mint: %v", err)
	}
	tuple := testTuple(0)
	tok, err := mint.Mint(context.Background(), tuple)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Exactly at expiry + skew: not yet expired.
	edge := fixedNow.Add(DefaultTTL + MaxClockSkew)
	verifier, err := New(sealer, WithClock(clockAt(edge)))
	if err != nil {
		t.Fatalf("New verifier: %v", err)
	}
	if _, err := verifier.Verify(context.Background(), tuple, tok.Value); err != nil {
		t.Fatalf("Verify at expiry+skew boundary: %v", err)
	}
	// Exactly at issued - skew: not yet "future".
	early := fixedNow.Add(-MaxClockSkew)
	verifier, err = New(sealer, WithClock(clockAt(early)))
	if err != nil {
		t.Fatalf("New verifier: %v", err)
	}
	if _, err := verifier.Verify(context.Background(), tuple, tok.Value); err != nil {
		t.Fatalf("Verify at issued-skew boundary: %v", err)
	}
}

func TestVerify_Mismatch_EachDimension_NoLeak(t *testing.T) {
	t.Parallel()
	a, err := New(testSealer(t), WithClock(clockAt(fixedNow)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tuple := testTuple(5)
	tok, err := a.Mint(context.Background(), tuple)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*RenderTuple)
	}{
		{"tenant", func(rt *RenderTuple) { rt.Identity.TenantID = "foreign-tenant" }},
		{"user", func(rt *RenderTuple) { rt.Identity.UserID = "foreign-user" }},
		{"session", func(rt *RenderTuple) { rt.Identity.SessionID = "foreign-session" }},
		{"agent", func(rt *RenderTuple) { rt.AgentID = "foreign-agent" }},
		{"server", func(rt *RenderTuple) { rt.ServerID = "foreign-server" }},
		{"resource", func(rt *RenderTuple) { rt.ResourceURI = "ui://app/foreign.html" }},
		{"generation", func(rt *RenderTuple) { rt.DescriptorFingerprint = "gen-old" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expected := mutate(tuple, tc.mutate)
			_, err := a.Verify(context.Background(), expected, tok.Value)
			if !errors.Is(err, ErrTokenMismatch) {
				t.Fatalf("Verify error = %v, want ErrTokenMismatch", err)
			}
			// The mismatch must not leak WHICH dimension differed:
			// the error is the bare sentinel, no dimension detail.
			if err.Error() != ErrTokenMismatch.Error() {
				t.Fatalf("mismatch error leaks detail: %q", err)
			}
		})
	}
}

func TestVerify_Mismatch_ExpiredWinsOverMismatch(t *testing.T) {
	t.Parallel()
	sealer := testSealer(t)
	mint, err := New(sealer, WithClock(clockAt(fixedNow)))
	if err != nil {
		t.Fatalf("New mint: %v", err)
	}
	tok, err := mint.Mint(context.Background(), testTuple(0))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// A different tuple AND an expired clock: time validity classifies
	// before tuple matching, so the outcome is Expired, never Mismatch.
	late := fixedNow.Add(DefaultTTL + MaxClockSkew + time.Second)
	verifier, err := New(sealer, WithClock(clockAt(late)))
	if err != nil {
		t.Fatalf("New verifier: %v", err)
	}
	if _, err := verifier.Verify(context.Background(), testTuple(1), tok.Value); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("Verify(expired+foreign tuple) error = %v, want ErrTokenExpired", err)
	}
}

func TestMint_CancelledContext(t *testing.T) {
	t.Parallel()
	a, err := New(testSealer(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Mint(ctx, testTuple(0)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Mint(cancelled ctx) error = %v, want context.Canceled", err)
	}
}

func TestVerify_CancelledContext(t *testing.T) {
	t.Parallel()
	a, err := New(testSealer(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tok, err := a.Mint(context.Background(), testTuple(0))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Verify(ctx, testTuple(0), tok.Value); !errors.Is(err, context.Canceled) {
		t.Fatalf("Verify(cancelled ctx) error = %v, want context.Canceled", err)
	}
}

// claimsWithExtraKey seals a claim set carrying an additional JSON key
// (the forged-field shape DisallowUnknownFields must reject).
func claimsWithExtraKey(t *testing.T, sealer auth.Sealer, c Claims, key string, val any) string {
	t.Helper()
	pt, err := canonicalJSON(c)
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(pt, &m); err != nil {
		t.Fatalf("claims unmarshal: %v", err)
	}
	m[key] = val
	forged, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("forged marshal: %v", err)
	}
	return sealedPlaintextToken(t, sealer, forged)
}
