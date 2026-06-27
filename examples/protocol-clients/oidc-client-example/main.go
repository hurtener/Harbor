// Command oidc-client-example is a worked Harbor Protocol client that
// obtains a JWT from an OAuth2 / OIDC provider via the
// client-credentials grant and attaches it to a production
// `harbor serve` instance. It is the runnable companion to the
// production-identity setup guide
// (docs/site/protocol/production-identity-setup.md): the guide explains
// the claim shape `harbor serve`'s verifier enforces; this program shows
// a real client walking the on-ramp end to end.
//
// Like its sibling event-viewer, it is deliberately SDK-free — pure
// stdlib against the Harbor Protocol wire. The point is that the
// Protocol IS the integration surface: a third-party client in any
// language obtains a token from its identity provider and attaches with
// nothing but HTTP and a JWT. No Harbor Go import is required, and the
// phase smoke compile-gates that (an `internal/...` or `sdk/...` import
// creeping in would break the SDK-free guarantee the guide promises).
//
// What it does, in order:
//
//  1. Runs the OAuth2 client-credentials grant against your identity
//     provider's token endpoint (client_secret_basic authentication, the
//     spec-recommended default) and receives a signed JWT access token.
//  2. Decodes the token's own `scopes` claim — without verifying it; the
//     RUNTIME verifies, the client merely reports what it was granted —
//     so the operator can see which entitlements the IdP issued.
//  3. Calls `runtime.info` against `harbor serve` with the token in the
//     `Authorization: Bearer` header. A 200 proves the runtime's JWKS
//     verifier accepted the IdP-signed token: the production identity
//     path is live. The runtime's build identity, Protocol version, and
//     advertised capabilities are printed, and the Protocol major version
//     is checked for compatibility.
//
// The (tenant, user, session) isolation triple travels INSIDE the JWT
// claims (mapped from your IdP per the setup guide), so no separate
// identity header is needed: the verified claims become the request's
// identity at the Protocol edge.
//
// Usage:
//
//	export OIDC_TOKEN_URL=https://your-idp.example.com/oauth/token
//	export OIDC_CLIENT_ID=harbor-client
//	export OIDC_CLIENT_SECRET=...           # keep this out of your shell history
//	export OIDC_SCOPE='console:fleet'        # optional, space-separated
//	export HARBOR_BASE_URL=https://harbor.example.com   # your `harbor serve`
//	go run ./examples/protocol-clients/oidc-client-example
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

func main() {
	// Identity-provider token endpoint + the client's own credentials.
	// In the client-credentials grant the CLIENT (this machine) is the
	// principal; the IdP maps the client onto the Harbor identity claims
	// (tenant/user/session/scopes) per the setup guide.
	tokenURL := mustEnv("OIDC_TOKEN_URL")
	clientID := mustEnv("OIDC_CLIENT_ID")
	clientSecret := mustEnv("OIDC_CLIENT_SECRET")
	scope := os.Getenv("OIDC_SCOPE") // optional

	baseURL := envOr("HARBOR_BASE_URL", "http://127.0.0.1:8686")

	// 1. The OAuth2 client-credentials grant. RFC 6749 §4.4: POST the
	// grant to the token endpoint; authenticate the client with HTTP
	// Basic (client_secret_basic) — the spec-recommended default that
	// keeps the secret out of the request body.
	token := obtainToken(tokenURL, clientID, clientSecret, scope)

	// 2. Report the scopes the IdP granted. We decode the token's own
	// payload WITHOUT verifying — the runtime verifies the signature;
	// the client just surfaces what it was issued so the operator can
	// confirm the IdP's claim mapping. Never trust these for an
	// authorization decision client-side.
	granted := decodeGrantedScopes(token)
	fmt.Printf("obtained access token from %s (granted scopes: %s)\n",
		tokenURL, formatScopes(granted))

	// 3. runtime.info — the attach handshake. A 200 here is the proof
	// the production identity path works end to end: `harbor serve`
	// fetched its IdP's JWK Set, resolved this token's `kid`, verified
	// the ES256/RS256 signature, and accepted the (tenant, user,
	// session) claim triple. An empty identity body is backfilled from
	// the verified JWT by the runtime.
	var info struct {
		InstanceID      string   `json:"instance_id"`
		BuildVersion    string   `json:"build_version"`
		ProtocolVersion string   `json:"protocol_version"`
		Capabilities    []string `json:"capabilities"`
	}
	mustPostJSON(baseURL+"/v1/control/runtime.info", token, []byte(`{"identity":{}}`), &info)

	const builtAgainstMajor = "0" // Protocol 0.x — same major ⇒ compatible
	if got := majorOf(info.ProtocolVersion); got != builtAgainstMajor {
		fail("protocol major version skew: runtime speaks %s, this client was built against %s.x",
			info.ProtocolVersion, builtAgainstMajor)
	}

	fmt.Printf("runtime %s (build %s) speaks Protocol %s; capabilities: %s\n",
		info.InstanceID, info.BuildVersion, info.ProtocolVersion,
		strings.Join(info.Capabilities, ", "))
	fmt.Println("attach OK — runtime.info accepted the OIDC-issued token")
}

