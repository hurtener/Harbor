package httptransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/receipts"
	llmtopup "github.com/hurtener/Harbor/sdk/llm/topup"
)

// testServiceToken is a documented dummy fixture, never a real credential.
const testServiceToken = "fixture-runtime-service-token"

func TestClient_DeliverBatch_AuthenticatesCanonicalPartialAck(t *testing.T) {
	receiptA := validReceipt(t, "receipt-a")
	receiptB := validReceipt(t, "receipt-b")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testServiceToken {
			t.Errorf("authorization = %q", got)
		}
		var req receiptBatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Version != contractVersion || len(req.Receipts) != 2 {
			t.Errorf("request = %#v", req)
		}
		var canonical map[string]any
		if err := json.Unmarshal(req.Receipts[0], &canonical); err != nil {
			t.Errorf("canonical receipt: %v", err)
		}
		if canonical["receipt_id"] != receiptA.ReceiptID {
			t.Errorf("receipt_id = %v", canonical["receipt_id"])
		}
		_ = json.NewEncoder(w).Encode(receiptBatchResponse{Version: contractVersion, Acks: []receipts.DeliveryAck{{ReceiptID: receiptA.ReceiptID, CanonicalBodyHash: receiptA.CanonicalBodyHash}}})
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	acks, err := client.DeliverBatch(context.Background(), []llm.AttemptUsageReceipt{receiptA, receiptB})
	if err != nil {
		t.Fatalf("DeliverBatch: %v", err)
	}
	if len(acks) != 1 || acks[0].ReceiptID != receiptA.ReceiptID {
		t.Fatalf("acks = %#v", acks)
	}
	if got := client.Readiness().Receipt; got != "degraded" {
		t.Fatalf("receipt readiness = %q, want degraded after partial ack", got)
	}
}

func TestClient_DeliverBatch_PreservesLegacyCanonicalWire(t *testing.T) {
	receipt := validLegacyReceipt(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req receiptBatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if len(req.Receipts) != 1 {
			t.Errorf("receipt count = %d, want 1", len(req.Receipts))
			return
		}
		body := []byte(req.Receipts[0])
		if bytes.Contains(body, []byte(`"route_mode"`)) || !bytes.Contains(body, []byte(`"ReceiptID"`)) {
			t.Errorf("legacy wire shape changed: %s", body)
			return
		}
		parsed, err := llm.UnmarshalCanonicalAttemptUsageReceipt(body)
		if err != nil {
			t.Errorf("strict parse: %v", err)
			return
		}
		if parsed.RouteMode != "" || parsed.CanonicalBodyHash != receipt.CanonicalBodyHash {
			t.Errorf("parsed legacy receipt = %#v", parsed)
			return
		}
		roundTrip, err := llm.MarshalCanonicalAttemptUsageReceipt(parsed)
		if err != nil || !bytes.Equal(roundTrip, body) {
			t.Errorf("legacy re-marshal = %s, %v; want %s", roundTrip, err, body)
			return
		}
		_ = json.NewEncoder(w).Encode(receiptBatchResponse{Version: contractVersion, Acks: []receipts.DeliveryAck{{ReceiptID: receipt.ReceiptID, CanonicalBodyHash: receipt.CanonicalBodyHash}}})
	}))
	defer server.Close()

	acks, err := newTestClient(t, server.URL).DeliverBatch(context.Background(), []llm.AttemptUsageReceipt{receipt})
	if err != nil {
		t.Fatalf("DeliverBatch: %v", err)
	}
	if len(acks) != 1 || acks[0].CanonicalBodyHash != receipt.CanonicalBodyHash {
		t.Fatalf("acks = %#v", acks)
	}
}

func TestClient_DeliverBatch_StrictFailureMatrix(t *testing.T) {
	receipt := validReceipt(t, "receipt-strict")
	oversize := strings.Repeat("x", maxResponseBytes+1)
	tests := map[string]struct {
		body string
	}{
		"unknown":    {body: `{"version":1,"acks":[],"extra":true}`},
		"duplicate":  {body: `{"version":1,"version":1,"acks":[]}`},
		"trailing":   {body: `{"version":1,"acks":[]} {}`},
		"wrong hash": {body: fmt.Sprintf(`{"version":1,"acks":[{"receipt_id":%q,"canonical_body_hash":"wrong"}]}`, receipt.ReceiptID)},
		"oversize":   {body: oversize},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client := newTestClient(t, server.URL)
			_, err := client.DeliverBatch(context.Background(), []llm.AttemptUsageReceipt{receipt})
			if !errors.Is(err, ErrDelivery) {
				t.Fatalf("error = %v, want ErrDelivery", err)
			}
			if strings.Contains(err.Error(), testServiceToken) || strings.Contains(err.Error(), server.URL) {
				t.Fatalf("error leaked transport material: %v", err)
			}
		})
	}
}

