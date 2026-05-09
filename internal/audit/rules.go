package audit

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strings"
)

// Placeholder is the value substituted in for any redacted field.
const Placeholder = "***"

// MaxDepth caps the deep-walk recursion to defend against
// pathologically nested or cyclic payloads. A payload that exceeds
// it produces ErrRedactionDepthExceeded — the contract says callers
// must NOT emit on error, so this fails closed.
const MaxDepth = 64

// Canonical secret-name aliases. These are matched case-insensitively
// against map keys and reflective struct field names (incl. yaml/json
// tag names). The list is intentionally short: it covers the shapes
// every downstream subsystem agreed are universally sensitive.
var (
	apiKeyAliases       = []string{"api_key", "apikey", "api-key", "x-api-key"}
	bearerKeyAliases    = []string{"bearer", "bearer_token"}
	authorizationKeys   = []string{"authorization", "authorisation"}
	passwordAliases     = []string{"password", "passwd", "pass"}
	secretAliases       = []string{"secret", "client_secret", "private_key", "signing_key"}
	tokenAliases        = []string{"token", "access_token", "refresh_token", "id_token"}
	cookieAliases       = []string{"cookie", "set-cookie"}
)

// keyRule redacts values whose containing key (map key OR struct
// field name OR yaml/json tag name) matches one of aliases.
//
// Multi-modal references (ArtifactRef) are passed through unchanged;
// they are not the bytes themselves and carry no secret payload.
type keyRule struct {
	name    string
	aliases []string
}

func (r *keyRule) Name() string { return r.name }

func (r *keyRule) Apply(_ context.Context, payload any) (any, error) {
	return walkRedactKeys(payload, r.aliases, 0)
}

// regexValueRule scans every string-typed leaf of payload and applies
// a regex-driven replacement. Used for in-string credentials like
// `Authorization: Bearer xxx` where the secret is embedded in the
// value rather than the key.
type regexValueRule struct {
	name    string
	pattern *regexp.Regexp
	repl    string
}

func (r *regexValueRule) Name() string { return r.name }

func (r *regexValueRule) Apply(_ context.Context, payload any) (any, error) {
	return walkReplaceStrings(payload, func(s string) string {
		return r.pattern.ReplaceAllString(s, r.repl)
	}, 0)
}

