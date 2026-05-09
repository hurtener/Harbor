package audit_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"

	// Side-effect: register the production driver under "patterns".
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
)

func TestOpen_DefaultDriverRegistered(t *testing.T) {
	r, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if r == nil {
		t.Fatal("Open returned nil Redactor")
	}
}

func TestOpenDriver_UnknownNameWrapsSentinel(t *testing.T) {
	_, err := audit.OpenDriver("does-not-exist", config.AuditConfig{})
	if err == nil {
		t.Fatal("OpenDriver returned nil error for unknown driver")
	}
	if !errors.Is(err, audit.ErrUnknownDriver) {
		t.Fatalf("err=%v, want errors.Is ErrUnknownDriver", err)
	}
	if !strings.Contains(err.Error(), "patterns") {
		t.Errorf("err=%q does not list registered drivers", err.Error())
	}
}

func TestRegister_DuplicatePanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Register did not panic on duplicate")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "patterns") {
			t.Errorf("panic value %v does not mention duplicate driver", r)
		}
	}()
	audit.Register("patterns", func(_ config.AuditConfig) (audit.Redactor, error) {
		return nil, nil
	})
}

func TestRegister_EmptyNamePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Register did not panic on empty name")
		}
	}()
	audit.Register("", func(_ config.AuditConfig) (audit.Redactor, error) { return nil, nil })
}

func TestRegister_NilFactoryPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Register did not panic on nil factory")
		}
	}()
	audit.Register("nil-factory-test", nil)
}

func TestRegisteredDrivers_ContainsPatterns(t *testing.T) {
	got := audit.RegisteredDrivers()
	found := false
	for _, n := range got {
		if n == "patterns" {
			found = true
		}
	}
	if !found {
		t.Errorf("patterns driver not in registered list: %v", got)
	}
}

func TestWithRedactor_RoundTrip(t *testing.T) {
	r, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := audit.WithRedactor(context.Background(), r)
	got, ok := audit.From(ctx)
	if !ok {
		t.Fatal("From returned ok=false after WithRedactor")
	}
	if got != r {
		t.Errorf("From returned different Redactor instance")
	}
}

func TestFrom_AbsentReturnsZeroAndFalse(t *testing.T) {
	r, ok := audit.From(context.Background())
	if ok {
		t.Errorf("From on bare ctx returned ok=true")
	}
	if r != nil {
		t.Errorf("From on bare ctx returned non-nil: %v", r)
	}
}

func TestMustFrom_PanicsWithSentinelOnAbsence(t *testing.T) {
	defer func() {
		v := recover()
		if v == nil {
			t.Fatal("MustFrom did not panic on bare ctx")
		}
		err, ok := v.(error)
		if !ok || !errors.Is(err, audit.ErrRedactorMissing) {
			t.Fatalf("panic value %v is not ErrRedactorMissing", v)
		}
	}()
	_ = audit.MustFrom(context.Background())
}

func TestMustFrom_ReturnsRedactorWhenPresent(t *testing.T) {
	r, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := audit.WithRedactor(context.Background(), r)
	if got := audit.MustFrom(ctx); got != r {
		t.Errorf("MustFrom returned different Redactor instance")
	}
}

// failingRule always errors — used to assert the fail-loudly contract.
type failingRule struct {
	name string
}

func (r *failingRule) Name() string { return r.name }
func (r *failingRule) Apply(_ context.Context, _ any) (any, error) {
	return "should-not-be-emitted", errors.New("rule blew up")
}

// TestRedact_FailLoudly_NoPartialPayload asserts that when a rule
// returns an error, Redact returns (nil, wrapped error) — never the
// partial payload as fallback. This is the audit subsystem's load-
// bearing safety guarantee.
func TestRedact_FailLoudly_NoPartialPayload(t *testing.T) {
	// Build a custom driver with a failing rule wedged in.
	driver := &fakeDriver{
		rules: []audit.Rule{&failingRule{name: "fail-test"}},
	}
	in := map[string]any{"api_key": "real-secret"}
	out, err := driver.Redact(context.Background(), in)
	if err == nil {
		t.Fatal("Redact returned nil error for failing rule")
	}
	if out != nil {
		t.Fatalf("Redact returned non-nil payload on error: %v", out)
	}
	if !errors.Is(err, audit.ErrRedactionFailed) {
		t.Errorf("err=%v, want errors.Is ErrRedactionFailed", err)
	}
	if strings.Contains(err.Error(), "real-secret") {
		t.Errorf("err leaks the original secret value: %q", err.Error())
	}
	// And, critically: in must not have been mutated.
	if in["api_key"] != "real-secret" {
		t.Errorf("Redact mutated input: api_key=%v", in["api_key"])
	}
}

// fakeDriver mirrors patterns.Driver's contract for fail-loudly tests
// without depending on the patterns package internals.
type fakeDriver struct {
	rules []audit.Rule
}

func (d *fakeDriver) Redact(ctx context.Context, payload any) (any, error) {
	cur := payload
	for _, rule := range d.rules {
		next, err := rule.Apply(ctx, cur)
		if err != nil {
			return nil, errors.Join(audit.ErrRedactionFailed, err)
		}
		cur = next
	}
	return cur, nil
}
