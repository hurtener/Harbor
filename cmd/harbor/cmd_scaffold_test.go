// cmd/harbor/cmd_scaffold_test.go — cobra-driver tests for the
// Phase 67 `harbor scaffold` subcommand. The engine-level tests live
// in cmd/harbor/scaffold/scaffold_test.go; this file pins the
// CLI-facing wire shape (--json), the CLIError mapping for every
// negative path, the --quiet posture, and the golden-file diff
// against testdata/golden/minimal-react/.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The Phase 63 root_test.go declares the package-level `update` flag
// (`var update = flag.Bool("update", ...)`); this file reuses it for
// the scaffold goldens so `go test -update ./cmd/harbor/...`
// regenerates the help golden AND the scaffold goldens in one pass.

// goldenProjectName is the fixed project name the golden was rendered
// against. The test diffs every rendered file against
// testdata/golden/minimal-react/<file> for this name; regression
// fires the moment the template mutates without a -update run.
const goldenProjectName = "acme-agent"

// TestScaffold_Golden_MatchesAcmeAgent diffs every file the scaffold
// renders against the committed golden under
// testdata/golden/minimal-react/. Pass -update to regenerate.
func TestScaffold_Golden_MatchesAcmeAgent(t *testing.T) {
	// Cannot run in parallel: shares the global -update flag.
	out := filepath.Join(t.TempDir(), goldenProjectName)
	root := NewRootCmd()
	root.SetArgs([]string{"scaffold", "--name", goldenProjectName, "--output", out})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	if err := root.Execute(); err != nil {
		t.Fatalf("scaffold %q: unexpected error: %v", goldenProjectName, err)
	}
	goldenDir := filepath.Join("testdata", "golden", "minimal-react")
	files := []string{"README.md", "agent.go", "agent_test.go", "go.mod", "harbor.yaml"}
	for _, name := range files {
		actual, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Errorf("read scaffolded %s: %v", name, err)
			continue
		}
		goldenPath := filepath.Join(goldenDir, name)
		if *update {
			if err := os.WriteFile(goldenPath, actual, 0o644); err != nil {
				t.Fatalf("update golden %s: %v", goldenPath, err)
			}
			t.Logf("updated golden: %s", goldenPath)
			continue
		}
		expected, err := os.ReadFile(goldenPath)
		if err != nil {
			t.Errorf("read golden %s: %v", goldenPath, err)
			continue
		}
		if !bytes.Equal(actual, expected) {
			t.Errorf("scaffolded %s does not match golden %s (regenerate via: go test -update ./cmd/harbor/...)",
				name, goldenPath)
		}
	}
}

// TestScaffoldCmd_JSON_HappyPath pins the --json wire shape on
// success. Smoke scripts depend on this exactly.
func TestScaffoldCmd_JSON_HappyPath(t *testing.T) {
	t.Parallel()
	out := filepath.Join(t.TempDir(), "jsonish")
	root := NewRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"scaffold", "--name", "jsonish", "--output", out, "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v (stderr: %q)", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr should be empty on success; got: %q", stderr.String())
	}
	body := strings.TrimSpace(stdout.String())
	if body == "" {
		t.Fatal("--json stdout is empty")
	}
	if strings.Contains(body, "\n") {
		t.Errorf("--json stdout is multi-line (should be one JSON object): %q", body)
	}
	var parsed struct {
		Name      string   `json:"name"`
		OutputDir string   `json:"output_dir"`
		Files     []string `json:"files"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v (body: %q)", err, body)
	}
	if parsed.Name != "jsonish" {
		t.Errorf(".name expected %q, got %q", "jsonish", parsed.Name)
	}
	if !strings.HasSuffix(parsed.OutputDir, "jsonish") {
		t.Errorf(".output_dir expected to end with %q, got %q", "jsonish", parsed.OutputDir)
	}
	wantFiles := []string{"README.md", "agent.go", "agent_test.go", "go.mod", "harbor.yaml"}
	sort.Strings(parsed.Files)
	if len(parsed.Files) != len(wantFiles) {
		t.Errorf(".files len expected %d, got %d (%v)", len(wantFiles), len(parsed.Files), parsed.Files)
	}
	for i, want := range wantFiles {
		if i >= len(parsed.Files) {
			break
		}
		if parsed.Files[i] != want {
			t.Errorf(".files[%d] expected %q, got %q", i, want, parsed.Files[i])
		}
	}
}

// TestScaffoldCmd_Human_HappyPath pins the human-mode multi-line
// output. Smoke scripts grep for the line prefixes.
func TestScaffoldCmd_Human_HappyPath(t *testing.T) {
	t.Parallel()
	out := filepath.Join(t.TempDir(), "humanish")
	root := NewRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"scaffold", "--name", "humanish", "--output", out})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	body := stdout.String()
	if !strings.HasPrefix(body, "scaffolded \"humanish\" at ") {
		t.Errorf("human-mode output should start with `scaffolded \"humanish\" at `; got: %q", body)
	}
	if !strings.Contains(body, "files:") {
		t.Error("human-mode output missing `files:` header")
	}
	for _, f := range []string{"README.md", "agent.go", "agent_test.go", "go.mod", "harbor.yaml"} {
		if !strings.Contains(body, "  "+f) {
			t.Errorf("human-mode output missing file entry %q", f)
		}
	}
}

