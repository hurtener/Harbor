package config

import (
	"testing"
	"time"
)

// TestToolPolicyConfig_MaxAttemptsOffByOne pins the operator-facing
// `max_attempts` (TOTAL attempts incl. the first) → `MaxRetries`
// (retries AFTER the first) translation. max_attempts:1 → 0 retries
// (one attempt); max_attempts:5 → 4 retries (five attempts).
func TestToolPolicyConfig_MaxAttemptsOffByOne(t *testing.T) {
	cases := []struct {
		name           string
		maxAttempts    int
		wantMaxRetries int
	}{
		{"single attempt", 1, 0},
		{"two attempts", 2, 1},
		{"five attempts", 5, 4},
		{"omitted falls through to default", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ToolPolicyConfig{MaxAttempts: tc.maxAttempts}.ToToolPolicy()
			if err != nil {
				t.Fatalf("ToToolPolicy: %v", err)
			}
			if got.MaxRetries != tc.wantMaxRetries {
				t.Fatalf("max_attempts=%d: MaxRetries=%d, want %d",
					tc.maxAttempts, got.MaxRetries, tc.wantMaxRetries)
			}
		})
	}
}

// TestToolPolicyConfig_SingleAttemptPinsRetryOnEmpty proves
// max_attempts:1 projects RetryOnEmpty=true so the runtime builds an
// explicit empty RetryOn — without it, the MaxRetries:0 fall-through
// would silently restore the default 3 retries. max_attempts>=2 (or an
// explicit retry_on) must NOT set the flag.
func TestToolPolicyConfig_SingleAttemptPinsRetryOnEmpty(t *testing.T) {
	one, err := ToolPolicyConfig{MaxAttempts: 1}.ToToolPolicy()
	if err != nil {
		t.Fatalf("ToToolPolicy: %v", err)
	}
	if !one.RetryOnEmpty {
		t.Error("max_attempts:1 must set RetryOnEmpty to pin a single attempt")
	}
	if len(one.RetryOn) != 0 {
		t.Errorf("max_attempts:1 should leave RetryOn empty, got %v", one.RetryOn)
	}

	// max_attempts:1 WITH an explicit retry_on keeps the allowlist and
	// does not force the empty pin (the operator opted into retries).
	withRetry, err := ToolPolicyConfig{MaxAttempts: 1, RetryOn: []string{"timeout"}}.ToToolPolicy()
	if err != nil {
		t.Fatalf("ToToolPolicy: %v", err)
	}
	if withRetry.RetryOnEmpty {
		t.Error("max_attempts:1 with explicit retry_on must not set RetryOnEmpty")
	}

	two, err := ToolPolicyConfig{MaxAttempts: 2}.ToToolPolicy()
	if err != nil {
		t.Fatalf("ToToolPolicy: %v", err)
	}
	if two.RetryOnEmpty {
		t.Error("max_attempts:2 must not set RetryOnEmpty")
	}
}

// TestToolPolicyConfig_PerFieldZeroFallThrough proves a partial policy
// leaves untouched fields at the zero value, so tools.ToolPolicy's own
// per-field resolved() fall-through fills them with the package default
// at dispatch. Setting only timeout_ms must NOT collapse MaxRetries to
// a non-default value — it stays zero (→ default 3 retries downstream).
func TestToolPolicyConfig_PerFieldZeroFallThrough(t *testing.T) {
	got, err := ToolPolicyConfig{TimeoutMS: 5000}.ToToolPolicy()
	if err != nil {
		t.Fatalf("ToToolPolicy: %v", err)
	}
	if got.TimeoutMS != 5000 {
		t.Fatalf("TimeoutMS=%d, want 5000", got.TimeoutMS)
	}
	// Every other field stays zero so the runtime inherits the default.
	if got.MaxRetries != 0 {
		t.Errorf("MaxRetries=%d, want 0 (fall-through to default)", got.MaxRetries)
	}
	if got.BackoffBase != 0 {
		t.Errorf("BackoffBase=%v, want 0 (fall-through)", got.BackoffBase)
	}
	if got.BackoffMax != 0 {
		t.Errorf("BackoffMax=%v, want 0 (fall-through)", got.BackoffMax)
	}
	if got.BackoffMult != 0 {
		t.Errorf("BackoffMult=%v, want 0 (fall-through)", got.BackoffMult)
	}
	if got.RetryOn != nil {
		t.Errorf("RetryOn=%v, want nil (fall-through)", got.RetryOn)
	}
}

