package state_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"

	// Side-effect: register the inmem driver under "inmem".
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

func TestNewEventID_NonEmpty(t *testing.T) {
	id := state.NewEventID()
	if id == "" {
		t.Fatal("NewEventID returned empty")
	}
	id2 := state.NewEventID()
	if id == id2 {
		t.Errorf("NewEventID returned identical IDs: %q", id)
	}
}

func TestValidateIdentity_Cases(t *testing.T) {
	good := identity.Quadruple{
		Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"},
	}
	if err := state.ValidateIdentity(good); err != nil {
		t.Errorf("good identity rejected: %v", err)
	}
	cases := []identity.Quadruple{
		{},
		{Identity: identity.Identity{UserID: "U", SessionID: "S"}},
		{Identity: identity.Identity{TenantID: "T", SessionID: "S"}},
		{Identity: identity.Identity{TenantID: "T", UserID: "U"}},
	}
	for i, q := range cases {
		err := state.ValidateIdentity(q)
		if !errors.Is(err, state.ErrIdentityRequired) {
			t.Errorf("case %d (%+v): err=%v, want ErrIdentityRequired", i, q, err)
		}
	}
	// Empty RunID must NOT be rejected — session-scoped state is fine.
	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"},
	}
	if err := state.ValidateIdentity(q); err != nil {
		t.Errorf("empty RunID rejected: %v", err)
	}
}

func TestValidateRecord_Cases(t *testing.T) {
	good := state.StateRecord{
		ID:       "01HABXXX",
		Identity: identity.Quadruple{Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}},
		Kind:     "k",
		Bytes:    []byte("x"),
	}
	if err := state.ValidateRecord(good); err != nil {
		t.Errorf("good record rejected: %v", err)
	}
	noID := good
	noID.ID = ""
	if err := state.ValidateRecord(noID); !errors.Is(err, state.ErrInvalidRecord) {
		t.Errorf("empty ID: err=%v, want ErrInvalidRecord", err)
	}
	noKind := good
	noKind.Kind = ""
	if err := state.ValidateRecord(noKind); !errors.Is(err, state.ErrInvalidRecord) {
		t.Errorf("empty Kind: err=%v, want ErrInvalidRecord", err)
	}
}

func TestValidateSaveIf_Cases(t *testing.T) {
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}}
	next := state.StateRecord{ID: "01HABXXX", Identity: q, Kind: "next", Bytes: []byte("x")}
	if err := state.ValidateSaveIf([]state.SlotExpectation{{Identity: q, Kind: next.Kind}}, next); err != nil {
		t.Fatalf("matching expected-absence predicate: %v", err)
	}
	cases := []struct {
		name         string
		expectations []state.SlotExpectation
		record       state.StateRecord
	}{
		{name: "invalid next", expectations: []state.SlotExpectation{{Identity: q, Kind: next.Kind}}, record: state.StateRecord{Identity: q, Kind: next.Kind}},
		{name: "empty predicates", record: next},
		{name: "incomplete expected identity", expectations: []state.SlotExpectation{{Kind: next.Kind}}, record: next},
		{name: "empty expected kind", expectations: []state.SlotExpectation{{Identity: q}}, record: next},
		{name: "duplicate slot", expectations: []state.SlotExpectation{{Identity: q, Kind: next.Kind}, {Identity: q, Kind: next.Kind}}, record: next},
		{name: "next slot not conditioned", expectations: []state.SlotExpectation{{Identity: q, Kind: "other"}}, record: next},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := state.ValidateSaveIf(tc.expectations, tc.record); !errors.Is(err, state.ErrInvalidRecord) && !errors.Is(err, state.ErrIdentityRequired) {
				t.Fatalf("ValidateSaveIf = %v, want validation sentinel", err)
			}
		})
	}
}

func TestValidateListScopes_Cases(t *testing.T) {
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}}
	validScope := state.ListScope{MaintenanceScoped: true}
	if err := state.ValidateListKind(validScope, "prefix"); err != nil {
		t.Fatalf("ValidateListKind valid: %v", err)
	}
	if err := state.ValidateListKind(state.ListScope{}, "prefix"); !errors.Is(err, state.ErrMaintenanceScopeRequired) {
		t.Fatalf("ValidateListKind missing scope = %v", err)
	}
	if err := state.ValidateListKind(validScope, ""); !errors.Is(err, state.ErrInvalidRecord) {
		t.Fatalf("ValidateListKind empty prefix = %v", err)
	}
	if err := state.ValidateListKindForIdentity(q, "prefix"); err != nil {
		t.Fatalf("ValidateListKindForIdentity valid: %v", err)
	}
	if err := state.ValidateListKindForIdentity(identity.Quadruple{}, "prefix"); !errors.Is(err, state.ErrIdentityRequired) {
		t.Fatalf("ValidateListKindForIdentity missing identity = %v", err)
	}
	if err := state.ValidateListKindForIdentity(q, ""); !errors.Is(err, state.ErrInvalidRecord) {
		t.Fatalf("ValidateListKindForIdentity empty prefix = %v", err)
	}
}

