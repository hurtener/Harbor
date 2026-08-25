package httptransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/receipts"
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
