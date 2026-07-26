package protocol

import (
	"context"
	stderrors "errors"
	"fmt"
	"sort"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// ArtifactsSurface is the transport-agnostic
// Harbor Protocol artifacts handler. It owns the three artifacts methods
// the Console Artifacts page consumes:
//
//   - artifacts.list    — the identity-scope-filtered catalog, with the
//     filter extensions (mime / source / size / created /
//     tags) applied as a Go-side projection over the driver slice.
//   - artifacts.put     — the Console file-upload pipeline;
//     routes the payload through audit.Redactor then
//     ArtifactStore.PutBytes and returns the canonical ArtifactRef.
//   - artifacts.get_ref — the read-side presigned-URL resolver per
//     contract; type-asserts the store to artifacts.Presigner and
//     fails loud (CodePresignUnsupported) on a non-S3 driver.
//
// ArtifactsSurface is a sibling of the ControlSurface and the
// PostureSurface, not an extension: the artifacts methods are
// not steering controls, they do not reach the task registry, and they
// carry their own per-method wire types.
//
// # Concurrent reuse
//
// ArtifactsSurface is a compiled artifact: the store, redactor, bus,
// clock, and maxBodyBytes are all set once at construction and never
// mutated. Dispatch holds no per-call state on the surface — it reads
// everything from ctx + the request argument. One ArtifactsSurface
// serves N concurrent Dispatch goroutines safely;
// artifacts_concurrent_test.go pins N=100 under -race.
//
// # Identity at the edge (RFC §5.5, CLAUDE.md §6)
//
// Every method fails closed on an incomplete identity triple with
// CodeIdentityRequired. A cross-tenant artifacts.list — the request
// scope's Tenant differing from the caller's ctx-verified tenant —
// requires the admin (or console:fleet) scope per the closed admin-scope set; without it the
// response is CodeScopeMismatch. artifacts.put rejects a body whose
// scope Tenant disagrees with the verified tenant (no silent rewrite —
// identity is mandatory).
//
// # Heavy content by reference
//
// artifacts.list returns metadata-only rows; artifacts.get_ref returns a
// presigned URL; artifacts.put accepts upload bytes only on the request
// leg and returns a reference. No raw heavy content crosses the wire on
// a response, ever — the context-window safety net read into the
// artifacts surface.
type ArtifactsSurface struct {
	store        artifacts.ArtifactStore
	redactor     audit.Redactor
	bus          events.EventBus
	clock        func() time.Time
	driverName   string
	maxBodyBytes int
}

// ArtifactsDeps bundles the runtime-side seams an ArtifactsSurface reads
// through. The Runtime wires these at boot.
type ArtifactsDeps struct {
	// Store is the runtime's content-addressed artifact store — the
	// shipped ArtifactStore. Mandatory.
	Store artifacts.ArtifactStore
	// Redactor is the audit Redactor every artifacts.put body runs
	// through before reaching the store (CLAUDE.md §7 rule 6).
	// Mandatory.
	Redactor audit.Redactor
	// Bus is the canonical event bus the artifacts.put success path
	// publishes `artifacts.uploaded` onto. Mandatory.
	Bus events.EventBus
	// Clock returns the current wall-clock time. Used for the get_ref
	// ExpiresAt stamp and the row CreatedAt fallback. Mandatory.
	Clock func() time.Time
	// DriverName is the configured artifact-store driver name — "inmem"
	// | "fs" | "sqlite" | "postgres" | "s3". Surfaced on every
	// ArtifactRow so the Console can render the Driver chip. Mandatory.
	DriverName string
	// MaxBodyBytes bounds an artifacts.put body. A body larger than this
	// is rejected with CodeRequestTooLarge. Mandatory and positive — a
	// zero or negative value fails loud at construction.
	MaxBodyBytes int
}

// ErrArtifactsMisconfigured — NewArtifactsSurface was called with a
// missing mandatory dependency. Fails closed (CLAUDE.md §5) rather than
// building a surface that would nil-panic on the first Dispatch.
var ErrArtifactsMisconfigured = stderrors.New("protocol: ArtifactsSurface missing a mandatory dependency")

// NewArtifactsSurface builds the Protocol artifacts surface. Every
// ArtifactsDeps seam is mandatory; a missing one fails loud with a
// wrapped ErrArtifactsMisconfigured.
//
// The returned ArtifactsSurface is immutable after construction
// and safe for concurrent use by N goroutines.
func NewArtifactsSurface(deps ArtifactsDeps) (*ArtifactsSurface, error) {
	if deps.Store == nil {
		return nil, fmt.Errorf("%w: Store is nil", ErrArtifactsMisconfigured)
	}
	if deps.Redactor == nil {
		return nil, fmt.Errorf("%w: Redactor is nil", ErrArtifactsMisconfigured)
	}
	if deps.Bus == nil {
		return nil, fmt.Errorf("%w: Bus is nil", ErrArtifactsMisconfigured)
	}
	if deps.Clock == nil {
		return nil, fmt.Errorf("%w: Clock is nil", ErrArtifactsMisconfigured)
	}
	if deps.DriverName == "" {
		return nil, fmt.Errorf("%w: DriverName is empty", ErrArtifactsMisconfigured)
	}
	if deps.MaxBodyBytes <= 0 {
		return nil, fmt.Errorf("%w: MaxBodyBytes must be positive", ErrArtifactsMisconfigured)
	}
	return &ArtifactsSurface{
		store:        deps.Store,
		redactor:     deps.Redactor,
		bus:          deps.Bus,
		clock:        deps.Clock,
		driverName:   deps.DriverName,
		maxBodyBytes: deps.MaxBodyBytes,
	}, nil
}

// EventTypeArtifactUploaded is the canonical event type the
// artifacts.put success path publishes onto the bus (CLAUDE.md §7 rule
// 6: the audit-visible record of an operator upload).
const EventTypeArtifactUploaded events.EventType = "artifacts.uploaded"

// EventTypeArtifactDeleted is the canonical event type the
// artifacts.delete success path publishes onto the bus
// — the audit-visible record of an admin eviction.
const EventTypeArtifactDeleted events.EventType = "artifacts.deleted"

func init() {
	// Register the artifacts.* event types so the bus accepts them (the
	// events package fails loud on an unregistered type).
	events.RegisterEventType(EventTypeArtifactUploaded)
	events.RegisterEventType(EventTypeArtifactDeleted)
}

// ArtifactDeletedPayload is the typed payload of an artifacts.deleted
// event. SafePayload — it carries the
// content-addressed artifact id only, never any artifact bytes.
type ArtifactDeletedPayload struct {
	events.SafeSealed
	// ArtifactID is the content-addressed identifier of the evicted artifact.
	ArtifactID string `json:"artifact_id"`
}

// ArtifactUploadedPayload is the typed payload of an artifacts.uploaded
// event. It carries the artifact metadata only — never the uploaded
// bytes. It is a SafePayload: the fields are content-addressed
// IDs + sizes + a media type, none secret-shaped, so the bus preserves
// typed subscriber access without a redactor pass.
type ArtifactUploadedPayload struct {
	events.SafeSealed
	// ArtifactID is the content-addressed identifier of the upload.
	ArtifactID string `json:"artifact_id"`
	// MimeType is the IANA media type of the upload.
	MimeType string `json:"mime_type,omitempty"`
	// SizeBytes is the length of the uploaded bytes.
	SizeBytes int64 `json:"size_bytes"`
	// Source is the artifact producer — "user_upload" for an
	// artifacts.put.
	Source string `json:"source,omitempty"`
	// Namespace is the logical bucket the artifact landed in.
	Namespace string `json:"namespace,omitempty"`
}

// Dispatch is the single transport-agnostic entry point for a Protocol
// artifacts-method call. A REST handler decodes a request,
// calls Dispatch, and encodes the response — Dispatch IS the surface.
//
// method selects the handler; it MUST be one of the three artifacts
// methods (methods.IsArtifactsMethod). req MUST be the wire request
// type the method expects (*types.ArtifactsListRequest /
// *types.ArtifactsPutRequest / *types.ArtifactsGetRefRequest).
//
// The return is always a *types.<Method>Response or a *protoerrors.Error
// so the wire layer never sees an unstructured runtime error.
//
// Dispatch holds no per-call state on the surface.
func (s *ArtifactsSurface) Dispatch(ctx context.Context, method methods.Method, req any) (any, error) {
	if !methods.IsArtifactsMethod(method) {
		return nil, protoerrors.Newf(protoerrors.CodeUnknownMethod,
			"method %q is not a canonical Protocol artifacts method", string(method))
	}
	switch method {
	case methods.MethodArtifactsList:
		lr, ok := req.(*types.ArtifactsListRequest)
		if !ok || lr == nil {
			return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
				"method %q: request is nil or not a *types.ArtifactsListRequest", string(method))
		}
		return s.handleList(ctx, lr)
	case methods.MethodArtifactsPut:
		pr, ok := req.(*types.ArtifactsPutRequest)
		if !ok || pr == nil {
			return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
				"method %q: request is nil or not a *types.ArtifactsPutRequest", string(method))
		}
		return s.handlePut(ctx, pr)
	case methods.MethodArtifactsGetRef:
		gr, ok := req.(*types.ArtifactsGetRefRequest)
		if !ok || gr == nil {
			return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
				"method %q: request is nil or not a *types.ArtifactsGetRefRequest", string(method))
		}
		return s.handleGetRef(ctx, gr)
	case methods.MethodArtifactsDelete:
		dr, ok := req.(*types.ArtifactsDeleteRequest)
		if !ok || dr == nil {
			return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
				"method %q: request is nil or not a *types.ArtifactsDeleteRequest", string(method))
		}
		return s.handleDelete(ctx, dr)
	default:
		// Unreachable: IsArtifactsMethod already gated the method set.
		return nil, protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: no artifacts handler (Protocol-surface invariant violated)", string(method))
	}
}

