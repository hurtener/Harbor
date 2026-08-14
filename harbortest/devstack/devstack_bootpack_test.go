package devstack

// devstack_bootpack_test.go — the HA-66 boot-baseline parity at the
// devstack seam (CLAUDE.md §17.6 twin discipline): the eager immutable
// index is opened, validated and collision-pre-read BEFORE the run-loop
// driver is constructed, and the SAME frozen index is handed to the driver
// as its run-start boot baseline — never a second index, never one opened
// after task processing started.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

// TestDevStack_BootIndexOpenedBeforeDriverStart assembles a full devstack
// with ONE real boot-pack declaration and asserts the run-loop driver
// carries the eager frozen index: the declared (tenant, agent) key resolves
// the frozen entry and a foreign key misses. The driver only receives the
// reader at construction (before Start), so a non-nil reader is itself the
// proof the index was opened and validated before the driver started.
func TestDevStack_BootIndexOpenedBeforeDriverStart(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "foundation")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `---
name: workbench-foundation
title: Workbench Foundation
trigger: when asked about workbench
task_type: domain
tags: [ops, boot]
---
Boot skill.

## Steps
- do the thing
- verify the thing
`
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := devstackV128Config(t, func(c *config.Config) {
		c.Skills = config.SkillsConfig{
			Driver: "localdb",
			DSN:    filepath.Join(t.TempDir(), "skills.sqlite"),
			BootAgentPacks: []config.BootAgentPackConfig{{
				TenantID:  DefaultDevTenant,
				AgentID:   devAgentConfigID,
				Directory: root,
				Include:   []string{"foundation"},
			}},
		}
	})
	stack, err := TryAssemble(cfg, AssembleOpts{})
	if err != nil {
		if stack != nil {
			stack.Close()
		}
		t.Fatalf("TryAssemble (boot baseline declared): %v", err)
	}
	defer stack.Close()

	if stack.RunLoopDriver == nil {
		t.Fatal("devstack assembled without a run-loop driver")
	}
	reader := stack.RunLoopDriver.BootPackReader()
	if reader == nil {
		t.Fatal("run-loop driver has no boot-pack reader — the eager index was not supplied before driver start")
	}
	entries, ok := reader.Lookup(DefaultDevTenant, devAgentConfigID)
	if !ok || len(entries) != 1 {
		t.Fatalf("driver boot reader Lookup(%q, %q) = %d entries ok=%v, want the ONE frozen boot entry",
			DefaultDevTenant, devAgentConfigID, len(entries), ok)
	}
	if entries[0].Skill.Name != "workbench-foundation" {
		t.Fatalf("driver boot reader entry name = %q, want workbench-foundation", entries[0].Skill.Name)
	}
	if _, ok := reader.Lookup("foreign-tenant", devAgentConfigID); ok {
		t.Fatal("driver boot reader served a foreign (tenant, agent) key")
	}
}
