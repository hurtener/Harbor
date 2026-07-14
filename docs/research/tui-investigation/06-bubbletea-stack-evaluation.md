# Bubble Tea Stack Evaluation

> Snapshot date: 2026-07-14. Versions are research inputs for a future RFC,
> not dependency authorization.

## 1. Conclusion

Bubble Tea v2 is a viable foundation for Harbor's generic Protocol-client
TUI. It can reproduce OpenCode's geometry, hierarchy, colors, responsive
breakpoints, ordered overlays, keyboard behavior, and animation cadence under
a pinned terminal profile.

It cannot guarantee pixel-identical output across every terminal because
selection, font metrics, color quantization, Unicode width, image protocols,
and attribute support remain terminal-dependent.

Recommended research baseline:

```text
charm.land/bubbletea/v2                 v2.0.8
charm.land/lipgloss/v2                  v2.0.5
charm.land/bubbles/v2                   v2.1.1
github.com/charmbracelet/x/ansi         v0.11.7
```

Conditional:

```text
charm.land/glamour/v2                   v2.0.1
github.com/rivo/uniseg                   v0.4.7
```

Do not initially add Harmonica, Reflow, another TUI framework, an image
framework, Huh, Wish, file pickers, shell/PTY widgets, repository browsers,
diff viewers, or editors.

## 2. Current Versions And Constraints

| Library | Module | Stable version | Minimum Go | Use |
|---|---|---:|---:|---|
| Bubble Tea | `charm.land/bubbletea/v2` | 2.0.8 | 1.25 | runtime and cell renderer |
| Lip Gloss | `charm.land/lipgloss/v2` | 2.0.5 | 1.25 | styles, composition, layers |
| Bubbles | `charm.land/bubbles/v2` | 2.1.1 | 1.25 | selected key/input/viewport primitives |
| Glamour | `charm.land/glamour/v2` | 2.0.1 | 1.25.8 | optional Markdown |
| Charm ANSI | `github.com/charmbracelet/x/ansi` | 0.11.7 | 1.24.2 | ANSI-safe width/wrap/cut |
| Uniseg | `github.com/rivo/uniseg` | 0.4.7 | 1.18 | conditional grapheme editor |

Harbor's Go 1.26 floor satisfies these requirements.

Bubbles 2.1.1 pins slightly earlier Bubble Tea and Lip Gloss releases. A
future Harbor module should pin the desired newer versions explicitly and let
Go minimum-version selection resolve the graph.

## 3. Static-Binary And Platform Posture

The evaluated Bubble Tea, Lip Gloss, selected Bubbles packages, Glamour,
Harmonica, and ANSI dependency paths are pure Go. They are compatible with
Harbor's `CGO_ENABLED=0` requirement and support Linux, macOS, and Windows.

This is not a substitute for Harbor's own gates. A future implementation must
still prove:

- `CGO_ENABLED=0` build;
- Linux/macOS/Windows compilation;
- static Linux linkage;
- Windows Terminal resize and cleanup;
- terminal-state restoration after signals and failures; and
- race-clean SSE ingestion and shutdown.

## 4. Terminal Capability Coverage

| Capability | Bubble Tea v2 posture | Harbor implication |
|---|---|---|
| Alternate screen | declarative on `View` | full-screen TUI default |
| Inline mode | leave alternate screen off | optional compact attach/status mode |
| Runtime screen switch | supported | can move between inline boot status and full screen |
| Mouse | cell and all-motion modes | configurable; maintain own hit map |
| Kitty keyboard | basic disambiguation automatic, enhancements available | leader and modifier handling with fallbacks |
| Focus events | supported | pause attention/sound policy on focus |
| Bracketed paste | enabled by default | safe multiline prompt handling |
| Clipboard | OSC 52 set/read with terminal restrictions | explicit copy command; do not assume read |
| Color profile | truecolor, 256, 16, ASCII, non-TTY | compile semantic theme per profile |
| Background mode | terminal query | dark/light auto with configured fallback |
| Resize | initial and subsequent messages | exact 79/80 and 120/121 breakpoints |
| Hyperlinks | OSC 8 through Lip Gloss | external docs, OAuth, artifact links |
| Images | low-level Kitty/Sixel/iTerm encoders only | metadata/external-open initially |
| Progress bar | terminal-native support exists | not required for core visual grammar |

