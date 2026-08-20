package bodyscope_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/bodyscope"
)

// moduleRoot walks up from the test's working directory to the module
// root so the gates scan the real tree rather than a fixture.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for range 8 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("module root not found above the test working directory")
	return ""
}

// handRolledGateAllowList records the files permitted to reconcile a
// body identity component by hand, and why. The list is the reviewed
// exception set; every entry is prose a reviewer can disagree with.
var handRolledGateAllowList = map[string]string{
	filepath.Join("transports", "control", "control.go"): "the impersonation gate reconciles the body's ACTOR triplet against the verified identity, a different contract from the body-scope reconciler: the top-level triple is deliberately the impersonated identity and the actor is the audit anchor",
	"artifacts.go":                           "the surface re-checks the tenant posture the registry already states per method (list/delete elevatable, put admin-only, get_ref flat) so an embedder calling Dispatch directly meets the same answer as a wire caller; delete carries no tenant check of its own and rides the surface policy",
	filepath.Join("auth", "rotate_token.go"): "the auth package sits BELOW the body-scope gate in the import graph — the gate reads the verified scope set from here — so the token-rotation surface cannot import it. The transport gate at transports/stream/auth_handler.go is the authority; this is defence in depth",
	filepath.Join("client", "client.go"):     "the Protocol client library, not a server-side gate: it compares two request-body scopes to each other while composing an outbound request, and reconciles nothing against a verified identity",
}

// elevationSiteAllowList records the files permitted to seat a verified
// identity or cross the tenant boundary, and why.
var elevationSiteAllowList = map[string]string{
	filepath.Join("internal", "protocol", "auth", "middleware.go"):               "the request-edge write: the auth middleware seats the token-verified triple once per request",
	filepath.Join("internal", "protocol", "transports", "stream", "stream.go"):   "the streaming transport's identity choke point seats the request's established triple once, for the deployments that carry it outside a bearer token",
	filepath.Join("internal", "protocol", "bodyscope", "bodyscope.go"):           "the shared reconciler: the one gate that checks the admin-tier claim and records the crossing before granting it",
	filepath.Join("internal", "protocol", "transports", "control", "control.go"): "an accepted admin impersonation runs under the impersonated identity; the impersonation gate verified the admin claim and pinned the actor to the verified identity before this call, and the accepted path is audited",
	filepath.Join("internal", "protocol", "control.go"):                          "a cross-tenant topology snapshot runs under the named tenant; the surface verified the admin claim and wrote the audit record before this call",
	filepath.Join("internal", "protocol", "posture.go"):                          "a cross-tenant posture read runs under the named tenant; the surface verified the admin-tier claim before this call and writes the audit record on the way out",
	filepath.Join("internal", "tasks", "agent_reach_admission.go"):               "a persisted Agent-reach receipt re-seats the verified task identity only after envelope, digest, identity, and task-binding checks pass; it cannot widen the signed authority",

	// Per-ROW re-scopes behind an already-authorized fan-in. Each reads a
	// row under that row's own identity; the listing or search that
	// produced the row was gated on the caller's verified admin-tier
	// claim and recorded before these run.
	filepath.Join("internal", "sessions", "protocol", "enricher.go"):                "a fleet session listing rolls up each row's counters under that row's own identity; the mint re-checks the admin-tier claim itself rather than inheriting the listing's check",
	filepath.Join("internal", "search", "tasks", "index.go"):                        "a fleet task search reads each hit under the identity of the session that matched; the mint re-checks the admin-tier claim itself rather than inheriting the fan-in's check",
	filepath.Join("internal", "memory", "strategy", "semantic.go"):                  "an embedding call is attributed to the identity the stored vectors are scoped under, so provenance follows the record rather than the reader",
	filepath.Join("internal", "skills", "drivers", "localdb", "search_semantic.go"): "an embedding call is attributed to the identity the stored skill is scoped under, so provenance follows the record rather than the reader",
	filepath.Join("internal", "skills", "drivers", "postgres", "search.go"):         "an embedding call is attributed to the identity the stored skill is scoped under, so provenance follows the record rather than the reader",
	filepath.Join("internal", "skills", "snapshot_candidate_searcher.go"):           "a frozen, already-authorized run snapshot attributes semantic embedding to its bound candidate identity; cross-tenant attribution uses the same reviewed path as the configured store drivers",
}

// TestGate_Coverage_EveryScopeCarryingRequestIsRegistered is the
// coverage half. A canonical Protocol request type that carries an
// identity scope has a surface whose policy governs it, and every
// registry row names a type the canonical packages still declare.
func TestGate_Coverage_EveryScopeCarryingRequestIsRegistered(t *testing.T) {
	root := moduleRoot(t)
	typesDir := filepath.Join(root, "internal", "protocol", "types")

	violations, files, err := bodyscope.ScanWireTypes(typesDir)
	if err != nil {
		t.Fatalf("ScanWireTypes: %v", err)
	}
	if files < 10 {
		t.Fatalf("the coverage scan read only %d files under %s; a scan that reaches nothing cannot gate anything", files, typesDir)
	}
	for _, v := range violations {
		t.Errorf("%s", v)
	}
}

