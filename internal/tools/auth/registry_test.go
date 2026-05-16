package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/tools"
)

func TestRegister_EmptyNameRejected(t *testing.T) {
	t.Parallel()
	err := Register("", okFactory)
	if !errors.Is(err, ErrDriverEmptyName) {
		t.Fatalf("Register(empty) err = %v, want ErrDriverEmptyName", err)
	}
}

func TestRegister_NilFactoryRejected(t *testing.T) {
	t.Parallel()
	err := Register("some-name", nil)
	if !errors.Is(err, ErrDriverNilFactory) {
		t.Fatalf("Register(nil) err = %v, want ErrDriverNilFactory", err)
	}
}

func TestRegister_DuplicateRejected(t *testing.T) {
	t.Parallel()
	name := "test-dup-driver"
	defer unregisterForTest(name)

	if err := Register(name, okFactory); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	err := Register(name, okFactory)
	if !errors.Is(err, ErrDriverDuplicate) {
		t.Fatalf("second Register err = %v, want ErrDriverDuplicate", err)
	}
}

func TestRegistry_ResolveUnknownDriverFailsLoud(t *testing.T) {
	t.Parallel()
	_, err := Resolve(context.Background(), "no-such-driver-xyz", ProviderConfig{}, FactoryDeps{})
	if !errors.Is(err, ErrDriverUnknown) {
		t.Fatalf("Resolve err = %v, want ErrDriverUnknown", err)
	}
}

func TestMustRegister_PanicsOnError(t *testing.T) {
	t.Parallel()
	name := "test-must-panic"
	defer unregisterForTest(name)

	MustRegister(name, okFactory)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("MustRegister did not panic on duplicate")
		}
	}()
	MustRegister(name, okFactory)
}

func TestRegisteredDrivers_Sorted(t *testing.T) {
	t.Parallel()
	names := []string{"zzz-test-driver-1", "aaa-test-driver-2"}
	defer func() {
		for _, n := range names {
			unregisterForTest(n)
		}
	}()
	for _, n := range names {
		MustRegister(n, okFactory)
	}
	listed := RegisteredDrivers()
	var foundIdxA, foundIdxZ = -1, -1
	for i, n := range listed {
		if n == "aaa-test-driver-2" {
			foundIdxA = i
		}
		if n == "zzz-test-driver-1" {
			foundIdxZ = i
		}
	}
	if foundIdxA == -1 || foundIdxZ == -1 {
		t.Fatalf("test drivers not found in %v", listed)
	}
	if foundIdxA >= foundIdxZ {
		t.Fatalf("RegisteredDrivers not sorted: aaa idx=%d, zzz idx=%d", foundIdxA, foundIdxZ)
	}
}

// okFactory is a no-op Factory for registry-bookkeeping tests. The
// returned provider does NOT satisfy real OAuth semantics — these
// tests only verify the registry's lookup behaviour.
func okFactory(_ ProviderConfig, _ FactoryDeps) (OAuthProvider, error) {
	return registryTestProvider{}, nil
}

// registryTestProvider is a no-op OAuthProvider that satisfies the
// interface so the registry's bookkeeping tests compile. Test-only
// per CLAUDE.md §13 — never reachable from production code.
type registryTestProvider struct{}

func (registryTestProvider) Token(_ context.Context, _ tools.ToolSourceID) (Token, error) {
	return Token{}, nil
}
func (registryTestProvider) InitiateFlow(_ context.Context, _ tools.ToolSourceID) (FlowInitiation, error) {
	return FlowInitiation{}, nil
}
func (registryTestProvider) CompleteFlow(_ context.Context, _, _ string) (Token, error) {
	return Token{}, nil
}
func (registryTestProvider) Revoke(_ context.Context, _ tools.ToolSourceID) error { return nil }
func (registryTestProvider) Close(_ context.Context) error                        { return nil }