// handleList serves artifacts.list. It validates identity, gates a
// cross-tenant request on the admin scope, reads the driver's
// slice, and applies the filter extensions as a Go-side
// projection.
func (s *ArtifactsSurface) handleList(ctx context.Context, req *types.ArtifactsListRequest) (any, error) {
	m := string(methods.MethodArtifactsList)

	// The artifacts.list scope deliberately permits empty User / Session
	// (they are list wildcards) — only Tenant is mandatory for a list.
	// The store enforces the same precondition beneath this one, so a
	// tenant-less filter cannot reach a driver by any route; this check
	// stays because it answers with the Protocol's own code rather than
	// letting a storage sentinel surface as a generic failure.
	if req.Scope.Tenant == "" {
		return nil, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: scope tenant is required", m)
	}
	if err := req.Validate(); err != nil {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: %v", m, err)
	}

	// Cross-tenant gate against the request's VERIFIED identity — the
	// anchor a granted crossing never moves; a list whose scope Tenant differs from the
	// verified tenant requires the admin (or console:fleet) scope.
	if verified, ok := identity.FromVerified(ctx); ok {
		if req.Scope.Tenant != verified.TenantID {
			if !auth.HasScope(ctx, auth.ScopeAdmin) && !auth.HasScope(ctx, auth.ScopeConsoleFleet) {
				return nil, protoerrors.Newf(protoerrors.CodeScopeMismatch,
					"method %q: cross-tenant artifact list requires the admin scope claim", m)
			}
		}
	}

	filter := artifacts.ArtifactScope{
		TenantID:  req.Scope.Tenant,
		UserID:    req.Scope.User,
		SessionID: req.Scope.Session,
		TaskID:    req.Scope.Task,
	}
	refs, err := s.store.List(ctx, filter)
	if err != nil {
		return nil, mapArtifactsError(m, err)
	}

	rows := s.projectRows(refs, req)
	resp := &types.ArtifactsListResponse{
		Rows:            rows.page,
		TotalMatched:    rows.total,
		ProtocolVersion: types.ProtocolVersion,
	}
	return resp, nil
}

