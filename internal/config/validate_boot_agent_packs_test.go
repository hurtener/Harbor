package config_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
)

// boot_agent_packs — Phase 248 (HA-66) config-contract tests. The
// `skills.boot_agent_packs` block declares boot-time, operator-managed
// per-agent skill pack FILE sources the runtime's boot resolver composes
// for the boot/default agent. These tests pin: strict decoding, the
// deterministic closed bounds, duplicate rejection, the exactly-one-
// relative-package-directory-name include shape, config-file-relative
// directory resolution (never CWD), the no-CWD LoadFromBytes fallback,
// the skills driver/DSN requirement when the block is non-empty, the
// runtime-resolved boot-agent match helper, and N>=100 concurrent reuse.

const validBootAgentPackSkillsBlock = `skills:
  driver: localdb
  dsn: ":memory:"
  boot_agent_packs:
    - tenant_id: acme
      agent_id: harbor-dev-agent
      directory: /etc/harbor/skills
      include: [workbench-foundation]
`

// bootAgentPackFixtureYAML returns the valid_minimal fixture's bytes
// with an appended skills block (the caller supplies the whole
// `skills:` block so both the driver/DSN-present and driver/DSN-absent
// shapes are reachable).
func bootAgentPackFixtureYAML(t *testing.T, skillsBlock string) []byte {
	t.Helper()
	base, err := os.ReadFile(validMinimalFixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var b strings.Builder
	b.Write(base)
	b.WriteString("\n")
	b.WriteString(skillsBlock)
	return []byte(b.String())
}

// bootAgentPackConfig returns a valid config whose `skills` block is
// replaced with a driver/DSN-backed block carrying the given pack
// declarations. Callers break it in subtests.
func bootAgentPackConfig(t *testing.T, packs ...config.BootAgentPackConfig) *config.Config {
	t.Helper()
	cfg := mustLoadValid(t)
	cfg.Skills = config.SkillsConfig{
		Driver:         "localdb",
		DSN:            ":memory:",
		BootAgentPacks: packs,
	}
	return cfg
}

// validBootAgentPack is the canonical valid declaration used across the
// table tests. agent_id deliberately matches the value the runtime would
// resolve; the config package never hard-codes it, the tests only use it
// as a stand-in for the authoritative runtime value.
func validBootAgentPack(tenantID, agentID string) config.BootAgentPackConfig {
	return config.BootAgentPackConfig{
		TenantID:  tenantID,
		AgentID:   agentID,
		Directory: "/etc/harbor/skills",
		Include:   []string{"workbench-foundation"},
	}
}

// TestLoad_BootAgentPacks_StrictDecoding pins the exact YAML shape: the
// four documented fields decode, and anything else is refused — an
// unknown sub-field, a misspelled parent key, or a mistyped value shape
// all fail Load with ErrConfigInvalid (the loader runs goccy
// yaml.Strict()).
func TestLoad_BootAgentPacks_StrictDecoding(t *testing.T) {
	t.Parallel()

	// The canonical shape round-trips.
	cfg, err := config.LoadFromBytes(context.Background(), bootAgentPackFixtureYAML(t, validBootAgentPackSkillsBlock))
	if err != nil {
		t.Fatalf("LoadFromBytes(canonical shape): %v", err)
	}
	packs := cfg.Skills.BootAgentPacks
	if len(packs) != 1 {
		t.Fatalf("BootAgentPacks = %d entries, want 1", len(packs))
	}
	if packs[0].TenantID != "acme" || packs[0].AgentID != "harbor-dev-agent" {
		t.Errorf("entry identity = (%q, %q), want (acme, harbor-dev-agent)", packs[0].TenantID, packs[0].AgentID)
	}
	if packs[0].Directory != "/etc/harbor/skills" {
		t.Errorf("Directory = %q, want /etc/harbor/skills", packs[0].Directory)
	}
	if len(packs[0].Include) != 1 || packs[0].Include[0] != "workbench-foundation" {
		t.Errorf("Include = %v, want [workbench-foundation]", packs[0].Include)
	}

	cases := []struct {
		name     string
		block    string
		fragment string // error text the rejection must contain
	}{
		{
			name: "unknown field inside an entry",
			block: `skills:
  driver: localdb
  dsn: ":memory:"
  boot_agent_packs:
    - tenant_id: acme
      agent_id: harbor-dev-agent
      directory: /etc/harbor/skills
      include: [workbench-foundation]
      pack_name: sneaky
`,
			fragment: "pack_name",
		},
		{
			name: "misspelled parent key",
			block: `skills:
  driver: localdb
  dsn: ":memory:"
  boot_agent_pack:
    - tenant_id: acme
      agent_id: harbor-dev-agent
      directory: /etc/harbor/skills
      include: [workbench-foundation]
`,
			fragment: "boot_agent_pack",
		},
		{
			name: "include given as a scalar",
			block: `skills:
  driver: localdb
  dsn: ":memory:"
  boot_agent_packs:
    - tenant_id: acme
      agent_id: harbor-dev-agent
      directory: /etc/harbor/skills
      include: workbench-foundation
`,
			fragment: "include",
		},
		{
			name: "tenant_id missing",
			block: `skills:
  driver: localdb
  dsn: ":memory:"
  boot_agent_packs:
    - agent_id: harbor-dev-agent
      directory: /etc/harbor/skills
      include: [workbench-foundation]
`,
			fragment: "tenant_id",
		},
		{
			name: "agent_id missing",
			block: `skills:
  driver: localdb
  dsn: ":memory:"
  boot_agent_packs:
    - tenant_id: acme
      directory: /etc/harbor/skills
      include: [workbench-foundation]
`,
			fragment: "agent_id",
		},
		{
			name: "directory missing",
			block: `skills:
  driver: localdb
  dsn: ":memory:"
  boot_agent_packs:
    - tenant_id: acme
      agent_id: harbor-dev-agent
      include: [workbench-foundation]
`,
			fragment: "directory",
		},
		{
			name: "include missing",
			block: `skills:
  driver: localdb
  dsn: ":memory:"
  boot_agent_packs:
    - tenant_id: acme
      agent_id: harbor-dev-agent
      directory: /etc/harbor/skills
`,
			fragment: "include",
		},
		{
			name: "include empty list",
			block: `skills:
  driver: localdb
  dsn: ":memory:"
  boot_agent_packs:
    - tenant_id: acme
      agent_id: harbor-dev-agent
      directory: /etc/harbor/skills
      include: []
`,
			fragment: "include",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.LoadFromBytes(context.Background(), bootAgentPackFixtureYAML(t, tc.block))
			if err == nil {
				t.Fatalf("LoadFromBytes accepted malformed block:\n%s", tc.block)
			}
			if !errors.Is(err, config.ErrConfigInvalid) {
				t.Errorf("err = %v, want wrapping ErrConfigInvalid", err)
			}
			if !strings.Contains(err.Error(), tc.fragment) {
				t.Errorf("err = %v, want it to mention %q", err, tc.fragment)
			}
		})
	}
}

