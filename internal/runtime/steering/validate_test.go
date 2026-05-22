package steering

import (
	"errors"
	"strings"
	"testing"
)

// nestToDepth builds a payload nested to exactly d map levels. d=1
// is the top-level map carrying a scalar; d=2 is a map carrying a
// map carrying a scalar; etc.
func nestToDepth(d int) map[string]any {
	if d <= 1 {
		return map[string]any{"leaf": "v"}
	}
	return map[string]any{"child": nestToDepth(d - 1)}
}

func TestValidatePayload_NilAndEmptyAreValid(t *testing.T) {
	if err := ValidatePayload(nil); err != nil {
		t.Errorf("ValidatePayload(nil) = %v, want nil (a bare control carries no payload)", err)
	}
	if err := ValidatePayload(map[string]any{}); err != nil {
		t.Errorf("ValidatePayload(empty) = %v, want nil", err)
	}
}

func TestValidatePayload_DepthBound(t *testing.T) {
	// Depth exactly at the cap (6) is valid.
	if err := ValidatePayload(nestToDepth(MaxPayloadDepth)); err != nil {
		t.Errorf("ValidatePayload(depth=%d) = %v, want nil", MaxPayloadDepth, err)
	}
	// Depth one past the cap is rejected.
	err := ValidatePayload(nestToDepth(MaxPayloadDepth + 1))
	if !errors.Is(err, ErrPayloadInvalid) {
		t.Fatalf("ValidatePayload(depth=%d) = %v, want ErrPayloadInvalid", MaxPayloadDepth+1, err)
	}
	if !strings.Contains(err.Error(), "depth") {
		t.Errorf("depth-violation error %q does not name the bound", err.Error())
	}
}

func TestValidatePayload_KeysBound(t *testing.T) {
	atCap := make(map[string]any, MaxPayloadKeys)
	for i := range MaxPayloadKeys {
		atCap[string(rune('a'+i%26))+string(rune('0'+i/26))] = i
	}
	if err := ValidatePayload(atCap); err != nil {
		t.Errorf("ValidatePayload(keys=%d) = %v, want nil", MaxPayloadKeys, err)
	}

	overCap := make(map[string]any, MaxPayloadKeys+1)
	for i := range MaxPayloadKeys + 1 {
		overCap[strings.Repeat("k", 1)+string(rune(i))] = i
	}
	err := ValidatePayload(overCap)
	if !errors.Is(err, ErrPayloadInvalid) {
		t.Fatalf("ValidatePayload(keys=%d) = %v, want ErrPayloadInvalid", MaxPayloadKeys+1, err)
	}
	if !strings.Contains(err.Error(), "key") {
		t.Errorf("keys-violation error %q does not name the bound", err.Error())
	}
}

func TestValidatePayload_KeysBoundIsPerMap(t *testing.T) {
	// Two maps of MaxPayloadKeys each is valid (the cap is per-map,
	// not cumulative).
	mkMap := func() map[string]any {
		m := make(map[string]any, MaxPayloadKeys)
		for i := range MaxPayloadKeys {
			m[string(rune(i))] = i
		}
		return m
	}
	p := map[string]any{"a": mkMap(), "b": mkMap()}
	if err := ValidatePayload(p); err != nil {
		t.Errorf("ValidatePayload(two full maps) = %v, want nil (per-map cap)", err)
	}
}

func TestValidatePayload_ListItemsBound(t *testing.T) {
	atCap := make([]any, MaxPayloadListItems)
	for i := range atCap {
		atCap[i] = i
	}
	if err := ValidatePayload(map[string]any{"list": atCap}); err != nil {
		t.Errorf("ValidatePayload(list=%d) = %v, want nil", MaxPayloadListItems, err)
	}

	overCap := make([]any, MaxPayloadListItems+1)
	for i := range overCap {
		overCap[i] = i
	}
	err := ValidatePayload(map[string]any{"list": overCap})
	if !errors.Is(err, ErrPayloadInvalid) {
		t.Fatalf("ValidatePayload(list=%d) = %v, want ErrPayloadInvalid", MaxPayloadListItems+1, err)
	}
	if !strings.Contains(err.Error(), "list") {
		t.Errorf("list-violation error %q does not name the bound", err.Error())
	}
}

