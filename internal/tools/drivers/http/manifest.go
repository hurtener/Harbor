package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/hurtener/Harbor/internal/tools"
)

// Manifest is the UTCP-style YAML schema describing one or more
// HTTP tools as Harbor `Tool`s. Operators ship manifests via
// `ToolsConfig.HTTPManifests` (paths to YAML files); Harbor loads
// each at boot via `LoadManifest` and registers the tools via
// `RegisterManifest`.
//
// Schema (YAML):
//
//	auth:
//	  weather_key:
//	    kind: api_key
//	    header: X-API-Key
//	    secret: ${WEATHER_API_KEY}      # env-ref form is mandatory; literal secrets rejected
//	  github_bot:
//	    kind: bearer
//	    secret: ${GITHUB_BOT_TOKEN}
//
//	tools:
//	  - name: weather.lookup
//	    method: GET
//	    url_template: https://api.weather.example/v1/now?city={{ .Args.city | urlquery }}
//	    description: Look up current weather by city name.
//	    auth_ref: weather_key
//	    args_schema: '{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}'
//	    out_schema:  '{"type":"object","properties":{"temp_c":{"type":"number"}}}'
//	    side_effect: external
//	    tags: [weather]
//	    loading: always
//
//	  - name: gh.create_issue
//	    method: POST
//	    url_template: https://api.github.com/repos/{{ .Args.owner }}/{{ .Args.repo }}/issues
//	    body_template: '{"title":{{ .Args.title }},"body":{{ .Args.body }}}'
//	    auth_ref: github_bot
//
// Security boundary: a template that references the `.Auth`
// namespace is rejected loudly at load time (AGENTS.md §7 — no
// credential passthrough). Secrets are sourced from `auth_ref`
// lookups only, and the manifest may NOT inline literal secret
// strings — every `auth.<name>.secret` value MUST match `${ENV_VAR}`
// shape so the actual value lives in the operator's environment /
// secret manager.
type Manifest struct {
	// Tools declares the HTTP tool surface.
	Tools []ManifestTool `yaml:"tools"`
	// Auth declares static-auth specs keyed by `auth_ref`. A tool
	// references one via ManifestTool.AuthRef.
	Auth map[string]ManifestAuth `yaml:"auth,omitempty"`
}

// ManifestTool is one HTTP-tool entry in a manifest.
type ManifestTool struct {
	Name        string            `yaml:"name"`
	Method      string            `yaml:"method"`
	URLTemplate string            `yaml:"url_template"`
	Description string            `yaml:"description,omitempty"`
	ArgsSchema  string            `yaml:"args_schema,omitempty"`
	OutSchema   string            `yaml:"out_schema,omitempty"`
	Headers     map[string]string `yaml:"headers,omitempty"`
	Body        string            `yaml:"body_template,omitempty"`
	AuthRef     string            `yaml:"auth_ref,omitempty"`
	SideEffect  tools.SideEffect  `yaml:"side_effect,omitempty"`
	Tags        []string          `yaml:"tags,omitempty"`
	AuthScopes  []string          `yaml:"auth_scopes,omitempty"`
	Loading     tools.LoadingMode `yaml:"loading,omitempty"`
	Policy      *tools.ToolPolicy `yaml:"policy,omitempty"`
	// SourceID overrides the descriptor's ToolSourceID; defaults to
	// "manifest:<file>#<name>" when empty.
	SourceID string `yaml:"source_id,omitempty"`
}

// ManifestAuth is one static-auth spec entry in a manifest. The
// Secret field carries the operator-supplied secret value in
// `${ENV_VAR}` reference form; literal values are rejected at load
// time (AGENTS.md §7).
type ManifestAuth struct {
	Kind   AuthKind `yaml:"kind"`
	Header string   `yaml:"header,omitempty"`
	Query  string   `yaml:"query,omitempty"`
	Cookie string   `yaml:"cookie,omitempty"`
	Secret string   `yaml:"secret" secret:"true"`
}

// ErrManifestInvalid wraps every manifest-loader / validation
// failure. Callers compare via errors.Is.
var ErrManifestInvalid = errors.New("http: manifest invalid")

// envRefRegex matches `${IDENT}` references in manifest secret
// fields. The IDENT is restricted to typical env-var shape
// (alphanumeric + underscore, starting with a non-digit) so a
// typo'd reference fails loudly at expansion time.
var envRefRegex = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

