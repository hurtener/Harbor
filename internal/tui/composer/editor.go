// Package composer implements the TUI's local editor-quality composer model.
package composer

import (
	"errors"
	"sort"
	"strings"
	"unicode"
)

const (
	// MaxHistory bounds submitted local prompt history.
	MaxHistory = 50
	// MaxStash bounds explicitly stashed drafts.
	MaxStash = 50
	// MaxAutocompleteRows is the binding popup height.
	MaxAutocompleteRows = 10
)

var ErrDisabled = errors.New("composer: submission disabled")

// Attachment is a local upload reference and its optional Runtime disposition hint.
type Attachment struct {
	ID, Name, Path, MIME, Disposition string
	Uploading, Failed                 bool
}

// Candidate is one slash-command or structured-reference completion.
type Candidate struct{ Kind, Value, Detail string }

// Submission is an immutable snapshot of one validated draft and its uploaded
// artifact references. Preparing it never mutates editor state.
type Submission struct {
	Text        string
	Attachments []Attachment
}

type snapshot struct {
	text   []rune
	cursor int
	anchor int
}

// Editor is a value-updated, bounded composer model. It contains interaction
// state only and never stores Runtime conversation rows.
type Editor struct {
	text              []rune
	cursor, anchor    int
	undo, redo        []snapshot
	history, stash    []string
	historyIndex      int
	pendingDraft      []rune
	coalescing        bool
	coalesceAt        int
	attachments       []Attachment
	disabled, running bool
	disabledReason    string
	candidates        []Candidate
	completionIndex   int
}

// New returns an empty composer.
func New() Editor { return Editor{anchor: -1, historyIndex: -1} }

func (e Editor) clone() Editor {
	e.text = append([]rune(nil), e.text...)
	e.pendingDraft = append([]rune(nil), e.pendingDraft...)
	e.undo = cloneSnapshots(e.undo)
	e.redo = cloneSnapshots(e.redo)
	e.history = append([]string(nil), e.history...)
	e.stash = append([]string(nil), e.stash...)
	e.attachments = append([]Attachment(nil), e.attachments...)
	e.candidates = append([]Candidate(nil), e.candidates...)
	return e
}

func cloneSnapshots(in []snapshot) []snapshot {
	out := make([]snapshot, len(in))
	for i := range in {
		out[i] = snapshot{append([]rune(nil), in[i].text...), in[i].cursor, in[i].anchor}
	}
	return out
}

func (e Editor) snap() snapshot { return snapshot{append([]rune(nil), e.text...), e.cursor, e.anchor} }
func (e *Editor) checkpoint() {
	e.undo = append(e.undo, e.snap())
	if len(e.undo) > MaxHistory {
		e.undo = append([]snapshot(nil), e.undo[len(e.undo)-MaxHistory:]...)
	}
	e.redo = nil
}

// resetHistoryNav abandons any in-progress history browsing so a stale parked
// draft cannot resurrect after the content has been mutated by a real edit.
func (e *Editor) resetHistoryNav() {
	e.historyIndex = -1
	e.pendingDraft = nil
}

// hasSelection reports whether a non-empty selection is active.
func (e Editor) hasSelection() bool {
	_, _, ok := e.Selection()
	return ok
}

// Text returns the complete multiline draft.
func (e Editor) Text() string { return string(e.text) }

// Cursor returns the rune offset.
func (e Editor) Cursor() int { return e.cursor }

// BeforeCursor returns the draft prefix ending at the cursor.
func (e Editor) BeforeCursor() string { return string(e.text[:e.cursor]) }

// Selection returns normalized rune offsets and whether a selection exists.
func (e Editor) Selection() (int, int, bool) {
	if e.anchor < 0 || e.anchor == e.cursor {
		return 0, 0, false
	}
	if e.anchor < e.cursor {
		return e.anchor, e.cursor, true
	}
	return e.cursor, e.anchor, true
}

