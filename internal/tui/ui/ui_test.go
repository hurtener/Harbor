package ui

import (
	"strings"
	"testing"
)

func TestMeasure_ExactResponsiveTransitions(t *testing.T) {
	for _, tc := range []struct {
		width              int
		horizontal, joined bool
		sidebar            int
	}{{79, false, false, 0}, {80, true, false, 0}, {120, true, false, 0}, {121, true, true, 42}} {
		got := Measure(tc.width, 24)
		if got.HorizontalActions != tc.horizontal || got.JoinedSidebar != tc.joined || got.SidebarWidth != tc.sidebar {
			t.Fatalf("width %d: %#v", tc.width, got)
		}
	}
	for _, size := range [][2]int{{40, 12}, {240, 60}} {
		got := Measure(size[0], size[1])
		if got.MainWidth < 1 || got.ContentHeight < 1 || got.ContentWidth > got.Width {
			t.Fatalf("size %v: %#v", size, got)
		}
	}
}

func TestTheme_AllModesPreserveSemanticDistinctions(t *testing.T) {
	for _, mode := range []Mode{ModeDark, ModeLight} {
		for _, profile := range []Profile{ProfileTrueColor, ProfileANSI256, ProfileANSI16} {
			theme := NewTheme(mode, profile)
			values := map[string]struct{}{}
			for _, role := range []Role{RoleError, RoleWarning, RoleSuccess, RoleInfo} {
				value := theme.SemanticValue(role)
				if value == "" {
					t.Fatalf("mode=%d profile=%d role=%d empty", mode, profile, role)
				}
				values[value] = struct{}{}
			}
			if len(values) < 3 {
				t.Fatalf("semantic colors collapsed: %#v", values)
			}
		}
	}
	mono := NewTheme(ModeDark, ProfileMono)
	if mono.Style(RoleError, nil).GetBold() == mono.Style(RoleWarning, nil).GetBold() && mono.Style(RoleError, nil).GetUnderline() == mono.Style(RoleWarning, nil).GetUnderline() {
		t.Fatal("monochrome semantic attributes collapsed")
	}
}

func TestCompileTheme_EnvironmentProfilesNoColorAndBackground(t *testing.T) {
	cases := []struct {
		environment Environment
		profile     Profile
		mode        Mode
	}{{Environment{TERM: "xterm-256color", COLORTERM: "truecolor"}, ProfileTrueColor, ModeDark}, {Environment{TERM: "xterm-256color", Light: true}, ProfileANSI256, ModeLight}, {Environment{TERM: "xterm"}, ProfileANSI16, ModeDark}, {Environment{TERM: "xterm-256color", NoColor: true}, ProfileMono, ModeDark}, {Environment{TERM: "dumb"}, ProfileMono, ModeDark}}
	for _, tc := range cases {
		theme := CompileTheme(tc.environment)
		if theme.Profile() != tc.profile || theme.Mode() != tc.mode {
			t.Fatalf("%#v -> %d/%d", tc.environment, theme.Profile(), theme.Mode())
		}
	}
	values := map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor", "NO_COLOR": "", "COLORFGBG": "0;15"}
	environment := EnvironmentFrom(func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	if !environment.NoColor || !environment.Light {
		t.Fatalf("environment=%#v", environment)
	}
}

func TestWidth_UnicodeCorpusAndLongIdentifiers(t *testing.T) {
	cases := map[string]int{"界": 2, "e\u0301": 1, "🙂": 2, "✈️": 2, "👩‍👩‍👧‍👦": 2, "01K0HARBORLONGIDENTIFIER000000000000": 36}
	for value, want := range cases {
		if got := Width(value); got != want {
			t.Errorf("Width(%q)=%d want %d", value, got, want)
		}
	}
	line := PadRight("界e\u0301🙂", 12)
	if Width(line) != 12 {
		t.Fatalf("padded width=%d %q", Width(line), line)
	}
}

func TestClusters_CombiningZWJVariationAndCJKStayAtomic(t *testing.T) {
	values := []string{"界", "e\u0301", "👩‍👩‍👧‍👦", "✈️"}
	for _, value := range values {
		clusters := Clusters(value)
		if len(clusters) != 1 || clusters[0].Text != value || clusters[0].Width != Width(value) {
			t.Fatalf("Clusters(%q)=%#v", value, clusters)
		}
	}
}

func TestComponents_ExactGeometryAndSemanticStyles(t *testing.T) {
	theme := NewTheme(ModeDark, ProfileTrueColor)
	card := HeavyCard(theme, RoleWarning, 30, "approval required")
	for _, line := range strings.Split(card, "\n") {
		if Width(line) != 30 {
			t.Fatalf("card width=%d line=%q", Width(line), line)
		}
	}
	composer := Composer(theme, 40, "fixture", "tenant/user/session")
	lines := strings.Split(composer, "\n")
	if len(lines) != 5 {
		t.Fatalf("composer rows=%d", len(lines))
	}
	for _, line := range lines {
		if Width(line) != 40 {
			t.Fatalf("composer width=%d line=%q", Width(line), line)
		}
	}
	l := Measure(240, 60)
	w, h := ComposerGeometry(l, 20)
	if w != 168 || h != 24 {
		t.Fatalf("dense composer=%dx%d", w, h)
	}
	w, h, top := DialogGeometry(Measure(40, 12), DialogXLarge, 30)
	if w != 38 || h != 10 || top != 2 {
		t.Fatalf("dialog=%dx%d@%d", w, h, top)
	}
	if theme.Profile() != ProfileTrueColor || theme.Mode() != ModeDark {
		t.Fatal("theme identity changed")
	}
	if Truncate("x", 0) != "" {
		t.Fatal("zero truncate")
	}
	if !strings.Contains(DescribeLayout(Measure(80, 24)), "actions=horizontal") {
		t.Fatal("layout description")
	}
	smallWidth, _ := ComposerGeometry(Measure(40, 12), 1)
	if smallWidth != 36 {
		t.Fatalf("small composer width=%d", smallWidth)
	}
}

func TestComposer_AllFoundationStatesAndMonoRoleAttributes(t *testing.T) {
	theme := NewTheme(ModeDark, ProfileMono)
	for _, state := range []string{"idle", "focused", "disabled", "running", "retry 2/3 · 4s", "attachment · report.pdf"} {
		composer := Composer(theme, 40, state, "tenant/user/session")
		if len(strings.Split(composer, "\n")) != 5 || Width(strings.Split(composer, "\n")[0]) != 40 {
			t.Fatalf("state %q composer geometry", state)
		}
	}
	for _, role := range []Role{RoleError, RoleWarning, RoleSuccess, RoleInfo, RolePrimary, RoleAccent, RoleMuted, RoleText} {
		rendered := theme.Style(role, nil).Render("x")
		if rendered == "" {
			t.Fatalf("role %d empty", role)
		}
	}
	colored := NewTheme(ModeDark, ProfileTrueColor).Style(RoleText, ptrTestRole(RolePanel))
	if colored.GetBackground() == nil {
		t.Fatal("semantic background missing")
	}
}
func ptrTestRole(role Role) *Role { return &role }
