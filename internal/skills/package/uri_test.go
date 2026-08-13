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
	u, err := skillpkg.NewURI(h, "demo-skill")
	if err != nil {
		t.Fatalf("NewURI: %v", err)
	}
	if u.String() != "skillpkg:"+h+"/demo-skill" {
		t.Fatalf("URI string = %q", u.String())
	}
	parsed, err := skillpkg.ParseURI(u.String())
	if err != nil {
		t.Fatalf("ParseURI: %v", err)
	}
	if parsed != u {
		t.Fatalf("ParseURI round-trip mismatch: %+v vs %+v", parsed, u)
	}
	// Hash-only form.
	noName, err := skillpkg.NewURI(h, "")
	if err != nil {
		t.Fatalf("NewURI: %v", err)
	}
	if noName.String() != "skillpkg:"+h {
		t.Fatalf("hash-only URI = %q", noName.String())
	}
	if _, err := skillpkg.ParseURI(noName.String()); err != nil {
		t.Fatalf("ParseURI(hash-only): %v", err)
	}
}

func TestURI_ResolverNeutral(t *testing.T) {
	// The URI must carry no tenant / user / session / resolver
	// authority / authorization material — only hash + optional name.
	u, err := skillpkg.NewURI("v1:"+strings.Repeat("a", 64), "demo")
	if err != nil {
		t.Fatalf("NewURI: %v", err)
	}
	s := u.String()
	if strings.Contains(s, "//") {
		t.Fatalf("URI %q has an authority component (resolver-neutrality violated)", s)
	}
	for _, forbidden := range []string{"tenant", "user", "session", "token", "auth", "bearer"} {
		if strings.Contains(strings.ToLower(s), forbidden) {
			t.Fatalf("URI %q carries %q material", s, forbidden)
		}
	}
}

func TestURI_ParseRejects(t *testing.T) {
	good := "v1:" + strings.Repeat("a", 64)
	cases := []struct {
		name string
		in   string
	}{
		{"wrong scheme", "other:" + good},
		{"empty", ""},
		{"no hash", "skillpkg:"},
		{"hash not versioned", "skillpkg:abcdef"},
		{"hash short hex", "skillpkg:v1:" + strings.Repeat("a", 63)},
		{"hash uppercase hex", "skillpkg:v1:" + strings.Repeat("A", 64)},
		{"hash bad version", "skillpkg:x1:" + strings.Repeat("a", 64)},
		{"name empty after slash", "skillpkg:" + good + "/"},
		{"name double slash", "skillpkg:" + good + "/a/b"},
		{"name uppercase", "skillpkg:" + good + "/Demo"},
		{"name bad char", "skillpkg:" + good + "/demo!"},
		{"name non-ascii", "skillpkg:" + good + "/démø"},
		{"name too long", "skillpkg:" + good + "/" + strings.Repeat("n", skillpkg.MaxURINameRunes+1)},
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

func TestURI_BoundedLength(t *testing.T) {
	// The whole URI is bounded; a maximal hash + maximal name stays
	// within the bound.
	h := "v1:" + strings.Repeat("a", 64)
	name := strings.Repeat("n", skillpkg.MaxURINameRunes)
	u, err := skillpkg.ParseURI(fmt.Sprintf("skillpkg:%s/%s", h, name))
	if err != nil {
		t.Fatalf("ParseURI(maximal): %v", err)
	}
	if len([]rune(u.String())) > skillpkg.MaxURIRunes {
		t.Fatalf("maximal URI %q exceeds MaxURIRunes", u.String())
	}
}
