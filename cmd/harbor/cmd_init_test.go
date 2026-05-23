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

func TestInitCmd_JSON_HappyPath(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "alpha")
	root := NewRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"init", "--name", "alpha", "--target", target, "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v (stderr: %q)", err, stderr.String())
	}
	body := strings.TrimSpace(stdout.String())
	var parsed struct {
		Name      string   `json:"name"`
		TargetDir string   `json:"target_dir"`
		Files     []string `json:"files"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v (body: %q)", err, body)
	}
	if parsed.Name != "alpha" {
		t.Errorf(".name = %q, want alpha", parsed.Name)
	}
	if !strings.HasSuffix(parsed.TargetDir, "alpha") {
		t.Errorf(".target_dir does not end in alpha; got %q", parsed.TargetDir)
	}
	wantBases := []string{"AGENTS.md", "CLAUDE.md", "README.md", "harbor.yaml"}
	gotBases := make([]string, 0, len(parsed.Files))
	for _, f := range parsed.Files {
		gotBases = append(gotBases, filepath.Base(f))
	}
	sort.Strings(gotBases)
	if len(gotBases) != len(wantBases) {
		t.Fatalf(".files basenames = %v, want %d entries", gotBases, len(wantBases))
	}
	for i, w := range wantBases {
		if gotBases[i] != w {
			t.Errorf(".files[%d] basename = %q, want %q", i, gotBases[i], w)
		}
	}
}

func TestInitCmd_Human_HappyPath(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "humanish")
	root := NewRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"init", "--target", target})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v (stderr: %q)", err, stderr.String())
	}
	body := stdout.String()
	if !strings.Contains(body, "initialised \"humanish\"") {
		t.Errorf("expected initialised marker; got: %s", body)
	}
	if !strings.Contains(body, "Next steps:") {
		t.Errorf("expected Next steps: section; got: %s", body)
	}
}

func TestInitCmd_RefusesToOverwrite_FailsLoud(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	pre := filepath.Join(target, "harbor.yaml")
	if err := os.WriteFile(pre, []byte("pre"), 0o644); err != nil {
		t.Fatalf("WriteFile pre: %v", err)
	}
	root := NewRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"init", "--name", "alpha", "--target", target, "--json"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected failure")
	}
	var cli CLIError
	if !errors.As(err, &cli) {
		t.Fatalf("error is not a CLIError: %T %v", err, err)
	}
	if cli.Code != CodeInitFileExists {
		t.Errorf("CLIError.Code = %q, want %q", cli.Code, CodeInitFileExists)
	}
}

func TestInitCmd_InvalidName_FailsLoud(t *testing.T) {
	t.Parallel()
	root := NewRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"init", "--name", "Bad Name", "--target", t.TempDir(), "--json"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected failure")
	}
	var cli CLIError
	if !errors.As(err, &cli) {
		t.Fatalf("error is not a CLIError: %T %v", err, err)
	}
	if cli.Code != CodeInitInvalidProjectName {
		t.Errorf("CLIError.Code = %q, want %q", cli.Code, CodeInitInvalidProjectName)
	}
}