// SetText replaces the draft and resets undo traversal.
func (e Editor) SetText(value string) Editor {
	e = e.clone()
	e.checkpoint()
	e.resetHistoryNav()
	e.coalescing = false
	e.text = []rune(value)
	e.cursor = len(e.text)
	e.anchor = -1
	return e
}

// Insert inserts text at the cursor, replacing a selection. Newlines are retained.
//
// Consecutive single-character non-whitespace insertions at the same position
// coalesce into one undo group, so ordinary typing does not exhaust the bounded
// undo budget one rune at a time.
func (e Editor) Insert(value string) Editor {
	e = e.clone()
	r := []rune(value)
	coalesce := e.coalescing && e.coalesceAt == e.cursor && !e.hasSelection() &&
		len(r) == 1 && !unicode.IsSpace(r[0])
	if !coalesce {
		e.checkpoint()
	}
	e.resetHistoryNav()
	e.deleteSelection()
	e.text = append(e.text[:e.cursor], append(r, e.text[e.cursor:]...)...)
	e.cursor += len(r)
	e.anchor = -1
	if len(r) == 1 && !unicode.IsSpace(r[0]) {
		e.coalescing, e.coalesceAt = true, e.cursor
	} else {
		e.coalescing = false
	}
	return e
}

// Paste applies bracketed-paste content as one undoable edit.
func (e Editor) Paste(value string) Editor { return e.Insert(strings.ReplaceAll(value, "\r\n", "\n")) }

// Move moves by rune count and optionally extends selection.
func (e Editor) Move(delta int, selectText bool) Editor {
	e = e.clone()
	if selectText && e.anchor < 0 {
		e.anchor = e.cursor
	}
	if !selectText {
		e.anchor = -1
	}
	e.cursor = max(0, min(len(e.text), e.cursor+delta))
	return e
}

// MoveWord moves to the adjacent word boundary.
func (e Editor) MoveWord(direction int, selectText bool) Editor {
	e = e.clone()
	if selectText && e.anchor < 0 {
		e.anchor = e.cursor
	}
	if !selectText {
		e.anchor = -1
	}
	if direction < 0 {
		for e.cursor > 0 && unicode.IsSpace(e.text[e.cursor-1]) {
			e.cursor--
		}
		for e.cursor > 0 && !unicode.IsSpace(e.text[e.cursor-1]) {
			e.cursor--
		}
	} else {
		for e.cursor < len(e.text) && !unicode.IsSpace(e.text[e.cursor]) {
			e.cursor++
		}
		for e.cursor < len(e.text) && unicode.IsSpace(e.text[e.cursor]) {
			e.cursor++
		}
	}
	return e
}

// MoveLine moves to a line boundary.
func (e Editor) MoveLine(end, selectText bool) Editor {
	e = e.clone()
	if selectText && e.anchor < 0 {
		e.anchor = e.cursor
	}
	if !selectText {
		e.anchor = -1
	}
	if end {
		for e.cursor < len(e.text) && e.text[e.cursor] != '\n' {
			e.cursor++
		}
	} else {
		for e.cursor > 0 && e.text[e.cursor-1] != '\n' {
			e.cursor--
		}
	}
	return e
}

// SelectAll selects the complete draft.
func (e Editor) SelectAll() Editor { e = e.clone(); e.anchor = 0; e.cursor = len(e.text); return e }

// Backspace removes the selection or preceding rune.
func (e Editor) Backspace() Editor {
	e = e.clone()
	if len(e.text) == 0 {
		return e
	}
	e.checkpoint()
	e.resetHistoryNav()
	e.coalescing = false
	if e.deleteSelection() {
		return e
	}
	if e.cursor > 0 {
		e.text = append(e.text[:e.cursor-1], e.text[e.cursor:]...)
		e.cursor--
	}
	return e
}

func (e *Editor) deleteSelection() bool {
	start, end, ok := e.Selection()
	if !ok {
		return false
	}
	e.text = append(e.text[:start], e.text[end:]...)
	e.cursor = start
	e.anchor = -1
	return true
}

