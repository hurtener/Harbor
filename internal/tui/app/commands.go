package app

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// CommandID is a stable command registry key.
type CommandID string

// Command describes one dispatch, palette, which-key, footer, and help entry.
type Command struct {
	ID                           CommandID
	Title, Description, Category string
	Bindings                     []string
	Suggested, Hidden            bool
	Enabled                      func(Context) (bool, string)
}

// Context is the command activation context.
type Context struct {
	ModalOpen, Connected, Erased, HasTranscript, HasAttachment, HasFollowUp bool
	TaskControl, SessionLifecycle, SessionScope                             bool
}

// CommandView is a resolved command with its disabled reason.
type CommandView struct {
	Command        Command
	Enabled        bool
	DisabledReason string
}

// Registry is an immutable command catalog.
type Registry struct{ commands []Command }

func cloneCommand(command Command) Command {
	command.Bindings = append([]string(nil), command.Bindings...)
	return command
}

// NewRegistry validates and copies command definitions.
func NewRegistry(commands ...Command) (Registry, error) {
	seen := make(map[CommandID]struct{}, len(commands))
	bindings := make(map[string]CommandID, len(commands))
	copyCommands := append([]Command(nil), commands...)
	for i := range copyCommands {
		command := &copyCommands[i]
		if command.ID == "" || strings.TrimSpace(command.Title) == "" {
			return Registry{}, fmt.Errorf("command %d: id and title required", i)
		}
		if _, exists := seen[command.ID]; exists {
			return Registry{}, fmt.Errorf("command %q: duplicate id", command.ID)
		}
		seen[command.ID] = struct{}{}
		command.Bindings = append([]string(nil), command.Bindings...)
		if len(command.Bindings) > 0 {
			binding := strings.Join(command.Bindings, " ")
			if prior, exists := bindings[binding]; exists {
				return Registry{}, fmt.Errorf("command %q: binding %q duplicates %q", command.ID, binding, prior)
			}
			bindings[binding] = command.ID
		}
	}
	return Registry{commands: copyCommands}, nil
}

