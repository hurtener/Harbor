// cmd/harbor/devenv_test.go — tests for the dev-only ./.env auto-load
// (devenv.go): parser scope, environment-wins semantics, fail-loud
// malformed handling, the names-only stderr line, and the
// --no-env-file wiring on `harbor dev`.

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeEnvFile writes content as a .env file in a fresh temp dir and
// returns its path.
func writeEnvFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write .env fixture: %v", err)
	}
	return path
}

// cleanupEnvKeys unsets keys after the test so loadDevEnv's os.Setenv
// side effects never leak across tests.
func cleanupEnvKeys(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		key := k
		if _, exists := os.LookupEnv(key); exists {
			t.Fatalf("env fixture key %s already set in the test process; pick a unique name", key)
		}
		t.Cleanup(func() { os.Unsetenv(key) })
	}
}

// TestLoadDevEnv_LoadsAndReports — table over the supported syntax:
// plain assignments, export prefix, quotes, comments, blank lines,
// CRLF endings, duplicate keys (last wins). Each case asserts the env
// values set and the exact names-only stderr line.
func TestLoadDevEnv_LoadsAndReports(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		wantEnv  map[string]string
		wantLine string
	}{
		{
			name:    "basic assignments",
			content: "HARBOR_DEVENV_T1_A=alpha\nHARBOR_DEVENV_T1_B=beta\n",
			wantEnv: map[string]string{
				"HARBOR_DEVENV_T1_A": "alpha",
				"HARBOR_DEVENV_T1_B": "beta",
			},
			wantLine: "harbor dev: loaded 2 vars from %s (HARBOR_DEVENV_T1_A, HARBOR_DEVENV_T1_B)",
		},
		{
			name:    "export prefix stripped",
			content: "export HARBOR_DEVENV_T2_A=alpha\n",
			wantEnv: map[string]string{
				"HARBOR_DEVENV_T2_A": "alpha",
			},
			wantLine: "harbor dev: loaded 1 var from %s (HARBOR_DEVENV_T2_A)",
		},
		{
			name:    "quotes stripped no escape processing",
			content: "HARBOR_DEVENV_T3_A=\"has spaces # not a comment\"\nHARBOR_DEVENV_T3_B='single \\n literal'\n",
			wantEnv: map[string]string{
				"HARBOR_DEVENV_T3_A": "has spaces # not a comment",
				"HARBOR_DEVENV_T3_B": `single \n literal`,
			},
			wantLine: "harbor dev: loaded 2 vars from %s (HARBOR_DEVENV_T3_A, HARBOR_DEVENV_T3_B)",
		},
		{
			name:    "comments and blank lines",
			content: "# full-line comment\n\nHARBOR_DEVENV_T4_A=alpha # trailing comment\n   # indented comment\nHARBOR_DEVENV_T4_B=\"quoted\" # after quote\n",
			wantEnv: map[string]string{
				"HARBOR_DEVENV_T4_A": "alpha",
				"HARBOR_DEVENV_T4_B": "quoted",
			},
			wantLine: "harbor dev: loaded 2 vars from %s (HARBOR_DEVENV_T4_A, HARBOR_DEVENV_T4_B)",
		},
		{
			name:    "crlf line endings",
			content: "HARBOR_DEVENV_T5_A=alpha\r\nHARBOR_DEVENV_T5_B=beta\r\n",
			wantEnv: map[string]string{
				"HARBOR_DEVENV_T5_A": "alpha",
				"HARBOR_DEVENV_T5_B": "beta",
			},
			wantLine: "harbor dev: loaded 2 vars from %s (HARBOR_DEVENV_T5_A, HARBOR_DEVENV_T5_B)",
		},
		{
			name:    "duplicate key last assignment wins reported once",
			content: "HARBOR_DEVENV_T6_A=first\nHARBOR_DEVENV_T6_A=second\n",
			wantEnv: map[string]string{
				"HARBOR_DEVENV_T6_A": "second",
			},
			wantLine: "harbor dev: loaded 1 var from %s (HARBOR_DEVENV_T6_A)",
		},
		{
			name:    "hash inside unquoted value without preceding space kept",
			content: "HARBOR_DEVENV_T7_A=pass#word\n",
			wantEnv: map[string]string{
				"HARBOR_DEVENV_T7_A": "pass#word",
			},
			wantLine: "harbor dev: loaded 1 var from %s (HARBOR_DEVENV_T7_A)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keys := make([]string, 0, len(tc.wantEnv))
			for k := range tc.wantEnv {
				keys = append(keys, k)
			}
			cleanupEnvKeys(t, keys...)
			path := writeEnvFile(t, tc.content)

			var stderr bytes.Buffer
			if err := loadDevEnv(path, &stderr); err != nil {
				t.Fatalf("loadDevEnv: %v", err)
			}
			for k, want := range tc.wantEnv {
				if got := os.Getenv(k); got != want {
					t.Errorf("env %s = %q, want %q", k, got, want)
				}
			}
			wantLine := strings.ReplaceAll(tc.wantLine, "%s", path)
			if got := strings.TrimSuffix(stderr.String(), "\n"); got != wantLine {
				t.Errorf("stderr line = %q, want %q", got, wantLine)
			}
		})
	}
}

