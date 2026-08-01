package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// MaxModelToolNameBytes is Harbor's provider-safe, model-visible tool-name
// budget. Catalog keys remain unchanged; only declarations and model-authored
// references use this projection.
const MaxModelToolNameBytes = 44

const modelToolNameDigestBytes = 8
const minModelToolNameDigestBudget = modelToolNameDigestBytes + 1

var reservedModelToolNames = [...]string{
	"_finish",
	"_spawn_task",
	"_await_task",
	"_task_status",
	"_cancel_task",
	"_steer_task",
	"_pause_task",
	"_resume_task",
}

// ReservedModelToolNames returns the runtime-control names that claim the
// model-visible tool namespace before catalog tools. The caller owns the
// returned slice. Keeping this vocabulary below any concrete planner lets
// declarations and catalog meta-tools seed the same projection without a
// tools-to-planner dependency.
func ReservedModelToolNames() []string {
	return append([]string(nil), reservedModelToolNames[:]...)
}

// NewModelToolNameProjectionWithReservedControls projects catalogNames after
// seeding Harbor's runtime-control namespace. It is the allocation-conscious
// production form of calling [NewModelToolNameProjection] with
// [ReservedModelToolNames]: the package-owned array is read during
// construction but never exposed to callers.
func NewModelToolNameProjectionWithReservedControls(catalogNames []string) ModelToolNameProjection {
	return NewModelToolNameProjection(catalogNames, reservedModelToolNames[:])
}

// ModelToolNameEntry is one catalog name that survived projection onto the
// provider-safe model-visible namespace.
type ModelToolNameEntry struct {
	CatalogName  string
	DeclaredName string
}

// ModelToolNameCollision records a catalog name omitted because an earlier
// catalog name already owns its model-visible declaration.
type ModelToolNameCollision struct {
	DeclaredName string
	DeclaredTool string
	DroppedTool  string
}

// ModelToolNameProjection is an immutable declared-name projection. The first
// catalog name in construction order wins a residual collision. Reserved names
// claim their declaration without becoming catalog-resolvable entries.
//
// A constructed projection is safe for concurrent reuse: all maps and slices
// are private construction-time copies and no method mutates them.
type ModelToolNameProjection struct {
	entries    []ModelToolNameEntry
	collisions []ModelToolNameCollision
	byDeclared map[string]string
	byCatalog  map[string]string
	reserved   map[string]struct{}
}

// NewModelToolNameProjection projects catalogNames in declaration precedence
// order. Empty names are ignored. A repeated catalog name is a benign
// re-encounter; a distinct name mapping to an occupied declaration is returned
// through Collisions.
func NewModelToolNameProjection(catalogNames, reserved []string) ModelToolNameProjection {
	p := ModelToolNameProjection{
		entries:    make([]ModelToolNameEntry, 0, len(catalogNames)),
		byDeclared: make(map[string]string, len(catalogNames)+len(reserved)),
		byCatalog:  make(map[string]string, len(catalogNames)),
		reserved:   make(map[string]struct{}, len(reserved)),
	}
	for _, name := range reserved {
		if name == "" {
			continue
		}
		declared := ModelVisibleToolName(name)
		p.byDeclared[declared] = name
		p.reserved[declared] = struct{}{}
	}
	for _, name := range catalogNames {
		if name == "" {
			continue
		}
		if _, seen := p.byCatalog[name]; seen {
			continue
		}
		declared := ModelVisibleToolName(name)
		if owner, occupied := p.byDeclared[declared]; occupied {
			p.collisions = append(p.collisions, ModelToolNameCollision{
				DeclaredName: declared,
				DeclaredTool: owner,
				DroppedTool:  name,
			})
			continue
		}
		p.byDeclared[declared] = name
		p.byCatalog[name] = declared
		p.entries = append(p.entries, ModelToolNameEntry{CatalogName: name, DeclaredName: declared})
	}
	return p
}

// Entries returns the surviving projection in construction order. The caller
// owns the returned slice.
func (p ModelToolNameProjection) Entries() []ModelToolNameEntry {
	return append([]ModelToolNameEntry(nil), p.entries...)
}

// Collisions returns every distinct dropped catalog name in encounter order.
// The caller owns the returned slice.
func (p ModelToolNameProjection) Collisions() []ModelToolNameCollision {
	return append([]ModelToolNameCollision(nil), p.collisions...)
}

// ResolveDeclared returns the catalog name that owns declaredName. It never
// falls back to treating model-authored input as a raw catalog key.
func (p ModelToolNameProjection) ResolveDeclared(declaredName string) (string, bool) {
	if _, reserved := p.reserved[declaredName]; reserved {
		return "", false
	}
	name, ok := p.byDeclared[declaredName]
	return name, ok
}

// DeclaredName returns the model-visible name for a surviving catalog entry.
// A dropped collider returns found=false.
func (p ModelToolNameProjection) DeclaredName(catalogName string) (string, bool) {
	name, ok := p.byCatalog[catalogName]
	return name, ok
}

// ModelVisibleToolName maps a catalog key to its deterministic provider-safe
// declaration. Invalid runes become underscores. Over-budget names retain
// their semantically useful tail and append a digest of the full sanitized
// key so tools from one long source remain distinguishable.
func ModelVisibleToolName(name string) string {
	return ModelVisibleToolNameTo(name, MaxModelToolNameBytes)
}

// ModelVisibleToolNameTo applies ModelVisibleToolName with an explicit budget.
// It is total for every integer budget so measurement and adversarial tests can
// sweep the boundary without production-only assumptions.
func ModelVisibleToolNameTo(name string, budget int) string {
	clean := true
	for _, r := range name {
		if !IsModelToolNameRune(r) {
			clean = false
			break
		}
	}
	if clean && len(name) <= budget {
		return name
	}
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		if IsModelToolNameRune(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	s := b.String()
	if len(s) > budget {
		s = shortenModelToolName(s, budget)
	}
	return s
}

func shortenModelToolName(s string, budget int) string {
	if budget <= 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	digest := hex.EncodeToString(sum[:])[:modelToolNameDigestBytes]
	if budget < minModelToolNameDigestBudget {
		return digest[:budget]
	}
	keep := budget - modelToolNameDigestBytes - 1
	return s[len(s)-keep:] + "_" + digest
}

// IsModelToolNameRune reports whether a rune is accepted unchanged in a
// provider-safe model-visible tool name.
func IsModelToolNameRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		return true
	default:
		return false
	}
}
