# OpenCode Visual Grammar And Bubble Tea Fidelity Targets

## 1. What Produces The Look

OpenCode's TUI does not rely on dense ornament. Its recognizable feel comes
from a stable grammar:

- near-black full-screen canvas;
- three stepped surfaces: base, panel, and element/menu;
- two to four cells of horizontal indentation;
- one-row vertical rhythm;
- selective heavy left rules instead of rectangular borders;
- bright primary text over heavily muted metadata;
- one semantic accent per state;
- fixed-width overlays and sidebar;
- a bottom-anchored composer with a visually open right edge;
- background-highlighted selected rows;
- sticky transcript scrolling that does not yank the viewport when the user
  has scrolled upward; and
- fast local activity animation paired with slow atmospheric animation.

The content can be replaced with Harbor sessions, tasks, tools, events,
artifacts, approvals, and runtime posture without losing this grammar.

## 2. Canonical Measurements

| Element | Value |
|---|---:|
| Main horizontal padding | 2 cells each side |
| Main bottom padding | 1 row |
| Major vertical gap | 1 row |
| Sidebar width | 42 columns |
| Wide mode | terminal width `> 120` |
| Narrow action mode | terminal width `< 80` |
| Default home composer cap | 75 columns |
| Auto home composer cap | `max(75, floor(width * 0.7))` |
| Dialog medium | 60 columns |
| Dialog large | 88 columns |
| Dialog extra large | 116 columns |
| Dialog top placement | one quarter of terminal height |
| Autocomplete maximum | 10 rows |
| Default mouse-wheel step | 3 rows |
| Standard spinner interval | 80 ms |
| Active composer spinner | 40 ms |
| Metadata fade | 160 ms at 16 ms ticks |
| Background pulse | 30 FPS, 4600 ms period |
| Startup indicator delay | 500 ms |
| Startup indicator minimum visible time | 3 seconds |

Sources:

- `_ref/opencode-dev/packages/tui/src/routes/home.tsx:31-37,70-92`
- `_ref/opencode-dev/packages/tui/src/routes/session/index.tsx:263-272,1165-1185`
- `_ref/opencode-dev/packages/tui/src/routes/session/sidebar.tsx:26-48`
- `_ref/opencode-dev/packages/tui/src/ui/dialog.tsx:21-65`
- `_ref/opencode-dev/packages/tui/src/component/prompt/autocomplete.tsx:532-551,712-779`
- `_ref/opencode-dev/packages/tui/src/util/scroll.ts:8-27`

## 3. Responsive Layout

### 3.1 Home

The home screen is a centered vertical stack:

1. flexible top space;
2. up to four rows of spacer;
3. four-row logo;
4. one row of spacer;
5. the composer with one row of top padding;
6. optional content below the composer;
7. flexible bottom space; and
8. a separate full-width footer.

At small widths, the 75-column composer cap is constrained by the parent
rather than forcing horizontal overflow.

Harbor content:

```text
Ask Harbor... "Why is the latest task paused?"
```

The footer should carry the Runtime endpoint, identity/session scope,
connection state, and version.

### 3.2 Session

The session route has a flexible main column and an optional 42-column
sidebar. Main content width is:

```text
terminal width - 4 outer padding - (42 when sidebar participates in layout)
```

At 121 columns and above, the sidebar is visible in normal layout. At 120 or
below, a manually opened sidebar overlays the right side over a dark scrim.
At less than 80 columns, approval and rejection controls stack vertically.

The exact threshold behavior is intentional:

- width 79: narrow actions;
- width 80: horizontal actions;
- width 120: sidebar is still overlay-only;
- width 121: sidebar joins normal layout.

### 3.3 Harbor Sidebar Contents

The first Harbor sidebar should show:

- session title and visible identity scope;
- active task and parent/child task state;
- planner and tool activity summary;
- pending intervention count;
- recent artifact references;
- event-stream state; and
- Runtime/Protocol version.

It should not contain file changes, worktrees, LSP, or source status.

## 4. Borders And Surfaces

### 4.1 Character Set

The signature border is a heavy vertical line:

```text
┃
```