// TestValidate_BootAgentPacks_Bounds pins the deterministic closed
// bounds: declaration count, per-declaration include count, per-field
// lengths, and the aggregate include count. Every bound const is
// exported (config.MaxBootAgentPacks etc.) so the loader and the future
// boot resolver share ONE ceiling.
func TestValidate_BootAgentPacks_Bounds(t *testing.T) {
	t.Parallel()

	runes := func(n int) string { return strings.Repeat("x", n) }

	cases := []struct {
		name        string
		mutate      func(*config.Config)
		errFragment string
	}{
		{
			name: "declaration count over cap",
			mutate: func(cfg *config.Config) {
				packs := make([]config.BootAgentPackConfig, config.MaxBootAgentPacks+1)
				for i := range packs {
					packs[i] = validBootAgentPack(fmt.Sprintf("tenant-%03d", i), "boot-agent")
				}
				cfg.Skills.BootAgentPacks = packs
			},
			errFragment: "at most 64 declarations",
		},
		{
			name: "per-declaration include count over cap",
			mutate: func(cfg *config.Config) {
				inc := make([]string, config.MaxBootAgentPackIncludes+1)
				for i := range inc {
					inc[i] = fmt.Sprintf("pack-%03d", i)
				}
				cfg.Skills.BootAgentPacks = []config.BootAgentPackConfig{
					{TenantID: "acme", AgentID: "boot-agent", Directory: "/etc/harbor/skills", Include: inc},
				}
			},
			errFragment: "at most 64 entries",
		},
		{
			name: "tenant_id over field runes",
			mutate: func(cfg *config.Config) {
				cfg.Skills.BootAgentPacks = []config.BootAgentPackConfig{
					validBootAgentPack(runes(config.MaxBootAgentPackFieldRunes+1), "boot-agent"),
				}
			},
			errFragment: "tenant_id",
		},
		{
			name: "agent_id over field runes",
			mutate: func(cfg *config.Config) {
				cfg.Skills.BootAgentPacks = []config.BootAgentPackConfig{
					validBootAgentPack("acme", runes(config.MaxBootAgentPackFieldRunes+1)),
				}
			},
			errFragment: "agent_id",
		},
		{
			name: "include name over field runes",
			mutate: func(cfg *config.Config) {
				cfg.Skills.BootAgentPacks = []config.BootAgentPackConfig{
					{TenantID: "acme", AgentID: "boot-agent", Directory: "/etc/harbor/skills",
						Include: []string{runes(config.MaxBootAgentPackFieldRunes + 1)}},
				}
			},
			errFragment: "at most 256 runes",
		},
		{
			name: "directory over runes",
			mutate: func(cfg *config.Config) {
				cfg.Skills.BootAgentPacks = []config.BootAgentPackConfig{
					{TenantID: "acme", AgentID: "boot-agent",
						Directory: runes(config.MaxBootAgentPackDirectoryRunes + 1), Include: []string{"pack-a"}},
				}
			},
			errFragment: "directory",
		},
		{
			name: "aggregate include count over cap",
			mutate: func(cfg *config.Config) {
				// 5 declarations x 64 includes: each per-declaration count
				// is exactly at the per-entry cap (64), so only the
				// aggregate (320 > 256) can trip.
				inc := make([]string, config.MaxBootAgentPackIncludes)
				for i := range inc {
					inc[i] = fmt.Sprintf("pack-%03d", i)
				}
				packs := make([]config.BootAgentPackConfig, 0, 5)
				for i := range 5 {
					packs = append(packs, config.BootAgentPackConfig{
						TenantID: fmt.Sprintf("tenant-%03d", i), AgentID: "boot-agent",
						Directory: "/etc/harbor/skills", Include: inc,
					})
				}
				cfg.Skills.BootAgentPacks = packs
			},
			errFragment: "aggregate include count",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := bootAgentPackConfig(t, validBootAgentPack("acme", "boot-agent"))
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate accepted an out-of-bounds boot_agent_packs declaration")
			}
			if !strings.Contains(err.Error(), "skills.boot_agent_packs") {
				t.Errorf("err = %v, want it to name skills.boot_agent_packs", err)
			}
			if !strings.Contains(err.Error(), tc.errFragment) {
				t.Errorf("err = %v, want it to contain %q", err, tc.errFragment)
			}
		})
	}
}

