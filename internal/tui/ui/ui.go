// Package ui defines Harbor's terminal design tokens and geometry primitives.
package ui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const (
	// OuterPadding is the horizontal canvas inset.
	OuterPadding = 2
	// SidebarWidth is the fixed sidebar width.
	SidebarWidth = 42
	// ActionBreakpoint is the first width with horizontal actions.
	ActionBreakpoint = 80
	// SidebarBreakpoint is the final width with an overlay sidebar.
	SidebarBreakpoint = 120
	// HomeComposerWidth is the minimum wide-terminal composer cap.
	HomeComposerWidth = 75
	// DialogMedium is the medium dialog cap.
	DialogMedium = 60
	// DialogLarge is the large dialog cap.
	DialogLarge = 88
	// DialogXLarge is the extra-large dialog cap.
	DialogXLarge = 116
	// AutocompleteRows is the maximum autocomplete height.
	AutocompleteRows = 10
)

// Profile identifies the terminal color capability.
type Profile uint8

const (
	ProfileTrueColor Profile = iota
	ProfileANSI256
	ProfileANSI16
	ProfileMono
)

// Mode identifies a dark or light terminal background.
type Mode uint8

const (
	ModeDark Mode = iota
	ModeLight
)

// Role names a semantic color; components never choose literal colors.
type Role uint8

const (
	RolePrimary Role = iota
	RoleSecondary
	RoleAccent
	RoleError
	RoleWarning
	RoleSuccess
	RoleInfo
	RoleText
	RoleMuted
	RoleCanvas
	RolePanel
	RoleElement
	RoleBorder
)

// Theme is an immutable compiled semantic palette.
type Theme struct {
	profile Profile
	mode    Mode
	colors  [13]string
}

// Environment describes terminal capabilities used to compile a theme.
type Environment struct {
	TERM, COLORTERM string
	NoColor, Light  bool
}

// EnvironmentFrom reads terminal capability variables. NO_COLOR is enabled
// by presence, including an explicitly empty value.
func EnvironmentFrom(lookup func(string) (string, bool)) Environment {
	term, _ := lookup("TERM")
	colorTerm, _ := lookup("COLORTERM")
	_, noColor := lookup("NO_COLOR")
	background, _ := lookup("COLORFGBG")
	theme, _ := lookup("HARBOR_TUI_THEME")
	return Environment{TERM: term, COLORTERM: colorTerm, NoColor: noColor, Light: strings.HasSuffix(background, ";15") || strings.EqualFold(theme, "light")}
}

// CompileTheme selects profile and background mode from terminal capabilities.
func CompileTheme(environment Environment) Theme {
	mode := ModeDark
	if environment.Light {
		mode = ModeLight
	}
	if environment.NoColor || environment.TERM == "dumb" {
		return NewTheme(mode, ProfileMono)
	}
	colorTerm := strings.ToLower(environment.COLORTERM)
	if strings.Contains(colorTerm, "truecolor") || strings.Contains(colorTerm, "24bit") {
		return NewTheme(mode, ProfileTrueColor)
	}
	term := strings.ToLower(environment.TERM)
	if strings.Contains(term, "256color") {
		return NewTheme(mode, ProfileANSI256)
	}
	return NewTheme(mode, ProfileANSI16)
}

var dark = [13]string{"#fab283", "#5c9cf5", "#9d7cd8", "#e06c75", "#f5a742", "#7fd88f", "#56b6c2", "#eeeeee", "#808080", "#0a0a0a", "#141414", "#1e1e1e", "#484848"}
var light = [13]string{"#3b7dd8", "#7b5bb6", "#d68c27", "#d1383d", "#d68c27", "#3d9a57", "#318795", "#1a1a1a", "#8a8a8a", "#ffffff", "#fafafa", "#f5f5f5", "#b8b8b8"}
var ansi256Dark = [13]string{"216", "75", "140", "168", "214", "114", "73", "255", "244", "232", "233", "234", "239"}
var ansi256Light = [13]string{"68", "97", "172", "160", "172", "71", "66", "234", "245", "231", "255", "254", "250"}
var ansi16Dark = [13]string{"11", "12", "13", "9", "11", "10", "14", "15", "8", "0", "0", "8", "8"}
var ansi16Light = [13]string{"4", "5", "3", "1", "3", "2", "6", "0", "8", "15", "15", "7", "8"}