Cards generally draw only this left edge. The composer uses `╹` as its lower
terminator and `▀` for a one-row shadow/base treatment. Other recurring glyphs
include `•`, `●`, `✓`, `○`, `↳`, and Braille spinner frames.

Source: `_ref/opencode-dev/packages/tui/src/ui/border.ts:1-21` and
`packages/tui/src/component/prompt/index.tsx:1347-1358,1484-1508`.

### 4.2 Activity Card

- heavy left rule in the activity accent;
- panel background;
- top/bottom padding 1;
- left padding 2;
- top margin 1 except first item;
- hover background switches to element surface.

### 4.3 Result Or Error Card

- heavy left rule;
- top margin 1;
- top/bottom padding 1;
- left padding 2;
- internal gap 1;
- panel background;
- menu background on hover;
- semantic border color for warning/error/approval.

### 4.4 Vertical Rhythm

- One blank row between semantic units.
- No blank row between tightly related one-line status rows.
- Three-cell leading indent for inline activity.
- Two-cell icon gutter.
- Wrapped continuation aligns under content, not the icon.
- `↳` introduces subordinate metrics or status.

Representative Harbor frame:

```text
   ◉ Task inspect-runtime is running
     ↳ Waiting for tool result

   ✓ Task reconcile-session completed
     ↳ 4 events · 1.8s
```

Intervention frame:

```text
┃
┃  Session paused: operator approval required
┃
```

## 5. Composer

### 5.1 Geometry

The composer fills its parent and contains:

1. left-bordered input surface;
2. text area;
3. one-row metadata line inside the surface;
4. one-row lower-left/shadow treatment;
5. one-row external status/hint line; and
6. autocomplete directly above it.

Inner padding is two cells left and right and one row top. The textarea starts
at one row and grows to a configured maximum or
`max(6, floor(terminalHeight/3))`.

### 5.2 States

| State | Treatment |
|---|---|
| Idle | border tinted toward current agent/planner accent |
| Leader pending | neutral border, muted input |
| Alternate mode | primary border and explicit mode label |
| Disabled | cursor effectively hidden |
| Running | animated block spinner and interrupt hint |
| Retry/error | error text, attempt, and retry countdown |
| Attachment | compact secondary filename/artifact badge |
| Static animation mode | `[⋯]` or `⋯` fallback |

Harbor's metadata row can read:

```text
ReAct · default planner · session 01J...
```

The lower hint row can read:

```text
tenant/user/session                         ctrl+p commands
```

### 5.3 Autocomplete

Autocomplete is composer-width, appears above the input, and is capped at ten
rows and by available space. It uses the menu surface, heavy side rule,
one-cell row padding, primary active-row background, hidden scrollbar, and
wraparound selection.

Harbor candidates:

- slash commands such as `/pause`, `/resume`, `/redirect`, `/tasks`, and
  `/artifacts`;
- structured references such as `@session`, `@task`, `@artifact`, and
  `@tool`; and
- locally saved operator actions.

## 6. Dialogs And Command Palette

### 6.1 Modal Shell

Dialogs use a full-terminal dark scrim and centered panel, placed one quarter
of the way down. Width is capped to terminal width minus two cells and one of
60, 88, or 116 columns.

Backdrop click closes the dialog, but releasing a text selection does not.
Escape and Ctrl-C pop the top dialog and restore prior focus.

### 6.2 Select Dialog

Standard geometry:

- outer gap 1 and bottom padding 1;
- header/filter padding 4 left/right;
- scrollbox padding 1 left/right;
- category left padding 3;
- row left/right padding 3;
- footer left padding 4, right padding 2;
- footer item gap 2; and
- list height `min(rendered rows, floor(height/2)-6)`.

The current item uses `●`. The active row is bold with a primary background.
Category headings are bold accent. Search and empty state are muted.

The command palette is this same list with a duplicated Suggested category
before the complete categorized command set.

### 6.3 Harbor Dialog Uses

- sessions;
- tasks;
- commands;
- themes;
- Runtime/tool status;
- planner or agent selection when the Protocol supports selection;
- artifact references; and
- event filters.