// TestValidate_BootAgentPacks_DirectoryWhitespace pins the directory
// whitespace contract on the PURE validation path — the value a
// hand-built *Config stores is exactly what LoadFromBytes / WithOverrides
// hand validation: surrounding whitespace is rejected OUTRIGHT and the
// rune bound applies to the STORED/raw value, never a trimmed copy, so an
// arbitrary run of spaces cannot pad a directory past the ceiling (the
// old TrimSpace-before-length-check let a padded value validate and pass
// through unresolved to boot).
func TestValidate_BootAgentPacks_DirectoryWhitespace(t *testing.T) {
	t.Parallel()

	spacePad := strings.Repeat(" ", config.MaxBootAgentPackDirectoryRunes+1) + "/etc/harbor/skills"

	cases := []struct {
		name      string
		directory string
		wantErr   bool
	}{
		{name: "clean relative directory", directory: "skills", wantErr: false},
		{name: "clean absolute directory", directory: "/etc/harbor/skills", wantErr: false},
		{name: "whitespace only", directory: "   ", wantErr: true},
		{name: "tab only", directory: "\t", wantErr: true},
		{name: "leading whitespace", directory: " /etc/harbor/skills", wantErr: true},
		{name: "trailing whitespace", directory: "/etc/harbor/skills ", wantErr: true},
		{name: "leading and trailing whitespace", directory: "  /etc/harbor/skills  ", wantErr: true},
		{name: "tab inside the value", directory: "\t/etc/harbor/skills", wantErr: true},
		{name: "space-padded value cannot bypass the rune bound", directory: spacePad, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := bootAgentPackConfig(t, config.BootAgentPackConfig{
				TenantID: "acme", AgentID: "boot-agent",
				Directory: tc.directory, Include: []string{"pack-a"},
			})
			err := cfg.Validate()
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Validate rejected a clean directory %q: %v", tc.directory, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate accepted whitespace-surrounded directory %q", tc.directory)
			}
			if !strings.Contains(err.Error(), "skills.boot_agent_packs[0].directory") {
				t.Errorf("err = %v, want it to name skills.boot_agent_packs[0].directory", err)
			}
			if !strings.Contains(err.Error(), "whitespace") {
				t.Errorf("err = %v, want the surrounding-whitespace reason", err)
			}
		})
	}
}

