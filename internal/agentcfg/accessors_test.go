package agentcfg_test

import (
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
)

// TestPayloadAccessors covers the pure ConfigPayload accessor helpers in both
// the set and unset arms — they back the run-start projections and the diff
// surface, so exercising them directly pins their behaviour independent of a
// driver.
func TestPayloadAccessors(t *testing.T) {
	base := "operator-base"
	user := "user-layer"
	temp := 0.5
	full := agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{Base: &base, User: &user},
		Skills:       &agentcfg.SkillsSelection{Names: []string{"s1", "s2"}},
		ToolExposure: &agentcfg.ToolExposure{PausedServers: []string{"srv"}, DisabledTools: []string{"srv_t"}},
		Connections:  &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{{Name: "c1", Transport: agentcfg.MCPTransportHTTP, URL: "http://x"}}},
		LLMParams:    &agentcfg.LLMParams{Temperature: &temp},
	}
	if got := full.SkillNames(); len(got) != 2 {
		t.Errorf("SkillNames = %v", got)
	}
	if got := full.PausedServers(); len(got) != 1 || got[0] != "srv" {
		t.Errorf("PausedServers = %v", got)
	}
	if got := full.DisabledTools(); len(got) != 1 || got[0] != "srv_t" {
		t.Errorf("DisabledTools = %v", got)
	}
	if got := full.ConnectionDescriptors(); len(got) != 1 || got[0].Name != "c1" {
		t.Errorf("ConnectionDescriptors = %v", got)
	}
	if b, ok := full.BasePrompt(); !ok || b != base {
		t.Errorf("BasePrompt = %q,%v", b, ok)
	}
	if u, ok := full.UserPrompt(); !ok || u != user {
		t.Errorf("UserPrompt = %q,%v", u, ok)
	}
	if lp, ok := full.LLMParamsView(); !ok || lp.Temperature == nil {
		t.Errorf("LLMParamsView = %+v,%v", lp, ok)
	}

	// The unset arms return zero values / false.
	var empty agentcfg.ConfigPayload
	if empty.SkillNames() != nil || empty.PausedServers() != nil || empty.DisabledTools() != nil || empty.ConnectionDescriptors() != nil {
		t.Error("empty payload accessors should be nil")
	}
	if _, ok := empty.BasePrompt(); ok {
		t.Error("empty BasePrompt should be unset")
	}
	if _, ok := empty.UserPrompt(); ok {
		t.Error("empty UserPrompt should be unset")
	}
	if _, ok := empty.LLMParamsView(); ok {
		t.Error("empty LLMParamsView should be unset")
	}
}

// TestDiffHelpers covers the pure diff functions + their Changed() predicates
// directly (they are otherwise only exercised through the driver conformance).
func TestDiffHelpers(t *testing.T) {
	b1, b2 := "base-a", "base-b"
	u1, u2 := "user-a", "user-b"
	from := agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{Base: &b1, User: &u1},
		ToolExposure: &agentcfg.ToolExposure{PausedServers: []string{"keep", "drop"}, DisabledTools: []string{"t-keep"}},
		Connections:  &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{{Name: "c-old"}}},
	}
	to := agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{Base: &b2, User: &u2},
		ToolExposure: &agentcfg.ToolExposure{PausedServers: []string{"keep", "new"}, DisabledTools: []string{"t-keep", "t-new"}},
		Connections:  &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{{Name: "c-new"}}},
	}

	pl := agentcfg.DiffPromptLayers(from, to)
	if !pl.Changed() || !pl.BaseChanged || !pl.UserChanged || pl.BaseTo != "base-b" || pl.UserTo != "user-b" {
		t.Errorf("DiffPromptLayers = %+v", pl)
	}

	te := agentcfg.DiffToolExposure(from, to)
	if !te.Changed() {
		t.Errorf("DiffToolExposure unchanged: %+v", te)
	}
	if len(te.PausedAdded) != 1 || te.PausedAdded[0] != "new" || len(te.PausedResumed) != 1 || te.PausedResumed[0] != "drop" {
		t.Errorf("paused diff = %+v", te)
	}
	if len(te.DisabledAdded) != 1 || te.DisabledAdded[0] != "t-new" {
		t.Errorf("disabled diff = %+v", te)
	}

	cn := agentcfg.DiffConnections(from, to)
	if !cn.Changed() || len(cn.Added) != 1 || cn.Added[0] != "c-new" || len(cn.Removed) != 1 || cn.Removed[0] != "c-old" {
		t.Errorf("DiffConnections = %+v", cn)
	}

	sk := agentcfg.DiffSkills([]string{"a", "b"}, []string{"b", "c"})
	if !sk.Changed() || len(sk.Added) != 1 || sk.Added[0] != "c" || len(sk.Removed) != 1 || sk.Removed[0] != "a" {
		t.Errorf("DiffSkills = %+v", sk)
	}

	m1, m2 := "model-a", "model-b"
	re1 := "low"
	lp := agentcfg.DiffLLMParams(
		agentcfg.ConfigPayload{LLMParams: &agentcfg.LLMParams{Model: &m1, ReasoningEffort: &re1}},
		agentcfg.ConfigPayload{LLMParams: &agentcfg.LLMParams{Model: &m2}},
	)
	if !lp.Changed() || !lp.ModelChanged || lp.ModelTo != "model-b" || !lp.ReasoningEffortChanged {
		t.Errorf("DiffLLMParams = %+v", lp)
	}

	// No-change arms: a payload diffed against itself reports unchanged.
	if agentcfg.DiffPromptLayers(from, from).Changed() {
		t.Error("identical prompt-layer diff should be unchanged")
	}
	if agentcfg.DiffToolExposure(from, from).Changed() {
		t.Error("identical tool-exposure diff should be unchanged")
	}
	if agentcfg.DiffConnections(from, from).Changed() {
		t.Error("identical connections diff should be unchanged")
	}
}
