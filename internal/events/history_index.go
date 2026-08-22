package events

// This file contains the typed, payload-free metadata projection used by
// windowed history reads.  Durable drivers persist the projection beside the
// payload entry and consumers can therefore satisfy identity/type/time/count
// queries without materialising every opaque event body.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// EventMetadata is the immutable routing projection of an Event. It contains
// no payload bytes. CostSummary is populated when the canonical payload has
// the common Cost/Usage shape, allowing session counters to remain exact
// without loading every event body.
type EventMetadata struct {
	Type       EventType
	Identity   identity.Quadruple
	OccurredAt time.Time
	Sequence   uint64
	Internal   bool

	// CostSummary is true when CostDollars and TotalTokens came from the
	// event payload. Zero is a valid value, so callers must check the flag.
	CostSummary bool
	CostDollars float64
	TotalTokens int64
}

// NewEventMetadata projects one event without retaining its payload. It
// fails loudly when a payload cannot be represented as JSON; a malformed
// projection must never look like an event with zero cost or zero type.
func NewEventMetadata(ev Event) (EventMetadata, error) {
	m := EventMetadata{
		Type:       ev.Type,
		Identity:   ev.Identity,
		OccurredAt: ev.OccurredAt,
		Sequence:   ev.Sequence,
		Internal:   IsBusInternalNotice(ev.Type),
	}
	if ev.Payload == nil {
		return EventMetadata{}, fmt.Errorf("events: metadata for seq=%d: nil payload", ev.Sequence)
	}
	var raw any
	if rm, ok := ev.Payload.(RedactedMap); ok {
		raw = rm.Data
	} else {
		b, err := json.Marshal(ev.Payload)
		if err != nil {
			return EventMetadata{}, fmt.Errorf("events: metadata for seq=%d: marshal payload: %w", ev.Sequence, err)
		}
		if err := json.Unmarshal(b, &raw); err != nil {
			return EventMetadata{}, fmt.Errorf("events: metadata for seq=%d: decode payload: %w", ev.Sequence, err)
		}
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return m, nil
	}
	// Only the canonical LLM cost event owns the Cost/Usage summary. Other
	// payloads may happen to contain similarly named fields, but counting
	// those would change the session-counter contract.
	if ev.Type != EventType("llm.cost.recorded") {
		return m, nil
	}
	cost, costOK := obj["Cost"].(map[string]any)
	usage, usageOK := obj["Usage"].(map[string]any)
	if !costOK && !usageOK {
		return m, nil
	}
	if v, ok := cost["TotalCost"].(float64); ok && !math.IsNaN(v) && !math.IsInf(v, 0) {
		m.CostDollars = v
	}
	if v, ok := usage["TotalTokens"].(float64); ok && v >= math.MinInt64 && v <= math.MaxInt64 {
		m.TotalTokens = int64(v)
	}
	m.CostSummary = true
	return m, nil
}

// MatchMetadataWire applies the same identity/type/time predicate as
// MatchWire, without inspecting a payload.
func MatchMetadataWire(m EventMetadata, wire prototypes.EventFilter) bool {
	if len(wire.EventTypes) > 0 {
		matched := false
		for _, typ := range wire.EventTypes {
			if string(m.Type) == typ {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if !containsOrEmpty(wire.TenantIDs, m.Identity.TenantID) ||
		!containsOrEmpty(wire.UserIDs, m.Identity.UserID) ||
		!containsOrEmpty(wire.SessionIDs, m.Identity.SessionID) ||
		!containsOrEmpty(wire.RunIDs, m.Identity.RunID) {
		return false
	}
	if !wire.Since.IsZero() && m.OccurredAt.Before(wire.Since) {
		return false
	}
	if !wire.Until.IsZero() && !m.OccurredAt.Before(wire.Until) {
		return false
	}
	return true
}

// MatchMetadataScoped applies the by-id Filter predicate to metadata.
func MatchMetadataScoped(m EventMetadata, f Filter) bool {
	if f.Tenant != "" && m.Identity.TenantID != f.Tenant {
		return false
	}
	if f.User != "" && m.Identity.UserID != f.User {
		return false
	}
	if f.Session != "" && m.Identity.SessionID != f.Session {
		return false
	}
	if f.Run != "" && m.Identity.RunID != f.Run {
		return false
	}
	if len(f.Types) == 0 {
		return true
	}
	for _, typ := range f.Types {
		if m.Type == typ {
			return true
		}
	}
	return false
}

// MetadataListPage is the payload-free equivalent of EventListPage.
type MetadataListPage struct {
	Events     []EventMetadata
	NextCursor uint64
	HasMore    bool
	Truncated  bool
}

// EventMetadataReplayer is an optional HistoryReplayer capability. Drivers
// that can maintain the projection implement it so aggregates and counter
// rollups do not scan opaque event bodies.
type EventMetadataReplayer interface {
	ListWindowMetadata(ctx context.Context, q EventListQuery) (MetadataListPage, error)
}

// MetadataListWindowFromSnapshot applies the events.list paging grammar to a
// sequence-ordered snapshot while projecting only metadata.
func MetadataListWindowFromSnapshot(snapshot []Event, before uint64, limit int, wire prototypes.EventFilter) (MetadataListPage, error) {
	if limit <= 0 || len(snapshot) == 0 {
		return MetadataListPage{}, nil
	}
	matches := make([]EventMetadata, 0, limit+1)
	for i := len(snapshot) - 1; i >= 0; i-- {
		ev := snapshot[i]
		if before != 0 && ev.Sequence >= before {
			continue
		}
		m, err := NewEventMetadata(ev)
		if err != nil {
			return MetadataListPage{}, err
		}
		if m.Internal || !MatchMetadataWire(m, wire) {
			continue
		}
		matches = append(matches, m)
		if len(matches) > limit {
			break
		}
	}
	var page MetadataListPage
	if len(matches) > limit {
		page.HasMore = true
		matches = matches[:limit]
		page.NextCursor = matches[len(matches)-1].Sequence
	}
	for i, j := 0, len(matches)-1; i < j; i, j = i+1, j-1 {
		matches[i], matches[j] = matches[j], matches[i]
	}
	page.Events = matches
	return page, nil
}
