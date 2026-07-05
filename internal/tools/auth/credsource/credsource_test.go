package credsource

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestRegister_RejectsBadInput pins the registry's fail-loud on empty
// name / nil factory / duplicate.
func TestRegister_RejectsBadInput(t *testing.T) {
	if err := Register("", func(Config) (Source, error) { return nil, nil }); !errors.Is(err, ErrSourceEmptyName) {
		t.Fatalf("empty name: want ErrSourceEmptyName, got %v", err)
	}
	if err := Register("bad-nil", nil); !errors.Is(err, ErrSourceNilFactory) {
		t.Fatalf("nil factory: want ErrSourceNilFactory, got %v", err)
	}

	name := "dup-test-source"
	f := func(Config) (Source, error) { return Static("id", "sc"), nil }
	if err := Register(name, f); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	t.Cleanup(func() { unregisterForTest(name) })
	if err := Register(name, f); !errors.Is(err, ErrSourceDuplicate) {
		t.Fatalf("duplicate: want ErrSourceDuplicate, got %v", err)
	}
}

// TestResolve_UnknownSource_ListsRegistered pins that the factory error
// names the offending source AND lists the registered names so a typo is
// obvious.
func TestResolve_UnknownSource_ListsRegistered(t *testing.T) {
	name := "conf-probe-source"
	if err := Register(name, func(Config) (Source, error) { return Static("id", "sc"), nil }); err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { unregisterForTest(name) })

	_, err := Resolve("no-such-source-xyz", Config{})
	if !errors.Is(err, ErrSourceUnknown) {
		t.Fatalf("want ErrSourceUnknown, got %v", err)
	}
	if !strings.Contains(err.Error(), "no-such-source-xyz") {
		t.Fatalf("error must name the offending source: %v", err)
	}
	if !strings.Contains(err.Error(), name) {
		t.Fatalf("error must list the registered source %q: %v", name, err)
	}
}

// TestMustRegister_PanicsOnDuplicate pins the init()-time panic contract.
func TestMustRegister_PanicsOnDuplicate(t *testing.T) {
	name := "must-dup-source"
	MustRegister(name, func(Config) (Source, error) { return Static("id", "sc"), nil })
	t.Cleanup(func() { unregisterForTest(name) })

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("MustRegister on duplicate did not panic")
		}
	}()
	MustRegister(name, func(Config) (Source, error) { return Static("id", "sc"), nil })
}

// TestStatic_ResolvesVerbatim pins the direct-construction convenience:
// the pre-resolved source returns exactly what it was given, never
// expires, and validates trivially.
func TestStatic_ResolvesVerbatim(t *testing.T) {
	s := Static("client-xyz", "secret-xyz")
	if err := s.ValidateAtBoot(context.Background()); err != nil {
		t.Fatalf("ValidateAtBoot: %v", err)
	}
	cred, err := s.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.ClientID != "client-xyz" || cred.ClientSecret != "secret-xyz" {
		t.Fatalf("Resolve = %+v, want client-xyz/secret-xyz", cred)
	}
	if !cred.ExpiresAt.IsZero() {
		t.Fatalf("static credential must not advertise expiry, got %v", cred.ExpiresAt)
	}
}
