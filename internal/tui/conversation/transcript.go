package conversation

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/tui/projection"
)

var (
	ErrExportWrite = errors.New("tui export: write failed")
	ErrExportEmpty = errors.New("tui export: transcript is empty")
)

// Transcript tracks viewport behavior over the canonical projection.
type Transcript struct {
	Projection                                        projection.Projection
	Selected                                          int
	Follow                                            bool
	NewOutput                                         int
	Query                                             string
	Matches                                           []int
	Compact, ShowReasoning, ShowTools, ShowTimestamps bool
}

// NewTranscript starts at the semantic bottom.
func NewTranscript(p projection.Projection) Transcript {
	return Transcript{Projection: p, Selected: max(0, len(p.Blocks)-1), Follow: true, ShowReasoning: true, ShowTools: true}
}

// Replace applies a new projection without yanking a scrolled-away reader.
func (t Transcript) Replace(p projection.Projection) Transcript {
	old := len(t.Projection.Blocks)
	t.Projection = p
	if len(p.Blocks) > old {
		if t.Follow {
			t.Selected = max(0, len(p.Blocks)-1)
			t.NewOutput = 0
		} else {
			t.NewOutput += len(p.Blocks) - old
		}
	}
	return t.Search(t.Query)
}

// Scroll moves semantic selection and disables follow unless at bottom.
func (t Transcript) Scroll(delta int) Transcript {
	if len(t.Projection.Blocks) == 0 {
		return t
	}
	t.Selected = max(0, min(len(t.Projection.Blocks)-1, t.Selected+delta))
	t.Follow = t.Selected == len(t.Projection.Blocks)-1
	if t.Follow {
		t.NewOutput = 0
	}
	return t
}

// Jump moves to the next/previous semantic block matching visible preferences.
func (t Transcript) Jump(direction int) Transcript {
	if direction == 0 {
		return t
	}
	for i := t.Selected + direction; i >= 0 && i < len(t.Projection.Blocks); i += direction {
		b := t.Projection.Blocks[i]
		if (b.Kind != "reasoning" || t.ShowReasoning) && (b.Kind != "tool" || t.ShowTools) {
			t.Selected = i
			t.Follow = i == len(t.Projection.Blocks)-1
			return t
		}
	}
	return t
}

// Search incrementally records deterministic block matches.
func (t Transcript) Search(query string) Transcript {
	t.Query = query
	t.Matches = nil
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return t
	}
	for i, b := range t.Projection.Blocks {
		if strings.Contains(strings.ToLower(b.Text+" "+b.Tool+" "+b.EventType+" "+b.Kind), q) {
			t.Matches = append(t.Matches, i)
		}
	}
	if len(t.Matches) > 0 {
		t.Selected = t.Matches[0]
		t.Follow = false
	}
	return t
}

// ExportOptions independently selects detail without mutating display preferences.
type ExportOptions struct{ Reasoning, Tools, Metadata, RawEvents bool }

// Export writes the redacted projection as stable Markdown.
func (t Transcript) Export(w io.Writer, options ExportOptions) error {
	if len(t.Projection.Blocks) == 0 {
		return ErrExportEmpty
	}
	if _, err := fmt.Fprintf(w, "# Harbor session %s\n\n", t.Projection.Identity.Session); err != nil {
		return fmt.Errorf("%w: %w", ErrExportWrite, err)
	}
	for _, b := range t.Projection.Blocks {
		if b.Kind == "reasoning" && !options.Reasoning {
			continue
		}
		if b.Kind == "tool" && !options.Tools {
			continue
		}
		heading := b.Kind
		if heading != "" {
			heading = strings.ToUpper(heading[:1]) + heading[1:]
		}
		if b.Kind == "event" && !options.RawEvents {
			continue
		}
		if _, err := fmt.Fprintf(w, "## %s\n\n", heading); err != nil {
			return fmt.Errorf("%w: %w", ErrExportWrite, err)
		}
		if options.Metadata {
			if _, err := fmt.Fprintf(w, "- Status: %s\n- Time: %s\n\n", b.Status, b.At.Format(time.RFC3339)); err != nil {
				return fmt.Errorf("%w: %w", ErrExportWrite, err)
			}
		}
		text := b.Text
		if text == "" {
			text = b.Tool
			if text == "" {
				text = b.EventType
			}
		}
		if _, err := fmt.Fprintf(w, "%s\n\n", text); err != nil {
			return fmt.Errorf("%w: %w", ErrExportWrite, err)
		}
	}
	return nil
}

