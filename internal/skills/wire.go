package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

// CanonicalContentHash returns the canonical sha256 hex of `s`'s
// content fields. The hash is the LWW gate and the idempotency key
// for `Upsert`:
//
//   - Origin / OriginRef / Scope / ScopeTenantID / ScopeProjectID
//     are EXCLUDED: the same skill imported via two paths
//     (PackImport from a re-published pack vs. Generated from a
//     planner re-derivation) hashes identically when the content is
//     the same, so the conflict-policy gate fires on actual content
//     drift rather than provenance noise.
//   - Lifecycle timestamps (`CreatedAt`, `UpdatedAt`, `LastUsed`)
//     and `UseCount` are EXCLUDED: they evolve over the row's life
//     without semantic content change.
//   - `Extra` IS INCLUDED via its canonical JSON-key-sorted text
//     representation (the generator may stamp model metadata there
//     that legitimately participates in identity).
//
// The hash is computed over a fixed-key envelope, NOT over a struct
// rendering, so future field additions don't silently break stored
// hashes. The envelope format and the included-fields list ARE
// load-bearing — changes need a new content-hash version (D-046).
//
// Slice-shaped fields (`Tags`, `RequiredTools`, `RequiredNS`,
// `RequiredTags`) are sorted before hashing so ordering noise from
// caller-side normalisation doesn't perturb the hash. Ordered slices
// (`Steps`, `Preconditions`, `FailureModes`) preserve their order
// because the planner's procedural rendering depends on it.
func CanonicalContentHash(s Skill) string {
	tags := sortedCopy(s.Tags)
	reqTools := sortedCopy(s.RequiredTools)
	reqNS := sortedCopy(s.RequiredNS)
	reqTags := sortedCopy(s.RequiredTags)

	// Field separator is `\x1f` (ASCII unit-separator) so caller-
	// supplied strings containing whitespace / newlines / pipes
	// can't collide with the framing.
	const sep = "\x1f"
	var b strings.Builder
	b.WriteString("name=")
	b.WriteString(s.Name)
	b.WriteString(sep)
	b.WriteString("title=")
	b.WriteString(s.Title)
	b.WriteString(sep)
	b.WriteString("description=")
	b.WriteString(s.Description)
	b.WriteString(sep)
	b.WriteString("trigger=")
	b.WriteString(s.Trigger)
	b.WriteString(sep)
	b.WriteString("task_type=")
	b.WriteString(s.TaskType)
	b.WriteString(sep)
	b.WriteString("tags=")
	b.WriteString(strings.Join(tags, ","))
	b.WriteString(sep)
	b.WriteString("steps=")
	b.WriteString(strings.Join(s.Steps, "\n"))
	b.WriteString(sep)
	b.WriteString("preconditions=")
	b.WriteString(strings.Join(s.Preconditions, "\n"))
	b.WriteString(sep)
	b.WriteString("failure_modes=")
	b.WriteString(strings.Join(s.FailureModes, "\n"))
	b.WriteString(sep)
	b.WriteString("required_tools=")
	b.WriteString(strings.Join(reqTools, ","))
	b.WriteString(sep)
	b.WriteString("required_ns=")
	b.WriteString(strings.Join(reqNS, ","))
	b.WriteString(sep)
	b.WriteString("required_tags=")
	b.WriteString(strings.Join(reqTags, ","))
	b.WriteString(sep)
	b.WriteString("extra=")
	b.WriteString(canonicalExtra(s.Extra))

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func sortedCopy(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	sort.Strings(out)
	return out
}

// canonicalExtra returns a key-sorted text rendering of `extra`.
// Accepts string / int / int64 / float64 / bool / nil values;
// anything else hashes as a stable `<unhashable>` sentinel so a
// caller-side bug produces a deterministic hash instead of a panic
// or non-deterministic ordering.
func canonicalExtra(extra map[string]any) string {
	if len(extra) == 0 {
		return ""
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(';')
		}
		b.WriteString(k)
		b.WriteByte('=')
		switch v := extra[k].(type) {
		case nil:
			b.WriteString("<nil>")
		case string:
			b.WriteString(v)
		case bool:
			b.WriteString(strconv.FormatBool(v))
		case int:
			b.WriteString(strconv.FormatInt(int64(v), 10))
		case int64:
			b.WriteString(strconv.FormatInt(v, 10))
		case float64:
			b.WriteString(strconv.FormatFloat(v, 'g', -1, 64))
		default:
			b.WriteString("<unhashable>")
		}
	}
	return b.String()
}
