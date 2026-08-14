package bootpacks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/importer"
	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

// TestNew_RejectsDeps pins the fail-loud dependency contract: a nil
// parser or nil catalog is refused before any I/O.
func TestNew_RejectsDeps(t *testing.T) {
	if _, err := New(context.Background(), nil, Deps{}); !errors.Is(err, ErrDepsIncomplete) {
		t.Fatalf("New with empty Deps: err=%v, want ErrDepsIncomplete", err)
	}
	if _, err := New(context.Background(), nil, Deps{Parser: newTestParser(t)}); !errors.Is(err, ErrDepsIncomplete) {
		t.Fatalf("New with nil Catalog: err=%v, want ErrDepsIncomplete", err)
	}
	if _, err := New(context.Background(), nil, Deps{Catalog: allToolsCatalog}); !errors.Is(err, ErrDepsIncomplete) {
		t.Fatalf("New with nil Parser: err=%v, want ErrDepsIncomplete", err)
	}
}

// cancellingParser wraps the real parser and cancels the load context
// on its first parse, so the loader's per-include ctx gates fire
// deterministically between includes.
type cancellingParser struct {
	inner  Parser
	cancel context.CancelFunc
	once   sync.Once
}

func (p *cancellingParser) ImportPackageMarkdown(ctx context.Context, src importer.PackageMarkdownSource) (importer.PackageIngest, error) {
	p.once.Do(p.cancel)
	return p.inner.ImportPackageMarkdown(ctx, src)
}

// TestNew_HonoursCancellation pins the eager loader's cancellation
// contract: a cancelled context fails the load loud between includes,
// and a pre-cancelled context fails before any I/O.
func TestNew_HonoursCancellation(t *testing.T) {
	// Pre-cancelled context: no declarations, no I/O — still refused.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New(ctx, nil, testDeps(t, nil)); !errors.Is(err, context.Canceled) {
		t.Fatalf("New with pre-cancelled ctx: err=%v, want context.Canceled", err)
	}

	// Mid-load cancellation: the first include parses, the parser
	// cancels the ctx, and the second include's gate stops the load.
	root := t.TempDir()
	writePackDir(t, root, "a", map[string]string{"SKILL.md": validSkillMD("a")})
	writePackDir(t, root, "b", map[string]string{"SKILL.md": validSkillMD("b")})
	ctx2, cancel2 := context.WithCancel(context.Background())
	deps := testDeps(t, nil)
	deps.Parser = &cancellingParser{inner: deps.Parser, cancel: cancel2}
	_, err := New(ctx2, []config.BootAgentPackConfig{
		declaration(t, "acme", "agent", root, "a", "b"),
	}, deps)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("New mid-load cancel: err=%v, want context.Canceled", err)
	}

	// Cancellation between declarations: the last include of the first
	// declaration parses, cancels, and the second declaration's gate
	// stops the load.
	ctx3, cancel3 := context.WithCancel(context.Background())
	deps3 := testDeps(t, nil)
	parsed := 0
	deps3.Parser = &countingParser{inner: deps3.Parser, after: func(n int) {
		if n == 2 {
			cancel3()
		}
	}, parsed: &parsed}
	_, err = New(ctx3, []config.BootAgentPackConfig{
		declaration(t, "acme", "agent", root, "a", "b"),
		declaration(t, "globex", "agent", root, "a", "b"),
	}, deps3)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("New cancel between declarations: err=%v, want context.Canceled", err)
	}
}

// countingParser wraps the real parser and runs after(n) after the
// n-th parse.
type countingParser struct {
	inner  Parser
	parsed *int
	after  func(n int)
}

func (p *countingParser) ImportPackageMarkdown(ctx context.Context, src importer.PackageMarkdownSource) (importer.PackageIngest, error) {
	*p.parsed++
	p.after(*p.parsed)
	return p.inner.ImportPackageMarkdown(ctx, src)
}

