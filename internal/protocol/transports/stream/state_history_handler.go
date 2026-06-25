// Package stream — addition: the `state.history` windowed event-replay
// handler. Like the `memory.*` reads and `pause.list`, it is a one-shot
// request/response — POST JSON in, JSON out — and lives in the stream
// package because its identity edge is the same one Subscribe / Replay /
// the memory reads use, and it reads the same durable event-bus substrate
// the SSE stream tails.
//
// Route shape:
//
//	POST /v1/state/history
//
// The handler resolves identity at the edge (an incomplete triple →
// CodeIdentityRequired / 401), derives admin authority SOLELY from the
// verified ctx scope (the request body carries NO elevation knob), asserts
// the bus implements events.HistoryReplayer (a bus without the windowed
// capability → CodeRuntimeError, surfaced loud, never a silent empty
// page), dispatches Bounds + Window, projects events → the flat
// types.StateEvent wire shape (heavy content carried by a routable
// types.StateArtifactRef, never inline), and encodes. An unknown or
// cross-identity session is CodeNotFound (404 — existence is never
// revealed across identities, mirroring tasks.get).
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
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// StateHistoryRoutePattern is the route the state-history handler mounts
// under. Exported so internal/protocol/transports can mount it under the
// same pattern it documents.
const StateHistoryRoutePattern = "POST /v1/state/history"

// maxStateHistoryBodyBytes bounds a state.history request body. The wire
// payload is small (an identity + a session id + two ints); 64 KiB is
// comfortably over the realistic ceiling and fails closed on a client
// that streams an unbounded body at the edge.
const maxStateHistoryBodyBytes = 64 << 10

// ErrStateHistoryMisconfigured — NewStateHistoryHandler was called with a
// nil mandatory dependency (EventBus or ArtifactStore).
var ErrStateHistoryMisconfigured = errors.New("stream: state-history handler missing a mandatory dependency")

// StateHistoryHandler serves the `state.history` windowed-read route. It
// is a safe-for-concurrent-reuse compiled artifact: every field is set
// once at construction; ServeHTTP holds no per-request state.
type StateHistoryHandler struct {
	bus       events.EventBus
	artifacts artifacts.ArtifactStore
	logger    *slog.Logger
}

// StateHistoryOption configures NewStateHistoryHandler at construction.
type StateHistoryOption func(*StateHistoryHandler)

// WithStateHistoryLogger sets the slog.Logger the handler logs decode /
// projection failures to. A nil logger (the default) routes to
// slog.Default().
func WithStateHistoryLogger(l *slog.Logger) StateHistoryOption {
	return func(h *StateHistoryHandler) {
		if l != nil {
			h.logger = l
		}
	}
}

