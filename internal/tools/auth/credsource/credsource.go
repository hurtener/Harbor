// Package credsource is the §4.4 seam through which an OAuth provider's
// OWN client credential (`client_id` / `client_secret`) resolves. It
// answers a single question: WHERE does the runtime get the credential
// it uses to authenticate itself to an authorization server or a
// credential broker?
//
// Two sources ship:
//
//   - env    — the credential is read from the process environment ONCE
//     at boot (the historical behavior; the default). Fail-loud at boot
//     when a named env var is unset. A credential that changes after
//     exec can never reach a running runtime — but the process env is
//     fixed at exec anyway, so this is not a limitation of env, it is a
//     property of env.
//   - remote — the runtime PULLS the credential from a coordinator-
//     served endpoint at first need, authenticated by the runtime's own
//     service token (itself env-indirected). The pulled credential is
//     held in memory only, TTL-capped, single-flighted, and refetched on
//     expiry. A coordinator that mints a runtime's credential AFTER the
//     runtime booted reaches it with zero touch — the motivation for the
//     whole seam (a broker credential for the `tokenexchange` driver, one
//     level up from the brokered downstream tokens that strategy already pulls).
//
// # Why a pull, never a push
//
// A credential arriving in-band over the Protocol is credential
// passthrough (CLAUDE.md §7 rule 3), the shape the token-exchange strategy already rejects.
// The remote source mirrors that pull posture one level up: the
// credential is pulled from the authority that owns it, cached in memory
// only, never persisted (a sealed per-runtime copy would recreate the
// shadow-store revocation hole a persisted copy would create). With `remote`, the broker
// client secret never enters the runtime's environment at all —
// defense-in-depth.
//
// # Fail loud, one mode per source
//
// Every failure is explicit (CLAUDE.md §13): the env source fails the
// boot when a named env var is empty; the remote source fails the tool
// call with the typed [ErrCredentialSourceUnavailable] when a fetch is
// unreachable / non-200 / malformed. There is NEVER a fallback from
// remote to env, to an unauthenticated call, or to the interactive flow.
// A provider declares exactly ONE source; the config validator rejects a
// dual declaration.
//
// # No import of the auth package
//
// This package deliberately depends on nothing under
// `internal/tools/auth` (only the low-level `internal/events`,
// `internal/audit`, `internal/identity`). `internal/tools/auth`
// (`BuildProviders`) imports THIS package to construct the source; the
// concrete drivers (`oauth2`, `tokenexchange`) reach the resolved
// credential through the [Source] carried on `auth.ProviderConfig`. The
// one-directional dependency keeps the seam a leaf.
package credsource

import (
	"context"
	"time"
)

// Canonical source names. The `internal/config` validator's
// `allowedCredentialSources` allowlist mirrors these constants; the
// drift guard `TestRegisteredSourcesMatchConfigAllowlist` asserts no
// drift between the two surfaces.
const (
	// SourceEnv resolves the client credential from the process
	// environment once at boot (the default).
	SourceEnv = "env"
	// SourceRemote pulls the client credential from a coordinator-served
	// endpoint at first need.
	SourceRemote = "remote"
)

// ClientCredential is one resolved OAuth client credential. The zero
// [ClientCredential.ExpiresAt] means "no expiry advertised" — env and
// static credentials never expire; only the remote source populates it
// (derived from the coordinator response's `expires_in`).
//
// The secret fields are NEVER logged and NEVER appear in an error
// string; callers pass them straight into the authorization-server /
// broker request body.
type ClientCredential struct {
	// ClientID is the resolved OAuth client_id. Required — a resolver
	// that yields an empty ClientID has failed and returns an error
	// instead.
	ClientID string
	// ClientSecret is the resolved OAuth client_secret. NEVER logged.
	ClientSecret string
	// ExpiresAt is the credential's validity horizon. Zero means "no
	// expiry advertised" (env / static). The remote source's in-memory
	// serve horizon never exceeds this value.
	ExpiresAt time.Time
}

// Source is the §4.4 seam interface every credential source implements.
// It is a MANDATORY interface — both shipped drivers implement both
// methods; there is no optional-capability ceremony (§4.4).
//
// Concurrent reuse: a Source is a compiled artifact built once
// at boot and shared across every provider invocation. Implementations
// hold immutable configuration plus at most an internally-synchronised
// in-memory cache; per-run identity is read from `ctx`, never from the
// Source.
type Source interface {
	// ValidateAtBoot performs the source's boot-time readiness check,
	// called ONCE by `BuildProviders` while the config is still being
	// assembled.
	//
	//   - The env source RESOLVES and caches the credential now,
	//     reproducing the historical boot-time fail-loud (an unset env
	//     var crashes assembly with a message naming the field).
	//   - The remote source validates only the block SHAPE (url
	//     well-formed, auth-token env var non-empty) and DEFERS the
	//     network fetch to the first Resolve — so a coordinator-minted
	//     credential reaches an already-running runtime with zero touch.
	ValidateAtBoot(ctx context.Context) error

	// Resolve returns the current client credential, fetching or
	// refreshing as the source requires. Called by provider drivers at
	// credential-need time (the `oauth2` driver at construction; the
	// `tokenexchange` driver lazily at exchange time). Implementations
	// are single-flight and memory-cached where a fetch is involved.
	//
	// A failure returns a wrapped [ErrCredentialSourceUnavailable]
	// carrying the provider name and (for remote) the endpoint — with
	// ZERO secret bytes. Callers `errors.Is` against the sentinel.
	Resolve(ctx context.Context) (ClientCredential, error)
}

// static is the pre-resolved [Source] returned by [Static]: a caller
// that already holds the credential (a headless embedder wiring
// `auth.ProviderConfig` by hand, a driver unit test) wraps it so the
// driver's single credential-resolution path (always through a Source)
// stays uniform — there is no second "static fields" path on
// `ProviderConfig` (§13, one mode).
type static struct {
	cred ClientCredential
}

// Static returns a [Source] that resolves to the given, already-resolved
// credential and never expires. It is NOT registered in the factory (it
// has no config-selectable name); it exists solely so direct
// construction of `auth.ProviderConfig` — outside `BuildProviders` — has
// a Source to supply. Emptiness is the caller's responsibility; the
// driver validates the resolved credential at its point of use.
func Static(clientID, clientSecret string) Source {
	return static{cred: ClientCredential{ClientID: clientID, ClientSecret: clientSecret}}
}

func (s static) ValidateAtBoot(context.Context) error { return nil }

func (s static) Resolve(context.Context) (ClientCredential, error) { return s.cred, nil }
