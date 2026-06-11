package main

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/singlesource"
	"github.com/hurtener/Harbor/internal/runtime/steering"
)

// TestGen_MethodTableInLockstep pins the methods join table against the
// canonical registry: every methods.Methods() entry has a row, no row
// names an unregistered method, every named wire type resolves through
// the typeInstanceIndex, every route is concrete, and every steering-
// control method resolves a registered ControlType. A new Protocol
// method without a docs row fails this test — the same mechanism
// TestSingleSource_CanonicalMethodsInLockstep uses for the method set.
func TestGen_MethodTableInLockstep(t *testing.T) {
	table := methodTable()

	for _, m := range methods.Methods() {
		e, ok := table[m]
		if !ok {
			t.Errorf("method %q registered in internal/protocol/methods but missing from methodTable — add its docs row", m)
			continue
		}
		if e.Route == "" || strings.Contains(e.Route, "{method}") {
			t.Errorf("method %q: route %q is empty or still carries the {method} wildcard", m, e.Route)
		}
		if e.Request == "" && e.RequestNote == "" {
			t.Errorf("method %q: neither Request nor RequestNote set", m)
		}
		if e.Response == "" && e.ResponseNote == "" {
			t.Errorf("method %q: neither Response nor ResponseNote set", m)
		}
		for _, name := range []string{e.Request, e.Response} {
			if name == "" {
				continue
			}
			if _, ok := singlesource.CanonicalWireTypes[name]; !ok {
				t.Errorf("method %q: wire type %q is not in singlesource.CanonicalWireTypes", m, name)
			}
			if _, ok := typeInstanceIndex[name]; !ok {
				t.Errorf("method %q: wire type %q has no typeInstanceIndex instance", m, name)
			}
		}
		if classify(m) == "unclassified" {
			t.Errorf("method %q: classify() found no predicate — extend classify / methodClusters", m)
		}
	}

	for m := range table {
		if !methods.IsValidMethod(m) {
			t.Errorf("methodTable names %q, which is not a canonical method — remove the stale row", m)
		}
	}

	// Every steering-control method must resolve a registered
	// ControlType so the RFC §6.3 scope column renders from
	// steering.RequiredScope, never a hand-typed value.
	for _, m := range methods.Methods() {
		if m == methods.MethodStart || !methods.IsControlMethod(m) {
			continue
		}
		ct, ok := methodToControlType[m]
		if !ok {
			t.Errorf("steering-control method %q has no methodToControlType entry", m)
			continue
		}
		if _, ok := steering.RequiredScope(ct); !ok {
			t.Errorf("method %q: ControlType %q has no RequiredScope entry", m, ct)
		}
	}
}

// TestGen_EventPayloadIndexInLockstep pins the event payload index
// against the live registry as the generator binary populates it (the
// prod-aggregator import plus the payload packages): every registered
// type has an index entry, no index entry names an unregistered type,
// an empty payload set carries an explanatory note, and every indexed
// payload implements events.EventPayload.
func TestGen_EventPayloadIndexInLockstep(t *testing.T) {
	registered := make(map[events.EventType]bool)
	for _, et := range events.EventTypes() {
		registered[et] = true
		entry, ok := eventPayloadIndex[et]
		if !ok {
			t.Errorf("event type %q registered but missing from eventPayloadIndex — add its payload row", et)
			continue
		}
		if len(entry.Payloads) == 0 && entry.Note == "" {
			t.Errorf("event type %q: empty payload set without an explanatory Note", et)
		}
		for _, pt := range entry.Payloads {
			if !pt.Implements(eventPayloadIface) && !reflect.PointerTo(pt).Implements(eventPayloadIface) {
				t.Errorf("event type %q: indexed payload %s does not implement events.EventPayload", et, pt)
			}
		}
	}
	for et := range eventPayloadIndex {
		if !registered[et] {
			t.Errorf("eventPayloadIndex names %q, which is not in the registry the generator binary sees — remove the stale row or import its registering package", et)
		}
	}
}

// TestGen_EventConstantsAllRegistered closes the registry-read blind
// spot: an event type registered from a package the generator does NOT
// import would be invisible to events.EventTypes() in this binary and
// silently absent from the docs. The test scans the production tree for
// `events.EventType = "..."` constant declarations and asserts each
// declared wire string is registered in this binary's registry.
func TestGen_EventConstantsAllRegistered(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	// Matches a typed EventType constant declaration: either the
	// subsystem-side `Name events.EventType = "..."` or the events
	// package's own `Name EventType = "..."`. A bare string assignment
	// (e.g. a metric label named "event_type") does not match — the
	// `EventType` token here is the declared TYPE, not part of the name.
	constRe := regexp.MustCompile(`\b[A-Za-z0-9_]+\s+(?:events\.)?EventType\s*=\s*"([a-z0-9_.]+)"`)

	err := filepath.WalkDir(filepath.Join(repoRoot, "internal"), func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, m := range constRe.FindAllStringSubmatch(string(src), -1) {
			et := events.EventType(m[1])
			if !events.IsValidEventType(et) {
				t.Errorf("%s declares event type %q which is NOT registered in the generator binary's registry — import its package from cmd/harbor-gen-protocol-docs/events.go and index its payload", path, et)
			}
			if _, ok := eventPayloadIndex[et]; !ok {
				t.Errorf("%s declares event type %q which has no eventPayloadIndex entry", path, et)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
}

// TestGen_ErrorTableInLockstep pins the error guidance table against
// errors.Codes(): every canonical code has a row, no row names an
// unregistered code.
func TestGen_ErrorTableInLockstep(t *testing.T) {
	for _, code := range protoerrors.Codes() {
		entry, ok := errorTable[code]
		if !ok {
			t.Errorf("error code %q registered but missing from errorTable — add its guidance row", code)
			continue
		}
		if entry.When == "" || entry.Retry == "" {
			t.Errorf("error code %q: When/Retry guidance incomplete", code)
		}
	}
	for code := range errorTable {
		if !protoerrors.IsValidCode(code) {
			t.Errorf("errorTable names %q, which is not a canonical code — remove the stale row", code)
		}
	}
}

// TestGen_TypeInstanceIndexInLockstep pins the reflection index against
// singlesource.CanonicalWireTypes: every canonical name has an
// instance whose reflect name matches, and no instance is stale.
func TestGen_TypeInstanceIndexInLockstep(t *testing.T) {
	for name := range singlesource.CanonicalWireTypes {
		rt, ok := typeInstanceIndex[name]
		if !ok {
			t.Errorf("canonical wire type %q has no typeInstanceIndex instance — add it", name)
			continue
		}
		if rt.Name() != name {
			t.Errorf("typeInstanceIndex[%q] resolves to %s — name mismatch", name, rt)
		}
		if rt.Kind() != reflect.Struct {
			t.Errorf("typeInstanceIndex[%q] is %s, want a struct", name, rt.Kind())
		}
	}
	for name := range typeInstanceIndex {
		if _, ok := singlesource.CanonicalWireTypes[name]; !ok {
			t.Errorf("typeInstanceIndex names %q, which is not in CanonicalWireTypes — remove the stale entry", name)
		}
	}
}
