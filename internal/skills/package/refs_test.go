package skillpkg_test

import (
	"strings"
	"testing"

	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

// refKey renders one SupportRef in a stable string form for
// assertions: "L:dest" (link), "I:dest" (image), "D:dest"
// (definition).
func refKey(r skillpkg.SupportRef) string {
	switch {
	case r.InDefinition:
		return "D:" + r.Dest
	case r.Kind == skillpkg.SupportRefImage:
		return "I:" + r.Dest
	default:
		return "L:" + r.Dest
	}
}

func refKeys(t *testing.T, body string) []string {
	t.Helper()
	refs := skillpkg.ScanSupportRefs(body)
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if r.Start != r.End {
			// Every span-bearing ref's span must slice back to its
			// exact destination text (the rewrite target).
			if body[r.Start:r.End] != r.Dest {
				t.Fatalf("span [%d,%d) = %q, dest %q", r.Start, r.End, body[r.Start:r.End], r.Dest)
			}
		}
		out = append(out, refKey(r))
	}
	return out
}

func TestScanSupportRefs_Inline(t *testing.T) {
	body := "See [the guide](docs/guide.md) and ![diagram](assets/logo.png)."
	got := refKeys(t, body)
	want := []string{"L:docs/guide.md", "I:assets/logo.png"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("refs = %v, want %v", got, want)
	}
}

func TestScanSupportRefs_ReferenceStyle(t *testing.T) {
	body := "Refs: [guide][g] and ![logo][l].\n\n[g]: docs/guide.md\n[l]: assets/logo.png\n"
	got := refKeys(t, body)
	want := []string{"L:docs/guide.md", "I:assets/logo.png", "D:docs/guide.md", "D:assets/logo.png"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("refs = %v, want %v", got, want)
	}
}

func TestScanSupportRefs_CollapsedAndShortcut(t *testing.T) {
	body := "Collapsed [guide][] and ![logo][]. Shortcut [guide] and ![logo].\n\n" +
		"[guide]: docs/guide.md\n[logo]: assets/logo.png\n"
	got := refKeys(t, body)
	want := []string{
		"L:docs/guide.md", "I:assets/logo.png", // collapsed usages
		"L:docs/guide.md", "I:assets/logo.png", // shortcut usages
		"D:docs/guide.md", "D:assets/logo.png",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("refs = %v, want %v", got, want)
	}
}

func TestScanSupportRefs_UndefinedLabelsNotEmitted(t *testing.T) {
	// An undefined reference label renders literally in Markdown — it
	// is not a support reference.
	body := "Undefined [nope][missing] and ![also][gone] and plain [text]."
	if got := refKeys(t, body); len(got) != 0 {
		t.Fatalf("undefined labels emitted refs: %v", got)
	}
}

func TestScanSupportRefs_LabelFold(t *testing.T) {
	body := "See [Guide][GUIDE] and ![Logo][ logo ].\n\n[guide]: docs/guide.md\n[LOGO]: assets/logo.png\n"
	got := refKeys(t, body)
	want := []string{"L:docs/guide.md", "I:assets/logo.png", "D:docs/guide.md", "D:assets/logo.png"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("refs = %v, want %v", got, want)
	}
}

func TestScanSupportRefs_DefinitionForms(t *testing.T) {
	body := "[a]: <assets/logo.png>\n[b]: docs/guide.md \"the guide\"\n[c]:\n[d]: docs/usage.txt\n"
	got := refKeys(t, body)
	want := []string{"D:assets/logo.png", "D:docs/guide.md", "D:", "D:docs/usage.txt"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("refs = %v, want %v", got, want)
	}
}

func TestScanSupportRefs_FencedCodeSkipped(t *testing.T) {
	body := "```\n![fake](assets/logo.png) [fake2](docs/guide.md)\n```\n" +
		"Real ![diagram](assets/logo.png).\n\n" +
		"~~~\n![fake3](assets/logo.png)\n~~~\n"
	got := refKeys(t, body)
	want := []string{"I:assets/logo.png"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("refs = %v, want %v", got, want)
	}
}

