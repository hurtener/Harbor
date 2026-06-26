// jwks.go — the production JWKS-backed KeySet driver.
//
// The static KeySet in auth.go suffices for the dev-token path; a real
// deployment fronts an external OIDC provider whose signing keys are
// published as a JWK Set (RFC 7517) at a `.well-known/jwks.json`
// endpoint (or a local file). This file implements the JWKS-backed
// KeySet that resolves a JWT `kid` header against that published set,
// behind the SAME auth.KeySet interface NewValidator already consumes —
// additive, no reshape of the validation surface.
//
// # Sources
//
// A JWKSKeySet loads its set from exactly one of two sources:
//
//   - URL: fetched over HTTP with a bounded client timeout and a
//     bounded response-size limit (a hostile or misbehaving IdP cannot
//     stream an unbounded body).
//   - File: read from a local path (an operator who syncs the keyset
//     out-of-band, or an air-gapped deployment).
//
// The set is parsed with the standard library only — no third-party
// JWKS dependency. RSA keys (`kty:"RSA"`, base64url `n`/`e`) and ECDSA
// keys (`kty:"EC"`, `crv` P-256/384/521, base64url `x`/`y`) are
// supported; symmetric (`kty:"oct"`) and any other material is
// rejected, because Harbor's algorithm allowlist is asymmetric-only.
//
// # Cache, refresh, and refresh bounding
//
// The parsed set is cached. A `KeyByID` lookup serves from cache while
// the cache is within its TTL. Two conditions trigger a re-fetch: the
// TTL elapsing, and a `kid` miss (the IdP may have rotated in a new
// signing key). The on-miss re-fetch is RATE-LIMITED by a minimum
// refresh interval: a flapping IdP, or an attacker spraying unknown
// `kid` values, cannot drive an unbounded fetch storm — at most one
// network fetch per minimum-interval window. A miss that arrives inside
// the window resolves against the current cache and returns
// ErrUnknownKey without touching the network.
//
// # Fail-loud construction
//
// The constructor performs the initial fetch+parse synchronously and
// returns the error if it fails: a Runtime serving the Protocol edge
// must not boot with an unverifiable identity surface. A source that
// yields zero usable asymmetric signing keys is a construction error.
//
// # Concurrent reuse
//
// A JWKSKeySet is a compiled artifact. Its source, allowlist, clock,
// TTL, and bounding interval are set once at construction and never
// mutated. The only mutable state is the key-snapshot cache and the
// refresh bookkeeping, both internally synchronized (an RWMutex over
// the snapshot, a serialize-refreshes mutex with single-flight). No
// background goroutine is started — refresh is on-demand under the
// caller's ctx — so there is nothing to leak. One JWKSKeySet is safe to
// share across N concurrent KeyByID goroutines.
package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"sync"
	"time"
)

// Default cache + refresh-bounding knobs. A KeyByID that finds a fresh,
// matching key never touches the network; the bound caps how often a
// `kid` miss (or TTL expiry) can drive a fetch.
const (
	// defaultJWKSTTL is how long a fetched set is served from cache
	// before a TTL-driven refresh on the next lookup.
	defaultJWKSTTL = 5 * time.Minute
	// defaultJWKSMinRefreshInterval is the floor between two network
	// fetches. A `kid` miss inside this window resolves against the
	// current cache (and may return ErrUnknownKey) without re-fetching.
	defaultJWKSMinRefreshInterval = 1 * time.Minute
	// defaultJWKSHTTPTimeout bounds a single keyset fetch.
	defaultJWKSHTTPTimeout = 10 * time.Second
	// defaultJWKSMaxStale is the safe default max-stale ceiling: the
	// longest a cached key snapshot is honored without a successful
	// refresh before KeyByID fails closed. ~12× the refresh TTL — tight
	// enough to bound a revoked key to roughly this window of continued
	// acceptance, loose enough to ride out a transient IdP outage without
	// auth flapping. The single source for the default; referenced
	// everywhere the ceiling defaults.
	defaultJWKSMaxStale = 1 * time.Hour
	// maxJWKSBytes caps the response body a URL source will read — a
	// misbehaving IdP cannot stream an unbounded keyset into memory.
	maxJWKSBytes = 1 << 20 // 1 MiB
	// minRSAModulusBits is the floor an RSA public key must meet to be
	// accepted. A key below this is treated as malformed (defense in
	// depth against a weak/compromised IdP key enabling forgery).
	minRSAModulusBits = 2048
)