// TestValidate_BootAgentPacks_Duplicates pins the duplicate rejections:
// the exact (tenant_id, agent_id) pair may be declared once, and each
// declaration's include list is unique both raw and after case-normalised
// comparison (the resolver matches package-directory names
// case-insensitively).
func TestValidate_BootAgentPacks_Duplicates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		packs       []config.BootAgentPackConfig
		errFragment string
	}{
		{
			name: "duplicate exact tenant agent pair",
			packs: []config.BootAgentPackConfig{
				validBootAgentPack("acme", "boot-agent"),
				validBootAgentPack("acme", "boot-agent"),
			},
			errFragment: "duplicates (tenant_id=\"acme\", agent_id=\"boot-agent\")",
		},
		{
			name: "same pair with a different directory is still a duplicate",
			packs: []config.BootAgentPackConfig{
				validBootAgentPack("acme", "boot-agent"),
				{TenantID: "acme", AgentID: "boot-agent", Directory: "/other/dir", Include: []string{"pack-b"}},
			},
			errFragment: "duplicates",
		},
		{
			name: "duplicate raw include",
			packs: []config.BootAgentPackConfig{
				{TenantID: "acme", AgentID: "boot-agent", Directory: "/etc/harbor/skills",
					Include: []string{"workbench-foundation", "workbench-foundation"}},
			},
			errFragment: "duplicate include",
		},
		{
			name: "duplicate case-variant include",
			packs: []config.BootAgentPackConfig{
				{TenantID: "acme", AgentID: "boot-agent", Directory: "/etc/harbor/skills",
					Include: []string{"workbench-foundation", "Workbench-Foundation"}},
			},
			errFragment: "case-variant",
		},
		{
			name: "distinct pairs with the same include do not collide",
			packs: []config.BootAgentPackConfig{
				validBootAgentPack("acme", "boot-agent"),
				validBootAgentPack("acme-other", "boot-agent"),
			},
			errFragment: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := bootAgentPackConfig(t, tc.packs...)
			err := cfg.Validate()
			if tc.errFragment == "" {
				if err != nil {
					t.Fatalf("Validate rejected valid packs: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate accepted a duplicate declaration")
			}
			if !strings.Contains(err.Error(), tc.errFragment) {
				t.Errorf("err = %v, want it to contain %q", err, tc.errFragment)
			}
		})
	}
}

// TestValidate_BootAgentPacks_IncludeShape pins that every include
// entry is EXACTLY one relative package-directory name: empty / dot /
// absolute / traversing / multi-segment / backslash / drive-prefixed
// shapes are all rejected.
func TestValidate_BootAgentPacks_IncludeShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		include string
		wantErr bool
	}{
		{name: "valid package-directory name", include: "workbench-foundation", wantErr: false},
		{name: "valid name with underscore", include: "workbench_foundation", wantErr: false},
		{name: "empty", include: "", wantErr: true},
		{name: "whitespace only", include: "   ", wantErr: true},
		{name: "leading whitespace", include: " pack-a", wantErr: true},
		{name: "trailing whitespace", include: "pack-a ", wantErr: true},
		{name: "dot", include: ".", wantErr: true},
		{name: "dotdot", include: "..", wantErr: true},
		{name: "absolute unix", include: "/etc/harbor/skills", wantErr: true},
		{name: "traversing via separator", include: "../pack-a", wantErr: true},
		{name: "multi-segment", include: "workbench/foundation", wantErr: true},
		{name: "backslash separator", include: `workbench\foundation`, wantErr: true},
		{name: "backslash prefix", include: `\workbench`, wantErr: true},
		{name: "windows drive", include: "C:pack-a", wantErr: true},
		{name: "uri-ish colon", include: "a:b", wantErr: true},
		{name: "trailing separator", include: "pack-a/", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := bootAgentPackConfig(t, config.BootAgentPackConfig{
				TenantID: "acme", AgentID: "boot-agent",
				Directory: "/etc/harbor/skills", Include: []string{tc.include},
			})
			err := cfg.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Validate accepted include %q", tc.include)
				}
				if !strings.Contains(err.Error(), ".include[0]") {
					t.Errorf("err = %v, want it to name the include[0] path", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate rejected valid include %q: %v", tc.include, err)
			}
		})
	}
}