All should share one list model and style grammar.

## 7. Type Hierarchy

Terminal typography is attribute-driven:

1. primary text for titles, values, and user content;
2. muted text for paths, explanations, timestamps, IDs, and shortcuts;
3. accent text for category headings and active mode;
4. semantic colors only for state-bearing success/warning/error; and
5. computed contrast text on selected backgrounds.

Bold is used for titles, category headings, selected rows, and compact state
badges. Muted text does more work than borders.

## 8. Default Theme Contract

### 8.1 Core Colors

| Token | Dark | Light |
|---|---|---|
| primary | `#fab283` | `#3b7dd8` |
| secondary | `#5c9cf5` | `#7b5bb6` |
| accent | `#9d7cd8` | `#d68c27` |
| error | `#e06c75` | `#d1383d` |
| warning | `#f5a742` | `#d68c27` |
| success | `#7fd88f` | `#3d9a57` |
| info | `#56b6c2` | `#318795` |
| text | `#eeeeee` | `#1a1a1a` |
| textMuted | `#808080` | `#8a8a8a` |

### 8.2 Surfaces

| Token | Dark | Light |
|---|---|---|
| background | `#0a0a0a` | `#ffffff` |
| panel | `#141414` | `#fafafa` |
| element/menu | `#1e1e1e` | `#f5f5f5` |
| borderSubtle | `#3c3c3c` | `#d4d4d4` |
| border | `#484848` | `#b8b8b8` |
| borderActive | `#606060` | `#a0a0a0` |

Default thinking opacity is 0.6. The theme contract also includes diff,
Markdown, and syntax tokens, but the generic Harbor TUI initially needs only
semantic status, surfaces, borders, Markdown, reasoning, and data-view tokens.

OpenCode ships 33 theme JSON assets in the captured snapshot. Harbor need not
ship all themes initially, but should use the same semantic token contract so
additional themes do not require component changes.

## 9. Interaction And Scrolling

### 9.1 Transcript

- sticky to bottom while already at bottom;
- does not jump when user has scrolled up;
- one blank row at the top;
- optional scrollbar adds one-cell viewport and track padding;
- page movement is half viewport;
- half-page command is quarter viewport;
- line movement is one row; and
- semantic next/previous navigation uses recorded block offsets.

### 9.2 Mouse

- configurable mouse capture;
- click composer to focus;
- hover changes interactive background/foreground;
- click transcript block only if no text selection is active;
- keyboard filtering switches list input mode to keyboard so synthetic hover
  does not steal selection; and
- mouse-down selects, mouse-up activates.

Bubble Tea cannot exactly reproduce OpenTUI framebuffer selection. Harbor
should support explicit copy, right-click where available, and terminal-native
selection modifiers rather than pretending universal copy-on-select parity.

## 10. Animation

Use animation only while the relevant component is visible:

- 80 ms standard spinner;
- 40 ms active run spinner;
- optional 160 ms metadata fade;
- 1 second retry countdown;
- 500 ms delayed startup indicator; and
- optional 30 FPS home pulse.

The pulse is atmosphere, not core feedback. It should be disabled in reduced-
motion mode and may be deferred from the first release.

## 11. Bubble Tea Rendering Strategy

### 11.1 Root Composition

Render in layers:

1. base route;
2. narrow sidebar scrim/overlay;
3. autocomplete;
4. toast;
5. modal backdrop and dialog; and
6. startup indicator.

Track width and height explicitly. Calculate content dimensions before calling
Lip Gloss because rendered width includes borders and padding.

### 11.2 Layout Constants

```go
const (
    outerPaddingX    = 2
    sidebarWidth     = 42
    wideBreakpoint   = 120 // wide when width > 120
    narrowBreakpoint = 80  // narrow when width < 80
    homePromptWidth  = 75
    dialogMedium     = 60
    dialogLarge      = 88
    dialogXLarge     = 116
    autocompleteRows = 10
)
```

### 11.3 Custom Left Rule

Use a small manual renderer rather than Lip Gloss's rectangular border API.
Prefix each visual line with styled `┃`; this makes one-sided cards,
per-line semantic colors, and composer terminators deterministic.