// LoadManifest reads a YAML manifest from disk and returns a
// fully-validated `*Manifest` with `${ENV_VAR}` secret references
// expanded against `os.Getenv`. Returns `ErrManifestInvalid` wrapped
// with the offending detail on any validation failure (unknown
// fields, missing referenced env var, literal secret value, name
// collision, template-secret leak, etc.).
//
// Path traversal is mitigated via `filepath.Clean` — the resulting
// absolute path is what the loader reads.
func LoadManifest(path string) (*Manifest, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: empty path", ErrManifestInvalid)
	}
	clean := filepath.Clean(path)
	abs, err := filepath.Abs(clean)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve path %q: %w", ErrManifestInvalid, path, err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("%w: read %q: %w", ErrManifestInvalid, abs, err)
	}
	var m Manifest
	if err := yaml.UnmarshalWithOptions(data, &m, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("%w: parse %q: %w", ErrManifestInvalid, abs, err)
	}
	if err := m.validate(abs); err != nil {
		return nil, err
	}
	if err := m.expandSecrets(abs); err != nil {
		return nil, err
	}
	return &m, nil
}

// validate runs structural checks against the parsed manifest.
// Called before expandSecrets so a leaky template fails before the
// loader touches the environment.
func (m *Manifest) validate(source string) error {
	if len(m.Tools) == 0 {
		return fmt.Errorf("%w: %q has no tools", ErrManifestInvalid, source)
	}
	seen := make(map[string]struct{}, len(m.Tools))
	for i, t := range m.Tools {
		path := fmt.Sprintf("tools[%d](%s)", i, t.Name)
		if strings.TrimSpace(t.Name) == "" {
			return fmt.Errorf("%w: %s name is empty", ErrManifestInvalid, path)
		}
		if _, dup := seen[t.Name]; dup {
			return fmt.Errorf("%w: duplicate tool name %q in %q", ErrManifestInvalid, t.Name, source)
		}
		seen[t.Name] = struct{}{}

		method := strings.ToUpper(strings.TrimSpace(t.Method))
		if _, ok := allowedMethods[method]; !ok {
			return fmt.Errorf("%w: %s method %q not in allowlist", ErrManifestInvalid, path, t.Method)
		}
		if strings.TrimSpace(t.URLTemplate) == "" {
			return fmt.Errorf("%w: %s url_template is empty", ErrManifestInvalid, path)
		}
		// Secret-leak check on the URL + body + headers BEFORE compile.
		if err := checkNoSecretLeak("url_template", t.URLTemplate); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrManifestInvalid, path, err)
		}
		if t.Body != "" {
			if err := checkNoSecretLeak("body_template", t.Body); err != nil {
				return fmt.Errorf("%w: %s: %w", ErrManifestInvalid, path, err)
			}
		}
		for k, v := range t.Headers {
			if err := checkNoSecretLeak("header["+k+"]", v); err != nil {
				return fmt.Errorf("%w: %s: %w", ErrManifestInvalid, path, err)
			}
		}

		// Schema fields, when present, must be valid JSON. We don't
		// compile here — compile happens in RegisterManifest where
		// the cached validators are stored.
		if t.ArgsSchema != "" {
			if !json.Valid([]byte(t.ArgsSchema)) {
				return fmt.Errorf("%w: %s args_schema is not valid JSON", ErrManifestInvalid, path)
			}
		}
		if t.OutSchema != "" {
			if !json.Valid([]byte(t.OutSchema)) {
				return fmt.Errorf("%w: %s out_schema is not valid JSON", ErrManifestInvalid, path)
			}
		}

		// auth_ref MUST resolve to an entry in m.Auth.
		if t.AuthRef != "" {
			if _, ok := m.Auth[t.AuthRef]; !ok {
				return fmt.Errorf("%w: %s auth_ref %q has no matching entry under the top-level auth section",
					ErrManifestInvalid, path, t.AuthRef)
			}
		}
	}

	// Validate each auth entry's spec shape and reject literal
	// secret values up front. expandSecrets does the env lookup
	// afterwards.
	for name, a := range m.Auth {
		path := fmt.Sprintf("auth[%s]", name)
		spec := AuthSpec{
			Kind: a.Kind, HeaderName: a.Header, QueryParam: a.Query, CookieName: a.Cookie,
		}
		if err := spec.Validate(); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrManifestInvalid, path, err)
		}
		if a.Kind == AuthKindNone {
			// Auth entries should never declare Kind=none — that's
			// the absence of auth, not an entry to reference.
			return fmt.Errorf("%w: %s kind must not be empty (use api_key / bearer / cookie)",
				ErrManifestInvalid, path)
		}
		if strings.TrimSpace(a.Secret) == "" {
			return fmt.Errorf("%w: %s secret is empty", ErrManifestInvalid, path)
		}
		if !envRefRegex.MatchString(a.Secret) {
			return fmt.Errorf("%w: %s secret must be a ${ENV_VAR} reference (got literal value, refusing)",
				ErrManifestInvalid, path)
		}
	}

	return nil
}

