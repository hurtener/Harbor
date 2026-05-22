package devdraft

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// RoutePrefix is the canonical path prefix the draft handler is
// mounted under by `harbor dev`. The Phase 60 transport mux owns
// `/v1/control/...` + `/v1/events`; the draft surface is `/v1/dev/
// drafts/...` so a future Console can discover it via a stable
// well-known root.
const RoutePrefix = "/v1/dev/drafts"

// Stable wire error codes. The Console + scripted clients branch on
// these strings; new codes ADD entries here. Codes mirror the
// Protocol error-code naming convention even though this surface is
// not (yet) part of the Single Source CanonicalWireTypes (D-093).
const (
	// CodeIdentityRequired — the request reached the handler without
	// a verified identity in ctx. Maps to 401.
	CodeIdentityRequired = "identity_required"
	// CodeInvalidRequest — the request was structurally malformed
	// (bad JSON, missing required field, unknown method).
	CodeInvalidRequest = "invalid_request"
	// CodeNotFound — the draft does not exist for the caller.
	CodeNotFound = "not_found"
	// CodeUnsafePath — a path component escaped its allowed root
	// (CLAUDE.md §7 rule 5).
	CodeUnsafePath = "unsafe_path"
	// CodeUnknownTemplate — the create body named a template not in
	// scaffold.Templates().
	CodeUnknownTemplate = "unknown_template"
	// CodeOutputDirExists — save's output dir already exists.
	CodeOutputDirExists = "output_dir_exists"
	// CodeValidationFailed — save's pre-promotion validation pass
	// failed (the draft's harbor.yaml does not pass config.Validate).
	CodeValidationFailed = "validation_failed"
	// CodeInternal — catch-all server-side error. Maps to 500.
	CodeInternal = "internal_error"
)

// errorEnvelope is the JSON shape every non-2xx response wraps.
// Mirrors the structured-error shape `cmd/harbor`'s CLIError uses on
// the CLI side — keeps the operator's tooling vocabulary consistent
// between subcommands.
type errorEnvelope struct {
	Error string `json:"error"`
	Code  string `json:"code"`
	Hint  string `json:"hint,omitempty"`
}

// createRequest is the POST /v1/dev/drafts/ body.
type createRequest struct {
	Name     string `json:"name"`
	Template string `json:"template,omitempty"`
}

// createResponse is the POST /v1/dev/drafts/ response body. Mirrors
// the engine's Draft minus the per-file Content payload (operators
// fetch the seeded content via Get).
type createResponse struct {
	DraftID   string   `json:"draft_id"`
	Template  string   `json:"template"`
	Files     []string `json:"files"`
	FileCount int      `json:"file_count"`
}

// getResponse is the GET /v1/dev/drafts/{id} response body. Echoes
// the engine's Draft including file contents (the Console editor
// hydrates the file list with this body).
type getResponse struct {
	*Draft
}

// patchRequest is the PATCH /v1/dev/drafts/{id}/files/{path} body.
// Content is the raw file content; ContentBase64 is the same content
// base64-encoded for clients that cannot send raw bytes in a JSON
// string (the Console uses base64 to stay binary-safe for templates
// with embedded NULs / non-UTF-8).
type patchRequest struct {
	Content       string `json:"content,omitempty"`
	ContentBase64 string `json:"content_base64,omitempty"`
}

// patchResponse is the PATCH response body.
type patchResponse struct {
	DraftID string `json:"draft_id"`
	Path    string `json:"path"`
	Size    int    `json:"size"`
}

// previewResponse is the POST /v1/dev/drafts/{id}/preview response.
type previewResponse struct {
	DraftID string   `json:"draft_id"`
	OK      bool     `json:"ok"`
	Errors  []string `json:"errors,omitempty"`
}

// saveRequest is the POST /v1/dev/drafts/{id}/save body. Both fields
// are mandatory.
type saveRequest struct {
	Name      string `json:"name"`
	OutputDir string `json:"output_dir"`
}

