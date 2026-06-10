// cmd/harbor/cmd_skill_test.go — Phase 111d (D-201): the `harbor
// skill import` / `harbor skill rm` verb behaviour. Arg validation,
// exit-shaped CLIError codes, --json wire shape, identity scoping,
// and the duplicate-name / overwrite conflict surface — all against
// a real localdb store resolved from a temp harbor.yaml (the same
// resolution `harbor dev` uses).

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// skillCmdYAML mirrors the bootDevStack test fixture plus a skills
// block. The DSN placeholder is filled per-test with a t.TempDir path.
const skillCmdYAML = `
server:
  bind_addr: 127.0.0.1:0
  shutdown_grace_period: 5s
identity:
  jwt_algorithms:
    - ES256
  issuer: https://issuer.example.com
  audience: harbor
  jwks_url: https://issuer.example.com/.well-known/jwks.json
telemetry:
  log_format: text
  log_level: error
  service_name: harbor-test
state:
  driver: inmem
llm:
  driver: mock
  timeout: 30s
  context_window_reserve: 0.05
governance:
  repair_attempts: 1
events:
  driver: inmem
  max_subscribers_per_session: 16
  subscriber_buffer_size: 256
  idle_timeout: 60s
  drop_window: 1s
  replay_buffer_size: 1024
sessions:
  idle_ttl: 24h
  hard_cap: 720h
  sweep_interval: 15m
artifacts:
  driver: inmem
  heavy_output_threshold_bytes: 32768
tasks:
  driver: inprocess
  retain_turn_timeout: 5m
  continuation_hop_limit: 8
distributed:
  bus_driver: loopback
  remote_driver: loopback
memory:
  driver: inmem
  strategy: none
skills:
  driver: localdb
  dsn: %s
`

const skillCmdFixture = `---
name: triage-incident
title: Triage an incident
trigger: when a support ticket arrives
---
Classify a ticket and recommend the next action.

## Steps

- Read the user's report.
- Match against known categories.
`

// writeSkillCmdProject lays out a temp dir with harbor.yaml (skills →
// a tempdir sqlite DSN) and a fixture Skills.md; returns both paths.
func writeSkillCmdProject(t *testing.T) (cfgPath, fixturePath string) {
	t.Helper()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "skills.sqlite")
	cfgPath = filepath.Join(dir, "harbor.yaml")
	if err := os.WriteFile(cfgPath, fmt.Appendf(nil, skillCmdYAML, dsn), 0o600); err != nil {
		t.Fatalf("write harbor.yaml: %v", err)
	}
	fixturePath = filepath.Join(dir, "triage.skill.md")
	if err := os.WriteFile(fixturePath, []byte(skillCmdFixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return cfgPath, fixturePath
}

// runSkillCmd executes the root tree with args; returns stdout,
// stderr, and the Execute error.
func runSkillCmd(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	root := NewRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(args)
	err := root.Execute()
	return stdout.String(), stderr.String(), err
}

func TestSkillImport_HappyPath_ThenRm(t *testing.T) {
	cfgPath, fixturePath := writeSkillCmdProject(t)

	stdout, stderr, err := runSkillCmd(t, "skill", "import", fixturePath, "--config", cfgPath)
	if err != nil {
		t.Fatalf("skill import: %v (stderr=%s)", err, stderr)
	}
	if !strings.Contains(stdout, `imported "triage-incident"`) {
		t.Errorf("stdout missing honest import line: %q", stdout)
	}
	if !strings.Contains(stdout, "driver=localdb") {
		t.Errorf("stdout missing resolved-store line: %q", stdout)
	}

	stdout, stderr, err = runSkillCmd(t, "skill", "rm", "triage-incident", "--config", cfgPath)
	if err != nil {
		t.Fatalf("skill rm: %v (stderr=%s)", err, stderr)
	}
	if !strings.Contains(stdout, `removed "triage-incident"`) {
		t.Errorf("stdout missing honest rm line: %q", stdout)
	}
}

func TestSkillImport_JSONShape(t *testing.T) {
	cfgPath, fixturePath := writeSkillCmdProject(t)

	stdout, stderr, err := runSkillCmd(t, "--json", "skill", "import", fixturePath, "--config", cfgPath)
	if err != nil {
		t.Fatalf("skill import --json: %v (stderr=%s)", err, stderr)
	}
	var out skillImportOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("unmarshal --json output: %v (stdout=%q)", err, stdout)
	}
	if out.Result != "imported" || out.Report.Name != "triage-incident" || out.Report.Steps != 2 {
		t.Errorf("json output = %+v, want imported triage-incident with 2 steps", out)
	}
	if out.Driver != "localdb" {
		t.Errorf("json output driver = %q, want localdb", out.Driver)
	}
}

