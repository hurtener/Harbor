package serve

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
)

// mcp_reattacher_descriptor_parity_test.go — the mechanical guard for the
// failure class the v1.24 checkpoint audit found: the run-start re-attach
// rebuilt the driver config field-by-field and silently DROPPED the
// egress-substitution declaration two phases had threaded end to end, so a
// byte-eligible connection came back `online`, emitted its lifecycle event,
// and was byte-INELIGIBLE for the rest of the process's life.
//
// Two arms, and the pair is the point:
//
//  1. a REFLECTED field-set check, so a NEW descriptor field cannot land
//     without either being carried or being named deliberate — the guard the
//     original drop would have tripped;
//  2. a per-field VALUE check over a fully-populated descriptor, so "listed as
//     carried" cannot drift from "actually carried".
//
// An arm-1-only test would pass against a rebuild that listed the field and
// assigned the zero value; an arm-2-only test would pass forever while a
// later field is added and dropped.

// reattachCarriedFields maps each descriptor field to the assertion that it
// reached the rebuilt driver config. The keys are checked against the
// descriptor's REFLECTED field set below, so this table cannot silently go
// stale.
var reattachCarriedFields = map[string]func(t *testing.T, got config.MCPServerConfig){
	"Name": func(t *testing.T, got config.MCPServerConfig) {
		if got.Name != "parity-conn" {
			t.Errorf("Name = %q, want %q", got.Name, "parity-conn")
		}
	},
	"Transport": func(t *testing.T, got config.MCPServerConfig) {
		if want := transportModeForAdd(agentcfg.MCPTransportHTTP); got.TransportMode != want {
			t.Errorf("TransportMode = %q, want %q", got.TransportMode, want)
		}
	},
	"Command": func(t *testing.T, got config.MCPServerConfig) {
		if len(got.Command) != 2 || got.Command[0] != "parity-bin" || got.Command[1] != "--flag" {
			t.Errorf("Command = %v, want [parity-bin --flag]", got.Command)
		}
	},
	"URL": func(t *testing.T, got config.MCPServerConfig) {
		if got.URL != "https://parity.example/mcp" {
			t.Errorf("URL = %q, want the declared endpoint", got.URL)
		}
	},
	"OAuthProvider": func(t *testing.T, got config.MCPServerConfig) {
		if got.OAuthProvider != "parity-provider" {
			t.Errorf("OAuthProvider = %q, want %q", got.OAuthProvider, "parity-provider")
		}
	},
	"MetaAnnotations": func(t *testing.T, got config.MCPServerConfig) {
		if got.MetaAnnotations["harbor.tier"] != "parity" {
			t.Errorf("MetaAnnotations = %v, want harbor.tier=parity", got.MetaAnnotations)
		}
	},
	"OAuthDiscoveryAllowedOrigins": func(t *testing.T, got config.MCPServerConfig) {
		if len(got.OAuthDiscoveryAllowedOrigins) != 1 ||
			got.OAuthDiscoveryAllowedOrigins[0] != "https://as.example" {
			t.Errorf("OAuthDiscoveryAllowedOrigins = %v, want [https://as.example]",
				got.OAuthDiscoveryAllowedOrigins)
		}
	},
	"Injection": func(t *testing.T, got config.MCPServerConfig) {
		if got.Injection == nil {
			t.Fatal("Injection was dropped: a persisted per-user credential mapping " +
				"that does not come back leaves the connection dialling unauthenticated")
		}
		if got.Injection.Provider != "parity-broker" || got.Injection.Form != "header" {
			t.Errorf("Injection = %+v, want the declared broker + form", got.Injection)
		}
	},
	"ArtifactByteEligible": func(t *testing.T, got config.MCPServerConfig) {
		if !got.ArtifactByteEligible {
			t.Fatal("ArtifactByteEligible was dropped: the re-attached connection is " +
				"byte-INELIGIBLE, so the artifact id the model authors is handed to the " +
				"remote server as a literal string on every call after a restart")
		}
	},
	"ArtifactParams": func(t *testing.T, got config.MCPServerConfig) {
		if got.ArtifactParams == nil {
			t.Fatal("ArtifactParams was dropped: egress substitution has nothing to map, " +
				"so a declared byte parameter silently receives the id string")
		}
		if params := got.ArtifactParams["ingest"]; len(params) != 1 || params[0] != "doc" {
			t.Errorf("ArtifactParams[ingest] = %v, want [doc]", got.ArtifactParams["ingest"])
		}
	},
}

// reattachDeliberatelyAbsent names descriptor fields the re-attach must NOT
// carry, with the reason. Empty today — `Headers` is the one deliberate
// absence and it is not a descriptor field at all (it is never persisted), so
// there is nothing on the descriptor to exempt. The map exists so a future
// deliberate omission is recorded here rather than by deleting an arm above.
var reattachDeliberatelyAbsent = map[string]string{}