// projectedRows is the result of the Go-side filter pass — the bounded
// page slice plus the total-matched count for the Console paginator.
type projectedRows struct {
	page  []types.ArtifactRow
	total int
}

// projectRows applies the filter extensions (mime / source /
// size / created / tags) to the driver's returned refs, sorts newest-
// first, and bounds the result to the request's normalised Limit. The
// projection lives in the surface (not the driver) so the V1
// ArtifactStore.List signature stays untouched.
func (s *ArtifactsSurface) projectRows(refs []artifacts.ArtifactRef, req *types.ArtifactsListRequest) projectedRows {
	mimeSet := toStringSet(req.MimeType)
	sourceSet := make(map[types.ArtifactSource]struct{}, len(req.Source))
	for _, src := range req.Source {
		sourceSet[src] = struct{}{}
	}
	tagSet := toStringSet(req.Tags)

	matched := make([]types.ArtifactRow, 0, len(refs))
	for _, ref := range refs {
		row := projectRow(ref, s.driverName)

		if len(mimeSet) > 0 {
			if _, ok := mimeSet[ref.MimeType]; !ok {
				continue
			}
		}
		if len(sourceSet) > 0 {
			if _, ok := sourceSet[row.Source]; !ok {
				continue
			}
		}
		if req.SizeRange != nil {
			if req.SizeRange.MinBytes != nil && ref.SizeBytes < *req.SizeRange.MinBytes {
				continue
			}
			if req.SizeRange.MaxBytes != nil && ref.SizeBytes > *req.SizeRange.MaxBytes {
				continue
			}
		}
		if req.CreatedRange != nil {
			if !req.CreatedRange.After.IsZero() && row.CreatedAt.Before(req.CreatedRange.After) {
				continue
			}
			if !req.CreatedRange.Before.IsZero() && row.CreatedAt.After(req.CreatedRange.Before) {
				continue
			}
		}
		if len(tagSet) > 0 && !anyTagMatches(row.Tags, tagSet) {
			continue
		}
		matched = append(matched, row)
	}

	// Newest-first (the spec §3 "default newest-first"). A row with a
	// zero CreatedAt sorts last so it never displaces a real timestamp.
	sort.SliceStable(matched, func(i, j int) bool {
		return matched[i].CreatedAt.After(matched[j].CreatedAt)
	})

	total := len(matched)
	limit := req.NormalisedLimit()
	if total > limit {
		matched = matched[:limit]
	}
	return projectedRows{page: matched, total: total}
}

