package artifacts

import (
	"context"
	"errors"
	"time"
)

// Presigner is an OPTIONAL capability interface that backends with
// native presigned-URL support implement. This is the explicit
// exception to the "no optional capabilities" rule in AGENTS.md §4.4:
// only S3-compatible object stores have presigned URLs natively, and
// the capability cannot be reasonably faked by the other V1 drivers
// (InMem / FS / SQLite-blob / Postgres-blob) without bolting a
// separate signing service onto Harbor — which is out of V1 scope.
//
// Per RFC §6.10 + brief 05 §3, presigned URLs are the read-side
// hand-off path for media-class artifacts (D-021, D-022): the runtime
// hands a Console / Protocol client a time-bounded HTTPS URL the
// client downloads directly from object storage, bypassing the
// runtime's bytes path entirely.
//
// Callers that need presigned URLs type-assert the `ArtifactStore`
// they hold to `Presigner`; absence is a typed error
// (`ErrPresignUnsupported`), not a silent fallback. The Phase 19 S3
// driver is the only V1 driver implementing this capability.
//
// Identity is mandatory at the Presigner boundary just like every
// other ArtifactStore method: implementations MUST validate the
// `scope` and reject with `ErrIdentityRequired` when any of
// tenant/user/session is empty.
//
// `expiry` is bounded — implementations MUST reject expiries shorter
// than 1 minute or longer than 7 days (S3's documented limit). Out-
// of-range expiries return a clear error rather than being silently
// clamped (AGENTS.md §5: fail loudly).
type Presigner interface {
	// PresignGet returns a time-bounded HTTPS URL the caller can hand
	// to a downstream consumer for direct download of the artifact's
	// bytes. Read-side only — there is no PresignPut / PresignDelete
	// counterpart at V1 (write-side presigned URLs are an attack
	// surface; documented in the Phase 19 plan).
	//
	// Returns a wrapped error when:
	//   - the scope fails identity validation (`ErrIdentityRequired`),
	//   - the artifact does not exist in this scope (`ErrNotFound`),
	//   - `expiry` is out of the `[1 minute, 7 days]` range,
	//   - the underlying signer fails.
	PresignGet(ctx context.Context, scope ArtifactScope, id string, expiry time.Duration) (string, error)
}

// ErrPresignUnsupported is returned (wrapped) when a caller asks an
// `ArtifactStore` for presigned URLs via type-assertion to
// `Presigner` and the underlying driver does not implement the
// capability. The error's presence is part of the failure-loud
// contract — silent fallback to byte-streaming would mask backend
// configuration mistakes.
var ErrPresignUnsupported = errors.New("artifacts: presigned URLs not supported by this driver")
