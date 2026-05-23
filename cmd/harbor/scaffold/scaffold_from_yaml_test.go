// cmd/harbor/scaffold/scaffold_from_yaml_test.go — engine-level
// coverage for Phase 83o (D-154): scaffold reads an upstream
// harbor.yaml, materialises per-custom-tool Go stubs, and supports
// `--patch` for re-runs that preserve operator-edited files.

package scaffold

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimalUpstreamYAML returns a complete, validator-passing
// harbor.yaml that declares one custom tool. The `tools.built_in`
// entry exercises the built-in projection; the `tools.custom` entry
// exercises the fan-out renderer.
func minimalUpstreamYAML(t *testing.T) string {
	t.Helper()
	const body = `
server:
  bind_addr: 127.0.0.1:8080
  shutdown_grace_period: 30s
identity:
  jwt_algorithms: [RS256]
  issuer: https://issuer.example.com
  audience: scaffold-from-yaml-test
  jwks_url: https://issuer.example.com/.well-known/jwks.json
telemetry:
  log_format: json
  log_level: info
  service_name: scaffold-from-yaml-test
llm:
  provider: openrouter
  model: anthropic/claude-haiku-4-5
  api_key: env.OPENROUTER_API_KEY
  timeout: 60s
  model_profiles:
    anthropic/claude-haiku-4-5:
      context_window_tokens: 200000
tools:
  built_in:
    - clock.now
  custom:
    - name: weather.lookup
      description: Look up current weather by city.
      input:
        city: string
        units: string
      output:
        temp_c: number
        summary: string
`
	dir := t.TempDir()
	path := filepath.Join(dir, "harbor.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write upstream yaml: %v", err)
	}
	return path
}

func TestScaffold_FromConfig_GeneratesCustomToolStubs(t *testing.T) {
	t.Parallel()
	upstream := minimalUpstreamYAML(t)
	out := filepath.Join(t.TempDir(), "alpha")
	res, err := Scaffold(Options{
		Name:           "alpha",
		OutputDir:      out,
		FromConfigPath: upstream,
	})
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	// tools/weather_lookup.go + tools/weather_lookup_test.go must land.
	want := []string{
		"tools/weather_lookup.go",
		"tools/weather_lookup_test.go",
	}
	for _, w := range want {
		found := false
		for _, f := range res.Files {
			if f == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %s in res.Files, got: %v", w, res.Files)
		}
		if _, statErr := os.Stat(filepath.Join(out, w)); statErr != nil {
			t.Errorf("expected %s on disk: %v", w, statErr)
		}
	}
	// The yaml MUST have been copied verbatim into the project (the
	// operator's edits survive, comments preserved).
	copied, err := os.ReadFile(filepath.Join(out, "harbor.yaml"))
	if err != nil {
		t.Fatalf("read copied yaml: %v", err)
	}
	if !strings.Contains(string(copied), "weather.lookup") {
		t.Fatalf("copied yaml does not contain weather.lookup; got:\n%s", string(copied))
	}
	// The generated tool body must mention the typed Input/Output
	// struct fields (deterministic field order by JSON name).
	stub, err := os.ReadFile(filepath.Join(out, "tools/weather_lookup.go"))
	if err != nil {
		t.Fatalf("read tool stub: %v", err)
	}
	stubBody := string(stub)
	for _, expect := range []string{
		"type WeatherLookupInput struct",
		"type WeatherLookupOutput struct",
		`City string ` + "`json:\"city\"`",
		`Units string ` + "`json:\"units\"`",
		`TempC float64 ` + "`json:\"temp_c\"`",
		`Summary string ` + "`json:\"summary\"`",
		"func WeatherLookup(ctx context.Context",
	} {
		if !strings.Contains(stubBody, expect) {
			t.Errorf("tools/weather_lookup.go missing %q; got:\n%s", expect, stubBody)
		}
	}
	// The agent.go must include the RegisterTools function with both
	// the built-in and the custom tool wired.
	agent, err := os.ReadFile(filepath.Join(out, "agent.go"))
	if err != nil {
		t.Fatalf("read agent.go: %v", err)
	}
	agentBody := string(agent)
	for _, expect := range []string{
		"func RegisterTools(cat tools.ToolCatalog) error",
		`"clock.now"`,
		"customtools.WeatherLookup",
	} {
		if !strings.Contains(agentBody, expect) {
			t.Errorf("agent.go missing %q; got:\n%s", expect, agentBody)
		}
	}
}

