package llm

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func canonicalReceiptFixture(t testing.TB) (AttemptUsageReceipt, []byte) {
	t.Helper()
	started := time.Date(2026, 8, 25, 12, 0, 0, 123_000_000, time.UTC)
	receipt := AttemptUsageReceipt{
		ReceiptID:           "grant-1/call-1/nonce-1/0/0/0/1",
		GrantID:             "grant-1",
		RouteMode:           ExternalGrantRouteRuntimeDefault,
		LogicalCallID:       "call-1",
		AttemptNonce:        "nonce-1",
		OrganizationID:      "org-1",
		RuntimeID:           "runtime-1",
		TenantID:            "tenant-1",
		UserID:              "user-1",
		SessionID:           "session-1",
		LogicalRunID:        "run-1",
		Provider:            "mock",
		ProviderModelID:     "model-1",
		PolicyGeneration:    7,
		AttemptNumber:       1,
		RequestedReasoning:  ReasoningLow,
		EffectiveReasoning:  ReasoningLow,
		PromptTokens:        11,
		CompletionTokens:    5,
		ReasoningTokens:     2,
		TotalTokens:         16,
		CacheReadTokens:     3,
		CacheWriteTokens:    1,
		InputCostMicros:     101,
		OutputCostMicros:    202,
		ReasoningCostMicros: 33,
		TotalCostMicros:     336,
		Currency:            "USD",
		LatencyMS:           987,
		Status:              "success",
		StartedAt:           started,
		CompletedAt:         started.Add(987 * time.Millisecond),
		IdempotencyKey:      "grant-1/call-1/nonce-1/0/0/0/1",
	}
	var err error
	receipt.CanonicalBodyHash, err = CanonicalAttemptUsageReceiptBodyHash(receipt)
	if err != nil {
		t.Fatal(err)
	}
	body, err := MarshalCanonicalAttemptUsageReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	return receipt, body
}