// DefaultRegistry returns the shell's complete command source.
func DefaultRegistry() Registry {
	connected := func(ctx Context) (bool, string) {
		if !ctx.Connected {
			return false, "requires a live authenticated Runtime attachment"
		}
		return true, ""
	}
	notErased := func(ctx Context) (bool, string) {
		if ok, reason := connected(ctx); !ok {
			return ok, reason
		}
		if ctx.Erased {
			return false, "erased sessions require Start Fresh"
		}
		return true, ""
	}
	hasTranscript := func(ctx Context) (bool, string) {
		if !ctx.HasTranscript {
			return false, "no transcript is available"
		}
		return true, ""
	}
	hasAttachment := func(ctx Context) (bool, string) {
		if !ctx.HasAttachment {
			return false, "no attachment is selected"
		}
		return true, ""
	}
	startFresh := func(ctx Context) (bool, string) {
		if ctx.Erased {
			return true, ""
		}
		return connected(ctx)
	}
	taskControl := func(ctx Context) (bool, string) {
		if ok, reason := notErased(ctx); !ok {
			return ok, reason
		}
		if !ctx.TaskControl {
			return false, "Runtime does not advertise task_control"
		}
		return true, ""
	}
	sessionScope := func(ctx Context) (bool, string) {
		if ok, reason := connected(ctx); !ok {
			return ok, reason
		}
		if !ctx.SessionScope {
			return false, "JWT lacks admin or console:fleet scope for session browsing"
		}
		return true, ""
	}
	sessionDelete := func(ctx Context) (bool, string) {
		if ok, reason := notErased(ctx); !ok {
			return ok, reason
		}
		if !ctx.SessionLifecycle {
			return false, "Runtime does not advertise session_lifecycle"
		}
		return true, ""
	}
	hasFollowUp := func(ctx Context) (bool, string) {
		if !ctx.HasFollowUp {
			return false, "no local follow-up is queued"
		}
		return true, ""
	}
	registry, err := NewRegistry(
		Command{ID: "palette", Title: "Command palette", Description: "Find an available action", Category: "Application", Bindings: []string{"ctrl+p"}, Suggested: true},
		Command{ID: "help", Title: "Keyboard help", Description: "Show all reachable commands", Category: "Application", Bindings: []string{"ctrl+x", "?"}},
		Command{ID: "sidebar", Title: "Toggle runtime context", Description: "Show the fixed context sidebar", Category: "View", Bindings: []string{"ctrl+x", "s"}, Suggested: true},
		Command{ID: "theme", Title: "Switch theme", Description: "Choose dark, light, or automatic mode", Category: "View", Bindings: []string{"ctrl+x", "t"}},
		Command{ID: "route-session", Title: "Conversation", Description: "Return to the active session conversation", Category: "Runtime", Bindings: []string{"f1"}, Enabled: connected},
		Command{ID: "route-tasks", Title: "Tasks", Description: "Inspect task lifecycle, tree, progress, groups, and latency", Category: "Runtime", Bindings: []string{"f2"}, Enabled: connected},
		Command{ID: "route-tools", Title: "Tools", Description: "Inspect transport, OAuth, approval, metrics, and content posture", Category: "Runtime", Bindings: []string{"f3"}, Enabled: connected},
		Command{ID: "route-artifacts", Title: "Artifacts", Description: "Inspect artifact references and safe preview posture", Category: "Runtime", Bindings: []string{"f4"}, Enabled: connected},
		Command{ID: "route-events", Title: "Events", Description: "Inspect live and retained event diagnostics", Category: "Runtime", Bindings: []string{"f5"}, Enabled: connected},
		Command{ID: "route-posture", Title: "Runtime posture", Description: "Inspect health, drivers, capabilities, governance, and LLM", Category: "Runtime", Bindings: []string{"f6"}, Enabled: connected},
		Command{ID: "route-interventions", Title: "Interventions", Description: "Open the PauseToken-correlated intervention queue", Category: "Runtime", Bindings: []string{"f7"}, Enabled: connected},
		Command{ID: "route-diagnostics", Title: "Diagnostics", Description: "Inspect stream, retention, partiality, and capability failures", Category: "Runtime", Bindings: []string{"f8"}, Enabled: connected},
		Command{ID: "actions", Title: "Actions", Description: "Open the canonical capability and scope-gated action matrix", Category: "Runtime", Bindings: []string{"f9"}, Enabled: connected},
		Command{ID: "sessions", Title: "Switch session", Description: "Search and attach another authorized session", Category: "Session", Bindings: []string{"ctrl+x", "l"}, Enabled: sessionScope},
		Command{ID: "session-new", Title: "Start Fresh", Description: "Attach a new authorized session ID", Category: "Session", Bindings: []string{"ctrl+x", "n"}, Enabled: startFresh},
		Command{ID: "session-rename", Title: "Rename session", Description: "Set the canonical session title", Category: "Session", Bindings: []string{"ctrl+r"}, Enabled: notErased},
		Command{ID: "session-delete", Title: "Delete session", Description: "Erase the active session after confirmation", Category: "Session", Bindings: []string{"ctrl+x", "d"}, Enabled: sessionDelete},
		Command{ID: "submit", Title: "Submit turn", Description: "Start work in the active session", Category: "Session", Bindings: []string{"enter"}, Enabled: taskControl},
		Command{ID: "reauthenticate", Title: "Replace credential", Description: "Install a JWT in memory and reconnect", Category: "Session", Bindings: []string{"ctrl+x", "i"}},
		Command{ID: "search", Title: "Search transcript", Description: "Incrementally filter semantic transcript blocks", Category: "Transcript", Bindings: []string{"ctrl+x", "f"}, Enabled: hasTranscript},
		Command{ID: "export", Title: "Export transcript", Description: "Write the redacted projection as Markdown", Category: "Transcript", Bindings: []string{"ctrl+x", "x"}, Enabled: hasTranscript},
		Command{ID: "reasoning", Title: "Toggle reasoning", Description: "Collapse or expand reasoning blocks", Category: "Transcript", Bindings: []string{"ctrl+x", "r"}, Enabled: hasTranscript},
		Command{ID: "tool-detail", Title: "Toggle tool detail", Description: "Collapse or expand tool blocks", Category: "Transcript", Bindings: []string{"ctrl+x", "o"}, Enabled: hasTranscript},
		Command{ID: "timestamps", Title: "Toggle timestamps", Description: "Show or hide transcript timestamps", Category: "Transcript", Bindings: []string{"ctrl+x", "y"}, Enabled: hasTranscript},
		Command{ID: "compact", Title: "Toggle compact mode", Description: "Use native terminal scrollback", Category: "View", Bindings: []string{"ctrl+x", "c"}},
		Command{ID: "reduced-motion", Title: "Toggle reduced motion", Description: "Disable or enable terminal animation", Category: "View", Bindings: []string{"ctrl+x", "m"}},
		Command{ID: "stash", Title: "Stash draft", Description: "Store the draft locally", Category: "Composer", Bindings: []string{"ctrl+x", "b"}},
		Command{ID: "stash-pop", Title: "Restore stashed draft", Description: "Pop the newest local draft", Category: "Composer", Bindings: []string{"ctrl+x", "p"}},
		Command{ID: "followup-cancel", Title: "Cancel queued follow-up", Description: "Remove the newest undispatched local intent", Category: "Composer", Bindings: []string{"ctrl+x", "k"}, Enabled: hasFollowUp},
		Command{ID: "followup-retry", Title: "Retry failed follow-up", Description: "Resume ordered dispatch from failed local intent", Category: "Composer", Bindings: []string{"ctrl+x", "j"}, Enabled: hasFollowUp},
		Command{ID: "attachment", Title: "Attach file", Description: "Upload an artifact with a disposition hint", Category: "Composer", Bindings: []string{"ctrl+x", "a"}, Enabled: notErased},
		Command{ID: "attachment-remove", Title: "Remove attachment", Description: "Remove the newest local attachment", Category: "Composer", Bindings: []string{"ctrl+x", "e"}, Enabled: hasAttachment},
		Command{ID: "attachment-retry", Title: "Retry attachment", Description: "Retry the newest failed upload", Category: "Composer", Bindings: []string{"ctrl+x", "u"}, Enabled: hasAttachment},
		Command{ID: "quit", Title: "Quit", Description: "Restore the terminal and exit", Category: "Application", Bindings: []string{"q"}},
	)
	if err != nil {
		panic(err)
	}
	return registry
}

