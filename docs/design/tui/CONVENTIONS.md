# Harbor TUI Quality And Interaction Conventions

This document is binding for every native Harbor TUI phase. The minimum quality
bar is the OpenCode TUI captured in `docs/research/tui-investigation/`: Harbor
may adapt content to its generic Runtime test/control role, but it may not ship
with weaker information hierarchy, keyboard ergonomics, responsive behavior,
streaming stability, visual polish, or terminal lifecycle safety.

The implementation target is **equivalent or better perceived quality**, not a
literal source-code or product-feature clone. Terminal-dependent effects that
Bubble Tea cannot reproduce exactly must preserve geometry, hierarchy, state
semantics, and responsive behavior under the pinned test terminal and degrade
honestly elsewhere.

## 1. Binding Reference Set

The acceptance authority, in order:

1. `docs/research/tui-investigation/05-opencode-visual-grammar.md` for exact
   geometry, tokens, responsive thresholds, layers, animation, and golden-state
   matrix.
2. `docs/research/tui-investigation/11-command-and-lifecycle-inventory.md` for
   command breadth, editing behavior, dialogs, navigation, and lifecycle.
3. `docs/research/tui-investigation/03-parts-tools-plugins.md` for typed-part and
   generic-tool rendering behavior.
4. `docs/research/tui-investigation/08-additional-reusable-opencode-patterns.md`
   through `10-final-saturation-findings.md` for compact mode, attention,
   interventions, retries, reconciliation, and cleanup.
5. This document for Harbor-specific quality gates and exclusions.

A reviewer may reject a technically functional phase when its terminal frames
fall materially below this reference set.

## 2. Product Boundary

The Harbor TUI is a generic Runtime test/control client. It replaces coding
content with Harbor sessions, turns, tasks, planners, tools, artifacts, events,
interventions, memory, and posture while preserving the reference interaction
quality.

Never add Git/VCS, repository trees, source editing, patches, shell execution,
LSP, worktrees, or coding-specific tool cards to satisfy visual parity. Parity
is measured in presentation and interaction quality, not coding-agent breadth.

## 3. Visual Grammar

The following are mandatory unless a golden review proves a better treatment:

- three stepped surfaces: canvas, panel, and element/menu;
- two-cell outer horizontal padding and one-row semantic rhythm;
- selective heavy `┃` left rules instead of box-heavy dashboards;
- bright primary content, strongly muted metadata, and one semantic accent per
  state;
- a bottom-anchored composer with an open visual edge and integrated metadata;
- background-highlighted active rows with computed contrast text;
- fixed-width dialogs and sidebar that do not stretch into sparse layouts; and
- content-aligned wrapping with a two-cell icon gutter and continuation indent.

The TUI must not resemble a generic admin dashboard rendered in a terminal.
Dense bordered grids, undifferentiated text dumps, and interchangeable card
stacks fail this convention.

Canonical measurements:

| Element | Contract |
|---|---:|
| Outer horizontal padding | 2 cells |
| Sidebar | 42 columns |
| Wide layout | width greater than 120 |
| Narrow actions | width less than 80 |
| Home composer cap | 75 columns or 70% on wide terminals |
| Dialog widths | 60 / 88 / 116 columns |
| Autocomplete cap | 10 rows |
| Standard spinner | 80 ms |
| Active-run spinner | 40 ms |

## 4. Responsive Contract

The exact threshold behavior is binding:

- 79 columns: controls stack;
- 80 columns: controls become horizontal;
- 120 columns: sidebar remains overlay-only;
- 121 columns: the 42-column sidebar joins layout.

No view may panic, calculate a negative dimension, overflow horizontally, or
hide the only route to an action at 40x12. At 240x60, fixed-width surfaces stay
fixed and the screen does not become an empty field of widely separated cards.

## 5. Composer Quality

The composer is a product surface, not a textarea appended to a log viewer. It
must provide:

- multiline editing, selection, undo/redo, word and line movement, and
  bracketed paste;
- bounded local history and draft stash;
- slash-command and structured-reference autocomplete;
- idle, focused, disabled, running, retry/error, attachment, and reduced-motion
  states;
- explicit current session/planner metadata and identity scope;
- active-run interruption guidance; and
- stable height growth up to the viewport-derived cap.

