// Package renderers provides immutable safe renderer registries for TUI data.
package renderers

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"unicode"
)

const defaultSummaryLimit = 240

// heavyFoldThreshold is the terminal-absorption bound: a normalised
// string or byte-slice field whose length reaches it folds to the
// heavy-content marker instead of rendering, so a single oversize field
// cannot flood a terminal's scrollback.
//
// It answers how many bytes a human's scrollback may absorb, not how
// many bytes may enter a model's context window, so it carries its own
// literal instead of aliasing the config package's heavy-output
// default as it once did. That constant has since moved to 128 KiB for the
// prompt-size question; a 128 KiB field pasted into a terminal is still
// unreadable, so this bound did not follow it.
const heavyFoldThreshold = 32 * 1024

// Value is a renderer input. Payload must already be a canonical redacted wire
// projection; the registry additionally bounds and escapes it for terminals.
type Value struct {
	Kind, ID, Title, Status string
	Payload                 any
}

// Block is a terminal-neutral rendered summary.
type Block struct {
	Kind, ID, Title, Status, Summary string
}

// Renderer converts one canonical value to a bounded block.
type Renderer func(Value) Block

// Registry is immutable after construction and safe for concurrent reuse.
type Registry struct {
	byKind   map[string]Renderer
	fallback Renderer
}

// New constructs a registry from a copied definition map.
func New(definitions map[string]Renderer) Registry {
	byKind := make(map[string]Renderer, len(definitions))
	for kind, renderer := range definitions {
		if strings.TrimSpace(kind) != "" && renderer != nil {
			byKind[kind] = renderer
		}
	}
	return Registry{byKind: byKind, fallback: Generic}
}

// Render dispatches by kind and always returns a safe generic fallback.
func (r Registry) Render(value Value) Block {
	value.Kind = SafeText(value.Kind)
	value.ID = SafeText(value.ID)
	value.Title = SafeText(value.Title)
	value.Status = SafeText(value.Status)
	value.Payload = Normalize(value.Payload)
	renderer := r.byKind[value.Kind]
	if renderer == nil {
		renderer = r.fallback
	}
	block := renderer(value)
	block.Kind, block.ID = SafeText(block.Kind), SafeText(block.ID)
	block.Title, block.Status = SafeText(block.Title), SafeText(block.Status)
	block.Summary = SafeText(block.Summary)
	return block
}

// Kinds returns the deterministic registered inventory.
func (r Registry) Kinds() []string {
	out := make([]string, 0, len(r.byKind))
	for kind := range r.byKind {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

// Generic renders unknown or malformed values as bounded metadata-only JSON.
func Generic(value Value) Block {
	summary := "metadata unavailable"
	if value.Payload != nil {
		if body, err := json.Marshal(Normalize(value.Payload)); err == nil {
			summary = string(body)
		} else {
			summary = fmt.Sprintf("unrenderable %T", value.Payload)
		}
	}
	title := strings.TrimSpace(value.Title)
	if title == "" {
		title = "Unknown " + strings.TrimSpace(value.Kind)
	}
	return Block{Kind: value.Kind, ID: value.ID, Title: title, Status: value.Status, Summary: summary}
}

// Builtins returns the in-repo renderers through the same registry path used
// for unknown values. Built-ins intentionally remain metadata-only.
func Builtins() Registry {
	return New(map[string]Renderer{
		"event":    Generic,
		"tool":     Generic,
		"result":   Generic,
		"artifact": Generic,
	})
}

// Normalize converts arbitrary typed payloads to JSON-shaped values, recursively
// redacts canonical secret fields, strips terminal controls, and refuses raw
// heavy content. Heavy values must arrive as an artifact reference.
func Normalize(value any) any {
	return normalize(reflect.ValueOf(value), map[visit]bool{})
}

type visit struct {
	typ reflect.Type
	ptr uintptr
}

func normalize(value reflect.Value, seen map[visit]bool) any {
	if !value.IsValid() {
		return nil
	}
	for value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil
		}
		if value.Kind() == reflect.Pointer {
			key := visit{value.Type(), value.Pointer()}
			if seen[key] {
				return "[CYCLIC]"
			}
			seen[key] = true
			defer delete(seen, key)
		}
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.String:
		text := value.String()
		if len(text) >= heavyFoldThreshold {
			return "[HEAVY CONTENT OMITTED: artifact reference required]"
		}
		return SafeText(text)
	case reflect.Bool:
		return value.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return value.Uint()
	case reflect.Float32, reflect.Float64:
		return value.Float()
	case reflect.Slice, reflect.Array:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			if value.Len() >= heavyFoldThreshold {
				return "[HEAVY CONTENT OMITTED: artifact reference required]"
			}
			return fmt.Sprintf("[binary %d bytes]", value.Len())
		}
		out := make([]any, value.Len())
		for index := range value.Len() {
			out[index] = normalize(value.Index(index), seen)
		}
		return out
	case reflect.Map:
		out := map[string]any{}
		iterator := value.MapRange()
		for iterator.Next() {
			key := SafeText(fmt.Sprint(iterator.Key().Interface()))
			if secretKey(key) {
				out[key] = "[REDACTED]"
			} else {
				out[key] = normalize(iterator.Value(), seen)
			}
		}
		return out
	case reflect.Struct:
		body, err := json.Marshal(value.Interface())
		if err != nil {
			return "[unrenderable " + value.Type().String() + "]"
		}
		var decoded any
		if json.Unmarshal(body, &decoded) != nil {
			return "[unrenderable " + value.Type().String() + "]"
		}
		return normalize(reflect.ValueOf(decoded), seen)
	default:
		return SafeText(fmt.Sprint(value.Interface()))
	}
}

func secretKey(key string) bool {
	lower := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	for _, fragment := range []string{"token", "secret", "password", "authorization", "api_key", "apikey", "credential", "cookie", "private_key"} {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}

// SafeText removes terminal control and escape characters and bounds a display
// field. Every operator-visible value passes this function.
func SafeText(value string) string {
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > defaultSummaryLimit {
		value = string(runes[:defaultSummaryLimit-1]) + "…"
	}
	return value
}
