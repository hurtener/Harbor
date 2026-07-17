package conversation

import (
	"testing"

	"github.com/hurtener/Harbor/internal/tui/projection"
)

func blocks(ids ...string) []projection.Block {
	out := make([]projection.Block, len(ids))
	for i, id := range ids {
		out[i] = projection.Block{ID: id, Kind: "text", Text: id}
	}
	return out
}

// A scrolled (non-follow) reader must stay anchored to the SAME block by ID
// when an earlier block is deleted, rather than shifting by one index.
func TestTranscript_ReplaceKeepsSemanticAnchorWhenEarlierBlockDeleted(t *testing.T) {
	p := projection.Projection{Blocks: blocks("a", "b", "c", "d", "e")}
	view := NewTranscript(p).Scroll(-2) // move off the tail onto block "c"
	if view.Follow {
		t.Fatalf("expected scrolled reader to disable follow: %#v", view)
	}
	if got := view.SelectedID(); got != "c" {
		t.Fatalf("anchor id before replace = %q, want c", got)
	}

	// Delete an EARLIER block ("b"); the shape now shifts every later index down.
	next := projection.Projection{Blocks: blocks("a", "c", "d", "e")}
	view = view.Replace(next)

	if got := view.SelectedID(); got != "c" {
		t.Fatalf("anchor id after deletion = %q, want c (semantic offset shifted)", got)
	}
	if view.Selected != 1 {
		t.Fatalf("Selected = %d, want 1 (re-resolved index of block c)", view.Selected)
	}
	if view.Follow {
		t.Fatalf("scrolled reader must not be yanked into follow: %#v", view)
	}
}

// When the anchored block itself is deleted, selection falls back to the
// nearest surviving neighbor (preferring the preceding block) without jumping
// to the top or bottom.
func TestTranscript_ReplaceFallsBackToNeighborWhenAnchorDeleted(t *testing.T) {
	p := projection.Projection{Blocks: blocks("a", "b", "c", "d", "e")}
	view := NewTranscript(p).Scroll(-2) // anchored on "c"
	if view.SelectedID() != "c" {
		t.Fatalf("precondition anchor = %q", view.SelectedID())
	}

	// Delete the anchored block "c" (classic pause.resumed / tool.approved).
	next := projection.Projection{Blocks: blocks("a", "b", "d", "e")}
	view = view.Replace(next)

	if got := view.SelectedID(); got != "b" {
		t.Fatalf("fallback anchor = %q, want b (nearest preceding survivor)", got)
	}
	if view.Selected != 1 {
		t.Fatalf("Selected = %d, want 1", view.Selected)
	}
	if view.Follow {
		t.Fatalf("fallback must not enable follow / yank to bottom: %#v", view)
	}
	if view.Selected == 0 || view.Selected == len(next.Blocks)-1 {
		t.Fatalf("selection jumped to an edge (%d) rather than a sensible neighbor", view.Selected)
	}
}

// If every preceding block is also gone, fall back forward to the nearest
// surviving following block rather than clamping blindly.
func TestTranscript_ReplaceFallsBackForwardWhenPrecedingGone(t *testing.T) {
	p := projection.Projection{Blocks: blocks("a", "b", "c", "d")}
	view := NewTranscript(p).Scroll(-2) // anchored on "b"
	if view.SelectedID() != "b" {
		t.Fatalf("precondition anchor = %q", view.SelectedID())
	}
	// Remove the anchor "b" and everything before it.
	next := projection.Projection{Blocks: blocks("c", "d")}
	view = view.Replace(next)
	if got := view.SelectedID(); got != "c" {
		t.Fatalf("forward fallback anchor = %q, want c", got)
	}
}

// Follow-tail behavior is unchanged: a following transcript bottom-anchors on
// growth (and on shrink) and reports no unread new output.
func TestTranscript_ReplaceFollowStaysAtTail(t *testing.T) {
	p := projection.Projection{Blocks: blocks("a", "b", "c")}
	view := NewTranscript(p)
	if !view.Follow || view.Selected != 2 || view.SelectedID() != "c" {
		t.Fatalf("new transcript should follow the tail: %#v", view)
	}

	// Growth: tail advances, still following, no unread accounting.
	grew := projection.Projection{Blocks: blocks("a", "b", "c", "d", "e")}
	view = view.Replace(grew)
	if !view.Follow || view.Selected != 4 || view.SelectedID() != "e" {
		t.Fatalf("follow did not track new tail: %#v", view)
	}
	if view.NewOutput != 0 {
		t.Fatalf("following reader should have no unread new output, got %d", view.NewOutput)
	}

	// Shrink while following: tail re-clamps, no out-of-range Selected.
	shrank := projection.Projection{Blocks: blocks("a", "b", "c")}
	view = view.Replace(shrank)
	if !view.Follow || view.Selected != 2 || view.SelectedID() != "c" {
		t.Fatalf("follow did not re-clamp to new tail on shrink: %#v", view)
	}
}

