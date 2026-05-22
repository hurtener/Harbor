// cmd/harbor/cmd_stub_test.go — tests for the six Phase 63 stub
// subcommands. Each must exit non-zero with a structured CLIError of
// {Code: "not_implemented", Hint: <mentions a phase number>}.
//
// The §13 "test stubs as production defaults" amendment requires the
// non-zero exit + the structured error pointing to the implementing
// phase so a script invoking `harbor dev` against a Phase 63 build is
// not fooled into thinking work happened.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
)

// stubCase pins each stub subcommand's expected hint phase number — a
// regression here means a subcommand has been re-pointed without
// updating this test, which is precisely the kind of drift we want to
// catch.
type stubCase struct {
	name      string
	phaseHint *regexp.Regexp // matches the .hint field
}

var stubCases = []stubCase{
	// `scaffold` (Phase 67 / D-087), `validate` (Phase 68 / D-088),
	// `dev` (Phase 64 / D-089), `inspect-events` / `inspect-runs`
	// (Phase 69 / D-101), and `inspect-topology` (Phase 70 / D-102)
	// all graduated out of the stub table — their cobra-driver tests
	// live in their respective cmd_*_test.go files. The stubCases
	// table is now empty; the Test* loops below are no-ops in that
	// state, which is the intended terminal shape until a future
	// phase introduces a new stub subcommand.
}

// TestStubSubcommands_Human_ReturnsCLIError pins the human-mode output:
// `Error: harbor <sub>: not yet implemented (see phase NN ...)` on
// stderr; Execute returns a non-nil error so main() exits non-zero.
func TestStubSubcommands_Human_ReturnsCLIError(t *testing.T) {
	for _, tc := range stubCases {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := NewRootCmd()
			var out, errBuf bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&errBuf)
			root.SetArgs([]string{tc.name})
			err := root.Execute()
			if err == nil {
				t.Fatalf("`harbor %s` returned nil error — stubs MUST exit non-zero (§13 amendment)", tc.name)
			}
			// The returned error must be a CLIError with the
			// not_implemented code.
			var cli CLIError
			if !errors.As(err, &cli) {
				t.Fatalf("`harbor %s` returned non-CLIError %T: %v", tc.name, err, err)
			}
			if cli.Code != CodeNotImplemented {
				t.Errorf("`harbor %s` CLIError.Code expected %q, got %q", tc.name, CodeNotImplemented, cli.Code)
			}
			if cli.Subcommand != tc.name {
				t.Errorf("`harbor %s` CLIError.Subcommand expected %q, got %q", tc.name, tc.name, cli.Subcommand)
			}
			if !tc.phaseHint.MatchString(cli.Hint) {
				t.Errorf("`harbor %s` CLIError.Hint %q does not mention the implementing phase (regex %q)", tc.name, cli.Hint, tc.phaseHint)
			}
			// Stderr body has the human prefix.
			gotErr := errBuf.String()
			wantPrefix := "Error: harbor " + tc.name + ":"
			if !strings.HasPrefix(gotErr, wantPrefix) {
				t.Errorf("`harbor %s` stderr did not start with %q; got: %q", tc.name, wantPrefix, gotErr)
			}
			// Stdout stays clean — errors go to stderr only.
			if out.String() != "" {
				t.Errorf("`harbor %s` wrote to stdout (should be stderr only): %q", tc.name, out.String())
			}
		})
	}
}

// TestStubSubcommands_JSON_StructuredErrorShape pins the --json wire
// shape for each stub subcommand: a single-line JSON object with the
// documented fields. Smoke scripts depend on this exactly.
func TestStubSubcommands_JSON_StructuredErrorShape(t *testing.T) {
	for _, tc := range stubCases {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := NewRootCmd()
			var out, errBuf bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&errBuf)
			root.SetArgs([]string{tc.name, "--json"})
			err := root.Execute()
			if err == nil {
				t.Fatalf("`harbor %s --json` returned nil error", tc.name)
			}
			body := strings.TrimSpace(errBuf.String())
			if body == "" {
				t.Fatalf("`harbor %s --json` produced empty stderr", tc.name)
			}
			// Single line.
			if strings.Contains(body, "\n") {
				t.Errorf("`harbor %s --json` stderr is multi-line (should be one JSON object + trailing newline): %q", tc.name, body)
			}
			var parsed map[string]string
			if jsonErr := json.Unmarshal([]byte(body), &parsed); jsonErr != nil {
				t.Fatalf("`harbor %s --json` emitted invalid JSON: %v (body: %q)", tc.name, jsonErr, body)
			}
			if parsed["code"] != CodeNotImplemented {
				t.Errorf(".code expected %q, got %q", CodeNotImplemented, parsed["code"])
			}
			if parsed["error"] != "not yet implemented" {
				t.Errorf(".error expected %q, got %q", "not yet implemented", parsed["error"])
			}
			if !tc.phaseHint.MatchString(parsed["hint"]) {
				t.Errorf(".hint %q does not mention the implementing phase (regex %q)", parsed["hint"], tc.phaseHint)
			}
			// The Subcommand field must NOT leak onto the wire.
			if strings.Contains(body, `"subcommand"`) {
				t.Errorf("--json body leaked subcommand field: %q", body)
			}
		})
	}
}

// TestStubSubcommands_QuietFlag_DoesNotSuppressErrors was removed
// when `inspect-topology` (the last remaining Phase 63 stub)
// graduated in Phase 70 / D-102. The --quiet-doesn't-silence-errors
// behaviour is now exercised by each graduated subcommand's own test
// file (cmd_dev_test.go, cmd_inspect_topology_test.go, etc.) against
// their real error paths instead of synthetic stub errors. Restore
// this test if a new stub subcommand is introduced.
