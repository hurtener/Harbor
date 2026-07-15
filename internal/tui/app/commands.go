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
type Context struct{ ModalOpen, Connected bool }

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
	}
	return Registry{commands: copyCommands}, nil
}

// DefaultRegistry returns the fixture shell's complete command source.
func DefaultRegistry() Registry {
	connected := func(ctx Context) (bool, string) {
		if !ctx.Connected {
			return false, "requires an attached Runtime (available in a later release)"
		}
		return true, ""
	}
	registry, err := NewRegistry(
		Command{ID: "palette", Title: "Command palette", Description: "Find an available action", Category: "Application", Bindings: []string{"ctrl+p"}, Suggested: true},
		Command{ID: "help", Title: "Keyboard help", Description: "Show all reachable commands", Category: "Application", Bindings: []string{"?"}},
		Command{ID: "sidebar", Title: "Toggle runtime context", Description: "Show the fixed context sidebar", Category: "View", Bindings: []string{"ctrl+x", "s"}, Suggested: true},
		Command{ID: "theme", Title: "Switch theme", Description: "Choose dark, light, or automatic mode", Category: "View", Bindings: []string{"ctrl+x", "t"}},
		Command{ID: "sessions", Title: "Switch session", Description: "Attach to another authorized session", Category: "Session", Bindings: []string{"ctrl+x", "l"}, Enabled: connected},
		Command{ID: "submit", Title: "Submit turn", Description: "Start work in the active session", Category: "Session", Bindings: []string{"enter"}, Enabled: connected},
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
