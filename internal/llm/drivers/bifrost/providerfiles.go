// Provider-native multimodal upload pass + the driver-owned file_id
// lifecycle (the `provider_native` attachment disposition — RFC §6.5,
// §6.10).
//
// When a `Complete` request carries a `ProviderNative`-flagged content
// part without a `ProviderFileID`, the driver uploads the part's
// content to the provider's own file surface (`FileUploadRequest`),
// receives the opaque `file_id`, and rewrites the part to the
// provider file reference before translation. The upload happens
// INSIDE the driver so `LLMClient` stays one method and any
// `CompleteRequest` builder — planner-driven or hand-rolled — gets
// provider-native handling with no run loop, config file, or Protocol
// in the path.
//
// The `file_id` cache is identity-scoped — keyed by the
// `(tenant, user, session)` triple plus a content key (the
// content-addressed artifact ref, or a sha256 of inline bytes) — so a
// re-attached artifact is not re-uploaded every turn and a `file_id`
// NEVER crosses a session boundary (AGENTS.md §6). Lifecycle is
// driver-owned end to end: TTL expiry and LRU eviction issue a
// best-effort remote `FileDeleteRequest`, and `Close` sweeps every
// remaining entry, so a headless consumer who never runs a dev loop
// does not leak provider-side files.
//
// Degradation is loud, never silent: a provider that does not support
// file upload for a modality keeps the part's canonical
// `ArtifactStub` rendering (the universal degradation, RFC §6.5) and
// the driver logs a Warn naming the provider and modality. Any other
// upload failure fails the call.

package bifrost

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
)

const (
	// defaultFileCacheCapacity bounds the identity-scoped file_id
	// cache. Evicting past capacity deletes the remote file
	// (best-effort) so the cache size also bounds provider-side
	// storage held by one driver instance.
	defaultFileCacheCapacity = 128
	// defaultFileCacheTTL is how long a cached file_id stays
	// reusable. An expired entry is deleted remotely (best-effort)
	// and the next attach re-uploads.
	defaultFileCacheTTL = time.Hour
)

// fileCacheKey scopes a cached provider file_id. The identity triple
// is the isolation boundary (AGENTS.md §6 — never widen it); the
// content key is the content-addressed artifact ref (`ref:<id>` —
// artifact ids embed a sha256 prefix) or a digest of inline bytes
// (`sha256:<hex>`).
type fileCacheKey struct {
	tenant  string
	user    string
	session string
	content string
}

// fileCacheEntry is one cached upload.
type fileCacheEntry struct {
	key      fileCacheKey
	fileID   string
	storedAt time.Time
}

// providerFileCache is the internally-synchronized LRU+TTL cache of
// provider file_ids. All methods are safe for concurrent
// use; mutation happens only under `mu`. Remote deletion is the
// CALLER's job — the cache returns the evicted/expired file_ids so
// the driver can issue `FileDeleteRequest` outside the lock.
type providerFileCache struct {
	mu       sync.Mutex
	entries  map[fileCacheKey]*list.Element
	order    *list.List // of *fileCacheEntry; front = most recently used
	capacity int
	ttl      time.Duration
	// now is the clock seam — tests swap it; production is time.Now.
	now func() time.Time

	// inflight serializes same-key fills so two concurrent Completes
	// attaching the same content upload exactly once — the follower
	// re-reads the cache after the leader's put. Without this a
	// concurrent double-upload would orphan one remote file (the
	// second put overwrites the entry and the first file_id is never
	// deleted).
	inflightMu sync.Mutex
	inflight   map[fileCacheKey]*keyLock
}

// keyLock is one per-key serialization point, reference-counted so
// the inflight map stays bounded by ACTIVE fills.
type keyLock struct {
	mu   sync.Mutex
	refs int
}

func newProviderFileCache(capacity int, ttl time.Duration) *providerFileCache {
	return &providerFileCache{
		entries:  make(map[fileCacheKey]*list.Element),
		order:    list.New(),
		capacity: capacity,
		ttl:      ttl,
		now:      time.Now,
		inflight: make(map[fileCacheKey]*keyLock),
	}
}