// CanonicalRules returns the V1 default rule set. Order is
// deterministic so golden-file tests are stable across runs.
//
// Rules in order:
//
//   1. api_key, password, secret, token, cookie, authorization,
//      bearer (key-based redaction).
//   2. bearer_in_value (regex over string values for embedded
//      `Bearer xxx` credentials).
//   3. multimodal (inline DataURL / base64 image|audio|file content).
//
// Each rule's Name() is enumerable via patterns.Driver.Names() (the
// production driver shipped in this phase).
func CanonicalRules() []Rule {
	return []Rule{
		&keyRule{name: "api_key", aliases: apiKeyAliases},
		&keyRule{name: "password", aliases: passwordAliases},
		&keyRule{name: "secret", aliases: secretAliases},
		&keyRule{name: "token", aliases: tokenAliases},
		&keyRule{name: "cookie", aliases: cookieAliases},
		&keyRule{name: "authorization", aliases: authorizationKeys},
		&keyRule{name: "bearer", aliases: bearerKeyAliases},
		&regexValueRule{
			name:    "bearer_in_value",
			pattern: regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._\-+/=~]+`),
			repl:    "Bearer " + Placeholder,
		},
		&multimodalRule{name: "multimodal"},
	}
}

// matchesAlias case-insensitively checks whether key (after
// trimming whitespace) is one of the canonical aliases.
func matchesAlias(key string, aliases []string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	for _, a := range aliases {
		if k == a {
			return true
		}
	}
	return false
}

// walkRedactKeys deep-walks payload and replaces values whose
// containing key matches one of aliases. Returns a deep-copied
// result; never mutates input.
func walkRedactKeys(v any, aliases []string, depth int) (any, error) {
	if depth >= MaxDepth {
		return nil, fmt.Errorf("%w (depth=%d)", ErrRedactionDepthExceeded, depth)
	}
	if v == nil {
		return nil, nil
	}
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, sub := range val {
			if matchesAlias(k, aliases) {
				if isArtifactRef(sub) {
					out[k] = sub
					continue
				}
				out[k] = Placeholder
				continue
			}
			r, err := walkRedactKeys(sub, aliases, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			r, err := walkRedactKeys(item, aliases, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case string, []byte, bool, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, float32, float64:
		return v, nil
	}
	return reflectiveRedactKeys(v, aliases, depth+1)
}

// reflectiveRedactKeys handles pointers, structs, and other reflect
// types that the type-switch above didn't catch. Structs are
// converted to map[string]any (using yaml/json tag → field name)
// so the result is JSON-serializable for log emit.
func reflectiveRedactKeys(v any, aliases []string, depth int) (any, error) {
	if depth >= MaxDepth {
		return nil, fmt.Errorf("%w (depth=%d)", ErrRedactionDepthExceeded, depth)
	}
	if isArtifactRef(v) {
		return v, nil
	}
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return nil, nil
	}
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return nil, nil
		}
		return reflectiveRedactKeys(rv.Elem().Interface(), aliases, depth+1)
	case reflect.Struct:
		t := rv.Type()
		out := make(map[string]any, t.NumField())
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name := fieldName(f)
			if matchesAlias(name, aliases) {
				field := rv.Field(i).Interface()
				if isArtifactRef(field) {
					out[name] = field
				} else {
					out[name] = Placeholder
				}
				continue
			}
			r, err := walkRedactKeys(rv.Field(i).Interface(), aliases, depth+1)
			if err != nil {
				return nil, err
			}
			out[name] = r
		}
		return out, nil
	case reflect.Map:
		// Best-effort: keys must be strings; otherwise pass through.
		if rv.Type().Key().Kind() != reflect.String {
			return v, nil
		}
		out := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			k := iter.Key().String()
			subVal := iter.Value().Interface()
			if matchesAlias(k, aliases) {
				if isArtifactRef(subVal) {
					out[k] = subVal
				} else {
					out[k] = Placeholder
				}
				continue
			}
			r, err := walkRedactKeys(subVal, aliases, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			r, err := walkRedactKeys(rv.Index(i).Interface(), aliases, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}

// fieldName returns the YAML or JSON tag name (before any comma),
// falling back to the lowercased Go field name. Matches the loader's
// yamlName helper but with a json fallback so the audit redactor
// works on JSON-tagged event types too.
func fieldName(f reflect.StructField) string {
	if tag := f.Tag.Get("yaml"); tag != "" {
		return splitTagName(tag)
	}
	if tag := f.Tag.Get("json"); tag != "" {
		return splitTagName(tag)
	}
	return strings.ToLower(f.Name)
}

func splitTagName(tag string) string {
	if comma := strings.IndexByte(tag, ','); comma >= 0 {
		return tag[:comma]
	}
	return tag
}

// walkReplaceStrings deep-walks payload and applies replace to every
// string-typed leaf. Used by regex-driven rules.
func walkReplaceStrings(v any, replace func(string) string, depth int) (any, error) {
	if depth >= MaxDepth {
		return nil, fmt.Errorf("%w (depth=%d)", ErrRedactionDepthExceeded, depth)
	}
	if v == nil {
		return nil, nil
	}
	switch val := v.(type) {
	case string:
		return replace(val), nil
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, sub := range val {
			r, err := walkReplaceStrings(sub, replace, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			r, err := walkReplaceStrings(item, replace, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case []byte, bool, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, float32, float64:
		return v, nil
	}
	return reflectiveReplaceStrings(v, replace, depth+1)
}

func reflectiveReplaceStrings(v any, replace func(string) string, depth int) (any, error) {
	if depth >= MaxDepth {
		return nil, fmt.Errorf("%w (depth=%d)", ErrRedactionDepthExceeded, depth)
	}
	if isArtifactRef(v) {
		return v, nil
	}
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return nil, nil
	}
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return nil, nil
		}
		return reflectiveReplaceStrings(rv.Elem().Interface(), replace, depth+1)
	case reflect.Struct:
		t := rv.Type()
		out := make(map[string]any, t.NumField())
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			r, err := walkReplaceStrings(rv.Field(i).Interface(), replace, depth+1)
			if err != nil {
				return nil, err
			}
			out[fieldName(f)] = r
		}
		return out, nil
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return v, nil
		}
		out := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			r, err := walkReplaceStrings(iter.Value().Interface(), replace, depth+1)
			if err != nil {
				return nil, err
			}
			out[iter.Key().String()] = r
		}
		return out, nil
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			r, err := walkReplaceStrings(rv.Index(i).Interface(), replace, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}
