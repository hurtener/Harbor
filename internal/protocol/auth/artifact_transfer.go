package auth

import (
	"context"
	"errors"
	"strings"
)

// ArtifactTransferClaim is a signed, method-restricted browser transfer grant.
// A read names one immutable ref; a write names one exact destination namespace
// before put completes. Neither token can invoke another Protocol method.
const ArtifactTransferClaim = "artifact_transfer"

// ErrArtifactTransferMalformed rejects a present but incomplete or widened grant.
var ErrArtifactTransferMalformed = errors.New("auth: artifact transfer claim malformed")

// ArtifactTransferProof is verified bearer authority for exactly one transfer
// method and one owner session. The Protocol edge still checks body identity.
type ArtifactTransferProof struct {
	Mode     string
	ID       string
	MaxBytes int64
}

type artifactTransferKey struct{}

// WithArtifactTransfer seats a validated signed grant in request context.
func WithArtifactTransfer(ctx context.Context, proof ArtifactTransferProof) context.Context {
	if !validArtifactTransfer(proof) {
		return ctx
	}
	return context.WithValue(ctx, artifactTransferKey{}, proof)
}

// ArtifactTransferFrom returns only a verified, bounded transfer grant.
func ArtifactTransferFrom(ctx context.Context) (ArtifactTransferProof, bool) {
	p, ok := ctx.Value(artifactTransferKey{}).(ArtifactTransferProof)
	return p, ok && validArtifactTransfer(p)
}

func parseArtifactTransfer(raw any) (ArtifactTransferProof, error) {
	m, ok := raw.(map[string]any)
	if !ok || len(m) != 3 {
		return ArtifactTransferProof{}, ErrArtifactTransferMalformed
	}
	mode, mok := m["mode"].(string)
	id, iok := m["id"].(string)
	max, bok := m["max_bytes"].(float64)
	if !mok || !iok || !bok || max != float64(int64(max)) {
		return ArtifactTransferProof{}, ErrArtifactTransferMalformed
	}
	p := ArtifactTransferProof{Mode: mode, ID: id, MaxBytes: int64(max)}
	if !validArtifactTransfer(p) {
		return ArtifactTransferProof{}, ErrArtifactTransferMalformed
	}
	return p, nil
}

func validArtifactTransfer(p ArtifactTransferProof) bool {
	if p.MaxBytes <= 0 || p.MaxBytes > 64<<20 {
		return false
	}
	switch p.Mode {
	case "read":
		return len(p.ID) > 0 && len(p.ID) <= 128 && !strings.ContainsAny(p.ID, "* /\\\t\r\n")
	case "write":
		if len(p.ID) == 0 || len(p.ID) > 128 {
			return false
		}
		for _, c := range p.ID {
			switch {
			case c >= '0' && c <= '9', c >= 'A' && c <= 'Z',
				c >= 'a' && c <= 'z', c == '_', c == '-':
			default:
				return false
			}
		}
		return true
	default:
		return false
	}
}