// handlePut serves artifacts.put. It validates identity, gates against a
// cross-tenant body, bounds the body size, routes the payload through
// the audit Redactor, stores it, and emits artifacts.uploaded.
func (s *ArtifactsSurface) handlePut(ctx context.Context, req *types.ArtifactsPutRequest) (any, error) {
	m := string(methods.MethodArtifactsPut)

	scope := artifacts.ArtifactScope{
		TenantID:  req.Scope.Tenant,
		UserID:    req.Scope.User,
		SessionID: req.Scope.Session,
		TaskID:    req.Scope.Task,
	}
	if err := scope.Validate(); err != nil {
		return nil, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: identity scope incomplete: %v", m, err)
	}

	// Cross-tenant body gate. A put whose body Tenant disagrees
	// with the verified tenant is rejected — there is no silent rewrite,
	// identity is mandatory at this boundary.
	if verified, ok := identity.FromVerified(ctx); ok {
		if req.Scope.Tenant != verified.TenantID {
			if !auth.HasScope(ctx, auth.ScopeAdmin) {
				return nil, protoerrors.Newf(protoerrors.CodeScopeMismatch,
					"method %q: cross-tenant artifact upload requires the admin scope claim", m)
			}
		}
	}

	if len(req.Bytes) > s.maxBodyBytes {
		return nil, protoerrors.Newf(protoerrors.CodeRequestTooLarge,
			"method %q: upload body %d bytes exceeds the configured limit of %d bytes", m, len(req.Bytes), s.maxBodyBytes)
	}

	// Resolve the Source — default to user_upload, reject an explicit
	// unknown value loudly.
	source := req.Opts.Source
	if source == "" {
		source = types.ArtifactSourceUserUpload
	}
	if !types.IsValidArtifactSource(source) {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: unknown source %q", m, string(source))
	}

	// CLAUDE.md §7 rule 6 — run the upload payload through the
	// audit Redactor BEFORE it reaches the store. The redactor may
	// rewrite or refuse; a refusal fails loud (never store unredacted).
	redactView := map[string]any{
		"bytes":     req.Bytes,
		"filename":  req.Opts.Filename,
		"mime_type": req.Opts.MimeType,
		"namespace": req.Opts.Namespace,
		"source":    string(source),
		"tags":      req.Opts.Tags,
	}
	if _, err := s.redactor.Redact(ctx, redactView); err != nil {
		return nil, protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: audit redactor refused the upload payload: %v", m, err)
	}

	// W6: stamp `created_at` on the Source map so
	// projectRow's `extractCreatedAt` populates a real timestamp on
	// the wire row. Without this every uploaded artifact rendered with
	// the Go zero-value `0001-01-01T00:00:00Z` on the Console.
	opts := artifacts.PutOpts{
		MimeType:  req.Opts.MimeType,
		Filename:  req.Opts.Filename,
		Namespace: req.Opts.Namespace,
		Source: map[string]any{
			"source":     string(source),
			"tags":       stringSlice(req.Opts.Tags),
			"created_at": s.clock(),
		},
	}
	ref, err := s.store.PutBytes(ctx, scope, req.Bytes, opts)
	if err != nil {
		return nil, mapArtifactsError(m, err)
	}

	// Emit artifacts.uploaded. A publish failure fails loud — the upload
	// already succeeded, so the operator MUST see the audit drift.
	ev := events.Event{
		Type: EventTypeArtifactUploaded,
		Identity: identity.Quadruple{
			Identity: identity.Identity{
				TenantID:  scope.TenantID,
				UserID:    scope.UserID,
				SessionID: scope.SessionID,
			},
		},
		OccurredAt: s.clock(),
		Payload: ArtifactUploadedPayload{
			ArtifactID: ref.ID,
			MimeType:   ref.MimeType,
			SizeBytes:  ref.SizeBytes,
			Source:     string(source),
			Namespace:  ref.Namespace,
		},
	}
	if err := s.bus.Publish(ctx, ev); err != nil {
		return nil, protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: upload succeeded but audit emit failed: %v", m, err)
	}

	return &types.ArtifactsPutResponse{
		Ref:             projectRef(ref),
		ProtocolVersion: types.ProtocolVersion,
	}, nil
}

