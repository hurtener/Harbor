// cmd/harbor/token_crypto.go — the operator-managed signing key surface
// behind `harbor token`.
//
// Unlike the in-memory ephemeral dev signer (devauth.go), the
// `harbor token` signer is PERSISTABLE: an operator generates an
// asymmetric keypair once, keeps the private key, points
// `harbor serve`'s `identity.jwks_file` at the emitted public JWK Set,
// and mints tokens against it for as long as they control the key. The
// `harbor serve` verifier is unchanged — it trusts the key for exactly
// one reason: the operator configured `jwks_file` to point at it,
// identical to pointing at an external IdP's published keys.
//
// This file owns the three crypto pieces the dev signer never needed:
//
//   - keypair generation for both an ECDSA (ES256) and an RSA (RS256)
//     branch — both on the verifier's asymmetric algorithm allowlist;
//   - PEM-encoded PKCS#8 private-key write (mode 0600) and read — a
//     deliberate departure from the dev signer's "key never touches
//     disk" posture;
//   - a hand-written RFC 7517 JWK Set emitter for the public half, whose
//     `kid` is the RFC 7638 JWK thumbprint (a stable, content-derived
//     identifier, never a hardcoded constant). The verifier's JWK
//     consumer parses `n`/`e` (RSA) and `crv`/`x`/`y` (EC) as base64url,
//     so the emitter matches that encoding exactly.
//
// The private key is never logged and never printed; only its file path
// is surfaced. The minted token is written to stdout (it is the
// command's product), the private key never is.

package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"

	"github.com/golang-jwt/jwt/v5"
)

// algES256 / algRS256 are the two algorithms `harbor token keygen`
// generates. ES256 (ECDSA P-256) is the default — keypair generation is
// ~10x faster than equivalent-strength RSA. RS256 (RSA-2048) is the
// opt-in. Both sit on the Protocol verifier's asymmetric allowlist.
const (
	algES256 = "ES256"
	algRS256 = "RS256"
)

// tokenRSABits is the RSA modulus size for the RS256 branch. 2048 is the
// modern floor the verifier's JWK consumer enforces; a smaller modulus
// would be rejected at load.
const tokenRSABits = 2048

// Filesystem permissions for the emitted material. The private key is
// owner-read/write only; its parent directory is owner-only. The public
// JWK Set is world-readable (it is published to the verifier).
const (
	privatePEMMode os.FileMode = 0o600
	keyDirMode     os.FileMode = 0o700
	jwksFileMode   os.FileMode = 0o644
)

// Sentinel errors. Callers compare via errors.Is to map onto a CLIError.
var (
	// ErrTokenKeyExists — keygen refused to overwrite an existing
	// private key (the operator must pass --force or pick a fresh
	// directory). Overwriting a signing key silently would orphan every
	// token minted against the old key.
	ErrTokenKeyExists = errors.New("token: private key file already exists")
	// ErrTokenUnknownAlg — an unsupported algorithm was requested for
	// keygen, or a loaded key resolved to an algorithm outside the
	// supported set.
	ErrTokenUnknownAlg = errors.New("token: unsupported algorithm")
	// ErrTokenKeyParse — a private key file could not be decoded as a
	// PEM-wrapped PKCS#8 / SEC1 / PKCS#1 asymmetric private key.
	ErrTokenKeyParse = errors.New("token: private key could not be parsed")
)

// generateSigningKey generates a fresh asymmetric keypair for alg. The
// returned crypto.Signer is either an *ecdsa.PrivateKey (ES256) or an
// *rsa.PrivateKey (RS256).
func generateSigningKey(alg string) (crypto.Signer, error) {
	switch alg {
	case algES256:
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("token: generate ES256 key: %w", err)
		}
		return key, nil
	case algRS256:
		key, err := rsa.GenerateKey(rand.Reader, tokenRSABits)
		if err != nil {
			return nil, fmt.Errorf("token: generate RS256 key: %w", err)
		}
		return key, nil
	default:
		return nil, fmt.Errorf("%w: %q (supported: %s, %s)", ErrTokenUnknownAlg, alg, algES256, algRS256)
	}
}