func TestSkillImport_DuplicateRejectsNonZero_OverwriteSucceeds(t *testing.T) {
	cfgPath, fixturePath := writeSkillCmdProject(t)

	if _, stderr, err := runSkillCmd(t, "skill", "import", fixturePath, "--config", cfgPath); err != nil {
		t.Fatalf("first import: %v (stderr=%s)", err, stderr)
	}

	// Duplicate: non-zero with the stable rejection code.
	_, stderr, err := runSkillCmd(t, "skill", "import", fixturePath, "--config", cfgPath)
	if err == nil {
		t.Fatal("duplicate import succeeded — want rejection")
	}
	var cli CLIError
	if !errors.As(err, &cli) || cli.Code != CodeSkillImportRejected {
		t.Fatalf("err = %v (code=%q), want CLIError code %q", err, cli.Code, CodeSkillImportRejected)
	}
	if !strings.Contains(stderr, "rejected") {
		t.Errorf("stderr missing honest rejection: %q", stderr)
	}

	// --overwrite: succeeds and says so.
	stdout, stderr, err := runSkillCmd(t, "skill", "import", fixturePath, "--config", cfgPath, "--overwrite")
	if err != nil {
		t.Fatalf("overwrite import: %v (stderr=%s)", err, stderr)
	}
	if !strings.Contains(stdout, `overwrote "triage-incident"`) {
		t.Errorf("stdout missing honest overwrite line: %q", stdout)
	}
}

func TestSkillImport_InvalidFile_RejectsNonZero(t *testing.T) {
	cfgPath, _ := writeSkillCmdProject(t)
	bad := filepath.Join(t.TempDir(), "bad.skill.md")
	if err := os.WriteFile(bad, []byte("no frontmatter\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := runSkillCmd(t, "skill", "import", bad, "--config", cfgPath)
	var cli CLIError
	if !errors.As(err, &cli) || cli.Code != CodeSkillImportRejected {
		t.Fatalf("err = %v, want CLIError code %q", err, CodeSkillImportRejected)
	}
	if !strings.Contains(cli.Message, "missing YAML frontmatter") {
		t.Errorf("message %q does not surface the validator's reason", cli.Message)
	}
}

func TestSkillRm_MissingName_FailsNonZero(t *testing.T) {
	cfgPath, _ := writeSkillCmdProject(t)
	_, _, err := runSkillCmd(t, "skill", "rm", "no-such-skill", "--config", cfgPath)
	var cli CLIError
	if !errors.As(err, &cli) || cli.Code != CodeSkillRmFailed {
		t.Fatalf("err = %v, want CLIError code %q", err, CodeSkillRmFailed)
	}
}

func TestSkillImport_NoSkillsBlock_ConfigInvalid(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "harbor.yaml")
	// Same fixture minus the skills block.
	yaml := strings.Split(fmt.Sprintf(skillCmdYAML, "unused"), "skills:")[0]
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(dir, "x.skill.md")
	if err := os.WriteFile(fixture, []byte(skillCmdFixture), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := runSkillCmd(t, "skill", "import", fixture, "--config", cfgPath)
	var cli CLIError
	if !errors.As(err, &cli) || cli.Code != CodeSkillConfigInvalid {
		t.Fatalf("err = %v, want CLIError code %q", err, CodeSkillConfigInvalid)
	}
}

func TestSkillImport_IdentityScoped_RmUnderOtherTenantFails(t *testing.T) {
	cfgPath, fixturePath := writeSkillCmdProject(t)
	if _, stderr, err := runSkillCmd(t, "skill", "import", fixturePath, "--config", cfgPath); err != nil {
		t.Fatalf("import: %v (stderr=%s)", err, stderr)
	}
	// rm under a different tenant cannot see the dev-tenant skill.
	_, _, err := runSkillCmd(t, "skill", "rm", "triage-incident", "--config", cfgPath, "--tenant", "other")
	var cli CLIError
	if !errors.As(err, &cli) || cli.Code != CodeSkillRmFailed {
		t.Fatalf("cross-tenant rm err = %v, want CLIError code %q (§6 isolation)", err, CodeSkillRmFailed)
	}
	// rm under the importing identity succeeds.
	if _, stderr, err := runSkillCmd(t, "skill", "rm", "triage-incident", "--config", cfgPath); err != nil {
		t.Fatalf("same-identity rm: %v (stderr=%s)", err, stderr)
	}
}