// NewStateHistoryHandler builds the state-history handler over an
// events.EventBus + an artifacts.ArtifactStore. Both are mandatory — a
// nil fails loud with ErrStateHistoryMisconfigured rather than building a
// handler that would nil-panic on the first request (CLAUDE.md §5). The
// bus need not implement events.HistoryReplayer at construction; a bus
// without the windowed capability is surfaced per-request as
// CodeRuntimeError (loud, never a silent empty page).
//
// The returned *StateHistoryHandler is immutable after construction and
// safe for concurrent use by N goroutines.
func NewStateHistoryHandler(bus events.EventBus, arts artifacts.ArtifactStore, opts ...StateHistoryOption) (*StateHistoryHandler, error) {
	if bus == nil {
		return nil, fmt.Errorf("%w: events.EventBus is nil", ErrStateHistoryMisconfigured)
	}
	if arts == nil {
		return nil, fmt.Errorf("%w: artifacts.ArtifactStore is nil", ErrStateHistoryMisconfigured)
	}
	h := &StateHistoryHandler{
		bus:       bus,
		artifacts: arts,
		logger:    slog.Default(),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// Handler returns the http.Handler for `POST /v1/state/history`.
func (h *StateHistoryHandler) Handler() http.Handler {
	return http.HandlerFunc(h.serve)
}

// serve answers `POST /v1/state/history`.
func (h *StateHistoryHandler) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeStateHistoryError(w, protoerrors.CodeInvalidRequest, http.StatusMethodNotAllowed,
			"state.history accepts POST only")
		return
	}

	// Identity edge — a missing / incomplete triple fails closed with
	// CodeIdentityRequired (401).
	verified, err := resolveIdentity(r)
	if err != nil {
		writeStateHistoryError(w, protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			"identity scope incomplete: "+err.Error())
		return
	}

	var req prototypes.StateHistoryRequest
	if perr := decodeStateHistoryBody(w, r, &req); perr != nil {
		writeStateHistoryError(w, perr.code, perr.status, perr.message)
		return
	}

	if req.SessionID == "" {
		writeStateHistoryError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"state.history requires a non-empty session_id")
		return
	}
	if req.Limit < 0 {
		writeStateHistoryError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"state.history limit must be >= 0 (zero ⇒ default)")
		return
	}
	limit := clampStateHistoryLimit(req.Limit)

	// adminScoped derives SOLELY from the verified ctx scope — the request
	// body carries no elevation knob (authority comes from the verified
	// JWT, never the request).
	adminScoped := auth.HasScope(r.Context(), auth.ScopeAdmin)

	// Cross-identity gate. A body that names a tenant/user other than the
	// verified one is a cross-identity read: it requires the verified
	// admin scope, and absent it returns CodeNotFound (404 — existence is
	// never revealed across identities, mirroring tasks.get; never a 403
	// that would green-light an existence leak).
	crossIdentity := (req.Identity.Tenant != "" && req.Identity.Tenant != verified.TenantID) ||
		(req.Identity.User != "" && req.Identity.User != verified.UserID)

	effTenant, effUser := verified.TenantID, verified.UserID
	filterAdmin := false
	if crossIdentity {
		if !adminScoped {
			writeStateHistoryError(w, protoerrors.CodeNotFound, http.StatusNotFound,
				"state.history: session not found")
			return
		}
		if req.Identity.Tenant != "" {
			effTenant = req.Identity.Tenant
		}
		if req.Identity.User != "" {
			effUser = req.Identity.User
		}
		filterAdmin = true
	}

	f := events.Filter{
		Tenant:  effTenant,
		User:    effUser,
		Session: req.SessionID,
		Admin:   filterAdmin,
	}

	// Assert the bus implements the windowed-read capability. A bus
	// without it is surfaced loud (CodeRuntimeError / 500) — never a
	// silent empty page (CLAUDE.md §5 / §13).
	hr, ok := h.bus.(events.HistoryReplayer)
	if !ok {
		writeStateHistoryError(w, protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			"state.history: the configured event bus does not support windowed history reads")
		return
	}

	head, tail, truncated, err := hr.Bounds(r.Context(), f)
	if err != nil {
		code, status, msg := classifyStateHistoryError(err)
		writeStateHistoryError(w, code, status, msg)
		return
	}

	evs, err := hr.Window(r.Context(), req.Before, limit, f)
	if err != nil {
		code, status, msg := classifyStateHistoryError(err)
		writeStateHistoryError(w, code, status, msg)
		return
	}

	resp := h.project(r.Context(), evs, head, tail, truncated)
	h.encode(r.Context(), w, verified, &resp)
}

// project turns a window of events.Event into the wire response: flat
// StateEvent rows (heavy content by routable StateArtifactRef) plus the
// head/tail bounds and the scroll-up cursor.
func (h *StateHistoryHandler) project(ctx context.Context, evs []events.Event, head, tail uint64, truncated bool) prototypes.StateHistoryResponse {
	out := prototypes.StateHistoryResponse{
		Events:       make([]prototypes.StateEvent, 0, len(evs)),
		HeadSequence: head,
		TailSequence: tail,
	}
	for _, ev := range evs {
		out.Events = append(out.Events, h.projectEvent(ctx, ev))
	}
	// NextCursor is the lowest sequence in this page (the events are
	// oldest-first, so the first is the lowest). Zero when the head is
	// reached — no older events remain.
	if len(out.Events) > 0 {
		lowest := out.Events[0].Sequence
		if lowest > head {
			out.NextCursor = lowest
			out.HasMore = true
		}
	}
	// Truncated reflects the bus's honest retention signal: false on the
	// gap-free durable log (never trims in V1), true on a wrapped
	// best-effort ring whose retained head is no longer the session's first
	// sequence — so a windowed read over an evicting ring is never silently
	// presented as a complete from-seq-1 conversation (CLAUDE.md §13).
	out.Truncated = truncated
	return out
}