func TestClient_DeliverBatch_RefusesRedirectAndHonorsCancellation(t *testing.T) {
	receipt := validReceipt(t, "receipt-redirect")
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("redirect target must not be reached")
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	client := newTestClient(t, redirect.URL)
	if _, err := client.DeliverBatch(context.Background(), []llm.AttemptUsageReceipt{receipt}); !errors.Is(err, ErrDelivery) {
		t.Fatalf("redirect error = %v", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	client = newTestClient(t, blocked.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.DeliverBatch(ctx, []llm.AttemptUsageReceipt{receipt})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrDelivery) {
			t.Fatalf("cancellation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("delivery did not stop after cancellation")
	}
	close(release)
	blocked.Close()
}

func TestClient_ConcurrentReuse(t *testing.T) {
	receipt := validReceipt(t, "receipt-concurrent")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(receiptBatchResponse{Version: contractVersion, Acks: []receipts.DeliveryAck{{ReceiptID: receipt.ReceiptID, CanonicalBodyHash: receipt.CanonicalBodyHash}}})
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := client.DeliverBatch(context.Background(), []llm.AttemptUsageReceipt{receipt})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent delivery: %v", err)
		}
	}
}

func TestClient_TopUp_AuthenticatedCanonicalResponseLossAndConcurrentReuse(t *testing.T) {
	predecessor := validTopUpGrant()
	var calls atomic.Int32
	var mu sync.Mutex
	type observedRequest struct {
		key    string
		reason llm.ExternalGrantRenewalReason
	}
	requests := make([]observedRequest, 0, 104)
	failedByKey := make(map[string]bool)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testServiceToken {
			t.Errorf("authorization=%q", got)
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, llmtopup.MaxRequestBytes+1))
		if err != nil || len(body) > llmtopup.MaxRequestBytes {
			t.Errorf("bounded request: %v bytes=%d", err, len(body))
			return
		}
		request, err := llmtopup.ParseCanonicalRequest(body)
		if err != nil {
			t.Errorf("parse request: %v", err)
			return
		}
		if err := llmtopup.ValidateIdempotencyHeader(r.Header.Get("Idempotency-Key"), request.IdempotencyKey); err != nil {
			t.Errorf("request idempotency: %v", err)
			return
		}
		mu.Lock()
		requests = append(requests, observedRequest{key: request.IdempotencyKey, reason: request.RenewalReason})
		firstForKey := !failedByKey[request.IdempotencyKey]
		failedByKey[request.IdempotencyKey] = true
		mu.Unlock()
		calls.Add(1)
		if firstForKey {
			http.Error(w, "stored but response unavailable", http.StatusServiceUnavailable)
			return
		}
		successor := topUpSuccessor(request)
		response, err := llmtopup.NewResponse(request, successor)
		if err != nil {
			t.Errorf("response: %v", err)
			return
		}
		payload, err := llmtopup.MarshalCanonicalResponse(request, response)
		if err != nil {
			t.Errorf("marshal response: %v", err)
			return
		}
		w.Header().Set("Idempotency-Key", request.IdempotencyKey)
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	client, err := New(Config{ReceiptURL: server.URL, TopUpURL: server.URL, AuthToken: testServiceToken, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("constructor made %d idle calls", got)
	}
	if _, err := client.TopUp(context.Background(), predecessor, 100); !errors.Is(err, ErrTopUp) {
		t.Fatalf("simulated response loss=%v", err)
	}
	if _, err := client.TopUp(context.Background(), predecessor, 100); err != nil {
		t.Fatalf("response-loss replay: %v", err)
	}
	if _, err := client.Renew(context.Background(), predecessor, 100, llm.ExternalGrantRenewalLeaseInsufficient); !errors.Is(err, ErrTopUp) {
		t.Fatalf("reasoned simulated response loss=%v", err)
	}
	if _, err := client.Renew(context.Background(), predecessor, 100, llm.ExternalGrantRenewalLeaseInsufficient); err != nil {
		t.Fatalf("reasoned response-loss replay: %v", err)
	}

	const concurrent = 100
	errs := make(chan error, concurrent)
	var wg sync.WaitGroup
	for range concurrent {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := client.TopUp(context.Background(), predecessor, 100)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent top-up: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	keysByReason := make(map[llm.ExternalGrantRenewalReason]string)
	for _, request := range requests {
		if prior := keysByReason[request.reason]; prior != "" && prior != request.key {
			t.Fatalf("identical predecessor/units/reason changed idempotency key for %q", request.reason)
		}
		keysByReason[request.reason] = request.key
	}
	if keysByReason[llm.ExternalGrantRenewalExpired] == keysByReason[llm.ExternalGrantRenewalLeaseInsufficient] {
		t.Fatal("different renewal reasons reused the idempotency key")
	}
	if readiness := client.Readiness(); readiness.TopUp != "wired" {
		t.Fatalf("top-up readiness=%+v", readiness)
	}
}

func TestClient_TopUp_StrictResponseAndNetworkFailureMatrix(t *testing.T) {
	predecessor := validTopUpGrant()
	tests := map[string]struct {
		mutate func([]byte) []byte
		header func(string) string
	}{
		"missing header": {header: func(string) string { return "" }},
		"wrong header":   {header: func(string) string { return "sha256:wrong" }},
		"duplicate key": {mutate: func(body []byte) []byte {
			return bytes.Replace(body, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1)
		}},
		"unknown field": {mutate: func(body []byte) []byte {
			return bytes.Replace(body, []byte(`"successor"`), []byte(`"unknown":true,"successor"`), 1)
		}},
		"trailing": {mutate: func(body []byte) []byte { return append(body, []byte(` {}`)...) }},
		"oversize": {mutate: func([]byte) []byte { return bytes.Repeat([]byte("x"), llmtopup.MaxResponseBytes+1) }},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				request, err := llmtopup.ParseCanonicalRequest(body)
				if err != nil {
					t.Errorf("request: %v", err)
					return
				}
				response, err := llmtopup.NewResponse(request, topUpSuccessor(request))
				if err != nil {
					t.Errorf("response: %v", err)
					return
				}
				payload, _ := llmtopup.MarshalCanonicalResponse(request, response)
				if tc.mutate != nil {
					payload = tc.mutate(payload)
				}
				header := request.IdempotencyKey
				if tc.header != nil {
					header = tc.header(header)
				}
				if header != "" {
					w.Header().Set("Idempotency-Key", header)
				}
				_, _ = w.Write(payload)
			}))
			defer server.Close()
			client, err := New(Config{ReceiptURL: server.URL, TopUpURL: server.URL, AuthToken: testServiceToken, Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.TopUp(context.Background(), predecessor, 100); !errors.Is(err, ErrTopUp) {
				t.Fatalf("strict response=%v", err)
			}
		})
	}

	t.Run("cancellation", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			close(started)
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		defer func() { close(release); server.Close() }()
		client, _ := New(Config{ReceiptURL: server.URL, TopUpURL: server.URL, AuthToken: testServiceToken, Timeout: time.Second})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := client.TopUp(ctx, predecessor, 100); done <- err }()
		<-started
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, ErrTopUp) {
				t.Fatalf("cancellation=%v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("top-up ignored cancellation")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		defer func() { close(release); server.Close() }()
		client, _ := New(Config{ReceiptURL: server.URL, TopUpURL: server.URL, AuthToken: testServiceToken, Timeout: 20 * time.Millisecond})
		if _, err := client.TopUp(context.Background(), predecessor, 100); !errors.Is(err, ErrTopUp) {
			t.Fatalf("timeout=%v", err)
		}
	})

	t.Run("redirect", func(t *testing.T) {
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("redirect followed") }))
		defer target.Close()
		redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
		}))
		defer redirect.Close()
		client, _ := New(Config{ReceiptURL: redirect.URL, TopUpURL: redirect.URL, AuthToken: testServiceToken, Timeout: time.Second})
		if _, err := client.TopUp(context.Background(), predecessor, 100); !errors.Is(err, ErrTopUp) {
			t.Fatalf("redirect=%v", err)
		}
	})
}

