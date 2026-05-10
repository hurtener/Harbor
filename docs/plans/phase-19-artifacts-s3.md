# Phase 19 — ArtifactStore S3-style driver

## Summary

Land `internal/artifacts/drivers/s3/`: a S3-compatible `ArtifactStore` driver suitable for AWS S3, MinIO, Cloudflare R2, GCS via the S3 compat shim, and any other operator-controlled object store with the same API surface. Inherits `internal/artifacts/conformancetest.Run` verbatim and adds the only post-Phase-17 surface extension: presigned-URL `GetRef` for cross-tenant artifact handoff scenarios. RFC §6.10 explicitly flags this as a V1 stretch — slip-to-V1.1 is acceptable if calendar pressure builds, but ship-on-time is preferred so operators with object-store-first deployments don't have to bridge the gap themselves.

## RFC anchor

- RFC §6.10
- RFC §9
- RFC §3.5
- RFC §4

## Briefs informing this phase

- brief 05

## Brief findings incorporated

- **brief 05 §1 (mandatory artifacts policy + S3 driver in the V1 set).** "Artifacts-2: SQLite-blob and Postgres-blob drivers, plus S3-style driver. Persistent artifact lifetimes that survive restart." Phase 19 is the third half — operators deploying with object-store-as-primary (the dominant pattern for cloud-native runs) get a first-class driver out of the box.
- **brief 05 §3 (presigned-URL `GetRef`).** "lifecycle integration; presigned-URL `GetRef` path." A presigned URL lets the runtime hand a Console / Protocol client a time-bounded download URL without proxying the bytes — important for media-class artifacts (D-021, D-022) and large tool outputs.
- **brief 05 §6 (artifact cleanup tests, scope-mismatch rejection, cross-tenant isolation).** Conformance suite covers all three; the driver inherits verbatim. S3-specific cleanup edge cases (race between `Delete` and an in-flight presigned `GET`) are documented as known V1 limitations — operator's responsibility to expire short URLs.

## Findings I'm departing from (if any)

- None.

## Goals

- New driver under `internal/artifacts/drivers/s3/` registered as `"s3"` via `init()`. Implements the full `artifacts.ArtifactStore` interface.
- Built on `github.com/aws/aws-sdk-go-v2` + `github.com/aws/aws-sdk-go-v2/service/s3` (the AWS SDK v2 — pure Go, modular, the canonical Go S3 client). The endpoint is configurable so MinIO / R2 / any S3-compat backend work without code changes.
- Object-key layout: `<prefix>/<tenant>/<user>/<session>/<task>/<namespace>/<id>` for blob bytes; sibling object `<prefix>/<tenant>/<user>/<session>/<task>/<namespace>/<id>.meta.json` for the `ArtifactRef` metadata. The `<prefix>` allows multiple Harbor deployments to share one bucket.
- Configuration via `ArtifactsConfig` extensions: `S3Bucket`, `S3Endpoint`, `S3Region`, `S3Prefix`, `S3AccessKeyID`, `S3SecretAccessKey` (the last two `secret:"true"` for redaction; usable directly OR via SDK default credential chain). Validator rejects empty Bucket when `Driver = "s3"`.
- **Surface extension: `Presigner`.** The driver implements an OPTIONAL capability interface `Presigner` (defined in `internal/artifacts/`) exposing `PresignGet(ctx, scope, id, expiry) (string, error)`. This is the only place across the V1 ArtifactStore drivers where an optional capability is acceptable — it's a backend-specific feature (only S3-compat stores have presigned URLs natively). Per AGENTS.md §4.4 "No optional-capability ceremony when all V1 drivers will implement everything" — this case is the explicit exception (only S3 has it; the others are legitimately incapable of producing presigned URLs without a separate signing service). Documented + justified inline.
- Pass `internal/artifacts/conformancetest.Run` end-to-end with zero scenario forks. Add ONE supplemental S3-specific test class — `Presign*` — that targets the new `Presigner` capability.
- Tests gate on `HARBOR_S3_*` env vars (or a `HARBOR_TEST_S3_DSN` umbrella var). When absent, tests `t.Skip` cleanly. CI provides a `minio/minio:latest` service container; locally contributors without docker see clean skips.
- Boot-log integration: `cmd/harbor/main.go` blank-imports the driver; `artifacts.RegisteredDrivers()` includes `"s3"`.

## Non-goals

