// cmd/harbor/validate_test.go — Phase 68 (D-088) tests for `harbor
// validate`. Pins the stable error messages by category via golden
// files. A change to any message is a deliberate `go test -update`.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// validateCase pins each fixture + its expected exit code + the golden
// stems. Golden files live at testdata/validate/golden/<name>.txt
// (human mode) and testdata/validate/golden/<name>.json (--json mode).
type validateCase struct {
	name       string
	fixture    string // relative to cmd/harbor/, "" → no fixture (e.g. nonexistent)
	wantCode   string // CLIError.Code expected, empty for the valid case
	wantExit   int    // 0 valid, 1 validation_failed, 2 validation_internal_error
	wantInBody []string
}

var validateCases = []validateCase{
	{
		name:       "Valid",
		fixture:    "testdata/validate/valid.yaml",
		wantCode:   "",
		wantExit:   0,
		wantInBody: []string{"ok"},
	},
	{
		name:       "MissingLLMProvider",
		fixture:    "testdata/validate/missing-llm-provider.yaml",
		wantCode:   CodeValidationFailed,
		wantExit:   1,
		wantInBody: []string{"config.llm.provider", "config.semantic"},
	},
	{
		name:       "MissingIdentityIssuer",
		fixture:    "testdata/validate/missing-identity-issuer.yaml",
		wantCode:   CodeValidationFailed,
		wantExit:   1,
		wantInBody: []string{"identity.issuer", "config.semantic"},
	},
	{
		name:       "UnknownStateDriver",
		fixture:    "testdata/validate/unknown-state-driver.yaml",
		wantCode:   CodeValidationFailed,
		wantExit:   1,
		wantInBody: []string{"state.driver", "cassandra"},
	},
	{
		name:       "MalformedYAML",
		fixture:    "testdata/validate/malformed-yaml.yaml",
		wantCode:   CodeValidationFailed,
		wantExit:   1,
		wantInBody: []string{"config.parse"},
	},
	{
		name:       "NotFound",
		fixture:    "", // pass an explicit bogus path below
		wantCode:   CodeValidationInternal,
		wantExit:   2,
		wantInBody: []string{"io.not_found", "file not found"},
	},
}

// TestValidate_Human_PinnedByGolden runs each fixture in human mode,
// asserts exit code + body substrings, and golden-compares the body.
// Run `go test -update ./cmd/harbor/...` to regenerate.
func TestValidate_Human_PinnedByGolden(t *testing.T) {
	for _, tc := range validateCases {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := []string{"validate"}
			if tc.fixture != "" {
				args = append(args, tc.fixture)
			} else {
				args = append(args, "testdata/validate/this-file-does-not-exist.yaml")
			}
			stdout, stderr, runErr := runValidateCmd(t, args)
			assertValidateExit(t, tc, runErr)
			body := stdout
			if tc.wantExit != 0 {
				body = stderr
			}
			for _, sub := range tc.wantInBody {
				if !strings.Contains(body, sub) {
					t.Errorf("body missing substring %q\n--- body ---\n%s", sub, body)
				}
			}
			goldenPath := filepath.FromSlash("testdata/validate/golden/" + goldenStem(tc.name) + ".txt")
			compareOrUpdateGolden(t, goldenPath, body)
		})
	}
}

// TestValidate_JSON_PinnedByGolden mirrors the human-mode test for the
// --json wire shape. The body is a single line of JSON; we parse it
// back to assert structural validity AND compare against the golden.
func TestValidate_JSON_PinnedByGolden(t *testing.T) {
	for _, tc := range validateCases {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := []string{"validate", "--json"}
			if tc.fixture != "" {
				args = append(args, tc.fixture)
			} else {
				args = append(args, "testdata/validate/this-file-does-not-exist.yaml")
			}
			stdout, stderr, runErr := runValidateCmd(t, args)
			assertValidateExit(t, tc, runErr)
			body := stdout
			if tc.wantExit != 0 {
				body = stderr
			}
			// Single line + trailing newline.
			body = strings.TrimRight(body, "\n")
			if strings.Contains(body, "\n") {
				t.Errorf("--json body is multi-line:\n%s", body)
			}
			// JSON shape sanity.
			if tc.wantExit == 0 {
				var parsed struct {
					OK   bool   `json:"ok"`
					File string `json:"file"`
				}
				if err := json.Unmarshal([]byte(body), &parsed); err != nil {
					t.Fatalf("--json invalid JSON: %v (body: %q)", err, body)
				}
				if !parsed.OK {
					t.Errorf("--json .ok expected true on valid fixture, got false")
				}
			} else {
				var parsed validationBody
				if err := json.Unmarshal([]byte(body), &parsed); err != nil {
					t.Fatalf("--json invalid JSON: %v (body: %q)", err, body)
				}
				if parsed.Code != tc.wantCode {
					t.Errorf("--json .code expected %q, got %q", tc.wantCode, parsed.Code)
				}
				if len(parsed.Errors) == 0 {
					t.Errorf("--json .errors[] is empty; expected at least one finding")
				}
			}
			goldenPath := filepath.FromSlash("testdata/validate/golden/" + goldenStem(tc.name) + ".json")
			compareOrUpdateGolden(t, goldenPath, body+"\n")
		})
	}
}

