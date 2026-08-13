package rollups

import (
	"testing"
	"time"
)

func TestBucketStart_FixedUTCGrid(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
		size BucketSize
		want time.Time
	}{
		{"minute-mid", time.Date(2026, 8, 13, 12, 34, 56, 789, time.UTC), BucketMinute, time.Date(2026, 8, 13, 12, 34, 0, 0, time.UTC)},
		{"minute-boundary", time.Date(2026, 8, 13, 12, 34, 0, 0, time.UTC), BucketMinute, time.Date(2026, 8, 13, 12, 34, 0, 0, time.UTC)},
		{"minute-last-ns", time.Date(2026, 8, 13, 12, 34, 59, 999_999_999, time.UTC), BucketMinute, time.Date(2026, 8, 13, 12, 34, 0, 0, time.UTC)},
		{"minute-epoch", time.Unix(0, 0).UTC(), BucketMinute, time.Unix(0, 0).UTC()},
		{"hour-mid", time.Date(2026, 8, 13, 12, 34, 56, 789, time.UTC), BucketHour, time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)},
		{"hour-boundary", time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), BucketHour, time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)},
		{"hour-last-ns", time.Date(2026, 8, 13, 12, 59, 59, 999_999_999, time.UTC), BucketHour, time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)},
		{"hour-epoch", time.Unix(0, 0).UTC(), BucketHour, time.Unix(0, 0).UTC()},
		{"day-mid", time.Date(2026, 8, 13, 23, 59, 59, 999_999_999, time.UTC), BucketDay, time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)},
		{"day-boundary", time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC), BucketDay, time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)},
		{"day-leap", time.Date(2024, 2, 29, 15, 30, 0, 0, time.UTC), BucketDay, time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BucketStart(tc.in, tc.size); !got.Equal(tc.want) {
				t.Fatalf("BucketStart(%v, %s) = %v; want %v", tc.in, tc.size, got, tc.want)
			}
		})
	}
}

// TestBucketStart_PureFunction pins the addressability contract: the same
// (instant, size) input always yields the same boundary — the basis for
// deterministic rows across restarts and across concurrent consumers.
func TestBucketStart_PureFunction(t *testing.T) {
	instants := []time.Time{
		time.Date(2026, 8, 13, 12, 34, 56, 789, time.UTC),
		time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2033, 5, 18, 3, 3, 3, 3, time.UTC),
	}
	for _, size := range AllBucketSizes {
		for _, in := range instants {
			want := BucketStart(in, size)
			for i := 0; i < 100; i++ {
				if got := BucketStart(in, size); !got.Equal(want) {
					t.Fatalf("BucketStart(%v, %s) not pure: %v vs %v", in, size, got, want)
				}
			}
		}
	}
}

// TestBucketStart_NonUTCInputs: a local-time instant still floors onto the
// UTC grid (the grid is fixed, never local-adjusted).
func TestBucketStart_NonUTCInputs(t *testing.T) {
	in := time.Date(2026, 8, 13, 12, 0, 0, 0, time.FixedZone("other", 2*60*60))
	want := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	if got := BucketStart(in, BucketHour); !got.Equal(want) {
		t.Fatalf("BucketStart(%v, hour) = %v; want %v (UTC-normalised)", in, got, want)
	}
}

func TestBucketSpan(t *testing.T) {
	from := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		from time.Time
		to   time.Time
		size BucketSize
		want int64
	}{
		// bucketSpan counts the buckets that INTERSECT the half-open
		// window [from, to) — the maximum number of distinct query
		// buckets a result can carry. A window that starts mid-bucket
		// spans parts of two buckets.
		{"one minute", from, from.Add(time.Minute), BucketMinute, 1},
		{"60 minutes", from, from.Add(time.Hour), BucketMinute, 60},
		{"exact boundary excludes next", from, from.Add(3 * time.Hour), BucketHour, 3},
		{"partial last hour", from, from.Add(2*time.Hour + 30*time.Minute), BucketHour, 3},
		{"mid-hour start", from.Add(15 * time.Minute), from.Add(time.Hour + 15*time.Minute), BucketHour, 2},
		{"24h from midnight", time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC), BucketDay, 1},
		{"24h from mid-day", from, from.Add(24 * time.Hour), BucketDay, 2},
		{"two days from midnight", time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC), BucketDay, 2},
		{"48h from mid-day", from, from.Add(48 * time.Hour), BucketDay, 3},
		{"partial day", from, from.Add(36 * time.Hour), BucketDay, 2},
		{"week at hour", from, from.Add(7 * 24 * time.Hour), BucketHour, 168},
		{"1440 minutes at day", from, from.Add(24 * time.Hour), BucketMinute, 1440},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bucketSpan(tc.from, tc.to, tc.size)
			if err != nil {
				t.Fatalf("bucketSpan: %v", err)
			}
			if got != tc.want {
				t.Fatalf("bucketSpan = %d; want %d", got, tc.want)
			}
		})
	}

	if _, err := bucketSpan(from, from, BucketHour); err == nil {
		t.Fatal("empty window must error")
	}
	if _, err := bucketSpan(from.Add(time.Hour), from, BucketHour); err == nil {
		t.Fatal("reversed window must error")
	}
}

func TestBucketSizeValidate(t *testing.T) {
	for _, size := range AllBucketSizes {
		if err := size.Validate(); err != nil {
			t.Fatalf("Validate(%q): %v", size, err)
		}
	}
	if err := BucketSize("week").Validate(); err == nil {
		t.Fatal("unknown bucket size must fail validation")
	}
}