// A scrolled reader is never yanked to the bottom by a growing projection; the
// existing NewOutput accounting is preserved.
func TestTranscript_ReplaceScrolledReaderNotYankedOnGrowth(t *testing.T) {
	p := projection.Projection{Blocks: blocks("a", "b", "c")}
	view := NewTranscript(p).Scroll(-2) // anchored on "a"
	if view.SelectedID() != "a" {
		t.Fatalf("precondition anchor = %q", view.SelectedID())
	}
	grew := projection.Projection{Blocks: blocks("a", "b", "c", "d")}
	view = view.Replace(grew)
	if view.Follow {
		t.Fatalf("scrolled reader yanked into follow: %#v", view)
	}
	if view.SelectedID() != "a" || view.Selected != 0 {
		t.Fatalf("anchor drifted: id=%q selected=%d", view.SelectedID(), view.Selected)
	}
	if view.NewOutput != 1 {
		t.Fatalf("NewOutput accounting = %d, want 1", view.NewOutput)
	}
}

// SelectByID restores an anchor by stable ID (the app-layer ScrollBlockID
// restore path) and keeps the reader off the tail.
func TestTranscript_SelectByIDRestoresAnchor(t *testing.T) {
	p := projection.Projection{Blocks: blocks("a", "b", "c", "d")}
	view := NewTranscript(p) // follows tail "d"
	restored := view.SelectByID("b")
	if restored.SelectedID() != "b" || restored.Selected != 1 {
		t.Fatalf("SelectByID did not anchor to b: id=%q selected=%d", restored.SelectedID(), restored.Selected)
	}
	if restored.Follow {
		t.Fatalf("restoring a mid-list anchor must disable follow: %#v", restored)
	}
	// Unknown id is a no-op preserving the prior anchor.
	unchanged := restored.SelectByID("zzz")
	if unchanged.SelectedID() != "b" || unchanged.Selected != 1 {
		t.Fatalf("unknown SelectByID mutated anchor: %#v", unchanged)
	}
	// Restoring the tail re-enables follow.
	tail := view.SelectByID("d")
	if !tail.Follow || tail.SelectedID() != "d" {
		t.Fatalf("restoring the tail should follow: %#v", tail)
	}
}

// NextMatch walks the recorded match set (not semantic blocks) and clamps at
// the ends like Jump.
func TestTranscript_NextMatchWalksMatchSet(t *testing.T) {
	p := projection.Projection{Blocks: []projection.Block{
		{ID: "0", Kind: "text", Text: "alpha find"},
		{ID: "1", Kind: "text", Text: "beta"},
		{ID: "2", Kind: "text", Text: "gamma find"},
		{ID: "3", Kind: "text", Text: "delta"},
		{ID: "4", Kind: "text", Text: "epsilon find"},
	}}
	view := NewTranscript(p).Search("find")
	if len(view.Matches) != 3 || view.Selected != 0 || view.SelectedID() != "0" {
		t.Fatalf("search should land on first match: %#v", view)
	}
	// Forward across the match set, skipping non-matching blocks.
	view = view.NextMatch(1)
	if view.Selected != 2 || view.SelectedID() != "2" {
		t.Fatalf("NextMatch(1) = %d/%q, want 2/2", view.Selected, view.SelectedID())
	}
	view = view.NextMatch(1)
	if view.Selected != 4 || view.SelectedID() != "4" {
		t.Fatalf("NextMatch(1) = %d/%q, want 4/4", view.Selected, view.SelectedID())
	}
	// Clamp at the last match (no wrap).
	view = view.NextMatch(1)
	if view.Selected != 4 {
		t.Fatalf("NextMatch past last match should clamp, got %d", view.Selected)
	}
	// Backward through the set.
	view = view.NextMatch(-1)
	if view.Selected != 2 || view.SelectedID() != "2" {
		t.Fatalf("NextMatch(-1) = %d/%q, want 2/2", view.Selected, view.SelectedID())
	}
	// No active search is a no-op.
	cleared := NewTranscript(p).NextMatch(1)
	if cleared.Selected != NewTranscript(p).Selected {
		t.Fatalf("NextMatch without matches mutated selection: %d", cleared.Selected)
	}
}

// Scroll and Jump keep the tracked anchor ID consistent with Selected so the
// next Replace re-resolves against the right block.
func TestTranscript_ScrollAndJumpTrackAnchorID(t *testing.T) {
	p := projection.Projection{Blocks: []projection.Block{
		{ID: "u", Kind: "user", Text: "q"},
		{ID: "r", Kind: "reasoning", Text: "think"},
		{ID: "t", Kind: "tool", Tool: "lookup"},
		{ID: "a", Kind: "text", Text: "answer"},
	}}
	view := NewTranscript(p).Scroll(-1) // onto "t"
	if view.SelectedID() != "t" {
		t.Fatalf("scroll anchor = %q, want t", view.SelectedID())
	}
	view.ShowReasoning = false
	view.ShowTools = false
	view = view.Scroll(-3) // clamp to top "u"
	if view.SelectedID() != "u" {
		t.Fatalf("scroll-to-top anchor = %q, want u", view.SelectedID())
	}
	view = view.Jump(1) // next visible block skips reasoning+tool -> "a"
	if view.Selected != 3 || view.SelectedID() != "a" {
		t.Fatalf("jump anchor = %d/%q, want 3/a", view.Selected, view.SelectedID())
	}
}
