package topup_test

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	llmsdk "github.com/hurtener/Harbor/sdk/llm"
	"github.com/hurtener/Harbor/sdk/llm/topup"
)

func TestPublicContract_CanonicalRoundTripAndExpiryOnlyCapacityPreservation(t *testing.T) {
	predecessor, successor := renewalPair()
	request, err := topup.NewRequest(predecessor, 100)
	if err != nil {
		t.Fatal(err)
	}
	if request.RenewalReason != llmsdk.ExternalGrantRenewalExpired {
		t.Fatalf("reason=%q", request.RenewalReason)
	}
	requestBytes, err := topup.MarshalCanonicalRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	parsedRequest, err := topup.ParseCanonicalRequest(requestBytes)
	if err != nil {
		t.Fatal(err)
	}
	if parsedRequest.IdempotencyKey != request.IdempotencyKey || parsedRequest.Predecessor.Signature != predecessor.Signature {
		t.Fatalf("parsed request=%+v", parsedRequest)
	}
	response, err := topup.NewResponse(request, successor)
	if err != nil {
		t.Fatal(err)
	}
	responseBytes, err := topup.MarshalCanonicalResponse(request, response)
	if err != nil {
		t.Fatal(err)
	}
	parsedResponse, err := topup.ParseCanonicalResponse(request, responseBytes)
	if err != nil {
		t.Fatal(err)
	}
	if parsedResponse.Successor.Lease.TokenUnits != predecessor.Lease.TokenUnits || parsedResponse.Successor.Lease.ConsumedUnits != predecessor.Lease.ConsumedUnits {
		t.Fatal("expiry-only response widened compute")
	}
	if err := topup.ValidateIdempotencyHeader(request.IdempotencyKey, parsedResponse.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
}

func TestPublicContract_IdempotencyAndCanonicalAdversaries(t *testing.T) {
	predecessor, successor := renewalPair()
	request, err := topup.NewRequest(predecessor, 100)
	if err != nil {
		t.Fatal(err)
	}
	same, _ := topup.TopUpIdempotencyKey(predecessor, 100)
	if same != request.IdempotencyKey {
		t.Fatal("same request changed idempotency identity")
	}
	changedUnits, _ := topup.TopUpIdempotencyKey(predecessor, 101)
	changedGrant := predecessor
	changedGrant.Signature += "x"
	changedBytes, _ := topup.TopUpIdempotencyKey(changedGrant, 100)
	if changedUnits == same || changedBytes == same {
		t.Fatal("changed request reused idempotency identity")
	}
	insufficientKey, err := topup.RenewalIdempotencyKey(predecessor, 100, llmsdk.ExternalGrantRenewalLeaseInsufficient)
	if err != nil || insufficientKey == same {
		t.Fatalf("different renewal reason reused identity: key=%q err=%v", insufficientKey, err)
	}
	reasoned, err := topup.NewRequestForReason(predecessor, 100, llmsdk.ExternalGrantRenewalLeaseInsufficient)
	if err != nil || reasoned.IdempotencyKey != insufficientKey {
		t.Fatalf("reason-aware request=%+v err=%v", reasoned, err)
	}

	requestBytes, _ := topup.MarshalCanonicalRequest(request)
	predecessorBytes, _ := llmsdk.MarshalCanonicalExternalGrant(predecessor)
	reorderedRequest := []byte(fmt.Sprintf(`{"idempotency_key":%q,"version":1,"requested_units":100,"renewal_reason":%q,"predecessor":%s}`,
		request.IdempotencyKey, request.RenewalReason, predecessorBytes))
	requestMutations := [][]byte{
		append([]byte(" "), requestBytes...),
		bytes.Replace(requestBytes, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		bytes.Replace(requestBytes, []byte(`"grant_id":"grant-a"`), []byte(`"grant_id":"grant-a","grant_id":"grant-a"`), 1),
		bytes.Replace(requestBytes, []byte(`"requested_units":100`), []byte(`"requested_units":100,"unknown":true`), 1),
		append(append([]byte(nil), requestBytes...), []byte(` {}`)...),
		reorderedRequest,
	}
	for i, mutated := range requestMutations {
		if _, err := topup.ParseCanonicalRequest(mutated); !errors.Is(err, topup.ErrInvalidRequest) {
			t.Fatalf("request mutation %d=%v", i, err)
		}
	}

	response, err := topup.NewResponse(request, successor)
	if err != nil {
		t.Fatal(err)
	}
	responseBytes, _ := topup.MarshalCanonicalResponse(request, response)
	successorBytes, _ := llmsdk.MarshalCanonicalExternalGrant(successor)
	reorderedResponse := []byte(fmt.Sprintf(`{"idempotency_key":%q,"version":1,"successor":%s}`, response.IdempotencyKey, successorBytes))
	responseMutations := [][]byte{
		append([]byte(" "), responseBytes...),
		bytes.Replace(responseBytes, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		bytes.Replace(responseBytes, []byte(`"grant_id":"grant-a"`), []byte(`"grant_id":"grant-a","grant_id":"grant-a"`), 1),
		bytes.Replace(responseBytes, []byte(`"idempotency_key"`), []byte(`"unknown":true,"idempotency_key"`), 1),
		reorderedResponse,
	}
	for i, mutated := range responseMutations {
		if _, err := topup.ParseCanonicalResponse(request, mutated); !errors.Is(err, topup.ErrInvalidResponse) {
			t.Fatalf("response mutation %d=%v", i, err)
		}
	}
	if err := topup.ValidateIdempotencyHeader("different", request.IdempotencyKey); !errors.Is(err, topup.ErrIdempotencyMismatch) {
		t.Fatalf("header mismatch=%v", err)
	}
}

func TestPublicContract_BoundsAndInsufficientLeaseReason(t *testing.T) {
	predecessor, successor := renewalPair()
	predecessor.Lease.TokenUnits = 10
	successor.Lease.TokenUnits = 110
	request, err := topup.NewRequest(predecessor, 100)
	if err != nil {
		t.Fatal(err)
	}
	if request.RenewalReason != llmsdk.ExternalGrantRenewalLeaseInsufficient {
		t.Fatalf("reason=%q", request.RenewalReason)
	}
	if _, err := topup.NewResponse(request, successor); err != nil {
		t.Fatal(err)
	}
	if _, err := topup.NewRequest(predecessor, topup.MaxRequestedUnits+1); !errors.Is(err, topup.ErrInvalidRequest) {
		t.Fatalf("unit overflow=%v", err)
	}
	predecessor.Signature = strings.Repeat("x", topup.MaxCanonicalGrantBytes)
	if _, err := topup.NewRequest(predecessor, 1); !errors.Is(err, topup.ErrInvalidRequest) {
		t.Fatalf("oversize predecessor=%v", err)
	}
}

func TestPublicContract_ValidationFailureMatrix(t *testing.T) {
	predecessor, successor := renewalPair()
	request, err := topup.NewRequest(predecessor, 100)
	if err != nil {
		t.Fatal(err)
	}
	response, err := topup.NewResponse(request, successor)
	if err != nil {
		t.Fatal(err)
	}
	invalidReason := llmsdk.ExternalGrantRenewalReason("invalid")
	if _, err := topup.NewRequestForReason(predecessor, 100, invalidReason); !errors.Is(err, topup.ErrInvalidRequest) {
		t.Fatalf("invalid constructor reason=%v", err)
	}
	if _, err := topup.RenewalIdempotencyKey(predecessor, 0, llmsdk.ExternalGrantRenewalExpired); !errors.Is(err, topup.ErrInvalidRequest) {
		t.Fatalf("zero key units=%v", err)
	}
	if _, err := topup.RenewalIdempotencyKey(predecessor, 100, invalidReason); !errors.Is(err, topup.ErrInvalidRequest) {
		t.Fatalf("invalid key reason=%v", err)
	}

	requestMutations := []func(*topup.Request){
		func(r *topup.Request) { r.Version++ },
		func(r *topup.Request) { r.RequestedUnits = 0 },
		func(r *topup.Request) { r.RenewalReason = invalidReason },
		func(r *topup.Request) { r.IdempotencyKey = "sha256:wrong" },
		func(r *topup.Request) {
			r.RenewalReason = llmsdk.ExternalGrantRenewalExpired
			r.RequestedUnits = r.Predecessor.Lease.RemainingTokens() + 1
		},
	}
	for i, mutate := range requestMutations {
		invalid := request
		mutate(&invalid)
		if err := topup.ValidateRequest(invalid); !errors.Is(err, topup.ErrInvalidRequest) {
			t.Fatalf("invalid request %d=%v", i, err)
		}
		if _, err := topup.MarshalCanonicalRequest(invalid); !errors.Is(err, topup.ErrInvalidRequest) {
			t.Fatalf("marshal invalid request %d=%v", i, err)
		}
	}
	if _, err := topup.NewResponse(topup.Request{}, successor); !errors.Is(err, topup.ErrInvalidRequest) {
		t.Fatalf("response from invalid request=%v", err)
	}
	for i, invalid := range []topup.Response{
		{Version: response.Version + 1, IdempotencyKey: response.IdempotencyKey, Successor: response.Successor},
		{Version: response.Version, IdempotencyKey: "sha256:wrong", Successor: response.Successor},
		{Version: response.Version, IdempotencyKey: response.IdempotencyKey, Successor: predecessor},
	} {
		if err := topup.ValidateResponse(request, invalid); !errors.Is(err, topup.ErrInvalidResponse) {
			t.Fatalf("invalid response %d=%v", i, err)
		}
		if _, err := topup.MarshalCanonicalResponse(request, invalid); !errors.Is(err, topup.ErrInvalidResponse) {
			t.Fatalf("marshal invalid response %d=%v", i, err)
		}
	}
	if _, err := topup.ParseCanonicalRequest(nil); !errors.Is(err, topup.ErrInvalidRequest) {
		t.Fatalf("empty request=%v", err)
	}
	if _, err := topup.ParseCanonicalRequest(bytes.Repeat([]byte("x"), topup.MaxRequestBytes+1)); !errors.Is(err, topup.ErrInvalidRequest) {
		t.Fatalf("oversize request=%v", err)
	}
	if _, err := topup.ParseCanonicalResponse(request, nil); !errors.Is(err, topup.ErrInvalidResponse) {
		t.Fatalf("empty response=%v", err)
	}
	if _, err := topup.ParseCanonicalResponse(request, bytes.Repeat([]byte("x"), topup.MaxResponseBytes+1)); !errors.Is(err, topup.ErrInvalidResponse) {
		t.Fatalf("oversize response=%v", err)
	}
}

func renewalPair() (llmsdk.ExternalGrant, llmsdk.ExternalGrant) {
	issued := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	predecessor := llmsdk.ExternalGrant{
		Version: llmsdk.ExternalGrantVersionAgentBound, KeyID: "key-a", Audience: "harbor-runtime", GrantID: "grant-a",
		RouteMode: llmsdk.ExternalGrantRouteRuntimeDefault, OrganizationID: "org-a", RuntimeID: "runtime-a", AgentID: "agent-a",
		TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a", LogicalRunID: "run-a", LogicalCallID: "call-a", AttemptNonce: "nonce-a",
		PolicyGeneration: 1, MaxReasoning: llmsdk.ReasoningMedium, MaxOutputTokens: 100,
		Lease:    llmsdk.ComputeLease{LeaseID: "lease-a", Epoch: 1, TokenUnits: 200, ExpiresAt: issued.Add(10 * time.Minute)},
		IssuedAt: issued, ExpiresAt: issued.Add(5 * time.Minute), Signature: "signature-a",
	}
	successor := predecessor
	successor.KeyID = "key-b"
	successor.Lease.Epoch++
	successor.IssuedAt = issued.Add(time.Minute)
	successor.ExpiresAt = issued.Add(6 * time.Minute)
	successor.Lease.ExpiresAt = issued.Add(11 * time.Minute)
	successor.Signature = "signature-b"
	return predecessor, successor
}
