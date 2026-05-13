package importer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
)

func newInmemStore(t *testing.T) artifacts.ArtifactStore {
	t.Helper()
	s, err := inmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func TestScanFrontmatter_HappyPath(t *testing.T) {
	src := []byte("---\nname: x\ntrigger: y\n---\nbody\n")
	fm, body, err := scanFrontmatter(src)
	if err != nil {
		t.Fatalf("scanFrontmatter: %v", err)
	}
	if string(fm.Bytes) != "name: x\ntrigger: y\n" {
		t.Errorf("frontmatter raw = %q, want %q", fm.Bytes, "name: x\ntrigger: y\n")
	}
	if string(body) != "body\n" {
		t.Errorf("body = %q, want %q", body, "body\n")
	}
}

func TestScanFrontmatter_MissingOpen(t *testing.T) {
	_, _, err := scanFrontmatter([]byte("no frontmatter here\n"))
	if !errors.Is(err, ErrMissingFrontmatter) {
		t.Errorf("err = %v, want ErrMissingFrontmatter", err)
	}
}

func TestScanFrontmatter_MissingClose(t *testing.T) {
	_, _, err := scanFrontmatter([]byte("---\nname: x\ntrigger: y\nbody without closing fence\n"))
	if !errors.Is(err, ErrMalformedYAML) {
		t.Errorf("err = %v, want ErrMalformedYAML", err)
	}
}

func TestScanFrontmatter_FenceAtEOF(t *testing.T) {
	src := []byte("---\nname: x\n---")
	fm, body, err := scanFrontmatter(src)
	if err != nil {
		t.Fatalf("scanFrontmatter: %v", err)
	}
	if string(fm.Bytes) != "name: x\n" {
		t.Errorf("fm = %q", fm.Bytes)
	}
	if len(body) != 0 {
		t.Errorf("body = %q, want empty", body)
	}
}

func TestClassifySection(t *testing.T) {
	cases := []struct {
		in   string
		want canonicalSection
	}{
		{"## Steps", sectionSteps},
		{"## steps", sectionSteps},
		{"## Step", sectionSteps},
		{"## STEPS", sectionSteps},
		{"## Steps:", sectionSteps},
		{"## Preconditions", sectionPreconditions},
		{"## precondition", sectionPreconditions},
		{"## Failure modes", sectionFailureModes},
		{"## failure mode", sectionFailureModes},
		{"## Failure Modes:", sectionFailureModes},
		{"## Examples", sectionUnknown},
		{"## ", sectionUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := classifySection(tc.in); got != tc.want {
				t.Errorf("classifySection(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSlugify(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Foo Bar", "foo-bar"},
		{"foo--bar", "foo-bar"},
		{"foo_bar_baz", "foo-bar-baz"},
		{"foo.skill", "foo-skill"},
		{"Already-good", "already-good"},
		{"", ""},
		{"---", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := slugify(tc.in); got != tc.want {
				t.Errorf("slugify(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNameFallbackFromHint(t *testing.T) {
	if got := nameFallbackFromHint("skills/foo-bar.md"); got != "foo-bar" {
		t.Errorf("got %q", got)
	}
	if got := nameFallbackFromHint(""); got != "" {
		t.Errorf("empty hint = %q", got)
	}
}

func TestParseFrontmatter_HappyPath(t *testing.T) {
	raw := []byte("name: x\ntrigger: y\ntask_type: code\n")
	f, err := parseFrontmatter(raw)
	if err != nil {
		t.Fatalf("parseFrontmatter: %v", err)
	}
	if f.Name != "x" || f.Trigger != "y" || f.TaskType != "code" {
		t.Errorf("fields = %+v", f)
	}
}

func TestParseFrontmatter_Malformed(t *testing.T) {
	_, err := parseFrontmatter([]byte("name: : :\ntrigger: ?"))
	if !errors.Is(err, ErrMalformedYAML) {
		t.Errorf("err = %v, want ErrMalformedYAML", err)
	}
}

func TestStripTrailingBlankLines(t *testing.T) {
	cases := []struct{ in, want string }{
		{"abc", "abc"},
		{"abc\n", "abc"},
		{"abc\n\n", "abc"},
		{"abc\n\n\n", "abc"},
		{"\nabc\n", "\nabc"},
	}
	for _, tc := range cases {
		got := stripTrailingBlankLines(tc.in)
		if got != tc.want {
			t.Errorf("stripTrailingBlankLines(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHasSchemeOrAbs(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"foo/bar.txt", false},
		{"./foo.txt", false},
		{"/abs/path", true},
		{"http://example.com", true},
		{"https://example.com", true},
		{"data:text/plain;base64,Zm9v", true},
		{"artifact://abc_def", true},
	}
	for _, tc := range cases {
		got := hasSchemeOrAbs(tc.in)
		if got != tc.want {
			t.Errorf("hasSchemeOrAbs(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestBodyParse_UnknownSection(t *testing.T) {
	store := newInmemStore(t)
	src := ImportSource{}
	body := []byte("desc\n\n## Examples\n\n- something\n")
	_, _, _, err := bodyParse(context.Background(), store, src, body)
	if !errors.Is(err, ErrUnknownSection) {
		t.Errorf("err = %v, want ErrUnknownSection", err)
	}
}

func TestBodyParse_DuplicateSection(t *testing.T) {
	store := newInmemStore(t)
	src := ImportSource{}
	body := []byte("desc\n\n## Steps\n\n- one\n\n## Steps\n\n- two\n")
	_, _, _, err := bodyParse(context.Background(), store, src, body)
	if !errors.Is(err, ErrUnknownSection) {
		t.Errorf("err = %v, want ErrUnknownSection on duplicate", err)
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("err message should mention duplicate: %v", err)
	}
}

func TestBodyParse_NonListProseInSection(t *testing.T) {
	store := newInmemStore(t)
	src := ImportSource{}
	body := []byte("desc\n\n## Steps\n\nthis is prose not a list item\n")
	_, _, _, err := bodyParse(context.Background(), store, src, body)
	if !errors.Is(err, ErrUnknownSection) {
		t.Errorf("err = %v, want ErrUnknownSection on prose-in-section", err)
	}
}

func TestSplitLinesKeepEmpty(t *testing.T) {
	cases := []struct {
		in   []byte
		want []string
	}{
		{[]byte("a\nb\n"), []string{"a\n", "b\n"}},
		{[]byte("a\nb"), []string{"a\n", "b"}},
		{[]byte(""), nil},
		{[]byte("\n"), []string{"\n"}},
	}
	for _, tc := range cases {
		got := splitLinesKeepEmpty(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitLinesKeepEmpty(%q) len = %d, want %d", tc.in, len(got), len(tc.want))
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitLinesKeepEmpty(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}
