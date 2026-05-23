package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/tools"
)

// TestKnownNames_MirrorsConfigAllowlist asserts the two surfaces
// (`builtin.KnownNames()` and `internal/config`'s built-in allowlist)
// stay in lockstep. The §4.4 seam pattern requires the config
// package to MIRROR the registry rather than import it, so a missed
// entry on either side has to fail this test.
func TestKnownNames_MirrorsConfigAllowlist(t *testing.T) {
	t.Parallel()
	want := KnownNames()
	got := config.KnownBuiltInTools()
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("builtin.KnownNames() %v != config.KnownBuiltInTools() %v — update the mirror in either internal/tools/builtin or internal/config", want, got)
	}
}

// TestRegister_UnknownNameFailsLoudly asserts a typo in
// `tools.built_in` doesn't silently no-op — it fails closed with
// ErrUnknownBuiltIn naming the known names per CLAUDE.md §5 fail-
// loud posture.
func TestRegister_UnknownNameFailsLoudly(t *testing.T) {
	t.Parallel()
	cat := tools.NewCatalog()
	err := Register(cat, []string{"clock.nowt"})
	if !errors.Is(err, ErrUnknownBuiltIn) {
		t.Fatalf("want ErrUnknownBuiltIn, got %v", err)
	}
}

// TestRegister_KnownNamesRegister asserts every name in the registry
// actually registers when passed through Register. Guards against an
// entry in the registry whose payload types the inproc deriver
// can't represent.
func TestRegister_KnownNamesRegister(t *testing.T) {
	t.Parallel()
	cat := tools.NewCatalog()
	if err := Register(cat, KnownNames()); err != nil {
		t.Fatalf("Register(KnownNames()): %v", err)
	}
	for _, name := range KnownNames() {
		if _, ok := cat.Resolve(name); !ok {
			t.Fatalf("Resolve(%q): not found after Register", name)
		}
	}
}

// TestRegister_NilCatalogFailsLoudly asserts the defensive nil-check
// surfaces ErrRegisterFailed.
func TestRegister_NilCatalogFailsLoudly(t *testing.T) {
	t.Parallel()
	err := Register(nil, []string{"text.echo"})
	if !errors.Is(err, ErrRegisterFailed) {
		t.Fatalf("want ErrRegisterFailed, got %v", err)
	}
}

// TestRegister_EmptyListIsNoOp asserts opting in to zero built-ins
// is a valid configuration — no error, no side effect.
func TestRegister_EmptyListIsNoOp(t *testing.T) {
	t.Parallel()
	cat := tools.NewCatalog()
	if err := Register(cat, nil); err != nil {
		t.Fatalf("Register(nil): %v", err)
	}
}

// TestClockNow_ReturnsParseableRFC3339AndCloseEpochMS asserts the
// returned values agree with each other and are within a small
// tolerance of the test's clock reading.
func TestClockNow_ReturnsParseableRFC3339AndCloseEpochMS(t *testing.T) {
	t.Parallel()
	before := time.Now().UnixMilli()
	out, err := ClockNow(context.Background(), ClockNowArgs{})
	if err != nil {
		t.Fatalf("ClockNow: %v", err)
	}
	after := time.Now().UnixMilli()
	if _, err := time.Parse(time.RFC3339, out.RFC3339); err != nil {
		t.Fatalf("rfc3339 %q does not parse: %v", out.RFC3339, err)
	}
	if out.EpochMS < before-100 || out.EpochMS > after+100 {
		t.Fatalf("epoch_ms %d outside [%d, %d]", out.EpochMS, before-100, after+100)
	}
	if out.Timezone != "UTC" {
		t.Fatalf("timezone = %q, want UTC", out.Timezone)
	}
}

// TestTextEcho_RoundTripsTextAndTag asserts the simple echo
// contract.
func TestTextEcho_RoundTripsTextAndTag(t *testing.T) {
	t.Parallel()
	out, err := TextEcho(context.Background(), TextEchoArgs{Text: "hi", Tag: "alpha"})
	if err != nil {
		t.Fatalf("TextEcho: %v", err)
	}
	if out.Echoed != "hi" || out.Tag != "alpha" {
		t.Fatalf("got %+v, want Echoed=hi Tag=alpha", out)
	}
}

// TestTextEcho_InvokeThroughCatalog asserts the registered descriptor
// actually round-trips JSON through the inproc dispatch path — covers
// the deriver against the live tool's payload types.
func TestTextEcho_InvokeThroughCatalog(t *testing.T) {
	t.Parallel()
	cat := tools.NewCatalog()
	if err := Register(cat, []string{"text.echo"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	desc, ok := cat.Resolve("text.echo")
	if !ok {
		t.Fatal("text.echo not registered")
	}
	args := json.RawMessage(`{"text":"hello","tag":"beta"}`)
	res, err := desc.Invoke(context.Background(), args)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out, ok := res.Value.(TextEchoOut)
	if !ok {
		t.Fatalf("Invoke returned %T, want TextEchoOut", res.Value)
	}
	if out.Echoed != "hello" || out.Tag != "beta" {
		t.Fatalf("Invoke returned %+v, want Echoed=hello Tag=beta", out)
	}
}
