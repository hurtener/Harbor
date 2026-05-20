package types

import (
	"encoding/json"
	"fmt"
	"time"
)

// Phase 73l (Wave 13 / D-120) — the artifacts-page Protocol wire types.
//
// Phase 73l ships three Protocol methods over the runtime's content-
// addressed artifact store (Phases 17–19, Shipped):
//
//   - artifacts.list    — the identity-scope-filtered catalog, with the
//     Phase 73l filter extensions (mime / source / size / created /
//     tags) applied as a Go-side projection over the driver's slice.
//   - artifacts.put     — the Console (and Playground) file-upload
//     pipeline per Brief 11 §PG-2; routes through audit.Redactor then
//     ArtifactStore.PutBytes and returns the canonical ArtifactRef.
//   - artifacts.get_ref — the read-side presigned-URL resolver per
//     D-022 / D-026; type-asserts the store to artifacts.Presigner.
//
// Every type here is a flat, Protocol-owned struct — never a re-export
// of internal/artifacts Go types (RFC §5.1 reject-on-sight smell). In
// particular, heavy bytes NEVER travel inline through these wire types:
// artifacts.list returns metadata-only rows, artifacts.get_ref returns a
// presigned URL, and artifacts.put accepts the upload bytes only on the
// request leg (the response is a reference). This is the D-026 context-
// window safety net read into the Protocol surface.

// ArtifactSource is the closed enum of artifact producers. A free-text
// field would invite drift; the enum can be extended via a future RFC
// PR. Deserialisation of an unknown source returns CodeInvalidRequest
// loudly (the artifacts surface fails loud per CLAUDE.md §13) — there is
// no silent "unknown" bucket.
type ArtifactSource string

// Canonical ArtifactSource values. The set is closed.
const (
	// ArtifactSourceTool — the artifact was produced by a tool call.
	ArtifactSourceTool ArtifactSource = "tool"
	// ArtifactSourcePlanner — the artifact was produced by a planner
	// decision.
	ArtifactSourcePlanner ArtifactSource = "planner"
	// ArtifactSourceUserUpload — the artifact was uploaded by an operator
	// through the Console (or Playground) file-upload pipeline. This is
	// the default Source for an artifacts.put with no explicit Source.
	ArtifactSourceUserUpload ArtifactSource = "user_upload"
	// ArtifactSourceSystem — the artifact was produced by the runtime
	// itself (a generated report, a system dump).
	ArtifactSourceSystem ArtifactSource = "system"
)

// IsValidArtifactSource reports whether s is one of the four canonical
// artifact sources. The artifacts handler uses this to fail loud on an
// unknown source value rather than silently dropping the filter.
func IsValidArtifactSource(s ArtifactSource) bool {
	switch s {
	case ArtifactSourceTool, ArtifactSourcePlanner, ArtifactSourceUserUpload, ArtifactSourceSystem:
		return true
	}
	return false
}

// ArtifactScope is the flat wire identity an artifacts-method request
// carries. It mirrors the four-field internal/artifacts.ArtifactScope
// shape — `(tenant, user, session, task)` — because the artifact store
// keys artifacts on tasks (RFC §6.10), but it is a Protocol-owned wire
// type, never a re-export.
//
// Identity is mandatory: Tenant / User / Session must be non-empty for
// artifacts.put and artifacts.get_ref. Task is optional for session-
// scoped artifacts. For artifacts.list, empty fields are wildcards
// (tenant-wide listing requires the admin scope per D-079).
type ArtifactScope struct {
	// Tenant / User / Session are the mandatory isolation triple. An
	// empty component fails put / get_ref closed at the Protocol edge.
	Tenant  string `json:"tenant"`
	User    string `json:"user"`
	Session string `json:"session"`
	// Task is the per-task scope inside a session. Optional — empty for
	// session-scoped artifacts; a list filter treats an empty Task as a
	// wildcard.
	Task string `json:"task,omitempty"`
}