// TestLoadInclude_EscapeRejected pins the last-line-of-defense
// containment check: a hand-crafted include that lexically escapes the
// declared directory fails loud even when the shape validation pass
// was bypassed (the check lives in loadInclude, not only in the
// declaration validator).
func TestLoadInclude_EscapeRejected(t *testing.T) {
	root := t.TempDir()
	deps := testDeps(t, nil)
	l := &loader{parser: deps.Parser, catalog: deps.Catalog, limits: deps.Limits.Normalize()}
	_, err := l.loadInclude(context.Background(), &config.BootAgentPackConfig{
		Directory: filepath.Join(root, "base"),
	}, "../escape")
	if !errors.Is(err, ErrBootPackInvalid) {
		t.Fatalf("loadInclude escaping include: err=%v, want ErrBootPackInvalid", err)
	}
}

// TestNew_RejectsDeclarationShape pins the shape/bounds rejections
// that fire with zero filesystem I/O: relative directories (never
// CWD-resolved), duplicate keys, include shape, and the
// declaration/item/aggregate-count bounds.
func TestNew_RejectsDeclarationShape(t *testing.T) {
	root := t.TempDir()
	writePackDir(t, root, "pkg", map[string]string{"SKILL.md": validSkillMD("pkg")})

	cases := []struct {
		name    string
		packs   func() []config.BootAgentPackConfig
		wantErr error
	}{
		{
			name: "relative directory is never CWD-resolved",
			packs: func() []config.BootAgentPackConfig {
				return []config.BootAgentPackConfig{
					declaration(t, "acme", "agent", "skills", "pkg"),
				}
			},
			wantErr: ErrRelativeDirectory,
		},
		{
			name: "duplicate (tenant, agent) pair",
			packs: func() []config.BootAgentPackConfig {
				return []config.BootAgentPackConfig{
					declaration(t, "acme", "agent", root, "pkg"),
					declaration(t, "acme", "agent", root, "pkg"),
				}
			},
			wantErr: ErrBootPackInvalid,
		},
		{
			name: "empty tenant",
			packs: func() []config.BootAgentPackConfig {
				return []config.BootAgentPackConfig{declaration(t, "", "agent", root, "pkg")}
			},
			wantErr: ErrBootPackInvalid,
		},
		{
			name: "empty agent",
			packs: func() []config.BootAgentPackConfig {
				return []config.BootAgentPackConfig{declaration(t, "acme", "", root, "pkg")}
			},
			wantErr: ErrBootPackInvalid,
		},
		{
			name: "include with a path separator",
			packs: func() []config.BootAgentPackConfig {
				return []config.BootAgentPackConfig{declaration(t, "acme", "agent", root, "a/b")}
			},
			wantErr: ErrBootPackInvalid,
		},
		{
			name: "include is dot-dot",
			packs: func() []config.BootAgentPackConfig {
				return []config.BootAgentPackConfig{declaration(t, "acme", "agent", root, "..")}
			},
			wantErr: ErrBootPackInvalid,
		},
		{
			name: "include with surrounding whitespace",
			packs: func() []config.BootAgentPackConfig {
				return []config.BootAgentPackConfig{declaration(t, "acme", "agent", root, " pkg ")}
			},
			wantErr: ErrBootPackInvalid,
		},
		{
			name: "include with a drive/URI form",
			packs: func() []config.BootAgentPackConfig {
				return []config.BootAgentPackConfig{declaration(t, "acme", "agent", root, "C:pkg")}
			},
			wantErr: ErrBootPackInvalid,
		},
		{
			name: "include over the field rune bound",
			packs: func() []config.BootAgentPackConfig {
				return []config.BootAgentPackConfig{declaration(t, "acme", "agent", root, strings.Repeat("x", config.MaxBootAgentPackFieldRunes+1))}
			},
			wantErr: ErrBoundExceeded,
		},
		{
			name: "tenant over the field rune bound",
			packs: func() []config.BootAgentPackConfig {
				return []config.BootAgentPackConfig{declaration(t, strings.Repeat("t", config.MaxBootAgentPackFieldRunes+1), "agent", root, "pkg")}
			},
			wantErr: ErrBoundExceeded,
		},
		{
			name: "agent over the field rune bound",
			packs: func() []config.BootAgentPackConfig {
				return []config.BootAgentPackConfig{declaration(t, "acme", strings.Repeat("a", config.MaxBootAgentPackFieldRunes+1), root, "pkg")}
			},
			wantErr: ErrBoundExceeded,
		},
		{
			name: "directory over the rune bound",
			packs: func() []config.BootAgentPackConfig {
				long := root + "/" + strings.Repeat("d", config.MaxBootAgentPackDirectoryRunes)
				return []config.BootAgentPackConfig{declaration(t, "acme", "agent", long, "pkg")}
			},
			wantErr: ErrBoundExceeded,
		},
		{
			name: "no includes",
			packs: func() []config.BootAgentPackConfig {
				return []config.BootAgentPackConfig{declaration(t, "acme", "agent", root)}
			},
			wantErr: ErrBootPackInvalid,
		},
		{
			name: "declaration count over bound",
			packs: func() []config.BootAgentPackConfig {
				out := make([]config.BootAgentPackConfig, 0, config.MaxBootAgentPacks+1)
				for i := 0; i <= config.MaxBootAgentPacks; i++ {
					out = append(out, declaration(t, fmt.Sprintf("t%03d", i), "agent", root, "pkg"))
				}
				return out
			},
			wantErr: ErrBoundExceeded,
		},
		{
			name: "include count over bound",
			packs: func() []config.BootAgentPackConfig {
				includes := make([]string, config.MaxBootAgentPackIncludes+1)
				for i := range includes {
					includes[i] = fmt.Sprintf("pkg%d", i)
				}
				return []config.BootAgentPackConfig{declaration(t, "acme", "agent", root, includes...)}
			},
			wantErr: ErrBoundExceeded,
		},
		{
			name: "aggregate include count over bound",
			packs: func() []config.BootAgentPackConfig {
				var out []config.BootAgentPackConfig
				total := config.MaxBootAgentPackAggregateIncludes + 1
				for i := 0; total > 0; i++ {
					n := config.MaxBootAgentPackIncludes
					if n > total {
						n = total
					}
					includes := make([]string, n)
					for j := range includes {
						includes[j] = fmt.Sprintf("pkg-%d-%d", i, j)
					}
					out = append(out, declaration(t, fmt.Sprintf("t%03d", i), "agent", root, includes...))
					total -= n
				}
				return out
			},
			wantErr: ErrBoundExceeded,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(context.Background(), tc.packs(), testDeps(t, nil))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("New: err=%v, want errors.Is(err, %v)", err, tc.wantErr)
			}
		})
	}
}