// TestScaffoldCmd_InvalidName_FailsLoud pins the
// CodeInvalidProjectName mapping. The structured error code is the
// smoke-script-asserted wire shape.
func TestScaffoldCmd_InvalidName_FailsLoud(t *testing.T) {
	t.Parallel()
	root := NewRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	out := filepath.Join(t.TempDir(), "out")
	root.SetArgs([]string{"scaffold", "--name", "Invalid/Name", "--output", out, "--json"})
	err := root.Execute()
	if err == nil {
		t.Fatal("invalid name should fail loud")
	}
	var cli CLIError
	if !errors.As(err, &cli) {
		t.Fatalf("error is not a CLIError: %T %v", err, err)
	}
	if cli.Code != CodeInvalidProjectName {
		t.Errorf("CLIError.Code expected %q, got %q", CodeInvalidProjectName, cli.Code)
	}
	if cli.Subcommand != "scaffold" {
		t.Errorf("CLIError.Subcommand expected %q, got %q", "scaffold", cli.Subcommand)
	}
	body := strings.TrimSpace(stderr.String())
	var parsed struct {
		Code string `json:"code"`
	}
	if jsonErr := json.Unmarshal([]byte(body), &parsed); jsonErr != nil {
		t.Fatalf("invalid JSON on stderr: %v (body: %q)", jsonErr, body)
	}
	if parsed.Code != CodeInvalidProjectName {
		t.Errorf(".code expected %q, got %q", CodeInvalidProjectName, parsed.Code)
	}
}

// TestScaffoldCmd_OutputDirExists_FailsLoud pins the
// CodeOutputDirExists mapping. Acceptance criterion: scaffolding
// twice into the same dir must fail the second time.
func TestScaffoldCmd_OutputDirExists_FailsLoud(t *testing.T) {
	t.Parallel()
	out := filepath.Join(t.TempDir(), "duplicate")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	root := NewRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"scaffold", "--name", "agent", "--output", out, "--json"})
	err := root.Execute()
	if err == nil {
		t.Fatal("pre-existing dir should fail loud")
	}
	var cli CLIError
	if !errors.As(err, &cli) {
		t.Fatalf("error is not CLIError: %v", err)
	}
	if cli.Code != CodeOutputDirExists {
		t.Errorf(".Code expected %q, got %q", CodeOutputDirExists, cli.Code)
	}
}

// TestScaffoldCmd_UnknownTemplate_FailsLoud pins the
// CodeUnknownTemplate mapping. The hint must list the known
// templates so the operator can correct without docs.
func TestScaffoldCmd_UnknownTemplate_FailsLoud(t *testing.T) {
	t.Parallel()
	out := filepath.Join(t.TempDir(), "out")
	root := NewRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"scaffold", "--name", "agent", "--output", out,
		"--template", "does-not-exist"})
	err := root.Execute()
	if err == nil {
		t.Fatal("unknown template should fail loud")
	}
	var cli CLIError
	if !errors.As(err, &cli) {
		t.Fatalf("error is not CLIError: %v", err)
	}
	if cli.Code != CodeUnknownTemplate {
		t.Errorf(".Code expected %q, got %q", CodeUnknownTemplate, cli.Code)
	}
	if !strings.Contains(cli.Hint, "minimal-react") {
		t.Errorf("hint should list known templates; got: %q", cli.Hint)
	}
}

// TestScaffoldCmd_Quiet_DoesNotSuppressErrors pins the --quiet
// posture for the scaffold subcommand: --quiet suppresses
// informational output, NEVER error output (parity with Phase 63's
// stub-quiet check).
func TestScaffoldCmd_Quiet_DoesNotSuppressErrors(t *testing.T) {
	t.Parallel()
	root := NewRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	out := filepath.Join(t.TempDir(), "out")
	root.SetArgs([]string{"scaffold", "--name", "Invalid/Name", "--output", out, "--quiet"})
	err := root.Execute()
	if err == nil {
		t.Fatal("invalid name should fail loud even with --quiet")
	}
	if stderr.Len() == 0 {
		t.Fatal("--quiet suppressed error output (must not)")
	}
}

// TestScaffoldCmd_Quiet_SuppressesInformationalOutput pins the other
// half: on success, --quiet suppresses the human-mode success
// summary. (--json mode is unaffected — the JSON object is the
// machine-readable contract, not informational output.)
func TestScaffoldCmd_Quiet_SuppressesInformationalOutput(t *testing.T) {
	t.Parallel()
	out := filepath.Join(t.TempDir(), "quietish")
	root := NewRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"scaffold", "--name", "quietish", "--output", out, "--quiet"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("--quiet should suppress informational stdout output; got: %q", stdout.String())
	}
}

// TestScaffoldCmd_DefaultsOutputToProjectName pins the cobra body's
// default-output-dir behaviour: when --output is omitted, the
// scaffold writes to ./<name>. We exercise this from a tmp CWD so
// the test does not pollute the working tree.
func TestScaffoldCmd_DefaultsOutputToProjectName(t *testing.T) {
	t.Parallel()
	tmpCWD := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	defer func() { _ = os.Chdir(cwd) }()
	if err := os.Chdir(tmpCWD); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	root := NewRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"scaffold", "--name", "defaultpath"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v (stderr: %q)", err, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(tmpCWD, "defaultpath", "harbor.yaml")); err != nil {
		t.Errorf("default ./<name> not produced: %v", err)
	}
}