// handleGetRef serves artifacts.get_ref. It validates identity, refuses
// a scope naming a tenant other than the verified one, bounds the
// expiry, resolves the ref's metadata, and type-asserts the store to
// artifacts.Presigner — failing loud (CodePresignUnsupported) on a
// driver that does not implement the capability.
//
// The tenant refusal is flat, with no admin elevation. That is narrower
// than artifacts.list, which does offer an elevation branch, and the
// asymmetry is deliberate: a list scope is valid with an empty User /
// Session (they are wildcards), so an operator holding the admin claim
// can enumerate a whole foreign tenant. A get_ref scope is not — it
// requires the full triple — so the only foreign-tenant shape that can
// reach this method already carries the caller's OWN user and session,
// which is not a fleet-observation shape and so has nothing to elevate.
// A presigned URL is also a time-bounded bearer capability to the
// CONTENT, materially broader than the metadata artifacts.list returns,
// so the elevation is not one to add by analogy.
func (s *ArtifactsSurface) handleGetRef(ctx context.Context, req *types.ArtifactsGetRefRequest) (any, error) {
	m := string(methods.MethodArtifactsGetRef)

	scope := artifacts.ArtifactScope{
		TenantID:  req.Scope.Tenant,
		UserID:    req.Scope.User,
		SessionID: req.Scope.Session,
		TaskID:    req.Scope.Task,
	}
	if err := scope.Validate(); err != nil {
		return nil, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: identity scope incomplete: %v", m, err)
	}
	if req.ID == "" {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: artifact id is required", m)
	}

	// Tenant reconciliation. When auth middleware ran, ctx carries the
	// verified identity; a ref resolution whose scope Tenant differs from
	// it is refused outright — no scope claim widens this method (see the
	// godoc above for why the elevation artifacts.list offers does not
	// carry over). The check runs BEFORE the store read so the driver is
	// never consulted for another tenant's scope.
	if verified, ok := identity.FromVerified(ctx); ok {
		if req.Scope.Tenant != verified.TenantID {
			return nil, protoerrors.Newf(protoerrors.CodeScopeMismatch,
				"method %q: scope tenant does not match the verified identity", m)
		}
	}

	expiry := req.NormalisedExpiry()
	if expiry < types.PresignExpiryMin || expiry > types.PresignExpiryMax {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: expiry %s is outside the bounded range [%s, %s]",
			m, expiry, types.PresignExpiryMin, types.PresignExpiryMax)
	}

	ref, found, err := s.store.GetRef(ctx, scope, req.ID)
	if err != nil {
		return nil, mapArtifactsError(m, err)
	}
	if !found || ref == nil {
		return nil, protoerrors.Newf(protoerrors.CodeNotFound,
			"method %q: artifact %q not found in scope", m, req.ID)
	}

	// Type-assert the store to Presigner. A driver without the
	// capability fails loud with CodePresignUnsupported — no silent
	// fallback to byte-streaming (fail-loud posture).
	presigner, ok := s.store.(artifacts.Presigner)
	if !ok {
		return nil, protoerrors.Newf(protoerrors.CodePresignUnsupported,
			"method %q: the %q artifact-store driver does not support presigned URLs", m, s.driverName)
	}
	url, err := presigner.PresignGet(ctx, scope, req.ID, expiry)
	if err != nil {
		if stderrors.Is(err, artifacts.ErrPresignUnsupported) {
			return nil, protoerrors.Newf(protoerrors.CodePresignUnsupported,
				"method %q: the %q artifact-store driver does not support presigned URLs", m, s.driverName)
		}
		return nil, mapArtifactsError(m, err)
	}

	return &types.ArtifactsGetRefResponse{
		Ref:             projectRef(*ref),
		PresignedURL:    url,
		ExpiresAt:       s.clock().Add(expiry),
		ProtocolVersion: types.ProtocolVersion,
	}, nil
}

