package config_test

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

// TestReservedMCPMetaPathToken_WholeKeyAndPerSegment pins the guard rule that
// this phase's whole design hangs on: the reserved check is WHOLE-KEY **AND**
// PER-SEGMENT, never per-segment alone.
//
// Both arms are load-bearing and neither subsumes the other:
//
//   - Per-segment-ONLY would ADMIT `io.modelcontextprotocol/ui`, which is
//     refused today. Splitting it on `.` yields ["io",
//     "modelcontextprotocol/ui"] and NEITHER segment carries the
//     `io.modelcontextprotocol/` prefix, so the spec-namespace arm of
//     IsReservedMCPMetaKey never fires. Dropping the whole-key arm would
//     therefore LOOSEN a security guard while looking like a tightening.
//   - Whole-key-ONLY (today's behaviour) ADMITS `tenant.foo`, which under path
//     nesting writes into the runtime-owned `tenant` node.
func TestReservedMCPMetaPathToken_WholeKeyAndPerSegment(t *testing.T) {
	t.Run("whole-key arm: the spec namespace a per-segment-only check would lose", func(t *testing.T) {
		for _, k := range []string{
			"io.modelcontextprotocol/ui",
			"io.modelcontextprotocol/",
			"io.modelcontextprotocol/anything",
		} {
			tok, reserved := config.ReservedMCPMetaPathToken(k)
			if !reserved {
				t.Errorf("ReservedMCPMetaPathToken(%q) = false — a per-segment-ONLY guard would admit a spec-reserved key that is refused today", k)
			}
			if tok != k {
				t.Errorf("ReservedMCPMetaPathToken(%q) token = %q, want the whole key", k, tok)
			}
		}
	})

	t.Run("per-segment arm: reserved token at every position", func(t *testing.T) {
		cases := map[string]string{
			"tenant.foo":            "tenant", // first
			"vendor.user.id":        "user",   // middle
			"vendor.nested.session": "session",
			"a.b.agent_id":          "agent_id", // last
			"traceparent.x":         "traceparent",
			"x.tracestate.y":        "tracestate",
		}
		for k, wantTok := range cases {
			tok, reserved := config.ReservedMCPMetaPathToken(k)
			if !reserved {
				t.Errorf("ReservedMCPMetaPathToken(%q) = false, want true (reserved segment %q)", k, wantTok)
				continue
			}
			if tok != wantTok {
				t.Errorf("ReservedMCPMetaPathToken(%q) token = %q, want %q", k, tok, wantTok)
			}
		}
	})

	t.Run("strict superset: everything IsReservedMCPMetaKey refuses is still refused", func(t *testing.T) {
		for _, k := range []string{
			"tenant", "user", "session", "agent_id", "traceparent", "tracestate",
			"io.modelcontextprotocol/ui",
		} {
			if !config.IsReservedMCPMetaKey(k) {
				t.Fatalf("test premise broken: IsReservedMCPMetaKey(%q) = false", k)
			}
			if _, reserved := config.ReservedMCPMetaPathToken(k); !reserved {
				t.Errorf("ReservedMCPMetaPathToken(%q) = false but IsReservedMCPMetaKey = true — the path guard must be a strict SUPERSET", k)
			}
		}
	})

	t.Run("non-reserved paths stay admitted", func(t *testing.T) {
		for _, k := range []string{
			"deployment", "fleet", "vendor.tag", "vendor.account_id",
			"a.b.c.d", "io.modelcontextprotocol", "agent", "traceparent2",
			"Tenant.foo", // case-sensitive, like IsReservedMCPMetaKey
		} {
			if tok, reserved := config.ReservedMCPMetaPathToken(k); reserved {
				t.Errorf("ReservedMCPMetaPathToken(%q) = true (token %q), want false", k, tok)
			}
		}
	})
}

// TestValidateMCPMetaAnnotationKey_Table covers the one shared per-key rule all
// four validation doors delegate to.
func TestValidateMCPMetaAnnotationKey_Table(t *testing.T) {
	deep := strings.Repeat("a.", config.MaxMCPMetaKeyDepth) + "leaf" // MaxMCPMetaKeyDepth+1 segments
	atCap := strings.Repeat("a.", config.MaxMCPMetaKeyDepth-1) + "leaf"

	bad := map[string]string{
		"":                           "empty",
		"   ":                        "empty",
		"a..b":                       "empty path segment",
		"a.":                         "empty path segment",
		"tenant":                     "reserved",
		"tenant.foo":                 "reserved",
		"vendor.session":             "reserved",
		"io.modelcontextprotocol/ui": "reserved",
		deep:                         "exceeding the cap",
	}
	for k, wantText := range bad {
		err := config.ValidateMCPMetaAnnotationKey(k)
		if err == nil {
			t.Errorf("ValidateMCPMetaAnnotationKey(%q) = nil, want an error containing %q", k, wantText)
			continue
		}
		if !strings.Contains(err.Error(), wantText) {
			t.Errorf("ValidateMCPMetaAnnotationKey(%q) = %q, want text %q", k, err.Error(), wantText)
		}
	}

	for _, k := range []string{"deployment", "vendor.tag", "vendor.account_id", atCap} {
		if err := config.ValidateMCPMetaAnnotationKey(k); err != nil {
			t.Errorf("ValidateMCPMetaAnnotationKey(%q) = %v, want nil", k, err)
		}
	}
}