// TestNew_RejectsFilesystemMatrix pins the strict filesystem gate: the
// include root and the SKILL.md must be exactly one case-sensitive
// top-level regular single-link file, and nothing else may live in the
// package directory.
func TestNew_RejectsFilesystemMatrix(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T, root string) string // returns include dir name
		wantErr error
	}{
		{
			name: "missing directory",
			setup: func(t *testing.T, root string) string {
				return "does-not-exist"
			},
			wantErr: os.ErrNotExist,
		},
		{
			name: "directory is a regular file",
			setup: func(t *testing.T, root string) string {
				path := filepath.Join(root, "not-a-dir")
				if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				return "not-a-dir"
			},
			wantErr: ErrNotDirectory,
		},
		{
			name: "directory is a symlink",
			setup: func(t *testing.T, root string) string {
				real := filepath.Join(root, "real")
				if err := os.MkdirAll(real, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(real, "SKILL.md"), []byte(validSkillMD("real")), 0o644); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(root, "linked")
				if err := os.Symlink(real, link); err != nil {
					t.Fatalf("Symlink: %v", err)
				}
				return "linked"
			},
			wantErr: ErrSymlink,
		},
		{
			name: "empty package directory",
			setup: func(t *testing.T, root string) string {
				writePackDir(t, root, "empty", nil)
				return "empty"
			},
			wantErr: ErrSkillMDEntry,
		},
		{
			name: "wrong-case skill file",
			setup: func(t *testing.T, root string) string {
				writePackDir(t, root, "wrongcase", map[string]string{
					"skill.md": validSkillMD("wrongcase"),
				})
				return "wrongcase"
			},
			wantErr: ErrSkillMDEntry,
		},
		{
			name: "SKILL.md in a subdirectory",
			setup: func(t *testing.T, root string) string {
				dir := writePackDir(t, root, "nested", nil)
				if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "sub", "SKILL.md"), []byte(validSkillMD("nested")), 0o644); err != nil {
					t.Fatal(err)
				}
				return "nested"
			},
			wantErr: ErrSkillMDEntry,
		},
		{
			name: "extra file alongside SKILL.md",
			setup: func(t *testing.T, root string) string {
				writePackDir(t, root, "extra-file", map[string]string{
					"SKILL.md":   validSkillMD("extra-file"),
					"README.txt": "extra",
				})
				return "extra-file"
			},
			wantErr: ErrSkillMDEntry,
		},
		{
			name: "extra subdirectory alongside SKILL.md",
			setup: func(t *testing.T, root string) string {
				dir := writePackDir(t, root, "extra-dir", map[string]string{
					"SKILL.md": validSkillMD("extra-dir"),
				})
				if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
					t.Fatal(err)
				}
				return "extra-dir"
			},
			wantErr: ErrSkillMDEntry,
		},
		{
			name: "SKILL.md is a symlink",
			setup: func(t *testing.T, root string) string {
				real := filepath.Join(root, "real-skill.md")
				if err := os.WriteFile(real, []byte(validSkillMD("real")), 0o644); err != nil {
					t.Fatal(err)
				}
				dir := writePackDir(t, root, "symlink-file", nil)
				if err := os.Symlink(real, filepath.Join(dir, "SKILL.md")); err != nil {
					t.Fatalf("Symlink: %v", err)
				}
				return "symlink-file"
			},
			wantErr: ErrSymlink,
		},
		{
			name: "SKILL.md is a hardlink (nlink > 1)",
			setup: func(t *testing.T, root string) string {
				dir := writePackDir(t, root, "hardlink", map[string]string{
					"SKILL.md": validSkillMD("hardlink"),
				})
				// A second name in ANOTHER directory bumps nlink while
				// the package directory keeps exactly one entry.
				other := filepath.Join(root, "elsewhere")
				if err := os.MkdirAll(other, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(filepath.Join(dir, "SKILL.md"), filepath.Join(other, "link")); err != nil {
					t.Fatalf("Link: %v", err)
				}
				return "hardlink"
			},
			wantErr: ErrHardlink,
		},
		{
			name: "SKILL.md too large",
			setup: func(t *testing.T, root string) string {
				writePackDir(t, root, "too-large", map[string]string{
					"SKILL.md": strings.Repeat("a", skillpkg.MaxPackageSkillMDBytes+1),
				})
				return "too-large"
			},
			wantErr: ErrBoundExceeded,
		},
		{
			name: "aggregate bytes over bound",
			setup: func(t *testing.T, root string) string {
				writePackDir(t, root, "aggregate", map[string]string{
					"SKILL.md": validSkillMD("aggregate"),
				})
				return "aggregate"
			},
			wantErr: ErrBoundExceeded,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			include := tc.setup(t, root)
			deps := testDeps(t, nil)
			if errors.Is(tc.wantErr, ErrBoundExceeded) && tc.name == "aggregate bytes over bound" {
				deps.Limits.MaxAggregateBytes = 16 // any real SKILL.md exceeds this
			}
			_, err := New(context.Background(), []config.BootAgentPackConfig{
				declaration(t, "acme", "agent", root, include),
			}, deps)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("New: err=%v, want errors.Is(err, %v)", err, tc.wantErr)
			}
		})
	}
}