// ErrJWKSSource — the JWKS source configuration is invalid: neither a
// URL nor a file was supplied, or both were. Fail closed at
// construction rather than guess which the operator meant.
var ErrJWKSSource = errors.New("auth: JWKS source misconfigured (set exactly one of URL or File)")

// ErrJWKSNoUsableKeys — the fetched/parsed set contained no usable
// asymmetric signing key. A keyset with only symmetric / encryption /
// malformed entries cannot verify any Harbor token; fail closed.
var ErrJWKSNoUsableKeys = errors.New("auth: JWKS contained no usable asymmetric signing keys")

// ErrJWKSStale — the cached JWKS key snapshot exceeded the configured
// max-stale ceiling without a successful refresh: the runtime can no
// longer vouch for the cached keys (a key the IdP has revoked may still
// be present in the stale snapshot), so KeyByID fails CLOSED rather than
// serving a possibly-revoked key. Distinct from ErrUnknownKey (the kid
// never resolved). The Validator surfaces it onto the "jwks_stale" wire
// reason; the middleware maps it onto the existing auth-rejected
// Protocol code (HTTP 401). The ceiling BOUNDS — it does not make
// instantaneous — revocation: a revoked key may still be honored until
// the snapshot ages past the ceiling. Pair a tight ceiling with
// overlapping IdP signing keys and short token TTLs.
var ErrJWKSStale = errors.New("auth: JWKS key set too stale (max-stale ceiling exceeded); failing closed")

// JWKSSource selects where a JWKSKeySet loads its JWK Set. Exactly one
// of URL or File must be set.
type JWKSSource struct {
	// URL is an `https://…/.well-known/jwks.json`-shaped endpoint the
	// keyset is fetched from over HTTP.
	URL string
	// File is a local filesystem path to a JWK Set JSON document.
	File string
}

// JWKSOption configures NewJWKSKeySet. The defaults are production-safe;
// the options exist mainly so tests can drive the clock, inject a
// counting HTTP client, and shrink the timing windows deterministically.
type JWKSOption func(*jwksConfig)

type jwksConfig struct {
	ttl        time.Duration
	minRefresh time.Duration
	maxStale   time.Duration
	now        func() time.Time
	httpClient *http.Client
	logger     *slog.Logger
}

// WithJWKSRefreshTTL overrides how long a fetched set is served from
// cache before a TTL-driven refresh. A non-positive value keeps the
// default.
func WithJWKSRefreshTTL(d time.Duration) JWKSOption {
	return func(c *jwksConfig) {
		if d > 0 {
			c.ttl = d
		}
	}
}

// WithJWKSMinRefreshInterval overrides the floor between two network
// fetches (the on-miss refresh bound). A non-positive value keeps the
// default.
func WithJWKSMinRefreshInterval(d time.Duration) JWKSOption {
	return func(c *jwksConfig) {
		if d > 0 {
			c.minRefresh = d
		}
	}
}

// WithJWKSMaxStale sets the maximum age a cached key snapshot may reach,
// without a successful refresh, before KeyByID fails closed with
// ErrJWKSStale. A non-positive value keeps the package default
// (defaultJWKSMaxStale) — the ceiling is never disabled (Harbor's
// posture is fail-closed; there is no "unbounded" path). The ceiling
// BOUNDS — but does not make instantaneous — revocation: a revoked key
// may still be honored until the snapshot ages past the ceiling. Pair a
// tight ceiling with overlapping IdP signing keys and short token TTLs.
func WithJWKSMaxStale(d time.Duration) JWKSOption {
	return func(c *jwksConfig) {
		if d > 0 {
			c.maxStale = d
		}
	}
}

// WithJWKSClock overrides the keyset's clock — used by tests to drive
// TTL expiry and refresh bounding deterministically. A nil clock keeps
// the default (time.Now).
func WithJWKSClock(now func() time.Time) JWKSOption {
	return func(c *jwksConfig) {
		if now != nil {
			c.now = now
		}
	}
}

// WithJWKSHTTPClient overrides the HTTP client a URL source fetches
// with. Tests inject a client whose Transport counts requests and
// serves a fixture; a nil client keeps the default (a fresh client with
// a bounded timeout). Ignored by a File source.
func WithJWKSHTTPClient(client *http.Client) JWKSOption {
	return func(c *jwksConfig) {
		if client != nil {
			c.httpClient = client
		}
	}
}