func validTopUpGrant() llm.ExternalGrant {
	issued := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	return llm.ExternalGrant{
		Version: llm.ExternalGrantVersionAgentBound, KeyID: "key-a", Audience: "harbor-runtime", GrantID: "grant-top-up",
		RouteMode: llm.ExternalGrantRouteRuntimeDefault, OrganizationID: "org-a", RuntimeID: "runtime-a", AgentID: "agent-a",
		TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a", LogicalRunID: "run-a", LogicalCallID: "call-a", AttemptNonce: "nonce-a",
		PolicyGeneration: 1, MaxReasoning: llm.ReasoningMedium, MaxOutputTokens: 100,
		Lease:    llm.ComputeLease{LeaseID: "lease-a", Epoch: 1, TokenUnits: 200, ExpiresAt: issued.Add(10 * time.Minute)},
		IssuedAt: issued, ExpiresAt: issued.Add(5 * time.Minute), Signature: "signature-a",
	}
}

func topUpSuccessor(request llmtopup.Request) llm.ExternalGrant {
	successor := request.Predecessor
	successor.KeyID, successor.Signature = "key-b", "signature-b"
	successor.Lease.Epoch++
	if request.RenewalReason == llm.ExternalGrantRenewalLeaseInsufficient {
		successor.Lease.TokenUnits += request.RequestedUnits
	}
	successor.IssuedAt = successor.IssuedAt.Add(time.Minute)
	successor.ExpiresAt = successor.ExpiresAt.Add(time.Minute)
	successor.Lease.ExpiresAt = successor.Lease.ExpiresAt.Add(time.Minute)
	return successor
}