// TestValidateMCPMetaPathCollisions_Table pins the rule that converts the
// silent scalar/map overwrite into a loud refusal — and, as a consequence,
// makes the annotation merge order-independent.
func TestValidateMCPMetaPathCollisions_Table(t *testing.T) {
	cases := []struct {
		name        string
		annotations []string
		injection   string
		wantErr     bool
		wantKeys    []string
	}{
		{
			name:        "annotation prefixes annotation",
			annotations: []string{"vendor", "vendor.id"},
			wantErr:     true,
			wantKeys:    []string{"vendor", "vendor.id"},
		},
		{
			name:        "flat annotation collides with the injection meta path",
			annotations: []string{"vendor"},
			injection:   "vendor.api_key",
			wantErr:     true,
			wantKeys:    []string{"vendor", "vendor.api_key"},
		},
		{
			name:        "annotation equal to the injection meta path",
			annotations: []string{"vendor.api_key"},
			injection:   "vendor.api_key",
			wantErr:     true,
			wantKeys:    []string{"vendor.api_key"},
		},
		{
			name:        "deep prefix relationship",
			annotations: []string{"a.b", "a.b.c.d"},
			wantErr:     true,
			wantKeys:    []string{"a.b", "a.b.c.d"},
		},
		{
			name:        "shared namespace, disjoint leaves is fine",
			annotations: []string{"vendor.tag", "vendor.region", "fleet"},
			wantErr:     false,
		},
		{
			name:        "annotation shares a namespace with the injection path",
			annotations: []string{"vendor.account_id"},
			injection:   "vendor.api_key",
			wantErr:     false,
		},
		{
			name:        "sibling names that merely share a prefix STRING are not a path collision",
			annotations: []string{"vendor", "vendorx"},
			wantErr:     false,
		},
		{
			name:        "single declaration cannot collide",
			annotations: []string{"vendor"},
			wantErr:     false,
		},
		{
			name:        "empty keys are ignored (the per-key rule refuses them)",
			annotations: []string{"", "   ", "vendor"},
			wantErr:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := config.ValidateMCPMetaPathCollisions(tc.annotations, tc.injection)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateMCPMetaPathCollisions(%v, %q) = nil, want a collision error", tc.annotations, tc.injection)
			}
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("ValidateMCPMetaPathCollisions(%v, %q) = %v, want nil", tc.annotations, tc.injection, err)
				}
				return
			}
			for _, want := range tc.wantKeys {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("collision error %q does not name the offending key %q", err.Error(), want)
				}
			}
		})
	}
}

// TestValidateMCPMetaPathCollisions_Deterministic asserts the error message is
// stable across runs despite the caller handing keys in map-iteration order —
// otherwise a boot failure would name a different pair on every restart.
func TestValidateMCPMetaPathCollisions_Deterministic(t *testing.T) {
	forward := config.ValidateMCPMetaPathCollisions([]string{"vendor", "vendor.id", "alpha", "zeta"}, "")
	reverse := config.ValidateMCPMetaPathCollisions([]string{"zeta", "alpha", "vendor.id", "vendor"}, "")
	if forward == nil || reverse == nil {
		t.Fatalf("expected a collision both ways, got forward=%v reverse=%v", forward, reverse)
	}
	if forward.Error() != reverse.Error() {
		t.Errorf("collision error is order-dependent:\n forward=%q\n reverse=%q", forward.Error(), reverse.Error())
	}
}