Every keybinding needs a conventional fallback. Kitty-specific modifier and
release behavior is not universal, especially through tmux or older terminals.

## 5. Renderer Fidelity

Bubble Tea v2's cell-buffer renderer materially closes the gap with OpenTUI:

- changed cell buffers are diffed rather than blindly repainting;
- identical views are skipped;
- synchronized-update mode is used where supported;
- terminal Unicode mode can switch width semantics;
- the default ceiling is 60 FPS, configurable up to 120; and
- recent releases explicitly address difficult emoji-width behavior.

This supports OpenCode's 40 ms and 80 ms activity ticks and optional 30 FPS
home pulse without requiring a second rendering engine.

Lip Gloss v2 provides:

- cell-based layers and a compositor for dialogs, sidebar overlays,
  autocomplete, toasts, and startup status;
- width/height/size measurement;
- horizontal and vertical joining;
- ANSI and hyperlink-preserving wrapping; and
- immutable style values suitable for a compiled theme.

It does not provide true alpha compositing. Scrims need a precomputed dark
surface or dim approximation.

## 6. Geometry Authority

Use `github.com/charmbracelet/x/ansi` as the one app-owned authority for:

- visible string width;
- ANSI-preserving wrapping;
- word and hard wrapping;
- truncation; and
- cell-range cutting.

Do not mix Reflow, `go-runewidth`, custom rune counting, and ANSI width logic
for ordinary layout. A future composer may import `rivo/uniseg` directly only
if Harbor owns grapheme-aware movement and deletion.

## 7. Bubbles Components

### 7.1 Viewport

Useful capabilities:

- explicit width and height;
- vertical/horizontal scrolling;
- top/bottom checks and navigation;
- scroll percentages;
- configurable three-row default wheel delta;
- soft wrapping;
- gutters and per-line styling; and
- highlights.

Do not make the viewport Harbor's transcript model. `SetContent` rescans text,
and the component lacks an append-only semantic-block API. Harbor should keep
typed transcript blocks, semantic offsets, and a bounded visible projection,
then use viewport mechanics for movement.

Sticky-bottom rule:

1. record `AtBottom` before an event update;
2. update typed blocks;
3. recompute the visible projection; and
4. go to bottom only if the prior state was at bottom.

### 7.2 Textarea

Bubbles textarea already supports multiline wrapping, dynamic height,
focus/blur, cursor modes, paste messages, keymaps, and focused/blurred styles.

Correctness caveat: internal editing remains partly rune-indexed. Deleting
backward may split a combining sequence or ZWJ emoji even when display width
is grapheme-aware. Before adopting it, run a corpus covering:

- combining accents;
- emoji skin tones;
- flags;
- ZWJ families;
- variation selectors;
- CJK; and
- mixed ANSI-free and pasted text.

If those tests fail, Harbor should own a small composer buffer with Uniseg
grapheme boundaries rather than fork the whole textarea.

Prefer terminal bracketed paste and Bubble Tea OSC 52 commands over an OS-
clipboard subprocess dependency.

### 7.3 List

Bubbles list provides filtering, pagination, status, help, spinner, delegates,
and selection. Its built-in chrome does not match OpenCode's command palette.
Harbor should build one custom grouped select/palette model and reuse it for
sessions, commands, tasks, themes, artifacts, and filters.

### 7.4 Key And Spinner

Use selected key-binding and spinner primitives. Keep Harbor's command IDs,
mode stack, leader sequence, contextual activation, and help generation in an
app-owned registry.

## 8. Markdown

Glamour v2 is the obvious optional renderer. It supports GFM, tables, links,
custom styles, wrapping, and Chroma syntax highlighting, and remains pure Go.

Costs:

- a materially larger dependency graph;
- document-oriented full reparse;
- default spacing that does not match Harbor's one-row grammar; and
- no incremental streaming AST.

If approved:

1. create a long-lived custom renderer per theme and content width;
2. cache completed blocks;
3. rerender only the active streaming block;
4. coalesce SSE deltas to a bounded visual cadence; and
5. normalize Glamour spacing into Harbor's transcript block grammar.

If dependency minimization wins, the first release can render plain wrapped
text, inline emphasis/code, and links with a small safe subset. That is a
visible product tradeoff and should be explicit in the phase plan.

## 9. Required Harbor-Owned Components

