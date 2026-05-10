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
//	<prefix>/<tenant>/<user>/<session>/<task>/<namespace>/<id>
//	<prefix>/<tenant>/<user>/<session>/<task>/<namespace>/<id>.meta.json
//
// `<prefix>` is the operator-configured `S3Prefix` (may be empty —
// then the layout is rooted at the bucket itself). Empty `TaskID`
// becomes the literal segment `_` so the key hierarchy stays five
// levels deep below the namespace, parallel to the FS driver's
// `emptyTaskSentinel`. The `<id>.meta.json` sibling carries the
// `ArtifactRef` JSON (mime, size, sha, scope, namespace, source).
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
// Dedup. `PutBytes` / `PutText` HEAD the destination key first; if
// the existing object's ETag (or sibling `.meta.json` SHA256) matches
// the new bytes' SHA, the existing ref is returned without re-uploading.
//
// `Delete`. HEAD-then-batch-delete. Both blob + sibling meta land in a
// single `DeleteObjects` call. The HEAD pays a round trip but lets the
// driver return the same `(existed bool, error)` shape as the other
// drivers (S3's DeleteObject returns success regardless of prior
// existence).
//
// Concurrency. The SDK's `*s3.Client` and `*s3.PresignClient` are
// safe for concurrent use. The driver itself adds only an atomic
// closed flag; the conformance suite's `Concurrent_PutGet_NoRace`
// gate (N=128 default) and the supplemental N=32 stress in
// `concurrent_test.go` prove the contract holds end-to-end.
//
// Presigner capability. The driver implements `artifacts.Presigner`
// (`PresignGet` only — write-side presigned URLs are an attack
// surface intentionally not exposed at V1; see Phase 19 plan
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
	defaultNamespace  = "default"
	defaultMimeBytes  = "application/octet-stream"
	defaultMimeText   = "text/plain; charset=utf-8"
	emptyTaskSentinel = "_"
	metaSuffix        = ".meta.json"
	defaultRegion     = "us-east-1"

	// Presign expiry bounds per the Phase 19 plan / S3's documented
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
	// hook the conformance suite's goroutine-leak check (D-025) can
	// invoke. We clone DefaultTransport (preserving the SDK's
	// connection / TLS / dial characteristics) and tighten
	// `IdleConnTimeout` to 1s so connection-pool goroutines unwind
	// quickly under the test's 2s settle window. Production traffic
	// still gets keep-alive within the 1s window, which is enough for
	// burst latency wins on multi-call request paths.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.IdleConnTimeout = 1 * time.Second
	httpClient := &http.Client{Transport: transport}

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
	artifacts.Register("s3", func(cfg config.ArtifactsConfig) (artifacts.ArtifactStore, error) {
		return New(cfg)
	})
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

	// Dedup probe: if the meta object already exists and decodes to a
	// ref with the same SHA, the bytes are already stored. Avoids the
	// re-upload AND keeps the original ref's `Source` / `Filename`
	// (matching FS / InMem dedup contract: "first writer wins").
	existingRef, found, err := d.fetchRef(ctx, metaKey)
	if err != nil {
		return artifacts.ArtifactRef{}, fmt.Errorf("artifacts/s3: dedup probe: %w", err)
	}
	if found && existingRef.SHA256 == hexDigest {
		return *existingRef, nil
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

// Get implements artifacts.ArtifactStore. Found-false is NOT an
// error.
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
	blobKey := d.blobKey(scope, namespace, id)
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

// GetRef implements artifacts.ArtifactStore. Found-false is NOT an
// error.
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
	metaKey := d.blobKey(scope, namespace, id) + metaSuffix
	ref, found, err := d.fetchRef(ctx, metaKey)
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

// Exists implements artifacts.ArtifactStore. 404 → (false, nil).
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
	blobKey := d.blobKey(scope, namespace, id)
	_, err := d.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: awsmw.String(d.bucket),
		Key:    awsmw.String(blobKey),
	})
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("artifacts/s3: HeadObject %q: %w", blobKey, err)
	}
	return true, nil
}

