package annotate

import (
	"context"
	"sort"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tools"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// windowDurations maps each wire metrics window onto its span.
var windowDurations = map[prototypes.ToolMetricsWindow]time.Duration{
	prototypes.ToolWindow1h:  time.Hour,
	prototypes.ToolWindow24h: 24 * time.Hour,
	prototypes.ToolWindow7d:  7 * 24 * time.Hour,
}

// degradedErrorRate / offlineErrorRate bound the health pill: below
// degraded is Healthy, [degraded, offline) is Degraded, at/above offline
// is Offline. Zero observed invocations is Healthy (an honest "no
// failures observed," never a degraded value).
const (
	degradedErrorRate = 0.05
	offlineErrorRate  = 0.50
)

// toolHistory reads the session-scoped tool lifecycle events for a
// window from the bus's HistoryReplayer, returning them oldest-first.
// A bus that implements no windowed history read returns (nil, false) —
// the annotator then reports honest zero-observation metrics rather than
// a fabricated value or a silent error.
func (a *Annotator) toolHistory(ctx context.Context, id identity.Identity, types []events.EventType) ([]events.Event, bool) {
	if a.events == nil {
		return nil, false
	}
	hr, ok := a.events.(events.HistoryReplayer)
	if !ok {
		return nil, false
	}
	evs, err := hr.Window(ctx, 0, a.scanLimit, events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
		Types:   types,
	})
	if err != nil {
		return nil, false
	}
	return evs, true
}

// LastUsedAt implements protocol.Annotator. It returns the most recent
// invocation instant of toolID in the caller's scope, or the zero value
// when the tool has no recorded invocation (the honest "never").
func (a *Annotator) LastUsedAt(ctx context.Context, id identity.Identity, toolID string) time.Time {
	evs, ok := a.toolHistory(ctx, id, []events.EventType{
		tools.EventTypeToolInvoked,
		tools.EventTypeToolCompleted,
		tools.EventTypeToolFailed,
	})
	if !ok {
		return time.Time{}
	}
	var last time.Time
	for _, ev := range evs {
		if name, ok := toolNameOf(ev); ok && name == toolID {
			if ev.OccurredAt.After(last) {
				last = ev.OccurredAt
			}
		}
	}
	return last
}

// Metrics implements protocol.Annotator. It folds the session-scoped
// terminal tool events (tool.completed / tool.failed) into per-window
// error rates + the selected window's invocation / failure counts + the
// health pill. A tool with no recorded terminal events reads a
// zero-observation Healthy pill.
func (a *Annotator) Metrics(ctx context.Context, id identity.Identity, toolID string, window prototypes.ToolMetricsWindow) prototypes.ToolMetrics {
	out := prototypes.ToolMetrics{
		ID:     toolID,
		Window: window,
		Status: prototypes.ToolStatusHealthy,
	}
	evs, ok := a.toolHistory(ctx, id, []events.EventType{
		tools.EventTypeToolCompleted,
		tools.EventTypeToolFailed,
	})
	if !ok {
		return out
	}
	now := a.clock()

	// Per-window (invocations, failures) accumulators.
	agg := map[prototypes.ToolMetricsWindow]*windowCounts{
		prototypes.ToolWindow1h:  {},
		prototypes.ToolWindow24h: {},
		prototypes.ToolWindow7d:  {},
	}
	for _, ev := range evs {
		name, ok := toolNameOf(ev)
		if !ok || name != toolID {
			continue
		}
		failed := ev.Type == tools.EventTypeToolFailed
		age := now.Sub(ev.OccurredAt)
		for w, span := range windowDurations {
			if age <= span {
				agg[w].inv++
				if failed {
					agg[w].fail++
				}
			}
		}
	}

	out.ErrorRate1h = rate(agg[prototypes.ToolWindow1h])
	out.ErrorRate24h = rate(agg[prototypes.ToolWindow24h])
	out.ErrorRate7d = rate(agg[prototypes.ToolWindow7d])

	sel := agg[window]
	if sel == nil {
		sel = agg[prototypes.ToolWindow1h]
	}
	out.Invocations = sel.inv
	out.Failures = sel.fail
	out.Status = statusFor(rate(sel), sel.inv)
	return out
}