func TestValidatePayload_StringLenBound(t *testing.T) {
	atCap := strings.Repeat("x", MaxPayloadStringLen)
	if err := ValidatePayload(map[string]any{"s": atCap}); err != nil {
		t.Errorf("ValidatePayload(string=%d) = %v, want nil", MaxPayloadStringLen, err)
	}

	overCap := strings.Repeat("x", MaxPayloadStringLen+1)
	err := ValidatePayload(map[string]any{"s": overCap})
	if !errors.Is(err, ErrPayloadInvalid) {
		t.Fatalf("ValidatePayload(string=%d) = %v, want ErrPayloadInvalid", MaxPayloadStringLen+1, err)
	}
	if !strings.Contains(err.Error(), "char") {
		t.Errorf("string-violation error %q does not name the bound", err.Error())
	}
}

func TestValidatePayload_StringLenCountsRunesNotBytes(t *testing.T) {
	// A multi-byte rune string at the rune cap is valid even though
	// its byte length far exceeds the cap.
	s := strings.Repeat("é", MaxPayloadStringLen) // 2 bytes/rune
	if err := ValidatePayload(map[string]any{"s": s}); err != nil {
		t.Errorf("ValidatePayload(%d multibyte runes) = %v, want nil", MaxPayloadStringLen, err)
	}
}

func TestValidatePayload_TotalBytesBound(t *testing.T) {
	// One string just over 16 KiB exceeds the total-bytes cap.
	big := strings.Repeat("a", MaxPayloadTotalBytes+1)
	// Keep each individual string under the per-string cap by
	// splitting across a list, so the *total* check is what fires.
	chunks := make([]any, 0)
	remaining := MaxPayloadTotalBytes + 200
	for remaining > 0 {
		n := MaxPayloadStringLen
		if n > remaining {
			n = remaining
		}
		chunks = append(chunks, strings.Repeat("a", n))
		remaining -= n
	}
	_ = big
	err := ValidatePayload(map[string]any{"chunks": chunks})
	if !errors.Is(err, ErrPayloadInvalid) {
		t.Fatalf("ValidatePayload(>16KiB total) = %v, want ErrPayloadInvalid", err)
	}
	if !strings.Contains(err.Error(), "total size") {
		t.Errorf("total-bytes-violation error %q does not name the bound", err.Error())
	}
}

func TestValidatePayload_UnsupportedLeafType(t *testing.T) {
	cases := map[string]any{
		"channel": make(chan int),
		"func":    func() {},
		"complex": complex(1, 2),
	}
	for name, bad := range cases {
		err := ValidatePayload(map[string]any{name: bad})
		if !errors.Is(err, ErrUnsupportedPayloadValue) {
			t.Errorf("ValidatePayload(%s leaf) = %v, want ErrUnsupportedPayloadValue", name, err)
		}
	}
}

func TestValidatePayload_AcceptedScalarLeaves(t *testing.T) {
	p := map[string]any{
		"bool":   true,
		"f64":    3.14,
		"int":    int(7),
		"int64":  int64(9),
		"uint":   uint(3),
		"nil":    nil,
		"str":    "ok",
		"list":   []any{1, "two", false, nil},
		"nested": map[string]any{"inner": []any{map[string]any{"x": 1}}},
	}
	if err := ValidatePayload(p); err != nil {
		t.Errorf("ValidatePayload(mixed valid scalars) = %v, want nil", err)
	}
}

// TestValidatePayload_NeverTruncates is the fail-loud contract: a
// payload that violates a bound is REJECTED, the function never
// returns a silently-shrunk payload. ValidatePayload returns only an
// error (not a payload), so this is structural — the test documents
// the contract and asserts the over-bound input still errors.
func TestValidatePayload_NeverTruncates(t *testing.T) {
	over := map[string]any{"s": strings.Repeat("x", MaxPayloadStringLen*2)}
	if err := ValidatePayload(over); err == nil {
		t.Fatal("ValidatePayload silently accepted an over-bound payload — fail-loud contract violated")
	}
	// The caller's map is untouched — ValidatePayload does not mutate.
	if got := len(over["s"].(string)); got != MaxPayloadStringLen*2 {
		t.Errorf("ValidatePayload mutated the caller's payload (string len now %d)", got)
	}
}
