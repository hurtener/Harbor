// Package interventions implements the PauseToken-correlated TUI intervention inbox.
package interventions

import (
	"sort"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tui/renderers"
	"github.com/hurtener/Harbor/internal/tui/surface"
)

// Kind is an operator-facing pause cause.
type Kind string

const (
	KindApproval Kind = "approval required"
	KindOAuth    Kind = "OAuth required"
	KindInput    Kind = "input required"
	KindPause    Kind = "paused"
)

// Item is one immutable PauseToken-correlated queue entry.
type Item struct {
	Token, RunID, Reason, Status, Resolution string
	Kind                                     Kind
	PausedAt                                 time.Time
	ResolvedAt, ExpiresAt                    time.Time
	Payload                                  any
	PayloadRef                               *types.PauseArtifactRef
}

// Inbox is a value-updated queue with resolved-token anti-resurrection.
type Inbox struct {
	Surface  surface.State
	items    map[string]Item
	resolved map[string]uint64
	gen      uint64
	selected string
}

// New constructs an empty inbox.
func New() Inbox {
	return Inbox{Surface: surface.State{Status: surface.Loading}, items: map[string]Item{}, resolved: map[string]uint64{}}
}

// Reconcile applies an authoritative pause snapshot without resurrecting a
// token resolved by a newer generation.
func (i Inbox) Reconcile(snapshots []types.PauseSnapshot, generation uint64) Inbox {
	return i.reconcile(snapshots, generation, time.Now(), true, false)
}

// ReconcilePage applies a typed canonical page. Missing entries are only
// resolved-elsewhere when the response is a complete first page.
func (i Inbox) ReconcilePage(response types.PauseListResponse, generation uint64, now time.Time) Inbox {
	complete := response.Page <= 1 && response.PageCount <= 1 && !response.Truncated
	return i.reconcile(response.Snapshots, generation, now, complete, response.Truncated)
}

func (i Inbox) reconcile(snapshots []types.PauseSnapshot, generation uint64, now time.Time, complete, truncated bool) Inbox {
	next := i.clone()
	next.gen = max(i.gen, generation)
	next.Surface = surface.State{Status: surface.Loaded}
	if truncated {
		next.Surface = surface.State{Status: surface.Partial, Reason: "resumed PauseToken retention is truncated"}
	}
	seen := make(map[string]bool, len(snapshots))
	for _, snapshot := range snapshots {
		if snapshot.Token == "" || next.resolved[snapshot.Token] >= generation {
			continue
		}
		seen[snapshot.Token] = true
		if snapshot.State == types.PauseStateResumed {
			item := next.items[snapshot.Token]
			item.Token, item.RunID, item.Reason, item.Kind = snapshot.Token, snapshot.Identity.Run, snapshot.Reason, classify(snapshot.Reason)
			item.Status, item.Resolution, item.ResolvedAt = "resolved", "resolved elsewhere", snapshot.ResumedAt
			if item.ResolvedAt.IsZero() {
				item.ResolvedAt = now
			}
			next.items[snapshot.Token] = item
			next.resolved[snapshot.Token] = generation
			continue
		}
		item := Item{Token: renderers.SafeText(snapshot.Token), RunID: renderers.SafeText(snapshot.Identity.Run), Reason: renderers.SafeText(snapshot.Reason), Status: string(snapshot.State), Kind: classify(snapshot.Reason), PausedAt: snapshot.PausedAt, Payload: renderers.Normalize(snapshot.Payload), PayloadRef: cloneRef(snapshot.PayloadRef)}
		if item.RunID == "" {
			if object, ok := item.Payload.(map[string]any); ok {
				if runID, ok := object["run_id"].(string); ok {
					item.RunID = renderers.SafeText(runID)
				}
			}
		}
		item.ExpiresAt = payloadExpiry(item.Payload)
		if !item.ExpiresAt.IsZero() && !now.Before(item.ExpiresAt) {
			item.Status, item.Resolution, item.ResolvedAt = "expired", "expired", now
			next.resolved[snapshot.Token] = generation
		}
		next.items[snapshot.Token] = item
	}
	if complete {
		for token, item := range next.items {
			if !seen[token] && item.Status == string(types.PauseStatePaused) {
				item.Status, item.Resolution, item.ResolvedAt = "resolved", "resolved elsewhere", now
				next.items[token] = item
				next.resolved[token] = generation
			}
		}
	}
	selected, selectedOK := next.items[next.selected]
	if !selectedOK || selected.Status != string(types.PauseStatePaused) {
		next.selected = ""
		for _, item := range next.Items() {
			if item.Status == string(types.PauseStatePaused) {
				next.selected = item.Token
				break
			}
		}
	}
	return next
}

// Resolve marks a token resolved, including resolved-elsewhere outcomes.
func (i Inbox) Resolve(token, outcome string, generation uint64) Inbox {
	next := i.clone()
	item := next.items[token]
	item.Status, item.Resolution, item.ResolvedAt = "resolved", renderers.SafeText(outcome), time.Now()
	next.items[token] = item
	next.resolved[token] = max(generation, next.gen)
	return next
}

// Items returns unresolved newest-first isolated entries.
func (i Inbox) Items() []Item {
	all := i.History()
	out := make([]Item, 0, len(all))
	for _, item := range all {
		if item.Status == string(types.PauseStatePaused) {
			out = append(out, item)
		}
	}
	return out
}

// History returns active entries and retained expiry/resolution tombstones.
func (i Inbox) History() []Item {
	out := make([]Item, 0, len(i.items))
	for _, item := range i.items {
		out = append(out, item)
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].PausedAt.After(out[b].PausedAt) })
	return out
}

// Active returns unresolved entries only.
func (i Inbox) Active() []Item {
	return i.Items()
}

// Select chooses an exact PauseToken and retains it across updates.
func (i Inbox) Select(token string) Inbox {
	next := i.clone()
	if _, ok := next.items[token]; ok {
		next.selected = token
	}
	return next
}

// Selected returns the retained exact token selection.
func (i Inbox) Selected() (Item, bool) {
	item, ok := i.items[i.selected]
	return item, ok
}

func (i Inbox) clone() Inbox {
	next := New()
	next.gen = i.gen
	next.Surface = i.Surface
	next.selected = i.selected
	for key, value := range i.items {
		next.items[key] = value
	}
	for key, value := range i.resolved {
		next.resolved[key] = value
	}
	return next
}

func cloneRef(ref *types.PauseArtifactRef) *types.PauseArtifactRef {
	if ref == nil {
		return nil
	}
	copy := *ref
	return &copy
}

func payloadExpiry(payload any) time.Time {
	object, ok := payload.(map[string]any)
	if !ok {
		return time.Time{}
	}
	for _, key := range []string{"expires_at", "expiry", "expires"} {
		text, ok := object[key].(string)
		if !ok {
			continue
		}
		if parsed, err := time.Parse(time.RFC3339, text); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func classify(reason string) Kind {
	lower := strings.ToLower(reason)
	switch {
	case strings.Contains(lower, "approval"):
		return KindApproval
	case strings.Contains(lower, "oauth"), strings.Contains(lower, "external"):
		return KindOAuth
	case strings.Contains(lower, "input"):
		return KindInput
	default:
		return KindPause
	}
}