// projectEvent flattens one event into the StateEvent wire shape and
// enriches any routable artifact refs from the artifact store
// (best-effort).
func (h *StateHistoryHandler) projectEvent(ctx context.Context, ev events.Event) prototypes.StateEvent {
	se := prototypes.StateEvent{
		Type:       string(ev.Type),
		Sequence:   ev.Sequence,
		OccurredAt: ev.OccurredAt.UTC(),
		Tenant:     ev.Identity.TenantID,
		User:       ev.Identity.UserID,
		Session:    ev.Identity.SessionID,
		Run:        ev.Identity.RunID,
		Extra:      ev.Extra,
	}
	se.Payload = payloadWireValue(ev.Payload)

	seeds := extractArtifactRefSeeds(se.Payload)
	if len(seeds) == 0 {
		return se
	}
	scope := artifacts.ArtifactScope{
		TenantID:  ev.Identity.TenantID,
		UserID:    ev.Identity.UserID,
		SessionID: ev.Identity.SessionID,
	}
	for _, seed := range seeds {
		ref := prototypes.StateArtifactRef{
			ID:        seed.id,
			MimeType:  seed.mime,
			SizeBytes: seed.size,
			SHA256:    seed.sha,
		}
		// Best-effort enrichment: a store hit fills SHA256 / Filename /
		// SizeBytes / MimeType. A miss leaves the payload-derived fields —
		// the ID still routes to artifacts.get_ref.
		if got, found, gerr := h.artifacts.GetRef(ctx, scope, seed.id); gerr == nil && found && got != nil {
			ref.SHA256 = got.SHA256
			ref.Filename = got.Filename
			if got.SizeBytes != 0 {
				ref.SizeBytes = got.SizeBytes
			}
			if got.MimeType != "" {
				ref.MimeType = got.MimeType
			}
		}
		se.Artifacts = append(se.Artifacts, ref)
	}
	return se
}

// payloadWireValue normalises an event payload to a JSON-friendly wire
// value. A RedactedMap (the durable-log post-redaction shape) surfaces
// its inner Data map; any other payload is passed through (the SSE
// projection's posture — the bus already redacted it on Publish).
func payloadWireValue(p events.EventPayload) any {
	if p == nil {
		return nil
	}
	if rm, ok := p.(events.RedactedMap); ok {
		return rm.Data
	}
	return p
}

// artifactRefSeed is a routable artifact reference pulled from a redacted
// event payload before store enrichment.
type artifactRefSeed struct {
	id   string
	mime string
	size int64
	sha  string
}

// extractArtifactRefSeeds walks an already-redaction-safe payload tree for
// heavy-content OFFLOAD references. The ONLY recognised signal is a
// STRING-valued "artifact_ref" key — the canonical heavy-output by-reference
// shape stamped by llm.ArtifactStub, audit.ArtifactRef, and
// dispatch.HeavyTruncationSummary (all `json:"artifact_ref"` strings) —
// or its PascalCase sibling "ArtifactRef" (the json-tag-less multimodal
// SafePayload events ImageMaterialized / FileUploaded).
//
// A bare "id" key is deliberately NOT a ref: tool-call ids, message ids,
// and the ArtifactStub `fetch` hint all carry "id" and would otherwise
// yield bogus, unroutable refs (and phantom Console artifact cards). The
// walk recurses nested maps/slices (a JSON-decoded tree is acyclic, so no
// recursion guard is needed) so a ref nested under any key is surfaced,
// and de-duplicates by id so the same offload is not emitted twice. The
// pulled id routes to artifacts.get_ref.
func extractArtifactRefSeeds(v any) []artifactRefSeed {
	var out []artifactRefSeed
	seen := map[string]struct{}{}
	walkArtifactRefs(v, &out, seen)
	return out
}

func walkArtifactRefs(v any, out *[]artifactRefSeed, seen map[string]struct{}) {
	switch t := v.(type) {
	case map[string]any:
		if seed, ok := artifactRefFromMap(t); ok {
			if _, dup := seen[seed.id]; !dup {
				seen[seed.id] = struct{}{}
				*out = append(*out, seed)
			}
		}
		for _, val := range t {
			walkArtifactRefs(val, out, seen)
		}
	case []any:
		for _, el := range t {
			walkArtifactRefs(el, out, seen)
		}
	}
}