// TestLoadDevEnv_EnvironmentWins — a variable already in the process
// environment is never overridden, and the skip is counted + named in
// the same stderr line.
func TestLoadDevEnv_EnvironmentWins(t *testing.T) {
	t.Setenv("HARBOR_DEVENV_KEPT", "from-environment")
	cleanupEnvKeys(t, "HARBOR_DEVENV_NEW")
	path := writeEnvFile(t, "HARBOR_DEVENV_NEW=loaded\nHARBOR_DEVENV_KEPT=from-dotenv\n")

	var stderr bytes.Buffer
	if err := loadDevEnv(path, &stderr); err != nil {
		t.Fatalf("loadDevEnv: %v", err)
	}
	if got := os.Getenv("HARBOR_DEVENV_KEPT"); got != "from-environment" {
		t.Errorf("environment must win: HARBOR_DEVENV_KEPT = %q, want %q", got, "from-environment")
	}
	if got := os.Getenv("HARBOR_DEVENV_NEW"); got != "loaded" {
		t.Errorf("HARBOR_DEVENV_NEW = %q, want %q", got, "loaded")
	}
	want := "harbor dev: loaded 1 var from " + path + " (HARBOR_DEVENV_NEW; 1 already set, kept environment: HARBOR_DEVENV_KEPT)"
	if got := strings.TrimSuffix(stderr.String(), "\n"); got != want {
		t.Errorf("stderr line = %q, want %q", got, want)
	}
}

// TestLoadDevEnv_ValuesNeverPrinted — the stderr line names variables,
// never values (the whole point of the names-only contract).
func TestLoadDevEnv_ValuesNeverPrinted(t *testing.T) {
	cleanupEnvKeys(t, "HARBOR_DEVENV_SECRET")
	path := writeEnvFile(t, "HARBOR_DEVENV_SECRET=sk-supersecret-123\n")

	var stderr bytes.Buffer
	if err := loadDevEnv(path, &stderr); err != nil {
		t.Fatalf("loadDevEnv: %v", err)
	}
	if strings.Contains(stderr.String(), "sk-supersecret-123") {
		t.Fatalf("stderr leaked a value: %q", stderr.String())
	}
}