// NewTheme compiles one terminal palette.
func NewTheme(mode Mode, profile Profile) Theme {
	var colors [13]string
	switch profile {
	case ProfileTrueColor:
		colors = dark
		if mode == ModeLight {
			colors = light
		}
	case ProfileANSI256:
		colors = ansi256Dark
		if mode == ModeLight {
			colors = ansi256Light
		}
	case ProfileANSI16:
		colors = ansi16Dark
		if mode == ModeLight {
			colors = ansi16Light
		}
	case ProfileMono:
		colors = [13]string{}
	}
	return Theme{profile: profile, mode: mode, colors: colors}
}

// Profile returns the compiled terminal capability.
func (t Theme) Profile() Profile { return t.profile }

// Mode returns the compiled background mode.
func (t Theme) Mode() Mode { return t.mode }

// Style returns a style for a semantic foreground/background pair.
func (t Theme) Style(foreground Role, background *Role) lipgloss.Style {
	s := lipgloss.NewStyle()
	if t.profile == ProfileMono {
		switch foreground {
		case RoleError:
			return s.Bold(true).Underline(true)
		case RoleWarning:
			return s.Underline(true)
		case RoleSuccess:
			return s.Bold(true)
		case RoleInfo:
			return s.Faint(true)
		case RolePrimary, RoleAccent:
			return s.Bold(true)
		case RoleMuted:
			return s.Faint(true)
		}
		return s
	}
	s = s.Foreground(lipgloss.Color(t.colors[foreground]))
	if background != nil {
		s = s.Background(lipgloss.Color(t.colors[*background]))
	}
	return s
}

// SemanticValue exposes a compiled role for style conformance tests.
func (t Theme) SemanticValue(role Role) string { return t.colors[role] }

// Width is Harbor's only visible-cell width implementation.
func Width(value string) int { return ansi.StringWidth(value) }

// Cluster is one grapheme and its visible width.
type Cluster struct {
	Text  string
	Width int
}

// Clusters splits plain text using x/ansi's grapheme-aware truncation. It
// deliberately shares the same width authority as every layout operation.
//
// The input MUST be plain text. On styled input ansi.Truncate returns a
// re-terminated string that is not a prefix of the source, so the scan could
// never advance; the prefix guard below consumes the remainder in one cluster
// instead of looping forever. Callers rendering styled content keep text and
// style separate (the canvas takes plain text plus one style per write).
func Clusters(value string) []Cluster {
	clusters := make([]Cluster, 0, len(value))
	for value != "" {
		var cluster string
		for cells := 1; cells <= 2 && cluster == ""; cells++ {
			cluster = ansi.Truncate(value, cells, "")
		}
		if cluster == "" || !strings.HasPrefix(value, cluster) {
			cluster = value
		}
		width := Width(cluster)
		if width == 0 {
			width = 1
		}
		clusters = append(clusters, Cluster{Text: cluster, Width: width})
		value = strings.TrimPrefix(value, cluster)
	}
	return clusters
}

// Truncate clips a string to visible cells.
func Truncate(value string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(value, width, "")
}

// PadRight clips and pads text to exactly width visible cells.
func PadRight(value string, width int) string {
	value = Truncate(value, width)
	return value + strings.Repeat(" ", max(0, width-Width(value)))
}

// Layout is the explicit pre-style root geometry.
type Layout struct {
	Width, Height                    int
	ContentWidth, ContentHeight      int
	MainWidth, SidebarWidth          int
	HorizontalActions, JoinedSidebar bool
}