// TestLoad_BootAgentPacks_DirectoryResolution pins the config-file
// provenance: a RELATIVE directory resolves against the loaded config
// file's directory (never CWD), an ABSOLUTE directory is Clean-ed and
// accepted, and a relative directory that lexically escapes the config
// directory is a loud load error.
func TestLoad_BootAgentPacks_DirectoryResolution(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		directory  string
		wantErr    bool
		wantSuffix string // relative to tmpDir; checked only when !wantErr and wantAbs == ""
		wantAbs    string // exact absolute expectation (takes precedence over wantSuffix)
	}{
		{
			name:       "relative directory resolves against the config dir",
			directory:  "skills",
			wantSuffix: "skills",
		},
		{
			name:       "relative directory with a sub-segment stays inside",
			directory:  "tenants/acme/skills",
			wantSuffix: filepath.Join("tenants", "acme", "skills"),
		},
		{
			name:      "relative directory escaping via dotdot",
			directory: "../escape",
			wantErr:   true,
		},
		{
			name:      "absolute directory accepted after clean",
			directory: "/etc/harbor/skills//",
			wantAbs:   filepath.Clean("/etc/harbor/skills//"),
		},
		{
			name:      "whitespace-surrounded absolute directory rejected",
			directory: " /etc/harbor/skills",
			wantErr:   true,
		},
		{
			name:      "whitespace-only directory rejected",
			directory: "   ",
			wantErr:   true,
		},
		{
			name:      "space-padded directory cannot bypass the rune bound",
			directory: strings.Repeat(" ", config.MaxBootAgentPackDirectoryRunes+1) + "/etc/harbor/skills",
			wantErr:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			cfgPath := filepath.Join(tmpDir, "harbor.yaml")
			block := fmt.Sprintf(`skills:
  driver: localdb
  dsn: ":memory:"
  boot_agent_packs:
    - tenant_id: acme
      agent_id: harbor-dev-agent
      directory: %q
      include: [workbench-foundation]
`, tc.directory)
			if err := os.WriteFile(cfgPath, bootAgentPackFixtureYAML(t, block), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			cfg, err := config.Load(context.Background(), cfgPath)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load accepted escaping directory %q", tc.directory)
				}
				if !errors.Is(err, config.ErrConfigInvalid) {
					t.Errorf("err = %v, want wrapping ErrConfigInvalid", err)
				}
				if !strings.Contains(err.Error(), "skills.boot_agent_packs[0].directory") {
					t.Errorf("err = %v, want it to name skills.boot_agent_packs[0].directory", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load(%q): %v", tc.directory, err)
			}
			want := tc.wantAbs
			if want == "" {
				want = filepath.Join(tmpDir, tc.wantSuffix)
			}
			if got := cfg.Skills.BootAgentPacks[0].Directory; got != want {
				t.Errorf("resolved directory = %q, want %q", got, want)
			}
		})
	}
}

// TestLoad_BootAgentPacks_RawBoundBeforeNormalization pins the HA-66
// re-review fix on the file-backed paths (Load and LoadFromBytesAt):
// the RAW directory value must satisfy the surrounding-whitespace rule
// and the rune ceiling BEFORE the resolver normalizes it. filepath.Clean
// / filepath.Join collapse an over-bound `a/../` path below the ceiling
// — an 8k-rune absolute value Clean-s to "/etc/harbor/skills" and an
// 8k-rune relative value Clean-s to a short relative path — so a raw
// value validation must refuse could otherwise slip through the
// resolver shortened. The resolver enforces the raw shape with the SAME
// shared helper validation uses (validateBootAgentPackDirectoryShape),
// so the bound cannot drift between the passes.
func TestLoad_BootAgentPacks_RawBoundBeforeNormalization(t *testing.T) {
	t.Parallel()

	// Raw values whose `a/..` pairs Clean entirely away: each pair
	// cancels, leaving a normalized value far below the ceiling. The
	// raw lengths exceed MaxBootAgentPackDirectoryRunes, so only a
	// bound on the RAW value can reject them.
	absCollapse := "/" + strings.Repeat("a/../", 1+config.MaxBootAgentPackDirectoryRunes/4) + "etc/harbor/skills"
	relCollapse := strings.Repeat("a/../", 1+config.MaxBootAgentPackDirectoryRunes/4) + "etc/harbor/skills"

	if got := len([]rune(absCollapse)); got <= config.MaxBootAgentPackDirectoryRunes {
		t.Fatalf("test invariant: raw absolute value is %d runes, want > %d", got, config.MaxBootAgentPackDirectoryRunes)
	}
	if got := len([]rune(relCollapse)); got <= config.MaxBootAgentPackDirectoryRunes {
		t.Fatalf("test invariant: raw relative value is %d runes, want > %d", got, config.MaxBootAgentPackDirectoryRunes)
	}
	// Prove the collapse premise: normalization DOES shorten both values
	// below the ceiling, so a bound applied after Clean/Join would
	// accept them.
	if clean := filepath.Clean(absCollapse); len([]rune(clean)) > config.MaxBootAgentPackDirectoryRunes {
		t.Fatalf("test invariant: Clean(%q) = %q is still over-bound — the collapse premise is broken", absCollapse, clean)
	}
	if clean := filepath.Clean(relCollapse); len([]rune(clean)) > config.MaxBootAgentPackDirectoryRunes {
		t.Fatalf("test invariant: Clean(%q) = %q is still over-bound — the collapse premise is broken", relCollapse, clean)
	}

	for _, tc := range []struct {
		name      string
		directory string
	}{
		{name: "absolute raw over-bound collapses under clean", directory: absCollapse},
		{name: "relative raw over-bound collapses under clean and join", directory: relCollapse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			cfgPath := filepath.Join(tmpDir, "harbor.yaml")
			block := fmt.Sprintf(`skills:
  driver: localdb
  dsn: ":memory:"
  boot_agent_packs:
    - tenant_id: acme
      agent_id: harbor-dev-agent
      directory: %q
      include: [workbench-foundation]
`, tc.directory)
			data := bootAgentPackFixtureYAML(t, block)
			if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			// File-backed Load — the resolver runs with the config
			// directory provenance and must reject the RAW value before
			// any Clean/Join shortening.
			if _, err := config.Load(context.Background(), cfgPath); !wantBootAgentPackDirectoryErr(t, tc.directory, err) {
				t.Fatalf("Load: unexpected result for raw directory %q", tc.directory)
			}

			// LoadFromBytesAt with a real path runs the SAME resolver —
			// the raw-bound check must fire identically.
			if _, err := config.LoadFromBytesAt(context.Background(), data, cfgPath); !wantBootAgentPackDirectoryErr(t, tc.directory, err) {
				t.Fatalf("LoadFromBytesAt: unexpected result for raw directory %q", tc.directory)
			}
		})
	}
}

