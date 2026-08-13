package rollups

import (
	"fmt"
	"time"
)

// BucketSize is the closed set of fixed UTC bucket sizes. Every bucket grid
// is anchored to UTC: hour buckets start at HH:00:00Z, day buckets at
// 00:00:00Z — never at a local-time or DST-adjusted boundary. A bucket
// boundary is a pure function of (instant, BucketSize), so two runs at two
// different instants that fall in the same bucket compute IDENTICAL
// boundaries, including across a runtime restart.
type BucketSize string

const (
	// BucketHour is the UTC hour grid: buckets [HH:00:00Z, HH+1:00:00Z).
	BucketHour BucketSize = "hour"
	// BucketDay is the UTC day grid: buckets [00:00:00Z, next 00:00:00Z).
	BucketDay BucketSize = "day"
)

// AllBucketSizes is the closed set in canonical (finest-first) order. The
// projector stores rows at BucketHour — the finest closed size — and queries
// coarsen (a day query groups hour rows by their day bucket), so every query
// size is available from one storage granularity.
var AllBucketSizes = [...]BucketSize{BucketHour, BucketDay}

// Duration returns the bucket's span. Unknown sizes panic; Validate is the
// fail-loud entry point for untrusted input.
func (b BucketSize) Duration() time.Duration {
	switch b {
	case BucketHour:
		return time.Hour
	case BucketDay:
		return 24 * time.Hour
	default:
		panic(fmt.Sprintf("rollups: unknown BucketSize %q", b))
	}
}

// Validate reports whether b is a closed bucket size.
func (b BucketSize) Validate() error {
	switch b {
	case BucketHour, BucketDay:
		return nil
	default:
		return fmt.Errorf("%w: Bucket=%q (allowed: %v)", ErrQueryInvalid, b, allBucketSizes())
	}
}

// BucketStart returns the start instant of the fixed UTC bucket containing t.
// The computation is a pure function of (t, size): t is normalised to UTC and
// the boundary is derived by calendar truncation, never by arithmetic on an
// arbitrary anchor. Hour buckets start at HH:00:00Z; day buckets at
// 00:00:00Z.
func BucketStart(t time.Time, size BucketSize) time.Time {
	u := t.UTC()
	switch size {
	case BucketHour:
		return time.Date(u.Year(), u.Month(), u.Day(), u.Hour(), 0, 0, 0, time.UTC)
	case BucketDay:
		return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	default:
		panic(fmt.Sprintf("rollups: unknown BucketSize %q", size))
	}
}

// bucketSpan returns the number of buckets whose start instant falls in the
// half-open window [from, to): every bucket that intersects the window. from
// must be before to. The count is ceil((to - floor(from))/d), exact in
// integer nanoseconds, so a window ending exactly on a bucket boundary
// excludes the bucket that starts there.
func bucketSpan(from, to time.Time, size BucketSize) (int64, error) {
	if !to.After(from) {
		return 0, fmt.Errorf("%w: window [%s, %s) is empty or reversed", ErrQueryInvalid, from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano))
	}
	first := BucketStart(from, size)
	d := int64(size.Duration())
	if first.After(from) {
		return 0, fmt.Errorf("%w: internal bucket floor moved forward", ErrQueryInvalid)
	}
	span := to.Sub(first)
	n := span.Nanoseconds()
	// ceil(n / d) with exact integer arithmetic.
	count := (n + d - 1) / d
	return count, nil
}

// allBucketSizes renders the closed set for error messages.
func allBucketSizes() []string {
	out := make([]string, 0, len(AllBucketSizes))
	for _, s := range AllBucketSizes {
		out = append(out, string(s))
	}
	return out
}