// WithJWKSLogger sets the slog.Logger the keyset emits refresh
// diagnostics to (a dropped unusable key, a refresh failure that fell
// back to the stale cache). A nil logger keeps slog.Default().
func WithJWKSLogger(l *slog.Logger) JWKSOption {
	return func(c *jwksConfig) {
		if l != nil {
			c.logger = l
		}
	}
}

// jwksFetcher returns the raw JWK Set bytes for a source. URL and File
// sources share the same downstream parse path.
type jwksFetcher func(ctx context.Context) ([]byte, error)

// jwksEntry is one resolved verification key + its algorithm name.
type jwksEntry struct {
	key crypto.PublicKey
	alg string
}

// JWKSKeySet is the production auth.KeySet backed by a JWK Set. It
// satisfies the KeySet interface; construct via NewJWKSKeySet.
//
// Refresh + staleness behavior (the availability/security tradeoff an
// operator should know):
//
//   - On a `kid` miss or a TTL-stale snapshot, a refresh fetch runs
//     under a single-flight lock and is rate-limited to one fetch per
//     min-refresh interval, so a hostile/flapping IdP or `kid` flood
//     cannot drive unbounded fetches. While that one fetch is in
//     flight, concurrent miss/stale lookups block on it (bounded by the
//     HTTP timeout); hits against the cached snapshot never block.
//   - When a refresh fetch fails (IdP unreachable / malformed), the
//     prior snapshot is retained and served up to the MAX-STALE CEILING
//     — availability is preferred over hard-failing every request during
//     a transient IdP outage, but only within a bound. Once the snapshot
//     ages past the ceiling without a successful refresh, KeyByID fails
//     closed with ErrJWKSStale: the runtime can no longer vouch for the
//     cached keys, so a key the IdP has REMOVED (revoked) is no longer
//     honored. The ceiling BOUNDS — it does not make instantaneous —
//     revocation: an operator who needs a tighter revocation window sets
//     a smaller ceiling and pairs it with overlapping IdP signing keys
//     and short token TTLs.
type JWKSKeySet struct {
	fetch      jwksFetcher
	allowed    map[string]struct{}
	ttl        time.Duration
	minRefresh time.Duration
	maxStale   time.Duration
	now        func() time.Time
	logger     *slog.Logger

	// snapMu guards the key snapshot + its fetch time. KeyByID's hot
	// path takes it for read only.
	snapMu    sync.RWMutex
	keys      map[string]jwksEntry
	fetchedAt time.Time

	// refreshMu serializes refreshes (single-flight) and guards
	// lastAttempt — the refresh-bounding bookkeeping.
	refreshMu   sync.Mutex
	lastAttempt time.Time
}

// NewJWKSKeySet builds a JWKS-backed KeySet over src, accepting only
// keys whose algorithm is in allowedAlgs (which MUST be a subset of the
// asymmetric AllowedAlgorithms; an empty list falls back to the full
// asymmetric allowlist). The initial fetch+parse runs synchronously —
// a bad source, an unreachable URL, or a set with no usable asymmetric
// signing key fails loud here (the Runtime must not serve the Protocol
// edge with an unverifiable identity surface).
func NewJWKSKeySet(ctx context.Context, src JWKSSource, allowedAlgs []string, opts ...JWKSOption) (*JWKSKeySet, error) {
	cfg := jwksConfig{
		ttl:        defaultJWKSTTL,
		minRefresh: defaultJWKSMinRefreshInterval,
		maxStale:   defaultJWKSMaxStale,
		now:        time.Now,
		logger:     slog.Default(),
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	fetch, err := newJWKSFetcher(src, cfg.httpClient)
	if err != nil {
		return nil, err
	}

	allowed := buildAllowedSet(allowedAlgs)

	ks := &JWKSKeySet{
		fetch:      fetch,
		allowed:    allowed,
		ttl:        cfg.ttl,
		minRefresh: cfg.minRefresh,
		maxStale:   cfg.maxStale,
		now:        cfg.now,
		logger:     cfg.logger,
	}

	// Initial fetch — fail loud at construction.
	raw, err := fetch(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: JWKS initial fetch: %w", err)
	}
	keys, err := ks.parse(raw)
	if err != nil {
		return nil, err
	}
	now := cfg.now()
	ks.keys = keys
	ks.fetchedAt = now
	ks.lastAttempt = now
	return ks, nil
}

// buildAllowedSet intersects the caller's allowlist with the asymmetric
// AllowedAlgorithms. An empty caller list means "every asymmetric
// algorithm Harbor accepts."
func buildAllowedSet(allowedAlgs []string) map[string]struct{} {
	global := make(map[string]struct{}, len(AllowedAlgorithms))
	for _, a := range AllowedAlgorithms {
		global[a] = struct{}{}
	}
	if len(allowedAlgs) == 0 {
		return global
	}
	out := make(map[string]struct{}, len(allowedAlgs))
	for _, a := range allowedAlgs {
		if _, ok := global[a]; ok {
			out[a] = struct{}{}
		}
	}
	if len(out) == 0 {
		// The caller's list disjoint from the asymmetric allowlist —
		// fall back to the global set rather than accept nothing (the
		// config validator already rejects non-asymmetric algorithms, so
		// this is defence-in-depth, not a likely path).
		return global
	}
	return out
}

// newJWKSFetcher selects the URL or File fetcher for src. Exactly one
// of URL / File must be set.
func newJWKSFetcher(src JWKSSource, client *http.Client) (jwksFetcher, error) {
	hasURL := src.URL != ""
	hasFile := src.File != ""
	switch {
	case hasURL == hasFile:
		// both set or both empty
		return nil, ErrJWKSSource
	case hasURL:
		if client == nil {
			client = &http.Client{Timeout: defaultJWKSHTTPTimeout}
		}
		url := src.URL
		return func(ctx context.Context) ([]byte, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return nil, fmt.Errorf("build request: %w", err)
			}
			req.Header.Set("Accept", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				return nil, fmt.Errorf("GET %s: %w", url, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("GET %s: unexpected status %d", url, resp.StatusCode)
			}
			body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes))
			if err != nil {
				return nil, fmt.Errorf("read body from %s: %w", url, err)
			}
			return body, nil
		}, nil
	default: // hasFile
		path := src.File
		return func(_ context.Context) ([]byte, error) {
			body, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read file %s: %w", path, err)
			}
			return body, nil
		}, nil
	}
}