// Measure calculates root geometry without styling.
func Measure(width, height int) Layout {
	width, height = max(1, width), max(1, height)
	contentWidth := max(1, width-OuterPadding*2)
	joined := width > SidebarBreakpoint
	sidebar := 0
	if joined {
		sidebar = min(SidebarWidth, max(0, contentWidth-1))
	}
	return Layout{Width: width, Height: height, ContentWidth: contentWidth, ContentHeight: max(1, height-1), MainWidth: max(1, contentWidth-sidebar), SidebarWidth: sidebar, HorizontalActions: width >= ActionBreakpoint, JoinedSidebar: joined}
}

// ComposerGeometry returns the capped composer width and viewport-derived height.
func ComposerGeometry(layout Layout, lines int) (int, int) {
	capWidth := min(layout.ContentWidth, max(HomeComposerWidth, layout.Width*7/10))
	if layout.Width <= SidebarBreakpoint {
		capWidth = min(layout.ContentWidth, HomeComposerWidth)
	}
	return max(1, capWidth), min(max(1, lines), max(6, layout.Height/3)) + 4
}

// DialogGeometry returns a fixed dialog's capped width, height, and top row.
func DialogGeometry(layout Layout, requestedWidth, rows int) (int, int, int) {
	width := min(requestedWidth, max(1, layout.Width-2))
	height := min(max(3, rows), max(3, layout.Height-2))
	return width, height, min(max(0, layout.Height/4), max(0, layout.Height-height))
}

// HeavyCard renders a selective heavy-left-rule card.
func HeavyCard(theme Theme, role Role, width int, lines ...string) string {
	contentWidth := max(1, width-3)
	rule := theme.Style(role, nil).Render("┃")
	panel := RolePanel
	rows := make([]string, 0, len(lines)+2)
	rows = append(rows, rule+theme.Style(RoleText, &panel).Render(PadRight("  ", width-1)))
	for _, line := range lines {
		rows = append(rows, rule+theme.Style(RoleText, &panel).Render("  "+PadRight(line, contentWidth)))
	}
	rows = append(rows, rule+theme.Style(RoleText, &panel).Render(PadRight("  ", width-1)))
	return strings.Join(rows, "\n")
}

// Composer renders the open-edge detached composer shell.
func Composer(theme Theme, width int, posture, identity string) string {
	return ComposerWithText(theme, width, posture, posture, identity, "")
}

// ComposerWithText renders the detached foundation composer.
func ComposerWithText(theme Theme, width int, posture, status, identity, draft string) string {
	width = max(8, width)
	inner := width - 3
	role := RolePrimary
	prompt := "Ask Harbor about this runtime..."
	switch {
	case strings.HasPrefix(posture, "idle"):
		// Idle is the pre-attach posture: the composer is not accepting input
		// yet. It reads as muted rather than relying on a trailing cursor glyph,
		// which truncates away on a narrow terminal.
		role = RoleMuted
		prompt = "Composer idle · attach a Runtime"
	case strings.HasPrefix(posture, "disabled"):
		role = RoleMuted
		prompt = "Composer unavailable"
	case strings.HasPrefix(posture, "running"):
		role = RoleWarning
		prompt = "Active work in progress"
	case strings.HasPrefix(posture, "retry"):
		role = RoleError
		prompt = "Submission failed · retry available"
	case strings.HasPrefix(posture, "attachment"):
		role = RoleInfo
		prompt = "Attachment ready · disposition applied at send"
	case strings.HasPrefix(posture, "focused"):
		prompt = "Ask Harbor about this runtime...  █"
	}
	if draft != "" {
		prompt = strings.ReplaceAll(draft, "\n", " ↵ ")
		prompt = Truncate(prompt, max(1, inner-2)) + " █"
	}
	border := theme.Style(role, nil).Render("┃")
	panel := RolePanel
	rows := []string{
		border + theme.Style(RoleMuted, &panel).Render("  "+PadRight(prompt, inner)),
		border + theme.Style(RoleText, &panel).Render("  "+PadRight("", inner)),
		border + theme.Style(RoleMuted, &panel).Render("  "+PadRight(status, inner)),
		theme.Style(role, nil).Render("╹") + theme.Style(RoleBorder, nil).Faint(true).Render(strings.Repeat("─", max(0, width-1))),
		theme.Style(RoleMuted, nil).Render(PadRight(identity, width)),
	}
	return strings.Join(rows, "\n")
}