// SizeRange is an optional byte-size filter for artifacts.list. Both
// bounds are inclusive; a nil bound is unbounded on that side.
type SizeRange struct {
	// MinBytes, when set, excludes artifacts strictly smaller than it.
	MinBytes *int64 `json:"min_bytes,omitempty"`
	// MaxBytes, when set, excludes artifacts strictly larger than it.
	MaxBytes *int64 `json:"max_bytes,omitempty"`
}

// TimeRange is an optional created-at filter for artifacts.list. Both
// bounds are inclusive; a zero bound is unbounded on that side.
type TimeRange struct {
	// After, when non-zero, excludes artifacts created strictly before
	// it.
	After time.Time `json:"after,omitempty"`
	// Before, when non-zero, excludes artifacts created strictly after
	// it.
	Before time.Time `json:"before,omitempty"`
}

// ArtifactRef is the flat Protocol projection of the storage-side
// internal/artifacts.ArtifactRef. It carries the catalog-rendering
// metadata a Console row needs — never the artifact bytes (D-026).
type ArtifactRef struct {
	// ID is the content-addressed identifier
	// (`{namespace}_{sha256[:12]}`).
	ID string `json:"id"`
	// MimeType is the IANA media type, when known.
	MimeType string `json:"mime_type,omitempty"`
	// SizeBytes is the length of the referenced bytes.
	SizeBytes int64 `json:"size_bytes"`
	// Filename is metadata only — never used for path construction.
	Filename string `json:"filename,omitempty"`
	// SHA256 is the full hex digest of the referenced bytes.
	SHA256 string `json:"sha256,omitempty"`
	// Namespace is the logical bucket the artifact lives in.
	Namespace string `json:"namespace,omitempty"`
	// Scope is the artifact's owning identity scope.
	Scope ArtifactScope `json:"scope"`
}

