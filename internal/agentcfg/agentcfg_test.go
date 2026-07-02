package agentcfg_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

func realDeps(t *testing.T) agentcfg.Deps {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 8,
		SubscriberBufferSize:     32,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	t.Cleanup(func() {
		_ = bus.Close(t.Context())
		_ = st.Close(t.Context())
	})
	return agentcfg.Deps{State: st, Bus: bus}
}

func TestContentHash_StableAcrossNameOrder(t *testing.T) {
	a, err := agentcfg.ContentHash(agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"b", "a", "c"}}})
	if err != nil {
		t.Fatalf("hash a: %v", err)
	}
	b, err := agentcfg.ContentHash(agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"c", "a", "b"}}})
	if err != nil {
		t.Fatalf("hash b: %v", err)
	}
	if a != b {
		t.Fatalf("hash must be order-independent: %q != %q", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("hash must be full hex SHA-256 (64 chars), got %d", len(a))
	}
}

func TestContentHash_DiffersOnContent(t *testing.T) {
	a, _ := agentcfg.ContentHash(agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"a"}}})
	b, _ := agentcfg.ContentHash(agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"a", "b"}}})
	if a == b {
		t.Fatalf("different content must hash differently")
	}
}

func TestContentHash_NilVsEmptySkills(t *testing.T) {
	none, _ := agentcfg.ContentHash(agentcfg.ConfigPayload{})
	empty, _ := agentcfg.ContentHash(agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{}}})
	if none == empty {
		t.Fatalf("absent skills section must differ from present-but-empty")
	}
}

func TestNormalizePayload_SortsAndDedups(t *testing.T) {
	got := agentcfg.NormalizePayload(agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"b", "a", "b", "c"}}})
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got.Skills.Names, want) {
		t.Fatalf("normalize=%v want %v", got.Skills.Names, want)
	}
}

func TestNormalizePayload_NilSkillsStaysNil(t *testing.T) {
	got := agentcfg.NormalizePayload(agentcfg.ConfigPayload{})
	if got.Skills != nil {
		t.Fatalf("nil skills must stay nil, got %+v", got.Skills)
	}
}

func TestDiffSkills_Deterministic(t *testing.T) {
	d := agentcfg.DiffSkills([]string{"a", "b"}, []string{"b", "c", "d"})
	if !reflect.DeepEqual(d.Added, []string{"c", "d"}) {
		t.Fatalf("added=%v want [c d]", d.Added)
	}
	if !reflect.DeepEqual(d.Removed, []string{"a"}) {
		t.Fatalf("removed=%v want [a]", d.Removed)
	}
	if !d.Changed() {
		t.Fatalf("Changed() should be true")
	}
	same := agentcfg.DiffSkills([]string{"a"}, []string{"a"})
	if same.Changed() {
		t.Fatalf("identical sets should report Changed()=false")
	}
}

func TestOpen_NilDepsFailLoud(t *testing.T) {
	if _, err := agentcfg.Open(t.Context(), agentcfg.Config{}, agentcfg.Deps{}); err == nil {
		t.Fatalf("Open with nil deps must fail loud")
	}
}

func TestOpen_UnknownDriver(t *testing.T) {
	_, err := agentcfg.Open(t.Context(), agentcfg.Config{Driver: "nope"}, realDeps(t))
	if err == nil {
		t.Fatalf("unknown driver must fail")
	}
}