// TestNew_RejectsParseMatrix pins the import gate: every malformed,
// non-UTF-8, support-referencing, or duplicate-name baseline entry
// fails the whole load loud, surfacing the pure parser's sentinels.
func TestNew_RejectsParseMatrix(t *testing.T) {
	cases := []struct {
		name    string
		docs    map[string]string // include -> SKILL.md content
		wantErr error
	}{
		{
			name:    "no frontmatter",
			docs:    map[string]string{"bad": "plain text without frontmatter"},
			wantErr: skillpkg.ErrSkillMDFrontmatterMissing,
		},
		{
			name:    "not valid UTF-8",
			docs:    map[string]string{"bad": "---\nname: x\ntrigger: t\n---\n\xff\xfe\n## Steps\n- s\n"},
			wantErr: importer.ErrPackageMarkdownNotUTF8,
		},
		{
			name:    "support reference in the body",
			docs:    map[string]string{"bad": "---\nname: x\ntrigger: t\n---\n![diagram](assets/logo.png)\n\n## Steps\n- s\n"},
			wantErr: importer.ErrPackageSupportRefMissing,
		},
		{
			name: "duplicate canonical skill name across includes",
			docs: map[string]string{
				"a": validSkillMD("same-name"),
				"b": validSkillMD("same-name"),
			},
			wantErr: ErrDuplicateName,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			var include []string
			for name, content := range tc.docs {
				writePackDir(t, root, name, map[string]string{"SKILL.md": content})
				include = append(include, name)
			}
			_, err := New(context.Background(), []config.BootAgentPackConfig{
				declaration(t, "acme", "agent", root, include...),
			}, testDeps(t, nil))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("New: err=%v, want errors.Is(err, %v)", err, tc.wantErr)
			}
		})
	}
}