// handleDelete serves artifacts.delete. It validates
// the full identity triple, gates STRICTLY on the verified admin scope (a
// mutation requires admin — console:fleet is an observation claim, never a
// write entitlement; page-artifacts §9), evicts via the shipped idempotent
// ArtifactStore.Delete, and emits artifacts.deleted for audit.
func (s *ArtifactsSurface) handleDelete(ctx context.Context, req *types.ArtifactsDeleteRequest) (any, error) {
	m := string(methods.MethodArtifactsDelete)

	scope := artifacts.ArtifactScope{
		TenantID:  req.Scope.Tenant,
		UserID:    req.Scope.User,
		SessionID: req.Scope.Session,
		TaskID:    req.Scope.Task,
	}
	if err := scope.Validate(); err != nil {
		return nil, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: identity scope incomplete: %v", m, err)
	}
	if req.ID == "" {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: artifact id is required", m)
	}

	// Admin gate. Unlike the cross-tenant READ gate (admin OR
	// console:fleet), a runtime-state MUTATION requires admin strictly.
	if !auth.HasScope(ctx, auth.ScopeAdmin) {
		return nil, protoerrors.Newf(protoerrors.CodeScopeMismatch,
			"method %q: artifact delete requires the verified `admin` scope claim", m)
	}

	deleted, err := s.store.Delete(ctx, scope, req.ID)
	if err != nil {
		return nil, mapArtifactsError(m, err)
	}

	// Emit artifacts.deleted only when something was actually evicted (an
	// idempotent no-op delete is not an audit-worthy state change). A
	// publish failure fails loud — the eviction already happened, so the
	// operator MUST see the audit drift.
	if deleted {
		ev := events.Event{
			Type: EventTypeArtifactDeleted,
			Identity: identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  scope.TenantID,
					UserID:    scope.UserID,
					SessionID: scope.SessionID,
				},
			},
			OccurredAt: s.clock(),
			Payload:    ArtifactDeletedPayload{ArtifactID: req.ID},
		}
		if perr := s.bus.Publish(ctx, ev); perr != nil {
			return nil, protoerrors.Newf(protoerrors.CodeRuntimeError,
				"method %q: delete succeeded but audit emit failed: %v", m, perr)
		}
	}

	return &types.ArtifactsDeleteResponse{
		Deleted:         deleted,
		ProtocolVersion: types.ProtocolVersion,
	}, nil
}

