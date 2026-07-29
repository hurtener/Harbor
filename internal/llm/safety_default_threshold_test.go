// safety_default_threshold_test.go — phase 213 (D-358). Every existing
// findContextLeak test passes an EXPLICIT threshold, so none of them
// would have noticed the LLM-context arm moving. These arms pin the
// three walked leak classes against the RESOLVED DEFAULT
// (llm.DefaultHeavyOutputThreshold = config.DefaultHeavyOutputThresholdBytes),
// so the guard and the constant can never drift apart silently.
//
// White-box (package llm) so findContextLeak is callable directly, the
// same shape safety_rolescope_test.go uses.

package llm

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

// TestFindContextLeak_DefaultThreshold_IsTheLLMContextArm pins the
// snapshot default to the LLM-context constant and AWAY from the
// Console inline-payload pin.
func TestFindContextLeak_DefaultThreshold_IsTheLLMContextArm(t *testing.T) {
	if DefaultHeavyOutputThreshold != config.DefaultHeavyOutputThresholdBytes {
		t.Fatalf("DefaultHeavyOutputThreshold = %d, want the LLM-context arm %d",
			DefaultHeavyOutputThreshold, config.DefaultHeavyOutputThresholdBytes)
	}
	if DefaultHeavyOutputThreshold != 128*1024 {
		t.Fatalf("DefaultHeavyOutputThreshold = %d, want 131072", DefaultHeavyOutputThreshold)
	}
	if DefaultHeavyOutputThreshold == config.DefaultConsoleInlinePayloadBytes {
		t.Fatal("the LLM-edge guard must not read the pinned Console inline-payload bound")
	}
}

// TestFindContextLeak_DefaultThreshold_AllThreeClasses walks each
// offloadable class just BELOW and exactly AT the resolved default.
// Below: silent (this is the band the raise inlines). At: a loud leak
// naming the site and the size.
func TestFindContextLeak_DefaultThreshold_AllThreeClasses(t *testing.T) {
	const th = DefaultHeavyOutputThreshold
	callID := "call_default_th"

	// A DataURL's leak size is the encoded URL length, so the payload
	// is sized from the decoded side and the assertion reads the URL.
	dataURL := func(decoded int) string {
		return "data:image/png;base64," +
			base64.StdEncoding.EncodeToString([]byte(strings.Repeat("Z", decoded)))
	}

	for _, tc := range []struct {
		name     string
		build    func(size int) CompleteRequest
		wantSite string
	}{
		{
			name: "RoleTool Content.Text",
			build: func(size int) CompleteRequest {
				body := strings.Repeat("X", size)
				return CompleteRequest{Model: "m", Messages: []ChatMessage{
					{Role: RoleAssistant, ToolCalls: []ToolCallStructured{{ID: callID, Name: "probe"}}},
					{Role: RoleTool, ToolCallID: &callID, Content: Content{Text: &body}},
				}}
			},
			wantSite: "Messages[1].Content.Text",
		},
		{
			name: "RoleTool PartText",
			build: func(size int) CompleteRequest {
				return CompleteRequest{Model: "m", Messages: []ChatMessage{
					{Role: RoleAssistant, ToolCalls: []ToolCallStructured{{ID: callID, Name: "probe"}}},
					{Role: RoleTool, ToolCallID: &callID, Content: Content{
						Parts: []ContentPart{{Type: PartText, Text: strings.Repeat("X", size)}},
					}},
				}}
			},
			wantSite: "Parts[0].Text",
		},
		{
			name: "ToolCalls[].Args",
			build: func(size int) CompleteRequest {
				// Valid JSON whose encoded length is exactly `size`.
				pad := size - len(`{"q":""}`)
				args := `{"q":"` + strings.Repeat("a", pad) + `"}`
				return CompleteRequest{Model: "m", Messages: []ChatMessage{
					{Role: RoleAssistant, ToolCalls: []ToolCallStructured{
						{ID: callID, Name: "probe", Args: json.RawMessage(args)},
					}},
				}}
			},
			wantSite: "ToolCalls[0].Args",
		},
	} {
		t.Run(tc.name+"/below the default is silent", func(t *testing.T) {
			if site, sz, ok := findContextLeak(tc.build(th-1), th); ok {
				t.Fatalf("a %d-byte payload leaked at site=%q size=%d; the band below %d now inlines",
					th-1, site, sz, th)
			}
		})
		t.Run(tc.name+"/at the default is a loud leak", func(t *testing.T) {
			site, sz, ok := findContextLeak(tc.build(th), th)
			if !ok {
				t.Fatalf("a %d-byte payload did NOT trip the guard at threshold %d", th, th)
			}
			if !strings.Contains(site, tc.wantSite) {
				t.Errorf("site = %q, want it to name %q", site, tc.wantSite)
			}
			if sz < th {
				t.Errorf("reported size = %d, want >= %d", sz, th)
			}
		})
	}

	// Binary DataURL parts leak at ANY role — the class the
	// auto-materialization pass is the exact counterpart for.
	t.Run("binary DataURL/below the default is silent", func(t *testing.T) {
		req := CompleteRequest{Model: "m", Messages: []ChatMessage{{
			Role: RoleUser,
			Content: Content{Parts: []ContentPart{{
				Type:  PartImage,
				Image: &ImagePart{DataURL: dataURL(64 * 1024), MIME: "image/png"},
			}}},
		}}}
		if site, sz, ok := findContextLeak(req, th); ok {
			t.Fatalf("a 64 KiB DataURL leaked at site=%q size=%d; it is inside the inlined band", site, sz)
		}
	})
	t.Run("binary DataURL/at the default is a loud leak", func(t *testing.T) {
		req := CompleteRequest{Model: "m", Messages: []ChatMessage{{
			Role: RoleUser,
			Content: Content{Parts: []ContentPart{{
				Type:  PartImage,
				Image: &ImagePart{DataURL: dataURL(th), MIME: "image/png"},
			}}},
		}}}
		site, sz, ok := findContextLeak(req, th)
		if !ok {
			t.Fatal("an over-threshold DataURL did NOT trip the guard")
		}
		if !strings.Contains(site, "Parts[0]") {
			t.Errorf("site = %q, want it to name the part", site)
		}
		if sz < th {
			t.Errorf("reported size = %d, want >= %d", sz, th)
		}
	})
}
