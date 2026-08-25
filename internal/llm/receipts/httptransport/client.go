// Package httptransport delivers canonical content-free usage receipts over
// one boot-pinned authenticated HTTP path.
package httptransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/receipts"
)

const (
	contractVersion        = 1
	defaultTimeout         = 10 * time.Second
	defaultMaxBatch        = 64
	maxRequestBytes        = 4 << 20
	maxResponseBytes       = 1 << 16
	transportStateWired    = 0
	transportStateReady    = 1
	transportStateDegraded = 2
)

var (
	// ErrInvalidConfig identifies an unsafe or incomplete boot transport.
	ErrInvalidConfig = errors.New("llm/receipts/httptransport: invalid configuration")
	// ErrDelivery identifies a bounded receipt delivery failure.
	ErrDelivery = errors.New("llm/receipts/httptransport: receipt delivery failed")
)

// Config is immutable constructor input. AuthToken is the already-resolved
// runtime service credential and must never be logged or serialized.
type Config struct {
	ReceiptURL string
	AuthToken  string
	Timeout    time.Duration
	MaxBatch   int
	HTTPClient *http.Client
}

// Readiness is a secret-free structural/observed transport projection.
type Readiness struct {
	Receipt string
}

// Client is safe for concurrent reuse. Its endpoints and credential are
// immutable; atomics retain only the latest secret-free transport state.
type Client struct {
	receiptURL   string
	authToken    string
	maxBatch     int
	httpClient   *http.Client
	receiptState atomic.Int32
	outboxState  atomic.Int32
}