// TestNew_DuplicateIncludePath pins the loader's own normalized-path
// dedup: a hand-built config that repeats an include must fail loud
// even though config validation (which would have rejected it) was
// skipped.
func TestNew_DuplicateIncludePath(t *testing.T) {
	root := t.TempDir()
	writePackDir(t, root, "pkg", map[string]string{"SKILL.md": validSkillMD("pkg")})
	_, err := New(context.Background(), []config.BootAgentPackConfig{
		declaration(t, "acme", "agent", root, "pkg", "pkg"),
	}, testDeps(t, nil))
	if !errors.Is(err, ErrDuplicateInclude) {
		t.Fatalf("New: err=%v, want ErrDuplicateInclude", err)
	}
}

// TestNew_RequiredToolsValidation pins the required-tools gate: a
// required_tools entry the static catalog cannot satisfy fails the
// load loud; required namespaces and tags are NOT validated in v1.28.
func TestNew_RequiredToolsValidation(t *testing.T) {
	root := t.TempDir()
	writePackDir(t, root, "pkg", map[string]string{
		"SKILL.md": `---
name: pkg
trigger: t
required_tools: [missing-tool]
required_namespaces: [ns-unknown]
required_tags: [tag-unknown]
---
## Steps
- s
`,
	})
	packs := []config.BootAgentPackConfig{declaration(t, "acme", "agent", root, "pkg")}

	// A catalog that does not know the declared tool rejects the load.
	_, err := New(context.Background(), packs, testDeps(t, staticCatalog{}))
	if !errors.Is(err, ErrRequiredTool) {
		t.Fatalf("New with incompatible catalog: err=%v, want ErrRequiredTool", err)
	}

	// A catalog that knows the tool accepts the load — the unknown
	// namespaces/tags are metadata and never validated.
	ix, err := New(context.Background(), packs, testDeps(t, staticCatalog{"missing-tool": true}))
	if err != nil {
		t.Fatalf("New with compatible catalog: %v", err)
	}
	entries, ok := ix.Lookup("acme", "agent")
	if !ok || len(entries) != 1 {
		t.Fatalf("Lookup = %d entries, ok=%v", len(entries), ok)
	}
	if got := entries[0].Skill.RequiredNS; len(got) != 1 || got[0] != "ns-unknown" {
		t.Fatalf("required namespaces were not preserved as metadata: %v", got)
	}
	if got := entries[0].Skill.RequiredTags; len(got) != 1 || got[0] != "tag-unknown" {
		t.Fatalf("required tags were not preserved as metadata: %v", got)
	}
}

