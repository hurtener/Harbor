package protocol

import "testing"

// TestWriteLockShard_GoldenAndBounded pins the exact shard for a set of golden
// owner keys. Because the same key must ALWAYS map to the same shard for
// per-owner serialisation to hold, freezing the values catches any accidental
// change to the FNV constants, the byte order, or the modulus — a regression a
// pure same-key self-equality check could not see. Every shard is necessarily
// in range [0, writeLockShards). Recompute the values if writeLockShards or the
// hash deliberately change.
func TestWriteLockShard_GoldenAndBounded(t *testing.T) {
	golden := map[string]uint32{
		"a\x00t\x00agent":          241,
		"u\x00t\x00alice\x00agent": 149,
		"u\x00t\x00bob\x00agent":   46,
		"":                         197,
	}
	for key, want := range golden {
		got := writeLockShard(key)
		if got != want {
			t.Fatalf("writeLockShard(%q) = %d, want golden %d (FNV constants / order / modulus changed?)", key, got, want)
		}
		if got >= writeLockShards {
			t.Fatalf("writeLockShard(%q) = %d, out of range [0,%d)", key, got, writeLockShards)
		}
	}
}

// TestWriteLockShard_Distributes is a sanity check that the hash spreads owners
// across many shards rather than collapsing them onto a few — a degenerate
// mapping would re-introduce the serialisation bottleneck the per-owner lock
// exists to avoid. 2000 distinct user owners must touch a large fraction of the
// 256 shards.
func TestWriteLockShard_Distributes(t *testing.T) {
	seen := make(map[uint32]struct{})
	for i := range 2000 {
		key := "u\x00t\x00user-" + itoa(i) + "\x00agent"
		seen[writeLockShard(key)] = struct{}{}
	}
	// With 2000 keys over 256 shards a healthy hash fills nearly all shards;
	// require a conservative floor so the test is robust but still catches a
	// collapsed distribution.
	if len(seen) < writeLockShards/2 {
		t.Fatalf("write-lock shards poorly distributed: only %d/%d shards used for 2000 owners",
			len(seen), writeLockShards)
	}
}

// itoa is a tiny allocation-light int->string for the test keys (avoids pulling
// strconv into a white-box test that needs nothing else from it).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