// scopeTypeName is the canonical body identity-scope type the coverage
// scan recognises. Held as a constant so the fixtures below can declare
// one without writing the declaration verbatim.
const scopeTypeName = "Identity" + "Scope"

// TestGate_Coverage_ScanIsNonVacuous proves the coverage scan bites: an
// unregistered scope-carrying request type in a fixture tree is
// reported.
func TestGate_Coverage_ScanIsNonVacuous(t *testing.T) {
	dir := t.TempDir()
	// The fixture declares the canonical scope shape so the scan has
	// something to recognise. The declaration is ASSEMBLED from
	// scopeTypeName rather than written out: the single-source guard
	// greps for the verbatim declaration of that wire type anywhere
	// outside its canonical package, and a test fixture must not read as
	// a second definition site to a scanner that reads bytes.
	src := "package types\n\n" +
		"type " + scopeTypeName + " struct{ Tenant, User, Session string }\n\n" +
		"// UnregisteredThingRequest carries a scope and has no registry row.\n" +
		"type UnregisteredThingRequest struct {\n\tIdentity " + scopeTypeName + "\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	violations, _, err := bodyscope.ScanWireTypes(dir)
	if err != nil {
		t.Fatalf("ScanWireTypes: %v", err)
	}
	found := false
	for _, v := range violations {
		if v.Kind == bodyscope.KindUnregisteredRequest && strings.Contains(v.Detail, "UnregisteredThingRequest") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the coverage scan did not report an unregistered scope-carrying request type; violations=%v", violations)
	}
}

// TestGate_Coverage_DeletingARegistrationFails proves the reverse
// direction bites: a registry row whose wire type is absent from the
// scanned tree is reported as stale. The fixture tree declares none of
// the canonical types, so every joined row must surface.
func TestGate_Coverage_DeletingARegistrationFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "empty.go"), []byte("package types\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	violations, _, err := bodyscope.ScanWireTypes(dir)
	if err != nil {
		t.Fatalf("ScanWireTypes: %v", err)
	}
	stale := 0
	for _, v := range violations {
		if v.Kind == bodyscope.KindStaleRegistration {
			stale++
		}
	}
	want := len(bodyscope.JoinedRequestTypes()) + len(bodyscope.ExemptScopeCarriers())
	if stale != want {
		t.Fatalf("stale-registration violations = %d, want %d (one per registry row absent from the scanned tree)", stale, want)
	}
}

// TestGate_Enforcement_NoHandRolledBodyScopeGates is the enforcement
// half. No file under internal/protocol reconciles a body identity
// component against a verified one outside the shared reconciler.
func TestGate_Enforcement_NoHandRolledBodyScopeGates(t *testing.T) {
	root := filepath.Join(moduleRoot(t), "internal", "protocol")

	violations, files, err := bodyscope.ScanHandRolledGates(root, handRolledGateAllowList)
	if err != nil {
		t.Fatalf("ScanHandRolledGates: %v", err)
	}
	if files < 30 {
		t.Fatalf("the enforcement scan read only %d files under %s; a scan that reaches nothing cannot gate anything", files, root)
	}
	for _, v := range violations {
		t.Errorf("%s", v)
	}
}