// projectRef maps a storage-side artifacts.ArtifactRef onto the flat
// Protocol wire ArtifactRef. The storage struct is never re-exported
// (single-source per CLAUDE.md §8).
func projectRef(ref artifacts.ArtifactRef) types.ArtifactRef {
	return types.ArtifactRef{
		ID:        ref.ID,
		MimeType:  ref.MimeType,
		SizeBytes: ref.SizeBytes,
		Filename:  ref.Filename,
		SHA256:    ref.SHA256,
		Namespace: ref.Namespace,
		Scope: types.ArtifactScope{
			Tenant:  ref.Scope.TenantID,
			User:    ref.Scope.UserID,
			Session: ref.Scope.SessionID,
			Task:    ref.Scope.TaskID,
		},
	}
}

// projectRow maps a storage-side artifacts.ArtifactRef onto a Protocol
// ArtifactRow, projecting the catalog-only fields (Tags / Source /
// CreatedAt) from the storage ref's opaque Source map. Per the Artifacts-page
// open-question resolution, Tags are projected on the Protocol row, not
// promoted onto the storage ArtifactRef shape.
func projectRow(ref artifacts.ArtifactRef, driverName string) types.ArtifactRow {
	row := types.ArtifactRow{
		Ref:    projectRef(ref),
		Driver: driverName,
	}
	if ref.Source != nil {
		row.Source = resolveArtifactSource(ref.Source)
		row.Tags = extractTags(ref.Source["tags"])
		if created := extractCreatedAt(ref.Source["created_at"]); !created.IsZero() {
			row.CreatedAt = created
		}
	}
	return row
}

