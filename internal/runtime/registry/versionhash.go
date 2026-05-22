package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// VersionHash computes the deterministic content hash of an
// AgentConfig (RFC §6.16 "version_hash"; algorithm settled in D-068).
//
// The hash answers "which configuration" — it bumps iff the
// configuration *content* changes and is otherwise stable across a
// plain restart. To make it deterministic regardless of caller-side
// ordering, the config is canonicalised before hashing:
//
//   - Prompts slice is sorted (prompt order is not semantic).
//   - Tools slice is sorted by (Name, SchemaDigest) (binding order is
//     not semantic).
//   - PlannerConfig / ModelPolicy maps are encoded with sorted keys
//     (Go's encoding/json already sorts map keys, but the canonical
//     struct makes the contract explicit and robust to a future
//     encoder swap).
//
// The canonical form is JSON-encoded and SHA-256 hashed; the result is
// lowercase hex. The function is pure: same content in, same hash out,
// no package-level state, safe for concurrent use (D-025).
func VersionHash(cfg AgentConfig) (string, error) {
	canonical := canonicalConfig{
		Prompts:       append([]string(nil), cfg.Prompts...),
		Tools:         make([]canonicalTool, 0, len(cfg.Tools)),
		PlannerConfig: sortedKV(cfg.PlannerConfig),
		ModelPolicy:   sortedKV(cfg.ModelPolicy),
	}
	sort.Strings(canonical.Prompts)
	for _, t := range cfg.Tools {
		canonical.Tools = append(canonical.Tools, canonicalTool(t))
	}
	sort.Slice(canonical.Tools, func(i, j int) bool {
		if canonical.Tools[i].Name != canonical.Tools[j].Name {
			return canonical.Tools[i].Name < canonical.Tools[j].Name
		}
		return canonical.Tools[i].SchemaDigest < canonical.Tools[j].SchemaDigest
	})

	b, err := json.Marshal(canonical)
	if err != nil {
		// json.Marshal of a struct of strings/slices/sorted-kv cannot
		// fail in practice; wrap loudly rather than swallow if it ever
		// does (AGENTS.md §5 "fail loudly").
		return "", fmt.Errorf("registry: version_hash canonical encode: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// canonicalConfig is the stable, order-independent shape VersionHash
// hashes. Field names are fixed so the JSON encoding is reproducible.
type canonicalConfig struct {
	Prompts       []string        `json:"prompts"`
	Tools         []canonicalTool `json:"tools"`
	PlannerConfig []canonicalKV   `json:"planner_config"`
	ModelPolicy   []canonicalKV   `json:"model_policy"`
}

type canonicalTool struct {
	Name         string `json:"name"`
	SchemaDigest string `json:"schema_digest"`
}

type canonicalKV struct {
	K string `json:"k"`
	V string `json:"v"`
}

// sortedKV flattens a map into a key-sorted slice so the encoding is
// deterministic and explicit (independent of any encoder's map-key
// ordering behaviour).
func sortedKV(m map[string]string) []canonicalKV {
	if len(m) == 0 {
		return []canonicalKV{}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]canonicalKV, 0, len(keys))
	for _, k := range keys {
		out = append(out, canonicalKV{K: k, V: m[k]})
	}
	return out
}