// LiveComposer renders a wrapped operational draft with an exact cursor and
// inverse-styled selection. It contains no fabricated Runtime metadata.
func LiveComposer(theme Theme, width int, posture, status, identity, draft string, cursor, selectionStart, selectionEnd int) string {
	width = max(8, width)
	inner := width - 3
	role := RolePrimary
	switch {
	case strings.HasPrefix(posture, "disabled"):
		role = RoleMuted
	case strings.HasPrefix(posture, "running"):
		role = RoleWarning
	case strings.HasPrefix(posture, "retry"):
		role = RoleError
	case strings.HasPrefix(posture, "attachment"):
		role = RoleInfo
	}
	runes := []rune(draft)
	cursor = max(0, min(len(runes), cursor))
	selectionStart = max(0, min(len(runes), selectionStart))
	selectionEnd = max(selectionStart, min(len(runes), selectionEnd))
	if len(runes) == 0 {
		runes = []rune("Ask Harbor...")
		cursor = 0
		selectionStart, selectionEnd = 0, 0
	}
	var lines []string
	var line strings.Builder
	column := 0
	flush := func() {
		lines = append(lines, line.String())
		line.Reset()
		column = 0
	}
	panel := RolePanel
	for i, r := range runes {
		switch {
		case i == cursor && draft != "":
			line.WriteString(theme.Style(RolePrimary, &panel).Reverse(true).Render(string(r)))
		case i >= selectionStart && i < selectionEnd:
			line.WriteString(theme.Style(RoleAccent, &panel).Reverse(true).Render(string(r)))
		default:
			line.WriteString(theme.Style(RoleText, &panel).Render(string(r)))
		}
		if r == '\n' || column+1 >= inner {
			flush()
			continue
		}
		column++
	}
	if line.Len() > 0 || len(lines) == 0 {
		if cursor == len([]rune(draft)) {
			line.WriteString(theme.Style(RolePrimary, &panel).Reverse(true).Render(" "))
		}
		flush()
	}
	maxLines := max(1, min(len(lines), 6))
	lines = lines[max(0, len(lines)-maxLines):]
	border := theme.Style(role, nil).Render("┃")
	rows := make([]string, 0, len(lines)+3)
	for _, value := range lines {
		rows = append(rows, border+theme.Style(RoleText, &panel).Render("  ")+value+theme.Style(RoleText, &panel).Render(PadRight("", max(0, inner-Width(value)))))
	}
	rows = append(rows,
		border+theme.Style(RoleMuted, &panel).Render("  "+PadRight(status, inner)),
		theme.Style(role, nil).Render("╹")+theme.Style(RoleBorder, nil).Faint(true).Render(strings.Repeat("─", max(0, width-1))),
		theme.Style(RoleMuted, nil).Render(PadRight(identity, width)),
	)
	return strings.Join(rows, "\n")
}

// DescribeLayout returns a stable geometry summary used by smoke gates.
func DescribeLayout(layout Layout) string {
	return fmt.Sprintf("%dx%d main=%d sidebar=%d joined=%t actions=%s", layout.Width, layout.Height, layout.MainWidth, layout.SidebarWidth, layout.JoinedSidebar, map[bool]string{true: "horizontal", false: "stacked"}[layout.HorizontalActions])
}