// KeyByID implements auth.KeySet. It serves from cache while the cache
// is fresh, within the max-stale ceiling, and the kid resolves; a miss,
// a TTL-stale cache, or a snapshot at/over the ceiling triggers a
// bounded refresh. A kid that does not resolve after the bounded refresh
// returns a wrapped ErrUnknownKey; a snapshot still past the ceiling
// after the refresh returns a wrapped ErrJWKSStale — regardless of
// whether the kid resolves — so a possibly-revoked key in a too-old
// snapshot is never served.
func (ks *JWKSKeySet) KeyByID(kid string) (crypto.PublicKey, string, error) {
	// Hot path: a cache that already carries the kid, is fresh by the
	// TTL, AND is within the max-stale ceiling. The ceiling gate here is
	// load-bearing: the freshness check is against the cache TTL, so a
	// ceiling tighter than the TTL would otherwise be silently raised to
	// the TTL — the hot path would return a key already past the ceiling
	// and the refresh-and-recheck below would never run.
	if entry, fresh, age, ok := ks.lookup(kid); ok && fresh && age < ks.maxStale {
		return entry.key, entry.alg, nil
	}

	// Miss, TTL-stale, or past the ceiling — attempt a bounded refresh,
	// then re-check. The refresh runs under a context bound by the
	// default HTTP timeout so a slow IdP cannot wedge a verification
	// indefinitely; the per-request ctx is not available on the KeySet
	// interface, so this boundary owns the deadline.
	ks.maybeRefresh()

	entry, _, age, ok := ks.lookup(kid)
	// Ceiling re-check AFTER the recovery fetch: a snapshot still past
	// the ceiling can no longer be vouched for — fail closed regardless
	// of whether the kid resolves. A request that arrived just as the
	// snapshot aged out still got its one recovery fetch above; the
	// ceiling never short-circuits a recovery attempt.
	if age >= ks.maxStale {
		return nil, "", fmt.Errorf("%w: snapshot age %s exceeds the %s ceiling", ErrJWKSStale, age, ks.maxStale)
	}
	if ok {
		return entry.key, entry.alg, nil
	}
	return nil, "", fmt.Errorf("%w: kid %q not in JWK set", ErrUnknownKey, kid)
}

// lookup reads the current snapshot under the read lock. age is the time
// since the snapshot's last successful fetch; fresh reports whether the
// snapshot is within its TTL.
func (ks *JWKSKeySet) lookup(kid string) (entry jwksEntry, fresh bool, age time.Duration, ok bool) {
	ks.snapMu.RLock()
	defer ks.snapMu.RUnlock()
	entry, ok = ks.keys[kid]
	age = ks.now().Sub(ks.fetchedAt)
	fresh = age < ks.ttl
	return entry, fresh, age, ok
}

