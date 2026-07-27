// Package s3 is Harbor's S3-compatible ArtifactStore driver. It is
// the operator-controlled-object-store production target — durable,
// multi-binary-friendly, and the canonical choice for cloud-native
// deployments. Speaks AWS S3, MinIO, Cloudflare R2, and any other
// S3-compatible API surface that the configured `Endpoint` and
// `UsePathStyle` knobs reach.
//
// Built on `github.com/aws/aws-sdk-go-v2`. CGO_ENABLED=0 stays — the
// SDK is pure Go.
//
// Object-key layout:
//
//	<prefix>/<tenant>/<user>/<session>/<namespace>/<id>
//	<prefix>/<tenant>/<user>/<session>/<namespace>/<id>.meta.json
//
// `<prefix>` is the operator-configured `S3Prefix` (may be empty —
// then the layout is rooted at the bucket itself). The
// `<id>.meta.json` sibling carries the `ArtifactRef` JSON (mime, size,
// sha, scope, namespace, source), and the producing task rides inside
// that JSON as `Scope.TaskID` — a provenance annotation.
//
// THE KEY IS THE READ KEY, AND ON THIS DRIVER IT HAS TO BE. Reads
// resolve on the isolation triple, so the object key holds exactly the
// triple. The task segment this layout used to carry was removed rather
// than merely ignored, and the reason is concurrency: an object store
// offers no atomic compare-and-set, so dedup here is a probe followed by
// a write. With the task in the key, N runs racing IDENTICAL bytes into
// one session each probe, each miss, and each write a SEPARATE object —
// so the store would hold N copies of one content-addressed id and
// `List` would return N rows for it. Keying on the triple makes the
// racers write the SAME key, so the store converges on one object by
// construction rather than by winning a probe. The in-memory,
// filesystem and SQL drivers get the same property from a mutex or a
// primary key; this driver gets it from the key itself.
//
// Objects written by an earlier build under the old
// `.../<session>/<task>/<namespace>/<id>` layout stay READABLE and
// DELETABLE: resolution probes the triple-keyed key first (one HEAD) and
// falls back to a `ListObjectsV2` scan of the session prefix, whose
// task-nested matches are returned in ascending task order — the same
// smallest-task collapse rule the filesystem driver's index rebuild and
// both SQL migrations apply. Nothing is rewritten in place; a bucket is
// migrated by being read.
//
// `Delete` removes EVERY copy under the triple — the triple-keyed object
// plus any task-nested leftover — because a delete that reported success
// while leaving a copy a later `Get` resolves is the silent degradation
// CLAUDE.md §13 forbids. That costs `Delete` one session-prefix
// listing.
//
// Identity-mandatory boundary. Tenant / user / session must be
// non-empty for Put*, Get, GetRef, Exists, Delete, and PresignGet.
// Empty `TaskID` is acceptable for session-scoped artifacts (matches
// the FS / InMem drivers).
//
// 404 semantics. `Get`, `GetRef`, `Exists`, `Delete` map S3 404 /
// `NoSuchKey` / `NotFound` to `(zero-value, false, nil)` — found-false
// is NOT an error, matching the FS driver's contract. Other errors
// (network, signature, permission) are wrapped and surfaced.
//
// Dedup. `PutBytes` / `PutText` resolve the triple first (a HEAD of the
// triple-keyed object, falling back to the session scan for a legacy
// task-nested one); if the resolved sibling `.meta.json` carries the new
// bytes' SHA, that existing ref is returned without re-uploading and the
// FIRST writer's provenance stamp is what comes back. Under a genuine
// concurrent tie the probe cannot order the writers — an object store
// has no compare-and-set — so both write the same key and the stored
// stamp is whichever write landed last. The stored artifact is still
// one; only the stamp is undetermined, and "first writer" is not a
// property an unordered pair of writes has.
//
// The cost, stated because it is a per-write cost and not only a
// migration one: storing a NEW artifact misses the HEAD and therefore
// pays one session-prefix listing before uploading. Skipping that
// listing would be cheaper and wrong on a bucket written by an earlier
// version — the same bytes would be stored a second time under the new
// key while the task-nested copy remained, so `List` would return two
// rows for one content-addressed id, which is the divergence keying on
// the triple exists to remove. The listing is scoped to one session's
// prefix, and it is the price of one uniform answer instead of two
// behaviours depending on when the bucket was written.
//
// `Delete`. Resolve-then-delete. The resolved key set carries the
// `(existed bool)` the other drivers return (S3's DeleteObject reports
// success regardless of prior existence), and every resolved copy — plus
// its sibling meta — is removed via per-key `DeleteObject` calls.
//
// Concurrency. The SDK's `*s3.Client` and `*s3.PresignClient` are
// safe for concurrent use. The driver itself adds only an atomic
// closed flag; the conformance suite's `Concurrent_PutGet_NoRace`
// gate (N=128 default) and the supplemental N=32 stress in
// `concurrent_test.go` prove the contract holds end-to-end.
//
// Presigner capability. The driver implements `artifacts.Presigner`
// (`PresignGet` only — write-side presigned URLs are an attack
// surface intentionally not exposed at V1; see plan
// non-goals). Expiry bounded `[1 minute, 7 days]` — out-of-range
// returns a clear error.
package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	awsmw "github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/config"
)

