package postgresruntime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

func TestPostgresRegistry_EnumeratesAllSixStores(t *testing.T) {
	want := []Subsystem{
		SubsystemState,
		SubsystemMemory,
		SubsystemArtifacts,
		SubsystemSkills,
		SubsystemSessionsTurns,
		SubsystemObservabilityRollups,
	}
	got := ManagedSubsystems()
	if len(got) != len(want) {
		t.Fatalf("managed PostgreSQL subsystems = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("managed PostgreSQL subsystem %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestPostgresRegistry_IncludesEveryEnabledProjection(t *testing.T) {
	cfg := &config.Config{
		State:         config.StateConfig{Driver: "postgres", DSN: "state"},
		Memory:        config.MemoryConfig{Driver: "postgres", DSN: "memory"},
		Artifacts:     config.ArtifactsConfig{Driver: "postgres", DSN: "artifacts"},
		Skills:        config.SkillsConfig{Driver: "postgres", DSN: "skills"},
		Sessions:      config.SessionsConfig{Turns: config.TurnsConfig{Driver: "postgres", DSN: "turns"}},
		Observability: config.ObservabilityConfig{Rollups: config.RollupsConfig{Driver: "postgres", DSN: "rollups"}},
	}
	got := PoolSpecs(cfg)
	if len(got) != len(ManagedSubsystems()) {
		t.Fatalf("PoolSpecs returned %d entries, want %d", len(got), len(ManagedSubsystems()))
	}
	seen := make(map[string]bool, len(got))
	for _, spec := range got {
		seen[spec.Subsystem] = true
	}
	for _, subsystem := range ManagedSubsystems() {
		if !seen[string(subsystem)] {
			t.Errorf("enabled subsystem %q missing from pool registry", subsystem)
		}
	}
}

func TestPostgresRegistry_DriversUseNamedMigrationRunner(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", ".."))
	expected := map[string]Subsystem{
		"internal/state/drivers/postgres":                 SubsystemState,
		"internal/memory/drivers/postgres":                SubsystemMemory,
		"internal/artifacts/drivers/postgres":             SubsystemArtifacts,
		"internal/skills/drivers/postgres":                SubsystemSkills,
		"internal/sessions/turns/drivers/postgres":        SubsystemSessionsTurns,
		"internal/observability/rollups/drivers/postgres": SubsystemObservabilityRollups,
	}
	var discovered []string
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() || entry.Name() != "postgres" || filepath.Base(filepath.Dir(path)) != "drivers" {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(path, "postgres.go")); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		discovered = append(discovered, filepath.ToSlash(rel))
		migrationPath := filepath.Join(path, "migrations.go")
		body, err := os.ReadFile(migrationPath)
		if err != nil {
			return err
		}
		text := string(body)
		if !strings.Contains(text, "RunPostgresNamed") || strings.Contains(text, "RunPostgres(") {
			return fmt.Errorf("%s must use RunPostgresNamed and not the legacy bare runner", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(discovered)
	wantPaths := make([]string, 0, len(expected))
	for path := range expected {
		wantPaths = append(wantPaths, path)
	}
	sort.Strings(wantPaths)
	if strings.Join(discovered, "\n") != strings.Join(wantPaths, "\n") {
		t.Fatalf("PostgreSQL driver registry drift: discovered=%v expected=%v", discovered, wantPaths)
	}
}