// obtainToken runs the client-credentials grant and returns the raw
// access-token string. Fails loudly on any non-200 or a response missing
// the access_token — a broken token exchange is never silently tolerated.
func obtainToken(tokenURL, clientID, clientSecret, scope string) string {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	if scope != "" {
		form.Set("scope", scope)
	}

	req, err := http.NewRequest(http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		fail("build token request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// client_secret_basic: the credentials travel in the Authorization
	// header, base64(client_id:client_secret), never in the body.
	req.SetBasicAuth(clientID, clientSecret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fail("token endpoint %s: %v", tokenURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		fail("token endpoint %s: read response body: %v", tokenURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		// The IdP returns an OAuth2 error body on failure; surface it so
		// a misconfigured client_id / scope is diagnosable.
		fail("token endpoint %s: HTTP %d: %s", tokenURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		fail("token endpoint %s: response is not a valid token JSON: %v", tokenURL, err)
	}
	if tok.AccessToken == "" {
		fail("token endpoint %s: response carried no access_token", tokenURL)
	}
	return tok.AccessToken
}

// decodeGrantedScopes pulls the `scopes` claim out of the JWT payload
// without verifying the signature. This is a client-side convenience for
// reporting only — the runtime is the authority that verifies the token.
// A token whose payload cannot be decoded yields no scopes rather than a
// hard failure: the runtime.info call is the real gate.
func decodeGrantedScopes(token string) []string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims struct {
		Scopes []string `json:"scopes"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil
	}
	return claims.Scopes
}

// mustPostJSON issues a Bearer-authenticated Protocol POST and decodes the
// JSON response into out, failing loudly on any non-200.
func mustPostJSON(rawurl, token string, reqBody []byte, out any) {
	req, err := http.NewRequest(http.MethodPost, rawurl, bytes.NewReader(reqBody))
	if err != nil {
		fail("build request %s: %v", rawurl, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fail("%s: %v", rawurl, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) //nolint:errcheck // best-effort error-body read for diagnostics; the non-200 is already the failure
		fail("%s: HTTP %d: %s", rawurl, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		fail("%s: decode response: %v", rawurl, err)
	}
}

// majorOf returns the major component of a dotted version string.
func majorOf(version string) string {
	return strings.Split(version, ".")[0]
}

// formatScopes renders a scope list for human output, or "(none)" when
// the token carried no scopes.
func formatScopes(scopes []string) string {
	if len(scopes) == 0 {
		return "(none)"
	}
	return strings.Join(scopes, ", ")
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fail("missing required environment variable %s", key)
	}
	return v
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "oidc-client-example: "+format+"\n", args...)
	os.Exit(1)
}