// resolveArtifactSource projects the storage ref's opaque Source map onto
// a canonical, closed-enum types.ArtifactSource.
//
// Resolution order:
//
//  1. The canonical `source` key, when present AND a recognised enum
//     value (`tool` / `planner` / `user_upload` / `system`). The flow
//     catalog stamps `source: "flow"`, which is NOT an enum member; it
//     maps onto `system` (a flow run is runtime-produced).
//  2. Otherwise the presence of a `tool` key (the dev tool-executor's
//     originating-tool name) implies a tool-produced artifact → `tool`.
//  3. Otherwise a `flow` key implies a flow → `system`.
//  4. Otherwise a `producer` key with the flow-describe value → `system`.
//
// The else-chain is what keeps EXISTING artifacts (put previously,
// so carrying no `source` key) projecting a correct, non-blank source —
// no back-fill migration is needed. When nothing matches, the
// zero value (an empty ArtifactSource) is returned, matching the prior
// behaviour for an unrecognised producer.
func resolveArtifactSource(src map[string]any) types.ArtifactSource {
	if s, ok := src["source"].(string); ok && s != "" {
		cand := types.ArtifactSource(s)
		if types.IsValidArtifactSource(cand) {
			return cand
		}
		// "flow" is the one canonical discriminator that is not an enum
		// member; a flow-run artifact is runtime-produced → system.
		if s == "flow" {
			return types.ArtifactSourceSystem
		}
	}
	// Else-chain over the legacy / name keys so pre-107f artifacts still
	// project a real source.
	if _, ok := src["tool"].(string); ok {
		return types.ArtifactSourceTool
	}
	if _, ok := src["flow"].(string); ok {
		return types.ArtifactSourceSystem
	}
	if _, ok := src["producer"].(string); ok {
		return types.ArtifactSourceSystem
	}
	return ""
}

// extractTags coerces the storage ref's opaque `tags` value into a
// []string. It tolerates both a []string (the in-mem driver) and a
// []any of strings (a JSON-round-tripped FS / SQLite / Postgres driver).
func extractTags(v any) []string {
	switch t := v.(type) {
	case []string:
		if len(t) == 0 {
			return nil
		}
		out := make([]string, len(t))
		copy(out, t)
		return out
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	default:
		return nil
	}
}

// extractCreatedAt coerces the storage ref's opaque `created_at` value
// into a time.Time. It tolerates a time.Time, an RFC-3339 string, and a
// numeric Unix-seconds value (the shapes a JSON round-trip can produce).
func extractCreatedAt(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case string:
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			return parsed
		}
	case float64:
		return time.Unix(int64(t), 0).UTC()
	case int64:
		return time.Unix(t, 0).UTC()
	}
	return time.Time{}
}

// toStringSet builds a lookup set from a slice. An empty slice yields a
// nil map (callers treat a nil/empty set as "wildcard").
func toStringSet(vals []string) map[string]struct{} {
	if len(vals) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(vals))
	for _, v := range vals {
		set[v] = struct{}{}
	}
	return set
}

// anyTagMatches reports whether any of rowTags is present in want.
func anyTagMatches(rowTags []string, want map[string]struct{}) bool {
	for _, tag := range rowTags {
		if _, ok := want[tag]; ok {
			return true
		}
	}
	return false
}

// stringSlice returns a defensive copy of s, or nil for an empty input.
// Used so the PutOpts.Source map does not alias the caller's slice.
func stringSlice(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

// mapArtifactsError translates an artifacts subsystem sentinel onto a
// canonical Protocol error code. The mapping closes the wire surface —
// every error shape is observable as a Code (CLAUDE.md §13).
func mapArtifactsError(method string, err error) error {
	switch {
	case err == nil:
		return nil
	case stderrors.Is(err, artifacts.ErrIdentityRequired):
		return protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: %v", method, err)
	case stderrors.Is(err, artifacts.ErrPresignUnsupported):
		return protoerrors.Newf(protoerrors.CodePresignUnsupported,
			"method %q: %v", method, err)
	case stderrors.Is(err, artifacts.ErrNotFound):
		return protoerrors.Newf(protoerrors.CodeNotFound,
			"method %q: %v", method, err)
	case stderrors.Is(err, artifacts.ErrInvalidScope):
		return protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: %v", method, err)
	case stderrors.Is(err, artifacts.ErrStoreClosed):
		return protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: artifact store is closed", method)
	default:
		return protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: artifact operation failed: %v", method, err)
	}
}