func TestTenantScanValidationAndContinuation_Cases(t *testing.T) {
	scope := state.ListScope{MaintenanceScoped: true}
	if err := state.ValidateScanKindForTenant(scope, "tenant", "prefix", 1); err != nil {
		t.Fatalf("valid tenant scan rejected: %v", err)
	}
	for _, tc := range []struct {
		name           string
		scope          state.ListScope
		tenant, prefix string
		limit          int
		want           error
	}{
		{name: "scope", tenant: "tenant", prefix: "prefix", limit: 1, want: state.ErrMaintenanceScopeRequired},
		{name: "tenant", scope: scope, prefix: "prefix", limit: 1, want: state.ErrInvalidScan},
		{name: "prefix", scope: scope, tenant: "tenant", limit: 1, want: state.ErrInvalidScan},
		{name: "zero limit", scope: scope, tenant: "tenant", prefix: "prefix", want: state.ErrInvalidScan},
		{name: "oversized limit", scope: scope, tenant: "tenant", prefix: "prefix", limit: state.MaxStateScanLimit + 1, want: state.ErrInvalidScan},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := state.ValidateScanKindForTenant(tc.scope, tc.tenant, tc.prefix, tc.limit); !errors.Is(err, tc.want) {
				t.Fatalf("ValidateScanKindForTenant = %v, want %v", err, tc.want)
			}
		})
	}

	cursor := state.StateScanCursor{UserID: "user", SessionID: "session", RunID: "run", Kind: "prefix.record"}
	encoded, err := state.EncodeStateScanContinuation(cursor, "tenant", "prefix", scope)
	if err != nil {
		t.Fatalf("EncodeStateScanContinuation: %v", err)
	}
	got, err := state.DecodeStateScanContinuation(encoded, "tenant", "prefix", scope)
	if err != nil || got != cursor {
		t.Fatalf("DecodeStateScanContinuation = %+v, %v; want %+v", got, err, cursor)
	}
	if got, err := state.DecodeStateScanContinuation("", "tenant", "prefix", scope); err != nil || got != (state.StateScanCursor{}) {
		t.Fatalf("empty continuation = %+v, %v", got, err)
	}
	if _, err := state.EncodeStateScanContinuation(state.StateScanCursor{}, "tenant", "prefix", scope); !errors.Is(err, state.ErrInvalidScan) {
		t.Fatalf("empty cursor = %v, want ErrInvalidScan", err)
	}
	if _, err := state.EncodeStateScanContinuation(state.StateScanCursor{UserID: strings.Repeat("x", 2000), SessionID: "s", Kind: "prefix.k"}, "tenant", "prefix", scope); !errors.Is(err, state.ErrInvalidScan) {
		t.Fatalf("oversized cursor = %v, want ErrInvalidScan", err)
	}
	encodeFixture := func(fields map[string]any) string {
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatalf("marshal cursor fixture: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	baseFields := func() map[string]any {
		return map[string]any{"v": 1, "t": "tenant", "p": "prefix", "m": true, "u": "u", "s": "s", "r": "", "k": "prefix.k"}
	}
	unknownFields := baseFields()
	unknownFields["x"] = 1
	wrongVersion := baseFields()
	wrongVersion["v"] = 2
	incompleteTuple := baseFields()
	incompleteTuple["u"] = ""
	kindOutsidePrefix := baseFields()
	kindOutsidePrefix["k"] = "other.k"
	trailingRaw, err := json.Marshal(baseFields())
	if err != nil {
		t.Fatalf("marshal trailing cursor fixture: %v", err)
	}
	trailing := base64.RawURLEncoding.EncodeToString(append(trailingRaw, []byte("{}")...))
	for _, tc := range []struct {
		name, continuation, tenant, prefix string
		scope                              state.ListScope
		want                               error
	}{
		{name: "scope", continuation: encoded, tenant: "tenant", prefix: "prefix", want: state.ErrMaintenanceScopeRequired},
		{name: "base64", continuation: "%%%", tenant: "tenant", prefix: "prefix", scope: scope, want: state.ErrInvalidScan},
		{name: "empty object", continuation: "e30", tenant: "tenant", prefix: "prefix", scope: scope, want: state.ErrInvalidScan},
		{name: "unknown field", continuation: encodeFixture(unknownFields), tenant: "tenant", prefix: "prefix", scope: scope, want: state.ErrInvalidScan},
		{name: "multiple values", continuation: trailing, tenant: "tenant", prefix: "prefix", scope: scope, want: state.ErrInvalidScan},
		{name: "wrong version", continuation: encodeFixture(wrongVersion), tenant: "tenant", prefix: "prefix", scope: scope, want: state.ErrInvalidScan},
		{name: "incomplete tuple", continuation: encodeFixture(incompleteTuple), tenant: "tenant", prefix: "prefix", scope: scope, want: state.ErrInvalidScan},
		{name: "kind outside prefix", continuation: encodeFixture(kindOutsidePrefix), tenant: "tenant", prefix: "prefix", scope: scope, want: state.ErrInvalidScan},
		{name: "too long", continuation: strings.Repeat("a", 2000), tenant: "tenant", prefix: "prefix", scope: scope, want: state.ErrInvalidScan},
		{name: "query mismatch", continuation: encoded, tenant: "other", prefix: "prefix", scope: scope, want: state.ErrInvalidScan},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := state.DecodeStateScanContinuation(tc.continuation, tc.tenant, tc.prefix, tc.scope); !errors.Is(err, tc.want) {
				t.Fatalf("DecodeStateScanContinuation = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestRegister_DuplicatePanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Register did not panic on duplicate")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "inmem") {
			t.Errorf("panic value %v missing duplicate driver", r)
		}
	}()
	state.Register("inmem", func(_ config.StateConfig) (state.StateStore, error) {
		return nil, nil
	})
}