const (
	defaultNamespace = "default"
	defaultMimeBytes = "application/octet-stream"
	defaultMimeText  = "text/plain; charset=utf-8"
	metaSuffix       = ".meta.json"
	defaultRegion    = "us-east-1"

	// Presign expiry bounds per the phase plan / S3's documented
	// limit. Out-of-range expiries are rejected with a clear error
	// (fail-loudly per AGENTS.md §5).
	minPresignExpiry = 1 * time.Minute
	maxPresignExpiry = 7 * 24 * time.Hour
)

// errPresignExpiryOutOfRange is the sentinel returned when PresignGet
// receives an `expiry` outside `[1 minute, 7 days]`. Wrapped at the
// boundary so callers can `errors.Is` against it without depending on
// the AWS SDK's error types.
var errPresignExpiryOutOfRange = errors.New("artifacts/s3: presign expiry out of range [1m, 7d]")

// New constructs an S3-compatible ArtifactStore. `cfg.S3Bucket` must
// be non-empty (validated upstream by `config.Validate` when
// `Driver == "s3"`; rechecked here defensively).
//
// Builds an `*s3.Client` from `aws-sdk-go-v2`:
//   - When `cfg.S3AccessKeyID` and `cfg.S3SecretAccessKey` are both
//     set, a `credentials.NewStaticCredentialsProvider` is used.
//     Otherwise the SDK's default credential chain (env vars, IRSA,
//     instance metadata, ~/.aws/credentials) applies.
//   - `cfg.S3Region` defaults to "us-east-1" when empty.
//   - `cfg.S3Endpoint` overrides the default AWS endpoint via
//     `s3.Options.BaseEndpoint` — the modern AWS SDK v2 path that
//     supersedes the deprecated `EndpointResolver`.
//   - `cfg.S3UsePathStyle` flips `s3.Options.UsePathStyle = true` for
//     MinIO / older R2 buckets.
//
// On construction, `New` issues a single `HeadBucket` to verify the
// bucket exists and the credentials work. A 404 is mapped to a clear
// "bucket not found at endpoint X" error rather than left as the SDK's
// raw exception.
func New(cfg config.ArtifactsConfig) (artifacts.ArtifactStore, error) {
	if cfg.S3Bucket == "" {
		return nil, fmt.Errorf("artifacts/s3: S3Bucket must be set")
	}

	region := cfg.S3Region
	if region == "" {
		region = defaultRegion
	}

	// Build our own HTTP client so Close can drain idle connections
	// deterministically. The SDK's default `BuildableHTTPClient` is
	// fine for production but it doesn't expose a CloseIdleConnections
	// hook the conformance suite's goroutine-leak check can
	// invoke. We clone DefaultTransport (preserving the SDK's
	// connection / TLS / dial characteristics) and tighten
	// `IdleConnTimeout` to 1s so connection-pool goroutines unwind
	// quickly under the test's 2s settle window. Production traffic
	// still gets keep-alive within the 1s window, which is enough for
	// burst latency wins on multi-call request paths.
	// http.DefaultTransport is always a *http.Transport in the stdlib;
	// the comma-ok guards against a future stdlib change rather than a
	// real runtime case.
	httpClient := &http.Client{}
	var transport *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = base.Clone()
		transport.IdleConnTimeout = 1 * time.Second
		httpClient.Transport = transport
	}

	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(region),
		awsconfig.WithHTTPClient(httpClient),
	}
	if cfg.S3AccessKeyID != "" && cfg.S3SecretAccessKey != "" {
		loadOpts = append(loadOpts,
			awsconfig.WithCredentialsProvider(
				credentials.NewStaticCredentialsProvider(
					cfg.S3AccessKeyID, cfg.S3SecretAccessKey, ""),
			),
		)
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("artifacts/s3: load AWS config: %w", err)
	}

	clientOpts := []func(*awss3.Options){}
	if cfg.S3Endpoint != "" {
		endpoint := cfg.S3Endpoint
		clientOpts = append(clientOpts, func(o *awss3.Options) {
			o.BaseEndpoint = awsmw.String(endpoint)
		})
	}
	if cfg.S3UsePathStyle {
		clientOpts = append(clientOpts, func(o *awss3.Options) {
			o.UsePathStyle = true
		})
	}

	client := awss3.NewFromConfig(awsCfg, clientOpts...)

	// HeadBucket on construction. Maps 404 to a clear error so
	// operator misconfigs surface at boot.
	headCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := client.HeadBucket(headCtx, &awss3.HeadBucketInput{
		Bucket: awsmw.String(cfg.S3Bucket),
	}); err != nil {
		if isNotFound(err) {
			endpoint := cfg.S3Endpoint
			if endpoint == "" {
				endpoint = "<aws default>"
			}
			return nil, fmt.Errorf("artifacts/s3: bucket %q not found at endpoint %q: %w",
				cfg.S3Bucket, endpoint, err)
		}
		return nil, fmt.Errorf("artifacts/s3: HeadBucket %q: %w", cfg.S3Bucket, err)
	}

	return &driver{
		client:    client,
		presigner: awss3.NewPresignClient(client),
		bucket:    cfg.S3Bucket,
		prefix:    strings.Trim(cfg.S3Prefix, "/"),
		transport: transport,
	}, nil
}