// TestLoadDevEnv_Malformed_FailsLoudWithLineNumber — bad entries fail
// the load with file:line precision; nothing is skipped silently.
func TestLoadDevEnv_Malformed_FailsLoudWithLineNumber(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		wantLine string   // the "path:N" fragment the error must carry
		secrets  []string // content fragments that must NEVER appear in the error
	}{
		{
			name:     "missing equals never echoes the line",
			content:  "HARBOR_DEVENV_OK=fine\nsk-live-4eC39HqLyjWDarjtT1zdp7dc\n",
			wantLine: ":2:",
			secrets:  []string{"sk-live", "4eC39Hq"},
		},
		{
			name:     "invalid key leading digit",
			content:  "1BAD=x\n",
			wantLine: ":1:",
			secrets:  []string{"1BAD"},
		},
		{
			// Real base64 secret bodies carry +/ so a pasted continuation line
			// hits the invalid-key path; a pure-alphanumeric prefix would parse
			// as a legitimate KEY= assignment instead (grammar-valid, harmless).
			name:     "invalid key never echoes secret-shaped material",
			content:  "# comment\nMIIEvQIB+DANBgkq/hkiG9w0BAQ==\n",
			wantLine: ":2:",
			secrets:  []string{"MIIEvQ", "hkiG9w"},
		},
		{
			name:     "invalid key embedded space",
			content:  "# comment\nBAD KEY=x\n",
			wantLine: ":2:",
			secrets:  []string{"BAD KEY"},
		},
		{
			name:     "unterminated double quote",
			content:  "HARBOR_DEVENV_Q=\"never closed\n",
			wantLine: ":1:",
			secrets:  []string{"never closed"},
		},
		{
			name:     "trailing garbage after closing quote never echoes the fragment",
			content:  "HARBOR_DEVENV_Q='it' s-secret\n",
			wantLine: ":1:",
			secrets:  []string{"s-secret"},
		},
		{
			name:     "utf-8 BOM named explicitly",
			content:  "\ufeffHARBOR_DEVENV_BOM=x\n",
			wantLine: ":1:",
			secrets:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The valid first line of the missing-equals case may be
			// applied before the failure — clean it up either way.
			t.Cleanup(func() { os.Unsetenv("HARBOR_DEVENV_OK") })
			path := writeEnvFile(t, tc.content)

			var stderr bytes.Buffer
			err := loadDevEnv(path, &stderr)
			if err == nil {
				t.Fatalf("loadDevEnv succeeded on malformed input %q", tc.content)
			}
			if !errors.Is(err, errDevEnvMalformed) {
				t.Errorf("error %v is not errDevEnvMalformed", err)
			}
			if !strings.Contains(err.Error(), path+tc.wantLine) {
				t.Errorf("error %q missing file:line fragment %q", err.Error(), path+tc.wantLine)
			}
			// An error message is still a log line: no fragment of the file's
			// content may ever be echoed (a malformed line is exactly where a
			// wrapped secret lands).
			for _, secret := range tc.secrets {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error %q echoes file content %q", err.Error(), secret)
				}
			}
			if stderr.Len() != 0 && strings.Contains(stderr.String(), "=") {
				t.Errorf("stderr %q carries assignment content", stderr.String())
			}
		})
	}
}

// TestLoadDevEnv_SizeCap — a multi-gigabyte ".env" is a mistake, named loudly
// before it is read into memory.
func TestLoadDevEnv_SizeCap(t *testing.T) {
	path := writeEnvFile(t, "HARBOR_DEVENV_BIG="+strings.Repeat("x", maxDevEnvBytes)+"\n")
	var stderr bytes.Buffer
	err := loadDevEnv(path, &stderr)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized .env must fail loudly with the limit named, got %v", err)
	}
}

// TestLoadDevEnv_MissingFile_SilentNoOp — absence is the normal case:
// no error, no output.
func TestLoadDevEnv_MissingFile_SilentNoOp(t *testing.T) {
	var stderr bytes.Buffer
	if err := loadDevEnv(filepath.Join(t.TempDir(), ".env"), &stderr); err != nil {
		t.Fatalf("loadDevEnv on a missing file must be a no-op, got %v", err)
	}
	if stderr.Len() != 0 {
		t.Errorf("missing file must print nothing, got %q", stderr.String())
	}
}

