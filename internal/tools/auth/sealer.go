package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// EnvelopeVersion is the on-disk version tag stamped on every
// encrypted token blob. Bumped only when the envelope format changes
// in a backwards-incompatible way; v1 = "[4-byte BE version][12-byte
// nonce][ciphertext+GCM tag]".
const EnvelopeVersion uint32 = 1

// KEKSizeBytes is the required length of the key-encryption key
// (AES-256). A wrong-length KEK fails the boot loud per CLAUDE.md §13
// amendment (PR #91 / D-082).
const KEKSizeBytes = 32

// nonceSize is GCM's standard nonce size in bytes.
const nonceSize = 12

// Sealer encrypts / decrypts token plaintext for at-rest storage.
// The envelope is "[4-byte BE version][12-byte fresh nonce][AES-GCM
// ciphertext+tag]" so KEK rotation (post-V1) can decrypt legacy
// records before re-encrypting under the new key.
//
// Concurrent reuse (D-025): The constructed Sealer is safe for N
// concurrent goroutines — cipher.AEAD's Seal / Open are
// concurrency-safe per crypto/cipher's documented contract, and the
// Sealer holds only the immutable AEAD reference after construction.
type Sealer interface {
	Seal(plaintext []byte) ([]byte, error)
	Open(ciphertext []byte) ([]byte, error)
}

// NewAESGCMSealer constructs a Sealer over AES-256-GCM keyed on kek.
// Returns wrapped ErrKEKMissing on wrong-length kek.
func NewAESGCMSealer(kek []byte) (Sealer, error) {
	if len(kek) != KEKSizeBytes {
		return nil, fmt.Errorf("%w: got %d bytes, want %d",
			ErrKEKMissing, len(kek), KEKSizeBytes)
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, fmt.Errorf("auth: NewCipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("auth: NewGCM: %w", err)
	}
	return &aesgcmSealer{aead: aead}, nil
}

type aesgcmSealer struct {
	aead cipher.AEAD
}

// Seal encrypts plaintext under the configured KEK. A fresh 12-byte
// nonce is generated per call via crypto/rand. Output shape is
// "[4-byte BE version][12-byte nonce][AES-GCM(plaintext)]".
func (s *aesgcmSealer) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("auth: nonce: %w", err)
	}
	// Total output length = version(4) + nonce(12) + ciphertext+tag.
	// Seal appends the ciphertext to its dst slice; we pre-allocate
	// the version+nonce prefix and let Seal grow as needed.
	out := make([]byte, 4+nonceSize, 4+nonceSize+len(plaintext)+s.aead.Overhead())
	binary.BigEndian.PutUint32(out[:4], EnvelopeVersion)
	copy(out[4:4+nonceSize], nonce)
	out = s.aead.Seal(out, nonce, plaintext, nil)
	return out, nil
}

// Open decrypts the envelope produced by Seal. Returns wrapped
// ErrTokenCipherCorrupt on length / version / GCM-authentication
// failures — the caller MUST NOT fall back to the input bytes.
func (s *aesgcmSealer) Open(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < 4+nonceSize+s.aead.Overhead() {
		return nil, fmt.Errorf("%w: blob too short (%d bytes)",
			ErrTokenCipherCorrupt, len(ciphertext))
	}
	version := binary.BigEndian.Uint32(ciphertext[:4])
	if version != EnvelopeVersion {
		return nil, fmt.Errorf("%w: unknown envelope version %d",
			ErrTokenCipherCorrupt, version)
	}
	nonce := ciphertext[4 : 4+nonceSize]
	payload := ciphertext[4+nonceSize:]
	plaintext, err := s.aead.Open(nil, nonce, payload, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: AEAD Open: %v", ErrTokenCipherCorrupt, err)
	}
	return plaintext, nil
}

// IsCipherCorrupt reports whether err wraps ErrTokenCipherCorrupt.
// Tiny convenience for callers comparing through errors.Is on the
// hot path.
func IsCipherCorrupt(err error) bool {
	return errors.Is(err, ErrTokenCipherCorrupt)
}
