package protocol

import (
	"time"

	"github.com/hurtener/Harbor/internal/observability/rollups"
)

// buildQualityBlock stamps the mandatory freshness block from the
// projector's quality snapshot and the requested window. The watermark and
// retention horizon are relayed verbatim; the coverage quality is derived
// from the retained horizon relative to the window (see coverageForWindow).
func buildQualityBlock(q rollups.Quality, from, to time.Time) QualityBlock {
	return QualityBlock{
		State:          q.State,
		Watermark:      q.Watermark,
		WatermarkAt:    q.WatermarkAt,
		RetentionStart: q.RetentionStart,
		RetentionEnd:   q.RetentionEnd,
		Coverage:       coverageForWindow(from, to, q.RetentionStart, q.RetentionEnd),
		Err:            q.Err,
	}
}

// coverageForWindow reports how the half-open query window [from, to)
// relates to the store's retained horizon. Retention is reported at the
// fixed UTC MINUTE storage granularity: rows exist for the minute buckets
// [retStart, retEnd] inclusive, so the retained envelope is
// [retStart, retEnd + 1min). The window is bucket-aligned (a whole number
// of query buckets, each a whole number of minutes), so all comparisons
// are exact — a partial bucket is never silently counted.
//
//   - CoverageCovered: the whole window lies inside the retained envelope.
//   - CoverageGap: no retained row could possibly fall in the window (the
//     window is entirely outside the envelope, or nothing is retained).
//   - CoveragePartial: the window overlaps the envelope but extends
//     outside it — older or newer history has been retained away or never
//     arrived, so the totals for the window are incomplete by retention.
func coverageForWindow(from, to, retStart, retEnd time.Time) Coverage {
	if retStart.IsZero() || retEnd.IsZero() {
		return CoverageGap
	}
	envEnd := retEnd.Add(time.Minute)
	if !to.After(retStart) || !from.Before(envEnd) {
		return CoverageGap
	}
	if !from.Before(retStart) && !to.After(envEnd) {
		return CoverageCovered
	}
	return CoveragePartial
}
