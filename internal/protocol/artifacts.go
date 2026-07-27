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
// Harbor Protocol artifacts handler. It owns the five artifacts methods
// the Console Artifacts page and the MCP-Apps host consume:
//
//   - artifacts.list    — the identity-scope-filtered catalog, with the
//     filter extensions (mime / source / size / created /
//     tags) applied as a Go-side projection over the driver slice.
//   - artifacts.put     — the Console file-upload pipeline; submits the
//     payload to audit.Redactor as an ADMISSION GATE (a refusal
//     refuses the upload; the redactor's rewrite is not stored), then
//     stores the bytes AS AUTHORED via ArtifactStore.PutBytes and
//     returns the canonical ArtifactRef.
//   - artifacts.get     — the driver-independent byte read; resolves
//     through ArtifactStore.Get, so every registered driver serves it.
//   - artifacts.get_ref — the read-side presigned-URL resolver per
//     contract; type-asserts the store to artifacts.Presigner and
//     fails loud (CodePresignUnsupported) on a driver without it.
//   - artifacts.delete  — the admin-gated, audited eviction.
//
// ArtifactsSurface is a sibling of the ControlSurface and the
// PostureSurface, not an extension: the artifacts methods are
// not steering controls, they do not reach the task registry, and they
// carry their own per-method wire types.
//
// # Two reads, one contract and one optimisation
//
// artifacts.get and artifacts.get_ref are NOT parallel implementations
// of one feature. artifacts.get is the CONTRACT — universal and
// driver-independent, because ArtifactStore.Get is a mandatory interface
// method. artifacts.get_ref is a TRANSPORT OPTIMISATION for the case
// where the store can hand bytes off its own edge, so a large media
// download need not transit the Runtime; it rests on the optional
// artifacts.Presigner capability, which exactly one shipped driver
// implements and which the default driver does not.
//
// Both resolve the same ref, under the same verified identity, under the
// same flat body-identity posture. They differ in WHO SERVES THE BYTES,
// not in who may read them — which is what makes keeping both
// defensible, and why a client that cannot presign now has a path
// rather than a refusal.
//
// # Concurrent reuse
//
// ArtifactsSurface is a compiled artifact: the store, redactor, bus,
// clock, driver name, body bound and fetch ceiling are all set once at
// construction and never mutated. Dispatch holds no per-call state on
// the surface — it reads everything from ctx + the request argument.
// One ArtifactsSurface serves N concurrent Dispatch goroutines safely;
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
// identity is mandatory). artifacts.get and artifacts.get_ref refuse a
// foreign tenant FLAT, with no elevation branch: both serve CONTENT,
// which is materially broader than the metadata a listing returns.
//
// # Where the bytes travel
//
// artifacts.list returns metadata-only rows; artifacts.get_ref returns a
// presigned URL; artifacts.put accepts upload bytes only on the request
// leg and returns a reference. artifacts.get is the one method here that
// returns stored bytes, and it returns them to the caller's own verified
// identity, bounded by the operator's fetch ceiling and truthful about
// that bound.
//
// That does not relax the context-window safety net. The net governs
// what reaches a MODEL'S CONTEXT — a heavy tool result is offloaded so
// the event stream and the prompt do not carry it — and an explicit
// read-back by the principal that already reaches those bytes is what
// the offload exists to make possible. The response is placed nowhere
// the redactor governs: not an event payload, not a trajectory entry,
// not a log line.
type ArtifactsSurface struct {
	store        artifacts.ArtifactStore
	redactor     audit.Redactor
	bus          events.EventBus
	clock        func() time.Time
	driverName   string
	maxBodyBytes int
	// fetchDefaultMaxBytes is the window served when an artifacts.get
	// names no bound of its own; fetchHardMaxBytes is the ceiling a
	// caller's own bound is clamped to. Both are operator policy,
	// resolved at construction, and immutable thereafter.
	fetchDefaultMaxBytes int64
	fetchHardMaxBytes    int64
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
	// FetchDefaultMaxBytes is the artifacts.get window served when the
	// caller names no bound. Mandatory and positive; it comes from the
	// operator's resolved artifacts configuration.
	FetchDefaultMaxBytes int
	// FetchHardMaxBytes is the ceiling an artifacts.get caller's own
	// bound is clamped to. Mandatory, positive, and at least
	// FetchDefaultMaxBytes — a default above its own ceiling is a
	// misconfiguration this constructor refuses rather than silently
	// reorders.
	FetchHardMaxBytes int
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
	if deps.FetchDefaultMaxBytes <= 0 {
		return nil, fmt.Errorf("%w: FetchDefaultMaxBytes must be positive", ErrArtifactsMisconfigured)
	}
	if deps.FetchHardMaxBytes <= 0 {
		return nil, fmt.Errorf("%w: FetchHardMaxBytes must be positive", ErrArtifactsMisconfigured)
	}
	if deps.FetchDefaultMaxBytes > deps.FetchHardMaxBytes {
		return nil, fmt.Errorf("%w: FetchDefaultMaxBytes (%d) must not exceed FetchHardMaxBytes (%d)",
			ErrArtifactsMisconfigured, deps.FetchDefaultMaxBytes, deps.FetchHardMaxBytes)
	}
	return &ArtifactsSurface{
		store:                deps.Store,
		redactor:             deps.Redactor,
		bus:                  deps.Bus,
		clock:                deps.Clock,
		driverName:           deps.DriverName,
		maxBodyBytes:         deps.MaxBodyBytes,
		fetchDefaultMaxBytes: int64(deps.FetchDefaultMaxBytes),
		fetchHardMaxBytes:    int64(deps.FetchHardMaxBytes),
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
// method selects the handler; it MUST be one of the five artifacts
// methods (methods.IsArtifactsMethod). req MUST be the wire request
// type the method expects (*types.ArtifactsListRequest /
// *types.ArtifactsPutRequest / *types.ArtifactsGetRequest /
// *types.ArtifactsGetRefRequest / *types.ArtifactsDeleteRequest).
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
	case methods.MethodArtifactsGet:
		g, ok := req.(*types.ArtifactsGetRequest)
		if !ok || g == nil {
			return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
				"method %q: request is nil or not a *types.ArtifactsGetRequest", string(method))
		}
		return s.handleGet(ctx, g)
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

// handleList serves artifacts.list. It validates identity, holds the
// listing to the caller's own user unless an administrative claim
// widens it, reads the driver's slice, and applies the filter
// extensions as a Go-side projection.
//
// # The listing's identity bound
//
// A listing is scoped to the caller's own user. Widening past that —
// naming another user, or asking for every user in the tenant — takes
// the admin or console:fleet claim, exactly as the events listing
// requires for the same widening. The two axes read differently on
// purpose:
//
//   - USER is an isolation principal, so an elided user folds to the
//     caller's own rather than fanning across the tenant. A listing row
//     carries the owning user id, session id, and content digest, and
//     those are the caller's to see only for the caller's own artifacts.
//   - SESSION is NOT an isolation boundary within one user, so it stays
//     the list wildcard it has always been: an elided session means
//     "every session of mine", and naming one of my own sessions needs
//     no claim. The events listing draws the same line — its user axis
//     elevates on a foreign value, its session axis does not — and the
//     everyday "show me my artifacts" flow depends on the wildcard.
//
// A ctx with no verified identity (in-process embedding, a background
// worker rooted outside any request) has no anchor to reconcile
// against and is left unrestricted, matching the identity package's
// documented posture and the cross-tenant gate directly below.
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

	scopeUser := req.Scope.User

	// Identity gates against the request's VERIFIED identity — the anchor
	// a granted crossing never moves.
	if verified, ok := identity.FromVerified(ctx); ok {
		widened := auth.HasScope(ctx, auth.ScopeAdmin) || auth.HasScope(ctx, auth.ScopeConsoleFleet)

		// Cross-tenant: a list whose scope Tenant differs from the
		// verified tenant requires the admin (or console:fleet) scope.
		if req.Scope.Tenant != verified.TenantID && !widened {
			return nil, protoerrors.Newf(protoerrors.CodeScopeMismatch,
				"method %q: cross-tenant artifact list requires the admin scope claim", m)
		}

		// Cross-user: a scope User that names somebody other than the
		// verified caller is refused, and an ELIDED user folds to the
		// caller's own rather than fanning across the tenant. Both are
		// the same widening and take the same claim; only the claimed
		// caller keeps the tenant-wide fan-in an empty user asks for.
		switch {
		case widened:
			// The claim grants the fan-in: a named foreign user and an
			// elided (all-users) one both pass through untouched.
		case scopeUser == "":
			scopeUser = verified.UserID
		case scopeUser != verified.UserID:
			return nil, protoerrors.Newf(protoerrors.CodeIdentityScopeRequired,
				"method %q: cross-user artifact list requires a verified `admin` or `console:fleet` scope", m)
		}
	}

	filter := artifacts.ArtifactScope{
		TenantID:  req.Scope.Tenant,
		UserID:    scopeUser,
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
// cross-tenant body, bounds the body size, submits the payload to the
// audit Redactor as an admission gate, stores the bytes AS AUTHORED, and
// emits artifacts.uploaded.
//
// Stored bytes are not redacted bytes, and that is the contract rather
// than an omission: an artifact exists precisely to HOLD the content the
// event stream and the prompt must not carry, and a reference to it
// passes the redactor unredacted because it is a reference. The redactor
// is therefore a refusal path here, not a transform — see the Redact
// call below.
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

	// CLAUDE.md §7 rule 6 — submit the upload payload to the audit
	// Redactor BEFORE it reaches the store.
	//
	// The redactor is an ADMISSION GATE here, not a transform: a refusal
	// fails loud and nothing is stored, and its REWRITE is deliberately
	// discarded because an artifact holds the bytes its author supplied.
	// Redaction governs what is EMITTED — the event payload below, the
	// trajectory, the prompt, the log — not what is stored.
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

// handleGet serves artifacts.get — the driver-independent byte read.
// It validates the full identity triple, refuses a scope naming a tenant
// other than the verified one, resolves the ref's metadata, reads the
// bytes through ArtifactStore.Get (a MANDATORY interface method, which
// is what makes this read work on every registered driver), and returns
// the requested window with a truthful account of its bound.
//
// The tenant refusal is FLAT, with no admin elevation, for exactly the
// reason artifacts.get_ref's is: this method hands over CONTENT, which
// is materially broader than the metadata artifacts.list returns, and
// the scope requires the full triple — so the only foreign-tenant shape
// that can reach here already carries the caller's own user and session,
// which is not a fleet-observation shape and so has nothing to elevate.
// The fifth artifacts method does not quietly become a wider door onto
// what the fourth guards narrowly.
//
// # The bound, and what it does not promise
//
// Three sources can bound one read — the caller's MaxBytes, the
// operator's default when the caller named none, and the operator's hard
// ceiling — and all three answer through ONE response field set
// (Truncated / TotalSizeBytes / ReturnedBytes) rather than growing a
// signal each. A request above the effective ceiling is SERVED at the
// ceiling and reports it, rather than being refused: a caller cannot
// know a deployment's ceiling before asking, so a refusal would cost a
// round trip and teach nothing. Truthful truncation is the correct
// posture; SILENT truncation is what the fail-loud rule names.
//
// The ceiling bounds ONE read. It is not a budget over repeated reads,
// and the governance layer's cost ceilings and rate limits remain the
// mechanism for aggregate consumption — saying otherwise would claim a
// property the knob does not have.
//
// # Cost
//
// A window is a CONTRACT, not a cost claim. ArtifactStore.Get returns
// whole bytes and every driver materialises the blob, so reading at an
// offset costs a full materialisation. A range-aware store method is a
// separate five-driver conformance change; no claim about which drivers
// serve a window incrementally is made here or in the interface godoc.
func (s *ArtifactsSurface) handleGet(ctx context.Context, req *types.ArtifactsGetRequest) (any, error) {
	m := string(methods.MethodArtifactsGet)

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
	// A negative offset or bound is not an omission — a caller that meant
	// "from the end" or "no limit" is asking for something this method
	// does not offer, and reinterpreting it would be a guess.
	if req.Offset < 0 {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: offset %d must not be negative", m, req.Offset)
	}
	if req.MaxBytes < 0 {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: max_bytes %d must not be negative", m, req.MaxBytes)
	}

	// Tenant reconciliation, BEFORE the store read so a driver is never
	// consulted for another tenant's scope. No scope claim widens this
	// method (see the godoc above).
	if verified, ok := identity.FromVerified(ctx); ok {
		if req.Scope.Tenant != verified.TenantID {
			return nil, protoerrors.Newf(protoerrors.CodeScopeMismatch,
				"method %q: scope tenant does not match the verified identity", m)
		}
	}

	ref, found, err := s.store.GetRef(ctx, scope, req.ID)
	if err != nil {
		return nil, mapArtifactsError(m, err)
	}
	if !found || ref == nil {
		// A ref outside the caller's triple and a ref that never existed
		// answer identically. The store does not distinguish them, and
		// neither does this: a distinguishable refusal would confirm the
		// existence of another identity's artifact to a caller that
		// cannot read it.
		return nil, protoerrors.Newf(protoerrors.CodeNotFound,
			"method %q: artifact %q not found in scope", m, req.ID)
	}

	blob, found, err := s.store.Get(ctx, scope, req.ID)
	if err != nil {
		return nil, mapArtifactsError(m, err)
	}
	if !found {
		// GetRef resolved and Get did not — a concurrent Delete, or a
		// driver inconsistency. Same shape as the unknown-ref case: the
		// caller cannot act on the difference.
		return nil, protoerrors.Newf(protoerrors.CodeNotFound,
			"method %q: artifact %q not found in scope", m, req.ID)
	}

	window, truncated := boundedWindow(blob, req.Offset, s.effectiveMaxBytes(req.MaxBytes))
	return &types.ArtifactsGetResponse{
		Ref:             projectRef(*ref),
		Content:         window,
		Offset:          req.Offset,
		ReturnedBytes:   int64(len(window)),
		TotalSizeBytes:  int64(len(blob)),
		Truncated:       truncated,
		ProtocolVersion: types.ProtocolVersion,
	}, nil
}

// effectiveMaxBytes resolves the window length one read may return:
// the caller's own bound, the operator's default when the caller named
// none, and never more than the operator's hard ceiling.
//
// The clamp is applied here and REPORTED by the caller through the same
// Truncated / TotalSizeBytes fields every other bound uses, so no branch
// of this function can shorten a response silently.
func (s *ArtifactsSurface) effectiveMaxBytes(requested int64) int64 {
	if requested <= 0 {
		requested = s.fetchDefaultMaxBytes
	}
	if requested > s.fetchHardMaxBytes {
		return s.fetchHardMaxBytes
	}
	return requested
}

// boundedWindow returns blob[offset : offset+maxBytes] (clipped to the
// blob) and whether bytes remain AFTER the returned window.
//
// An offset at or beyond the blob is not an error: the window is empty
// and nothing follows it, so truncated is false. The returned slice is a
// COPY — the response outlives the driver's buffer, and an in-memory
// driver hands back a slice a caller must not be able to alias.
func boundedWindow(blob []byte, offset, maxBytes int64) (window []byte, truncated bool) {
	total := int64(len(blob))
	if offset >= total {
		return nil, false
	}
	end := offset + maxBytes
	if end > total {
		end = total
	}
	out := make([]byte, end-offset)
	copy(out, blob[offset:end])
	return out, end < total
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