func TestScaffold_FromConfig_PatchPreservesExistingFiles(t *testing.T) {
	t.Parallel()
	upstream := minimalUpstreamYAML(t)
	out := filepath.Join(t.TempDir(), "alpha")
	if _, err := Scaffold(Options{
		Name:           "alpha",
		OutputDir:      out,
		FromConfigPath: upstream,
	}); err != nil {
		t.Fatalf("Scaffold initial: %v", err)
	}
	// Operator hand-edits the generated stub.
	stubPath := filepath.Join(out, "tools/weather_lookup.go")
	const sentinel = "// OPERATOR EDIT — must survive a --patch run"
	body, _ := os.ReadFile(stubPath)
	if err := os.WriteFile(stubPath, []byte(sentinel+"\n"+string(body)), 0o644); err != nil {
		t.Fatalf("operator edit: %v", err)
	}
	// Re-run scaffold with --patch + the same yaml. The stub must
	// land in Skipped, the operator's edit must survive.
	res, err := Scaffold(Options{
		Name:           "alpha",
		OutputDir:      out,
		FromConfigPath: upstream,
		Patch:          true,
	})
	if err != nil {
		t.Fatalf("Scaffold --patch: %v", err)
	}
	found := false
	for _, s := range res.Skipped {
		if s == "tools/weather_lookup.go" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected tools/weather_lookup.go in Skipped, got: %v", res.Skipped)
	}
	after, err := os.ReadFile(stubPath)
	if err != nil {
		t.Fatalf("read stub after patch: %v", err)
	}
	if !strings.HasPrefix(string(after), sentinel) {
		t.Fatalf("patch overwrote operator edit; first 200 bytes:\n%s", string(after[:min(200, len(after))]))
	}
}

func TestScaffold_FromConfig_PatchAddsNewTools(t *testing.T) {
	t.Parallel()
	upstream := minimalUpstreamYAML(t)
	out := filepath.Join(t.TempDir(), "alpha")
	if _, err := Scaffold(Options{
		Name:           "alpha",
		OutputDir:      out,
		FromConfigPath: upstream,
	}); err != nil {
		t.Fatalf("Scaffold initial: %v", err)
	}
	// Operator adds a second tool to the yaml + re-runs with --patch.
	body, _ := os.ReadFile(upstream)
	expanded := string(body) + `    - name: news.fetch
      description: Fetch the latest news headlines.
      input:
        topic: string
      output:
        headlines: "[]string"
`
	if err := os.WriteFile(upstream, []byte(expanded), 0o644); err != nil {
		t.Fatalf("expand yaml: %v", err)
	}
	res, err := Scaffold(Options{
		Name:           "alpha",
		OutputDir:      out,
		FromConfigPath: upstream,
		Patch:          true,
	})
	if err != nil {
		t.Fatalf("Scaffold --patch: %v", err)
	}
	newFound := false
	for _, f := range res.Files {
		if f == "tools/news_fetch.go" {
			newFound = true
			break
		}
	}
	if !newFound {
		t.Errorf("expected tools/news_fetch.go in Files, got: %v", res.Files)
	}
	if _, err := os.Stat(filepath.Join(out, "tools/news_fetch.go")); err != nil {
		t.Errorf("expected tools/news_fetch.go on disk: %v", err)
	}
}

func TestScaffold_FromConfig_InvalidUpstreamYAMLFailsLoud(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	upstream := filepath.Join(dir, "harbor.yaml")
	if err := os.WriteFile(upstream, []byte("not: valid: yaml: identity\n"), 0o644); err != nil {
		t.Fatalf("write bad yaml: %v", err)
	}
	out := filepath.Join(t.TempDir(), "alpha")
	_, err := Scaffold(Options{
		Name:           "alpha",
		OutputDir:      out,
		FromConfigPath: upstream,
	})
	if !errors.Is(err, ErrUpstreamConfigInvalid) {
		t.Fatalf("want ErrUpstreamConfigInvalid, got %v", err)
	}
}

func TestScaffold_NoPatch_ExistingOutputDirStillFails(t *testing.T) {
	t.Parallel()
	out := filepath.Join(t.TempDir(), "alpha")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := Scaffold(Options{Name: "alpha", OutputDir: out})
	if !errors.Is(err, ErrOutputDirExists) {
		t.Fatalf("want ErrOutputDirExists, got %v", err)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
