package composer

import (
	"fmt"
	"strings"
	"testing"
)

func TestEditor_EditingSelectionUndoPasteAndHistory(t *testing.T) {
	e := New().Insert("hello world").MoveWord(-1, true).Insert("Harbor").Paste("\r\nnext")
	if got := e.Text(); got != "hello Harbor\nnext" {
		t.Fatalf("text = %q", got)
	}
	e = e.Undo()
	if got := e.Text(); got != "hello Harbor" {
		t.Fatalf("undo = %q", got)
	}
	e = e.Redo()
	if got := e.Text(); got != "hello Harbor\nnext" {
		t.Fatalf("redo = %q", got)
	}
	var err error
	e, _, err = e.Submit()
	if err != nil {
		t.Fatal(err)
	}
	e = e.History(-1)
	if e.Text() != "hello Harbor\nnext" {
		t.Fatalf("history = %q", e.Text())
	}
}

func TestEditor_AutocompleteAttachmentsAndDisabledRules(t *testing.T) {
	all := []Candidate{{Kind: "reference", Value: "@tool"}, {Kind: "command", Value: "/resume"}, {Kind: "command", Value: "/pause"}}
	e := New().Complete(all, "")
	if len(e.Candidates()) != 3 {
		t.Fatal("missing candidates")
	}
	e = e.MoveCompletion(-1).AcceptCompletion()
	if e.Text() == "" {
		t.Fatal("completion not inserted")
	}
	e = e.SetAttachments([]Attachment{{ID: "a", Uploading: true}})
	if _, _, err := e.Submit(); err == nil {
		t.Fatal("uploading attachment submitted")
	}
	e = e.SetAttachments(nil).SetDisabled(true, "reauthentication required")
	if _, _, err := e.Submit(); err == nil {
		t.Fatal("disabled editor submitted")
	}
}

func TestEditor_MovementSelectionStashBoundsAndCompletionWrap(t *testing.T) {
	e := New().SetText("one two\nthree").MoveLine(false, false).MoveWord(-1, true)
	if _, _, ok := e.Selection(); !ok {
		t.Fatal("selection missing")
	}
	e = e.SelectAll().Backspace()
	if e.Text() != "" {
		t.Fatalf("delete selection=%q", e.Text())
	}
	e = e.Insert("draft").Stash()
	if e.Text() != "" {
		t.Fatal("stash did not clear")
	}
	e = e.PopStash()
	if e.Text() != "draft" {
		t.Fatalf("pop=%q", e.Text())
	}
	all := make([]Candidate, 20)
	for i := range all {
		all[i] = Candidate{Kind: "reference", Value: fmt.Sprintf("@item-%02d", i)}
	}
	e = e.Complete(all, "item").MoveCompletion(-1).AcceptCompletion()
	if len(e.Candidates()) != MaxAutocompleteRows || !strings.Contains(e.Text(), "@item-09") {
		t.Fatalf("completion cap/wrap: %d %q", len(e.Candidates()), e.Text())
	}
	for i := range MaxHistory + 5 {
		e = e.SetText(fmt.Sprint(i))
		var err error
		e, _, err = e.Submit()
		if err != nil {
			t.Fatal(err)
		}
	}
	e = e.History(-1)
	if e.Text() != fmt.Sprint(MaxHistory+4) {
		t.Fatalf("newest history=%q", e.Text())
	}
}

func TestEditor_AccessorsMovementAndNoopEdges(t *testing.T) {
	e := New().Insert("abc").Move(-2, true)
	if e.Cursor() != 1 {
		t.Fatalf("cursor=%d", e.Cursor())
	}
	e = e.Move(99, false).Backspace().SetRunning(true).SetAttachments([]Attachment{{ID: "a"}})
	if e.Text() != "ab" || !e.Running() || len(e.Attachments()) != 1 {
		t.Fatalf("editor=%q running=%v attachments=%d", e.Text(), e.Running(), len(e.Attachments()))
	}
	if New().Undo().Redo().Backspace().PopStash().History(-1).AcceptCompletion().Text() != "" {
		t.Fatal("no-op edge changed text")
	}
	if _, _, err := New().Submit(); err == nil {
		t.Fatal("empty submit accepted")
	}
}