// New constructs a boot-pinned authenticated client without making a network
// request or starting a goroutine.
func New(cfg Config) (*Client, error) {
	if err := validateEndpoint(cfg.ReceiptURL); err != nil {
		return nil, fmt.Errorf("%w: receipt endpoint", ErrInvalidConfig)
	}
	if strings.TrimSpace(cfg.AuthToken) == "" {
		return nil, fmt.Errorf("%w: authentication token is empty", ErrInvalidConfig)
	}
	if cfg.Timeout < 0 {
		return nil, fmt.Errorf("%w: timeout is negative", ErrInvalidConfig)
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	maxBatch := cfg.MaxBatch
	if maxBatch == 0 {
		maxBatch = defaultMaxBatch
	}
	if maxBatch < 1 || maxBatch > 1000 {
		return nil, fmt.Errorf("%w: max batch must be between 1 and 1000", ErrInvalidConfig)
	}
	var hc *http.Client
	if cfg.HTTPClient == nil {
		hc = &http.Client{Timeout: timeout}
	} else {
		clone := *cfg.HTTPClient
		clone.Timeout = timeout
		hc = &clone
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error {
		return fmt.Errorf("%w: redirect refused", ErrDelivery)
	}
	return &Client{
		receiptURL: cfg.ReceiptURL,
		authToken:  cfg.AuthToken,
		maxBatch:   maxBatch,
		httpClient: hc,
	}, nil
}

// Readiness reports whether each configured path is structurally wired or
// degraded after its most recent attempted exchange.
func (c *Client) Readiness() Readiness {
	state := c.receiptState.Load()
	if c.outboxState.Load() == transportStateDegraded {
		state = transportStateDegraded
	}
	return Readiness{
		Receipt: stateName(state, true),
	}
}

// SetOutboxHealth receives the secret-free durable-worker health projection.
// It is called by the outbox after startup and every maintenance retry; no
// endpoint, credential, receipt, or identity crosses this seam.
func (c *Client) SetOutboxHealth(healthy bool) {
	if healthy {
		c.outboxState.Store(transportStateReady)
		return
	}
	c.outboxState.Store(transportStateDegraded)
}

// Deliver preserves the transport-neutral single-receipt interface. The
// durable outbox detects DeliverBatch and uses the bounded batch path.
func (c *Client) Deliver(ctx context.Context, receipt llm.AttemptUsageReceipt) error {
	acks, err := c.DeliverBatch(ctx, []llm.AttemptUsageReceipt{receipt})
	if err != nil {
		return err
	}
	if len(acks) != 1 {
		return fmt.Errorf("%w: receipt was not acknowledged", ErrDelivery)
	}
	return nil
}

// DeliverBatch sends canonical receipt objects and accepts only exact
// receipt-id/body-hash acknowledgements. A partial acknowledgement is returned
// as-is so the durable outbox can delete only the acknowledged facts.
func (c *Client) DeliverBatch(ctx context.Context, batch []llm.AttemptUsageReceipt) ([]receipts.DeliveryAck, error) {
	if len(batch) == 0 || len(batch) > c.maxBatch {
		return nil, fmt.Errorf("%w: batch size is outside the configured bound", ErrDelivery)
	}
	rawReceipts := make([]json.RawMessage, len(batch))
	expected := make(map[string]string, len(batch))
	for i, receipt := range batch {
		if err := llm.ValidateAttemptUsageReceipt(receipt); err != nil || receipt.ReceiptID != receipt.IdempotencyKey {
			return nil, fmt.Errorf("%w: invalid canonical receipt", ErrDelivery)
		}
		if _, duplicate := expected[receipt.ReceiptID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate receipt identity", ErrDelivery)
		}
		body, err := llm.MarshalCanonicalAttemptUsageReceipt(receipt)
		if err != nil {
			return nil, fmt.Errorf("%w: canonical receipt marshal", ErrDelivery)
		}
		rawReceipts[i] = body
		expected[receipt.ReceiptID] = receipt.CanonicalBodyHash
	}
	payload, err := json.Marshal(receiptBatchRequest{Version: contractVersion, Receipts: rawReceipts})
	if err != nil || len(payload) > maxRequestBytes {
		return nil, fmt.Errorf("%w: request exceeds the bounded canonical envelope", ErrDelivery)
	}
	var response receiptBatchResponse
	if err := c.post(ctx, c.receiptURL, payload, &response, ErrDelivery); err != nil {
		c.receiptState.Store(transportStateDegraded)
		return nil, err
	}
	if response.Version != contractVersion || len(response.Acks) > len(batch) {
		c.receiptState.Store(transportStateDegraded)
		return nil, fmt.Errorf("%w: invalid acknowledgement envelope", ErrDelivery)
	}
	seen := make(map[string]struct{}, len(response.Acks))
	for _, ack := range response.Acks {
		want, ok := expected[ack.ReceiptID]
		if !ok || want != ack.CanonicalBodyHash {
			c.receiptState.Store(transportStateDegraded)
			return nil, fmt.Errorf("%w: acknowledgement identity or hash mismatch", ErrDelivery)
		}
		if _, duplicate := seen[ack.ReceiptID]; duplicate {
			c.receiptState.Store(transportStateDegraded)
			return nil, fmt.Errorf("%w: duplicate acknowledgement", ErrDelivery)
		}
		seen[ack.ReceiptID] = struct{}{}
	}
	if len(response.Acks) == len(batch) {
		c.receiptState.Store(transportStateReady)
	} else {
		c.receiptState.Store(transportStateDegraded)
	}
	return append([]receipts.DeliveryAck(nil), response.Acks...), nil
}

func (c *Client) post(ctx context.Context, endpoint string, payload []byte, out any, sentinel error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("%w: build request", sentinel)
	}
	req.Header.Set("Authorization", "Bearer "+c.authToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: authenticated request failed", sentinel)
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if readErr != nil {
		return fmt.Errorf("%w: response read failed", sentinel)
	}
	if len(raw) > maxResponseBytes {
		return fmt.Errorf("%w: response exceeds byte bound", sentinel)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%w: coordinator returned status %d", sentinel, resp.StatusCode)
	}
	if err := decodeStrict(raw, out); err != nil {
		return fmt.Errorf("%w: malformed response", sentinel)
	}
	return nil
}

type receiptBatchRequest struct {
	Version  int               `json:"version"`
	Receipts []json.RawMessage `json:"receipts"`
}

type receiptBatchResponse struct {
	Version int                    `json:"version"`
	Acks    []receipts.DeliveryAck `json:"acks"`
}

func decodeStrict(raw []byte, out any) error {
	if err := rejectDuplicateKeys(raw); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func rejectDuplicateKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := consumeJSONValue(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func consumeJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("duplicate object field")
			}
			seen[key] = struct{}{}
			if err := consumeJSONValue(dec); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("malformed object")
		}
	case '[':
		for dec.More() {
			if err := consumeJSONValue(dec); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("malformed array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

func validateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ErrInvalidConfig
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ErrInvalidConfig
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return ErrInvalidConfig
	}
	return nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func stateName(state int32, configured bool) string {
	if !configured {
		return "absent"
	}
	if state == transportStateDegraded {
		return "degraded"
	}
	return "wired"
}