func init() {
	artifacts.Register("s3", New)
}

type driver struct {
	client    *awss3.Client
	presigner *awss3.PresignClient
	bucket    string
	prefix    string
	transport *http.Transport
	closed    atomic.Bool
}

// PutBytes implements artifacts.ArtifactStore.
func (d *driver) PutBytes(ctx context.Context, scope artifacts.ArtifactScope, data []byte, opts artifacts.PutOpts) (artifacts.ArtifactRef, error) {
	if d.closed.Load() {
		return artifacts.ArtifactRef{}, artifacts.ErrStoreClosed
	}
	if err := scope.Validate(); err != nil {
		return artifacts.ArtifactRef{}, err
	}

	namespace := opts.Namespace
	if namespace == "" {
		namespace = defaultNamespace
	}
	mime := opts.MimeType
	if mime == "" {
		mime = defaultMimeBytes
	}

	digest := sha256.Sum256(data)
	hexDigest := hex.EncodeToString(digest[:])
	id := fmt.Sprintf("%s_%s", namespace, hexDigest[:12])

	blobKey := d.blobKey(scope, namespace, id)
	metaKey := blobKey + metaSuffix

	ref := artifacts.ArtifactRef{
		ID:        id,
		MimeType:  mime,
		SizeBytes: int64(len(data)),
		Filename:  opts.Filename,
		SHA256:    hexDigest,
		Scope:     scope,
		Namespace: namespace,
		Source:    cloneSource(opts.Source),
	}

	// Dedup probe on the TRIPLE, not on the caller's own key: if these
	// bytes are already stored anywhere under this session — including
	// under another task's segment — the artifact exists and the first
	// writer's ref is what comes back. Probing only the caller's key
	// would store a second copy under a second task and re-open the
	// enumerate-then-fail divergence the reconciled key closes.
	existingKey, existingFound, err := d.resolveBlobKey(ctx, scope, namespace, id)
	if err != nil {
		return artifacts.ArtifactRef{}, fmt.Errorf("artifacts/s3: dedup probe: %w", err)
	}
	if existingFound {
		existingRef, found, ferr := d.fetchRef(ctx, existingKey+metaSuffix)
		if ferr != nil {
			return artifacts.ArtifactRef{}, fmt.Errorf("artifacts/s3: dedup probe: %w", ferr)
		}
		if found && existingRef.SHA256 == hexDigest {
			return *existingRef, nil
		}
	}

	// Marshal meta first — if Source contains non-encodable values we
	// fail loudly here rather than after the blob is uploaded.
	metaBytes, err := json.Marshal(ref)
	if err != nil {
		return artifacts.ArtifactRef{}, fmt.Errorf("artifacts/s3: marshal meta: %w", err)
	}

	// PUT blob, then meta. We accept the brief inconsistency window
	// during which the blob exists without its meta — `Get` reads only
	// the blob (the meta is for `GetRef` and `List`). A subsequent
	// failed meta PUT is rare; we surface the error and the next Put
	// will retry both.
	if _, err := d.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:      awsmw.String(d.bucket),
		Key:         awsmw.String(blobKey),
		Body:        bytes.NewReader(data),
		ContentType: awsmw.String(mime),
	}); err != nil {
		return artifacts.ArtifactRef{}, fmt.Errorf("artifacts/s3: PutObject blob %q: %w", blobKey, err)
	}
	if _, err := d.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:      awsmw.String(d.bucket),
		Key:         awsmw.String(metaKey),
		Body:        bytes.NewReader(metaBytes),
		ContentType: awsmw.String("application/json"),
	}); err != nil {
		// Best-effort cleanup of the orphan blob so the next Put with
		// these bytes can succeed cleanly. Ignore cleanup errors.
		//nolint:errcheck // best-effort orphan-blob cleanup; the caller already gets the meta-PUT error
		_, _ = d.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
			Bucket: awsmw.String(d.bucket),
			Key:    awsmw.String(blobKey),
		})
		return artifacts.ArtifactRef{}, fmt.Errorf("artifacts/s3: PutObject meta %q: %w", metaKey, err)
	}

	return ref, nil
}