// ContentStats implements protocol.Annotator. It builds a per-tool
// result-size histogram from the MCP offload records in the session's
// event stream (the ONLY per-result byte-size signal the runtime emits —
// tool lifecycle events are content-free by construction), reports the
// configured heavy-content threshold + the count at/above it, and the
// negotiated DisplayMode map. A tool with no offloaded results reads an
// empty histogram (an honest "no heavy results recorded"), never a
// fabricated distribution.
func (a *Annotator) ContentStats(ctx context.Context, id identity.Identity, toolID string) prototypes.ToolContentStats {
	out := prototypes.ToolContentStats{
		ID:                  toolID,
		Histogram:           []prototypes.ToolContentBucket{},
		HeavyThresholdBytes: a.heavy,
		NegotiatedDisplay:   a.DisplayModes(ctx, id, toolID),
	}
	evs, ok := a.toolHistory(ctx, id, []events.EventType{mcpdrv.EventTypeMCPResourceOffloaded})
	if !ok {
		return out
	}
	// Power-of-two byte buckets keyed by inclusive upper bound.
	byBucket := map[int64]int64{}
	var heavyCount int64
	for _, ev := range evs {
		src, size, ok := offloadOf(ev)
		if !ok || src != toolID {
			continue
		}
		byBucket[bucketMax(size)]++
		if a.heavy > 0 && size >= a.heavy {
			heavyCount++
		}
	}
	out.HeavyCount = heavyCount
	if len(byBucket) > 0 {
		bounds := make([]int64, 0, len(byBucket))
		for b := range byBucket {
			bounds = append(bounds, b)
		}
		sort.Slice(bounds, func(i, j int) bool { return bounds[i] < bounds[j] })
		hist := make([]prototypes.ToolContentBucket, 0, len(bounds))
		for _, b := range bounds {
			hist = append(hist, prototypes.ToolContentBucket{MaxBytes: b, Count: byBucket[b]})
		}
		out.Histogram = hist
	}
	return out
}

// windowCounts is a per-window (invocations, failures) accumulator.
type windowCounts struct{ inv, fail int64 }

// rate returns the failure fraction for a window's counts, or 0 when no
// invocations were observed.
func rate(c *windowCounts) float64 {
	if c == nil || c.inv == 0 {
		return 0
	}
	return float64(c.fail) / float64(c.inv)
}

// statusFor maps an error rate + invocation count onto the health pill.
func statusFor(errorRate float64, invocations int64) prototypes.ToolStatus {
	if invocations == 0 || errorRate < degradedErrorRate {
		return prototypes.ToolStatusHealthy
	}
	if errorRate < offlineErrorRate {
		return prototypes.ToolStatusDegraded
	}
	return prototypes.ToolStatusOffline
}

// bucketMax returns the inclusive power-of-two upper bound of size's
// histogram bucket (1KiB, 2KiB, 4KiB, ...). A non-positive size buckets
// at the smallest bound.
func bucketMax(size int64) int64 {
	b := int64(1024)
	for b < size {
		b <<= 1
	}
	return b
}

// toolNameOf extracts the tool name from a tool lifecycle event, handling
// both the typed SafePayload (the in-memory bus) and the generic
// RedactedMap the durable bus rehydrates payloads as.
func toolNameOf(ev events.Event) (string, bool) {
	switch p := ev.Payload.(type) {
	case tools.ToolInvokedPayload:
		return p.ToolName, true
	case tools.ToolCompletedPayload:
		return p.ToolName, true
	case tools.ToolFailedPayload:
		return p.ToolName, true
	case events.RedactedMap:
		if v, ok := p.Data["ToolName"].(string); ok {
			return v, true
		}
	}
	return "", false
}

// offloadOf extracts the (source, size) of an MCP offload event, handling
// both the typed payload and the durable RedactedMap.
func offloadOf(ev events.Event) (string, int64, bool) {
	switch p := ev.Payload.(type) {
	case mcpdrv.ResourceOffloadedPayload:
		return p.Source, p.SizeBytes, p.Source != ""
	case events.RedactedMap:
		src, ok := p.Data["Source"].(string)
		if !ok || src == "" {
			return "", 0, false
		}
		// JSON numbers rehydrate as float64.
		var size int64
		switch n := p.Data["SizeBytes"].(type) {
		case float64:
			size = int64(n)
		case int64:
			size = n
		}
		return src, size, true
	}
	return "", 0, false
}