Stock libraries do not provide exact parity. Harbor needs:

1. root responsive layout with exact breakpoints;
2. typed transcript projection and semantic offsets;
3. answer, reasoning, task, planner, tool, artifact, pause, control, error, and
   unknown block renderers;
4. manual heavy-left-rule rendering;
5. composer shell with metadata, bottom treatment, hints, and grapheme-safe
   editor;
6. composer-width autocomplete;
7. grouped command/select palette;
8. modal stack and focus restoration;
9. fixed sidebar plus overlay mode;
10. mouse hit map and input-mode tracker;
11. semantic theme compiler for color profile/light/dark/reduced motion;
12. visibility-aware animation scheduler;
13. explicit clipboard/copy behavior; and
14. artifact rendering with visible degradation.

## 10. Event Ingestion Architecture

Do not implement one blocking `tea.Cmd` per event or a reconnect loop inside a
command. Bubble Tea commands cannot be forcibly cancelled while blocked.

Preferred model:

```text
owned cancellable Protocol reader goroutine
        -> bounded/coalescing client channel
        -> Program.Send(protocolEventMsg)
        -> pure Update reducer
        -> typed transcript projection
```

Requirements:

- one reader per connection;
- cancellation joined on shutdown;
- explicit reconnect state and backoff;
- `Last-Event-ID` replay;
- dropped-event/replay-gap handling;
- snapshot reconciliation after gaps;
- bounded queue and explicit coalescing/drop policy;
- answer/reasoning delta batching to a visual cadence; and
- no identity or session state stored in package globals.

## 11. Testing Strategy

Bubble Tea models are ordinary `Update` and `View` functions. Useful runtime
options include fixed input/output, environment, window size, color profile,
renderer disabling, and signal disabling.

There is no maintained v2 `teatest` package in the evaluated tag. Harbor
should own focused test helpers instead of planning around it.

Future gates:

- pure model-transition tests;
- ANSI-stripped geometry goldens for every size in the visual matrix;
- styled goldens under truecolor, 256, 16, ASCII, and monochrome profiles;
- Unicode/grapheme corpus;
- sticky streaming at bottom and while scrolled away;
- focus restoration and modal stacking;
- fixed-buffer renderer snapshots;
- PTY smoke for resize, focus, paste, mouse, and cleanup;
- Windows Terminal resize regression;
- race-tested SSE ingress and cancellation;
- deterministic tick messages rather than sleeps; and
- N>=100 concurrent client/reducer invocations where reusable compiled
  artifacts are introduced, matching Harbor's D-025 contract.

## 12. Dependency Recommendation

### Required Candidate Set

```text
charm.land/bubbletea/v2
charm.land/lipgloss/v2
charm.land/bubbles/v2       # selected packages only
github.com/charmbracelet/x/ansi
```

### Conditional

```text
charm.land/glamour/v2       # full Markdown decision
github.com/rivo/uniseg      # custom grapheme editor decision
```

### Exclude Initially

```text
github.com/charmbracelet/harmonica
github.com/muesli/reflow
github.com/mattn/go-runewidth as a direct dependency
github.com/charmbracelet/x/term as a direct dependency
terminal image frameworks
Huh, Wish, BubbleZone
file pickers, shell/PTY widgets, repository or diff components
```

This remains a future RFC dependency decision. Harbor's no-heavy-framework
posture and CGo-free build must be reviewed before implementation.

## 13. Official References

- Bubble Tea releases: <https://github.com/charmbracelet/bubbletea/releases>
- Bubble Tea 2.0.8: <https://github.com/charmbracelet/bubbletea/releases/tag/v2.0.8>
- Lip Gloss releases: <https://github.com/charmbracelet/lipgloss/releases>
- Lip Gloss 2.0.5: <https://github.com/charmbracelet/lipgloss/releases/tag/v2.0.5>
- Bubbles releases: <https://github.com/charmbracelet/bubbles/releases>
- Bubbles 2.1.1: <https://github.com/charmbracelet/bubbles/releases/tag/v2.1.1>
- Glamour 2.0.1: <https://github.com/charmbracelet/glamour/releases/tag/v2.0.1>
- Charm ANSI: <https://github.com/charmbracelet/x/tree/ansi/v0.11.7/ansi>
- Uniseg: <https://github.com/rivo/uniseg/tree/v0.4.7>
