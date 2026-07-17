package app

import (
	"strings"
	"testing"
)

func TestRegistry_OneSourceDrivesEveryPresentationAndDisabledReasons(t *testing.T) {
	r := DefaultRegistry()
	ctx := Context{}
	palette := r.Palette(ctx)
	help := r.Help(ctx)
	footer := r.Footer(ctx)
	which := r.WhichKey("ctrl+x", ctx)
	if len(palette) != len(help) || len(footer) == 0 || len(which) < 2 {
		t.Fatalf("palette=%d help=%d footer=%d which=%d", len(palette), len(help), len(footer), len(which))
	}
	view, ok := r.Dispatch("enter", ctx)
	if !ok || view.Enabled || !strings.Contains(view.DisabledReason, "live authenticated") {
		t.Fatalf("disabled dispatch=%#v %v", view, ok)
	}
	if _, err := NewRegistry(Command{ID: "x", Title: "x"}, Command{ID: "x", Title: "duplicate"}); err == nil {
		t.Fatal("duplicate accepted")
	}
	if _, err := NewRegistry(Command{ID: "x", Title: "x", Bindings: []string{"ctrl+x", "x"}}, Command{ID: "y", Title: "y", Bindings: []string{"ctrl+x", "x"}}); err == nil {
		t.Fatal("duplicate binding accepted")
	}
	if sequence, found := r.DispatchSequence([]string{"ctrl+x", "s"}, ctx); !found || sequence.Command.ID != "sidebar" {
		t.Fatal("leader sequence did not dispatch")
	}
}

func TestSelectAndFocus_OneModelOwnsFilteringPagingContextAndRestore(t *testing.T) {
	items := []SelectItem{{ID: "1", Category: "A", Title: "Alpha", Current: true, Actions: []CommandID{"help"}}, {ID: "2", Category: "B", Title: "Beta", Actions: []CommandID{"quit"}}, {ID: "3", Category: "A", Title: "Alpine"}}
	modal := NewSelect("Pick", items, "composer").SetCategory("A").SetQuery("alp").Move(1)
	if len(modal.Visible()) != 2 || modal.Current != 1 {
		t.Fatalf("modal=%#v visible=%#v", modal, modal.Visible())
	}
	stack := NewFocusStack("composer").Push(modal)
	if stack.Focus() != "modal" {
		t.Fatal("modal did not focus")
	}
	stack, focus, ok := stack.Pop()
	if !ok || focus != "composer" || stack.Focus() != "composer" {
		t.Fatalf("restore=%q %#v", focus, stack)
	}
	modal = NewSelect("Paged", items, "composer")
	modal.PageSize = 1
	modal = modal.PageBy(1)
	if len(modal.Visible()) != 1 {
		t.Fatal("paging failed")
	}
	stack = NewFocusStack("composer")
	if _, _, ok = stack.Pop(); ok {
		t.Fatal("empty stack popped")
	}
	stack = stack.Push(modal).ReplaceTop(modal.SetQuery("beta"))
	if top, exists := stack.Top(); !exists || top.Query != "beta" {
		t.Fatal("replace top failed")
	}
	empty := NewSelect("Empty", nil, "composer").Move(1)
	if empty.Current != 0 {
		t.Fatal("empty selection moved")
	}
	if !modal.BackdropClose(false) || modal.BackdropClose(true) {
		t.Fatal("backdrop selection behavior changed")
	}
}