// FollowUp is explicitly local intent until dispatch begins.
type FollowUp struct {
	ID, Text     string
	State        string
	Err          string
	ArtifactIDs  []string
	Dispositions map[string]string
}

// Queue is a bounded ordered local follow-up queue.
type Queue struct {
	entries []FollowUp
	next    uint64
	paused  bool
}

// Enqueue adds local-only intent.
func (q Queue) Enqueue(text string) Queue {
	return q.EnqueueTurn(text, nil, nil)
}

// EnqueueTurn adds local-only intent with artifact references retained until dispatch.
func (q Queue) EnqueueTurn(text string, artifactIDs []string, dispositions map[string]string) Queue {
	q.next++
	q.entries = append(q.entries, FollowUp{ID: fmt.Sprintf("local-%d", q.next), Text: text, State: "local queue", ArtifactIDs: append([]string(nil), artifactIDs...), Dispositions: cloneStrings(dispositions)})
	if len(q.entries) > 50 {
		q.entries = append([]FollowUp(nil), q.entries[len(q.entries)-50:]...)
	}
	return q
}

// Entries returns immutable queue state.
func (q Queue) Entries() []FollowUp {
	out := append([]FollowUp(nil), q.entries...)
	for i := range out {
		out[i].ArtifactIDs = append([]string(nil), out[i].ArtifactIDs...)
		out[i].Dispositions = cloneStrings(out[i].Dispositions)
	}
	return out
}

// Begin marks only the first pending entry as dispatching.
func (q Queue) Begin() (Queue, FollowUp, bool) {
	if q.paused {
		return q, FollowUp{}, false
	}
	for i := range q.entries {
		if q.entries[i].State == "local queue" {
			q.entries = append([]FollowUp(nil), q.entries...)
			q.entries[i].State = "dispatching"
			return q, q.entries[i], true
		}
	}
	return q, FollowUp{}, false
}

// Fail retains unrelated intent and pauses automatic dispatch.
func (q Queue) Fail(id string, err error) Queue {
	q.entries = append([]FollowUp(nil), q.entries...)
	for i := range q.entries {
		if q.entries[i].ID == id {
			q.entries[i].State = "failed"
			q.entries[i].Err = err.Error()
		}
	}
	q.paused = true
	return q
}

// Remove removes an entry only before dispatch.
func (q Queue) Remove(id string) Queue {
	q.entries = append([]FollowUp(nil), q.entries...)
	for i, e := range q.entries {
		if e.ID == id && e.State == "local queue" {
			q.entries = append(q.entries[:i], q.entries[i+1:]...)
			break
		}
	}
	return q
}

// Retry returns a failed entry to the head of ordered dispatch and resumes the queue.
func (q Queue) Retry(id string) Queue {
	q.entries = append([]FollowUp(nil), q.entries...)
	for i := range q.entries {
		if q.entries[i].ID == id && q.entries[i].State == "failed" {
			q.entries[i].State = "local queue"
			q.entries[i].Err = ""
			q.paused = false
			break
		}
	}
	return q
}

// Discard removes failed or still-local intent and resumes ordered dispatch.
func (q Queue) Discard(id string) Queue {
	q.entries = append([]FollowUp(nil), q.entries...)
	for i, entry := range q.entries {
		if entry.ID == id && (entry.State == "failed" || entry.State == "local queue") {
			q.entries = append(q.entries[:i], q.entries[i+1:]...)
			q.paused = false
			break
		}
	}
	return q
}

// Complete removes a successfully dispatched local follow-up.
func (q Queue) Complete(id string) Queue {
	q.entries = append([]FollowUp(nil), q.entries...)
	for i, entry := range q.entries {
		if entry.ID == id && entry.State == "dispatching" {
			q.entries = append(q.entries[:i], q.entries[i+1:]...)
			break
		}
	}
	return q
}