// wantBootAgentPackDirectoryErr reports whether err is the expected
// rejection for a malformed boot_agent_packs directory: wrapped in
// ErrConfigInvalid, naming the `skills.boot_agent_packs[0].directory`
// field, and carrying the rune-bound reason (proof the raw-value shape
// check fired rather than a normalization or escape error).
func wantBootAgentPackDirectoryErr(t *testing.T, directory string, err error) bool {
	t.Helper()
	if err == nil {
		t.Errorf("load accepted over-bound raw directory %q (normalized value would be short)", directory)
		return false
	}
	if !errors.Is(err, config.ErrConfigInvalid) {
		t.Errorf("err = %v, want wrapping ErrConfigInvalid", err)
		return false
	}
	if !strings.Contains(err.Error(), "skills.boot_agent_packs[0].directory") {
		t.Errorf("err = %v, want it to name skills.boot_agent_packs[0].directory", err)
		return false
	}
	if !strings.Contains(err.Error(), "at most 4096 runes") {
		t.Errorf("err = %v, want the rune-bound reason (raw shape check, not a normalization/escape path)", err)
		return false
	}
	return true
}

// TestLoadFromBytes_BootAgentPacks_NoCWDResolve pins the no-CWD
// fallback: a config loaded WITHOUT a file source (LoadFromBytes, or
// LoadFromBytesAt with an empty path) keeps a relative directory
// UNRESOLVED — the explicit relative state the later boot loader fails
// loud on — and never resolves it against the process working directory.
func TestLoadFromBytes_BootAgentPacks_NoCWDResolve(t *testing.T) {
	t.Parallel()

	block := `skills:
  driver: localdb
  dsn: ":memory:"
  boot_agent_packs:
    - tenant_id: acme
      agent_id: harbor-dev-agent
      directory: skills
      include: [workbench-foundation]
`
	data := bootAgentPackFixtureYAML(t, block)

	cfg, err := config.LoadFromBytes(context.Background(), data)
	if err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	if got := cfg.Skills.BootAgentPacks[0].Directory; got != "skills" {
		t.Errorf("LoadFromBytes resolved relative directory to %q, want the unresolved %q (never CWD)", got, "skills")
	}

	// LoadFromBytesAt with an empty path must behave exactly like
	// LoadFromBytes: filepath.Dir("") would be "." — the process CWD,
	// which is NOT the config file's directory.
	cfgAt, err := config.LoadFromBytesAt(context.Background(), data, "  ")
	if err != nil {
		t.Fatalf("LoadFromBytesAt(empty path): %v", err)
	}
	if got := cfgAt.Skills.BootAgentPacks[0].Directory; got != "skills" {
		t.Errorf("LoadFromBytesAt(empty path) resolved relative directory to %q, want the unresolved %q", got, "skills")
	}
}