// TestScanSupportRefs_FencedDefinitionsSkipped pins the P1 fence-skip
// closure for reference DEFINITIONS: a `[label]: dest` line inside a
// backtick or tilde fence is example text, not a real definition. It
// is not emitted, it does not resolve usages, and it does not
// participate in the last-wins label dedup — a real same-label
// definition outside the fence always wins, whether the fenced line
// precedes or follows it.
func TestScanSupportRefs_FencedDefinitionsSkipped(t *testing.T) {
	// Fenced definition AFTER a real definition: the fenced line must
	// not override the real dest for usages.
	body := "Real [logo]: assets/logo.png, usage ![logo][logo].\n\n" +
		"[logo]: assets/logo.png\n\n" +
		"```\n[logo]: assets/fake.png\n```\n"
	got := refKeys(t, body)
	want := []string{"I:assets/logo.png", "D:assets/logo.png"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("after-real refs = %v, want %v", got, want)
	}

	// Fenced definition BEFORE a real definition (backtick), and a
	// tilde-fenced same-label line: only the real definition is
	// scanned, and the usage resolves to it.
	body2 := "```\n[guide]: docs/fake.md\n[guide]: docs/fake2.md\n```\n" +
		"Real [guide][guide].\n\n" +
		"[guide]: docs/guide.md\n\n" +
		"~~~\n[guide]: docs/fake3.md\n~~~\n"
	got2 := refKeys(t, body2)
	want2 := []string{"L:docs/guide.md", "D:docs/guide.md"}
	if strings.Join(got2, " ") != strings.Join(want2, " ") {
		t.Fatalf("before-real refs = %v, want %v", got2, want2)
	}

	// A fence containing ONLY definitions emits nothing.
	body3 := "```\n[a]: assets/a.png\n[b]: assets/b.png\n~~~\n[c]: assets/c.png\n```\n"
	if got3 := refKeys(t, body3); len(got3) != 0 {
		t.Fatalf("fence-only definitions emitted refs: %v", got3)
	}
}

func TestScanSupportRefs_UnterminatedFenceClosesAtEOF(t *testing.T) {
	body := "```\n![fake](assets/logo.png)\n"
	if got := refKeys(t, body); len(got) != 0 {
		t.Fatalf("unterminated fence leaked refs: %v", got)
	}
}

func TestSplitDest(t *testing.T) {
	cases := []struct {
		name     string
		dest     string
		wantKind skillpkg.DestKind
		wantPath string
		wantFrag string
	}{
		{"relative", "docs/guide.md", skillpkg.DestRelative, "docs/guide.md", ""},
		{"relative fragment", "docs/guide.md#sec", skillpkg.DestRelative, "docs/guide.md", "sec"},
		{"nested relative fragment", "a/b/c.txt#x-y", skillpkg.DestRelative, "a/b/c.txt", "x-y"},
		{"scheme http", "https://example.com/x", skillpkg.DestScheme, "", ""},
		{"scheme data", "data:text/plain;base64,Zm9v", skillpkg.DestScheme, "", ""},
		{"scheme artifact", "artifact://abc_123", skillpkg.DestScheme, "", ""},
		{"drive-like colon", "assets/a:b.txt", skillpkg.DestRelative, "assets/a:b.txt", ""},
		{"absolute", "/etc/passwd", skillpkg.DestAbsolute, "", ""},
		{"fragment only", "#overview", skillpkg.DestFragment, "", "overview"},
		{"backslash", `a\b.txt`, skillpkg.DestAmbiguous, "", ""},
		{"query", "docs/guide.md?v=2", skillpkg.DestAmbiguous, "", ""},
		{"empty", "", skillpkg.DestAmbiguous, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, path, frag := skillpkg.SplitDest(c.dest)
			if kind != c.wantKind || path != c.wantPath || frag != c.wantFrag {
				t.Fatalf("SplitDest(%q) = (%v, %q, %q), want (%v, %q, %q)",
					c.dest, kind, path, frag, c.wantKind, c.wantPath, c.wantFrag)
			}
		})
	}
}

func TestCanonicalizeSupportDest(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "assets/logo.png", "assets/logo.png"},
		{"dot prefix", "./assets/logo.png", "assets/logo.png"},
		{"dot segment", "a/./b.txt", "a/b.txt"},
		{"parent collapse", "a/../b.txt", "b.txt"},
		{"deep", "examples/sub/demo.json", "examples/sub/demo.json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := skillpkg.CanonicalizeSupportDest(c.in)
			if err != nil {
				t.Fatalf("CanonicalizeSupportDest(%q): %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
	rejects := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"absolute", "/etc/passwd"},
		{"backslash", `a\b.txt`},
		{"root", "."},
		{"traversal", "../escape.png"},
		{"nested traversal", "a/../../escape.png"},
		{"space", "a b.png"},
		{"percent", "a%2Fb.png"},
		{"non-ascii", "éclair.png"},
		{"colon", "a:b.txt"},
	}
	for _, c := range rejects {
		t.Run("reject_"+c.name, func(t *testing.T) {
			if _, err := skillpkg.CanonicalizeSupportDest(c.in); err == nil {
				t.Fatalf("CanonicalizeSupportDest(%q): expected error", c.in)
			}
		})
	}
}
