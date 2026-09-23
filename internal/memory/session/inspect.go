package session

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/state"
)

// ErrItemNotFound does not distinguish an absent key from a foreign one.
var ErrItemNotFound = errors.New("session memory: item not found")

// Item is a read projection of committed memory, never an admission or journal.
// Value is already redacted at the execution boundary. It must still use the
// consumer's ordinary heavy-value and identity protections.
type Item struct {
	Key       string
	CreatedAt time.Time
	ExpiresAt time.Time
	Value     json.RawMessage
}

// Inspection is one bounded read of the same state used for execution. It is
// not another persisted transcript. Active runs expose no uncommitted content.
type Inspection struct {
	Items           []Item
	Summary         string
	RecentTurns     int
	EstimatedTokens int
}

func inspectionHandle(store state.StateStore, id identity.Quadruple, now func() time.Time) (*RetainedRun, error) {
	if store == nil || identity.Validate(id.Identity) != nil {
		return nil, ErrRetainedContextUnavailable
	}
	if now == nil {
		now = time.Now
	}
	return &RetainedRun{store: store, q: identity.Quadruple{Identity: id.Identity}, now: now}, nil
}

func itemKey(id identity.Identity, kind string, values ...string) string {
	// Length framing prevents ambiguous delimiter joins. Only immutable host
	// metadata participates; the viewing run and source contents do not.
	var b []byte
	for _, value := range append([]string{id.TenantID, id.UserID, id.SessionID, kind}, values...) {
		b = binary.AppendUvarint(b, uint64(len(value)))
		b = append(b, value...)
	}
	digest := sha256.Sum256(b)
	return "mem_" + hex.EncodeToString(digest[:])
}

func sourceKey(id identity.Identity, admission retainedAdmission) string {
	return itemKey(id, "source", string(admission.ID))
}

func checkpointKey(id identity.Identity, checkpoint *retainedCheckpoint) string {
	return itemKey(id, "checkpoint", strconv.FormatUint(checkpoint.Generation, 10), string(checkpoint.SourceThrough.ID), checkpoint.SourceDigest)
}

func sourceTime(id state.EventID) time.Time {
	parsed, err := ulid.ParseStrict(string(id))
	if err != nil {
		// EventID permits external sources. An unparseable timestamp is
		// unknown, never the time of inspection (which would renew its age).
		return time.Time{}
	}
	return ulid.Time(parsed.Time()).UTC()
}

// Inspect projects only currently authorized, unexpired committed state. It
// neither persists trimming nor renews a retention deadline.
func Inspect(ctx context.Context, store state.StateStore, id identity.Quadruple, now func() time.Time) (Inspection, error) {
	if err := ctx.Err(); err != nil {
		return Inspection{}, err
	}
	r, err := inspectionHandle(store, id, now)
	if err != nil {
		return Inspection{}, err
	}
	window, _, err := r.load(ctx)
	if err != nil {
		return Inspection{}, err
	}
	r.trim(&window)
	view := Inspection{Items: make([]Item, 0, len(window.Turns)+len(window.Evidence)+1), RecentTurns: len(window.Turns)}
	appendItem := func(key string, admission retainedAdmission, expires time.Time, value any) error {
		encoded, encodeErr := json.Marshal(value)
		if encodeErr != nil {
			return encodeErr
		}
		view.Items = append(view.Items, Item{Key: key, CreatedAt: sourceTime(admission.ID), ExpiresAt: expires, Value: encoded})
		return nil
	}
	if c := window.Checkpoint; c != nil {
		if err := appendItem(checkpointKey(id.Identity, c), c.SourceThrough, c.ExpiresAt, c); err != nil {
			return Inspection{}, err
		}
		b, err := json.Marshal(c.Narrative)
		if err != nil {
			return Inspection{}, err
		}
		view.Summary = string(b)
	}
	for _, source := range window.Evidence {
		if err := appendItem(sourceKey(id.Identity, source.Admission), source.Admission, source.ExpiresAt, source); err != nil {
			return Inspection{}, err
		}
	}
	for _, source := range window.Turns {
		if err := appendItem(sourceKey(id.Identity, source.Admission), source.Admission, source.ExpiresAt, source); err != nil {
			return Inspection{}, err
		}
	}
	steps, err := projectRetainedWindow(window)
	if err != nil {
		return Inspection{}, err
	}
	tr := &planner.Trajectory{Steps: steps}
	r.checkpoint = window.Checkpoint
	if err := r.applyCheckpoint(tr); err != nil {
		return Inspection{}, err
	}
	start, err := tr.ReplayStart()
	if err != nil {
		return Inspection{}, err
	}
	// This is an estimate of memory input, not a provider token count. Covered
	// raw steps are storage evidence, not additional model input.
	encoded, err := json.Marshal(struct {
		Summary *planner.Summary `json:"summary,omitempty"`
		Steps   []planner.Step   `json:"steps,omitempty"`
	}{Summary: tr.ActiveSummary(), Steps: tr.Steps[start:]})
	if err != nil {
		return Inspection{}, err
	}
	view.EstimatedTokens = (len(encoded) + 3) / 4
	if len(view.Items) == 0 && len(window.Active) == 0 && !window.Partial {
		view.EstimatedTokens = 0
	}
	return view, nil
}