// PutText implements artifacts.ArtifactStore.
func (d *driver) PutText(ctx context.Context, scope artifacts.ArtifactScope, text string, opts artifacts.PutOpts) (artifacts.ArtifactRef, error) {
	if opts.MimeType == "" {
		opts.MimeType = defaultMimeText
	}
	return d.PutBytes(ctx, scope, []byte(text), opts)
}

// Get implements artifacts.ArtifactStore. Resolves on the isolation
// triple; `scope.TaskID` is ignored beyond serving as the first key
// probed. Found-false is NOT an error.
func (d *driver) Get(ctx context.Context, scope artifacts.ArtifactScope, id string) ([]byte, bool, error) {
	if d.closed.Load() {
		return nil, false, artifacts.ErrStoreClosed
	}
	if err := scope.Validate(); err != nil {
		return nil, false, err
	}
	if id == "" {
		return nil, false, nil
	}
	namespace := namespaceFromID(id)
	if namespace == "" {
		return nil, false, nil
	}
	blobKey, found, err := d.resolveBlobKey(ctx, scope, namespace, id)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	out, err := d.client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: awsmw.String(d.bucket),
		Key:    awsmw.String(blobKey),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("artifacts/s3: GetObject %q: %w", blobKey, err)
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, false, fmt.Errorf("artifacts/s3: read body %q: %w", blobKey, err)
	}
	return data, true, nil
}

// GetRef implements artifacts.ArtifactStore. Resolves on the isolation
// triple; the returned ref carries the STORED provenance stamp, which
// is the first writer's and may differ from the caller's. Found-false is
// NOT an error.
func (d *driver) GetRef(ctx context.Context, scope artifacts.ArtifactScope, id string) (*artifacts.ArtifactRef, bool, error) {
	if d.closed.Load() {
		return nil, false, artifacts.ErrStoreClosed
	}
	if err := scope.Validate(); err != nil {
		return nil, false, err
	}
	if id == "" {
		return nil, false, nil
	}
	namespace := namespaceFromID(id)
	if namespace == "" {
		return nil, false, nil
	}
	blobKey, found, err := d.resolveBlobKey(ctx, scope, namespace, id)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	ref, found, err := d.fetchRef(ctx, blobKey+metaSuffix)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	out := *ref
	out.Source = cloneSource(ref.Source)
	return &out, true, nil
}

