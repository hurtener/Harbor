package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

// receiverMetaPreparationTransport reauthorizes SDK HTTP retries against the
// current provider and replaces the serialized credential using the shared
// injection engine. An SDK retry must never replay a revoked or expired value.
type receiverMetaPreparationTransport struct {
	base      http.RoundTripper
	injection *CredentialInjection
	source    tools.ToolSourceID
}

func (t *receiverMetaPreparationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodDelete {
		// Session termination has no JSON-RPC params for a meta credential. Close
		// the local SDK handle without sending an unauthenticated remote request.
		return receiverLocalTermination(req), nil
	}
	if _, ok := identity.From(req.Context()); !ok {
		return nil, ErrIdentityMissing
	}
	if req.Method != http.MethodPost || req.Body == nil {
		return nil, fmt.Errorf("%w: authenticated meta preparation requires a JSON-RPC POST", ErrInvalidConfig)
	}
	defer func() { _ = req.Body.Close() }()
	var message map[string]json.RawMessage
	if err := json.NewDecoder(req.Body).Decode(&message); err != nil {
		return nil, fmt.Errorf("mcp: decode receiver request: %w", err)
	}
	if message == nil {
		return nil, fmt.Errorf("%w: receiver request is not an object", ErrInvalidConfig)
	}
	params := make(map[string]json.RawMessage)
	if raw := message["params"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, fmt.Errorf("mcp: decode receiver params: %w", err)
		}
	}
	if params == nil {
		params = make(map[string]json.RawMessage)
	}
	meta := mcpsdk.Meta{}
	if raw := params["_meta"]; len(raw) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&meta); err != nil {
			return nil, fmt.Errorf("mcp: decode receiver metadata: %w", err)
		}
	}
	if meta == nil {
		meta = mcpsdk.Meta{}
	}
	if _, err := resolveCredentialInjection(req.Context(), t.injection, t.source, meta); err != nil {
		return nil, fmt.Errorf("mcp: authenticate receiver retry: %w", err)
	}
	encodedMeta, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("mcp: encode receiver metadata: %w", err)
	}
	params["_meta"] = encodedMeta
	encodedParams, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("mcp: encode receiver params: %w", err)
	}
	message["params"] = encodedParams
	body, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("mcp: encode receiver request: %w", err)
	}
	clone := req.Clone(req.Context())
	clone.Body = io.NopCloser(bytes.NewReader(body))
	clone.ContentLength = int64(len(body))
	clone.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	return t.base.RoundTrip(clone)
}

// receiverLocalTermination releases an SDK session whose termination cannot be
// authenticated. Cleanup never sends a credential after admission was refused.
func receiverLocalTermination(req *http.Request) *http.Response {
	return &http.Response{StatusCode: http.StatusNoContent, Status: http.StatusText(http.StatusNoContent), Header: make(http.Header), Body: http.NoBody, Request: req}
}
