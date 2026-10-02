// Package pricing validates immutable, operator-authorized all-charge ceilings.
// It never consumes model output, provider cost floats, or inferred market prices.
package pricing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

// ErrUnavailable means no trusted, complete price bound can be established.
var ErrUnavailable = errors.New("llm: trusted allocation pricing unavailable")

// Manifest is boot configuration, never a task or model supplied tariff.
// Any content change requires a new revision. All values are USD micro-units.
type Manifest struct {
	ID       string   `json:"id" yaml:"id"`
	Revision uint64   `json:"revision" yaml:"revision"`
	Currency string   `json:"currency" yaml:"currency"`
	Tariffs  []Tariff `json:"tariffs" yaml:"tariffs"`
}

// Tariff bounds every charge for one physical text-inference attempt. Explicit
// zero pointers distinguish a verified zero ceiling from an omitted dimension.
// Input/cache and output/reasoning ceilings are added, never discounted. The
// ancillary ceiling covers every charge not represented by those token rates.
// Version 1 requires Model and ModelVersion to be the same immutable provider
// selector. Operators must attest that the selector cannot alias another version.
type Tariff struct {
	// EndpointBinding is "provider_default" or "sha256:" plus the hash of
	// the exact operator-configured base URL. It contains no endpoint URL.
	EndpointBinding              string `json:"endpoint_binding" yaml:"endpoint_binding"`
	Provider                     string `json:"provider" yaml:"provider"`
	Model                        string `json:"model" yaml:"model"`
	ModelVersion                 string `json:"model_version" yaml:"model_version"`
	ImmutableModelVersion        bool   `json:"immutable_model_version" yaml:"immutable_model_version"`
	IncludesAllCharges           bool   `json:"includes_all_charges" yaml:"includes_all_charges"`
	InputMicroUSDPerMillion      *int64 `json:"input_micro_usd_per_million" yaml:"input_micro_usd_per_million"`
	OutputMicroUSDPerMillion     *int64 `json:"output_micro_usd_per_million" yaml:"output_micro_usd_per_million"`
	CacheReadMicroUSDPerMillion  *int64 `json:"cache_read_micro_usd_per_million" yaml:"cache_read_micro_usd_per_million"`
	CacheWriteMicroUSDPerMillion *int64 `json:"cache_write_micro_usd_per_million" yaml:"cache_write_micro_usd_per_million"`
	ReasoningMicroUSDPerMillion  *int64 `json:"reasoning_micro_usd_per_million" yaml:"reasoning_micro_usd_per_million"`
	RequestMicroUSD              *int64 `json:"request_micro_usd" yaml:"request_micro_usd"`
	AncillaryMicroUSD            *int64 `json:"ancillary_micro_usd" yaml:"ancillary_micro_usd"`
}

// Reference is a content-addressed tariff version pinned at task acceptance.
type Reference struct {
	ID       string
	Revision uint64
	SHA256   string
}

// Catalog owns detached immutable manifests. Construction is an operator/coordinator
// privilege in the SDK; no Protocol, prompt, or tool accepts manifest contents.
type Catalog struct{ entries map[key]entry }
type key struct {
	id       string
	revision uint64
}
type entry struct {
	manifest Manifest
	digest   string
}

func identifier(s string) bool {
	return s != "" && len(s) <= 256 && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "\x00\r\n\t")
}

// New validates and copies every manifest. Duplicate identities are refused even
// if their contents match, so precedence never selects a tariff accidentally.
func New(manifests []Manifest) (*Catalog, error) {
	c := &Catalog{entries: make(map[key]entry, len(manifests))}
	for _, m := range manifests {
		if !identifier(m.ID) || m.Revision == 0 || m.Currency != "USD" || len(m.Tariffs) == 0 {
			return nil, ErrUnavailable
		}
		k := key{m.ID, m.Revision}
		if _, exists := c.entries[k]; exists {
			return nil, fmt.Errorf("%w: duplicate manifest identity", ErrUnavailable)
		}
		targets := map[[3]string]bool{}
		for _, t := range m.Tariffs {
			if !identifier(t.Provider) || !identifier(t.Model) || !validEndpointBinding(t.EndpointBinding) || t.ModelVersion != t.Model || !t.ImmutableModelVersion || !t.IncludesAllCharges {
				return nil, ErrUnavailable
			}
			target := [3]string{t.Provider, t.Model, t.EndpointBinding}
			if targets[target] {
				return nil, fmt.Errorf("%w: duplicate tariff target", ErrUnavailable)
			}
			targets[target] = true
			for _, rate := range []*int64{t.InputMicroUSDPerMillion, t.OutputMicroUSDPerMillion, t.CacheReadMicroUSDPerMillion, t.CacheWriteMicroUSDPerMillion, t.ReasoningMicroUSDPerMillion, t.RequestMicroUSD, t.AncillaryMicroUSD} {
				if rate == nil || *rate < 0 {
					return nil, ErrUnavailable
				}
			}
		}
		b, err := json.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("pricing manifest: %w", err)
		}
		var detached Manifest
		if err = json.Unmarshal(b, &detached); err != nil {
			return nil, fmt.Errorf("pricing manifest: %w", err)
		}
		h := sha256.Sum256(b)
		c.entries[k] = entry{detached, hex.EncodeToString(h[:])}
	}
	return c, nil
}