func newTestClient(t *testing.T, endpoint string) *Client {
	t.Helper()
	client, err := New(Config{ReceiptURL: endpoint, AuthToken: testServiceToken, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func validReceipt(t *testing.T, id string) llm.AttemptUsageReceipt {
	t.Helper()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	receipt := llm.AttemptUsageReceipt{
		ReceiptID: id, GrantID: "grant-1", RouteMode: llm.ExternalGrantRouteRuntimeDefault,
		LogicalCallID: "call-1", AttemptNonce: "nonce-1", OrganizationID: "org-1",
		RuntimeID: "runtime-1", TenantID: "tenant-1", UserID: "user-1", SessionID: "session-1",
		LogicalRunID: "run-1", Provider: "provider-1", ProviderModelID: "model-1",
		PolicyGeneration: 1, AttemptNumber: 1, Status: "success", StartedAt: now,
		CompletedAt: now.Add(time.Second), IdempotencyKey: id,
	}
	hash, err := llm.CanonicalAttemptUsageReceiptBodyHash(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receipt.CanonicalBodyHash = hash
	return receipt
}

func validLegacyReceipt(t *testing.T) llm.AttemptUsageReceipt {
	t.Helper()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	receipt := llm.AttemptUsageReceipt{
		GrantID: "grant-legacy", LogicalCallID: "call-legacy", AttemptNonce: "nonce-legacy",
		OrganizationID: "org-1", RuntimeID: "runtime-1", TenantID: "tenant-1", UserID: "user-1",
		SessionID: "session-1", LogicalRunID: "run-1", Provider: "provider-1", ProviderModelID: "model-1",
		ProviderConnectionID: "connection-1", ProviderConnectionGeneration: 3, RouteID: "route-1",
		CredentialAssetGeneration: 5, PolicyGeneration: 1, AttemptNumber: 1, Status: "success",
		StartedAt: now, CompletedAt: now.Add(time.Second),
	}
	receipt.ReceiptID = llm.CanonicalAttemptID(receipt.GrantID, receipt.LogicalCallID, receipt.AttemptNonce, receipt.AttemptNumber, receipt.RetryNumber, receipt.DowngradeNumber, receipt.FallbackHop)
	receipt.IdempotencyKey = receipt.ReceiptID
	hash, err := llm.CanonicalAttemptUsageReceiptBodyHash(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receipt.CanonicalBodyHash = hash
	return receipt
}