// TestValidate_DefaultArgPath asserts that `harbor validate` with no
// argument tries to read `harbor.yaml` in the working directory. We
// run in a tmp working directory that does NOT have a harbor.yaml so
// the call hits the io.not_found path; the message MUST name
// "harbor.yaml" verbatim so operators get an actionable error.
func TestValidate_DefaultArgPath(t *testing.T) {
	// Switch working directory so the relative-path default fires
	// against a known-empty tree.
	dir := t.TempDir()
	prev, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(prev) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	stdout, stderr, runErr := runValidateCmd(t, []string{"validate"})
	if runErr == nil {
		t.Fatalf("expected error on missing default harbor.yaml; stdout=%q stderr=%q", stdout, stderr)
	}
	var cli CLIError
	if !errors.As(runErr, &cli) {
		t.Fatalf("expected CLIError, got %T", runErr)
	}
	if cli.Code != CodeValidationInternal {
		t.Errorf("expected %q, got %q", CodeValidationInternal, cli.Code)
	}
	if !strings.Contains(stderr, "harbor.yaml") {
		t.Errorf("error body did not mention the default path:\n%s", stderr)
	}
}

// TestValidate_QuietFlag_DoesNotSuppressErrors mirrors the cmd_stub
// assertion: --quiet suppresses informational output, NEVER errors.
func TestValidate_QuietFlag_DoesNotSuppressErrors(t *testing.T) {
	t.Parallel()
	_, stderr, runErr := runValidateCmd(t, []string{"validate", "--quiet", "testdata/validate/missing-llm-provider.yaml"})
	if runErr == nil {
		t.Fatal("expected non-nil error on invalid fixture")
	}
	if stderr == "" {
		t.Fatal("--quiet suppressed the error body — acceptance criterion: errors always emit")
	}
}

// TestValidate_QuietFlag_SuppressesSuccessLine asserts that --quiet
// silences the human-mode "<file>: ok" line on a valid fixture. The
// JSON-mode success body is NOT silenced (machine consumers want
// confirmation either way).
func TestValidate_QuietFlag_SuppressesSuccessLine(t *testing.T) {
	t.Parallel()
	stdout, _, runErr := runValidateCmd(t, []string{"validate", "--quiet", "testdata/validate/valid.yaml"})
	if runErr != nil {
		t.Fatalf("unexpected error on valid fixture: %v", runErr)
	}
	if stdout != "" {
		t.Errorf("--quiet did not suppress success line; stdout=%q", stdout)
	}
}

// TestParseLineFromGoccyMessage exercises the goccy parser-error
// line-extraction helper directly so we cover error shapes that the
// fixtures alone can't produce.
func TestParseLineFromGoccyMessage(t *testing.T) {
	t.Parallel()
	tests := map[string]int{
		"[2:3] string was used where mapping is expected": 2,
		"[10:1] unknown field":                            10,
		"no marker here":                                  0,
		"[abc:1] not a number":                            0,
		"[5:7] something\n  excerpt":                      5,
		"":                                                0,
	}
	for in, want := range tests {
		got := parseLineFromGoccyMessage(in)
		if got != want {
			t.Errorf("parseLineFromGoccyMessage(%q) = %d, want %d", in, got, want)
		}
	}
}

// TestLineForFieldPath exercises the AST field-path lookup helper.
// Three shapes: simple path, indexed sequence, missing field.
func TestLineForFieldPath(t *testing.T) {
	t.Parallel()
	data := []byte("a:\n  b:\n    c: 1\nlist:\n  - x: y\n  - x: z\nmap:\n  inner:\n    leaf: 7\n")
	tests := map[string]int{
		"a":                  1,
		"a.b":                2,
		"a.b.c":              3,
		"list":               4,
		"list[0]":            5, // first sequence element
		"list[1]":            6,
		"map.inner":          8,
		"map.inner.leaf":     9,
		"a.b.does_not_exist": 0,
		"missing":            0,
	}
	for path, want := range tests {
		got := lineForFieldPath(data, path)
		if got != want {
			t.Errorf("lineForFieldPath(%q) = %d, want %d", path, got, want)
		}
	}
}

