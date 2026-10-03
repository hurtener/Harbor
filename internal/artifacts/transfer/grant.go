// Package transfer implements exact, recipient-admitted runtime artifact copies.
// The coordinator handles signed references only; the source runtime sends the
// bytes directly to a boot-trusted peer after both owners admit the operation.
package transfer

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

var (
	// ErrInvalid identifies an invalid signed envelope or request.
	ErrInvalid = errors.New("artifact transfer: invalid request")
	// ErrUnauthorized identifies a missing owner, admission, peer, or signature.
	ErrUnauthorized = errors.New("artifact transfer: not authorized")
	// ErrConflict identifies reuse of an operation ID with different authority.
	ErrConflict = errors.New("artifact transfer: idempotency conflict")
	// ErrExpired rejects new work outside the signed validity interval.
	ErrExpired = errors.New("artifact transfer: expired")
	// ErrRevoked identifies an owner-revoked operation.
	ErrRevoked = errors.New("artifact transfer: revoked")
	// ErrTooLate means dispatch already began; revocation cannot roll it back.
	ErrTooLate = errors.New("artifact transfer: already dispatched")
	// ErrNotFound identifies an unavailable artifact or receipt.
	ErrNotFound = errors.New("artifact transfer: not found")
)

// Sign signs a fully specified transfer with an operator-owned Ed25519 key.
// No key generation or default authority is hidden in this helper.
func Sign(g types.ArtifactTransferGrant, key ed25519.PrivateKey) (types.ArtifactTransferGrant, error) {
	if len(key) != ed25519.PrivateKeySize {
		return g, ErrInvalid
	}
	g.Signature = ""
	if err := validateGrant(g); err != nil {
		return g, err
	}
	b, err := json.Marshal(g)
	if err != nil {
		return g, fmt.Errorf("encode transfer grant: %w", err)
	}
	g.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, b))
	return g, nil
}

func validateGrant(g types.ArtifactTransferGrant) error {
	for _, v := range []string{g.KeyID, g.TransferID, g.Purpose, g.ArtifactID, g.Source.Audience, g.Source.Tenant, g.Source.User, g.Source.Session, g.Destination.Audience, g.Destination.Tenant, g.Destination.User, g.Destination.Session} {
		if strings.TrimSpace(v) != v || v == "" || len(v) > 255 || strings.ContainsAny(v, "\x00\r\n") {
			return ErrInvalid
		}
	}
	if g.Version != 1 || g.Source.Epoch == 0 || g.Destination.Epoch == 0 || g.SizeBytes < 0 || len(g.SHA256) != 64 || strings.ToLower(g.SHA256) != g.SHA256 {
		return ErrInvalid
	}
	if _, err := hex.DecodeString(g.SHA256); err != nil {
		return ErrInvalid
	}
	if len(g.MimeType) > 255 {
		return ErrInvalid
	}
	if _, _, err := mime.ParseMediaType(g.MimeType); err != nil {
		return ErrInvalid
	}
	if g.IssuedAt.IsZero() || !g.ExpiresAt.After(g.IssuedAt) || g.ExpiresAt.Sub(g.IssuedAt) > 15*time.Minute {
		return ErrInvalid
	}
	return nil
}

func grantHash(g types.ArtifactTransferGrant) string {
	b, err := json.Marshal(g)
	if err != nil {
		return ""
	} // Invalid time encodings are refused by verify before any use.
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func verify(g types.ArtifactTransferGrant, keys map[string]ed25519.PublicKey) error {
	if err := validateGrant(g); err != nil {
		return err
	}
	key := keys[g.KeyID]
	if len(key) != ed25519.PublicKeySize {
		return ErrUnauthorized
	}
	sig, err := base64.RawURLEncoding.DecodeString(g.Signature)
	if err != nil {
		return ErrUnauthorized
	}
	g.Signature = ""
	b, err := json.Marshal(g)
	if err != nil {
		return fmt.Errorf("encode transfer signature: %w", err)
	}
	if !ed25519.Verify(key, b, sig) {
		return ErrUnauthorized
	}
	return nil
}