func TestRegister_EmptyNamePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Register did not panic on empty name")
		}
	}()
	state.Register("", func(_ config.StateConfig) (state.StateStore, error) { return nil, nil })
}

func TestRegister_NilFactoryPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Register did not panic on nil factory")
		}
	}()
	state.Register("nil-factory-test", nil)
}

func TestRegisteredDrivers_ContainsInmem(t *testing.T) {
	got := state.RegisteredDrivers()
	found := false
	for _, n := range got {
		if n == "inmem" {
			found = true
		}
	}
	if !found {
		t.Errorf("inmem driver not in registered list: %v", got)
	}
}

func TestOpen_DefaultDriver(t *testing.T) {
	cfg := config.StateConfig{Driver: "inmem"}
	s, err := state.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	if s == nil {
		t.Fatal("Open returned nil")
	}
}

func TestOpen_DefaultsToInmemWhenDriverEmpty(t *testing.T) {
	cfg := config.StateConfig{Driver: ""}
	s, err := state.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
}

func TestOpenDriver_UnknownNameWrapsSentinel(t *testing.T) {
	_, err := state.OpenDriver("does-not-exist", config.StateConfig{})
	if err == nil {
		t.Fatal("OpenDriver returned nil for unknown driver")
	}
	if !errors.Is(err, state.ErrUnknownDriver) {
		t.Fatalf("err=%v, want errors.Is ErrUnknownDriver", err)
	}
	if !strings.Contains(err.Error(), "inmem") {
		t.Errorf("err=%q does not list registered drivers", err.Error())
	}
}

func TestWithStore_RoundTrip(t *testing.T) {
	s, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	ctx := state.WithStore(context.Background(), s)
	got, ok := state.From(ctx)
	if !ok {
		t.Fatal("From returned ok=false after WithStore")
	}
	if got != s {
		t.Errorf("From returned different store")
	}
}

func TestFrom_AbsentReturnsZeroAndFalse(t *testing.T) {
	got, ok := state.From(context.Background())
	if ok {
		t.Errorf("From on bare ctx returned ok=true")
	}
	if got != nil {
		t.Errorf("From on bare ctx returned non-nil: %v", got)
	}
}

func TestMustFrom_PanicsOnAbsence(t *testing.T) {
	defer func() {
		v := recover()
		if v == nil {
			t.Fatal("MustFrom did not panic on bare ctx")
		}
		err, ok := v.(error)
		if !ok || !errors.Is(err, state.ErrStoreClosed) {
			t.Fatalf("panic value %v is not ErrStoreClosed", v)
		}
	}()
	_ = state.MustFrom(context.Background())
}