func marshalReceiptWithFreshHash(t testing.TB, receipt AttemptUsageReceipt) []byte {
	t.Helper()
	var err error
	receipt.CanonicalBodyHash, err = CanonicalAttemptUsageReceiptBodyHash(receipt)
	if err != nil {
		t.Fatal(err)
	}
	body, err := MarshalCanonicalAttemptUsageReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestUnmarshalCanonicalAttemptUsageReceipt_RoundTripsExactCanonicalWire(t *testing.T) {
	want, body := canonicalReceiptFixture(t)
	got, err := UnmarshalCanonicalAttemptUsageReceipt(body)
	if err != nil {
		t.Fatalf("UnmarshalCanonicalAttemptUsageReceipt() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed receipt differs:\n got: %#v\nwant: %#v", got, want)
	}
	roundTrip, err := MarshalCanonicalAttemptUsageReceipt(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(roundTrip, body) {
		t.Fatalf("round-trip bytes differ:\n got: %s\nwant: %s", roundTrip, body)
	}
}

func TestUnmarshalCanonicalAttemptUsageReceipt_RoundTripsLegacyBlankRouteMode(t *testing.T) {
	want, _ := canonicalReceiptFixture(t)
	want.RouteMode = ""
	want.ProviderConnectionID = "connection-legacy"
	want.ProviderConnectionGeneration = 3
	want.RouteID = "route-legacy"
	want.CredentialAssetGeneration = 5
	var err error
	want.CanonicalBodyHash, err = CanonicalAttemptUsageReceiptBodyHash(want)
	if err != nil {
		t.Fatal(err)
	}
	body, err := MarshalCanonicalAttemptUsageReceipt(want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"route_mode":"coordinator_bound"`)) {
		t.Fatalf("legacy canonical wire did not project coordinator_bound: %s", body)
	}

	got, err := UnmarshalCanonicalAttemptUsageReceipt(body)
	if err != nil {
		t.Fatalf("UnmarshalCanonicalAttemptUsageReceipt(legacy) error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy parsed receipt differs:\n got: %#v\nwant: %#v", got, want)
	}
	if got.RouteMode != "" {
		t.Fatalf("legacy parser route mode = %q, want blank legacy public value", got.RouteMode)
	}
	roundTrip, err := MarshalCanonicalAttemptUsageReceipt(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(roundTrip, body) {
		t.Fatalf("legacy round-trip bytes differ:\n got: %s\nwant: %s", roundTrip, body)
	}

	explicit := want
	explicit.RouteMode = ExternalGrantRouteCoordinatorBound
	explicit.CanonicalBodyHash, err = CanonicalAttemptUsageReceiptBodyHash(explicit)
	if err != nil {
		t.Fatal(err)
	}
	explicitBody, err := MarshalCanonicalAttemptUsageReceipt(explicit)
	if err != nil {
		t.Fatal(err)
	}
	explicitParsed, err := UnmarshalCanonicalAttemptUsageReceipt(explicitBody)
	if err != nil {
		t.Fatalf("UnmarshalCanonicalAttemptUsageReceipt(explicit) error = %v", err)
	}
	if !reflect.DeepEqual(explicitParsed, explicit) {
		t.Fatalf("explicit parsed receipt differs:\n got: %#v\nwant: %#v", explicitParsed, explicit)
	}
	if explicitParsed.RouteMode != ExternalGrantRouteCoordinatorBound {
		t.Fatalf("explicit parser route mode = %q, want coordinator_bound", explicitParsed.RouteMode)
	}
}

func TestUnmarshalCanonicalAttemptUsageReceipt_RejectsNonCanonicalDocuments(t *testing.T) {
	_, body := canonicalReceiptFixture(t)
	prefix := []byte(`{"receipt_id":"grant-1/call-1/nonce-1/0/0/0/1","grant_id":"grant-1",`)
	reorderedPrefix := []byte(`{"grant_id":"grant-1","receipt_id":"grant-1/call-1/nonce-1/0/0/0/1",`)
	tests := map[string][]byte{
		"unknown field":          bytes.Replace(body, []byte(`,"canonical_body_hash"`), []byte(`,"private_payload_marker":"do-not-return","canonical_body_hash"`), 1),
		"duplicate field":        bytes.Replace(body, []byte(`{"receipt_id":`), []byte(`{"receipt_id":"duplicate","receipt_id":`), 1),
		"missing required field": bytes.Replace(body, []byte(`"grant_id":"grant-1",`), nil, 1),
		"field casing drift":     bytes.Replace(body, []byte(`"receipt_id"`), []byte(`"Receipt_ID"`), 1),
		"reordered fields":       bytes.Replace(body, prefix, reorderedPrefix, 1),
		"trailing value":         append(append([]byte(nil), body...), []byte(`{}`)...),
		"trailing whitespace":    append(append([]byte(nil), body...), '\n'),
		"leading whitespace":     append([]byte{' '}, body...),
		"redundant escape":       bytes.Replace(body, []byte(`"provider":"mock"`), []byte(`"provider":"m\u006fck"`), 1),
		"noncanonical integer":   bytes.Replace(body, []byte(`"policy_generation":7`), []byte(`"policy_generation":7e0`), 1),
		"wrong numeric type":     bytes.Replace(body, []byte(`"prompt_tokens":11`), []byte(`"prompt_tokens":"11"`), 1),
		"integer overflow":       bytes.Replace(body, []byte(`"policy_generation":7`), []byte(`"policy_generation":18446744073709551616`), 1),
		"explicit omitted zero":  bytes.Replace(body, []byte(`"attempt_nonce":"nonce-1",`), []byte(`"attempt_nonce":"nonce-1","planner_step":0,`), 1),
	}
	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := UnmarshalCanonicalAttemptUsageReceipt(document)
			if !errors.Is(err, ErrInvalidUsageReceipt) {
				t.Fatalf("error = %v, want ErrInvalidUsageReceipt", err)
			}
			if strings.Contains(err.Error(), "private_payload_marker") || strings.Contains(err.Error(), "do-not-return") {
				t.Fatalf("error reflected untrusted receipt content: %v", err)
			}
		})
	}
}

func TestUnmarshalCanonicalAttemptUsageReceipt_RejectsMalformedReceiptFacts(t *testing.T) {
	valid, _ := canonicalReceiptFixture(t)
	tests := map[string]AttemptUsageReceipt{}

	missingIdentity := valid
	missingIdentity.OrganizationID = ""
	tests["missing identity"] = missingIdentity

	negativeUsage := valid
	negativeUsage.PromptTokens = -1
	tests["negative usage"] = negativeUsage

	negativeCost := valid
	negativeCost.TotalCostMicros = -1
	tests["negative cost"] = negativeCost

	badInterval := valid
	badInterval.CompletedAt = badInterval.StartedAt.Add(-time.Nanosecond)
	tests["backwards interval"] = badInterval

	badStatus := valid
	badStatus.Status = "unknown"
	tests["unsupported status"] = badStatus

	mixedRoute := valid
	mixedRoute.ProviderConnectionID = "connection-forbidden"
	mixedRoute.ProviderConnectionGeneration = 1
	mixedRoute.RouteID = "route-forbidden"
	mixedRoute.CredentialAssetGeneration = 1
	tests["mixed runtime-default route"] = mixedRoute

	wrongReceiptID := valid
	wrongReceiptID.ReceiptID = "receipt-forged"
	wrongReceiptID.IdempotencyKey = wrongReceiptID.ReceiptID
	tests["noncanonical receipt identity"] = wrongReceiptID

	wrongIdempotencyKey := valid
	wrongIdempotencyKey.IdempotencyKey = "receipt-replay-alias"
	tests["idempotency identity mismatch"] = wrongIdempotencyKey

	for name, receipt := range tests {
		t.Run(name, func(t *testing.T) {
			body := marshalReceiptWithFreshHash(t, receipt)
			_, err := UnmarshalCanonicalAttemptUsageReceipt(body)
			if !errors.Is(err, ErrInvalidUsageReceipt) {
				t.Fatalf("error = %v, want ErrInvalidUsageReceipt", err)
			}
		})
	}

	wrongHash := valid
	wrongHash.CanonicalBodyHash = strings.Repeat("0", 64)
	body, err := MarshalCanonicalAttemptUsageReceipt(wrongHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalCanonicalAttemptUsageReceipt(body); !errors.Is(err, ErrInvalidUsageReceipt) {
		t.Fatalf("wrong hash error = %v, want ErrInvalidUsageReceipt", err)
	}

	malformedHash := valid
	malformedHash.CanonicalBodyHash = "not-a-lowercase-sha256"
	body, err = MarshalCanonicalAttemptUsageReceipt(malformedHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalCanonicalAttemptUsageReceipt(body); !errors.Is(err, ErrInvalidUsageReceipt) {
		t.Fatalf("malformed hash error = %v, want ErrInvalidUsageReceipt", err)
	}

	uppercaseHash := valid
	uppercaseHash.CanonicalBodyHash = strings.ToUpper(uppercaseHash.CanonicalBodyHash)
	body, err = MarshalCanonicalAttemptUsageReceipt(uppercaseHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalCanonicalAttemptUsageReceipt(body); !errors.Is(err, ErrInvalidUsageReceipt) {
		t.Fatalf("uppercase hash error = %v, want ErrInvalidUsageReceipt", err)
	}
}

func FuzzUnmarshalCanonicalAttemptUsageReceipt_ExactOrRejected(f *testing.F) {
	_, canonical := canonicalReceiptFixture(f)
	f.Add(canonical)
	f.Add([]byte(`{}`))
	f.Add(append(append([]byte(nil), canonical...), []byte(` {}`)...))
	f.Fuzz(func(t *testing.T, data []byte) {
		receipt, err := UnmarshalCanonicalAttemptUsageReceipt(data)
		if err != nil {
			if !errors.Is(err, ErrInvalidUsageReceipt) {
				t.Fatalf("error = %v, want ErrInvalidUsageReceipt", err)
			}
			return
		}
		if err := ValidateAttemptUsageReceipt(receipt); err != nil {
			t.Fatalf("accepted invalid receipt: %v", err)
		}
		canonical, err := MarshalCanonicalAttemptUsageReceipt(receipt)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, canonical) {
			t.Fatalf("accepted noncanonical bytes:\n got: %s\nwant: %s", data, canonical)
		}
	})
}
