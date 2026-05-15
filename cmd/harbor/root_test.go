// cmd/harbor/root_test.go — golden-file test for `harbor --help`.
//
// Pass -update to regenerate the golden file in place:
//
//	go test -update ./cmd/harbor/...
//
// Future subcommand-completing phases (64-70) regenerate the golden in
// the same PR that adds the subcommand.

package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// update controls whether TestRoot_Help_MatchesGolden rewrites the
// golden file. It is plumbed via the standard testing flag set so the
// invocation is `go test -update`.
var update = flag.Bool("update", false, "regenerate testdata/golden/*.txt files")

const goldenHelpPath = "testdata/golden/help.txt"

// runRoot is a small helper: builds a fresh root, sets args, captures
// stdout/stderr separately, returns the combined output cobra emits
// for `--help` (cobra writes help to stdout). The helper avoids the
// shared-state risk of reusing a single root across tests (cobra
// commands carry mutable flag state through Execute).
func runRoot(t *testing.T, args []string) (stdout, stderr string, err error) {
	t.Helper()
	root := NewRootCmd()
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	err = root.Execute()
	return outBuf.String(), errBuf.String(), err
}

// TestRoot_Help_MatchesGolden pins the `harbor --help` output. A
// subcommand added in Phase 64+ MUST regenerate the golden in the
// same PR (-update flag), so the help surface stays in sync with the
// command tree.
func TestRoot_Help_MatchesGolden(t *testing.T) {
	stdout, stderr, err := runRoot(t, []string{"--help"})
	if err != nil {
		t.Fatalf("Execute(--help) returned error: %v", err)
	}
	if stderr != "" {
		t.Errorf("Execute(--help) wrote to stderr: %q", stderr)
	}
	got := stdout
	if *update {
		if writeErr := os.WriteFile(filepath.FromSlash(goldenHelpPath), []byte(got), 0o644); writeErr != nil {
			t.Fatalf("rewrite golden: %v", writeErr)
		}
		t.Logf("regenerated %s", goldenHelpPath)
		return
	}
	want, readErr := os.ReadFile(filepath.FromSlash(goldenHelpPath))
	if readErr != nil {
		t.Fatalf("read golden: %v", readErr)
	}
	if got != string(want) {
		t.Fatalf("`harbor --help` output drifted from %s — run `go test -update ./cmd/harbor/...` to regenerate.\n\n--- got ---\n%s\n--- want ---\n%s", goldenHelpPath, got, string(want))
	}
}

// TestNewRootCmd_RegistersAllSeven asserts every settled subcommand
// (RFC §8) is on the cobra tree. A future phase that drops or renames
// a subcommand WITHOUT an RFC update will fail this test.
func TestNewRootCmd_RegistersAllSeven(t *testing.T) {
	t.Parallel()
	root := NewRootCmd()
	want := map[string]bool{
		"dev":              false,
		"scaffold":         false,
		"validate":         false,
		"version":          false,
		"inspect-events":   false,
		"inspect-runs":     false,
		"inspect-topology": false,
	}
	for _, child := range root.Commands() {
		name := strings.SplitN(child.Use, " ", 2)[0]
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("subcommand %q missing from root (RFC §8 settled set)", name)
		}
	}
}

// TestNewRootCmd_GlobalFlagsRegistered asserts --quiet and --json are
// inherited by every subcommand (cobra inherits PersistentFlags through
// the tree). A regression here breaks the structured-error contract
// (subcommands rely on the inherited --json flag).
func TestNewRootCmd_GlobalFlagsRegistered(t *testing.T) {
	t.Parallel()
	root := NewRootCmd()
	for _, child := range root.Commands() {
		// Skip the auto-generated `help` command.
		if child.Name() == "help" {
			continue
		}
		// Persistent flags are inherited; the child's inherited
		// flag set should resolve --json and --quiet.
		flags := child.InheritedFlags()
		if flags.Lookup(flagJSON) == nil {
			t.Errorf("subcommand %q does not inherit --json", child.Name())
		}
		if flags.Lookup(flagQuiet) == nil {
			t.Errorf("subcommand %q does not inherit --quiet", child.Name())
		}
	}
}