// Exists implements artifacts.ArtifactStore. Resolves on the isolation
// triple; `scope.TaskID` is ignored. 404 → (false, nil).
func (d *driver) Exists(ctx context.Context, scope artifacts.ArtifactScope, id string) (bool, error) {
	if d.closed.Load() {
		return false, artifacts.ErrStoreClosed
	}
	if err := scope.Validate(); err != nil {
		return false, err
	}
	if id == "" {
		return false, nil
	}
	namespace := namespaceFromID(id)
	if namespace == "" {
		return false, nil
	}
	_, found, err := d.resolveBlobKey(ctx, scope, namespace, id)
	if err != nil {
		return false, err
	}
	return found, nil
}

// Delete implements artifacts.ArtifactStore. Resolves on the isolation
// triple and removes EVERY copy under it — the caller's own key plus any
// sibling an earlier build left under another task segment. Idempotent:
// the resolved key set carries the prior-existence boolean, then blob +
// meta are removed via per-key `DeleteObject` calls.
//
// This costs one `ListObjectsV2` over the session prefix per Delete. The
// alternative — delete the caller's key and stop — would let a later Get
// resolve a leftover copy, so the delete would have reported a success it
// did not perform (CLAUDE.md §13). Correctness is worth the round trip;
// a delete is rare next to a read.
//
// Note on `DeleteObject` vs `DeleteObjects` (plan deviation per
// AGENTS.md §4.3). The plan called for the batched `DeleteObjects`
// API, but several S3-compat backends (notably MinIO) reject it with
// `MissingContentMD5` because the AWS SDK v2 doesn't include the
// legacy Content-MD5 header. Per-key `DeleteObject` is universally
// accepted and pays only one extra round trip — acceptable trade.
// The conformance suite is the gate; this shape passes everywhere.
func (d *driver) Delete(ctx context.Context, scope artifacts.ArtifactScope, id string) (bool, error) {
	if d.closed.Load() {
		return false, artifacts.ErrStoreClosed
	}
	if err := scope.Validate(); err != nil {
		return false, err
	}
	if id == "" {
		return false, nil
	}
	namespace := namespaceFromID(id)
	if namespace == "" {
		return false, nil
	}

	// The resolved key set IS the existed-before-delete answer: S3's
	// DeleteObject returns success regardless of prior existence, so the
	// boolean has to come from a read.
	blobKeys, err := d.resolveBlobKeys(ctx, scope, namespace, id)
	if err != nil {
		return false, err
	}
	if len(blobKeys) == 0 {
		return false, nil
	}

	for _, blobKey := range blobKeys {
		metaKey := blobKey + metaSuffix
		if _, derr := d.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
			Bucket: awsmw.String(d.bucket),
			Key:    awsmw.String(blobKey),
		}); derr != nil && !isNotFound(derr) {
			return false, fmt.Errorf("artifacts/s3: DeleteObject blob %q: %w", blobKey, derr)
		}
		if _, derr := d.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
			Bucket: awsmw.String(d.bucket),
			Key:    awsmw.String(metaKey),
		}); derr != nil && !isNotFound(derr) {
			return false, fmt.Errorf("artifacts/s3: DeleteObject meta %q: %w", metaKey, derr)
		}
	}
	return true, nil
}