// TestToolPolicyConfig_TimeoutAndBackoffMapping pins the millisecond →
// time.Duration mapping and the direct timeout / mult copies.
func TestToolPolicyConfig_TimeoutAndBackoffMapping(t *testing.T) {
	got, err := ToolPolicyConfig{
		MaxAttempts:   3,
		TimeoutMS:     60000,
		BackoffBaseMS: 250,
		BackoffMaxMS:  10000,
		BackoffMult:   3,
	}.ToToolPolicy()
	if err != nil {
		t.Fatalf("ToToolPolicy: %v", err)
	}
	if got.TimeoutMS != 60000 {
		t.Errorf("TimeoutMS=%d, want 60000", got.TimeoutMS)
	}
	if got.MaxRetries != 2 {
		t.Errorf("MaxRetries=%d, want 2", got.MaxRetries)
	}
	if got.BackoffBase != 250*time.Millisecond {
		t.Errorf("BackoffBase=%v, want 250ms", got.BackoffBase)
	}
	if got.BackoffMax != 10*time.Second {
		t.Errorf("BackoffMax=%v, want 10s", got.BackoffMax)
	}
	if got.BackoffMult != 3 {
		t.Errorf("BackoffMult=%v, want 3", got.BackoffMult)
	}
}

// TestToolPolicyConfig_RetryOnMapping checks the retry_on allowlist
// passes valid classes through verbatim and rejects unknown ones.
func TestToolPolicyConfig_RetryOnMapping(t *testing.T) {
	got, err := ToolPolicyConfig{
		MaxAttempts: 4,
		RetryOn:     []string{"transient", "timeout", "5xx", "permanent"},
	}.ToToolPolicy()
	if err != nil {
		t.Fatalf("ToToolPolicy: %v", err)
	}
	want := []string{"transient", "timeout", "5xx", "permanent"}
	if len(got.RetryOn) != len(want) {
		t.Fatalf("RetryOn=%v, want %v", got.RetryOn, want)
	}
	for i := range want {
		if got.RetryOn[i] != want[i] {
			t.Errorf("RetryOn[%d]=%q, want %q", i, got.RetryOn[i], want[i])
		}
	}
}

// TestToolPolicyConfig_UnknownRetryOnErrors proves an unknown retry_on
// class fails loud (no silent drop, CLAUDE.md §5).
func TestToolPolicyConfig_UnknownRetryOnErrors(t *testing.T) {
	_, err := ToolPolicyConfig{
		MaxAttempts: 2,
		RetryOn:     []string{"transient", "bogus-class"},
	}.ToToolPolicy()
	if err == nil {
		t.Fatal("expected error for unknown retry_on class, got nil")
	}
}

// TestToolPolicyConfig_NegativeFieldsError guards the belt-and-braces
// non-negative checks in the projection.
func TestToolPolicyConfig_NegativeFieldsError(t *testing.T) {
	cases := []struct {
		name string
		cfg  ToolPolicyConfig
	}{
		{"negative max_attempts", ToolPolicyConfig{MaxAttempts: -1}},
		{"negative timeout_ms", ToolPolicyConfig{TimeoutMS: -1}},
		{"negative backoff_base_ms", ToolPolicyConfig{BackoffBaseMS: -1}},
		{"negative backoff_max_ms", ToolPolicyConfig{BackoffMaxMS: -1}},
		{"negative backoff_mult", ToolPolicyConfig{BackoffMult: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.cfg.ToToolPolicy(); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

// TestPerToolPolicy_DistinctOverridesProject proves two per-tool
// override configs project independently — the off-by-one + timeout are
// applied per entry, never shared. This mirrors the per-tool map an
// operator writes under `tool_policies:`.
func TestPerToolPolicy_DistinctOverridesProject(t *testing.T) {
	policies := map[string]ToolPolicyConfig{
		"slow_tool":  {MaxAttempts: 1, TimeoutMS: 60000},
		"flaky_tool": {MaxAttempts: 6, TimeoutMS: 2000},
	}

	slow, err := policies["slow_tool"].ToToolPolicy()
	if err != nil {
		t.Fatalf("slow ToToolPolicy: %v", err)
	}
	if slow.MaxRetries != 0 || slow.TimeoutMS != 60000 {
		t.Errorf("slow_tool: MaxRetries=%d TimeoutMS=%d, want 0/60000",
			slow.MaxRetries, slow.TimeoutMS)
	}

	flaky, err := policies["flaky_tool"].ToToolPolicy()
	if err != nil {
		t.Fatalf("flaky ToToolPolicy: %v", err)
	}
	if flaky.MaxRetries != 5 || flaky.TimeoutMS != 2000 {
		t.Errorf("flaky_tool: MaxRetries=%d TimeoutMS=%d, want 5/2000",
			flaky.MaxRetries, flaky.TimeoutMS)
	}
}