// saveResponse is the POST /v1/dev/drafts/{id}/save response.
type saveResponse struct {
	DraftID   string   `json:"draft_id"`
	Name      string   `json:"name"`
	OutputDir string   `json:"output_dir"`
	Files     []string `json:"files"`
	FileCount int      `json:"file_count"`
}

// discardResponse is the DELETE response.
type discardResponse struct {
	DraftID   string `json:"draft_id"`
	Discarded bool   `json:"discarded"`
}

// maxRequestBodyBytes caps inbound request bodies. PATCH carries
// file content (1 MiB cap per the Store's default + a small JSON
// envelope); everything else is small. 2 MiB is a generous upper
// bound that still rejects pathological payloads at the edge.
const maxRequestBodyBytes = 2 << 20

// Handler wraps a Store in an http.Handler that routes the five
// draft endpoints under RoutePrefix. The returned handler is a
// compiled artifact (D-025) and safe to share across N concurrent
// requests; per-request state lives in the request ctx.
//
// The handler does NOT perform auth — `harbor dev`'s boot wraps it
// in `auth.Middleware`, which injects the verified identity into
// ctx. Standalone tests that need to skip auth wrap the handler in
// their own ctx-injecting middleware.
type Handler struct {
	store  *Store
	logger *slog.Logger
}

// NewHandler builds a Handler. The store argument is mandatory; a
// nil store is an immediate construction error (CLAUDE.md §13
// fail-loud on operator-facing seam).
func NewHandler(store *Store, logger *slog.Logger) (*Handler, error) {
	if store == nil {
		return nil, fmt.Errorf("devdraft: NewHandler requires a non-nil Store")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{store: store, logger: logger}, nil
}

// ServeHTTP implements http.Handler. The router lives here in
// straight Go (no third-party mux) because the path shape is small,
// closed, and tightly coupled to the wire vocabulary above.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Strip the RoutePrefix; the rest is the per-endpoint suffix.
	path := strings.TrimPrefix(r.URL.Path, RoutePrefix)
	path = strings.TrimPrefix(path, "/")

	switch {
	case path == "" && r.Method == http.MethodPost:
		h.handleCreate(w, r)
	case path == "" && r.Method == http.MethodGet:
		// Reserved — list-all is a follow-up (no acceptance criterion
		// for V1). 405 keeps the surface honest.
		writeError(w, http.StatusMethodNotAllowed, errorEnvelope{
			Error: "list-all is not implemented at V1",
			Code:  CodeInvalidRequest,
			Hint:  "fetch a specific draft via GET " + RoutePrefix + "/{id}",
		})
	case path != "":
		h.dispatchDraftPath(w, r, path)
	default:
		writeError(w, http.StatusMethodNotAllowed, errorEnvelope{
			Error: fmt.Sprintf("method %s not allowed at %s", r.Method, r.URL.Path),
			Code:  CodeInvalidRequest,
		})
	}
}