// algForKey returns the JWT algorithm name for a loaded signing key. The
// EC branch maps the curve to its canonical ECDSA algorithm; the RSA
// branch is always RS256 (the algorithm keygen stamps).
func algForKey(signer crypto.Signer) (string, error) {
	switch k := signer.(type) {
	case *ecdsa.PrivateKey:
		switch k.Curve {
		case elliptic.P256():
			return "ES256", nil
		case elliptic.P384():
			return "ES384", nil
		case elliptic.P521():
			return "ES512", nil
		default:
			return "", fmt.Errorf("%w: unsupported EC curve", ErrTokenUnknownAlg)
		}
	case *rsa.PrivateKey:
		return "RS256", nil
	default:
		return "", fmt.Errorf("%w: unsupported key type %T", ErrTokenUnknownAlg, signer)
	}
}

// writePrivatePEM marshals signer to PKCS#8, PEM-wraps it, and writes it
// to path with mode 0600. When force is false it refuses to overwrite an
// existing file (ErrTokenKeyExists). When force is true and the file
// pre-exists, an explicit Chmod re-asserts 0600 (the OpenFile mode
// argument applies only on creation).
func writePrivatePEM(path string, signer crypto.Signer, force bool) error {
	der, err := x509.MarshalPKCS8PrivateKey(signer)
	if err != nil {
		return fmt.Errorf("token: marshal private key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	// Open atomically. Without --force, O_EXCL makes the kernel refuse an
	// existing file, closing the Stat-then-Open TOCTOU window (a file
	// appearing between a Stat and the Open would otherwise be silently
	// truncated, orphaning every token minted against the old key). With
	// --force, O_TRUNC overwrites. The mode argument applies only on create.
	flags := os.O_CREATE | os.O_WRONLY
	if force {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_EXCL
	}
	f, err := os.OpenFile(path, flags, privatePEMMode)
	if err != nil {
		if !force && errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: %s", ErrTokenKeyExists, path)
		}
		return fmt.Errorf("token: open %s: %w", path, err)
	}
	if _, err := f.Write(pemBytes); err != nil {
		_ = f.Close()
		return fmt.Errorf("token: write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("token: close %s: %w", path, err)
	}
	// Re-assert 0600 — a --force overwrite of a pre-existing file keeps
	// the old mode bits (OpenFile's perm applies only on create).
	if err := os.Chmod(path, privatePEMMode); err != nil {
		return fmt.Errorf("token: chmod %s: %w", path, err)
	}
	return nil
}

// loadPrivatePEM reads a PEM-wrapped private key from path and returns it
// as a crypto.Signer. It accepts PKCS#8 (what keygen writes) and falls
// back to SEC1 EC and PKCS#1 RSA encodings so an operator can supply a
// key produced by other tooling.
func loadPrivatePEM(path string) (crypto.Signer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("token: read %s: %w", path, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%w: %s is not PEM-encoded", ErrTokenKeyParse, path)
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		signer, ok := key.(crypto.Signer)
		if !ok {
			return nil, fmt.Errorf("%w: %s holds a non-signing key", ErrTokenKeyParse, path)
		}
		return signer, nil
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("%w: %s is not a PKCS#8 / SEC1 / PKCS#1 asymmetric private key", ErrTokenKeyParse, path)
}

// ecThumbprint / rsaThumbprint are the RFC 7638 canonical JWK members in
// the exact lexicographic order the thumbprint hash requires (no
// whitespace, required members only). json.Marshal of a struct keeps
// field order and emits no whitespace, so marshalling these yields the
// canonical form directly.
type ecThumbprint struct {
	Crv string `json:"crv"`
	Kty string `json:"kty"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

type rsaThumbprint struct {
	E   string `json:"e"`
	Kty string `json:"kty"`
	N   string `json:"n"`
}

// publicJWK builds the RFC 7517 public JWK for signer (the full member
// set written to jwks.json) plus the RFC 7638 thumbprint that serves as
// its `kid`. The `n`/`e` (RSA) and `crv`/`x`/`y` (EC) members are
// base64url-encoded to match the verifier's JWK consumer exactly; EC
// coordinates are left-padded to the curve's field size so the
// thumbprint is canonical.
func publicJWK(signer crypto.Signer, alg string) (map[string]string, string, error) {
	switch pub := signer.Public().(type) {
	case *ecdsa.PublicKey:
		crv, err := curveName(pub.Curve)
		if err != nil {
			return nil, "", err
		}
		// Extract the affine coordinates via the ecdh conversion rather
		// than the deprecated pub.X / pub.Y fields. ecdh's Bytes() returns
		// the uncompressed SEC1 point (0x04 || X || Y), each coordinate
		// left-padded to the curve's field size — exactly the fixed-length
		// octets RFC 7638 requires for a canonical thumbprint.
		ecdhPub, err := pub.ECDH()
		if err != nil {
			return nil, "", fmt.Errorf("token: convert EC public key: %w", err)
		}
		point := ecdhPub.Bytes()
		byteLen := (pub.Curve.Params().BitSize + 7) / 8
		if len(point) != 1+2*byteLen {
			return nil, "", fmt.Errorf("token: unexpected EC point length %d", len(point))
		}
		x := base64.RawURLEncoding.EncodeToString(point[1 : 1+byteLen])
		y := base64.RawURLEncoding.EncodeToString(point[1+byteLen:])
		kid, err := jwkThumbprint(ecThumbprint{Crv: crv, Kty: "EC", X: x, Y: y})
		if err != nil {
			return nil, "", err
		}
		return map[string]string{
			"kty": "EC",
			"crv": crv,
			"x":   x,
			"y":   y,
			"use": "sig",
			"alg": alg,
			"kid": kid,
		}, kid, nil
	case *rsa.PublicKey:
		n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
		kid, err := jwkThumbprint(rsaThumbprint{E: e, Kty: "RSA", N: n})
		if err != nil {
			return nil, "", err
		}
		return map[string]string{
			"kty": "RSA",
			"n":   n,
			"e":   e,
			"use": "sig",
			"alg": alg,
			"kid": kid,
		}, kid, nil
	default:
		return nil, "", fmt.Errorf("%w: unsupported public key type %T", ErrTokenUnknownAlg, pub)
	}
}

// curveName maps an elliptic.Curve to its RFC 7518 `crv` JWK value.
func curveName(curve elliptic.Curve) (string, error) {
	switch curve {
	case elliptic.P256():
		return "P-256", nil
	case elliptic.P384():
		return "P-384", nil
	case elliptic.P521():
		return "P-521", nil
	default:
		return "", fmt.Errorf("%w: unsupported EC curve", ErrTokenUnknownAlg)
	}
}

// jwkThumbprint computes the RFC 7638 thumbprint of a canonical JWK
// member struct: SHA-256 over the no-whitespace, lexicographically
// ordered JSON, base64url-encoded without padding.
func jwkThumbprint(canonical any) (string, error) {
	buf, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("token: marshal thumbprint members: %w", err)
	}
	sum := sha256.Sum256(buf)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// writeJWKSet writes a single-key RFC 7517 JWK Set to path (mode 0644).
// The document is the public material the verifier loads via
// `identity.jwks_file`.
func writeJWKSet(path string, jwk map[string]string) error {
	set := map[string]any{"keys": []map[string]string{jwk}}
	buf, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return fmt.Errorf("token: marshal JWK Set: %w", err)
	}
	buf = append(buf, '\n')
	if err := os.WriteFile(path, buf, jwksFileMode); err != nil {
		return fmt.Errorf("token: write %s: %w", path, err)
	}
	return nil
}

// signMethodForAlg maps a JWT algorithm name to its golang-jwt signing
// method. Only the asymmetric methods keygen / loaded keys can produce
// are supported.
func signMethodForAlg(alg string) (jwt.SigningMethod, error) {
	switch alg {
	case "ES256":
		return jwt.SigningMethodES256, nil
	case "ES384":
		return jwt.SigningMethodES384, nil
	case "ES512":
		return jwt.SigningMethodES512, nil
	case "RS256":
		return jwt.SigningMethodRS256, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrTokenUnknownAlg, alg)
	}
}

// mintJWT signs claims with signer under alg, stamping kid into the
// header so the verifier resolves the matching public key from the JWK
// Set.
func mintJWT(signer crypto.Signer, alg, kid string, claims jwt.MapClaims) (string, error) {
	method, err := signMethodForAlg(alg)
	if err != nil {
		return "", err
	}
	tok := jwt.NewWithClaims(method, claims)
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(signer)
	if err != nil {
		return "", fmt.Errorf("token: sign: %w", err)
	}
	return signed, nil
}
