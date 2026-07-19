package app

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/tui/projection"
	"github.com/hurtener/Harbor/internal/tui/ui"
)

// TestContextLabel_SurfacesCacheQualifier pins that the composer context
// readout appends a "(N cached)" qualifier when the session's cache-read
// count is non-zero, and omits it entirely otherwise (never "0 cached").
func TestContextLabel_SurfacesCacheQualifier(t *testing.T) {
	theme := ui.NewTheme(ui.ModeDark, ui.ProfileMono)

	withCache := projection.Projection{Usage: projection.Usage{
		TotalTokens: 20000, PromptTokens: 18000, ContextWindow: 128000, CacheReadTokens: 8000,
	}}
	m := NewModel(100, 30, theme, true, withCache)
	label := m.contextLabel()
	if !strings.Contains(label, "cached") {
		t.Errorf("contextLabel = %q, want a cache qualifier", label)
	}
	if !strings.HasPrefix(label, "Context ") {
		t.Errorf("contextLabel = %q, want the canonical Context prefix", label)
	}

	noCache := projection.Projection{Usage: projection.Usage{
		TotalTokens: 20000, PromptTokens: 18000, ContextWindow: 128000,
	}}
	m2 := NewModel(100, 30, theme, true, noCache)
	if got := m2.contextLabel(); strings.Contains(got, "cached") {
		t.Errorf("contextLabel = %q, must not render a cache qualifier at zero", got)
	}
}

// TestTurnStatus_SurfacesCacheQualifier pins that the per-turn status line
// appends a cache qualifier to the token count when the run's cache-read
// total is non-zero.
func TestTurnStatus_SurfacesCacheQualifier(t *testing.T) {
	m, controller, _ := operationalModel(t)
	id := controller.Identity()

	p := projection.Projection{
		Identity: id,
		Blocks: []projection.Block{
			{ID: "t1", Kind: "task", RunID: "run-x", Status: "completed", DurationMS: 1200},
		},
		RunUsage: map[string]projection.Usage{
			"run-x": {TotalTokens: 12000, PromptTokens: 10000, USD: 0.05, CacheReadTokens: 8000},
		},
	}
	m.transcript = m.transcript.Replace(p)

	status := m.turnStatus()
	if !strings.Contains(status, "cached") {
		t.Errorf("turnStatus = %q, want a cache qualifier when CacheReadTokens > 0", status)
	}

	// Zero cache reads: no qualifier.
	pZero := projection.Projection{
		Identity: id,
		Blocks: []projection.Block{
			{ID: "t1", Kind: "task", RunID: "run-y", Status: "completed", DurationMS: 1200},
		},
		RunUsage: map[string]projection.Usage{
			"run-y": {TotalTokens: 12000, PromptTokens: 10000, USD: 0.05},
		},
	}
	m.transcript = m.transcript.Replace(pZero)
	if got := m.turnStatus(); strings.Contains(got, "cached") {
		t.Errorf("turnStatus = %q, must not render a cache qualifier at zero", got)
	}
}