### 11.4 Scrolling

Use a viewport-like model but retain semantic block offsets. Before appending
streamed content, record whether the viewport is at bottom. Auto-follow only
when true. Render snapshots without ANSI for geometry tests, then test styles
separately.

### 11.5 Unicode Width

Every custom glyph, wrapped line, and extmark-like reference must use one
single width implementation. Golden tests should cover CJK, combining marks,
emoji, variation selectors, and long IDs.

## 12. Known Parity Limits

Exact parity is terminal-dependent:

- Lip Gloss cannot perform true RGBA framebuffer blending; scrims need a
  precomputed dark color or dim approximation.
- Truecolor quantizes under 256/16-color terminals.
- Half-block glyphs depend on font metrics.
- Bold may brighten color rather than only change weight.
- Italic and strikethrough are not universal.
- Mouse selection behavior differs from OpenTUI.
- Unicode width can differ by terminal and library version.
- Portable terminals cannot natively host arbitrary MCP App HTML, PDF, audio,
  or video.

The acceptance target is identical geometry, hierarchy, state semantics, and
responsive behavior under a pinned test terminal, with graceful semantic
degradation elsewhere.

## 13. Visual Acceptance Matrix

### 13.1 Terminal Sizes

| Size | Acceptance |
|---|---|
| 40x12 | no panic; no negative heights; compact/hide logo; dialog caps at 38; actions stack |
| 60x16 | composer constrained to 56 content columns; modal caps at 58; autocomplete uses available rows |
| 72x20 | golden wrapping for inline events, three-cell indent, two-cell icon gutter, narrow actions |
| 79x24 | final narrow case; no overflow in intervention hints |
| 80x24 | first horizontal-action case |
| 100x30 | sidebar hidden by default; manual sidebar overlays with scrim |
| 120x30 | still overlay-only sidebar |
| 121x30 | first normal sidebar layout; 42-column sidebar |
| 160x40 | roomy transcript; 116-column extra-large dialog fits |
| 240x60 | fixed widths remain fixed; layout does not become sparse |

### 13.2 States

Golden frames should cover:

- home idle and autocomplete;
- empty and populated session;
- sticky-at-bottom and scrolled-away streaming;
- normal and overlay sidebar;
- composer idle, focused, disabled, running, retry, and reduced-motion;
- approval initial, persistent confirmation, rejection editor, and expanded;
- command palette empty, filtered, grouped, current-item, and action-focused;
- stream reconnect, replay gap, and dropped-event warning;
- unknown event/tool/result fallback; and
- degraded 256-color and no-italic terminals.

### 13.3 Color Modes

- truecolor dark and light;
- 256-color dark and light;
- 16-color semantic fallback;
- `NO_COLOR` or monochrome with attributes/glyphs carrying state; and
- reduced motion.

## 14. Primary Sources

- `_ref/opencode-dev/packages/tui/src/routes/home.tsx`
- `_ref/opencode-dev/packages/tui/src/routes/session/index.tsx`
- `_ref/opencode-dev/packages/tui/src/routes/session/sidebar.tsx`
- `_ref/opencode-dev/packages/tui/src/routes/session/permission.tsx`
- `_ref/opencode-dev/packages/tui/src/routes/session/question.tsx`
- `_ref/opencode-dev/packages/tui/src/component/prompt/index.tsx`
- `_ref/opencode-dev/packages/tui/src/component/prompt/autocomplete.tsx`
- `_ref/opencode-dev/packages/tui/src/ui/dialog.tsx`
- `_ref/opencode-dev/packages/tui/src/ui/dialog-select.tsx`
- `_ref/opencode-dev/packages/tui/src/theme/index.ts`
- `_ref/opencode-dev/packages/tui/src/theme/assets/opencode.json`
- `_ref/opencode-dev/packages/tui/test/cli/tui/inline-tool-wrap-snapshot.test.tsx`
- `_ref/opencode-dev/packages/tui/test/cli/tui/__snapshots__/inline-tool-wrap-snapshot.test.tsx.snap`
