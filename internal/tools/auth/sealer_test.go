package auth

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"
)

// fixedKEK returns a deterministic dev KEK. NEVER used outside tests.
// The pattern (kek built from a known seed in a test helper) mirrors
// the §7 rule 2 testdata-fixture convention — documented dummy values
// only.
func fixedKEK(t *testing.T) []byte {
	t.Helper()
	kek := make([]byte, KEKSizeBytes)
	for i := range kek {
		kek[i] = byte(i)
	}
	return kek
}

// freshKEK returns a random KEK; useful for tests that need
// independent encryption contexts.
func freshKEK(t *testing.T) []byte {
	t.Helper()
	kek := make([]byte, KEKSizeBytes)
	if _, err := rand.Read(kek); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return kek
}

func TestSealer_RoundTrip_HappyPath(t *testing.T) {
	t.Parallel()
	sealer, err := NewAESGCMSealer(fixedKEK(t))
	if err != nil {
		t.Fatalf("NewAESGCMSealer: %v", err)
	}
	plaintext := []byte("dummy-access-token-VALUE-not-a-real-secret")
	ciphertext, err := sealer.Seal(plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Equal(ciphertext, plaintext) {
		t.Fatalf("ciphertext == plaintext — encryption is a no-op")
	}
	if bytes.Contains(ciphertext, plaintext) {
		t.Fatalf("ciphertext contains plaintext bytes — encryption is broken")
	}
	got, err := sealer.Open(ciphertext)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("Open returned %q, want %q", got, plaintext)
	}
}

func TestSealer_FreshNoncePerCall(t *testing.T) {
	t.Parallel()
	sealer, err := NewAESGCMSealer(fixedKEK(t))
	if err != nil {
		t.Fatalf("NewAESGCMSealer: %v", err)
	}
	pt := []byte("constant-plaintext")
	c1, err := sealer.Seal(pt)
	if err != nil {
		t.Fatalf("Seal 1: %v", err)
	}
	c2, err := sealer.Seal(pt)
	if err != nil {
		t.Fatalf("Seal 2: %v", err)
	}
	if bytes.Equal(c1, c2) {
		t.Fatalf("two seals of same plaintext produced identical ciphertext — nonce reuse")
	}
}

func TestSealer_WrongKEK_RejectsLoud(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 16, 31, 33, 64} {
		_, err := NewAESGCMSealer(make([]byte, n))
		if err == nil {
			t.Fatalf("len=%d: expected ErrKEKMissing, got nil", n)
		}
		if !errors.Is(err, ErrKEKMissing) {
			t.Fatalf("len=%d: want ErrKEKMissing, got %v", n, err)
		}
	}
}

func TestSealer_Open_TamperedCipher_FailsLoud(t *testing.T) {
	t.Parallel()
	sealer, err := NewAESGCMSealer(fixedKEK(t))
	if err != nil {
		t.Fatalf("NewAESGCMSealer: %v", err)
	}
	ct, err := sealer.Seal([]byte("data"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Flip the last byte (in the GCM tag).
	tampered := append([]byte(nil), ct...)
	tampered[len(tampered)-1] ^= 0xFF
	_, err = sealer.Open(tampered)
	if !errors.Is(err, ErrTokenCipherCorrupt) {
		t.Fatalf("want ErrTokenCipherCorrupt, got %v", err)
	}
}

func TestSealer_Open_WrongKEK_FailsLoud(t *testing.T) {
	t.Parallel()
	sealerA, _ := NewAESGCMSealer(fixedKEK(t))
	sealerB, _ := NewAESGCMSealer(freshKEK(t))
	ct, err := sealerA.Seal([]byte("data"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	_, err = sealerB.Open(ct)
	if !errors.Is(err, ErrTokenCipherCorrupt) {
		t.Fatalf("want ErrTokenCipherCorrupt with wrong KEK, got %v", err)
	}
}

func TestSealer_Open_TooShort_FailsLoud(t *testing.T) {
	t.Parallel()
	sealer, _ := NewAESGCMSealer(fixedKEK(t))
	_, err := sealer.Open(make([]byte, 3))
	if !errors.Is(err, ErrTokenCipherCorrupt) {
		t.Fatalf("want ErrTokenCipherCorrupt for short blob, got %v", err)
	}
}

func TestSealer_Open_UnknownVersion_FailsLoud(t *testing.T) {
	t.Parallel()
	sealer, _ := NewAESGCMSealer(fixedKEK(t))
	ct, err := sealer.Seal([]byte("data"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Corrupt the version header (first 4 bytes).
	bad := append([]byte(nil), ct...)
	bad[0] = 0xFF
	_, err = sealer.Open(bad)
	if !errors.Is(err, ErrTokenCipherCorrupt) {
		t.Fatalf("want ErrTokenCipherCorrupt for bad version, got %v", err)
	}
}
