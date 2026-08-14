package admission

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// Claims is the sealed render-admission claim set. It binds EXACTLY the
// one render tuple — the identity triple, the effective agent, the host
// server, the `ui://` resource URI, and the registry descriptor
// generation — plus the claim-family schema and version, the
// issued/expiry instants (Unix seconds), and the crypto-random 128-bit
// nonce. Nothing else may appear: no tool arguments, provider output,
// tool-context content, callback names, secrets, keys, or any authority
// beyond that single tuple.
type Claims struct {
	Schema    string `json:"schema"`
	Version   int    `json:"version"`
	TenantID  string `json:"tenant_id"`
	UserID    string `json:"user_id"`
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id"`
	ServerID  string `json:"server_id"`
	// ResourceURI is the exact, case-sensitive `ui://` resource URI of
	// the MCP App document being admitted.
	ResourceURI string `json:"resource_uri"`
	// DescriptorGeneration is the registry descriptor generation
	// fingerprint (the provider/catalog generation) current at mint
	// time. Verify requires the CURRENT fingerprint, so a stale
	// admission fails closed against a replaced descriptor.
	DescriptorGeneration string `json:"descriptor_generation"`
	IssuedAt             int64  `json:"issued_at"`  // Unix seconds
	ExpiresAt            int64  `json:"expires_at"` // Unix seconds
	// Nonce is the base64url (RawURLEncoding) rendering of the
	// crypto-random 128-bit claim nonce.
	Nonce string `json:"nonce"`
}

// IssuedAtTime returns the issued instant as a time.Time (UTC).
func (c Claims) IssuedAtTime() time.Time {
	return time.Unix(c.IssuedAt, 0).UTC()
}

// ExpiresAtTime returns the expiry instant as a time.Time (UTC).
func (c Claims) ExpiresAtTime() time.Time {
	return time.Unix(c.ExpiresAt, 0).UTC()
}

// canonicalJSON encodes v to strict canonical JSON: compact, no HTML
// escaping, no trailing newline, deterministic field order inherited
// from the struct layout. Two encodes of the same value yield identical
// bytes; a sealed admission is always canonical, and verify requires it.
func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// utf8ValidBytes reports whether b is entirely valid UTF-8. It is
// checked on the raw claims plaintext BEFORE decode: Go's
// encoding/json silently coerces invalid UTF-8 inside strings to U+FFFD
// instead of erroring, so the raw scan is the only place a
// non-UTF-8 sealed claim can be caught without silent value mutation.
func utf8ValidBytes(b []byte) bool {
	return utf8.Valid(b)
}

// strictDecode decodes b into v with DisallowUnknownFields and an
// explicit EOF requirement: any content after the single JSON value is
// a structural error, because a canonical claim never carries trailing
// bytes.
func strictDecode(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing content after claims JSON")
		}
		return err
	}
	return nil
}

// validateBoundString applies the shared string-claim bounds: nonempty
// (cardinality), valid UTF-8, no NUL byte, byte ceiling, rune ceiling.
// The same function guards mint inputs and sealed claims, so the two
// edges can never accept different shapes.
func validateBoundString(name, s string) error {
	if s == "" {
		return fmt.Errorf("field %s: empty", name)
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("field %s: not valid UTF-8", name)
	}
	if strings.IndexByte(s, 0) >= 0 {
		return fmt.Errorf("field %s: contains a NUL byte", name)
	}
	if len(s) > MaxClaimStringBytes {
		return fmt.Errorf("field %s: %d bytes, max %d", name, len(s), MaxClaimStringBytes)
	}
	if runes := utf8.RuneCountInString(s); runes > MaxClaimStringRunes {
		return fmt.Errorf("field %s: %d runes, max %d", name, runes, MaxClaimStringRunes)
	}
	return nil
}

// validateClaimsStructure validates the SHAPE of a decoded claim set:
// schema and version, every string field's bounds, the exact 128-bit
// nonce cardinality, non-zero time instants, a positive bounded
// lifetime, and no future issuance beyond clock skew. Every violation
// returns an error wrapping ErrTokenInvalid; a malformed claim set is
// never admitted regardless of how well-formed the tuple match would
// be.
func validateClaimsStructure(c *Claims, now time.Time) error {
	if c.Schema != Schema {
		return fmt.Errorf("%w: unknown claim schema %q", ErrTokenInvalid, c.Schema)
	}
	if c.Version != SchemaVersion {
		return fmt.Errorf("%w: unknown claim schema version %d", ErrTokenInvalid, c.Version)
	}
	fields := []struct {
		name string
		val  string
	}{
		{"tenant_id", c.TenantID},
		{"user_id", c.UserID},
		{"session_id", c.SessionID},
		{"agent_id", c.AgentID},
		{"server_id", c.ServerID},
		{"resource_uri", c.ResourceURI},
		{"descriptor_generation", c.DescriptorGeneration},
	}
	for _, f := range fields {
		if err := validateBoundString(f.name, f.val); err != nil {
			return fmt.Errorf("%w: %w", ErrTokenInvalid, err)
		}
	}
	if !strings.HasPrefix(c.ResourceURI, resourceScheme) {
		return fmt.Errorf("%w: resource_uri %q does not carry the ui:// scheme", ErrTokenInvalid, c.ResourceURI)
	}
	nonce, err := base64.RawURLEncoding.DecodeString(c.Nonce)
	if err != nil {
		return fmt.Errorf("%w: claim nonce is not valid base64url: %w", ErrTokenInvalid, err)
	}
	if len(nonce) != NonceSize {
		return fmt.Errorf("%w: claim nonce is %d bytes, want %d", ErrTokenInvalid, len(nonce), NonceSize)
	}
	if c.IssuedAt <= 0 || c.ExpiresAt <= 0 {
		return fmt.Errorf("%w: issued_at/expires_at missing or non-positive", ErrTokenInvalid)
	}
	lifetime := c.ExpiresAt - c.IssuedAt
	if lifetime <= 0 {
		return fmt.Errorf("%w: claim lifetime %ds is non-positive", ErrTokenInvalid, lifetime)
	}
	if lifetime > int64(MaxTTL/time.Second) {
		return fmt.Errorf("%w: claim lifetime %ds exceeds the max %s", ErrTokenInvalid, lifetime, MaxTTL)
	}
	if now.Unix() < c.IssuedAt-int64(MaxClockSkew/time.Second) {
		return fmt.Errorf("%w: claim issued_at %d is in the future beyond clock skew", ErrTokenInvalid, c.IssuedAt)
	}
	return nil
}

// checkClaimsExpiry applies the expiry half of the bounded clock skew:
// an admission whose expiry is beyond now + MaxClockSkew is expired.
// Kept separate from the structural validation so a well-formed but
// stale admission classifies as ErrTokenExpired rather than
// ErrTokenInvalid.
func checkClaimsExpiry(c *Claims, now time.Time) error {
	if now.Unix() > c.ExpiresAt+int64(MaxClockSkew/time.Second) {
		return fmt.Errorf("%w: admission expired at %s",
			ErrTokenExpired, c.ExpiresAtTime().Format(time.RFC3339))
	}
	return nil
}
