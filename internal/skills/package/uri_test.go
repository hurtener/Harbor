package skillpkg_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

func TestURI_RoundTrip(t *testing.T) {
	p := testPackage()
	h, err := skillpkg.PackageHash(p)
	if err != nil {
		t.Fatalf("PackageHash: %v", err)
	}
	for _, path := range []string{"assets/logo.png", "examples/demo.json", "docs/usage.txt"} {
		u, err := skillpkg.NewURI(h, path)
		if err != nil {
			t.Fatalf("NewURI(%q): %v", path, err)
		}
		if u.String() != "skillpkg://"+h+"/"+path {
			t.Fatalf("URI string = %q", u.String())
		}
		parsed, err := skillpkg.ParseURI(u.String())
		if err != nil {
			t.Fatalf("ParseURI: %v", err)
		}
		if parsed != u {
			t.Fatalf("ParseURI round-trip mismatch: %+v vs %+v", parsed, u)
		}
	}
	// String is the exact inverse of ParseURI for valid URIs.
	u, err := skillpkg.NewURI(h, "assets/logo.png")
	if err != nil {
		t.Fatalf("NewURI: %v", err)
	}
	again, err := skillpkg.ParseURI(u.String())
	if err != nil {
		t.Fatalf("ParseURI: %v", err)
	}
	if again.String() != u.String() {
		t.Fatalf("second round-trip drifted: %q vs %q", again.String(), u.String())
	}
}

func TestURI_ResolverNeutral(t *testing.T) {
	// The authority position carries ONLY the versioned package hash:
	// no resolver host, no userinfo, no port, no query, no fragment,
	// no identity/token material. The `//` is the scheme's authority
	// delimiter, not a network authority.
	h := "v1:" + strings.Repeat("a", 64)
	u, err := skillpkg.NewURI(h, "assets/logo.png")
	if err != nil {
		t.Fatalf("NewURI: %v", err)
	}
	s := u.String()
	if !strings.HasPrefix(s, "skillpkg://"+h+"/") {
		t.Fatalf("URI %q does not carry the versioned hash verbatim in the authority position", s)
	}
	for _, forbidden := range []string{"@", "?", "#"} {
		if strings.Contains(s, forbidden) {
			t.Fatalf("URI %q carries %q material (resolver-neutrality violated)", s, forbidden)
		}
	}
	for _, forbidden := range []string{"tenant", "user", "session", "token", "auth", "bearer"} {
		if strings.Contains(strings.ToLower(s), forbidden) {
			t.Fatalf("URI %q carries %q material", s, forbidden)
		}
	}
}

func TestURI_ParseRejects(t *testing.T) {
	h := "v1:" + strings.Repeat("a", 64)
	cases := []struct {
		name string
		in   string
	}{
		// Scheme.
		{"alternate scheme", "skillpack://" + h + "/a.txt"},
		{"scheme case", "SkillPkg://" + h + "/a.txt"},
		{"missing scheme", h + "/a.txt"},
		{"empty", ""},
		{"only scheme", "skillpkg://"},
		// Hash / authority.
		{"missing hash", "skillpkg:///a.txt"},
		{"missing path", "skillpkg://" + h},
		{"hash not versioned", "skillpkg://abcdef/a.txt"},
		{"hash short hex", "skillpkg://v1:" + strings.Repeat("a", 63) + "/a.txt"},
		{"hash uppercase hex", "skillpkg://v1:" + strings.Repeat("A", 64) + "/a.txt"},
		{"hash bad version", "skillpkg://x1:" + strings.Repeat("a", 64) + "/a.txt"},
		{"hash extra colon", "skillpkg://v1:" + strings.Repeat("a", 64) + ":8080/a.txt"},
		{"userinfo", "skillpkg://user@" + h + "/a.txt"},
		{"identity token", "skillpkg://" + h + "/a.txt@evil"},
		{"query", "skillpkg://" + h + "/a.txt?x=1"},
		{"fragment", "skillpkg://" + h + "/a.txt#frag"},
		// Path shape.
		{"path empty after slash", "skillpkg://" + h + "/"},
		{"path empty segment", "skillpkg://" + h + "/a//b.txt"},
		{"path dot segment", "skillpkg://" + h + "/a/./b.txt"},
		{"path traversal", "skillpkg://" + h + "/../escape.txt"},
		{"path nested traversal", "skillpkg://" + h + "/a/../../escape.txt"},
		{"path backslash", "skillpkg://" + h + `/a\b.txt`},
		{"path absolute", "skillpkg://" + h + "//etc/passwd"},
		{"path non-ascii", "skillpkg://" + h + "/éclair.png"},
		{"path space", "skillpkg://" + h + "/a b.txt"},
		{"path colon", "skillpkg://" + h + "/a:b.txt"},
		{"path root skillmd", "skillpkg://" + h + "/SKILL.md"},
		// Percent-encoding hostility.
		{"malformed percent", "skillpkg://" + h + "/a%zz.txt"},
		{"truncated percent", "skillpkg://" + h + "/a%.txt"},
		{"ambiguous percent unreserved", "skillpkg://" + h + "/a%61.txt"},
		{"ambiguous percent dot", "skillpkg://" + h + "/a%2eb.txt"},
		{"encoded slash", "skillpkg://" + h + "/a%2Fb.txt"},
		{"encoded backslash", "skillpkg://" + h + "/a%5Cb.txt"},
		{"encoded dot-segment", "skillpkg://" + h + "/%2e%2e/escape.txt"},
		{"encoded percent", "skillpkg://" + h + "/a%25b.txt"},
		{"encoded non-ascii", "skillpkg://" + h + "/%C3%A9.txt"},
		{"encoded nul", "skillpkg://" + h + "/a%00b.txt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := skillpkg.ParseURI(c.in)
			if err == nil {
				t.Fatalf("ParseURI(%q): expected error", c.in)
			}
			if !errors.Is(err, skillpkg.ErrMalformedURI) && !errors.Is(err, skillpkg.ErrURITooLong) {
				t.Fatalf("ParseURI(%q): error %v does not wrap a URI sentinel", c.in, err)
			}
		})
	}
}