- No multipart upload optimisation for large blobs at V1. The InMem/FS/SQL/PG drivers all accept byte slices; the S3 driver does the same. Multipart kicks in transparently inside the SDK if the object exceeds AWS's threshold (~5MB+); the driver doesn't expose a streaming API.
- No bucket-creation logic. Operators provision the bucket; the driver assumes it exists. (`New` does NOT call `CreateBucket`.)
- No lifecycle / TTL automation. S3-side bucket lifecycle policies are operator-configured outside of Harbor. The driver does NOT manipulate bucket lifecycle config.
- No event-driven notifications (S3 → SNS → Lambda style). V1 ships sync read/write only.
- No server-side encryption (SSE) configuration knobs at V1. Operators set bucket-level SSE outside Harbor; SDK requests inherit.
- No cross-region replication awareness. The driver speaks to the configured endpoint; replication is a bucket-config concern.
- No `PresignPut` / `PresignDelete`. Only `PresignGet` ships. Reasoning: write-side presigned URLs let arbitrary clients write into our bucket — that's an attack surface the runtime shouldn't open. Read-side presigned URLs are the read-handoff use case.

## Acceptance criteria

- [ ] `internal/artifacts/drivers/s3/s3.go` defines a `driver` struct implementing `artifacts.ArtifactStore`. Compile-time assertion: `var _ artifacts.ArtifactStore = (*driver)(nil)`. `init()` calls `artifacts.Register("s3", New)`.
- [ ] `New(cfg config.ArtifactsConfig) (artifacts.ArtifactStore, error)`:
  - Empty `S3Bucket` returns a clear error.
  - Builds an `*s3.Client` from `aws-sdk-go-v2` config (using `S3Region`, `S3Endpoint`, `S3AccessKeyID` + `S3SecretAccessKey` when provided; falls back to SDK default credential chain when access keys are absent).
  - Optionally pings the bucket with `HeadBucket` on construction; treat 404 as a clear error (`fmt.Errorf("artifacts/s3: bucket %q not found at endpoint %q: %w", ...)`); other errors are wrapped.
- [ ] **Config additions** (`internal/config/config.go`): `ArtifactsConfig` gains the seven `S3*` fields described below. Validator rejects empty `S3Bucket` when `Driver = "s3"`. Defaults: `S3Region` defaults to `us-east-1` when unset (covers MinIO + plain R2). `S3UsePathStyle` defaults to `false` (AWS native); operators flip on for MinIO.

```go
S3Bucket          string `yaml:"s3_bucket,omitempty"`
S3Endpoint        string `yaml:"s3_endpoint,omitempty"`
S3Region          string `yaml:"s3_region,omitempty"`
S3Prefix          string `yaml:"s3_prefix,omitempty"`
S3AccessKeyID     string `yaml:"s3_access_key_id,omitempty" secret:"true"`
S3SecretAccessKey string `yaml:"s3_secret_access_key,omitempty" secret:"true"`
S3UsePathStyle    bool   `yaml:"s3_use_path_style,omitempty"`  // MinIO / R2 typically need this
```

- [ ] `internal/artifacts/presigner.go` (new) defines the optional capability per the shape below.

```go
// Presigner is an OPTIONAL capability interface that backends with
// native presigned-URL support implement. This is the explicit
// exception to the "no optional capabilities" rule in AGENTS.md
// §4.4: only S3-compat stores have presigned URLs natively, and the
// capability cannot be reasonably faked by the other V1 drivers
// (InMem / FS / SQLite / Postgres).
//
// Callers that need presigned URLs type-assert the ArtifactStore to
// Presigner; absence is a typed error, not a silent fallback.
type Presigner interface {
    PresignGet(ctx context.Context, scope ArtifactScope, id string, expiry time.Duration) (string, error)
}

// ErrPresignUnsupported is returned (wrapped) when the underlying
// store doesn't implement Presigner.
var ErrPresignUnsupported = errors.New("artifacts: presigned URLs not supported by this driver")
```

