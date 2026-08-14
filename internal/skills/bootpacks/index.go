// index.go — the frozen lookup surface of the boot-pack index.
//
// An [Index] is built once by [New] and never mutates. Every lookup
// returns deep copies so no caller can mutate the frozen state or
// observe another caller's writes; the index itself never touches the
// filesystem after construction.

package bootpacks

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sort"
	"strconv"

	"github.com/hurtener/Harbor/internal/skills"
	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

// Index is the eager immutable boot-pack index: one bucket per exact
// (tenant_id, agent_id) key. Safe for N concurrent goroutines after
// construction.
type Index struct {
	byKey map[Key]*bucket
	keys  []Key
}

// bucket is the frozen per-key state: entries in canonical-name order,
// the canonical-name index, and the deterministic boot-pack set hash.
type bucket struct {
	entries []Entry
	byName  map[string]Entry
	setHash string
}

// newIndex freezes the buckets and derives the deterministic ordered
// key list (sorted by tenant_id then agent_id).
func newIndex(byKey map[Key]*bucket) *Index {
	keys := make([]Key, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].TenantID != keys[j].TenantID {
			return keys[i].TenantID < keys[j].TenantID
		}
		return keys[i].AgentID < keys[j].AgentID
	})
	return &Index{byKey: byKey, keys: keys}
}

// Keys returns the deterministic ordered (tenant_id, agent_id) keys of
// the index, sorted by tenant then agent. The returned slice is a
// fresh copy; the index's internal ordering is never exposed.
func (ix *Index) Keys() []Key {
	out := make([]Key, len(ix.keys))
	copy(out, ix.keys)
	return out
}

// Lookup returns a deep-copy snapshot of the frozen entries for the
// exact (tenantID, agentID) pair, ordered by canonical skill name, and
// whether the key exists. A key declared in the config always exists;
// a key absent from the config (including one removed between
// deployments) is simply not present — there is no tombstone.
func (ix *Index) Lookup(tenantID, agentID string) ([]Entry, bool) {
	b, ok := ix.byKey[Key{TenantID: tenantID, AgentID: agentID}]
	if !ok {
		return nil, false
	}
	out := make([]Entry, len(b.entries))
	for i, e := range b.entries {
		out[i] = deepCopyEntry(e)
	}
	return out, true
}

// OwnsName reports whether name is a boot-declared canonical skill
// name for the exact (tenantID, agentID) pair. The input is
// canonicalized (lowercased + trimmed) before the lookup, mirroring
// the loader's own dedup key.
func (ix *Index) OwnsName(tenantID, agentID, name string) bool {
	b, ok := ix.byKey[Key{TenantID: tenantID, AgentID: agentID}]
	if !ok {
		return false
	}
	_, owns := b.byName[skillpkg.CanonicalName(name)]
	return owns
}

// BootPackSetHash returns the deterministic boot-pack set hash for the
// exact (tenantID, agentID) pair: sha256 over the canonical ordered
// name+semantic-hash pairs of the key's entries (sorted by canonical
// name), hex-encoded. Identical files across restarts produce an
// identical hash; any content or membership change perturbs it.
func (ix *Index) BootPackSetHash(tenantID, agentID string) (string, bool) {
	b, ok := ix.byKey[Key{TenantID: tenantID, AgentID: agentID}]
	if !ok {
		return "", false
	}
	return b.setHash, true
}

// setHashEnvelope is the versioned prefix mixed into the set-hash
// input so the v1 envelope cannot collide with a bare framing hash.
const setHashEnvelope = "boot-pack-set-v1\x00"

// buildBucket freezes a key's entries: canonical-name order (the
// deterministic order every lookup and the set hash use) plus the
// canonical-name index and the set hash.
func buildBucket(entries []Entry) *bucket {
	sort.Slice(entries, func(i, j int) bool {
		return skillpkg.CanonicalName(entries[i].Skill.Name) < skillpkg.CanonicalName(entries[j].Skill.Name)
	})
	b := &bucket{
		entries: entries,
		byName:  make(map[string]Entry, len(entries)),
	}
	for _, e := range entries {
		b.byName[skillpkg.CanonicalName(e.Skill.Name)] = e
	}
	b.setHash = setHash(entries)
	return b
}

// setHash computes the deterministic set hash over the canonical
// ordered name+semantic-hash pairs. Each pair is length-framed so no
// skill name content can perturb the framing, and the entries are
// already unique by canonical name and sorted, so the input bytes are
// a pure function of the set.
func setHash(entries []Entry) string {
	h := sha256.New()
	_, _ = io.WriteString(h, setHashEnvelope)
	for _, e := range entries {
		writeFramed(h, skillpkg.CanonicalName(e.Skill.Name))
		writeFramed(h, e.SemanticHash)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeFramed appends the length-prefixed framing of one field:
// "<byte-len>:<bytes>;". The framing is unambiguous regardless of the
// field's content.
func writeFramed(w io.Writer, s string) {
	_, _ = io.WriteString(w, strconv.Itoa(len(s)))
	_, _ = io.WriteString(w, ":")
	_, _ = io.WriteString(w, s)
	_, _ = io.WriteString(w, ";")
}

// deepCopyEntry returns a deep copy of one entry: the Skill's slice
// and map fields are copied so the returned value shares no mutable
// backing array with the frozen index.
func deepCopyEntry(e Entry) Entry {
	e.Skill = deepCopySkill(e.Skill)
	return e
}

// deepCopySkill returns a deep copy of a stored skill: every slice
// field and the Extra map are copied. Nil slices stay nil so the
// absent/present distinction of the parsed envelope is preserved.
func deepCopySkill(s skills.Skill) skills.Skill {
	s.Tags = append([]string(nil), s.Tags...)
	s.Steps = append([]string(nil), s.Steps...)
	s.Preconditions = append([]string(nil), s.Preconditions...)
	s.FailureModes = append([]string(nil), s.FailureModes...)
	s.RequiredTools = append([]string(nil), s.RequiredTools...)
	s.RequiredNS = append([]string(nil), s.RequiredNS...)
	s.RequiredTags = append([]string(nil), s.RequiredTags...)
	if len(s.Extra) > 0 {
		m := make(map[string]any, len(s.Extra))
		for k, v := range s.Extra {
			m[k] = v
		}
		s.Extra = m
	}
	return s
}
