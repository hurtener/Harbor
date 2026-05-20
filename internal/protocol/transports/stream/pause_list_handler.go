// Package stream — Wave 13 additions (Phase 72e): the `pause.list`
// HTTP handler. Like `events.aggregate`, this is a one-shot
// request/response — POST JSON in, JSON out — and it lives in the
// stream package because its identity + cross-tenant scope-claim gating
// is the same one Subscribe / Aggregate use, and it shares the route
// prefix family the Console subscription surface registers.
//
// Route shape:
//
//	POST /v1/pause/list
//
// The handler reads identity from r.Context() (auth.Middleware) or the
// X-Harbor-* carrier headers (Phase 60 fallback), decodes the JSON body
// into a types.PauseListRequest, gates cross-tenant filters on
// auth.HasScope(ScopeAdmin), projects the snapshot from the unified
// pause/resume Coordinator (Phase 50), applies the D-026 heavy-content
// bypass on each row, and encodes the response. The response body is
// the wire types.PauseListResponse JSON; on failure, a JSON error body
// with the canonical Protocol Code.
//
// pause.list is READ-ONLY against the Coordinator — it does NOT call
// Resume, does NOT clear checkpoints. Resume actions continue through
// the Phase 54 `resume` / `approve` / `reject` control methods. The
// unified pause/resume primitive is never bypassed: pause.list reads
// the shipped Coordinator state, it does not reinvent pause
// coordination (CLAUDE.md §7 rule 4, §13).

package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
)

// PauseListRoutePattern is the http.ServeMux pattern the pause-list
// handler registers under. Exported so internal/protocol/transports can
// mount the handler under the same pattern it documents.
const PauseListRoutePattern = "POST /v1/pause/list"

// maxPauseListBodyBytes bounds the pause-list request body. The wire
// payload is small (a filter + two pagination ints); 32 KiB is
// comfortably over the realistic ceiling and fails closed on a client
// that streams an unbounded body at the edge.
const maxPauseListBodyBytes = 32 << 10

// pauseListArtifactNamespace is the artifact namespace heavy pause
// payloads are routed under (D-026). A dedicated namespace keeps the
// content-addressed IDs distinguishable from other artifact producers.
const pauseListArtifactNamespace = "pause_payload"

// ErrPauseListMisconfigured — NewPauseListHandler was called with a nil
// mandatory dependency (Coordinator, ArtifactStore, or the heavy-content
// threshold was non-positive).
var ErrPauseListMisconfigured = errors.New("stream: pause.list handler missing a mandatory dependency")

// PauseListHandler serves `POST /v1/pause/list`. It is the wire adapter
// over a pauseresume.Coordinator: decode the request, gate on scope
// claim if the filter is cross-tenant, project the snapshot, apply the
// D-026 heavy-content bypass per row, encode the response. The handler
// is a D-025-safe compiled artifact — every field is set once at
// construction; ServeHTTP holds no per-request state.
type PauseListHandler struct {
	coord     pauseresume.Coordinator
	artifacts artifacts.ArtifactStore
	bus       events.EventBus // optional — nil ⇒ no pause.payload_artifact_routed emit
	logger    *slog.Logger
	threshold int
}

// PauseListOption configures NewPauseListHandler at construction.
type PauseListOption func(*PauseListHandler)

// WithPauseListLogger sets the slog.Logger the handler logs decode /
// projection failures to. A nil logger (the default) routes to
// slog.Default().
func WithPauseListLogger(l *slog.Logger) PauseListOption {
	return func(h *PauseListHandler) {
		if l != nil {
			h.logger = l
		}
	}
}

// WithPauseListBus wires the canonical events.EventBus into the handler
// so the D-026 heavy-content bypass can publish a
// `pause.payload_artifact_routed` observation when a heavy payload is
// routed through the ArtifactStore. The bus is OPTIONAL — when not
// supplied, the bypass still happens (the heavy payload is still routed
// to the ArtifactStore and shipped by-reference) but the observation
// event is not emitted; the handler logs the bypass at Info instead so
// the routing is never fully silent. A nil bus is treated as
// "WithPauseListBus not supplied".
func WithPauseListBus(b events.EventBus) PauseListOption {
	return func(h *PauseListHandler) {
		if b != nil {
			h.bus = b
		}
	}
}