// List implements artifacts.ArtifactStore. `filter.TenantID` is
// required; every other empty field is a wildcard within that tenant.
// Iterates ListObjectsV2 pages under the derived prefix; ignores
// `.meta.json` keys when emitting blob refs (the sibling meta is fetched
// per-blob to populate the ref).
func (d *driver) List(ctx context.Context, filter artifacts.ArtifactScope) ([]artifacts.ArtifactRef, error) {
	if d.closed.Load() {
		return nil, artifacts.ErrStoreClosed
	}
	if err := filter.ValidateFilter(); err != nil {
		return nil, err
	}
	listPrefix := d.listPrefix(filter)
	var (
		out          []artifacts.ArtifactRef
		continuation *string
	)
	for {
		page, err := d.client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
			Bucket:            awsmw.String(d.bucket),
			Prefix:            awsmw.String(listPrefix),
			ContinuationToken: continuation,
		})
		if err != nil {
			return nil, fmt.Errorf("artifacts/s3: ListObjectsV2 prefix=%q: %w", listPrefix, err)
		}
		for _, obj := range page.Contents {
			key := awsmw.ToString(obj.Key)
			if strings.HasSuffix(key, metaSuffix) {
				// Sibling meta — fetched on demand below for blob keys.
				continue
			}
			metaKey := key + metaSuffix
			ref, found, err := d.fetchRef(ctx, metaKey)
			if err != nil {
				return nil, fmt.Errorf("artifacts/s3: List fetch meta %q: %w", metaKey, err)
			}
			if !found {
				// Orphan blob — skip silently. (Mirrors FS driver
				// tolerance for files that lack a sibling meta.)
				continue
			}
			if !matchesFilter(ref.Scope, filter) {
				// The key prefix narrows only as far as the session, so
				// this is where a `TaskID` filter is actually answered —
				// against the stored provenance stamp. It doubles as a
				// re-check of the identity components the prefix already
				// covered.
				continue
			}
			copyRef := *ref
			copyRef.Source = cloneSource(ref.Source)
			out = append(out, copyRef)
		}
		if page.IsTruncated == nil || !*page.IsTruncated {
			break
		}
		continuation = page.NextContinuationToken
	}
	return out, nil
}

// Close implements artifacts.ArtifactStore. SDK clients are stateless
// (no per-driver goroutines); Close flips the closed flag so subsequent
// calls return `ErrStoreClosed` and drains idle HTTP connections so
// the conformance suite's goroutine-leak gate sees the pool
// reset to baseline.
//
// `Close` is idempotent: subsequent calls are no-ops on the already-
// drained transport.
func (d *driver) Close(_ context.Context) error {
	d.closed.Store(true)
	if d.transport != nil {
		d.transport.CloseIdleConnections()
	}
	return nil
}

// PresignGet implements artifacts.Presigner. Returns a time-bounded
// HTTPS URL for direct download of the artifact's bytes. Read-side
// only — there is no PresignPut / PresignDelete (write-side presigned
// URLs are an attack surface intentionally not exposed at V1).
//
// Identity is mandatory at this boundary. `expiry` is bounded to
// [1 minute, 7 days]; out-of-range returns a wrapped clear error.
// If the artifact does not exist in this scope, returns a wrapped
// `artifacts.ErrNotFound` (presigning a key that doesn't exist would
// produce a URL that 404s — fail-loudly per AGENTS.md §5).
func (d *driver) PresignGet(ctx context.Context, scope artifacts.ArtifactScope, id string, expiry time.Duration) (string, error) {
	if d.closed.Load() {
		return "", artifacts.ErrStoreClosed
	}
	if err := scope.Validate(); err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("%w: empty id", artifacts.ErrNotFound)
	}
	if expiry < minPresignExpiry || expiry > maxPresignExpiry {
		return "", fmt.Errorf("%w: %s (must be in [%s, %s])",
			errPresignExpiryOutOfRange, expiry, minPresignExpiry, maxPresignExpiry)
	}
	namespace := namespaceFromID(id)
	if namespace == "" {
		return "", fmt.Errorf("%w: id %q has no namespace prefix", artifacts.ErrNotFound, id)
	}

	// Resolve on the triple — a presign is a read, and it must reach the
	// same object `Get` would. Resolution doubles as the existence check:
	// presigning a non-existent key would produce a URL that 404s, which
	// is silent degradation.
	blobKey, found, err := d.resolveBlobKey(ctx, scope, namespace, id)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%w: id=%q scope=%+v", artifacts.ErrNotFound, id, scope)
	}

	req, err := d.presigner.PresignGetObject(ctx,
		&awss3.GetObjectInput{
			Bucket: awsmw.String(d.bucket),
			Key:    awsmw.String(blobKey),
		},
		awss3.WithPresignExpires(expiry),
	)
	if err != nil {
		return "", fmt.Errorf("artifacts/s3: PresignGetObject %q: %w", blobKey, err)
	}
	return req.URL, nil
}