// Resolve returns all visible command presentations from one source.
func (r Registry) Resolve(ctx Context) []CommandView {
	views := make([]CommandView, 0, len(r.commands))
	for _, command := range r.commands {
		if command.Hidden {
			continue
		}
		enabled, reason := true, ""
		if command.Enabled != nil {
			enabled, reason = command.Enabled(ctx)
		}
		views = append(views, CommandView{Command: cloneCommand(command), Enabled: enabled, DisabledReason: reason})
	}
	return views
}

// Prefix reports whether strokes are an exact command, a pending prefix, or unmatched.
func (r Registry) Prefix(strokes []string, ctx Context) (CommandView, bool, bool) {
	var exact CommandView
	pending := false
	for _, view := range r.Resolve(ctx) {
		if len(strokes) > len(view.Command.Bindings) || !slices.Equal(strokes, view.Command.Bindings[:len(strokes)]) {
			continue
		}
		if len(strokes) == len(view.Command.Bindings) {
			exact = view
		} else {
			pending = true
		}
	}
	return exact, exact.Command.ID != "", pending
}

// Dispatch resolves one key through the same registry.
func (r Registry) Dispatch(key string, ctx Context) (CommandView, bool) {
	return r.DispatchSequence([]string{key}, ctx)
}

// DispatchSequence resolves a complete key sequence through the registry.
func (r Registry) DispatchSequence(keys []string, ctx Context) (CommandView, bool) {
	for _, view := range r.Resolve(ctx) {
		if slices.Equal(view.Command.Bindings, keys) {
			return view, true
		}
	}
	return CommandView{}, false
}

// Command resolves a command ID through the same enablement source.
func (r Registry) Command(id CommandID, ctx Context) (CommandView, bool) {
	for _, view := range r.Resolve(ctx) {
		if view.Command.ID == id {
			return view, true
		}
	}
	return CommandView{}, false
}

// Palette returns suggested entries first, then stable categories and titles.
func (r Registry) Palette(ctx Context) []CommandView {
	views := r.Resolve(ctx)
	sort.SliceStable(views, func(i, j int) bool {
		if views[i].Command.Suggested != views[j].Command.Suggested {
			return views[i].Command.Suggested
		}
		if views[i].Command.Category != views[j].Command.Category {
			return views[i].Command.Category < views[j].Command.Category
		}
		return views[i].Command.Title < views[j].Command.Title
	})
	return views
}

// WhichKey returns reachable commands beginning with a leader stroke.
func (r Registry) WhichKey(leader string, ctx Context) []CommandView {
	var out []CommandView
	for _, view := range r.Resolve(ctx) {
		if len(view.Command.Bindings) > 1 && view.Command.Bindings[0] == leader {
			out = append(out, view)
		}
	}
	return out
}

// Footer returns compact hints from suggested reachable commands.
func (r Registry) Footer(ctx Context) []string {
	var out []string
	for _, view := range r.Resolve(ctx) {
		if view.Command.Suggested && len(view.Command.Bindings) > 0 {
			out = append(out, strings.Join(view.Command.Bindings, " ")+" "+view.Command.Title)
		}
	}
	return out
}

// Help returns every visible command, including actionable disabled reasons.
func (r Registry) Help(ctx Context) []string {
	views := r.Resolve(ctx)
	out := make([]string, 0, len(views))
	for _, view := range views {
		line := strings.Join(view.Command.Bindings, " ") + "  " + view.Command.Title
		if !view.Enabled {
			line += " — unavailable: " + view.DisabledReason
		}
		out = append(out, line)
	}
	return out
}
