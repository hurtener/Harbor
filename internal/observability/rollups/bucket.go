package rollups

import (
	"fmt"
	"time"
)

// BucketSize is the closed set of fixed UTC bucket sizes. Every bucket grid
// is anchored to UTC: minute buckets start at MM:00Z, hour buckets at
// HH:00:00Z, day buckets at 00:00:00Z — never at a local-time or DST-adjusted
// boundary. A bucket boundary is a pure function of (instant, BucketSize), so
// two runs at two different instants that fall in the same bucket compute
// IDENTICAL boundaries, including across a runtime restart.
type BucketSize string

const (
	// BucketMinute is the UTC minute grid: buckets [MM:00Z, MM+1:00Z).
	// This is the STORAGE granularity — every stored row is keyed on the
	// minute grid (see StoreGranularity in extract.go).
	BucketMinute BucketSize = "minute"
	// BucketHour is the UTC hour grid: buckets [HH:00:00Z, HH+1:00:00Z).
	BucketHour BucketSize = "hour"
	// BucketDay is the UTC day grid: buckets [00:00:00Z, next 00:00:00Z).
	BucketDay BucketSize = "day"
)

// AllBucketSizes is the closed set in canonical (finest-first) order. The
// projector stores rows at BucketMinute — the finest closed size — and
// queries coarsen (an hour query groups minute rows by their hour bucket, a
// day query by their day bucket), so every query size is available from one
// storage granularity. BucketHour is NOT the storage granularity; it is only
// one of the coarser query sizes.
var AllBucketSizes = [...]BucketSize{BucketMinute, BucketHour, BucketDay}

// Duration returns the bucket's span. Unknown sizes panic; Validate is the
// fail-loud entry point for untrusted input.
func (b BucketSize) Duration() time.Duration {
	switch b {
	case BucketMinute:
		return time.Minute
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
	case BucketMinute, BucketHour, BucketDay:
		return nil
	default:
		return fmt.Errorf("%w: Bucket=%q (allowed: %v)", ErrQueryInvalid, b, allBucketSizes())
	}
}

// BucketStart returns the start instant of the fixed UTC bucket containing t.
// The computation is a pure function of (t, size): t is normalised to UTC and
// the boundary is derived by calendar truncation, never by arithmetic on an
// arbitrary anchor. Minute buckets start at MM:00Z; hour buckets at
// HH:00:00Z; day buckets at 00:00:00Z.
func BucketStart(t time.Time, size BucketSize) time.Time {
	u := t.UTC()
	switch size {
	case BucketMinute:
		return time.Date(u.Year(), u.Month(), u.Day(), u.Hour(), u.Minute(), 0, 0, time.UTC)
	case BucketHour:
		return time.Date(u.Year(), u.Month(), u.Day(), u.Hour(), 0, 0, 0, time.UTC)
	case BucketDay:
		return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	default:
		panic(fmt.Sprintf("rollups: unknown BucketSize %q", size))
	}
}

// bucketSpan returns the number of fixed UTC buckets the half-open window
// [from, to) covers. from and to MUST both be aligned to the size grid —
// each must equal its own BucketStart (Query.Validate enforces this; the
// re-check here makes a misaligned window fail loudly with ErrQueryInvalid
// rather than silently counting a partial bucket). For aligned edges the
// count is exact integer division: (to-from)/duration, so a window ending
// exactly on a bucket boundary excludes the bucket that starts there, and
// no window ever spans a partial bucket.
func bucketSpan(from, to time.Time, size BucketSize) (int64, error) {
	if !to.After(from) {
		return 0, fmt.Errorf("%w: window [%s, %s) is empty or reversed", ErrQueryInvalid, from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano))
	}
	if err := size.Validate(); err != nil {
		return 0, err
	}
	if !from.Equal(BucketStart(from, size)) {
		return 0, fmt.Errorf("%w: window From %s is not aligned to the %s grid (it must fall exactly on a fixed-UTC bucket boundary)",
			ErrQueryInvalid, from.Format(time.RFC3339Nano), size)
	}
	if !to.Equal(BucketStart(to, size)) {
		return 0, fmt.Errorf("%w: window To %s is not aligned to the %s grid (it must fall exactly on a fixed-UTC bucket boundary)",
			ErrQueryInvalid, to.Format(time.RFC3339Nano), size)
	}
	n := to.Sub(from).Nanoseconds()
	d := int64(size.Duration())
	if n%d != 0 {
		return 0, fmt.Errorf("%w: internal: window [%s, %s) is not a whole number of %s buckets",
			ErrQueryInvalid, from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano), size)
	}
	return n / d, nil
}

// allBucketSizes renders the closed set for error messages.
func allBucketSizes() []string {
	out := make([]string, 0, len(AllBucketSizes))
	for _, s := range AllBucketSizes {
		out = append(out, string(s))
	}
	return out
}