// Undo restores the preceding edit snapshot.
func (e Editor) Undo() Editor {
	e = e.clone()
	e.coalescing = false
	if len(e.undo) == 0 {
		return e
	}
	e.redo = append(e.redo, e.snap())
	s := e.undo[len(e.undo)-1]
	e.undo = e.undo[:len(e.undo)-1]
	e.text, e.cursor, e.anchor = s.text, s.cursor, s.anchor
	return e
}

// Redo reapplies an undone edit.
func (e Editor) Redo() Editor {
	e = e.clone()
	e.coalescing = false
	if len(e.redo) == 0 {
		return e
	}
	e.undo = append(e.undo, e.snap())
	s := e.redo[len(e.redo)-1]
	e.redo = e.redo[:len(e.redo)-1]
	e.text, e.cursor, e.anchor = s.text, s.cursor, s.anchor
	return e
}

// PrepareSubmission validates and snapshots a turn without mutating the draft.
func (e Editor) PrepareSubmission() (Submission, error) {
	if e.disabled {
		return Submission{}, errors.Join(ErrDisabled, errors.New(e.disabledReason))
	}
	for _, a := range e.attachments {
		if a.Uploading || a.Failed {
			return Submission{}, errors.Join(ErrDisabled, errors.New("attachment is not ready"))
		}
	}
	value := strings.TrimSpace(string(e.text))
	if value == "" && len(e.attachments) == 0 {
		return Submission{}, errors.Join(ErrDisabled, errors.New("draft is empty"))
	}
	return Submission{Text: value, Attachments: append([]Attachment(nil), e.attachments...)}, nil
}

// CommitSubmission records history and clears the exact prepared payload only
// after the Runtime accepted it. A changed draft is retained.
func (e Editor) CommitSubmission(submission Submission) Editor {
	e = e.clone()
	if submission.Text != "" {
		e.history = appendBounded(e.history, submission.Text, MaxHistory)
	}
	if string(e.text) == submission.Text && attachmentsEqual(e.attachments, submission.Attachments) {
		e.text, e.undo, e.redo, e.attachments, e.pendingDraft = nil, nil, nil, nil, nil
		e.cursor, e.anchor, e.historyIndex = 0, -1, -1
		e.coalescing = false
	}
	return e
}

// Submit is retained for local queue callers that commit immediately.
func (e Editor) Submit() (Editor, string, error) {
	submission, err := e.PrepareSubmission()
	if err != nil {
		return e, "", err
	}
	return e.CommitSubmission(submission), submission.Text, nil
}

// SetDisabled changes explicit availability and its actionable reason.
func (e Editor) SetDisabled(disabled bool, reason string) Editor {
	e = e.clone()
	e.disabled, e.disabledReason = disabled, reason
	return e
}

// DisabledReason returns the actionable reason submission is unavailable.
func (e Editor) DisabledReason() string { return e.disabledReason }

// SetRunning marks whether submission creates a local follow-up.
func (e Editor) SetRunning(running bool) Editor { e = e.clone(); e.running = running; return e }

// Running reports follow-up queue posture.
func (e Editor) Running() bool { return e.running }

// History moves through bounded local prompt history, parking the in-progress
// working draft at the bottom of the stack so paging back down returns the user
// to exactly what they were typing. The parked draft is captured the moment
// browsing begins and is discarded once a real edit mutates the content.
func (e Editor) History(direction int) Editor {
	e = e.clone()
	e.coalescing = false
	if len(e.history) == 0 {
		return e
	}
	if e.historyIndex < 0 {
		// Entering history navigation: stash the live draft so it can be
		// restored, and start just past the newest entry.
		e.pendingDraft = append([]rune(nil), e.text...)
		e.historyIndex = len(e.history)
	}
	e.historyIndex = max(0, min(len(e.history), e.historyIndex+direction))
	if e.historyIndex == len(e.history) {
		e.text = append([]rune(nil), e.pendingDraft...)
	} else {
		e.text = []rune(e.history[e.historyIndex])
	}
	e.cursor = len(e.text)
	e.anchor = -1
	return e
}