// maybeRefresh performs at most one network fetch per minimum-refresh
// window. It is single-flight (serialized by refreshMu) and skips the
// fetch entirely when another goroutine just refreshed (the snapshot
// became fresh) or when the bound has not elapsed since the last
// attempt. A fetch/parse failure is logged and leaves the prior cache
// in place — verification continues against the last-known-good set
// rather than failing every request because the IdP blipped.
func (ks *JWKSKeySet) maybeRefresh() {
	ks.refreshMu.Lock()
	defer ks.refreshMu.Unlock()

	now := ks.now()

	// Another goroutine may have refreshed while we waited for the lock.
	ks.snapMu.RLock()
	fresh := now.Sub(ks.fetchedAt) < ks.ttl
	ks.snapMu.RUnlock()
	if fresh && now.Sub(ks.lastAttempt) < ks.minRefresh {
		// Cache fresh AND we are inside the bound — nothing to do.
		return
	}
	// Refresh bound: never fetch more than once per window, regardless
	// of how many misses arrive.
	if now.Sub(ks.lastAttempt) < ks.minRefresh {
		return
	}
	ks.lastAttempt = now

	ctx, cancel := context.WithTimeout(context.Background(), defaultJWKSHTTPTimeout)
	defer cancel()
	raw, err := ks.fetch(ctx)
	if err != nil {
		ks.logRefreshFailure("auth: JWKS refresh fetch failed", err, now)
		return
	}
	keys, err := ks.parse(raw)
	if err != nil {
		ks.logRefreshFailure("auth: JWKS refresh parse failed", err, now)
		return
	}
	ks.snapMu.Lock()
	ks.keys = keys
	ks.fetchedAt = now
	ks.snapMu.Unlock()
}

// logRefreshFailure records a failed refresh. When the retained snapshot
// is still within the max-stale ceiling it logs at Warn (availability is
// preferred — the prior cache is still served). When the snapshot is
// already AT/OVER the ceiling it escalates to Error: the runtime is now
// failing closed and an operator needs to know the key source has been
// unreachable past the bound. The log is naturally rate-bounded — it
// only fires on the refresh path, which maybeRefresh gates to at most
// once per minimum-refresh window — so a flood of rejected requests does
// not flood the logs. No raw token is involved (only the fetch/parse
// error and the snapshot age).
func (ks *JWKSKeySet) logRefreshFailure(msg string, err error, now time.Time) {
	ks.snapMu.RLock()
	age := now.Sub(ks.fetchedAt)
	ks.snapMu.RUnlock()
	if age >= ks.maxStale {
		ks.logger.Error(msg+"; snapshot past max-stale ceiling — failing closed",
			slog.String("error", err.Error()),
			slog.Duration("snapshot_age", age),
			slog.Duration("max_stale", ks.maxStale))
		return
	}
	ks.logger.Warn(msg+"; serving prior cache",
		slog.String("error", err.Error()))
}

// jwk is one entry in a JWK Set, decoded from JSON. Only the fields
// Harbor consumes are modeled.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	// RSA
	N string `json:"n"`
	E string `json:"e"`
	// EC
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

type jwkSet struct {
	Keys []jwk `json:"keys"`
}

// parse decodes a JWK Set and resolves each usable asymmetric signing
// key into the kid→entry map. Symmetric, encryption-use, and malformed
// keys are skipped (a real JWKS routinely mixes signing and other
// material); the parse fails only if the JSON is malformed or the set
// yields ZERO usable keys (fail closed — a set Harbor cannot verify
// anything against is a misconfiguration).
func (ks *JWKSKeySet) parse(raw []byte) (map[string]jwksEntry, error) {
	var set jwkSet
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("auth: JWKS parse: %w", err)
	}
	out := make(map[string]jwksEntry, len(set.Keys))
	for i := range set.Keys {
		k := set.Keys[i]
		// Skip explicit encryption keys — Harbor verifies signatures.
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		key, alg, err := ks.resolveKey(k)
		if err != nil {
			ks.logger.Warn("auth: JWKS dropping unusable key",
				slog.String("kid", k.Kid),
				slog.String("kty", k.Kty),
				slog.String("error", err.Error()))
			continue
		}
		out[k.Kid] = jwksEntry{key: key, alg: alg}
	}
	if len(out) == 0 {
		return nil, ErrJWKSNoUsableKeys
	}
	return out, nil
}

