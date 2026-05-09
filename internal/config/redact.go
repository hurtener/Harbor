package config

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/goccy/go-yaml"
)

// redactedPlaceholder is what every secret-shaped field becomes in
// MarshalForLogging output. It is intentionally short and visually
// obvious so a redacted log line cannot be mistaken for the
// underlying value.
const redactedPlaceholder = "***"

// secretFallbackNames is the name-based fallback the redactor uses
// when a field lacks a `secret:"true"` tag. This is the operator
// safety net: even if a future contributor forgets the tag on a
// new secret field, the canonical names are still redacted.
var secretFallbackNames = map[string]struct{}{
	"api_key":      {},
	"apikey":       {},
	"token":        {},
	"password":     {},
	"secret":       {},
	"client_secret": {},
	"private_key":  {},
	"signing_key":  {},
}

// MarshalForLogging produces YAML bytes with secret-shaped fields
// replaced by "***". Field detection prefers `secret:"true"` struct
// tags; falls back to the canonical name list. The result is safe to
// emit to slog at boot and is intended only for logging — never feed
// it back through Load.
func (c *Config) MarshalForLogging() ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("config: MarshalForLogging called with nil")
	}
	clone := *c
	redactValue(reflect.ValueOf(&clone).Elem())
	return yaml.Marshal(&clone)
}

// redactValue walks v and overwrites every secret-shaped string
// leaf with redactedPlaceholder. Only string leaves are redacted —
// numeric / bool fields are passed through unchanged because Harbor
// does not store secrets in those shapes.
func redactValue(v reflect.Value) {
	if v.Kind() != reflect.Struct {
		return
	}
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		fv := v.Field(i)
		if fv.Kind() == reflect.Struct {
			redactValue(fv)
			continue
		}
		if !shouldRedact(f) {
			continue
		}
		if fv.Kind() != reflect.String || !fv.CanSet() {
			continue
		}
		if fv.String() == "" {
			continue
		}
		fv.SetString(redactedPlaceholder)
	}
}

// shouldRedact decides whether a field's value must be replaced by
// the redaction placeholder.
func shouldRedact(f reflect.StructField) bool {
	if f.Tag.Get("secret") == "true" {
		return true
	}
	name := yamlName(f)
	if name == "-" || name == "" {
		name = strings.ToLower(f.Name)
	}
	if _, ok := secretFallbackNames[name]; ok {
		return true
	}
	return false
}