// TestLoadDevEnv_CommentsOnly_SilentNoOp — a file with no assignments
// loads nothing and announces nothing.
func TestLoadDevEnv_CommentsOnly_SilentNoOp(t *testing.T) {
	path := writeEnvFile(t, "# only a comment\n\n   \n")
	var stderr bytes.Buffer
	if err := loadDevEnv(path, &stderr); err != nil {
		t.Fatalf("loadDevEnv: %v", err)
	}
	if stderr.Len() != 0 {
		t.Errorf("comments-only file must print nothing, got %q", stderr.String())
	}
}

// TestNewDevCmd_RegistersNoEnvFileFlag — `harbor dev` exposes the
// --no-env-file opt-out as a bool flag defaulting off (registered like
// --no-hot-reload).
func TestNewDevCmd_RegistersNoEnvFileFlag(t *testing.T) {
	cmd := newDevCmd()
	f := cmd.Flags().Lookup(flagDevNoEnvFile)
	if f == nil {
		t.Fatalf("dev command is missing the --%s flag", flagDevNoEnvFile)
	}
	if f.Value.Type() != "bool" {
		t.Fatalf("--%s must be a bool flag, got %q", flagDevNoEnvFile, f.Value.Type())
	}
	if f.DefValue != "false" {
		t.Fatalf("--%s must default to false, got %q", flagDevNoEnvFile, f.DefValue)
	}
}

// TestRunDev_MalformedEnvFile_FailsBoot — the runDev wiring: a
// malformed ./.env in the working directory fails the boot loudly with
// the file:line message, BEFORE any config load.
func TestRunDev_MalformedEnvFile_FailsBoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("not an assignment\n"), 0o600); err != nil {
		t.Fatalf("write .env fixture: %v", err)
	}
	t.Chdir(dir)

	root := NewRootCmd()
	var stderr bytes.Buffer
	root.SetOut(&stderr)
	root.SetErr(&stderr)
	root.SetArgs([]string{"dev"})
	err := root.Execute()
	if err == nil {
		t.Fatalf("harbor dev with a malformed .env must fail")
	}
	var cliErr CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error %v is not a CLIError", err)
	}
	if cliErr.Code != CodeBootConfigInvalid {
		t.Errorf("code = %q, want %q", cliErr.Code, CodeBootConfigInvalid)
	}
	if !strings.Contains(cliErr.Message, ".env:1:") {
		t.Errorf("message %q missing .env:1: file:line fragment", cliErr.Message)
	}
}

// TestRunDev_NoEnvFileFlag_SkipsDotenv — with --no-env-file the
// malformed ./.env is ignored entirely: the boot proceeds past the
// dotenv step and fails later on the (deliberately missing)
// harbor.yaml instead.
func TestRunDev_NoEnvFileFlag_SkipsDotenv(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("not an assignment\n"), 0o600); err != nil {
		t.Fatalf("write .env fixture: %v", err)
	}
	t.Chdir(dir)

	root := NewRootCmd()
	var stderr bytes.Buffer
	root.SetOut(&stderr)
	root.SetErr(&stderr)
	root.SetArgs([]string{"dev", "--" + flagDevNoEnvFile})
	err := root.Execute()
	if err == nil {
		t.Fatalf("harbor dev without harbor.yaml must fail")
	}
	var cliErr CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error %v is not a CLIError", err)
	}
	if strings.Contains(cliErr.Message, ".env") {
		t.Errorf("--%s must skip dotenv entirely; message %q still mentions .env", flagDevNoEnvFile, cliErr.Message)
	}
	if cliErr.Code != CodeBootConfigInvalid {
		t.Errorf("code = %q, want %q (missing harbor.yaml)", cliErr.Code, CodeBootConfigInvalid)
	}
}