// dispatchDraftPath routes a per-draft path suffix to the matching
// handler. The path shapes are:
//
//	{id}                              — GET, DELETE
//	{id}/files/{path}                 — PATCH
//	{id}/preview                      — POST
//	{id}/save                         — POST
func (h *Handler) dispatchDraftPath(w http.ResponseWriter, r *http.Request, suffix string) {
	parts := strings.SplitN(suffix, "/", 3)
	draftID := parts[0]
	if err := validateDraftID(draftID); err != nil {
		writeError(w, http.StatusBadRequest, errorEnvelope{
			Error: err.Error(),
			Code:  CodeInvalidRequest,
		})
		return
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			h.handleGet(w, r, draftID)
		case http.MethodDelete:
			h.handleDiscard(w, r, draftID)
		default:
			writeError(w, http.StatusMethodNotAllowed, errorEnvelope{
				Error: fmt.Sprintf("method %s not allowed at %s", r.Method, r.URL.Path),
				Code:  CodeInvalidRequest,
			})
		}
		return
	}

	switch parts[1] {
	case "files":
		if len(parts) < 3 || parts[2] == "" {
			writeError(w, http.StatusBadRequest, errorEnvelope{
				Error: "missing file path in PATCH request",
				Code:  CodeInvalidRequest,
			})
			return
		}
		if r.Method != http.MethodPatch {
			writeError(w, http.StatusMethodNotAllowed, errorEnvelope{
				Error: fmt.Sprintf("method %s not allowed at %s; use PATCH", r.Method, r.URL.Path),
				Code:  CodeInvalidRequest,
			})
			return
		}
		h.handlePatch(w, r, draftID, parts[2])
	case "preview":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, errorEnvelope{
				Error: fmt.Sprintf("method %s not allowed at %s; use POST", r.Method, r.URL.Path),
				Code:  CodeInvalidRequest,
			})
			return
		}
		h.handlePreview(w, r, draftID)
	case "save":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, errorEnvelope{
				Error: fmt.Sprintf("method %s not allowed at %s; use POST", r.Method, r.URL.Path),
				Code:  CodeInvalidRequest,
			})
			return
		}
		h.handleSave(w, r, draftID)
	default:
		writeError(w, http.StatusNotFound, errorEnvelope{
			Error: fmt.Sprintf("unknown draft subpath %q", parts[1]),
			Code:  CodeInvalidRequest,
		})
	}
}

func (h *Handler) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, errorEnvelope{
			Error: err.Error(),
			Code:  CodeInvalidRequest,
		})
		return
	}
	draft, err := h.store.Create(r.Context(), CreateOptions(req))
	if err != nil {
		h.writeStoreError(w, err)
		return
	}
	files := make([]string, 0, len(draft.Files))
	for _, f := range draft.Files {
		files = append(files, f.Path)
	}
	writeJSON(w, http.StatusCreated, createResponse{
		DraftID:   draft.ID,
		Template:  draft.Template,
		Files:     files,
		FileCount: len(files),
	})
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request, draftID string) {
	draft, err := h.store.Get(r.Context(), draftID)
	if err != nil {
		h.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, getResponse{Draft: draft})
}

func (h *Handler) handlePatch(w http.ResponseWriter, r *http.Request, draftID, rawPath string) {
	var req patchRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, errorEnvelope{
			Error: err.Error(),
			Code:  CodeInvalidRequest,
		})
		return
	}
	content, decodeErr := decodePatchContent(req)
	if decodeErr != nil {
		writeError(w, http.StatusBadRequest, errorEnvelope{
			Error: decodeErr.Error(),
			Code:  CodeInvalidRequest,
		})
		return
	}
	if err := h.store.WriteFile(r.Context(), draftID, rawPath, content); err != nil {
		h.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, patchResponse{
		DraftID: draftID,
		Path:    rawPath,
		Size:    len(content),
	})
}

func (h *Handler) handlePreview(w http.ResponseWriter, r *http.Request, draftID string) {
	res, err := h.store.Preview(r.Context(), draftID)
	if err != nil {
		h.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, previewResponse{
		DraftID: draftID,
		OK:      res.OK,
		Errors:  res.Errors,
	})
}

func (h *Handler) handleSave(w http.ResponseWriter, r *http.Request, draftID string) {
	var req saveRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, errorEnvelope{
			Error: err.Error(),
			Code:  CodeInvalidRequest,
		})
		return
	}
	res, err := h.store.Save(r.Context(), draftID, SaveOptions(req))
	if err != nil {
		h.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saveResponse{
		DraftID:   draftID,
		Name:      res.Name,
		OutputDir: res.OutputDir,
		Files:     res.Files,
		FileCount: len(res.Files),
	})
}

func (h *Handler) handleDiscard(w http.ResponseWriter, r *http.Request, draftID string) {
	if err := h.store.Discard(r.Context(), draftID); err != nil {
		h.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, discardResponse{
		DraftID:   draftID,
		Discarded: true,
	})
}

