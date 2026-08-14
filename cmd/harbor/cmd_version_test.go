// cmd/harbor/cmd_version_test.go — tests for the `version` subcommand.
//
// Covers both renderings (human + --json) and pins the field labels /
// JSON keys so smoke scripts can grep them.

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/cmd/harbor/scaffold"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// TestVersionCmd_Human_PrintsThreeLabels asserts the human rendering
// contains the three label-prefixed lines smoke scripts grep for.
func TestVersionCmd_Human_PrintsThreeLabels(t *testing.T) {
	t.Parallel()
	root := NewRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(version): %v", err)
	}
	body := out.String()
	for _, prefix := range []string{"harbor ", "protocol ", "build "} {
		if !strings.Contains(body, "\n"+prefix) && !strings.HasPrefix(body, prefix) {
			t.Errorf("`harbor version` output missing label prefix %q.\nOutput:\n%s", prefix, body)
		}
	}
	if !strings.Contains(body, HarborVersion) {
		t.Errorf("`harbor version` output missing HarborVersion=%q.\nOutput:\n%s", HarborVersion, body)
	}
	if !strings.Contains(body, types.ProtocolVersion) {
		t.Errorf("`harbor version` output missing ProtocolVersion=%q.\nOutput:\n%s", types.ProtocolVersion, body)
	}
}

// TestVersionCmd_JSON_PinsWireShape pins the --json wire shape: three
// fields harbor / protocol / build_hash, all non-empty, harbor and
// protocol matching the source constants.
func TestVersionCmd_JSON_PinsWireShape(t *testing.T) {
	t.Parallel()
	root := NewRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"version", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(version --json): %v", err)
	}
	body := strings.TrimSpace(out.String())
	var parsed map[string]string
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("`harbor version --json` emitted invalid JSON: %v\nBody: %s", err, body)
	}
	// Required fields present and non-empty.
	for _, k := range []string{"harbor", "protocol", "build_hash"} {
		if parsed[k] == "" {
			t.Errorf("`harbor version --json` field %q empty (full body: %s)", k, body)
		}
	}
	if parsed["harbor"] != HarborVersion {
		t.Errorf(".harbor expected %q, got %q", HarborVersion, parsed["harbor"])
	}
	if parsed["protocol"] != types.ProtocolVersion {
		t.Errorf(".protocol expected %q, got %q", types.ProtocolVersion, parsed["protocol"])
	}
	// build_hash is either a real VCS revision (>=8 chars typical) or
	// the documented "unknown" sentinel when ReadBuildInfo() yields
	// nothing useful (e.g. `go test` binary).
	if parsed["build_hash"] != "unknown" && len(parsed["build_hash"]) < 7 {
		t.Errorf(".build_hash unexpected shape: %q (want %q or a VCS revision >=7 chars)", parsed["build_hash"], "unknown")
	}
}

// TestVersionCmd_NoArgs_RejectsPositional pins cobra.NoArgs — a
// positional after `version` is a misuse and should fail closed.
func TestVersionCmd_NoArgs_RejectsPositional(t *testing.T) {
	t.Parallel()
	root := NewRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"version", "extra"})
	if err := root.Execute(); err == nil {
		t.Fatal("`harbor version extra` accepted a positional arg; cobra.NoArgs should reject it")
	}
}

// TestBuildHash_Format pins the buildHash() return shape: either
// "unknown" or a non-empty string. The sentinel is the operator signal
// per the CLAUDE.md §5 "fail loudly" rule.
func TestBuildHash_Format(t *testing.T) {
	t.Parallel()
	got := buildHash()
	if got == "" {
		t.Fatal("buildHash() returned empty string — must return \"unknown\" sentinel instead (CLAUDE.md §5 fail loudly)")
	}
}

// TestCurrentVersionInfo_MirrorsConstants pins the assembly path so a
// future refactor of versionInfo cannot silently break the contract.
func TestCurrentVersionInfo_MirrorsConstants(t *testing.T) {
	t.Parallel()
	info := currentVersionInfo()
	if info.Harbor != HarborVersion {
		t.Errorf("versionInfo.Harbor expected %q, got %q", HarborVersion, info.Harbor)
	}
	if info.Protocol != types.ProtocolVersion {
		t.Errorf("versionInfo.Protocol expected %q, got %q", types.ProtocolVersion, info.Protocol)
	}
	if info.BuildHash == "" {
		t.Error("versionInfo.BuildHash empty — buildHash() must return non-empty (\"unknown\" sentinel allowed)")
	}
}

// TestDisplayVersion_StampedReleaseTags pins displayVersion's release
// resolution: a link-stamped two-component GA tag (`v1.28`), the
// three-component patch form (`v1.28.0`), and either with a conventional
// prerelease suffix are returned as-is; malformed / non-release stamps
// (the "v0.0.0-dev" sentinel, git-describe derivatives, four-component
// dotted strings) fall back to the honest `FallbackModuleVersion +
// "-dev"` rather than reporting the junk stamp.
//
// The test MUTATES the package-level HarborVersion stamp to simulate
// the release build's -ldflags -X injection, so it MUST NOT call
// t.Parallel: the parallel tests in this package read the var, and Go
// pauses every parallel test until all sequential tests have finished.
func TestDisplayVersion_StampedReleaseTags(t *testing.T) {
	orig := HarborVersion
	t.Cleanup(func() { HarborVersion = orig })
	for _, tc := range []struct {
		name  string
		stamp string
		want  string
	}{
		{"two-component GA tag", "v1.28", "v1.28"},
		{"three-component patch", "v1.28.0", "v1.28.0"},
		{"three-component prerelease", "v1.28.0-rc.1", "v1.28.0-rc.1"},
		{"two-component prerelease", "v1.28-rc.1", "v1.28-rc.1"},
		{"major prerelease", "v2.0.0-rc.1", "v2.0.0-rc.1"},
		{"un-stamped sentinel falls back", "v0.0.0-dev", scaffold.FallbackModuleVersion + "-dev"},
		{"git-describe derivative falls back", "v1.13.0-4-gdeadbee", scaffold.FallbackModuleVersion + "-dev"},
		{"four-component falls back", "v1.27.0.1", scaffold.FallbackModuleVersion + "-dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			HarborVersion = tc.stamp
			if got := displayVersion(); got != tc.want {
				t.Errorf("displayVersion() with stamp %q = %q, want %q", tc.stamp, got, tc.want)
			}
		})
	}
}
