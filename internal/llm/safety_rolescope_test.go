package llm

import (
	"strings"
	"testing"
)

// White-box tests for the role-scoped heavy-content byte check
// (findContextLeak, refined by D-241). These call findContextLeak
// directly so the byte-check scope can be asserted in isolation from
// the token-window guard.

const rsThreshold = 32 * 1024

func rsHeavy() string { return strings.Repeat("X", rsThreshold+1) }

func toolCallID(s string) *string { return &s }

// TestFindContextLeak_RoleScope_ToolTextLeaks — a RoleTool message
// carrying heavy text (the offloadable class) trips the byte check.
func TestFindContextLeak_RoleScope_ToolTextLeaks(t *testing.T) {
	heavy := rsHeavy()
	req := CompleteRequest{
		Model: "m",
		Messages: []ChatMessage{
			{Role: RoleTool, ToolCallID: toolCallID("c1"), Content: Content{Text: &heavy}},
		},
	}
	site, size, ok := findContextLeak(req, rsThreshold)
	if !ok {
		t.Fatalf("RoleTool heavy text not flagged as a leak")
	}
	if size != len(heavy) {
		t.Errorf("size=%d, want %d", size, len(heavy))
	}
	if !strings.Contains(site, "Messages[0].Content.Text") {
		t.Errorf("site=%q does not name the tool-text leak", site)
	}
}

// TestFindContextLeak_RoleScope_ToolPartTextLeaks — heavy PartText on a
// RoleTool message is offloadable and trips the check.
func TestFindContextLeak_RoleScope_ToolPartTextLeaks(t *testing.T) {
	heavy := rsHeavy()
	req := CompleteRequest{
		Model: "m",
		Messages: []ChatMessage{
			{
				Role:       RoleTool,
				ToolCallID: toolCallID("c1"),
				Content:    Content{Parts: []ContentPart{{Type: PartText, Text: heavy}}},
			},
		},
	}
	site, _, ok := findContextLeak(req, rsThreshold)
	if !ok {
		t.Fatalf("RoleTool heavy PartText not flagged as a leak")
	}
	if !strings.Contains(site, "Parts[0].Text") {
		t.Errorf("site=%q does not name the tool-part-text leak", site)
	}
}

// TestFindContextLeak_RoleScope_BinaryPartLeaksAnyRole — a binary
// DataURL part above threshold trips the check regardless of role
// (binary parts are always offloadable to an ArtifactRef).
func TestFindContextLeak_RoleScope_BinaryPartLeaksAnyRole(t *testing.T) {
	heavy := rsHeavy()
	for _, role := range []Role{RoleSystem, RoleUser, RoleAssistant, RoleTool} {
		t.Run(string(role), func(t *testing.T) {
			req := CompleteRequest{
				Model: "m",
				Messages: []ChatMessage{
					{
						Role: role,
						Content: Content{Parts: []ContentPart{
							{Type: PartImage, Image: &ImagePart{DataURL: heavy, MIME: "image/png"}},
						}},
					},
				},
			}
			site, _, ok := findContextLeak(req, rsThreshold)
			if !ok {
				t.Fatalf("binary DataURL part on role %q not flagged as a leak", role)
			}
			if !strings.Contains(site, "Image.DataURL") {
				t.Errorf("site=%q does not name the binary-part leak", site)
			}
		})
	}
}

// TestFindContextLeak_RoleScope_ConversationTextExempt — heavy
// conversation text (Content.Text or PartText) on System / User /
// Assistant messages is EXEMPT from the byte check (governed by the
// token-window guard instead).
func TestFindContextLeak_RoleScope_ConversationTextExempt(t *testing.T) {
	heavy := rsHeavy()
	for _, role := range []Role{RoleSystem, RoleUser, RoleAssistant} {
		t.Run(string(role)+"_content_text", func(t *testing.T) {
			req := CompleteRequest{
				Model:    "m",
				Messages: []ChatMessage{{Role: role, Content: Content{Text: &heavy}}},
			}
			if site, _, ok := findContextLeak(req, rsThreshold); ok {
				t.Fatalf("conversation Content.Text on role %q flagged as a leak at %q; D-241 exempts it", role, site)
			}
		})
		t.Run(string(role)+"_part_text", func(t *testing.T) {
			req := CompleteRequest{
				Model: "m",
				Messages: []ChatMessage{{
					Role:    role,
					Content: Content{Parts: []ContentPart{{Type: PartText, Text: heavy}}},
				}},
			}
			if site, _, ok := findContextLeak(req, rsThreshold); ok {
				t.Fatalf("conversation PartText on role %q flagged as a leak at %q; D-241 exempts it", role, site)
			}
		})
	}
}