// TestLoadFromBytes_BootAgentPacks_DirectoryWhitespaceRejected pins the
// byte-source regression: LoadFromBytes preserves the RAW directory value
// (the resolve pass is a no-op without a config file), so a
// whitespace-surrounded directory must be rejected at validation rather
// than silently trimmed and accepted — arbitrarily many spaces cannot
// pad a directory past the rune bound on the value that is actually
// stored.
func TestLoadFromBytes_BootAgentPacks_DirectoryWhitespaceRejected(t *testing.T) {
	t.Parallel()

	spacePad := strings.Repeat(" ", config.MaxBootAgentPackDirectoryRunes+1) + "/etc/harbor/skills"
	cases := []struct {
		name      string
		directory string
	}{
		{name: "whitespace only", directory: "   "},
		{name: "leading whitespace", directory: " /etc/harbor/skills"},
		{name: "trailing whitespace", directory: "/etc/harbor/skills "},
		{name: "space-padded value cannot bypass the rune bound", directory: spacePad},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			block := fmt.Sprintf(`skills:
  driver: localdb
  dsn: ":memory:"
  boot_agent_packs:
    - tenant_id: acme
      agent_id: harbor-dev-agent
      directory: %q
      include: [workbench-foundation]
`, tc.directory)
			_, err := config.LoadFromBytes(context.Background(), bootAgentPackFixtureYAML(t, block))
			if err == nil {
				t.Fatalf("LoadFromBytes accepted whitespace-surrounded directory %q", tc.directory)
			}
			if !errors.Is(err, config.ErrConfigInvalid) {
				t.Errorf("err = %v, want wrapping ErrConfigInvalid", err)
			}
			if !strings.Contains(err.Error(), "skills.boot_agent_packs[0].directory") {
				t.Errorf("err = %v, want it to name skills.boot_agent_packs[0].directory", err)
			}
		})
	}
}

// TestLoadFromBytesAt_BootAgentPacks_ResolvesAgainstGivenPathDir proves
// LoadFromBytesAt resolves a relative directory against filepath.Dir(path)
// exactly like Load — the seam `harbor validate` uses — and enforces the
// same escape boundary.
func TestLoadFromBytesAt_BootAgentPacks_ResolvesAgainstGivenPathDir(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "harbor.yaml")
	block := `skills:
  driver: localdb
  dsn: ":memory:"
  boot_agent_packs:
    - tenant_id: acme
      agent_id: harbor-dev-agent
      directory: skills
      include: [workbench-foundation]
`
	data := bootAgentPackFixtureYAML(t, block)

	cfg, err := config.LoadFromBytesAt(context.Background(), data, cfgPath)
	if err != nil {
		t.Fatalf("LoadFromBytesAt: %v", err)
	}
	want := filepath.Join(tmpDir, "skills")
	if got := cfg.Skills.BootAgentPacks[0].Directory; got != want {
		t.Errorf("resolved directory = %q, want %q", got, want)
	}

	escapeBlock := strings.Replace(block, "directory: skills", "directory: ../escape", 1)
	_, err = config.LoadFromBytesAt(context.Background(), bootAgentPackFixtureYAML(t, escapeBlock), cfgPath)
	if err == nil {
		t.Fatal("LoadFromBytesAt accepted an escaping relative directory")
	}
	if !errors.Is(err, config.ErrConfigInvalid) {
		t.Errorf("err = %v, want wrapping ErrConfigInvalid", err)
	}
}

// TestValidate_BootAgentPacks_DriverDSNRequirements pins the composite-
// resolver contract: a NON-EMPTY boot_agent_packs block REQUIRES the
// skills driver + DSN (the boot resolver composes against the configured
// skill store), while an ABSENT block preserves today's behaviour (the
// whole skills block may be zero-valued).
func TestValidate_BootAgentPacks_DriverDSNRequirements(t *testing.T) {
	t.Parallel()

	pack := validBootAgentPack("acme", "boot-agent")

	// Non-empty block with neither driver nor DSN — the requirement fires.
	cfg := mustLoadValid(t)
	cfg.Skills = config.SkillsConfig{BootAgentPacks: []config.BootAgentPackConfig{pack}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "skills.driver") {
		t.Fatalf("Validate = %v, want a skills.driver error for a pack without a store", err)
	}

	// Non-empty block with a driver but no DSN — the existing
	// driver-requires-DSN rule fires (localdb is DSN-requiring).
	cfg = mustLoadValid(t)
	cfg.Skills = config.SkillsConfig{Driver: "localdb", BootAgentPacks: []config.BootAgentPackConfig{pack}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "skills.dsn") {
		t.Fatalf("Validate = %v, want a skills.dsn error for a pack with a driver but no DSN", err)
	}

	// Non-empty block with an unknown driver — the allowlist still fires.
	cfg = mustLoadValid(t)
	cfg.Skills = config.SkillsConfig{Driver: "sqlite", DSN: ":memory:", BootAgentPacks: []config.BootAgentPackConfig{pack}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "skills.driver") {
		t.Fatalf("Validate = %v, want a skills.driver allowlist error (no weakening of the driver contract)", err)
	}

	// Non-empty block with a valid driver + DSN passes.
	cfg = bootAgentPackConfig(t, pack)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected a valid driver+DSN pack block: %v", err)
	}

	// Absent block + zero-valued skills keeps today's behaviour.
	cfg = mustLoadValid(t)
	cfg.Skills = config.SkillsConfig{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected a zero-valued skills block (absent field must preserve compatibility): %v", err)
	}
}

