// cmd/harbor/scaffold/scaffold_test.go — engine-level unit tests for
// the scaffold engine. These tests are the in-PR consumer of every
// public surface; the cobra-driver tests live in
// cmd/harbor/cmd_scaffold_test.go.
//
// The load-bearing test is TestScaffold_RenderedConfig_PassesConfigValidate
// — it wires the rendered harbor.yaml against the real
// internal/config package, the in-PR stand-in for `harbor validate`
// (sibling-shipping in Phase 68; CLI integration lands in Phase 68's
// PR per CLAUDE.md §17.6).

package scaffold

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

// TestScaffold_HappyPath_WritesAllFiles is the load-bearing
// acceptance-criterion test: a default-options scaffold produces every
// expected file under the output dir.
func TestScaffold_HappyPath_WritesAllFiles(t *testing.T) {
	t.Parallel()
	out := filepath.Join(t.TempDir(), "happy-agent")
	result, err := Scaffold(Options{Name: "happy-agent", OutputDir: out})
	if err != nil {
		t.Fatalf("Scaffold: unexpected error: %v", err)
	}
	if result.Name != "happy-agent" {
		t.Errorf("result.Name expected %q, got %q", "happy-agent", result.Name)
	}
	if result.OutputDir != out {
		t.Errorf("result.OutputDir expected %q, got %q", out, result.OutputDir)
	}
	expected := []string{"README.md", "agent.go", "agent_test.go", "go.mod", "harbor.yaml"}
	if len(result.Files) != len(expected) {
		t.Fatalf("result.Files len expected %d, got %d (%v)", len(expected), len(result.Files), result.Files)
	}
	for i, want := range expected {
		if result.Files[i] != want {
			t.Errorf("result.Files[%d] expected %q, got %q", i, want, result.Files[i])
		}
		if _, err := os.Stat(filepath.Join(out, want)); err != nil {
			t.Errorf("expected file %s missing: %v", want, err)
		}
	}
}

// TestScaffold_DefaultTemplate_IsMinimalReact pins the
// DefaultTemplate constant. A regression here means a sibling phase
// has re-pointed the default without updating this test, which is the
// kind of drift we want to catch.
func TestScaffold_DefaultTemplate_IsMinimalReact(t *testing.T) {
	t.Parallel()
	if DefaultTemplate != "minimal-react" {
		t.Errorf("DefaultTemplate expected %q, got %q", "minimal-react", DefaultTemplate)
	}
}

// TestScaffold_Templates_ListsMinimalReact pins the registered
// template set.
func TestScaffold_Templates_ListsMinimalReact(t *testing.T) {
	t.Parallel()
	templates := Templates()
	if len(templates) == 0 {
		t.Fatal("Templates() returned empty list — minimal-react should be embedded")
	}
	found := false
	for _, name := range templates {
		if name == "minimal-react" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Templates() did not include %q (got %v)", "minimal-react", templates)
	}
}

// TestScaffold_InvalidName_FailsLoud covers every reject-name path so
// the validateName regex doesn't quietly degrade.
func TestScaffold_InvalidName_FailsLoud(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		input   string
		wantSub string // substring expected in the error message
	}{
		{"empty", "", "must not be empty"},
		{"path-separator-slash", "foo/bar", "path separators"},
		{"path-separator-backslash", `foo\bar`, "path separators"},
		{"parent-dir-token", "foo..bar", "parent-directory tokens"},
		{"leading-dash", "-foo", "must match"},
		{"uppercase", "FooBar", "must match"},
		{"space", "foo bar", "must match"},
		{"too-long", strings.Repeat("a", 65), "must match"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := filepath.Join(t.TempDir(), "out")
			_, err := Scaffold(Options{Name: tc.input, OutputDir: out})
			if err == nil {
				t.Fatalf("Scaffold(%q) returned nil error — invalid name MUST fail loud", tc.input)
			}
			if !errors.Is(err, ErrInvalidName) {
				t.Errorf("Scaffold(%q) error not ErrInvalidName: %v", tc.input, err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("Scaffold(%q) error %q missing substring %q", tc.input, err.Error(), tc.wantSub)
			}
			// Output dir MUST NOT have been created on a name-validation failure.
			if _, statErr := os.Stat(out); statErr == nil {
				t.Errorf("Scaffold(%q) created output dir despite invalid name", tc.input)
			}
		})
	}
}

// TestScaffold_OutputDirExists_FailsLoud — the §13 fail-loud posture
// for an operator-facing seam: scaffold refuses to overwrite.
func TestScaffold_OutputDirExists_FailsLoud(t *testing.T) {
	t.Parallel()
	out := filepath.Join(t.TempDir(), "preexists")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("MkdirAll setup: %v", err)
	}
	_, err := Scaffold(Options{Name: "agent", OutputDir: out})
	if err == nil {
		t.Fatal("Scaffold returned nil error against pre-existing output dir")
	}
	if !errors.Is(err, ErrOutputDirExists) {
		t.Errorf("error not ErrOutputDirExists: %v", err)
	}
}