// expandSecrets walks every auth entry and replaces the `${ENV_VAR}`
// reference with the actual value from `os.Getenv`. Missing
// references fail loudly — we don't allow a partial / empty auth at
// boot time.
func (m *Manifest) expandSecrets(source string) error {
	for name, a := range m.Auth {
		match := envRefRegex.FindStringSubmatch(a.Secret)
		// validate() already required the form, but defend.
		if len(match) != 2 {
			return fmt.Errorf("%w: auth[%s] secret ref malformed", ErrManifestInvalid, name)
		}
		envVar := match[1]
		value, ok := os.LookupEnv(envVar)
		if !ok || strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: auth[%s] env var %q unset or empty (manifest %s)",
				ErrManifestInvalid, name, envVar, source)
		}
		a.Secret = value
		m.Auth[name] = a
	}
	return nil
}

// RegisterManifest registers every tool in m against cat. Errors
// short-circuit on the first failure with the offending tool name
// in the wrapped detail; previously-registered tools remain in the
// catalog (the caller controls catalog lifetime).
//
// Concurrent reuse (D-025): each registered descriptor is safe under
// N concurrent invocations (see RegisterHTTPTool).
func RegisterManifest(cat tools.ToolCatalog, m *Manifest) error {
	if cat == nil {
		return fmt.Errorf("http.RegisterManifest: catalog is nil")
	}
	if m == nil {
		return fmt.Errorf("http.RegisterManifest: manifest is nil")
	}
	for i, t := range m.Tools {
		path := fmt.Sprintf("tools[%d](%s)", i, t.Name)
		opts, err := manifestToOptions(m, t)
		if err != nil {
			return fmt.Errorf("http.RegisterManifest %s: %w", path, err)
		}
		if err := RegisterHTTPTool(cat, t.Name, t.Method, t.URLTemplate, opts...); err != nil {
			return fmt.Errorf("http.RegisterManifest %s: %w", path, err)
		}
	}
	return nil
}

// manifestToOptions converts a ManifestTool entry into the HTTPOption
// slice consumed by RegisterHTTPTool. Lookups against the manifest's
// auth table happen here.
func manifestToOptions(m *Manifest, t ManifestTool) ([]HTTPOption, error) {
	opts := make([]HTTPOption, 0, 10)

	if t.Description != "" {
		opts = append(opts, WithDescription(t.Description))
	}
	if t.ArgsSchema != "" {
		opts = append(opts, WithArgsSchema([]byte(t.ArgsSchema)))
	}
	if t.OutSchema != "" {
		opts = append(opts, WithOutSchema([]byte(t.OutSchema)))
	}
	if len(t.Headers) > 0 {
		opts = append(opts, WithHeaders(t.Headers))
	}
	if t.Body != "" {
		opts = append(opts, WithBodyTemplate(t.Body))
	}
	if t.SideEffect != "" {
		opts = append(opts, WithSideEffect(t.SideEffect))
	}
	if t.Loading != "" {
		opts = append(opts, WithLoading(t.Loading))
	}
	if len(t.Tags) > 0 {
		opts = append(opts, WithTags(t.Tags...))
	}
	if len(t.AuthScopes) > 0 {
		opts = append(opts, WithAuthScopes(t.AuthScopes...))
	}
	if t.Policy != nil {
		opts = append(opts, WithPolicy(*t.Policy))
	}
	if t.AuthRef != "" {
		a, ok := m.Auth[t.AuthRef]
		if !ok {
			return nil, fmt.Errorf("auth_ref %q not found", t.AuthRef)
		}
		spec := AuthSpec{
			Kind: a.Kind, HeaderName: a.Header, QueryParam: a.Query, CookieName: a.Cookie,
		}
		opts = append(opts, WithAuth(spec, a.Secret))
	}
	if t.SourceID != "" {
		opts = append(opts, WithHTTPSource(tools.ToolSourceID(t.SourceID)))
	} else {
		opts = append(opts, WithHTTPSource(tools.ToolSourceID("manifest:"+t.Name)))
	}
	return opts, nil
}