func TestEditor_History_PreservesWorkingDraftAcrossBrowsing(t *testing.T) {
	e := New()
	for _, h := range []string{"alpha", "beta"} {
		e = e.SetText(h)
		var err error
		if e, _, err = e.Submit(); err != nil {
			t.Fatal(err)
		}
	}
	// An unsent, in-progress working draft.
	e = e.Insert("work in progress")
	if e.Text() != "work in progress" {
		t.Fatalf("draft=%q", e.Text())
	}
	// Browse up through history.
	e = e.History(-1)
	if e.Text() != "beta" {
		t.Fatalf("up=%q", e.Text())
	}
	e = e.History(-1)
	if e.Text() != "alpha" {
		t.Fatalf("up2=%q", e.Text())
	}
	// Page back down: alpha -> beta -> the original working draft (not nil).
	e = e.History(1)
	if e.Text() != "beta" {
		t.Fatalf("down=%q", e.Text())
	}
	e = e.History(1)
	if e.Text() != "work in progress" {
		t.Fatalf("restored draft=%q, want original working draft", e.Text())
	}
	if e.Cursor() != len([]rune("work in progress")) {
		t.Fatalf("cursor=%d, want end of restored draft", e.Cursor())
	}
	if _, _, ok := e.Selection(); ok {
		t.Fatal("anchor not reset on restored draft")
	}
}

func TestEditor_History_RealEditClearsParkedDraft(t *testing.T) {
	e := New().SetText("saved")
	var err error
	if e, _, err = e.Submit(); err != nil {
		t.Fatal(err)
	}
	e = e.Insert("typing") // working draft
	e = e.History(-1)      // browse to the sole history entry
	if e.Text() != "saved" {
		t.Fatalf("up=%q", e.Text())
	}
	e = e.Insert("X") // real edit while browsing must discard the parked draft
	if e.Text() != "savedX" {
		t.Fatalf("edit=%q", e.Text())
	}
	// Re-browsing must not resurrect the stale "typing" draft.
	e = e.History(-1)
	if e.Text() != "saved" {
		t.Fatalf("re-up=%q", e.Text())
	}
	e = e.History(1)
	if e.Text() == "typing" {
		t.Fatal("stale parked draft resurrected after real edit")
	}
	if e.Text() != "savedX" {
		t.Fatalf("parked draft=%q, want the post-edit content", e.Text())
	}
}

func TestEditor_Insert_CoalescesTypingIntoOneUndoGroup(t *testing.T) {
	e := New()
	for _, r := range "hello" {
		e = e.Insert(string(r))
	}
	if e.Text() != "hello" {
		t.Fatalf("typed=%q", e.Text())
	}
	// A single undo must remove the whole coalesced typing run, not one rune.
	e = e.Undo()
	if e.Text() != "" {
		t.Fatalf("coalesced undo=%q, want empty (whole run removed)", e.Text())
	}

	// Whitespace and non-typing edits break the coalescing run into groups.
	e = New()
	for _, r := range "ab cd" {
		e = e.Insert(string(r))
	}
	e = e.Undo()
	if e.Text() != "ab " {
		t.Fatalf("whitespace-boundary undo=%q, want %q", e.Text(), "ab ")
	}
}

func TestEditor_RestoreLocalBoundsHistoryAndStash(t *testing.T) {
	history := make([]string, MaxHistory+5)
	stash := make([]string, MaxStash+5)
	for i := range history {
		history[i], stash[i] = fmt.Sprint(i), fmt.Sprint(i)
	}
	e := New().RestoreLocal(history, stash).History(-1).PopStash()
	if len(e.HistoryEntries()) != MaxHistory || len(e.StashEntries()) != MaxStash-1 || e.Text() != fmt.Sprint(MaxStash+4) {
		t.Fatalf("history=%d stash=%d text=%q", len(e.HistoryEntries()), len(e.StashEntries()), e.Text())
	}
}