// cacheEntryOf unwraps a list element's payload. The cache is the
// only writer of `order`, and it only ever stores *fileCacheEntry —
// the two-value form exists for the checked-type-assertion rule; a
// nil return is impossible by construction and callers skip it
// defensively.
func cacheEntryOf(elem *list.Element) *fileCacheEntry {
	entry, ok := elem.Value.(*fileCacheEntry)
	if !ok {
		return nil
	}
	return entry
}

// lockKey acquires the per-key fill lock and returns its release
// func. Callers hold it across the get-miss → upload → put sequence.
func (c *providerFileCache) lockKey(key fileCacheKey) func() {
	c.inflightMu.Lock()
	l, ok := c.inflight[key]
	if !ok {
		l = &keyLock{}
		c.inflight[key] = l
	}
	l.refs++
	c.inflightMu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		c.inflightMu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(c.inflight, key)
		}
		c.inflightMu.Unlock()
	}
}

// get returns the cached file_id for key. On a TTL-expired hit the
// entry is removed and its file_id returned in `expired` so the
// caller deletes the remote file.
func (c *providerFileCache) get(key fileCacheKey) (fileID string, ok bool, expired []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, found := c.entries[key]
	if !found {
		return "", false, nil
	}
	entry := cacheEntryOf(elem)
	if entry == nil {
		return "", false, nil
	}
	if c.ttl > 0 && c.now().Sub(entry.storedAt) >= c.ttl {
		c.order.Remove(elem)
		delete(c.entries, key)
		return "", false, []string{entry.fileID}
	}
	c.order.MoveToFront(elem)
	return entry.fileID, true, nil
}

// put inserts (or refreshes) a cached file_id and returns the
// file_ids of any LRU-evicted entries for remote deletion.
func (c *providerFileCache) put(key fileCacheKey, fileID string) (evicted []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, found := c.entries[key]; found {
		if entry := cacheEntryOf(elem); entry != nil {
			entry.fileID = fileID
			entry.storedAt = c.now()
			c.order.MoveToFront(elem)
			return nil
		}
	}
	elem := c.order.PushFront(&fileCacheEntry{key: key, fileID: fileID, storedAt: c.now()})
	c.entries[key] = elem
	for c.capacity > 0 && c.order.Len() > c.capacity {
		back := c.order.Back()
		entry := cacheEntryOf(back)
		c.order.Remove(back)
		if entry == nil {
			continue
		}
		delete(c.entries, entry.key)
		evicted = append(evicted, entry.fileID)
	}
	return evicted
}

// drain removes every entry and returns all file_ids — the
// Close-time sweep.
func (c *providerFileCache) drain() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	ids := make([]string, 0, len(c.entries))
	for elem := c.order.Front(); elem != nil; elem = elem.Next() {
		if entry := cacheEntryOf(elem); entry != nil {
			ids = append(ids, entry.fileID)
		}
	}
	c.entries = make(map[fileCacheKey]*list.Element)
	c.order.Init()
	return ids
}