// Stash stores the current draft and clears it.
func (e Editor) Stash() Editor {
	e = e.clone()
	if v := string(e.text); v != "" {
		e.stash = appendBounded(e.stash, v, MaxStash)
		e.text = nil
		e.cursor = 0
		e.resetHistoryNav()
		e.coalescing = false
	}
	return e
}

// PopStash restores the newest stashed draft.
func (e Editor) PopStash() Editor {
	e = e.clone()
	if len(e.stash) == 0 {
		return e
	}
	e.text = []rune(e.stash[len(e.stash)-1])
	e.stash = e.stash[:len(e.stash)-1]
	e.cursor = len(e.text)
	e.resetHistoryNav()
	e.coalescing = false
	return e
}

// SetAttachments replaces local attachment lifecycle state.
func (e Editor) SetAttachments(values []Attachment) Editor {
	e = e.clone()
	e.attachments = append([]Attachment(nil), values...)
	return e
}

// Attachments returns an isolated copy.
func (e Editor) Attachments() []Attachment { return append([]Attachment(nil), e.attachments...) }

// HistoryEntries returns an isolated bounded history snapshot for local persistence.
func (e Editor) HistoryEntries() []string { return append([]string(nil), e.history...) }

// StashEntries returns an isolated bounded stash snapshot for local persistence.
func (e Editor) StashEntries() []string { return append([]string(nil), e.stash...) }

// RestoreLocal restores bounded interaction-only history and stash state.
func (e Editor) RestoreLocal(history, stash []string) Editor {
	e = e.clone()
	e.history = appendBoundedSlice(history, MaxHistory)
	e.stash = appendBoundedSlice(stash, MaxStash)
	e.historyIndex = -1
	e.pendingDraft = nil
	e.coalescing = false
	return e
}

// Complete filters, sorts, and caps slash/reference candidates to ten rows.
func (e Editor) Complete(all []Candidate, query string) Editor {
	e = e.clone()
	query = strings.ToLower(strings.TrimSpace(query))
	e.candidates = nil
	for _, c := range all {
		if query == "" || strings.Contains(strings.ToLower(c.Value+" "+c.Detail), query) {
			e.candidates = append(e.candidates, c)
		}
	}
	sort.SliceStable(e.candidates, func(i, j int) bool {
		if e.candidates[i].Kind != e.candidates[j].Kind {
			return e.candidates[i].Kind < e.candidates[j].Kind
		}
		return e.candidates[i].Value < e.candidates[j].Value
	})
	if len(e.candidates) > MaxAutocompleteRows {
		e.candidates = e.candidates[:MaxAutocompleteRows]
	}
	e.completionIndex = 0
	return e
}

// Candidates returns the fixed-height autocomplete rows.
func (e Editor) Candidates() []Candidate { return append([]Candidate(nil), e.candidates...) }

// SelectedCandidate returns the current autocomplete row.
func (e Editor) SelectedCandidate() (Candidate, bool) {
	if len(e.candidates) == 0 || e.completionIndex < 0 || e.completionIndex >= len(e.candidates) {
		return Candidate{}, false
	}
	return e.candidates[e.completionIndex], true
}

// MoveCompletion wraps autocomplete selection.
func (e Editor) MoveCompletion(delta int) Editor {
	e = e.clone()
	if len(e.candidates) > 0 {
		e.completionIndex = (e.completionIndex + delta%len(e.candidates) + len(e.candidates)) % len(e.candidates)
	}
	return e
}

// AcceptCompletion inserts the selected completion.
func (e Editor) AcceptCompletion() Editor {
	if len(e.candidates) == 0 {
		return e
	}
	return e.Insert(e.candidates[e.completionIndex].Value + " ")
}

func appendBounded(values []string, value string, limit int) []string {
	values = append(values, value)
	if len(values) > limit {
		values = append([]string(nil), values[len(values)-limit:]...)
	}
	return values
}

func appendBoundedSlice(values []string, limit int) []string {
	if len(values) > limit {
		values = values[len(values)-limit:]
	}
	return append([]string(nil), values...)
}

func attachmentsEqual(left, right []Attachment) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