// artifactRefFromMap recognises a heavy-offload ref by a STRING
// "artifact_ref" (canonical) or "ArtifactRef" (PascalCase multimodal)
// key. The sibling mime / size / hash fields are pulled in both
// spellings; absent siblings leave zero values (the id still routes).
func artifactRefFromMap(m map[string]any) (artifactRefSeed, bool) {
	id := mapString(m, "artifact_ref")
	if id == "" {
		id = mapString(m, "ArtifactRef")
	}
	if id == "" {
		return artifactRefSeed{}, false
	}
	return artifactRefSeed{
		id:   id,
		mime: firstNonEmpty(mapString(m, "mime"), mapString(m, "MIME"), mapString(m, "mime_type")),
		size: firstNonZeroInt64(mapInt64(m, "size_bytes"), mapInt64(m, "SizeBytes")),
		sha:  firstNonEmpty(mapString(m, "hash"), mapString(m, "Hash"), mapString(m, "sha256"), mapString(m, "SHA256")),
	}, true
}

// mapString returns m[key] as a string, or "" when absent / non-string.
func mapString(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

// mapInt64 returns m[key] coerced to int64, or 0 when absent / non-numeric.
func mapInt64(m map[string]any, key string) int64 {
	if n, ok := jsonNumberToInt64(m[key]); ok {
		return n
	}
	return 0
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonZeroInt64(vals ...int64) int64 {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

// jsonNumberToInt64 coerces a JSON-decoded numeric value (float64, or a
// json.Number-ish int) to int64.
func jsonNumberToInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	default:
		return 0, false
	}
}

// clampStateHistoryLimit applies the wire window bounds: zero ⇒ default;
// above the max ⇒ the max.
func clampStateHistoryLimit(limit int) int {
	if limit <= 0 {
		return prototypes.DefaultStateHistoryLimit
	}
	if limit > prototypes.MaxStateHistoryLimit {
		return prototypes.MaxStateHistoryLimit
	}
	return limit
}

// stateHistoryError is the internal carrier for a classified failure.
type stateHistoryError struct {
	code    protoerrors.Code
	status  int
	message string
}

// decodeStateHistoryBody reads the bounded request body into dst.
func decodeStateHistoryBody(w http.ResponseWriter, r *http.Request, dst *prototypes.StateHistoryRequest) *stateHistoryError {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxStateHistoryBodyBytes))
	if err != nil {
		return &stateHistoryError{
			code: protoerrors.CodeInvalidRequest, status: http.StatusBadRequest,
			message: "failed to read request body: " + err.Error(),
		}
	}
	if len(body) == 0 {
		// An empty body carries no identity — the resolveIdentity edge
		// already gated identity, but a body-less call cannot name a
		// session, so fail at the session check downstream.
		return nil
	}
	dec := json.NewDecoder(bytesReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return &stateHistoryError{
			code: protoerrors.CodeInvalidRequest, status: http.StatusBadRequest,
			message: "failed to decode request body: " + err.Error(),
		}
	}
	return nil
}

// classifyStateHistoryError maps an events-layer error onto a canonical
// Protocol Code + HTTP status + safe operator-facing message.
func classifyStateHistoryError(err error) (protoerrors.Code, int, string) {
	switch {
	case errors.Is(err, events.ErrNoHistory):
		// Unknown or empty session — existence is never revealed across
		// identities (mirrors tasks.get).
		return protoerrors.CodeNotFound, http.StatusNotFound,
			"state.history: session not found"
	case errors.Is(err, events.ErrIdentityScopeRequired):
		return protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			"state.history: identity scope incomplete"
	case errors.Is(err, events.ErrReplayUnavailable):
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			"state.history: windowed history reads are not available on this driver"
	case errors.Is(err, events.ErrBusClosed):
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			"state.history: event bus is closed"
	default:
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			"state.history: request failed"
	}
}

// encode writes a JSON 200 response. An encode failure is logged loudly
// but not re-surfaced — the status line is already committed.
func (h *StateHistoryHandler) encode(ctx context.Context, w http.ResponseWriter, id identity.Identity, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		h.logger.WarnContext(ctx, "state.history: response encode failed",
			slog.String("error", err.Error()),
			slog.String("tenant_id", id.TenantID),
			slog.String("user_id", id.UserID),
			slog.String("session_id", id.SessionID))
	}
}

// writeStateHistoryError writes a JSON error body with the canonical
// Protocol Code + the supplied HTTP status.
func writeStateHistoryError(w http.ResponseWriter, code protoerrors.Code, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&protoerrors.Error{ //nolint:errcheck // response status already committed — a write error cannot be recovered here.
		Code:    code,
		Message: message,
	})
}