// blobKey returns the object key for `(scope-triple, namespace, id)`.
// The driver's optional `prefix` (operator-configured `S3Prefix`) is
// folded in. `scope.TaskID` does NOT appear: it is a provenance
// annotation carried inside the sibling meta object, and keeping it out
// of the key is what makes concurrent writers of identical bytes
// converge on one object.
func (d *driver) blobKey(scope artifacts.ArtifactScope, namespace, id string) string {
	parts := []string{}
	if d.prefix != "" {
		parts = append(parts, d.prefix)
	}
	parts = append(parts, scope.TenantID, scope.UserID, scope.SessionID, namespace, id)
	return strings.Join(parts, "/")
}

// sessionPrefix returns `<prefix>/<tenant>/<user>/<session>/` — the
// narrowest prefix that still spans every task segment of one session,
// which is the search space of a triple-keyed read.
func (d *driver) sessionPrefix(scope artifacts.ArtifactScope) string {
	parts := []string{}
	if d.prefix != "" {
		parts = append(parts, d.prefix)
	}
	parts = append(parts, scope.TenantID, scope.UserID, scope.SessionID)
	return strings.Join(parts, "/") + "/"
}

// resolveBlobKey returns the ONE object key a triple-keyed read
// resolves to for `(scope-triple, namespace, id)`.
//
// The triple-keyed key is probed first: a single HEAD, and the answer
// for every object this driver has written. Only a miss pays the session
// listing, which exists to find objects an earlier build stored under a
// task segment (see resolveBlobKeys).
func (d *driver) resolveBlobKey(ctx context.Context, scope artifacts.ArtifactScope, namespace, id string) (string, bool, error) {
	direct := d.blobKey(scope, namespace, id)
	_, err := d.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: awsmw.String(d.bucket),
		Key:    awsmw.String(direct),
	})
	if err == nil {
		return direct, true, nil
	}
	if !isNotFound(err) {
		return "", false, fmt.Errorf("artifacts/s3: HeadObject %q: %w", direct, err)
	}
	keys, err := d.resolveBlobKeys(ctx, scope, namespace, id)
	if err != nil {
		return "", false, err
	}
	if len(keys) == 0 {
		return "", false, nil
	}
	return keys[0], true, nil
}

// resolveBlobKeys returns EVERY object key under the scope's session
// that stores `(namespace, id)`: the triple-keyed key this driver writes
// today, plus any task-nested key an earlier build left behind.
//
// The triple-keyed key sorts FIRST when present, and the task-nested
// ones follow in ascending task order (S3 returns keys in lexicographic
// order and the session prefix is fixed, so page order already is task
// order). The ordering is made explicit here rather than inferred from
// the scan, because the two layouts interleave lexicographically — a
// namespace segment and a task segment occupy the same position — so
// scan order alone would not put the current layout first.
//
// The canonical key is the answer whenever it exists; more than one key
// is only reachable in a bucket written before the read key was
// reconciled.
func (d *driver) resolveBlobKeys(ctx context.Context, scope artifacts.ArtifactScope, namespace, id string) ([]string, error) {
	prefix := d.sessionPrefix(scope)
	suffix := "/" + namespace + "/" + id
	canonical := d.blobKey(scope, namespace, id)
	var (
		canonicalFound bool
		legacy         []string
		continuation   *string
	)
	for {
		page, err := d.client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
			Bucket:            awsmw.String(d.bucket),
			Prefix:            awsmw.String(prefix),
			ContinuationToken: continuation,
		})
		if err != nil {
			return nil, fmt.Errorf("artifacts/s3: ListObjectsV2 prefix=%q: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			key := awsmw.ToString(obj.Key)
			if !strings.HasSuffix(key, suffix) {
				continue
			}
			if key == canonical {
				canonicalFound = true
				continue
			}
			legacy = append(legacy, key)
		}
		if page.IsTruncated == nil || !*page.IsTruncated {
			break
		}
		continuation = page.NextContinuationToken
	}
	out := make([]string, 0, len(legacy)+1)
	if canonicalFound {
		out = append(out, canonical)
	}
	return append(out, legacy...), nil
}