// ValidReference checks shape without establishing pricing authority.
func ValidReference(r Reference) bool {
	b, err := hex.DecodeString(r.SHA256)
	return identifier(r.ID) && r.Revision > 0 && err == nil && len(b) == sha256.Size && strings.ToLower(r.SHA256) == r.SHA256
}

// References returns only content-free immutable identities.
func (c *Catalog) References() []Reference {
	if c == nil {
		return nil
	}
	out := make([]Reference, 0, len(c.entries))
	for k, e := range c.entries {
		out = append(out, Reference{k.id, k.revision, e.digest})
	}
	return out
}

// ValidateReference proves the accepted hash still names the installed version.
func (c *Catalog) ValidateReference(r Reference) error {
	if c == nil || !ValidReference(r) {
		return ErrUnavailable
	}
	e, ok := c.entries[key{r.ID, r.Revision}]
	if !ok || e.digest != r.SHA256 {
		return ErrUnavailable
	}
	return nil
}

// Quote rounds each nonnegative category upward for each possible physical
// attempt, checking multiplication and addition before any arithmetic can wrap.
func (c *Catalog) Quote(r Reference, provider, model, endpoint string, input, output, attempts int64) (int64, error) {
	if err := c.ValidateReference(r); err != nil {
		return 0, err
	}
	if input <= 0 || output <= 0 || attempts <= 0 || attempts > 1000 {
		return 0, ErrUnavailable
	}
	for _, t := range c.entries[key{r.ID, r.Revision}].manifest.Tariffs {
		if t.Provider != provider || t.Model != model || t.ModelVersion != model || t.EndpointBinding != endpoint {
			continue
		}
		var sum int64
		for _, category := range []struct{ units, rate int64 }{{input, *t.InputMicroUSDPerMillion}, {input, *t.CacheReadMicroUSDPerMillion}, {input, *t.CacheWriteMicroUSDPerMillion}, {output, *t.OutputMicroUSDPerMillion}, {output, *t.ReasoningMicroUSDPerMillion}} {
			v, err := ceilRate(category.units, category.rate)
			if err != nil || sum > math.MaxInt64-v {
				return 0, ErrUnavailable
			}
			sum += v
		}
		for _, fixed := range []int64{*t.RequestMicroUSD, *t.AncillaryMicroUSD} {
			if sum > math.MaxInt64-fixed {
				return 0, ErrUnavailable
			}
			sum += fixed
		}
		if sum > math.MaxInt64/attempts {
			return 0, ErrUnavailable
		}
		return sum * attempts, nil
	}
	return 0, ErrUnavailable
}

func ceilRate(units, rate int64) (int64, error) {
	// Quotient decomposition avoids rejecting a representable result merely
	// because the unscaled product would overflow an int64.
	const million int64 = 1_000_000
	whole, remainder := units/million, units%million
	if rate != 0 && whole > math.MaxInt64/rate {
		return 0, ErrUnavailable
	}
	base := whole * rate
	rq, rr := rate/million, rate%million
	if rq != 0 && remainder > math.MaxInt64/rq {
		return 0, ErrUnavailable
	}
	tail := remainder * rq
	fraction := (remainder*rr + million - 1) / million
	if tail > math.MaxInt64-fraction || base > math.MaxInt64-tail-fraction {
		return 0, ErrUnavailable
	}
	return base + tail + fraction, nil
}

// EndpointBinding hashes an exact configured provider base URL. Empty selects
// only the pinned driver's provider default, never an arbitrary proxy endpoint.
func EndpointBinding(baseURL string) string {
	if baseURL == "" {
		return "provider_default"
	}
	h := sha256.Sum256([]byte(baseURL))
	return "sha256:" + hex.EncodeToString(h[:])
}

func validEndpointBinding(binding string) bool {
	if binding == "provider_default" {
		return true
	}
	if !strings.HasPrefix(binding, "sha256:") {
		return false
	}
	digest := strings.TrimPrefix(binding, "sha256:")
	b, err := hex.DecodeString(digest)
	return err == nil && len(b) == sha256.Size && strings.ToLower(digest) == digest
}