// TestGate_Enforcement_ScanIsNonVacuous proves the enforcement scan
// bites on both comparison shapes.
func TestGate_Enforcement_ScanIsNonVacuous(t *testing.T) {
	dir := t.TempDir()
	src := `package handler

type wire struct{ Tenant, User, Session string }
type id struct{ TenantID, UserID, SessionID string }

func crossShape(body wire, verified id) bool {
	return body.Tenant != verified.TenantID
}

func sameShape(body, verified wire) bool {
	return body.Session != verified.Session
}
`
	if err := os.WriteFile(filepath.Join(dir, "handler.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	violations, _, err := bodyscope.ScanHandRolledGates(dir, nil)
	if err != nil {
		t.Fatalf("ScanHandRolledGates: %v", err)
	}
	if len(violations) != 2 {
		t.Fatalf("hand-rolled-gate violations = %d, want 2 (one per comparison shape); got %v", len(violations), violations)
	}
}

// TestGate_Enforcement_HoistingIntoALocalDoesNotEvadeTheScan — pulling a
// component into a local before comparing is an ordinary refactor, and
// the bug class this gate closes is literally "a contributor writes the
// next handler". The scan reads through the hoist.
func TestGate_Enforcement_HoistingIntoALocalDoesNotEvadeTheScan(t *testing.T) {
	dir := t.TempDir()
	src := `package handler

type wire struct{ Tenant, User, Session string }
type id struct{ TenantID, UserID, SessionID string }

func hoisted(body wire, verified id) bool {
	bodyTenant := body.Tenant
	verifiedTenant := verified.TenantID
	return bodyTenant != verifiedTenant
}
`
	if err := os.WriteFile(filepath.Join(dir, "handler.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	violations, _, err := bodyscope.ScanHandRolledGates(dir, nil)
	if err != nil {
		t.Fatalf("ScanHandRolledGates: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("a hoisted comparison produced %d violations, want 1; got %v", len(violations), violations)
	}
}

// TestGate_Enforcement_CaseFoldingDoesNotEvadeTheScan — identity
// components are compared EXACTLY, so folding case over one is either a
// hand-rolled gate or a correctness bug. Either way the scan says so.
func TestGate_Enforcement_CaseFoldingDoesNotEvadeTheScan(t *testing.T) {
	dir := t.TempDir()
	src := `package handler

import "strings"

type wire struct{ Tenant, User, Session string }
type id struct{ TenantID, UserID, SessionID string }

func folded(body wire, verified id) bool {
	return strings.EqualFold(body.Tenant, verified.TenantID)
}
`
	if err := os.WriteFile(filepath.Join(dir, "handler.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	violations, _, err := bodyscope.ScanHandRolledGates(dir, nil)
	if err != nil {
		t.Fatalf("ScanHandRolledGates: %v", err)
	}
	if len(violations) == 0 {
		t.Fatal("a case-folded identity comparison was not reported")
	}
}

// TestGate_Coverage_AReasonlessExemptionIsAViolation — an exemption
// REMOVES a type from the gate's universe. It is the only allow-list
// that makes the gate inspect less, so a blank reason there fails just
// as the sibling scans' blank reasons do.
func TestGate_Coverage_AReasonlessExemptionIsAViolation(t *testing.T) {
	dir := t.TempDir()
	// SessionRow is a registered exempt carrier; re-declaring it here with
	// the fixture scope shape exercises the exempt branch.
	src := "package types\n\n" +
		"type " + scopeTypeName + " struct{ Tenant, User, Session string }\n\n" +
		"type SessionRow struct {\n\tIdentity " + scopeTypeName + "\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	violations, _, err := bodyscope.ScanWireTypes(dir)
	if err != nil {
		t.Fatalf("ScanWireTypes: %v", err)
	}
	for _, v := range violations {
		if v.Kind == bodyscope.KindUnregisteredCarrier {
			t.Errorf("a registered exempt carrier with a real reason was flagged: %s", v)
		}
	}
	// Every shipped exemption carries a non-empty reason.
	for name, why := range bodyscope.ExemptScopeCarriers() {
		if strings.TrimSpace(why) == "" {
			t.Errorf("exempt carrier %q carries no reason", name)
		}
	}
}

// TestGate_Enforcement_AllowListEntryNeedsAReason proves an exemption
// cannot be added silently.
func TestGate_Enforcement_AllowListEntryNeedsAReason(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "empty.go"), []byte("package p\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	violations, _, err := bodyscope.ScanHandRolledGates(dir, map[string]string{"some/file.go": "  "})
	if err != nil {
		t.Fatalf("ScanHandRolledGates: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("a reasonless allow-list entry produced %d violations, want 1; got %v", len(violations), violations)
	}
}

// TestGate_Minting_EveryElevationSiteIsRegistered is the minting half.
// Seating a verified identity and crossing a tenant boundary happen only
// at reviewed call sites.
func TestGate_Minting_EveryElevationSiteIsRegistered(t *testing.T) {
	root := moduleRoot(t)

	violations, files, err := bodyscope.ScanElevationSites(root, elevationSiteAllowList)
	if err != nil {
		t.Fatalf("ScanElevationSites: %v", err)
	}
	if files < 200 {
		t.Fatalf("the minting scan read only %d files under %s; a scan that reaches nothing cannot gate anything", files, root)
	}
	for _, v := range violations {
		t.Errorf("%s", v)
	}
}

// mintingFixture renders a file that calls an identity writer under the
// supplied import spec, so a test can vary how the package is bound.
func mintingFixture(importSpec, qualifier string) string {
	return `package worker

import (
	"context"

	` + importSpec + `
)

func widen(ctx context.Context, id ` + qualifier + `.Identity) (context.Context, error) {
	return ` + qualifier + `.WithElevated(ctx, id, "because")
}
`
}

// TestGate_Minting_ScanIsNonVacuous proves the minting scan bites, and
// registering the site clears it.
func TestGate_Minting_ScanIsNonVacuous(t *testing.T) {
	dir := t.TempDir()
	src := mintingFixture(`"github.com/hurtener/Harbor/internal/identity"`, "identity")
	if err := os.WriteFile(filepath.Join(dir, "worker.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	violations, _, err := bodyscope.ScanElevationSites(dir, nil)
	if err != nil {
		t.Fatalf("ScanElevationSites: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("minting violations = %d, want 1; got %v", len(violations), violations)
	}
	// Registering the site clears it.
	violations, _, err = bodyscope.ScanElevationSites(dir, map[string]string{"worker.go": "fixture"})
	if err != nil {
		t.Fatalf("ScanElevationSites: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("registering the site left %d violations: %v", len(violations), violations)
	}
}

// TestGate_Minting_AnImportAliasDoesNotEvadeTheScan — renaming an import
// is an ordinary refactor, so a scan keyed on the conventional package
// qualifier would quietly stop seeing the file that did it. The scan
// resolves the import PATH and follows the alias.
func TestGate_Minting_AnImportAliasDoesNotEvadeTheScan(t *testing.T) {
	dir := t.TempDir()
	src := mintingFixture(`ident "github.com/hurtener/Harbor/internal/identity"`, "ident")
	if err := os.WriteFile(filepath.Join(dir, "aliased.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	violations, _, err := bodyscope.ScanElevationSites(dir, nil)
	if err != nil {
		t.Fatalf("ScanElevationSites: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("an aliased import produced %d violations, want 1; got %v", len(violations), violations)
	}
}

// TestGate_Minting_AnUnrelatedIdentityQualifierIsNotFlagged — the scan
// keys on the import path, so a local symbol that merely happens to be
// spelled `identity` is not a minting site.
func TestGate_Minting_AnUnrelatedIdentityQualifierIsNotFlagged(t *testing.T) {
	dir := t.TempDir()
	src := `package worker

import "context"

type ident struct{}

var identity struct {
	WithElevated func(context.Context, ident, string) (context.Context, error)
}

func widen(ctx context.Context, i ident) (context.Context, error) {
	return identity.WithElevated(ctx, i, "because")
}
`
	if err := os.WriteFile(filepath.Join(dir, "lookalike.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	violations, _, err := bodyscope.ScanElevationSites(dir, nil)
	if err != nil {
		t.Fatalf("ScanElevationSites: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("a lookalike local symbol was flagged: %v", violations)
	}
}

// TestGate_Registry_EverySurfaceDeclaresItsPosture pins the closed
// registry: a policy names itself, carries an operator-facing wire name
// and a reason, and a cross-identity policy never leaves the session
// component elevatable.
func TestGate_Registry_EverySurfaceDeclaresItsPosture(t *testing.T) {
	surfaces := bodyscope.RegisteredSurfaces()
	if len(surfaces) == 0 {
		t.Fatal("the surface registry is empty")
	}
	for _, s := range surfaces {
		p, ok := bodyscope.PolicyFor(s)
		if !ok {
			t.Fatalf("surface %q is listed but has no policy", s)
		}
		if p.Surface != s {
			t.Errorf("surface %q declares Surface=%q; the row must name itself", s, p.Surface)
		}
		if p.Wire == "" {
			t.Errorf("surface %q declares no operator-facing wire name", s)
		}
		if strings.TrimSpace(p.Reason) == "" {
			t.Errorf("surface %q declares no reason; a posture nobody justified is a posture nobody reviewed", s)
		}
		// The session is the innermost isolation scope. A surface may
		// reach another session only as part of a whole cross-identity
		// read — never on its own, and never while the tenant or the
		// user stays pinned to the caller.
		if p.Session == bodyscope.AdminScoped && (p.Tenant != bodyscope.AdminScoped || p.User != bodyscope.AdminScoped) {
			t.Errorf("surface %q elevates the session while the tenant or the user stays pinned; the session is the innermost isolation scope and is reachable only as part of a whole cross-identity read", s)
		}
	}
}

// TestGate_Enforcement_AnAliasedStringsImportDoesNotEvadeTheScan — the
// enforcement scan resolves `strings` by import PATH, matching how the
// minting scan resolves `identity`. One scan following an alias and the
// other not would be a gap a reader could not predict.
func TestGate_Enforcement_AnAliasedStringsImportDoesNotEvadeTheScan(t *testing.T) {
	dir := t.TempDir()
	src := `package handler

import s "strings"

type wire struct{ Tenant, User, Session string }
type id struct{ TenantID, UserID, SessionID string }

func folded(body wire, verified id) bool {
	return s.EqualFold(body.Tenant, verified.TenantID)
}
`
	if err := os.WriteFile(filepath.Join(dir, "aliased.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	violations, _, err := bodyscope.ScanHandRolledGates(dir, nil)
	if err != nil {
		t.Fatalf("ScanHandRolledGates: %v", err)
	}
	if len(violations) == 0 {
		t.Fatal("an aliased strings import evaded the case-folding scan")
	}
}