// len reports the live entry count (test observability).
func (c *providerFileCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// applyProviderNative is the driver-internal upload pass. It walks
// the request's multimodal parts and, for every
// `ProviderNative`-flagged part without a `ProviderFileID`, resolves
// the content bytes (artifact store fetch, or inline DataURL decode),
// uploads them via `FileUploadRequest` (cache-aware), and rewrites
// the part to carry the returned `file_id`.
//
// Copy-on-write: the returned request shares untouched messages with
// the input; any message whose parts are rewritten gets cloned
// message + parts slices and cloned part structs, so the caller's
// request value is never mutated (no context bleed through a shared
// request — the concurrent-reuse contract).
func (d *Driver) applyProviderNative(
	ctx context.Context,
	bctx *bfschemas.BifrostContext,
	req llm.CompleteRequest,
	id identity.Quadruple,
) (llm.CompleteRequest, error) {
	if !requestHasProviderNativeWork(req) {
		return req, nil
	}

	messages := make([]llm.ChatMessage, len(req.Messages))
	copy(messages, req.Messages)
	for mi := range messages {
		if !messageHasProviderNativeWork(messages[mi]) {
			continue
		}
		parts := make([]llm.ContentPart, len(messages[mi].Content.Parts))
		copy(parts, messages[mi].Content.Parts)
		for pi := range parts {
			if err := ctx.Err(); err != nil {
				return req, err
			}
			rewritten, err := d.rewriteProviderNativePart(ctx, bctx, parts[pi], req.Model, id)
			if err != nil {
				return req, err
			}
			parts[pi] = rewritten
		}
		messages[mi].Content.Parts = parts
	}
	req.Messages = messages
	return req, nil
}

// requestHasProviderNativeWork reports whether any part needs the
// upload pass — the cheap pre-scan that keeps the zero-flag path
// allocation-free.
func requestHasProviderNativeWork(req llm.CompleteRequest) bool {
	for _, m := range req.Messages {
		if messageHasProviderNativeWork(m) {
			return true
		}
	}
	return false
}

func messageHasProviderNativeWork(m llm.ChatMessage) bool {
	for _, p := range m.Content.Parts {
		switch p.Type {
		case llm.PartImage:
			if p.Image != nil && p.Image.ProviderNative && p.Image.ProviderFileID == "" {
				return true
			}
		case llm.PartAudio:
			if p.Audio != nil && p.Audio.ProviderNative && p.Audio.ProviderFileID == "" {
				return true
			}
		case llm.PartFile:
			if p.File != nil && p.File.ProviderNative && p.File.ProviderFileID == "" {
				return true
			}
		}
	}
	return false
}

// rewriteProviderNativePart handles one part: a no-op for parts that
// don't need the upload, a cloned-and-rewritten part for those that
// do. The clone keeps the caller's part structs immutable.
func (d *Driver) rewriteProviderNativePart(
	ctx context.Context,
	bctx *bfschemas.BifrostContext,
	part llm.ContentPart,
	model string,
	id identity.Quadruple,
) (llm.ContentPart, error) {
	switch part.Type {
	case llm.PartImage:
		p := part.Image
		if p == nil || !p.ProviderNative || p.ProviderFileID != "" {
			return part, nil
		}
		fileID, err := d.providerFileFor(ctx, bctx, providerUpload{
			artifact: p.Artifact, dataURL: p.DataURL, mime: p.MIME, model: model,
		}, id)
		if err != nil || fileID == "" {
			return part, err
		}
		clone := *p
		clone.ProviderFileID = fileID
		part.Image = &clone
		return part, nil
	case llm.PartAudio:
		p := part.Audio
		if p == nil || !p.ProviderNative || p.ProviderFileID != "" {
			return part, nil
		}
		fileID, err := d.providerFileFor(ctx, bctx, providerUpload{
			artifact: p.Artifact, dataURL: p.DataURL, mime: p.MIME, model: model,
		}, id)
		if err != nil || fileID == "" {
			return part, err
		}
		clone := *p
		clone.ProviderFileID = fileID
		part.Audio = &clone
		return part, nil
	case llm.PartFile:
		p := part.File
		if p == nil || !p.ProviderNative || p.ProviderFileID != "" {
			return part, nil
		}
		fileID, err := d.providerFileFor(ctx, bctx, providerUpload{
			artifact: p.Artifact, dataURL: p.DataURL, mime: p.MIME,
			filename: p.Filename, model: model,
		}, id)
		if err != nil || fileID == "" {
			return part, err
		}
		clone := *p
		clone.ProviderFileID = fileID
		part.File = &clone
		return part, nil
	}
	return part, nil
}

// providerUpload is the per-part upload work order.
type providerUpload struct {
	artifact *llm.ArtifactStub
	dataURL  string
	mime     string
	filename string
	model    string
}

// providerFileFor resolves a part's content to a provider file_id:
// cache hit → reuse; miss → fetch bytes + upload + cache + emit
// `llm.provider_file.uploaded`. Returns ("", nil) for the loud
// degradation case (provider without upload support — the part keeps
// its `ArtifactStub` rendering) and for parts with nothing to upload
// (URL-only / empty — translation handles those).
func (d *Driver) providerFileFor(
	ctx context.Context,
	bctx *bfschemas.BifrostContext,
	up providerUpload,
	id identity.Quadruple,
) (string, error) {
	var contentKey string
	switch {
	case up.artifact != nil:
		contentKey = "ref:" + up.artifact.Ref
	case up.dataURL != "":
		sum := sha256.Sum256([]byte(up.dataURL))
		contentKey = "sha256:" + hex.EncodeToString(sum[:])
	default:
		// URL-only or empty part — nothing the driver can upload;
		// translation renders the existing supply form.
		return "", nil
	}

	key := fileCacheKey{
		tenant:  id.TenantID,
		user:    id.UserID,
		session: id.SessionID,
		content: contentKey,
	}
	// Serialize same-key fills: a concurrent re-attach of the same
	// content waits for the leader's upload and reuses its file_id.
	unlock := d.files.lockKey(key)
	defer unlock()
	cached, hit, expired := d.files.get(key)
	d.deleteProviderFiles(bctx, expired, "ttl_expired")
	if hit {
		return cached, nil
	}

	payload, artifactRef, err := d.resolveUploadBytes(ctx, up, id)
	if err != nil {
		return "", err
	}

	mime := up.mime
	if mime == "" && up.artifact != nil {
		mime = up.artifact.MIME
	}
	filename := up.filename
	if filename == "" {
		filename = uploadFilename(artifactRef, mime)
	}
	purpose := bfschemas.FilePurposeUserData
	if strings.HasPrefix(mime, "image/") {
		purpose = bfschemas.FilePurposeVision
	}
	modelName := up.model
	uploadReq := &bfschemas.BifrostFileUploadRequest{
		Provider: d.provider,
		Model:    &modelName,
		File:     payload,
		Filename: filename,
		Purpose:  purpose,
	}
	if mime != "" {
		ct := mime
		uploadReq.ContentType = &ct
	}

	resp, berr := d.client.FileUploadRequest(bctx, uploadReq)
	if berr != nil {
		if isUnsupportedOperation(berr) {
			// Loud degradation, never silent (AGENTS.md §13): the part
			// keeps its canonical ArtifactStub rendering — the
			// universal degradation (RFC §6.5) — and the operator sees
			// the Warn naming provider + modality.
			slog.Warn("bifrost: provider-native upload unsupported — keeping ArtifactStub reference",
				"tenant_id", id.TenantID,
				"user_id", id.UserID,
				"session_id", id.SessionID,
				"provider", string(d.provider),
				"modality", modalityForMIME(mime),
				"mime", mime,
				"artifact_ref", artifactRef,
			)
			return "", nil
		}
		return "", translateError(berr, "FileUploadRequest")
	}
	if resp == nil || resp.ID == "" {
		return "", fmt.Errorf("FileUploadRequest: bifrost: upload returned no file id")
	}

	evicted := d.files.put(key, resp.ID)
	d.deleteProviderFiles(bctx, evicted, "lru_evicted")
	d.emitProviderFileUploaded(ctx, id, up.model, artifactRef, mime, resp.ID, int64(len(payload)))
	return resp.ID, nil
}

// resolveUploadBytes produces the payload to upload: the artifact
// store's bytes for an Artifact-backed part (fail-loud on a missing
// store, a missing artifact, or a store error — never a silent skip),
// or the decoded inline DataURL bytes otherwise. The second return is
// the artifact ref for observability ("" for inline payloads).
func (d *Driver) resolveUploadBytes(ctx context.Context, up providerUpload, id identity.Quadruple) ([]byte, string, error) {
	if up.artifact != nil {
		if d.artifacts == nil {
			return nil, "", fmt.Errorf("bifrost: provider-native upload for artifact %q requires Deps.Artifacts (no artifact store wired)", up.artifact.Ref)
		}
		scope := artifacts.ArtifactScope{
			TenantID:  id.TenantID,
			UserID:    id.UserID,
			SessionID: id.SessionID,
		}
		payload, found, err := d.artifacts.Get(ctx, scope, up.artifact.Ref)
		if err != nil {
			return nil, "", fmt.Errorf("bifrost: provider-native upload: artifact %q fetch: %w", up.artifact.Ref, err)
		}
		if !found || len(payload) == 0 {
			return nil, "", fmt.Errorf("bifrost: provider-native upload: artifact %q not found in scope", up.artifact.Ref)
		}
		return payload, up.artifact.Ref, nil
	}
	payload, err := decodeDataURLPayload(up.dataURL)
	if err != nil {
		return nil, "", fmt.Errorf("bifrost: provider-native upload: %w", err)
	}
	return payload, "", nil
}

// decodeDataURLPayload extracts the raw bytes from a
// `data:<mime>[;base64],<payload>` URI. Mirrors the safety pass's
// decoder for the sub-threshold inline case (over-threshold DataURLs
// were already materialized to Artifact form upstream).
func decodeDataURLPayload(uri string) ([]byte, error) {
	if !strings.HasPrefix(uri, "data:") {
		return nil, fmt.Errorf("not a data URI")
	}
	body := uri[len("data:"):]
	comma := strings.IndexByte(body, ',')
	if comma < 0 {
		return nil, fmt.Errorf("data URI missing ','")
	}
	header := body[:comma]
	payload := body[comma+1:]
	if strings.Contains(header, "base64") {
		b, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return nil, fmt.Errorf("data URI base64 decode: %w", err)
		}
		return b, nil
	}
	return []byte(payload), nil
}