Autocomplete opens directly above the composer, wraps selection, shares its
width, and never covers the active input row.

## 6. Transcript And Streaming

- Follow output only while the viewport is already at the bottom.
- Never yank a reader who has scrolled upward.
- Preserve semantic block offsets for next/previous navigation.
- Batch high-rate text/reasoning deltas while applying lifecycle and
  intervention events immediately.
- Keep partial blocks visible through disconnect; reconcile or mark incomplete
  rather than erase them.
- Render every unknown event/tool/result through a safe generic fallback.
- Provide compact/native-scrollback mode where the terminal supports it.

Streaming that flickers, reflows unrelated blocks, loses the cursor, or makes
reading impossible fails the quality floor even when the underlying data is
correct.

## 7. Commands, Dialogs, And Focus

One command registry drives keybindings, command palette, which-key preview,
help text, and footer hints. Disabled commands explain why; they do not silently
disappear when the limitation is actionable.

All select dialogs share one filter/list/footer model, support arrows plus
Ctrl-P/Ctrl-N, page/home/end movement, wrap where appropriate, and restore
focus on close. Escape and Ctrl-C pop only the top modal before affecting the
underlying screen.

Core workflows must be fully keyboard-operable. Mouse support is additive and
must not degrade terminal-native text selection.

## 8. Themes And Accessibility

- Semantic tokens are the only component color source.
- Dark and light themes are first-class.
- Truecolor, 256-color, 16-color, and `NO_COLOR` paths preserve state meaning.
- Monochrome uses glyphs and attributes, never color alone.
- Reduced motion disables atmospheric animation and replaces spinners with a
  stable semantic fallback.
- CJK, combining marks, emoji, variation selectors, and long IDs use one width
  implementation and appear in goldens.

## 9. Harbor Honesty States

The first Harbor TUI is a single-operator development surface with exactly one
active session. Session selection replaces the active stream after draining it;
it does not create simultaneous per-session panes or imply multi-user presence.
The footer always shows the active identity triple. Auth rotation/replacement
preserves the draft, durable session reference, and replay cursor while visibly
showing the disconnected/reauthenticating state.

The UI must visually distinguish:

- disconnected, connecting, live, retrying, replaying, incomplete, and failed;
- exact values from `counters_partial`, aggregate `truncated`, scoped retention,
  and bounded tool analytics;
- unavailable capability from a legitimate zero;
- closed resumable session from erased terminal session; and
- locally queued intent from server-accepted work.

Muted metadata is not permission to hide these distinctions.

## 10. Acceptance Matrix

Every phase touching rendering updates goldens for applicable cells:

| Dimension | Required cases |
|---|---|
| Sizes | 40x12, 60x16, 72x20, 79x24, 80x24, 100x30, 120x30, 121x30, 160x40, 240x60 |
| Color | truecolor dark/light, 256 dark/light, 16-color, monochrome |
| Motion | normal and reduced |
| Input | keyboard-only, paste, resize during editing, modal focus restore |
| Stream | idle, active, scrolled-away, reconnect, replay gap, dropped event |
| Sessions | empty, populated, restart restore, closed/reopen-on-next-turn, erased, switch-with-stream-drain |
| Intervention | initial, confirm, reject editor, expired/resolved elsewhere |
| Notification | background-completion line, group-resolution rollup line (muted, one line — never a per-member fan-out) |
| Turn failure | foreground turn-failure status-strip line present, then cleared on next submit |
| Fallback | unknown event, unknown tool, unknown result, malformed safe payload |

Goldens validate geometry and hierarchy with ANSI stripped, then separate style
assertions validate tokens and attributes. PTY tests validate focus, cursor,
alternate-screen restoration, resize, suspend/resume, signals, and shutdown.

## 11. Review Gate

A TUI phase is not complete merely because commands work. Review includes:

1. side-by-side terminal captures against the relevant reference states;
2. the full applicable golden matrix;
3. keyboard walkthrough of every new workflow;
4. narrow/wide resize during active streaming and modal use;
5. color/accessibility degradation; and
6. PTY cleanup after success, error, signal, panic, and server loss.

Any visible regression in hierarchy, alignment, focus, scroll stability,
feedback latency, or state honesty is release-blocking. "Functional now, polish
later" is not an accepted phase deviation.