// TestExtractFieldPath exercises the dotted-path extractor over the
// loader's wrapped error message shape.
func TestExtractFieldPath(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"config: invalid configuration: config.llm.provider: must not be empty (source: <bytes>)": "llm.provider",
		"config: invalid configuration: config.identity.issuer: must not be empty (source: x)":    "identity.issuer",
		"config: invalid configuration: <bytes>: parse: [2:3] reason":                             "",
		"":     "",
		"junk": "",
	}
	for in, want := range tests {
		got := extractFieldPath(in)
		if got != want {
			t.Errorf("extractFieldPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestExtractParseReason exercises the parse-error reason extractor.
// Pinned because the message is a public golden contract.
func TestExtractParseReason(t *testing.T) {
	t.Parallel()
	in := "config: invalid configuration: <bytes>: parse: [2:3] reason\n   1 | server:\n>  2 |   bad\n         ^"
	want := "[2:3] reason"
	got := extractParseReason(in)
	if got != want {
		t.Errorf("extractParseReason()\n  got:  %q\n  want: %q", got, want)
	}
	// Without ": parse: " marker, fall back to stripErrorWrappers.
	in2 := "config: invalid configuration: config.x.y: bad (source: <bytes>)"
	want2 := "config.x.y: bad"
	got2 := extractParseReason(in2)
	if got2 != want2 {
		t.Errorf("extractParseReason() fallback\n  got:  %q\n  want: %q", got2, want2)
	}
}

// runValidateCmd builds a fresh root, sets args + buffers, runs Execute.
// Returns stdout, stderr, the error Execute returned. The helper
// avoids the shared-state risk of reusing a single root across tests.
func runValidateCmd(t *testing.T, args []string) (stdout, stderr string, err error) {
	t.Helper()
	root := NewRootCmd()
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	err = root.Execute()
	return outBuf.String(), errBuf.String(), err
}

// assertValidateExit asserts the returned error matches the expected
// exit code mapping: nil → exit 0, CLIError{Code} → tc.wantCode.
func assertValidateExit(t *testing.T, tc validateCase, runErr error) {
	t.Helper()
	if tc.wantExit == 0 {
		if runErr != nil {
			t.Fatalf("expected nil error on valid fixture, got %v", runErr)
		}
		return
	}
	if runErr == nil {
		t.Fatalf("expected non-nil error on invalid fixture; tc=%s", tc.name)
	}
	var cli CLIError
	if !errors.As(runErr, &cli) {
		t.Fatalf("expected CLIError, got %T: %v", runErr, runErr)
	}
	if cli.Code != tc.wantCode {
		t.Errorf("CLIError.Code expected %q, got %q", tc.wantCode, cli.Code)
	}
	if cli.Subcommand != "validate" {
		t.Errorf("CLIError.Subcommand expected %q, got %q", "validate", cli.Subcommand)
	}
	// The exit-code mapping is centralised in main.go::exitCodeFor;
	// pinned indirectly by checking the Code (the mapping is 1-1).
	if got := exitCodeFor(cli); got != tc.wantExit {
		t.Errorf("exitCodeFor(%q) = %d, want %d", cli.Code, got, tc.wantExit)
	}
}

// goldenStem turns a test name into a filesystem-safe stem.
func goldenStem(name string) string {
	return strings.ToLower(strings.ReplaceAll(name, "_", "-"))
}

// compareOrUpdateGolden does what its name says. Honours the package-
// level -update flag declared in root_test.go.
func compareOrUpdateGolden(t *testing.T, path, got string) {
	t.Helper()
	abs, _ := filepath.Abs(path)
	if *update {
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
		if err := os.WriteFile(abs, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("regenerated %s", path)
		return
	}
	want, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate: go test -update ./cmd/harbor/...)", path, err)
	}
	if got != string(want) {
		t.Fatalf("golden drift at %s — regenerate with `go test -update ./cmd/harbor/...`.\n\n--- got ---\n%s\n--- want ---\n%s", path, got, string(want))
	}
}

// guardrail: ensure the testdata/validate/ fixtures actually exist
// before the test runs — a missing fixture is a configuration bug,
// not a test failure.
func TestMain_FixturesExist(t *testing.T) {
	t.Parallel()
	for _, tc := range validateCases {
		if tc.fixture == "" {
			continue
		}
		if _, err := os.Stat(tc.fixture); err != nil {
			t.Errorf("fixture missing: %s (%v); platform=%s", tc.fixture, err, runtime.GOOS)
		}
	}
}
