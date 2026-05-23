package harborinit_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/cmd/harbor/init"
	"github.com/hurtener/Harbor/internal/config"
)

func TestInit_DefaultTemplateRenders(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	res, err := harborinit.Init(harborinit.Options{Name: "alpha", TargetDir: target})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	want := []string{"AGENTS.md", "CLAUDE.md", "README.md", "harbor.yaml"}
	if len(res.Files) != len(want) {
		t.Fatalf("Files = %v, want %d entries", res.Files, len(want))
	}
	for i, base := range want {
		got := filepath.Base(res.Files[i])
		if got != base {
			t.Errorf("Files[%d] basename = %q, want %q (full = %s)", i, got, base, res.Files[i])
		}
	}
}

func TestInit_NameDefaultsFromTargetBase(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "myagent")
	res, err := harborinit.Init(harborinit.Options{TargetDir: target})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if res.Name != "myagent" {
		t.Fatalf("Name = %q, want myagent (derived from target basename)", res.Name)
	}
	yamlBytes, err := os.ReadFile(filepath.Join(target, "harbor.yaml"))
	if err != nil {
		t.Fatalf("ReadFile harbor.yaml: %v", err)
	}
	if !strings.Contains(string(yamlBytes), "Harbor agent — myagent") {
		t.Fatalf("yaml header does not reference name=myagent; got:\n%s", string(yamlBytes))
	}
}

func TestInit_BadBasenameFallsBackToAgent(t *testing.T) {
	t.Parallel()
	// "1234-Bad Name" — uppercase + space — fails validateName, so the
	// fallback should be the literal "agent".
	target := filepath.Join(t.TempDir(), "1234-Bad Name")
	res, err := harborinit.Init(harborinit.Options{TargetDir: target})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if res.Name != "agent" {
		t.Fatalf("Name = %q, want agent (basename fails validateName so we fall back)", res.Name)
	}
}

func TestInit_ExplicitInvalidNameFailsLoudly(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	_, err := harborinit.Init(harborinit.Options{Name: "Bad Name", TargetDir: target})
	if !errors.Is(err, harborinit.ErrInvalidName) {
		t.Fatalf("want ErrInvalidName, got %v", err)
	}
}

func TestInit_RefusesToOverwriteExisting(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	pre := filepath.Join(target, "AGENTS.md")
	if err := os.WriteFile(pre, []byte("pre-existing"), 0o644); err != nil {
		t.Fatalf("WriteFile pre: %v", err)
	}
	_, err := harborinit.Init(harborinit.Options{Name: "alpha", TargetDir: target})
	if !errors.Is(err, harborinit.ErrFileExists) {
		t.Fatalf("want ErrFileExists, got %v", err)
	}
	// And the other three files MUST NOT have been written (no
	// partial init).
	for _, base := range []string{"harbor.yaml", "CLAUDE.md", "README.md"} {
		p := filepath.Join(target, base)
		if _, err := os.Stat(p); err == nil {
			t.Errorf("init wrote %s despite the collision — partial state must be avoided", p)
		}
	}
}

func TestInit_UnknownTemplateFailsLoudly(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	_, err := harborinit.Init(harborinit.Options{Name: "alpha", Template: "no-such-template", TargetDir: target})
	if !errors.Is(err, harborinit.ErrUnknownTemplate) {
		t.Fatalf("want ErrUnknownTemplate, got %v", err)
	}
}

func TestInit_YAMLPassesValidatorAfterUncommentingOpenRouter(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	if _, err := harborinit.Init(harborinit.Options{Name: "alpha", TargetDir: target}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(target, "harbor.yaml"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	uncommented := uncommentOpenRouterBlock(string(raw))
	// Sanity: at least one of the LLM provider example markers must
	// remain commented out (the operator only uncommented one).
	for _, marker := range []string{"# --- Example 2:", "# --- Example 3:", "# --- Example 4:"} {
		if !strings.Contains(uncommented, marker) {
			t.Fatalf("expected marker %q to remain commented in the operator-edited yaml; got:\n%s",
				marker, uncommented)
		}
	}
	cfg, err := config.LoadFromBytes(context.Background(), []byte(uncommented))
	if err != nil {
		t.Fatalf("LoadFromBytes after uncomment: %v\n--- yaml ---\n%s", err, uncommented)
	}
	if cfg.LLM.Provider != "openrouter" {
		t.Fatalf("cfg.LLM.Provider = %q, want openrouter", cfg.LLM.Provider)
	}
	if cfg.LLM.Model == "" {
		t.Fatalf("cfg.LLM.Model is empty")
	}
}

// uncommentOpenRouterBlock simulates an operator uncommenting the
// first LLM-provider example block in the init-dropped yaml.
//
// Implementation: between the "Example 1: OpenRouter" line and the
// "Example 2: Anthropic" line, drop the leading "  # " prefix from
// every line that has it (keeping the indent so the yaml stays
// valid). The provider/model/key/timeout/model_profiles all live in
// that range.
func uncommentOpenRouterBlock(raw string) string {
	const startMarker = "--- Example 1: OpenRouter"
	const endMarker = "--- Example 2: Anthropic"
	lines := strings.Split(raw, "\n")
	startIdx, endIdx := -1, -1
	for i, line := range lines {
		if startIdx == -1 && strings.Contains(line, startMarker) {
			startIdx = i
			continue
		}
		if startIdx != -1 && strings.Contains(line, endMarker) {
			endIdx = i
			break
		}
	}
	if startIdx == -1 || endIdx == -1 {
		return raw
	}
	for i := startIdx + 1; i < endIdx; i++ {
		l := lines[i]
		switch {
		case strings.HasPrefix(l, "  # "):
			lines[i] = "  " + l[4:]
		case strings.HasPrefix(l, "  #"):
			// A bare `  #` (blank comment line) becomes a blank line.
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}