// Delete implements artifacts.ArtifactStore. Idempotent: HEAD
// determines the prior existence boolean, then both blob + meta are
// removed via per-key `DeleteObject` calls.
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
	blobKey := d.blobKey(scope, namespace, id)
	metaKey := blobKey + metaSuffix

	// HEAD to determine existed-before-delete. Matches the conformance
	// suite's `(true, nil)` / `(false, nil)` contract; S3's DeleteObject
	// alone returns success regardless of prior existence.
	existed := false
	_, err := d.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: awsmw.String(d.bucket),
		Key:    awsmw.String(blobKey),
	})
	if err == nil {
		existed = true
	} else if !isNotFound(err) {
		return false, fmt.Errorf("artifacts/s3: HeadObject %q: %w", blobKey, err)
	}

	// Delete blob + meta via separate calls. Idempotent on absent
	// objects (S3 returns success regardless of prior existence; the
	// HEAD above carried the boolean).
	if _, err := d.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: awsmw.String(d.bucket),
		Key:    awsmw.String(blobKey),
	}); err != nil && !isNotFound(err) {
		return false, fmt.Errorf("artifacts/s3: DeleteObject blob %q: %w", blobKey, err)
	}
	if _, err := d.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: awsmw.String(d.bucket),
		Key:    awsmw.String(metaKey),
	}); err != nil && !isNotFound(err) {
		return false, fmt.Errorf("artifacts/s3: DeleteObject meta %q: %w", metaKey, err)
	}
	return existed, nil
}

// List implements artifacts.ArtifactStore. Empty fields in `filter`
// are wildcards. Iterates ListObjectsV2 pages under the derived
// prefix; ignores `.meta.json` keys when emitting blob refs (the
// sibling meta is fetched per-blob to populate the ref).
func (d *driver) List(ctx context.Context, filter artifacts.ArtifactScope) ([]artifacts.ArtifactRef, error) {
	if d.closed.Load() {
		return nil, artifacts.ErrStoreClosed
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
				// Defense in depth — the prefix already narrows by
				// scope, but a literal "_" task sentinel can match
				// loosely. Re-check.
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
// the conformance suite's goroutine-leak gate (D-025) sees the pool
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
	blobKey := d.blobKey(scope, namespace, id)

	// Verify existence first — presigning a non-existent key would
	// produce a URL that 404s, which is silent degradation.
	_, err := d.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: awsmw.String(d.bucket),
		Key:    awsmw.String(blobKey),
	})
	if err != nil {
		if isNotFound(err) {
			return "", fmt.Errorf("%w: id=%q scope=%+v", artifacts.ErrNotFound, id, scope)
		}
		return "", fmt.Errorf("artifacts/s3: presign HeadObject %q: %w", blobKey, err)
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

// blobKey returns the object key for `(scope, namespace, id)`. The
// driver's optional `prefix` (operator-configured `S3Prefix`) is
// folded in. Empty `TaskID` becomes the literal `_` segment so the
// hierarchy stays five levels deep below namespace.
func (d *driver) blobKey(scope artifacts.ArtifactScope, namespace, id string) string {
	task := scope.TaskID
	if task == "" {
		task = emptyTaskSentinel
	}
	parts := []string{}
	if d.prefix != "" {
		parts = append(parts, d.prefix)
	}
	parts = append(parts, scope.TenantID, scope.UserID, scope.SessionID, task, namespace, id)
	return strings.Join(parts, "/")
}

// listPrefix returns the longest common prefix that matches all
// objects whose scope satisfies `filter`. Empty fields stop the
// prefix walk — narrowing further would require client-side scope
// matching (which the driver does anyway in `List` for defense in
// depth).
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
	if filter.TaskID == "" {
		return strings.Join(parts, "/") + "/"
	}
	parts = append(parts, filter.TaskID)
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
	var statuser httpStatuser
	if errors.As(err, &statuser) {
		if statuser.HTTPStatusCode() == http.StatusNotFound {
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