// deleteProviderFiles best-effort deletes remote provider files
// (TTL-expired / LRU-evicted / Close-drained cache entries). Failures
// are logged at Warn and never block the call path — the remote file
// is orphaned at worst, and provider-side retention policies are the
// final backstop.
func (d *Driver) deleteProviderFiles(bctx *bfschemas.BifrostContext, fileIDs []string, reason string) {
	for _, fid := range fileIDs {
		_, berr := d.client.FileDeleteRequest(bctx, &bfschemas.BifrostFileDeleteRequest{
			Provider: d.provider,
			FileID:   fid,
		})
		if berr != nil && !isUnsupportedOperation(berr) {
			slog.Warn("bifrost: provider file delete failed (best-effort)",
				"provider", string(d.provider),
				"file_id", fid,
				"reason", reason,
				"err", translateError(berr, "FileDeleteRequest").Error(),
			)
		}
	}
}

// cleanupProviderFiles is the Close-time sweep: drain the cache and
// best-effort delete every remote file.
func (d *Driver) cleanupProviderFiles(ctx context.Context) {
	if d.files == nil {
		return
	}
	ids := d.files.drain()
	if len(ids) == 0 {
		return
	}
	bctx := bfschemas.NewBifrostContext(ctx, bfschemas.NoDeadline)
	d.deleteProviderFiles(bctx, ids, "client_close")
}