// ArtifactRow is the artifacts.list row shape. It wraps the canonical
// ArtifactRef with the catalog-only fields the Console table renders —
// Tags, the storage Driver name, and the creation timestamp. ArtifactRow
// is deliberately distinct from ArtifactRef so the Protocol's wire
// surface stays independent of the storage shape.
type ArtifactRow struct {
	// Ref is the canonical artifact reference.
	Ref ArtifactRef `json:"ref"`
	// Tags is the chip list assigned by the producing planner / tool.
	// Sourced from the storage-side `Source["tags"]` projection — never
	// promoted onto the storage ArtifactRef shape (D-120 open-question
	// resolution: project on the Protocol row, not the storage struct).
	Tags []string `json:"tags,omitempty"`
	// Source is the producer of the artifact — one of the four canonical
	// ArtifactSource values.
	Source ArtifactSource `json:"source,omitempty"`
	// Driver is the artifact store driver that holds this artifact —
	// "inmem" | "fs" | "sqlite" | "postgres" | "s3".
	Driver string `json:"driver,omitempty"`
	// CreatedAt is the artifact's put timestamp.
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// ArtifactsListRequest is the wire request for the artifacts.list
// Protocol method. The Phase 73l filter extensions — MimeType, Source,
// SizeRange, CreatedRange, Tags — are all optional; an empty field is a
// wildcard. The handler applies them as a Go-side projection over the
// driver's returned slice (the V1 ArtifactStore.List signature is not
// extended — driver conformance stays untouched, D-120).
type ArtifactsListRequest struct {
	// Scope is the caller's identity scope. A list filter treats empty
	// fields as wildcards; a Tenant differing from the caller's verified
	// tenant requires the admin scope per D-079.
	Scope ArtifactScope `json:"scope"`
	// MimeType is an OR-set of IANA media types. Empty == wildcard.
	MimeType []string `json:"mime_type,omitempty"`
	// Source is an OR-set of artifact producers. Empty == wildcard. An
	// unknown source value fails the request with CodeInvalidRequest.
	Source []ArtifactSource `json:"source,omitempty"`
	// SizeRange, when set, filters by stored byte size.
	SizeRange *SizeRange `json:"size_range,omitempty"`
	// CreatedRange, when set, filters by put timestamp.
	CreatedRange *TimeRange `json:"created_range,omitempty"`
	// Tags is an OR-set of tag strings. An artifact matches when it
	// carries at least one of the listed tags. Empty == wildcard.
	Tags []string `json:"tags,omitempty"`
	// Limit bounds the returned page. Defaults to DefaultArtifactsLimit
	// (100) when zero; values above MaxArtifactsLimit (1000) are clamped.
	Limit int `json:"limit,omitempty"`
}

// Default + maximum page bounds for artifacts.list.
const (
	// DefaultArtifactsLimit is the page size applied when a request omits
	// Limit.
	DefaultArtifactsLimit = 100
	// MaxArtifactsLimit is the hard ceiling on an artifacts.list page.
	MaxArtifactsLimit = 1000
)

// ArtifactsListResponse is the wire response for artifacts.list. Rows is
// the metadata-only page slice — never artifact bytes (D-026).
type ArtifactsListResponse struct {
	// Rows is the page slice, at most Limit rows. Empty when the filter
	// matched nothing.
	Rows []ArtifactRow `json:"rows"`
	// TotalMatched is the count of rows matching the filter before the
	// Limit truncation — the Console paginator renders "N of M".
	TotalMatched int `json:"total_matched"`
	// ProtocolVersion echoes the Protocol version the Runtime answered
	// under so a client can detect a version skew.
	ProtocolVersion string `json:"protocol_version"`
}

// ArtifactsPutOpts carries the optional upload metadata an artifacts.put
// request supplies. It mirrors the catalog-relevant subset of the
// storage-side internal/artifacts.PutOpts.
type ArtifactsPutOpts struct {
	// MimeType is the IANA media type of the upload. Empty defaults to
	// the store's own default (text/plain for PutText-shaped bytes).
	MimeType string `json:"mime_type,omitempty"`
	// Filename is metadata only — never used for path construction.
	Filename string `json:"filename,omitempty"`
	// Namespace is the logical bucket the artifact lands in. Empty
	// defaults to "default" at the store.
	Namespace string `json:"namespace,omitempty"`
	// Source is the artifact producer. Defaults to ArtifactSourceUserUpload
	// when empty (an artifacts.put IS, by construction, a user upload).
	// An explicit unknown value fails the request with
	// CodeInvalidRequest.
	Source ArtifactSource `json:"source,omitempty"`
	// Tags is the chip list to assign to the artifact.
	Tags []string `json:"tags,omitempty"`
}

// ArtifactsPutRequest is the wire request for the artifacts.put Protocol
// method (Brief 11 §PG-2). The upload bytes travel inline on the request
// leg only — the response is a reference, never an echo of the body.
// Body size is bounded by config.ProtocolConfig.MaxRequestBytes; an
// oversize body is rejected with CodeRequestTooLarge.
type ArtifactsPutRequest struct {
	// Scope is the caller's identity scope. Tenant / User / Session are
	// mandatory; a body whose Tenant disagrees with the caller's
	// verified tenant is rejected with CodeScopeMismatch.
	Scope ArtifactScope `json:"scope"`
	// Bytes is the artifact payload. JSON-encoded as a base64 string by
	// the standard []byte marshaller.
	Bytes []byte `json:"bytes"`
	// Opts is the optional upload metadata.
	Opts ArtifactsPutOpts `json:"opts,omitempty"`
}

// ArtifactsPutResponse is the wire response for artifacts.put. It
// carries the canonical ArtifactRef the store minted — never the
// uploaded bytes (D-026).
type ArtifactsPutResponse struct {
	// Ref is the content-addressed reference to the stored artifact.
	Ref ArtifactRef `json:"ref"`
	// ProtocolVersion echoes the Protocol version.
	ProtocolVersion string `json:"protocol_version"`
}

// ArtifactsGetRefRequest is the wire request for the artifacts.get_ref
// Protocol method — the read-side presigned-URL resolver. Expiry is
// bounded [PresignExpiryMin, PresignExpiryMax]; an out-of-range expiry
// is rejected with CodeInvalidRequest.
type ArtifactsGetRefRequest struct {
	// Scope is the caller's identity scope. Tenant / User / Session are
	// mandatory.
	Scope ArtifactScope `json:"scope"`
	// ID is the content-addressed artifact identifier to resolve.
	ID string `json:"id"`
	// Expiry is the presigned-URL validity window. Defaults to
	// DefaultPresignExpiry (15m) when zero; values outside
	// [PresignExpiryMin, PresignExpiryMax] are rejected loudly.
	Expiry time.Duration `json:"expiry,omitempty"`
}

// Presigned-URL expiry bounds for artifacts.get_ref, matching the
// internal/artifacts.Presigner interface contract (S3's documented
// limit).
const (
	// PresignExpiryMin is the floor on a presigned-URL expiry window.
	PresignExpiryMin = time.Minute
	// PresignExpiryMax is the ceiling on a presigned-URL expiry window
	// (7 days — S3's documented limit).
	PresignExpiryMax = 7 * 24 * time.Hour
	// DefaultPresignExpiry is the expiry applied when a request omits it.
	DefaultPresignExpiry = 15 * time.Minute
)

// ArtifactsGetRefResponse is the wire response for artifacts.get_ref. It
// carries the time-bounded presigned URL plus the artifact metadata —
// the Console's Preview / Download / Share all consume this single
// shape per D-022 / D-026.
type ArtifactsGetRefResponse struct {
	// Ref is the artifact metadata reference.
	Ref ArtifactRef `json:"ref"`
	// PresignedURL is the time-bounded HTTPS URL the Console downloads
	// the bytes from directly, bypassing the runtime's bytes path.
	PresignedURL string `json:"presigned_url"`
	// ExpiresAt is the wall-clock instant the presigned URL stops being
	// valid.
	ExpiresAt time.Time `json:"expires_at"`
	// ProtocolVersion echoes the Protocol version.
	ProtocolVersion string `json:"protocol_version"`
}

// NormalisedLimit returns the request's Limit clamped to
// [1, MaxArtifactsLimit], applying DefaultArtifactsLimit when the
// request omitted it. The handler calls this so a client that omits
// Limit gets the documented default and one that over-asks is bounded.
func (r ArtifactsListRequest) NormalisedLimit() int {
	switch {
	case r.Limit <= 0:
		return DefaultArtifactsLimit
	case r.Limit > MaxArtifactsLimit:
		return MaxArtifactsLimit
	default:
		return r.Limit
	}
}

// Validate checks the artifacts.list filter for an unknown ArtifactSource
// value, failing loud (CLAUDE.md §13) rather than silently dropping it.
// Returns an error naming the offending value when one is found.
func (r ArtifactsListRequest) Validate() error {
	for _, s := range r.Source {
		if !IsValidArtifactSource(s) {
			return fmt.Errorf("artifacts.list: unknown source %q", string(s))
		}
	}
	return nil
}

// NormalisedExpiry returns the request's Expiry, applying
// DefaultPresignExpiry when the request omitted it. The handler
// separately bounds-checks against [PresignExpiryMin, PresignExpiryMax]
// so it can return CodeInvalidRequest rather than silently clamping.
func (r ArtifactsGetRefRequest) NormalisedExpiry() time.Duration {
	if r.Expiry <= 0 {
		return DefaultPresignExpiry
	}
	return r.Expiry
}

// MarshalJSON is the standard json.Marshaler kept explicit so the
// ArtifactsListResponse shape stays stable across a field-reorder
// refactor. Round-trip is verified by artifacts_test.go.
func (r ArtifactsListResponse) MarshalJSON() ([]byte, error) {
	type alias ArtifactsListResponse
	return json.Marshal(alias(r))
}