func TestURI_HostileRoundTrip(t *testing.T) {
	// Hostile round-trip: Parse(String(u)) must return u for valid
	// URIs, and any hand-mangled variant must be rejected rather than
	// silently renormalized into a different URI.
	h := "v1:" + strings.Repeat("a", 64)
	u, err := skillpkg.NewURI(h, "assets/logo.png")
	if err != nil {
		t.Fatalf("NewURI: %v", err)
	}
	canonical := u.String()
	if parsed, err := skillpkg.ParseURI(canonical); err != nil || parsed != u {
		t.Fatalf("canonical round-trip: %+v err=%v", parsed, err)
	}
	// Every mutation of the canonical form must fail to parse (no
	// two strings may parse to the same URI — the parser accepts only
	// the canonical form).
	variants := []string{
		strings.ToUpper(canonical),
		strings.Replace(canonical, "skillpkg://", "skillpkg://.", 1),
		canonical + "/",
		canonical + "/.",
		canonical + "//",
	}
	for _, v := range variants {
		if parsed, err := skillpkg.ParseURI(v); err == nil && parsed == u {
			t.Fatalf("hostile variant %q round-tripped to the canonical URI", v)
		}
	}
}

func TestURI_NewURIRejects(t *testing.T) {
	h := "v1:" + strings.Repeat("a", 64)
	cases := []struct {
		name string
		hash string
		path string
	}{
		{"bad hash", "abcdef", "a.txt"},
		{"short hash", "v1:" + strings.Repeat("a", 63), "a.txt"},
		{"traversal path", h, "../escape.txt"},
		{"absolute path", h, "/etc/passwd"},
		{"backslash path", h, `a\b.txt`},
		{"dot segment", h, "a/./b.txt"},
		{"non-ascii path", h, "éclair.png"},
		{"root skillmd path", h, skillpkg.RootSkillFileName},
		{"empty path", h, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := skillpkg.NewURI(c.hash, c.path); !errors.Is(err, skillpkg.ErrMalformedURI) {
				t.Fatalf("NewURI(%q, %q): err=%v, want ErrMalformedURI", c.hash, c.path, err)
			}
		})
	}
}

func TestURI_BoundedLength(t *testing.T) {
	// The whole URI is bounded; a maximal hash + maximal canonical
	// path (two maximal per-segment runs) stays within the bound, and
	// one rune over is rejected.
	h := "v1:" + strings.Repeat("a", 64)
	prefixLen := len("skillpkg://") + len(h) + 1
	seg1 := strings.Repeat("n", skillpkg.MaxPackagePathSegmentRunes)
	seg2Len := skillpkg.MaxURIRunes - prefixLen - len(seg1) - 1 // -1 for the '/'
	path := seg1 + "/" + strings.Repeat("m", seg2Len)
	u, err := skillpkg.ParseURI(fmt.Sprintf("skillpkg://%s/%s", h, path))
	if err != nil {
		t.Fatalf("ParseURI(maximal): %v", err)
	}
	if len([]rune(u.String())) != skillpkg.MaxURIRunes {
		t.Fatalf("maximal URI length = %d, want %d", len([]rune(u.String())), skillpkg.MaxURIRunes)
	}
	over := seg1 + "/" + strings.Repeat("m", seg2Len+1)
	if _, err := skillpkg.ParseURI(fmt.Sprintf("skillpkg://%s/%s", h, over)); !errors.Is(err, skillpkg.ErrURITooLong) {
		t.Fatalf("over-long URI: err=%v, want ErrURITooLong", err)
	}
}