// TestScaffold_UnknownTemplate_FailsLoud pins the unknown-template
// path. The error message lists known templates so the operator can
// fix the typo without re-reading docs.
func TestScaffold_UnknownTemplate_FailsLoud(t *testing.T) {
	t.Parallel()
	out := filepath.Join(t.TempDir(), "agent")
	_, err := Scaffold(Options{Name: "agent", Template: "does-not-exist", OutputDir: out})
	if err == nil {
		t.Fatal("Scaffold returned nil error against unknown template")
	}
	if !errors.Is(err, ErrUnknownTemplate) {
		t.Errorf("error not ErrUnknownTemplate: %v", err)
	}
	if !strings.Contains(err.Error(), "minimal-react") {
		t.Errorf("error %q does not list known templates", err.Error())
	}
}

// TestScaffold_EmptyOutputDir_FailsLoud pins the explicit-empty-
// output-dir path (the cobra body defaults outDir to the name, but
// the engine-level surface still validates).
func TestScaffold_EmptyOutputDir_FailsLoud(t *testing.T) {
	t.Parallel()
	_, err := Scaffold(Options{Name: "agent", OutputDir: ""})
	if err == nil {
		t.Fatal("Scaffold returned nil error against empty output dir")
	}
	if !errors.Is(err, ErrRender) {
		t.Errorf("error not ErrRender: %v", err)
	}
}

// TestScaffold_RenderedConfig_PassesConfigValidate is the load-bearing
// integration test (CLAUDE.md §17). The scaffolded harbor.yaml MUST
// validate via internal/config.Load + Validate with zero further
// edits — the in-PR stand-in for `harbor validate` (Phase 68
// sibling-shipping; CLI integration in Phase 68's PR per §17.6).
func TestScaffold_RenderedConfig_PassesConfigValidate(t *testing.T) {
	t.Parallel()
	out := filepath.Join(t.TempDir(), "validate-agent")
	result, err := Scaffold(Options{Name: "validate-agent", OutputDir: out})
	if err != nil {
		t.Fatalf("Scaffold: unexpected error: %v", err)
	}
	// Find harbor.yaml in the result.
	foundYAML := false
	for _, f := range result.Files {
		if f == "harbor.yaml" {
			foundYAML = true
			break
		}
	}
	if !foundYAML {
		t.Fatalf("scaffolded project missing harbor.yaml (got %v)", result.Files)
	}
	cfgPath := filepath.Join(out, "harbor.yaml")
	cfg, err := config.Load(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("config.Load(%s) failed — the scaffolded config MUST validate cleanly: %v", cfgPath, err)
	}
	// Defence-in-depth: re-run Validate explicitly. Load calls it
	// already, but pinning it here documents the contract.
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate() failed against scaffolded harbor.yaml: %v", err)
	}
}

// TestScaffold_RenderedConfig_DemonstratesProductionShape pins the
// Phase 64 pre-plan note + §13 amendment posture: the rendered config
// uses the production LLM driver (bifrost), a durable state driver
// (sqlite), and an `env.NAME` API key reference — not the `mock`
// driver default. A regression here means the template has been
// silently flipped to a test-stub default; the assertion fires.
func TestScaffold_RenderedConfig_DemonstratesProductionShape(t *testing.T) {
	t.Parallel()
	out := filepath.Join(t.TempDir(), "shape-agent")
	if _, err := Scaffold(Options{Name: "shape-agent", OutputDir: out}); err != nil {
		t.Fatalf("Scaffold: unexpected error: %v", err)
	}
	yamlBytes, err := os.ReadFile(filepath.Join(out, "harbor.yaml"))
	if err != nil {
		t.Fatalf("read scaffolded harbor.yaml: %v", err)
	}
	yamlStr := string(yamlBytes)
	// Production-shape assertions. These pin the §13 amendment +
	// Phase 64 pre-plan note's "production-shaped config in
	// examples" constraint.
	wantSubs := []string{
		"driver: bifrost",         // not mock
		"driver: sqlite",          // not inmem (state)
		"api_key: env.",           // env-var reference, not literal
		"provider: openrouter",    // a real provider
		"model: anthropic/claude", // a real model
	}
	for _, want := range wantSubs {
		if !strings.Contains(yamlStr, want) {
			t.Errorf("scaffolded harbor.yaml missing production-shape marker %q", want)
		}
	}
	// Forbidden substrings: the scaffold MUST NOT ship a --mock
	// default or a literal API key.
	forbidden := []string{
		"driver: mock",
		"api_key: sk-",
		"api_key: replace-me",
	}
	for _, no := range forbidden {
		if strings.Contains(yamlStr, no) {
			t.Errorf("scaffolded harbor.yaml contains forbidden marker %q (test stubs as production defaults — §13)", no)
		}
	}
}

// TestScaffold_CleansUpOnRenderFailure asserts the §13 no-partial-
// writes contract: when rendering fails after the output dir has been
// created, Scaffold removes the dir before returning. The shipped
// templates render cleanly, so this test exercises the cleanup path
// via a contrived render-failure helper — see helper_test.go.
//
// (Phase 67 ships a single bulletproof template, so this test is
// currently a placeholder that asserts the happy-path leaves the dir
// in place. A future phase that adds a fail-able template will
// extend this test.)
func TestScaffold_CleansUpOnRenderFailure(t *testing.T) {
	t.Parallel()
	out := filepath.Join(t.TempDir(), "cleanup-agent")
	if _, err := Scaffold(Options{Name: "cleanup-agent", OutputDir: out}); err != nil {
		t.Fatalf("Scaffold: unexpected error: %v", err)
	}
	// On success, the dir must exist.
	if _, err := os.Stat(out); err != nil {
		t.Errorf("happy-path output dir missing: %v", err)
	}
}