// writeStoreError maps a Store error onto the right HTTP status +
// error code. The mapping is the single source of truth for the
// wire contract.
func (h *Handler) writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrIdentityMissing):
		writeError(w, http.StatusUnauthorized, errorEnvelope{
			Error: err.Error(),
			Code:  CodeIdentityRequired,
			Hint:  "the request reached the draft handler without a verified identity; check the auth middleware",
		})
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, errorEnvelope{
			Error: err.Error(),
			Code:  CodeNotFound,
		})
	case errors.Is(err, ErrUnsafePath):
		writeError(w, http.StatusBadRequest, errorEnvelope{
			Error: err.Error(),
			Code:  CodeUnsafePath,
			Hint:  "path components MUST be relative and MUST NOT contain parent-directory tokens",
		})
	case errors.Is(err, ErrUnknownTemplate):
		writeError(w, http.StatusBadRequest, errorEnvelope{
			Error: err.Error(),
			Code:  CodeUnknownTemplate,
		})
	case errors.Is(err, ErrInvalidName):
		writeError(w, http.StatusBadRequest, errorEnvelope{
			Error: err.Error(),
			Code:  CodeInvalidRequest,
		})
	case errors.Is(err, ErrOutputDirExists):
		writeError(w, http.StatusConflict, errorEnvelope{
			Error: err.Error(),
			Code:  CodeOutputDirExists,
			Hint:  "remove the existing dir or supply a different output path",
		})
	case errors.Is(err, ErrValidationFailed):
		writeError(w, http.StatusBadRequest, errorEnvelope{
			Error: err.Error(),
			Code:  CodeValidationFailed,
			Hint:  "edit the draft's harbor.yaml until preview reports ok=true, then retry save",
		})
	default:
		h.logger.Error("devdraft: internal handler error", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, errorEnvelope{
			Error: err.Error(),
			Code:  CodeInternal,
		})
	}
}

// decodeJSON reads a bounded request body and unmarshals into v.
// Returns a clear error message on malformed JSON / missing body /
// oversize payload.
func decodeJSON(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, maxRequestBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("request body is empty")
		}
		return fmt.Errorf("decode JSON: %w", err)
	}
	return nil
}

// decodePatchContent normalises the two content shapes the PATCH
// body supports (raw `content` string OR `content_base64`). Exactly
// one must be set.
func decodePatchContent(req patchRequest) ([]byte, error) {
	hasRaw := req.Content != ""
	hasB64 := req.ContentBase64 != ""
	switch {
	case hasRaw && hasB64:
		return nil, fmt.Errorf("PATCH body must set exactly one of {content, content_base64}")
	case hasRaw:
		return []byte(req.Content), nil
	case hasB64:
		decoded, err := base64.StdEncoding.DecodeString(req.ContentBase64)
		if err != nil {
			return nil, fmt.Errorf("decode content_base64: %w", err)
		}
		return decoded, nil
	default:
		// An explicit empty body is allowed — `content: ""` clears
		// the file. The split above distinguishes the case from
		// "neither field set"; here we accept the explicit-empty path.
		return []byte{}, nil
	}
}

// writeJSON marshals body as JSON and writes the response with the
// given status.
func writeJSON(w http.ResponseWriter, status int, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		// Marshal failure on a well-typed body is "impossible by
		// construction"; fall through with a generic 500 envelope.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		//nolint:errcheck // response-body write; a failure means the client disconnected and is non-actionable
		_, _ = w.Write([]byte(`{"error":"json marshal failed","code":"internal_error"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	//nolint:errcheck // response-body write; a failure means the client disconnected and is non-actionable
	_, _ = w.Write(raw)
}

// writeError is the failure-path twin of writeJSON.
func writeError(w http.ResponseWriter, status int, env errorEnvelope) {
	writeJSON(w, status, env)
}
