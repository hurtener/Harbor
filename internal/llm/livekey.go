package llm

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync/atomic"
)

// Key-rotation sentinels. Callers compare via errors.Is.
var (
	// ErrEmptyKey — a rotate carried an empty key value. Fails closed
	// before any swap (a blank key would silently break every call).
	ErrEmptyKey = errors.New("llm: rotate key value is empty")
	// ErrUnknownProvider — a rotate named a provider other than the bound
	// one. Multi-provider rotation is post-V1; V1 rotates the primary.
	ErrUnknownProvider = errors.New("llm: rotate provider is not the configured provider")
)

// LiveKey is the atomically-swappable holder for a provider's primary API
// key. It is the ONE sanctioned mutable seam behind the bifrost
// `Account.GetKeysForProvider` read path (CLAUDE.md §5 carve-out: an
// atomic primitive documented "internally synchronised"). The value lives
// in an `atomic.Pointer[string]` so a rotate swaps it without a lock and
// without blocking in-flight reads — bifrost picks up the new key on the
// next call, the old key is never read again (the settled key-rotation
// mechanism).
//
// Concurrent reuse: safe for N concurrent Get (the read hot path) +
// Rotate (the admin write) goroutines. No torn reads — Load/Store of the
// pointer is atomic.
//
// The key VALUE is a secret: LiveKey never logs it and never renders it in
// String/Fingerprint. Fingerprint returns a non-reversible digest for
// audit correlation.
type LiveKey struct {
	ptr atomic.Pointer[string]
}

// NewLiveKey returns an empty holder. The bifrost account fills it with
// the resolved key at construction via Init; the rotate service swaps it
// via Rotate. An empty holder's Get returns "" (the safety edge / bifrost
// then fails closed on the empty key, never a silent bad call).
func NewLiveKey() *LiveKey { return &LiveKey{} }

// Init seeds the holder with the boot-resolved key. Called once by the
// driver at construction. Idempotent re-seeding is allowed (a reconstruct
// re-seeds), but Rotate is the post-boot swap path.
func (k *LiveKey) Init(v string) {
	val := v
	k.ptr.Store(&val)
}

// Get returns the current key value. The bifrost account's per-call
// GetKeysForProvider reads through here. Returns "" when never seeded.
func (k *LiveKey) Get() string {
	if p := k.ptr.Load(); p != nil {
		return *p
	}
	return ""
}

// Rotate atomically swaps the key to v. The next Get returns v; in-flight
// reads see either the old or the new value, never a torn one. Returns
// ErrEmptyKey for a blank value (fail closed before the swap).
func (k *LiveKey) Rotate(v string) error {
	if v == "" {
		return ErrEmptyKey
	}
	val := v
	k.ptr.Store(&val)
	return nil
}

// Fingerprint returns a non-reversible digest of the CURRENT key for audit
// correlation — `sha256:<first 12 hex>`. Never the key itself. An empty
// holder returns "sha256:" (no digest). Operators correlate rotations by
// fingerprint without the secret ever leaving the holder.
func (k *LiveKey) Fingerprint() string {
	return Fingerprint(k.Get())
}

// Fingerprint computes the non-reversible audit digest for a key value —
// `sha256:<first 12 hex of SHA-256(value)>`. An empty value yields the
// bare prefix. Used by the rotate path so the audit event + response carry
// a correlatable token, never the secret.
func Fingerprint(value string) string {
	if value == "" {
		return "sha256:"
	}
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])[:12]
}

// ProviderKeyRotator binds a LiveKey to its provider and is the concrete
// the governance rotate-key service drives. It validates the provider
// (V1: only the bound primary) and the non-empty key, swaps the holder,
// and returns the new key's fingerprint — never the key.
//
// Immutable after construction except the holder it points at (which is
// itself internally synchronised); safe for concurrent RotateKey calls.
type ProviderKeyRotator struct {
	provider string
	key      *LiveKey
}

// NewProviderKeyRotator binds the rotator to a provider + its live key.
func NewProviderKeyRotator(provider string, key *LiveKey) *ProviderKeyRotator {
	return &ProviderKeyRotator{provider: provider, key: key}
}

// RotateKey swaps the bound provider's key. An empty `provider` targets
// the bound provider (the common single-provider case); a non-empty
// `provider` that differs from the bound one is rejected with
// ErrUnknownProvider (multi-provider rotation is post-V1). An empty
// newKey is rejected with ErrEmptyKey before any swap. Returns the
// resolved provider name + the new key's fingerprint for the audit event
// and the response — never the key.
func (r *ProviderKeyRotator) RotateKey(provider, newKey string) (resolvedProvider, fingerprint string, err error) {
	if provider != "" && provider != r.provider {
		return "", "", ErrUnknownProvider
	}
	if err := r.key.Rotate(newKey); err != nil {
		return "", "", err
	}
	return r.provider, Fingerprint(newKey), nil
}

// Provider returns the bound provider name (so callers can echo it on the
// response without re-deriving it).
func (r *ProviderKeyRotator) Provider() string { return r.provider }