// Delete removes one currently visible source/checkpoint with a conditional
// write. A derived checkpoint is invalidated wholesale when affected: an opaque
// summary cannot prove selective forgetting. All frozen admissions are fenced,
// so inference/dispatch/late settlement cannot reintroduce erased material.
// Other sessions and their admissions are untouched. No external action runs.
func Delete(ctx context.Context, store state.StateStore, id identity.Quadruple, key string, now func() time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	r, err := inspectionHandle(store, id, now)
	if err != nil {
		return 0, err
	}
	for range retainedContextAttempts {
		window, recordID, err := r.load(ctx)
		if err != nil {
			return 0, err
		}
		r.trim(&window)
		found, invalidate := false, false
		if c := window.Checkpoint; c != nil && checkpointKey(id.Identity, c) == key {
			found, invalidate = true, true
		}
		for i, source := range window.Turns {
			if sourceKey(id.Identity, source.Admission) == key {
				found = true
				invalidate = window.Checkpoint != nil && source.Admission.Sequence <= window.Checkpoint.SourceThrough.Sequence
				window.Turns = append(window.Turns[:i:i], window.Turns[i+1:]...)
				break
			}
		}
		for i, source := range window.Evidence {
			if sourceKey(id.Identity, source.Admission) == key {
				found, invalidate = true, true
				window.Evidence = append(window.Evidence[:i:i], window.Evidence[i+1:]...)
				break
			}
		}
		if !found {
			return 0, ErrItemNotFound
		}
		if invalidate {
			window.Checkpoint = nil
		}
		window.Active = nil
		window.Partial = true
		if err := r.save(ctx, recordID, window); errors.Is(err, state.ErrConditionFailed) {
			continue
		} else if err != nil {
			return 0, err
		}
		return len(window.Turns), nil
	}
	return 0, ErrRetainedContextUnavailable
}

// Put records an administrator-supplied conversation note, not a tool result or
// execution authority. It has no inference or side effects. Capacity is checked
// before publication; it never drops unsummarized turns to make a note fit.
func Put(ctx context.Context, store state.StateStore, redactor audit.Redactor, id identity.Quadruple, query, answer string, turns int, ttl time.Duration, now func() time.Time) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	r, err := inspectionHandle(store, id, now)
	if err != nil || redactor == nil || turns < 1 || turns > maxRetainedContextTurns || ttl <= 0 {
		return "", ErrRetainedContextUnavailable
	}
	if !utf8.ValidString(query) || !utf8.ValidString(answer) {
		return "", ErrRetainedContextUnavailable
	}
	// Redact content before attaching immutable host metadata. A custom redactor
	// cannot change admissions, retention or provenance through this input.
	redacted, err := redactor.Redact(ctx, map[string]any{"query": query, "answer": answer})
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrRetainedContextUnavailable, err)
	}
	encoded, err := json.Marshal(redacted)
	if err != nil {
		return "", ErrRetainedContextUnavailable
	}
	var safe struct {
		Query  *string `json:"query"`
		Answer *string `json:"answer"`
	}
	if err := decodeRetained(encoded, &safe); err != nil {
		return "", err
	}
	if safe.Query == nil || safe.Answer == nil {
		return "", ErrRetainedContextUnavailable
	}
	admission := retainedAdmission{ID: state.NewEventID(), RunID: "memory-note:" + string(state.NewEventID())}
	expires := r.now().Add(ttl)
	for range retainedContextAttempts {
		window, previous, err := r.load(ctx)
		if err != nil {
			return "", err
		}
		r.trim(&window)
		if !expires.After(r.now()) {
			return "", ErrRetainedContextUnavailable
		}
		if len(window.Turns) >= turns || window.LastAdmission == ^uint64(0) {
			return "", ErrRetainedContextCapacity
		}
		window.LastAdmission++
		admission.Sequence = window.LastAdmission
		window.Turns = append(window.Turns, retainedTurn{Admission: admission, ExpiresAt: expires, Status: "complete", Query: *safe.Query, Answer: *safe.Answer})
		if err := r.save(ctx, previous, window); errors.Is(err, state.ErrConditionFailed) {
			continue
		} else if err != nil {
			return "", err
		}
		return sourceKey(id.Identity, admission), nil
	}
	return "", ErrRetainedContextUnavailable
}