// TestPathWithin pins the lexical containment predicate used as the
// last line of defense against an include escaping its declared
// directory.
func TestPathWithin(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "etc", "harbor", "skills")
	cases := []struct {
		p, root string
		want    bool
	}{
		{root, root, true},
		{filepath.Join(root, "pkg"), root, true},
		{filepath.Join(root, "a", "b"), root, true},
		{filepath.Join(root, "pkgx"), root, true}, // a file whose name extends a sibling dir prefix is still under root
		{filepath.Join(string(filepath.Separator), "etc", "harbor", "skills2"), root, false}, // sibling, not a boundary
		{filepath.Join(string(filepath.Separator), "etc", "harbor2", "skills"), root, false},
		{filepath.Join(string(filepath.Separator), "tmp", "x"), root, false},
	}
	for _, tc := range cases {
		if got := pathWithin(tc.p, tc.root); got != tc.want {
			t.Errorf("pathWithin(%q, %q) = %v, want %v", tc.p, tc.root, got, tc.want)
		}
	}
}

// TestReadFileStrict_PathSwap pins the swap gate at the helper level:
// the fstat of the opened file must be the same file the Lstat
// described.
func TestReadFileStrict_PathSwap(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.txt")
	b := filepath.Join(root, "b.txt")
	if err := os.WriteFile(a, []byte("aaaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("bbbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	ia, err := os.Lstat(a)
	if err != nil {
		t.Fatal(err)
	}
	ib, err := os.Lstat(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifySameFile(ia, ia, a); err != nil {
		t.Fatalf("verifySameFile(same) = %v, want nil", err)
	}
	if err := verifySameFile(ia, ib, a); !errors.Is(err, ErrPathSwap) {
		t.Fatalf("verifySameFile(different) = %v, want ErrPathSwap", err)
	}
}

// TestSetHash_FramingAndDeterminism pins the set-hash framing: it is
// deterministic for identical ordered pairs, sensitive to membership
// and to name/hash content, and — through buildBucket's canonical
// ordering — independent of caller-side input order.
func TestSetHash_FramingAndDeterminism(t *testing.T) {
	entry := func(name, hash string) Entry {
		return Entry{Skill: deepCopySkill(skills.Skill{Name: name}), SemanticHash: hash}
	}
	// Names that could collide under naive framing: NUL is legal in a
	// Go string and must not perturb the length-prefixed framing.
	weird := entry("a\x00b", "h1")
	normal := entry("a", "\x00b\x00h1")

	set := []Entry{weird, normal}
	a := buildBucket(set).setHash
	// Reversing the input order must not change the hash: buildBucket
	// canonicalizes order before setHash runs.
	b := buildBucket([]Entry{set[1], set[0]}).setHash
	if a != b {
		t.Fatalf("set hash not canonical-order-insensitive: %q vs %q", a, b)
	}
	if a == buildBucket([]Entry{weird}).setHash {
		t.Fatal("set hash lost membership sensitivity")
	}
	if a == buildBucket([]Entry{normal, entry("a\x00b", "h2")}).setHash {
		t.Fatal("set hash lost content sensitivity")
	}
}
