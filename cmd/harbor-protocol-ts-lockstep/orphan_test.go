package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
)

// nonMethodRootedTypes are the canonical wire types that legitimately are NOT
// reachable from any method's request/response in the generated methods
// reference. Each needs a reason; an entry without one should not be here.
//
// Keep this map SMALL. Its purpose is to make the orphan check usable, not to
// absorb new orphans — a type added here instead of being wired to a method or
// deleted defeats the check entirely.
var nonMethodRootedTypes = map[string]string{
	// The version-negotiation surface. Served at the handshake entry point
	// rather than as a row in the method table, and consumed by clients
	// through types.CurrentHandshake() / Deprecations().
	"VersionHandshake": "version negotiation: served at the handshake entry point, not as a method",
	"Version":          "version negotiation: the parsed form of ProtocolVersion",
	"Deprecation":      "version negotiation: the deprecation-window record format",

	// The error envelope every method can return; it is the failure shape,
	// never a declared request/response cell.
	"Error": "the Protocol error envelope, returned by every method rather than declared by one",

	// The value type of GovernancePostureResponse.identity_tiers. The manifest
	// emits a map field as a bare "object" with no `ref`, so these are
	// reachable in reality but invisible to a ref-graph walk. This is a
	// manifest-expressiveness limit, not an orphan.
	"IdentityTierView": "map value type of GovernancePostureResponse.identity_tiers; map fields carry no ref in the manifest",
	"RateLimitView":    "nested in IdentityTierView, which is itself only reachable through a map field",
}

// TestManifest_NoOrphanWireTypes fails when a canonical wire type is published
// — registered in singlesource, emitted into the manifest, rendered into
// docs/site/protocol/types.md — but is not reachable from ANY method's
// request or response, directly or through the field `ref` graph.
//
// WHY THIS EXISTS. `GovernancePostureRequest` and `LLMPostureRequest` were
// exactly that: two types whose only field, `tenant_id`, was documented as
// gating a cross-tenant read behind `auth.ScopeAdmin`. Nothing ever decoded
// them — the posture family is decoded into the shared `RuntimeInfoRequest`
// envelope, whose `identity.tenant` is the real selector — so an admin naming
// another tenant silently received its OWN posture with a 200. The Protocol
// published a field the Runtime did not implement, for four phases, and the
// only reason it surfaced was that a strict decode turned the discard into a
// loud 400 (D-374).
//
// A type no method can reach is a type nothing decodes. That is the property
// worth pinning, and it is cheap: it is a reachability walk over data the
// generator already emits.
func TestManifest_NoOrphanWireTypes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot, committedManifestPath))
	if err != nil {
		t.Fatalf("read committed manifest: %v", err)
	}
	var man struct {
		Types map[string]struct {
			Fields []struct {
				Key string `json:"key"`
				Ref string `json:"ref"`
			} `json:"fields"`
		} `json:"types"`
	}
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}

	mdPath := filepath.Join(repoRoot, "docs/site/protocol/methods.md")
	md, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatalf("read %s: %v", mdPath, err)
	}
	// Roots: every type named in a method row's Request or Response cell.
	row := regexp.MustCompile("(?m)^\\|\\s*`([^`]+)`\\s*\\|\\s*`(?:GET|POST) [^`]+`\\s*\\|[^|]*\\|([^|]*)\\|([^|]*)\\|")
	link := regexp.MustCompile("\\[`([A-Za-z0-9_]+)`\\]")
	roots := map[string]bool{}
	rows := 0
	for _, m := range row.FindAllStringSubmatch(string(md), -1) {
		rows++
		for _, cell := range []string{m[2], m[3]} {
			for _, l := range link.FindAllStringSubmatch(cell, -1) {
				roots[l[1]] = true
			}
		}
	}
	// A zero-row parse would make every type look orphaned; fail on the
	// parse rather than emit a wall of false orphans.
	if rows == 0 {
		t.Fatalf("parsed ZERO method rows from %s — the generated table's shape changed and this check has gone inert", mdPath)
	}

	// Transitive closure over field refs.
	reachable := map[string]bool{}
	var walk func(string)
	walk = func(name string) {
		if reachable[name] {
			return
		}
		td, ok := man.Types[name]
		if !ok {
			return
		}
		reachable[name] = true
		for _, f := range td.Fields {
			if f.Ref != "" {
				walk(f.Ref)
			}
		}
	}
	for r := range roots {
		walk(r)
	}

	var orphans []string
	for name := range man.Types {
		if reachable[name] {
			continue
		}
		if _, allowed := nonMethodRootedTypes[name]; allowed {
			continue
		}
		orphans = append(orphans, name)
	}
	sort.Strings(orphans)
	if len(orphans) > 0 {
		t.Fatalf("orphan wire type(s) %v — published in the manifest and the Protocol reference but not reachable "+
			"from any method's request/response. Nothing decodes them, so any field they declare is a promise the "+
			"Runtime does not keep (D-374). Wire the type to a method, delete it, or — only if it is genuinely a "+
			"non-method surface — add it to nonMethodRootedTypes with a reason.", orphans)
	}

	// Allowlist hygiene: an entry naming a type that is no longer in the
	// manifest, or that HAS become reachable, is stale and must go.
	for name := range nonMethodRootedTypes {
		td, inManifest := man.Types[name]
		_ = td
		if !inManifest {
			t.Errorf("nonMethodRootedTypes names %q, which is not a canonical wire type — remove the stale entry", name)
			continue
		}
		if reachable[name] {
			t.Errorf("nonMethodRootedTypes names %q, but it IS reachable from a method — remove the stale exemption", name)
		}
	}
}
