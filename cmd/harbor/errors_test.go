// cmd/harbor/errors_test.go — unit tests for the CLIError type and the
// PrintCLIError sink. Pins the structured-error wire shape from D-084.

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestCLIError_Error_HumanRendering_WithSubcommand(t *testing.T) {
	t.Parallel()
	err := CLIError{
		Subcommand: "dev",
		Message:    "not yet implemented",
		Code:       CodeNotImplemented,
		Hint:       "see phase 64",
	}
	got := err.Error()
	want := "harbor dev: not yet implemented (see phase 64)"
	if got != want {
		t.Fatalf("CLIError.Error()\n  got:  %q\n  want: %q", got, want)
	}
}

func TestCLIError_Error_HumanRendering_NoSubcommand(t *testing.T) {
	t.Parallel()
	err := CLIError{
		Message: "invocation error: unknown flag",
		Code:    "invocation_error",
	}
	got := err.Error()
	want := "harbor: invocation error: unknown flag"
	if got != want {
		t.Fatalf("CLIError.Error()\n  got:  %q\n  want: %q", got, want)
	}
}

func TestCLIError_Error_NoHint_OmitsParenSuffix(t *testing.T) {
	t.Parallel()
	err := CLIError{
		Subcommand: "version",
		Message:    "something went wrong",
		Code:       "boom",
	}
	got := err.Error()
	want := "harbor version: something went wrong"
	if got != want {
		t.Fatalf("CLIError.Error() with empty Hint\n  got:  %q\n  want: %q", got, want)
	}
}

// TestCLIError_MarshalJSON_StableWireShape pins the JSON field names
// that smoke scripts and Console / IDE clients depend on.
func TestCLIError_MarshalJSON_StableWireShape(t *testing.T) {
	t.Parallel()
	err := CLIError{
		Subcommand: "dev", // must NOT appear in JSON — it's "-" tagged
		Message:    "not yet implemented",
		Code:       CodeNotImplemented,
		Hint:       "see phase 64",
	}
	buf, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		t.Fatalf("json.Marshal: %v", marshalErr)
	}
	got := string(buf)
	// Field order is not guaranteed but encoding/json emits in struct
	// definition order. Pin the exact string for deterministic asserts.
	want := `{"error":"not yet implemented","code":"not_implemented","hint":"see phase 64"}`
	if got != want {
		t.Fatalf("CLIError JSON shape\n  got:  %s\n  want: %s", got, want)
	}
	// Subcommand never surfaces on the wire.
	if strings.Contains(got, "dev") || strings.Contains(got, "Subcommand") {
		t.Errorf("CLIError JSON leaked Subcommand: %s", got)
	}
}

// TestCLIError_MarshalJSON_OmitsEmptyHint pins the omitempty behaviour
// for Hint — a CLIError without a hint produces a two-field object.
func TestCLIError_MarshalJSON_OmitsEmptyHint(t *testing.T) {
	t.Parallel()
	err := CLIError{
		Subcommand: "boom",
		Message:    "no hint here",
		Code:       "no_hint",
	}
	buf, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		t.Fatalf("json.Marshal: %v", marshalErr)
	}
	got := string(buf)
	want := `{"error":"no hint here","code":"no_hint"}`
	if got != want {
		t.Fatalf("CLIError JSON shape (no hint)\n  got:  %s\n  want: %s", got, want)
	}
}

// TestPrintCLIError_HumanMode_WritesPrefixedLine asserts the human
// rendering matches the documented form `Error: <CLIError.Error()>`.
func TestPrintCLIError_HumanMode_WritesPrefixedLine(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	err := CLIError{
		Subcommand: "dev",
		Message:    "not yet implemented",
		Code:       CodeNotImplemented,
		Hint:       "see phase 64",
	}
	if printErr := PrintCLIError(&buf, false, err); printErr != nil {
		t.Fatalf("PrintCLIError human-mode returned error: %v", printErr)
	}
	got := buf.String()
	want := "Error: harbor dev: not yet implemented (see phase 64)\n"
	if got != want {
		t.Fatalf("PrintCLIError human-mode\n  got:  %q\n  want: %q", got, want)
	}
}

// TestPrintCLIError_JSONMode_WritesSingleLineJSON asserts the JSON
// rendering is a single line followed by a newline (smoke scripts use
// per-line jq).
func TestPrintCLIError_JSONMode_WritesSingleLineJSON(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	err := CLIError{
		Subcommand: "dev",
		Message:    "not yet implemented",
		Code:       CodeNotImplemented,
		Hint:       "see phase 64",
	}
	if printErr := PrintCLIError(&buf, true, err); printErr != nil {
		t.Fatalf("PrintCLIError json-mode returned error: %v", printErr)
	}
	got := buf.String()
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("PrintCLIError json-mode missing trailing newline: %q", got)
	}
	// One newline only (single-line JSON + trailing).
	if strings.Count(got, "\n") != 1 {
		t.Fatalf("PrintCLIError json-mode emitted multi-line output: %q", got)
	}
	// Parse-back asserts the wire shape.
	var parsed map[string]any
	if jsonErr := json.Unmarshal([]byte(strings.TrimRight(got, "\n")), &parsed); jsonErr != nil {
		t.Fatalf("PrintCLIError json-mode emitted invalid JSON: %v (body: %q)", jsonErr, got)
	}
	if parsed["error"] != "not yet implemented" {
		t.Errorf(".error mismatch: %v", parsed["error"])
	}
	if parsed["code"] != CodeNotImplemented {
		t.Errorf(".code mismatch: %v", parsed["code"])
	}
	if parsed["hint"] != "see phase 64" {
		t.Errorf(".hint mismatch: %v", parsed["hint"])
	}
}

// writeFailer is a bytes.Buffer that errors on Write — used to exercise
// PrintCLIError's loud failure path (CLAUDE.md §5).
type writeFailer struct{}

func (writeFailer) Write(p []byte) (int, error) { return 0, errStubWrite }

var errStubWrite = stubErr("write failed")

type stubErr string

func (e stubErr) Error() string { return string(e) }

// TestPrintCLIError_FailLoudOnWriteFailure_JSON pins the §5 fail-loud
// contract: a writer that errors propagates the error instead of
// silently dropping the structured body.
func TestPrintCLIError_FailLoudOnWriteFailure_JSON(t *testing.T) {
	t.Parallel()
	err := CLIError{Message: "x", Code: "y"}
	got := PrintCLIError(writeFailer{}, true, err)
	if got == nil {
		t.Fatal("PrintCLIError json-mode swallowed a writer error — silent degradation forbidden (CLAUDE.md §5)")
	}
	if !strings.Contains(got.Error(), "write") {
		t.Errorf("PrintCLIError json-mode error did not mention write failure: %v", got)
	}
}

// TestPrintCLIError_FailLoudOnWriteFailure_Human mirrors the JSON-mode
// loud-failure assertion for the human path.
func TestPrintCLIError_FailLoudOnWriteFailure_Human(t *testing.T) {
	t.Parallel()
	err := CLIError{Message: "x", Code: "y"}
	got := PrintCLIError(writeFailer{}, false, err)
	if got == nil {
		t.Fatal("PrintCLIError human-mode swallowed a writer error — silent degradation forbidden (CLAUDE.md §5)")
	}
}