// emitProviderFileUploaded publishes `llm.provider_file.uploaded` —
// the observability surface for provider-native uploads (no task
// field carries the file_id). Best-effort; a nil bus (a directly
// constructed test Driver) is a no-op.
func (d *Driver) emitProviderFileUploaded(ctx context.Context, id identity.Quadruple, model, artifactRef, mime, fileID string, size int64) {
	if d.bus == nil {
		return
	}
	now := time.Now()
	_ = d.bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort observability emit — must not block the LLM call path.
		Type:       llm.EventTypeProviderFileUploaded,
		Identity:   id,
		OccurredAt: now,
		Payload: llm.ProviderFileUploadedPayload{
			Identity:    id,
			Provider:    string(d.provider),
			Model:       model,
			ArtifactRef: artifactRef,
			MIME:        mime,
			Modality:    modalityForMIME(mime),
			FileID:      fileID,
			SizeBytes:   size,
			OccurredAt:  now,
		},
	})
}

// modalityForMIME maps a MIME type onto the event payload's modality
// vocabulary: `image` / `audio` / `video` / `pdf` / `file`.
func modalityForMIME(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "image"
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	case strings.HasPrefix(mime, "video/"):
		return "video"
	case mime == "application/pdf":
		return "pdf"
	default:
		return "file"
	}
}

// uploadFilename synthesizes a filename for providers that require
// one when the part carried none.
func uploadFilename(artifactRef, mime string) string {
	base := "harbor-upload"
	if artifactRef != "" {
		base = artifactRef
	}
	if i := strings.IndexByte(mime, '/'); i >= 0 && i+1 < len(mime) {
		return base + "." + mime[i+1:]
	}
	return base
}

// isUnsupportedOperation matches bifrost's typed "this provider does
// not implement this request type" error (`unsupported_operation`) —
// the loud-degradation discriminator.
func isUnsupportedOperation(berr *bfschemas.BifrostError) bool {
	return berr != nil && berr.Error != nil && berr.Error.Code != nil &&
		*berr.Error.Code == "unsupported_operation"
}
