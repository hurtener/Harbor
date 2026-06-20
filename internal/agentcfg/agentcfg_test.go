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