// TestNormalizePayload_Connections_CanonicalForm proves the connections
// section is canonicalised: sorted by name, name-unique (last wins), with the
// argv Command order preserved verbatim. Equal states hash equal.
func TestNormalizePayload_Connections_CanonicalForm(t *testing.T) {
	a := agentcfg.ConfigPayload{Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{
		{Name: "zeta", Transport: agentcfg.MCPTransportStdio, Command: []string{"/bin/z", "--x"}},
		{Name: "alpha", Transport: agentcfg.MCPTransportHTTP, URL: "https://a"},
		{Name: "zeta", Transport: agentcfg.MCPTransportStdio, Command: []string{"/bin/z2"}}, // re-add: last wins
	}}}
	b := agentcfg.ConfigPayload{Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{
		{Name: "alpha", Transport: agentcfg.MCPTransportHTTP, URL: "https://a"},
		{Name: "zeta", Transport: agentcfg.MCPTransportStdio, Command: []string{"/bin/z2"}},
	}}}
	ha, err := agentcfg.ContentHash(a)
	if err != nil {
		t.Fatalf("hash a: %v", err)
	}
	hb, err := agentcfg.ContentHash(b)
	if err != nil {
		t.Fatalf("hash b: %v", err)
	}
	if ha != hb {
		t.Fatalf("re-ordered + re-added connections must hash equal: %s != %s", ha, hb)
	}
	norm := agentcfg.NormalizePayload(a).ConnectionDescriptors()
	if len(norm) != 2 || norm[0].Name != "alpha" || norm[1].Name != "zeta" {
		t.Fatalf("normalize not sorted/deduped: %+v", norm)
	}
	if len(norm[1].Command) != 1 || norm[1].Command[0] != "/bin/z2" {
		t.Errorf("last-wins dedup failed: %+v", norm[1])
	}
}

// TestDiffConnections proves the structured by-name set-diff.
func TestDiffConnections(t *testing.T) {
	from := agentcfg.ConfigPayload{Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{
		{Name: "keep", Transport: agentcfg.MCPTransportHTTP, URL: "https://k"},
		{Name: "drop", Transport: agentcfg.MCPTransportHTTP, URL: "https://d"},
	}}}
	to := agentcfg.ConfigPayload{Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{
		{Name: "keep", Transport: agentcfg.MCPTransportHTTP, URL: "https://k"},
		{Name: "add", Transport: agentcfg.MCPTransportHTTP, URL: "https://n"},
	}}}
	d := agentcfg.DiffConnections(from, to)
	if !d.Changed() {
		t.Fatal("expected a change")
	}
	if len(d.Added) != 1 || d.Added[0] != "add" {
		t.Errorf("added = %+v, want [add]", d.Added)
	}
	if len(d.Removed) != 1 || d.Removed[0] != "drop" {
		t.Errorf("removed = %+v, want [drop]", d.Removed)
	}
}

// TestNormalizePayload_LoadingModes_DropsEmptyKeys proves an empty-key entry
// in either loading-mode map is dropped by normalization (D-281) — a
// phantom key never perturbs the content hash.
func TestNormalizePayload_LoadingModes_DropsEmptyKeys(t *testing.T) {
	got := agentcfg.NormalizePayload(agentcfg.ConfigPayload{ToolExposure: &agentcfg.ToolExposure{
		ServerLoadingModes: map[string]string{"": "always", "srvA": "deferred"},
		ToolLoadingModes:   map[string]string{"": "deferred"},
	}})
	if len(got.ToolExposure.ServerLoadingModes) != 1 || got.ToolExposure.ServerLoadingModes["srvA"] != "deferred" {
		t.Fatalf("server_loading_modes = %+v, want only srvA", got.ToolExposure.ServerLoadingModes)
	}
	if got.ToolExposure.ToolLoadingModes != nil {
		t.Fatalf("tool_loading_modes should normalize to nil (only empty key), got %+v", got.ToolExposure.ToolLoadingModes)
	}
}