// NewPauseListHandler builds the pause-list handler over a
// pauseresume.Coordinator + an artifacts.ArtifactStore. coord and
// store are mandatory — a nil fails loud with ErrPauseListMisconfigured
// rather than building a handler that would nil-panic on the first
// request (CLAUDE.md §5). threshold is the configured heavy-content
// byte size (cfg.Artifacts.HeavyOutputThresholdBytes); a non-positive
// value fails loud (a zero threshold would route every payload).
//
// The returned *PauseListHandler is immutable after construction
// (D-025) and safe for concurrent use by N goroutines.
func NewPauseListHandler(coord pauseresume.Coordinator, store artifacts.ArtifactStore, threshold int, opts ...PauseListOption) (*PauseListHandler, error) {
	if coord == nil {
		return nil, fmt.Errorf("%w: pauseresume.Coordinator is nil", ErrPauseListMisconfigured)
	}
	if store == nil {
		return nil, fmt.Errorf("%w: artifacts.ArtifactStore is nil", ErrPauseListMisconfigured)
	}
	if threshold <= 0 {
		return nil, fmt.Errorf("%w: heavy-content threshold %d is non-positive", ErrPauseListMisconfigured, threshold)
	}
	h := &PauseListHandler{
		coord:     coord,
		artifacts: store,
		logger:    slog.Default(),
		threshold: threshold,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// ServeHTTP implements http.Handler. It resolves identity from
// r.Context() (auth.Middleware) or the X-Harbor-* carrier headers
// (Phase 60 fallback), decodes the JSON body into a PauseListRequest,
// gates cross-tenant filters on scope, projects the Coordinator
// snapshot, applies the heavy-content bypass, and encodes the response.
func (h *PauseListHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writePauseListError(w, protoerrors.CodeInvalidRequest, http.StatusMethodNotAllowed,
			"pause.list accepts POST only")
		return
	}

	// Identity at the edge — RFC §5.5, CLAUDE.md §6 rule 9. A missing /
	// incomplete triple fails closed with CodeIdentityRequired (401).
	id, err := resolveIdentity(r)
	if err != nil {
		writePauseListError(w, protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			"identity scope incomplete: "+err.Error())
		return
	}

	// Decode the body — bounded.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPauseListBodyBytes))
	if err != nil {
		writePauseListError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to read request body: "+err.Error())
		return
	}
	var req prototypes.PauseListRequest
	if len(body) > 0 {
		dec := json.NewDecoder(bytesReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writePauseListError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
				"failed to decode request body: "+err.Error())
			return
		}
	}

	// Defence in depth: when the body carries an identity, every
	// non-empty component MUST match the verified identity — a caller
	// cannot present a valid JWT for tenant T1 while submitting a body
	// claiming tenant T2.
	if perr := assertPauseBodyMatchesIdentity(req.Identity, id); perr != nil {
		writePauseListError(w, perr.code, perr.status, perr.message)
		return
	}

	// Cross-tenant gate — a filter naming a tenant other than the
	// caller's own, or more than one tenant, requires the verified
	// admin scope claim (D-079). The Coordinator's checkCrossTenantScope
	// is the authoritative enforcement; this edge check produces the
	// canonical CodeIdentityScopeRequired (403) Protocol error.
	adminScoped := auth.HasScope(r.Context(), auth.ScopeAdmin)
	if crossTenantRequested(req.Filter, id.TenantID) && !adminScoped {
		writePauseListError(w, protoerrors.CodeIdentityScopeRequired, http.StatusForbidden,
			"cross-tenant pause.list requires a verified `admin` scope claim")
		return
	}

	// Translate the wire request into the runtime-internal ListRequest.
	listReq, perr := toListRequest(req, id, adminScoped)
	if perr != nil {
		writePauseListError(w, perr.code, perr.status, perr.message)
		return
	}

	resp, lerr := h.coord.List(r.Context(), listReq)
	if lerr != nil {
		code, status, msg := classifyPauseListError(lerr)
		writePauseListError(w, code, status, msg)
		return
	}

	// Project the runtime snapshot into the wire response, applying the
	// D-026 heavy-content bypass per row.
	wireResp, perr := h.projectResponse(r.Context(), resp)
	if perr != nil {
		writePauseListError(w, perr.code, perr.status, perr.message)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(wireResp); err != nil {
		h.logger.WarnContext(r.Context(), "pause.list: response encode failed",
			slog.String("error", err.Error()),
			slog.String("tenant_id", id.TenantID),
			slog.String("user_id", id.UserID),
			slog.String("session_id", id.SessionID))
	}
}

// projectResponse maps the runtime ListResponse onto the wire
// PauseListResponse. Each row's Payload is checked against the
// heavy-content threshold (D-026): a payload whose JSON-marshalled byte
// length meets or exceeds the threshold is routed through the
// ArtifactStore and the row ships PayloadRef instead of inline bytes.
func (h *PauseListHandler) projectResponse(ctx context.Context, resp pauseresume.ListResponse) (*prototypes.PauseListResponse, *pauseListError) {
	out := &prototypes.PauseListResponse{
		Snapshots: make([]prototypes.PauseSnapshot, 0, len(resp.Snapshots)),
		Page:      resp.Page,
		PageSize:  resp.PageSize,
		PageCount: resp.PageCount,
		TotalRows: resp.TotalRows,
		Truncated: resp.Truncated,
	}
	for i, p := range resp.Snapshots {
		st := pauseresume.Status{}
		if i < len(resp.Statuses) {
			st = resp.Statuses[i]
		}
		snap := prototypes.PauseSnapshot{
			Token:  string(p.Token),
			Reason: string(p.Reason),
			State:  toWireState(st.State),
			Identity: prototypes.IdentityScope{
				Tenant:  p.Identity.TenantID,
				User:    p.Identity.UserID,
				Session: p.Identity.SessionID,
			},
			PausedAt:  p.PausedAt,
			ResumedAt: st.ResumedAt,
		}
		if len(p.Payload) > 0 {
			ref, perr := h.maybeRouteHeavyPayload(ctx, p)
			if perr != nil {
				return nil, perr
			}
			if ref != nil {
				snap.PayloadRef = ref
			} else {
				snap.Payload = p.Payload
			}
		}
		out.Snapshots = append(out.Snapshots, snap)
	}
	return out, nil
}

// maybeRouteHeavyPayload checks p.Payload against the heavy-content
// threshold. When the JSON-marshalled byte length is below the
// threshold it returns (nil, nil) — the caller ships the payload
// inline. When the length meets or exceeds the threshold it routes the
// payload through the ArtifactStore, emits a
// `pause.payload_artifact_routed` observation (when a bus is wired),
// and returns the by-reference PauseArtifactRef. A marshal or store
// failure fails loud — never a silent truncation (D-026, §13).
func (h *PauseListHandler) maybeRouteHeavyPayload(ctx context.Context, p pauseresume.Pause) (*prototypes.PauseArtifactRef, *pauseListError) {
	raw, err := json.Marshal(p.Payload)
	if err != nil {
		return nil, &pauseListError{
			code: protoerrors.CodeRuntimeError, status: http.StatusInternalServerError,
			message: "pause.list: pause payload could not be serialised",
		}
	}
	if len(raw) < h.threshold {
		return nil, nil
	}

	scope := artifacts.ArtifactScope{
		TenantID:  p.Identity.TenantID,
		UserID:    p.Identity.UserID,
		SessionID: p.Identity.SessionID,
	}
	ref, err := h.artifacts.PutBytes(ctx, scope, raw, artifacts.PutOpts{
		MimeType:  "application/json",
		Namespace: pauseListArtifactNamespace,
		Source: map[string]any{
			// methods.MethodPauseList is the single source for the
			// `pause.list` wire string (CLAUDE.md §8) — used here so
			// the Phase 58 single-source checker does not flag the
			// artifact-provenance literal.
			"producer":    string(methods.MethodPauseList),
			"pause_token": string(p.Token),
		},
	})
	if err != nil {
		return nil, &pauseListError{
			code: protoerrors.CodeRuntimeError, status: http.StatusInternalServerError,
			message: "pause.list: heavy payload could not be routed to the artifact store",
		}
	}

	// Make the bypass LOUD — emit a pause.payload_artifact_routed
	// observation when a bus is wired; log it at Info regardless. A
	// heavy payload is never silently truncated (D-026, §13).
	h.emitPayloadRouted(ctx, p, ref.ID, len(raw))

	return &prototypes.PauseArtifactRef{
		ID:        ref.ID,
		MimeType:  ref.MimeType,
		SizeBytes: ref.SizeBytes,
		Filename:  ref.Filename,
		SHA256:    ref.SHA256,
	}, nil
}

// emitPayloadRouted publishes the D-026 heavy-content-bypass
// observation. When no bus is wired, it logs at Info so the routing is
// never fully silent. A publish failure is logged loudly but does not
// fail the request — the bypass itself (routing the payload to the
// store) already succeeded; only the best-effort observation was lost.
func (h *PauseListHandler) emitPayloadRouted(ctx context.Context, p pauseresume.Pause, artifactID string, payloadBytes int) {
	h.logger.InfoContext(ctx, "pause.list: heavy pause payload routed to artifact store (D-026)",
		slog.String("pause_token", string(p.Token)),
		slog.String("artifact_id", artifactID),
		slog.Int("payload_bytes", payloadBytes),
		slog.Int("threshold_bytes", h.threshold))
	if h.bus == nil {
		return
	}
	ev := events.Event{
		Type: pauseresume.EventTypePausePayloadArtifactRouted,
		Identity: identity.Quadruple{
			Identity: p.Identity,
		},
		Payload: pauseresume.PausePayloadArtifactRoutedPayload{
			Token:          string(p.Token),
			ArtifactID:     artifactID,
			PayloadBytes:   payloadBytes,
			ThresholdBytes: h.threshold,
		},
	}
	if err := h.bus.Publish(ctx, ev); err != nil {
		h.logger.WarnContext(ctx, "pause.list: pause.payload_artifact_routed emit failed",
			slog.String("error", err.Error()),
			slog.String("pause_token", string(p.Token)))
	}
}

// pauseListError is the internal carrier for a classified pause.list
// failure — a canonical Protocol Code, an HTTP status, and a safe
// operator-facing message.
type pauseListError struct {
	code    protoerrors.Code
	status  int
	message string
}

// crossTenantRequested reports whether the wire filter reaches outside
// the caller's own tenant — a foreign tenant, or more than one tenant.
func crossTenantRequested(f prototypes.PauseFilter, callerTenant string) bool {
	if len(f.TenantIDs) == 0 {
		return false
	}
	if len(f.TenantIDs) > 1 {
		return true
	}
	return f.TenantIDs[0] != callerTenant
}

// assertPauseBodyMatchesIdentity is the defence-in-depth check: when the
// request body carries an identity, every non-empty component MUST
// match the verified identity. An entirely empty body identity is
// permitted (the verified identity in ctx is authoritative).
func assertPauseBodyMatchesIdentity(body prototypes.IdentityScope, verified identity.Identity) *pauseListError {
	if body.Tenant == "" && body.User == "" && body.Session == "" {
		return nil
	}
	if (body.Tenant != "" && body.Tenant != verified.TenantID) ||
		(body.User != "" && body.User != verified.UserID) ||
		(body.Session != "" && body.Session != verified.SessionID) {
		return &pauseListError{
			code: protoerrors.CodeIdentityRequired, status: http.StatusUnauthorized,
			message: "body identity scope does not match the verified identity",
		}
	}
	return nil
}

// toListRequest translates the wire PauseListRequest into the
// runtime-internal pauseresume.ListRequest. It validates the filter's
// Status / Reasons enum values at the edge (a malformed enum is a
// CodeInvalidRequest, never a silently-dropped filter).
func toListRequest(req prototypes.PauseListRequest, id identity.Identity, adminScoped bool) (pauseresume.ListRequest, *pauseListError) {
	states := make([]pauseresume.State, 0, len(req.Filter.Status))
	for _, s := range req.Filter.Status {
		ws := prototypes.PauseSnapshotState(s)
		if !prototypes.IsValidPauseSnapshotState(ws) {
			return pauseresume.ListRequest{}, &pauseListError{
				code: protoerrors.CodeInvalidRequest, status: http.StatusBadRequest,
				message: fmt.Sprintf("pause.list: %q is not a valid pause status", s),
			}
		}
		states = append(states, fromWireState(ws))
	}
	reasons := make([]pauseresume.Reason, 0, len(req.Filter.Reasons))
	for _, r := range req.Filter.Reasons {
		reason := pauseresume.Reason(r)
		if !pauseresume.IsValidReason(reason) {
			return pauseresume.ListRequest{}, &pauseListError{
				code: protoerrors.CodeInvalidRequest, status: http.StatusBadRequest,
				message: fmt.Sprintf("pause.list: %q is not a valid pause reason", r),
			}
		}
		reasons = append(reasons, reason)
	}
	return pauseresume.ListRequest{
		Identity: id,
		Filter: pauseresume.ListFilter{
			States:     states,
			TenantIDs:  req.Filter.TenantIDs,
			UserIDs:    req.Filter.UserIDs,
			SessionIDs: req.Filter.SessionIDs,
			RunIDs:     req.Filter.RunIDs,
			Reasons:    reasons,
			Since:      req.Filter.Since,
			Until:      req.Filter.Until,
		},
		Page:        req.Page,
		PageSize:    req.PageSize,
		AdminScoped: adminScoped,
	}, nil
}

// toWireState maps a runtime pause State onto the wire enum.
func toWireState(s pauseresume.State) prototypes.PauseSnapshotState {
	if s == pauseresume.StatusResumed {
		return prototypes.PauseStateResumed
	}
	return prototypes.PauseStatePaused
}

// fromWireState maps a wire pause-state enum onto the runtime State.
func fromWireState(s prototypes.PauseSnapshotState) pauseresume.State {
	if s == prototypes.PauseStateResumed {
		return pauseresume.StatusResumed
	}
	return pauseresume.StatusPaused
}

// classifyPauseListError maps a Coordinator.List error onto a canonical
// Protocol Code + HTTP status + safe operator-facing message.
func classifyPauseListError(err error) (protoerrors.Code, int, string) {
	switch {
	case errors.Is(err, pauseresume.ErrIdentityRequired):
		return protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			"pause.list: identity scope incomplete"
	case errors.Is(err, pauseresume.ErrInvalidPage):
		return protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"pause.list: invalid pagination — " + err.Error()
	case errors.Is(err, pauseresume.ErrCrossTenantScope):
		return protoerrors.CodeIdentityScopeRequired, http.StatusForbidden,
			"pause.list: cross-tenant filter requires the `admin` scope claim"
	default:
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			"pause.list: snapshot failed"
	}
}

// writePauseListError writes a JSON error body with the canonical
// Protocol Code + the supplied HTTP status. The body shape matches the
// REST control transport's error body so a client can decode both with
// the same Error wire type.
func writePauseListError(w http.ResponseWriter, code protoerrors.Code, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&protoerrors.Error{
		Code:    code,
		Message: message,
	})
}