// resolveKey converts one JWK into a public key + its algorithm name,
// enforcing the asymmetric-only allowlist. A symmetric (`oct`) or
// otherwise unsupported `kty` is rejected.
func (ks *JWKSKeySet) resolveKey(k jwk) (crypto.PublicKey, string, error) {
	switch k.Kty {
	case "RSA":
		key, err := parseRSAJWK(k)
		if err != nil {
			return nil, "", err
		}
		alg := k.Alg
		if alg == "" {
			alg = "RS256" // RSA JWKs commonly omit alg; RS256 is the canonical default
		}
		if err := ks.checkAlg(alg); err != nil {
			return nil, "", err
		}
		return key, alg, nil
	case "EC":
		key, inferredAlg, err := parseECJWK(k)
		if err != nil {
			return nil, "", err
		}
		alg := k.Alg
		if alg == "" {
			alg = inferredAlg
		}
		if err := ks.checkAlg(alg); err != nil {
			return nil, "", err
		}
		return key, alg, nil
	case "oct":
		return nil, "", errors.New("symmetric (oct) key rejected: Harbor's allowlist is asymmetric-only")
	default:
		return nil, "", fmt.Errorf("unsupported key type %q", k.Kty)
	}
}

// checkAlg enforces that alg is in the keyset's accepted set (the
// intersection of the operator's allowlist and the asymmetric
// AllowedAlgorithms).
func (ks *JWKSKeySet) checkAlg(alg string) error {
	if _, ok := ks.allowed[alg]; !ok {
		return fmt.Errorf("algorithm %q not in the accepted asymmetric allowlist", alg)
	}
	return nil
}

// parseRSAJWK builds an *rsa.PublicKey from the base64url `n`/`e`
// fields per RFC 7518 §6.3.1.
func parseRSAJWK(k jwk) (*rsa.PublicKey, error) {
	if k.N == "" || k.E == "" {
		return nil, errors.New("RSA JWK missing n or e")
	}
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("RSA JWK n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("RSA JWK e: %w", err)
	}
	n := new(big.Int).SetBytes(nBytes)
	e := new(big.Int).SetBytes(eBytes)
	if n.Sign() <= 0 || e.Sign() <= 0 {
		return nil, errors.New("RSA JWK n/e must be positive")
	}
	if !e.IsInt64() || e.Int64() > (1<<31-1) {
		return nil, errors.New("RSA JWK e out of range")
	}
	// Defense in depth: refuse undersized RSA moduli. A misconfigured or
	// compromised IdP publishing a weak key (e.g. 512/1024-bit) would
	// otherwise be accepted, enabling forgeable signatures. 2048 is the
	// modern floor.
	if n.BitLen() < minRSAModulusBits {
		return nil, fmt.Errorf("RSA JWK modulus too small: %d bits (minimum %d)", n.BitLen(), minRSAModulusBits)
	}
	return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
}

// parseECJWK builds an *ecdsa.PublicKey from the `crv`/`x`/`y` fields
// per RFC 7518 §6.2.1, validating the point lies on the named curve.
// The returned algorithm is the curve's canonical ECDSA algorithm.
func parseECJWK(k jwk) (*ecdsa.PublicKey, string, error) {
	var curve elliptic.Curve
	var alg string
	switch k.Crv {
	case "P-256":
		curve, alg = elliptic.P256(), "ES256"
	case "P-384":
		curve, alg = elliptic.P384(), "ES384"
	case "P-521":
		curve, alg = elliptic.P521(), "ES512"
	default:
		return nil, "", fmt.Errorf("EC JWK unsupported crv %q", k.Crv)
	}
	if k.X == "" || k.Y == "" {
		return nil, "", errors.New("EC JWK missing x or y")
	}
	xBytes, err := base64.RawURLEncoding.DecodeString(k.X)
	if err != nil {
		return nil, "", fmt.Errorf("EC JWK x: %w", err)
	}
	yBytes, err := base64.RawURLEncoding.DecodeString(k.Y)
	if err != nil {
		return nil, "", fmt.Errorf("EC JWK y: %w", err)
	}
	pub := &ecdsa.PublicKey{
		Curve: curve,
		X:     new(big.Int).SetBytes(xBytes),
		Y:     new(big.Int).SetBytes(yBytes),
	}
	// Validate the point is on the curve via the non-deprecated ECDH
	// conversion (which rejects an off-curve or identity point).
	if _, err := pub.ECDH(); err != nil {
		return nil, "", fmt.Errorf("EC JWK point invalid for %s: %w", k.Crv, err)
	}
	return pub, alg, nil
}