// fullyPopulatedDescriptor sets EVERY field to a distinctive non-zero value.
// A zero-valued field would make its arm pass against a rebuild that dropped
// it, which is the exact vacuity this file exists to prevent.
func fullyPopulatedDescriptor() agentcfg.MCPConnectionDescriptor {
	return agentcfg.MCPConnectionDescriptor{
		Name:                         "parity-conn",
		Transport:                    agentcfg.MCPTransportHTTP,
		Command:                      []string{"parity-bin", "--flag"},
		URL:                          "https://parity.example/mcp",
		OAuthProvider:                "parity-provider",
		MetaAnnotations:              map[string]string{"harbor.tier": "parity"},
		OAuthDiscoveryAllowedOrigins: []string{"https://as.example"},
		Injection: &agentcfg.MCPCredentialInjectionDescriptor{
			Provider: "parity-broker",
			Form:     "header",
			Header:   "X-Parity-Cred",
		},
		ArtifactByteEligible: true,
		ArtifactParams:       map[string][]string{"ingest": {"doc"}},
	}
}

// TestReattach_CarriesEveryDeclaredDescriptorField is arm 1 + arm 2.
func TestReattach_CarriesEveryDeclaredDescriptorField(t *testing.T) {
	t.Parallel()

	// --- arm 1: the reflected field set is fully accounted for.
	declared := reflect.TypeOf(agentcfg.MCPConnectionDescriptor{})
	var unaccounted []string
	for i := range declared.NumField() {
		name := declared.Field(i).Name
		_, carried := reattachCarriedFields[name]
		_, absent := reattachDeliberatelyAbsent[name]
		if !carried && !absent {
			unaccounted = append(unaccounted, name)
		}
	}
	if len(unaccounted) > 0 {
		sort.Strings(unaccounted)
		t.Fatalf("MCPConnectionDescriptor fields %v are neither carried by the run-start "+
			"re-attach nor recorded as deliberately absent. A field nothing carries forward "+
			"is INERT: the connection comes back online and silently lacks the surface. "+
			"Carry it in reattachServerConfig and add its arm here, or record the omission "+
			"in reattachDeliberatelyAbsent with a reason.", unaccounted)
	}
	// The reverse direction: a table arm for a field that no longer exists is a
	// stale registration, and it would hide the next drop behind a green count.
	for name := range reattachCarriedFields {
		if _, ok := declared.FieldByName(name); !ok {
			t.Errorf("reattachCarriedFields names %q, which is not a descriptor field", name)
		}
	}

	// --- arm 2: every listed field actually arrives.
	got := reattachServerConfig(fullyPopulatedDescriptor())
	for name, assert := range reattachCarriedFields {
		t.Run(name, func(t *testing.T) { assert(t, got) })
	}
}

// TestReattach_DoesNotAliasTheRevisionsMutableFields — the rebuilt config must
// own its slices and maps. Aliasing the revision's values would let a driver
// mutate the agent's stored desired state.
func TestReattach_DoesNotAliasTheRevisionsMutableFields(t *testing.T) {
	t.Parallel()
	desc := fullyPopulatedDescriptor()
	got := reattachServerConfig(desc)

	got.Command[0] = "mutated"
	got.MetaAnnotations["harbor.tier"] = "mutated"
	got.OAuthDiscoveryAllowedOrigins[0] = "https://mutated.example"
	got.ArtifactParams["ingest"][0] = "mutated"

	if desc.Command[0] != "parity-bin" {
		t.Error("Command aliases the descriptor's slice")
	}
	if desc.MetaAnnotations["harbor.tier"] != "parity" {
		t.Error("MetaAnnotations aliases the descriptor's map")
	}
	if desc.OAuthDiscoveryAllowedOrigins[0] != "https://as.example" {
		t.Error("OAuthDiscoveryAllowedOrigins aliases the descriptor's slice")
	}
	if desc.ArtifactParams["ingest"][0] != "doc" {
		t.Error("ArtifactParams aliases the descriptor's map value")
	}
}

// TestReattach_ThreadsTheDeploymentEgressCeiling — the ceiling is attacher
// state, not descriptor state, so it rides AttachDeps rather than the config.
// Omitting it silently restored the 8 MiB default across a restart for a
// deployment that deliberately lowered the bound.
func TestReattach_ThreadsTheDeploymentEgressCeiling(t *testing.T) {
	t.Parallel()
	const lowered = 4096
	a := NewMCPConnectionAttacher(nil, nil, nil, nil, identity.Identity{}, nil, nil, nil,
		WithArtifactEgressMaxBytes(lowered))
	if a.artifactEgressMaxBytes != lowered {
		t.Fatalf("attacher ceiling = %d, want %d", a.artifactEgressMaxBytes, lowered)
	}
	var closers []func(context.Context) error
	if got := a.reattachAttachDeps(reOwner(), &closers).ArtifactEgressMaxBytes; got != lowered {
		t.Fatalf("the run-start re-attach passes ArtifactEgressMaxBytes=%d, want the "+
			"deployment's lowered %d — a dropped ceiling silently reverts to the 8 MiB "+
			"default across every restart", got, lowered)
	}
}