// TestValidate_MCPMetaAnnotationPaths_BootDoor drives the rules through the
// BOOT config door (door 1) so the wiring — not just the predicate — is pinned.
func TestValidate_MCPMetaAnnotationPaths_BootDoor(t *testing.T) {
	deep := strings.Repeat("a.", config.MaxMCPMetaKeyDepth) + "leaf"

	cases := []struct {
		name     string
		server   config.MCPServerConfig
		wantText string
	}{
		{
			"reserved FIRST segment refused (newly)",
			config.MCPServerConfig{
				Name: "gcal", TransportMode: "streamable_http", URL: "https://gcal.example.test",
				MetaAnnotations: map[string]string{"tenant.foo": "x"},
			},
			"reserved",
		},
		{
			"reserved LAST segment refused",
			config.MCPServerConfig{
				Name: "gcal", TransportMode: "streamable_http", URL: "https://gcal.example.test",
				MetaAnnotations: map[string]string{"vendor.agent_id": "x"},
			},
			"reserved",
		},
		{
			"over-deep annotation path refused",
			config.MCPServerConfig{
				Name: "gcal", TransportMode: "streamable_http", URL: "https://gcal.example.test",
				MetaAnnotations: map[string]string{deep: "x"},
			},
			"exceeding the cap",
		},
		{
			"empty path segment refused",
			config.MCPServerConfig{
				Name: "gcal", TransportMode: "streamable_http", URL: "https://gcal.example.test",
				MetaAnnotations: map[string]string{"a..b": "x"},
			},
			"empty path segment",
		},
		{
			"annotation/annotation collision refused",
			config.MCPServerConfig{
				Name: "gcal", TransportMode: "streamable_http", URL: "https://gcal.example.test",
				MetaAnnotations: map[string]string{"vendor": "x", "vendor.id": "y"},
			},
			"collide",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mustLoadValid(t)
			cfg.Tools.MCPServers = []config.MCPServerConfig{tc.server}
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), "tools.mcp_servers[0].meta_annotations") {
				t.Errorf("err=%q missing the meta_annotations field path", err.Error())
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("err=%q missing text %q", err.Error(), tc.wantText)
			}
		})
	}
}

// TestValidate_MCPMetaAnnotationPaths_BootDoorAccepts proves a dotted key stays
// LEGAL (it is a supported shape on the shipped surface — only its merge
// meaning changes) and that a shared namespace across annotation + injection is
// exactly the arrangement this phase exists to enable.
func TestValidate_MCPMetaAnnotationPaths_BootDoorAccepts(t *testing.T) {
	cfg := mustLoadValid(t)
	withOAuthProvider(cfg)
	cfg.Tools.MCPServers = []config.MCPServerConfig{{
		Name: "gcal", TransportMode: "streamable_http", URL: "https://gcal.example.test",
		MetaAnnotations: map[string]string{
			"deployment":        "prod",
			"vendor.tag":        "blue",
			"vendor.account_id": "acct-42",
		},
		Injection: &config.MCPCredentialInjectionConfig{
			Provider: "m365-broker",
			Form:     config.MCPInjectionFormMeta,
			MetaKey:  "vendor.api_key",
		},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a nested annotation sharing a namespace with the injection path was rejected: %v", err)
	}
}

// TestValidate_MCPMetaAnnotationPaths_BootDoorRefusesInjectionCollision pins the
// case that today silently discards the operator's annotation at merge time: a
// FLAT `vendor` annotation plus `injection.meta_key: vendor.api_key`.
func TestValidate_MCPMetaAnnotationPaths_BootDoorRefusesInjectionCollision(t *testing.T) {
	cfg := mustLoadValid(t)
	withOAuthProvider(cfg)
	cfg.Tools.MCPServers = []config.MCPServerConfig{{
		Name: "gcal", TransportMode: "streamable_http", URL: "https://gcal.example.test",
		MetaAnnotations: map[string]string{"vendor": "flat"},
		Injection: &config.MCPCredentialInjectionConfig{
			Provider: "m365-broker",
			Form:     config.MCPInjectionFormMeta,
			MetaKey:  "vendor.api_key",
		},
	}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted a flat annotation colliding with the injection _meta path")
	}
	for _, want := range []string{"collide", "vendor", "vendor.api_key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err=%q missing %q", err.Error(), want)
		}
	}
}

// TestValidate_InjectionMetaKeyDepthCap_BootDoor closes the wire-only
// asymmetry: the depth cap lived ONLY at the wire door, so a boot-declared
// over-deep `meta_key` was accepted where the identical wire-declared one was
// refused. Both now consult config.MaxMCPMetaKeyDepth.
func TestValidate_InjectionMetaKeyDepthCap_BootDoor(t *testing.T) {
	cfg := mustLoadValid(t)
	withOAuthProvider(cfg)
	deep := strings.Repeat("x.", config.MaxMCPMetaKeyDepth) + "token" // MaxMCPMetaKeyDepth+1 segments
	cfg.Tools.MCPServers = []config.MCPServerConfig{{
		Name: "gcal", TransportMode: "streamable_http", URL: "https://gcal.example.test",
		Injection: &config.MCPCredentialInjectionConfig{
			Provider: "m365-broker",
			Form:     config.MCPInjectionFormMeta,
			MetaKey:  deep,
		},
	}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("boot config accepted an over-deep injection meta_key the wire door refuses")
	}
	if !strings.Contains(err.Error(), "exceeding the cap") {
		t.Errorf("err=%q missing the depth-cap text", err.Error())
	}
}