// TestContentHash_LoadingModes_IdempotentReSet proves an idempotent re-set of
// byte-identical loading-mode override content produces the SAME content
// hash (map-key ordering is stable via Go's canonical JSON encoding).
func TestContentHash_LoadingModes_IdempotentReSet(t *testing.T) {
	p := agentcfg.ConfigPayload{ToolExposure: &agentcfg.ToolExposure{
		ServerLoadingModes: map[string]string{"srvA": "deferred", "srvB": "always"},
		ToolLoadingModes:   map[string]string{"srvA_toolX": "always"},
	}}
	a, err := agentcfg.ContentHash(p)
	if err != nil {
		t.Fatalf("hash a: %v", err)
	}
	b, err := agentcfg.ContentHash(p)
	if err != nil {
		t.Fatalf("hash b: %v", err)
	}
	if a != b {
		t.Fatalf("idempotent re-set must hash identically: %q != %q", a, b)
	}
	// A differing value must hash differently.
	p2 := agentcfg.ConfigPayload{ToolExposure: &agentcfg.ToolExposure{
		ServerLoadingModes: map[string]string{"srvA": "always", "srvB": "always"},
		ToolLoadingModes:   map[string]string{"srvA_toolX": "always"},
	}}
	c, err := agentcfg.ContentHash(p2)
	if err != nil {
		t.Fatalf("hash c: %v", err)
	}
	if a == c {
		t.Fatalf("differing loading-mode content must hash differently")
	}
}

// TestDiffToolExposure_LoadingModeChanges proves the structured diff reports
// per-key loading-mode deltas for both the server and tool arms, including a
// set->unset transition and an unchanged key producing no entry.
func TestDiffToolExposure_LoadingModeChanges(t *testing.T) {
	from := agentcfg.ConfigPayload{ToolExposure: &agentcfg.ToolExposure{
		ServerLoadingModes: map[string]string{"srvA": "always", "srvB": "deferred"},
		ToolLoadingModes:   map[string]string{"srvA_toolX": "always"},
	}}
	to := agentcfg.ConfigPayload{ToolExposure: &agentcfg.ToolExposure{
		ServerLoadingModes: map[string]string{"srvA": "deferred"}, // srvA changed, srvB removed
		ToolLoadingModes:   map[string]string{"srvA_toolX": "always", "srvA_toolY": "deferred"},
	}}
	d := agentcfg.DiffToolExposure(from, to)
	if !d.Changed() {
		t.Fatal("expected a change")
	}
	wantServer := map[string]agentcfg.LoadingModeChange{
		"srvA": {Key: "srvA", From: "always", To: "deferred"},
		"srvB": {Key: "srvB", From: "deferred", To: ""},
	}
	if len(d.ServerLoadingChanges) != len(wantServer) {
		t.Fatalf("server_loading_changes = %+v, want %d entries", d.ServerLoadingChanges, len(wantServer))
	}
	for _, c := range d.ServerLoadingChanges {
		if !reflect.DeepEqual(c, wantServer[c.Key]) {
			t.Errorf("server change %q = %+v, want %+v", c.Key, c, wantServer[c.Key])
		}
	}
	if len(d.ToolLoadingChanges) != 1 || d.ToolLoadingChanges[0].Key != "srvA_toolY" {
		t.Fatalf("tool_loading_changes = %+v, want one entry for srvA_toolY", d.ToolLoadingChanges)
	}
	// An unchanged key (srvA_toolX unchanged) must NOT appear in the diff.
	for _, c := range d.ToolLoadingChanges {
		if c.Key == "srvA_toolX" {
			t.Fatalf("unchanged key srvA_toolX should not appear in the diff")
		}
	}
}

// TestConfigPayload_LoadingModeAccessors proves the ConfigPayload accessors
// return nil for an absent tool-exposure section and the stored maps
// otherwise.
func TestConfigPayload_LoadingModeAccessors(t *testing.T) {
	var empty agentcfg.ConfigPayload
	if empty.ServerLoadingModes() != nil || empty.ToolLoadingModes() != nil {
		t.Fatal("absent tool-exposure section must report nil loading maps")
	}
	p := agentcfg.ConfigPayload{ToolExposure: &agentcfg.ToolExposure{
		ServerLoadingModes: map[string]string{"srvA": "always"},
		ToolLoadingModes:   map[string]string{"srvA_x": "deferred"},
	}}
	if p.ServerLoadingModes()["srvA"] != "always" {
		t.Fatalf("ServerLoadingModes() = %+v", p.ServerLoadingModes())
	}
	if p.ToolLoadingModes()["srvA_x"] != "deferred" {
		t.Fatalf("ToolLoadingModes() = %+v", p.ToolLoadingModes())
	}
}