// listPrefix returns the longest common prefix that matches all
// objects whose scope satisfies `filter`. Empty fields stop the prefix
// walk.
//
// The walk stops at the SESSION even when `filter.TaskID` is set: the
// task is no longer a key segment, so descending into one would produce
// a prefix that matches nothing. A `TaskID` filter is answered
// client-side in `List` against the stored provenance stamp, which is
// where it lives.
func (d *driver) listPrefix(filter artifacts.ArtifactScope) string {
	parts := []string{}
	if d.prefix != "" {
		parts = append(parts, d.prefix)
	}
	if filter.TenantID == "" {
		return joinSlash(parts) + slashIfNonEmpty(parts)
	}
	parts = append(parts, filter.TenantID)
	if filter.UserID == "" {
		return strings.Join(parts, "/") + "/"
	}
	parts = append(parts, filter.UserID)
	if filter.SessionID == "" {
		return strings.Join(parts, "/") + "/"
	}
	parts = append(parts, filter.SessionID)
	return strings.Join(parts, "/") + "/"
}

func joinSlash(parts []string) string {
	return strings.Join(parts, "/")
}

func slashIfNonEmpty(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	return "/"
}

// fetchRef downloads and JSON-decodes the meta object at metaKey.
// Returns (nil, false, nil) on 404, (*ref, true, nil) on success, or
// a wrapped error.
func (d *driver) fetchRef(ctx context.Context, metaKey string) (*artifacts.ArtifactRef, bool, error) {
	out, err := d.client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: awsmw.String(d.bucket),
		Key:    awsmw.String(metaKey),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("artifacts/s3: fetchRef GetObject %q: %w", metaKey, err)
	}
	defer func() { _ = out.Body.Close() }()
	raw, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, false, fmt.Errorf("artifacts/s3: fetchRef read %q: %w", metaKey, err)
	}
	var ref artifacts.ArtifactRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		return nil, false, fmt.Errorf("artifacts/s3: fetchRef unmarshal %q: %w", metaKey, err)
	}
	return &ref, true, nil
}

// isNotFound maps the assorted shapes S3 / MinIO / R2 return for
// "object/bucket not found" into a single boolean. Covers:
//   - `*s3types.NoSuchKey` (GetObject)
//   - `*s3types.NoSuchBucket` (HeadBucket)
//   - `*s3types.NotFound` (HeadObject — yes, S3's HeadObject doesn't
//     return NoSuchKey, it returns a generic NotFound)
//   - smithy `*types.GenericAPIError` with code 404 / "NotFound" /
//     "NoSuchKey" / "NoSuchBucket"
//   - HTTP response with status 404 surfaced via smithy's
//     `*http.ResponseError`.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var nsk *s3types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nsb *s3types.NoSuchBucket
	if errors.As(err, &nsb) {
		return true
	}
	var nf *s3types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NoSuchBucket", "NotFound", "404":
			return true
		}
	}
	// Smithy's HTTP response error carries the HTTP status; check 404.
	type httpStatuser interface{ HTTPStatusCode() int }
	var stature httpStatuser
	if errors.As(err, &stature) {
		if stature.HTTPStatusCode() == http.StatusNotFound {
			return true
		}
	}
	return false
}

// namespaceFromID returns the namespace embedded in the canonical id
// shape `<namespace>_<sha[:12]>`. Returns "" when the id is malformed
// (no `_` separator) — callers map "" to a found-false response,
// matching the FS / InMem drivers' tolerance for invalid ids.
func namespaceFromID(id string) string {
	idx := strings.LastIndex(id, "_")
	if idx <= 0 {
		return ""
	}
	return id[:idx]
}

func matchesFilter(scope, filter artifacts.ArtifactScope) bool {
	if filter.TenantID != "" && scope.TenantID != filter.TenantID {
		return false
	}
	if filter.UserID != "" && scope.UserID != filter.UserID {
		return false
	}
	if filter.SessionID != "" && scope.SessionID != filter.SessionID {
		return false
	}
	if filter.TaskID != "" && scope.TaskID != filter.TaskID {
		return false
	}
	return true
}

func cloneSource(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// Compile-time assertions that driver satisfies ArtifactStore AND
// the optional Presigner capability.
var (
	_ artifacts.ArtifactStore = (*driver)(nil)
	_ artifacts.Presigner     = (*driver)(nil)
)