- [ ] All eight `ArtifactStore` methods implemented:
  - `PutBytes` / `PutText` — `s3.PutObject` for both blob + sibling `<id>.meta.json`. Same scope+namespace+id deduplication: HEAD the existing object first; if present and SHA matches, return existing ref without re-PUT.
  - `Get` / `GetRef` — `s3.GetObject` (blob) / `s3.GetObject` (meta.json). 404 maps to `(nil, false, nil)` — NOT an error (matches the FS driver's contract).
  - `Exists` — `s3.HeadObject`. 404 → `(false, nil)`.
  - `Delete` — `s3.DeleteObjects` (deletes blob + meta.json in one batch). Idempotent: returns `(true, nil)` if either object existed; `(false, nil)` if neither did.
  - `List` — `s3.ListObjectsV2` with the appropriate `Prefix` derived from the filter scope. Empty filter fields → wildcard (the conformance suite's `List_NilFieldsAreWildcards` test). Filters out `.meta.json` keys; pairs each blob key with its sibling meta.
  - `Close` — sets the closed flag; SDK clients are stateless (no goroutines to join). Subsequent calls return `artifacts.ErrStoreClosed`.
- [ ] `PresignGet(ctx, scope, id, expiry) (string, error)` — uses `s3.NewPresignClient(client).PresignGetObject(ctx, ...)`. The `expiry` is bounded to `[1 minute, 7 days]` (S3's documented limit); out-of-range returns a clear error. Identity validation runs at the boundary.
- [ ] `internal/artifacts/drivers/s3/s3_test.go` runs `conformancetest.Run` against a `MinIO`-backed S3 endpoint, gated on `HARBOR_TEST_S3_DSN` (or the individual `HARBOR_TEST_S3_*` env vars). Each test creates a unique prefix (`harbor_test_<random_hex>/`) so concurrent runs don't collide. Cleanup deletes the prefix.
- [ ] `internal/artifacts/drivers/s3/presign_test.go` — covers `PresignGet` happy path (URL is fetchable), expiry-out-of-range rejection, identity-required rejection, cross-tenant scope isolation (presigning under tenant A's scope returns a URL valid only for tenant A's object).
- [ ] `internal/artifacts/drivers/s3/concurrent_test.go` — supplemental N≥32 stress (lower than other drivers because S3 is rate-limited; the SDK's default retry policy handles bursts but stress-testing harder serves no purpose). Asserts no errors, no goroutine leak.
- [ ] `cmd/harbor/main.go` adds blank import (alphabetic).
- [ ] `.github/workflows/ci.yml` — new `artifacts-s3` job using a `minio/minio:RELEASE.2024-...` service container. Env: `MINIO_ROOT_USER=minioadmin`, `MINIO_ROOT_PASSWORD=minioadmin`. Container starts on port 9000 + 9001. Step env: `HARBOR_TEST_S3_*` set to the MinIO endpoint + credentials + a freshly-created `harbor-test` bucket. The job runs `go test -race -count=1 -timeout 240s ./internal/artifacts/drivers/s3/...`.
- [ ] Coverage on `internal/artifacts/drivers/s3` ≥ 80%. Lower than the SQL/PG drivers because some SDK error-mapping branches need network-level fault injection that's out of scope for unit tests.
- [ ] `make drift-audit` and `make preflight` green at commit time.
- [ ] `scripts/smoke/phase-19.sh` present and executable. Reports OK once the driver tests pass; SKIP if `HARBOR_TEST_S3_DSN` unset locally.
- [ ] `docs/plans/README.md` Phase 19 row Status flips from `Pending` to `Shipped`.
- [ ] `docs/glossary.md` adds `Presigner` and `PresignGet` entries.

## Files added or changed

- `internal/artifacts/presigner.go` (new) — `Presigner` capability interface + `ErrPresignUnsupported`.
- `internal/artifacts/presigner_test.go` (new) — assert the capability is implementable; the InMem/FS drivers do NOT implement it (verified by negative type-assertion).
- `internal/artifacts/drivers/s3/s3.go` (new)
- `internal/artifacts/drivers/s3/s3_test.go` (new)
- `internal/artifacts/drivers/s3/presign_test.go` (new)
- `internal/artifacts/drivers/s3/concurrent_test.go` (new)
- `internal/config/config.go` (modified) — `ArtifactsConfig` gains the `S3*` fields.
- `internal/config/loader.go` / `validate.go` (modified) — defaults + validation.
- `cmd/harbor/main.go` (modified) — additive blank import.
- `.github/workflows/ci.yml` (modified) — `artifacts-s3` job with MinIO service container.
- `scripts/smoke/phase-19.sh` (new)
- `docs/plans/phase-19-artifacts-s3.md` (this file)
- `docs/plans/README.md` (modified)
- `docs/glossary.md` (modified)
- `examples/harbor.yaml` (modified) — document `artifacts.s3_*` fields
- `go.mod` / `go.sum` (modified) — `github.com/aws/aws-sdk-go-v2` + `github.com/aws/aws-sdk-go-v2/service/s3` + `aws-sdk-go-v2/config` + `aws-sdk-go-v2/credentials`

## Public API surface

```go
package s3 // internal/artifacts/drivers/s3

import (
    "github.com/hurtener/Harbor/internal/artifacts"
    "github.com/hurtener/Harbor/internal/config"
)

func New(cfg config.ArtifactsConfig) (artifacts.ArtifactStore, error)
```

```go
// internal/artifacts/presigner.go
package artifacts

import (
    "context"
    "errors"
    "time"
)

type Presigner interface {
    PresignGet(ctx context.Context, scope ArtifactScope, id string, expiry time.Duration) (string, error)
}

var ErrPresignUnsupported = errors.New("artifacts: presigned URLs not supported by this driver")
```

The S3 driver is the ONLY V1 driver implementing `Presigner`. Callers type-assert; absence is a typed error.

## Test plan

- **Unit:** config validation (empty bucket, malformed endpoint, S3 access keys both empty AND no SDK chain credentials → expected behavior); driver-internal helpers (key-derivation, prefix-handling).
- **Integration:** `conformancetest.Run` against MinIO. Real driver, real network calls. Identity propagation already covered by the suite.
- **Conformance:** Phase 19 adds zero scenarios to the canonical suite. The supplemental `Presign*` tests live in `presign_test.go` and only run against backends implementing `Presigner`.
- **Concurrency / leak (D-025):** `Concurrent_PutGet_NoRace` runs against the S3 driver with N≥128 (the suite default); supplemental `concurrent_test.go` adds a S3-specific N≥32 case. Goroutine baseline restored.

## Smoke script additions

- `scripts/smoke/phase-19.sh` runs:
  - `go test -race -count=1 -timeout 240s ./internal/artifacts/drivers/s3/...` — OK on green; FAIL otherwise. Without `HARBOR_TEST_S3_DSN`, tests skip cleanly; smoke shows OK.
  - `skip "phase 19: artifact-s3 has no HTTP/Protocol surface yet (lands in Phase 60+)"`.

## Coverage target

- `internal/artifacts/drivers/s3`: 80%.
- `internal/artifacts` (Presigner additions): existing 85% + minor uplift.

## Dependencies

- Phase 17 (ArtifactStore interface + InMem + FS) — Phase 19 inherits the interface, conformance suite, registry pattern, scope-validation helpers.
- Phase 02 (config) — `config.ArtifactsConfig` extensions land here.

## Risks / open questions

- **AWS SDK v2 dependency surface.** Adding `aws-sdk-go-v2` pulls in 10+ transitive deps. The SDK is pure Go, well-maintained, and the canonical choice. `go mod tidy` will manage transitive bumps.
- **MinIO container CI cost.** ~30-45s spin-up. Acceptable; the existing `state-postgres` job already conditioned the team on container-backed CI.
- **Path-style addressing.** AWS S3 deprecated path-style addressing in 2020 for new buckets, but MinIO and R2 still require it. The `S3UsePathStyle` knob handles this; default OFF (AWS native), ON for MinIO.
- **Endpoint URL construction.** AWS-SDK-v2 prefers `EndpointResolverV2` for endpoint customisation. The driver MUST use the resolver (not the deprecated `EndpointResolver`) per AWS's documented best practice; the test against MinIO is the gate.
- **Rate-limit-driven retries.** SDK default retry policy (3 retries, exponential backoff) handles `ProvisionedThroughputExceededException` / `503 SlowDown`. Not customised at V1.
- **Object lifecycle on `Delete`.** S3 returns success even if the object didn't exist. We treat that as `(false, nil)` — but the SDK doesn't tell us "object existed before delete." The driver does a `HeadObject` BEFORE the delete to determine the boolean; this adds latency but matches the conformance contract. Documented inline.
- **Eventual consistency.** Modern S3 + MinIO are strong-consistent for read-after-write. Older S3 versions (pre-Dec 2020) are not — operators on archaic AWS regions are outside V1 scope.
- **No open RFC §11 questions block this phase.**

## Glossary additions

- **`Presigner`** — optional capability interface implemented only by backends with native presigned-URL support (Phase 19 S3 driver). Callers type-assert from `ArtifactStore`; absence is a typed error (`ErrPresignUnsupported`). The explicit exception to AGENTS.md §4.4's no-optional-capability rule. RFC §6.10.
- **`PresignGet`** — the read-side presigned-URL operation. Returns a time-bounded HTTPS URL the caller can hand to a Console / Protocol client for direct download without proxying bytes. Bounded to `[1 minute, 7 days]` per S3's documented limit. RFC §6.10.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage on `internal/artifacts/drivers/s3` ≥ 80%
- [ ] If multi-isolation paths changed: cross-session isolation test passes — yes; conformance suite covers it; `presign_test.go` adds S3-specific cross-tenant presign scope test.
- [ ] **Concurrent-reuse test passes** — `Concurrent_PutGet_NoRace` against the S3 driver with N≥128 + supplemental S3-specific N≥32 (D-025).
- [ ] If new vocabulary: glossary updated (yes — `Presigner`, `PresignGet`).
- [ ] If a brief finding was departed from: N/A.