// TestValidateBootAgentPacks_AgentMatch pins the pure boot-agent match
// helper: every declared entry must target the runtime-resolved
// boot/default agent id, and an empty resolved id fails when entries
// exist. The config package never hard-codes the agent id — the runtime
// passes the authoritative value.
func TestValidateBootAgentPacks_AgentMatch(t *testing.T) {
	t.Parallel()

	const resolvedAgent = "resolved-boot-agent"

	matching := []config.BootAgentPackConfig{
		validBootAgentPack("acme", resolvedAgent),
		validBootAgentPack("acme-other", resolvedAgent),
	}

	if err := config.ValidateBootAgentPacks(matching, resolvedAgent); err != nil {
		t.Fatalf("ValidateBootAgentPacks rejected all-matching entries: %v", err)
	}
	if err := config.ValidateBootAgentPacks(nil, ""); err != nil {
		t.Fatalf("ValidateBootAgentPacks(nil, \"\") = %v, want nil (absent field is valid for any resolved id)", err)
	}
	if err := config.ValidateBootAgentPacks(matching, ""); err == nil || !strings.Contains(err.Error(), "resolved boot/default agent id") {
		t.Fatalf("ValidateBootAgentPacks(entries, \"\") = %v, want a loud empty-resolved-id error", err)
	}

	mixed := []config.BootAgentPackConfig{
		validBootAgentPack("acme", resolvedAgent),
		validBootAgentPack("acme-other", "some-other-agent"),
	}
	err := config.ValidateBootAgentPacks(mixed, resolvedAgent)
	if err == nil {
		t.Fatal("ValidateBootAgentPacks accepted an entry targeting a different agent")
	}
	if !strings.Contains(err.Error(), "skills.boot_agent_packs[1].agent_id") {
		t.Errorf("err = %v, want it to name skills.boot_agent_packs[1].agent_id", err)
	}

	// The *Config method form reaches the same predicate.
	cfg := bootAgentPackConfig(t, mixed...)
	if err := cfg.ValidateBootAgentPacksForAgent(resolvedAgent); err == nil || !strings.Contains(err.Error(), "[1].agent_id") {
		t.Fatalf("ValidateBootAgentPacksForAgent = %v, want the [1].agent_id rejection", err)
	}
	cfgAll := bootAgentPackConfig(t, matching...)
	if err := cfgAll.ValidateBootAgentPacksForAgent(resolvedAgent); err != nil {
		t.Fatalf("ValidateBootAgentPacksForAgent rejected all-matching entries: %v", err)
	}
}

// TestValidateBootAgentPacks_ConcurrentReuse is the D-025 concurrent-
// reuse check for the pure boot-agent-pack validation helpers: N>=100
// goroutines invoke the standalone helper and the *Config method against
// a single shared immutable declaration slice, asserting no data race
// (the -race gate) and no goroutine leak.
func TestValidateBootAgentPacks_ConcurrentReuse(t *testing.T) {
	const (
		goroutines = 200
		iterations = 50
	)
	const resolvedAgent = "resolved-boot-agent"
	packs := make([]config.BootAgentPackConfig, 0, 32)
	for i := range 32 {
		packs = append(packs, validBootAgentPack(fmt.Sprintf("tenant-%03d", i), resolvedAgent))
	}
	cfg := bootAgentPackConfig(t, packs...)
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	var mismatches atomic.Int64
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range iterations {
				// Standalone helper — one shared immutable slice.
				if err := config.ValidateBootAgentPacks(packs, resolvedAgent); err != nil {
					mismatches.Add(1)
				}
				// *Config method — one shared immutable config.
				if err := cfg.ValidateBootAgentPacksForAgent(resolvedAgent); err != nil {
					mismatches.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if n := mismatches.Load(); n != 0 {
		t.Fatalf("%d concurrent validations observed unexpected errors", n)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		t.Fatalf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
	}
}
