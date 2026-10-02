package transfer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// ImportPath is the signed, pre-admitted import-only runtime peer endpoint.
const ImportPath = "/v1/artifacts/transfer/import"
const grantHeader = "X-Harbor-Artifact-Transfer"
const probeHeader = "X-Harbor-Artifact-Probe"

// HTTPConfig pins recipient audiences to trusted origins. The request and grant
// cannot select a URL. Plain HTTP is restricted to explicitly enabled loopback
// fixtures; production peers require HTTPS and every redirect is refused.
type HTTPConfig struct {
	Peers             map[string]string
	Timeout           time.Duration
	AllowLoopbackHTTP bool
}

// HTTPSender is an immutable, concurrently reusable direct byte transport.
type HTTPSender struct {
	peers  map[string]string
	client *http.Client
}

// NewHTTPSender validates all peer origins before constructing the transport.
func NewHTTPSender(c HTTPConfig) (*HTTPSender, error) {
	if c.Timeout <= 0 || c.Timeout > time.Minute {
		return nil, ErrInvalid
	}
	peers := map[string]string{}
	for audience, origin := range c.Peers {
		u, err := url.Parse(origin)
		if err != nil || audience == "" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return nil, ErrInvalid
		}
		if u.Scheme != "https" {
			ip := net.ParseIP(u.Hostname())
			if u.Scheme != "http" || !c.AllowLoopbackHTTP || ip == nil || !ip.IsLoopback() {
				return nil, ErrInvalid
			}
		}
		u.Path = ImportPath
		peers[audience] = u.String()
	}
	transport := &http.Transport{DisableKeepAlives: true, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: c.Timeout}
	return &HTTPSender{peers: peers, client: &http.Client{Transport: transport, Timeout: c.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrUnauthorized }}}, nil
}

// Close releases idle peer connections; in-flight calls remain context-owned.
func (s *HTTPSender) Close() { s.client.CloseIdleConnections() }

// Send delivers the exact artifact to the configured recipient runtime.
func (s *HTTPSender) Send(ctx context.Context, g types.ArtifactTransferGrant, b []byte) (types.ArtifactTransferReceipt, error) {
	return s.call(ctx, g, b, false)
}

// Probe retrieves only an already admitted content-free receipt.
func (s *HTTPSender) Probe(ctx context.Context, g types.ArtifactTransferGrant) (types.ArtifactTransferReceipt, error) {
	return s.call(ctx, g, nil, true)
}
func (s *HTTPSender) call(ctx context.Context, g types.ArtifactTransferGrant, b []byte, probe bool) (types.ArtifactTransferReceipt, error) {
	endpoint, ok := s.peers[g.Destination.Audience]
	if !ok {
		return types.ArtifactTransferReceipt{}, ErrUnauthorized
	}
	raw, err := json.Marshal(g)
	if err != nil {
		return types.ArtifactTransferReceipt{}, fmt.Errorf("encode transfer header: %w", err)
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return types.ArtifactTransferReceipt{}, fmt.Errorf("create transfer request: %w", err)
	}
	r.Header.Set(grantHeader, base64.RawURLEncoding.EncodeToString(raw))
	r.Header.Set("Content-Type", "application/octet-stream")
	if probe {
		r.Header.Set(probeHeader, "true")
	}
	resp, err := s.client.Do(r)
	if err != nil {
		return types.ArtifactTransferReceipt{}, fmt.Errorf("transfer peer request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // Read/validation errors take precedence over closing a response body.
	if resp.StatusCode != http.StatusOK {
		return types.ArtifactTransferReceipt{}, decodePeerError(resp)
	}
	var receipt types.ArtifactTransferReceipt
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 16*1024))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&receipt); err != nil {
		return receipt, fmt.Errorf("decode peer receipt: %w", err)
	}
	var extra any
	if err = decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return receipt, ErrInvalid
	}
	return receipt, nil
}

// Handler exposes only signed exact imports whose recipient has already
// admitted the same grant through authenticated Protocol. It grants no reads,
// listing, arbitrary writes, or access to another runtime surface.
func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != ImportPath || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		encoded := r.Header.Get(grantHeader)
		if len(encoded) > 16*1024 {
			http.Error(w, "invalid_transfer", http.StatusBadRequest)
			return
		}
		b, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			http.Error(w, "invalid_transfer", http.StatusBadRequest)
			return
		}
		var g types.ArtifactTransferGrant
		decoder := json.NewDecoder(bytes.NewReader(b))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&g); err != nil {
			http.Error(w, "invalid_transfer", http.StatusBadRequest)
			return
		}
		var extra any
		if err = decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			http.Error(w, "invalid_transfer", http.StatusBadRequest)
			return
		}
		probe := r.Header.Get(probeHeader) == "true"
		receipt, err := s.ImportAdmission(r.Context(), g, probe)
		if err != nil {
			writePeerError(w, err)
			return
		}
		if !probe {
			if r.Header.Get("Content-Type") != "application/octet-stream" {
				http.Error(w, "invalid_transfer", http.StatusBadRequest)
				return
			}
			if r.ContentLength >= 0 && r.ContentLength != g.SizeBytes {
				http.Error(w, "invalid_transfer_size", http.StatusBadRequest)
				return
			}
			data, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, g.SizeBytes+1))
			if readErr != nil || int64(len(data)) != g.SizeBytes {
				http.Error(w, "invalid_transfer_size", http.StatusRequestEntityTooLarge)
				return
			}
			receipt, err = s.Import(r.Context(), g, data)
			if err != nil {
				writePeerError(w, err)
				return
			}
		} else if r.ContentLength > 0 {
			http.Error(w, "invalid_probe", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err = json.NewEncoder(w).Encode(receipt); err != nil {
			return
		}
	})
}

func writePeerError(w http.ResponseWriter, err error) {
	code, status := "not_authorized", http.StatusForbidden
	switch {
	case errors.Is(err, ErrRevoked), errors.Is(err, artifacts.ErrScopeFenced):
		code = "revoked"
	case errors.Is(err, ErrExpired):
		code = "expired"
		status = http.StatusGone
	case errors.Is(err, ErrConflict):
		code = "conflict"
		status = http.StatusConflict
	case errors.Is(err, ErrNotFound):
		code = "not_found"
		status = http.StatusNotFound
	case errors.Is(err, ErrInvalid):
		code = "invalid"
		status = http.StatusBadRequest
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if encodeErr := json.NewEncoder(w).Encode(struct {
		Code string `json:"code"`
	}{code}); encodeErr != nil {
		return
	}
}
func decodePeerError(resp *http.Response) error {
	var wire struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1024)).Decode(&wire); err != nil {
		return fmt.Errorf("transfer peer status %d: %w", resp.StatusCode, ErrUnauthorized)
	}
	switch wire.Code {
	case "revoked":
		return ErrRevoked
	case "expired":
		return ErrExpired
	case "conflict":
		return ErrConflict
	case "not_found":
		return ErrNotFound
	case "invalid":
		return ErrInvalid
	default:
		return ErrUnauthorized
	}
}
