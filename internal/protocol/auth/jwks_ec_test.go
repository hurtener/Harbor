package auth

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func TestParseECJWK_ValidatedPointEncoding(t *testing.T) {
	for _, tc := range []struct {
		name, alg string
		curve     elliptic.Curve
	}{
		{"P-256", "ES256", elliptic.P256()},
		{"P-384", "ES384", elliptic.P384()},
		{"P-521", "ES512", elliptic.P521()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			private, err := ecdsa.GenerateKey(tc.curve, rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			point, err := private.PublicKey.Bytes()
			if err != nil {
				t.Fatal(err)
			}
			size := (len(point) - 1) / 2
			for _, padded := range []bool{false, true} {
				x, y := point[1:1+size], point[1+size:]
				if padded {
					x = append([]byte{0}, x...)
					y = append([]byte{0}, y...)
				}
				key, alg, err := parseECJWK(jwk{Crv: tc.name, X: base64.RawURLEncoding.EncodeToString(x), Y: base64.RawURLEncoding.EncodeToString(y)})
				if err != nil || alg != tc.alg {
					t.Fatalf("padded=%v alg=%s err=%v", padded, alg, err)
				}
				got, err := key.Bytes()
				if err != nil || !bytes.Equal(got, point) {
					t.Fatalf("key identity changed: %v", err)
				}
			}
			for _, coordinates := range [][]byte{{0}, bytes.Repeat([]byte{0xff}, size+1)} {
				value := base64.RawURLEncoding.EncodeToString(coordinates)
				if _, _, err := parseECJWK(jwk{Crv: tc.name, X: value, Y: value}); err == nil {
					t.Fatal("accepted an invalid or oversized point")
				}
			}
		})
	}
}
