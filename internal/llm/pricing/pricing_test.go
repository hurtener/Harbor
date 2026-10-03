package pricing

import (
	"errors"
	"math"
	"math/big"
	"sync"
	"testing"
)

func fixture() Manifest {
	return Manifest{ID: "synthetic-only", Revision: 1, Currency: "USD", Tariffs: []Tariff{{EndpointBinding: "provider_default", Provider: "openai", Model: "fixture-v1", ModelVersion: "fixture-v1", ImmutableModelVersion: true, IncludesAllCharges: true, InputMicroUSDPerMillion: new(int64(1)), OutputMicroUSDPerMillion: new(int64(2)), CacheReadMicroUSDPerMillion: new(int64(3)), CacheWriteMicroUSDPerMillion: new(int64(4)), ReasoningMicroUSDPerMillion: new(int64(5)), RequestMicroUSD: new(int64(6)), AncillaryMicroUSD: new(int64(7))}}}
}

func TestMonetaryPricing_CompleteImmutableCeilings(t *testing.T) {
	m := fixture()
	c, err := New([]Manifest{m})
	if err != nil {
		t.Fatal(err)
	}
	r := c.References()[0]
	// Each of five token categories rounds up to one micro-unit, plus 13 fixed.
	quote, err := c.Quote(r, "openai", "fixture-v1", "provider_default", 1, 1, 3)
	if err != nil || quote != 54 {
		t.Fatalf("quote=%d err=%v", quote, err)
	}
	*m.Tariffs[0].InputMicroUSDPerMillion = 999999999
	if quote, err = c.Quote(r, "openai", "fixture-v1", "provider_default", 1, 1, 3); err != nil || quote != 54 {
		t.Fatal("caller mutated immutable catalog", quote, err)
	}
	for _, tc := range []struct{ provider, model string }{{"anthropic", "fixture-v1"}, {"openai", "alias"}, {"openai", "fixture-v2"}} {
		if _, err = c.Quote(r, tc.provider, tc.model, "provider_default", 1, 1, 1); !errors.Is(err, ErrUnavailable) {
			t.Fatal("unpriced selector", err)
		}
	}
	if _, err = c.Quote(r, "openai", "fixture-v1", EndpointBinding("https://other.example.test"), 1, 1, 1); !errors.Is(err, ErrUnavailable) {
		t.Fatal("changed endpoint", err)
	}
	r.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	if err = c.ValidateReference(r); !errors.Is(err, ErrUnavailable) {
		t.Fatal("changed contents", err)
	}
}

func TestMonetaryPricing_RejectsIncompleteOrAmbiguousAuthority(t *testing.T) {
	for name, mutate := range map[string]func(*Manifest){
		"missing endpoint":    func(m *Manifest) { m.Tariffs[0].EndpointBinding = "" },
		"currency":            func(m *Manifest) { m.Currency = "EUR" },
		"no revision":         func(m *Manifest) { m.Revision = 0 },
		"no identity":         func(m *Manifest) { m.ID = "" },
		"no tariffs":          func(m *Manifest) { m.Tariffs = nil },
		"alias":               func(m *Manifest) { m.Tariffs[0].ModelVersion = "other" },
		"unpinned":            func(m *Manifest) { m.Tariffs[0].ImmutableModelVersion = false },
		"partial charges":     func(m *Manifest) { m.Tariffs[0].IncludesAllCharges = false },
		"missing cache write": func(m *Manifest) { m.Tariffs[0].CacheWriteMicroUSDPerMillion = nil },
		"negative":            func(m *Manifest) { m.Tariffs[0].AncillaryMicroUSD = new(int64(-1)) },
		"duplicate selector":  func(m *Manifest) { m.Tariffs = append(m.Tariffs, m.Tariffs[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			m := fixture()
			mutate(&m)
			if _, err := New([]Manifest{m}); !errors.Is(err, ErrUnavailable) {
				t.Fatal(err)
			}
		})
	}
	if _, err := New([]Manifest{fixture(), fixture()}); !errors.Is(err, ErrUnavailable) {
		t.Fatal("duplicate ID", err)
	}
	var c *Catalog
	if err := c.ValidateReference(Reference{}); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestMonetaryPricing_CheckedRoundingAgainstBigInteger(t *testing.T) {
	for _, units := range []int64{0, 1, 999999, 1000000, 1000001, 1 << 31, math.MaxInt64} {
		for _, rate := range []int64{0, 1, 999999, 1000000, 1000001, math.MaxInt64} {
			want := new(big.Int).Mul(big.NewInt(units), big.NewInt(rate))
			want.Add(want, big.NewInt(999999))
			want.Div(want, big.NewInt(1000000))
			got, err := ceilRate(units, rate)
			if !want.IsInt64() {
				if !errors.Is(err, ErrUnavailable) {
					t.Fatalf("overflow admitted %d %d", units, rate)
				}
				continue
			}
			if err != nil || got != want.Int64() {
				t.Fatalf("%d * %d got=%d err=%v want=%s", units, rate, got, err, want)
			}
		}
	}
	m := fixture()
	m.Tariffs[0].RequestMicroUSD = new(int64(math.MaxInt64))
	c, err := New([]Manifest{m})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Quote(c.References()[0], "openai", "fixture-v1", "provider_default", 1, 1, 1); !errors.Is(err, ErrUnavailable) {
		t.Fatal("sum overflow", err)
	}
	m = fixture()
	m.Tariffs[0].RequestMicroUSD = new(int64(math.MaxInt64 / 2))
	c, _ = New([]Manifest{m})
	if _, err = c.Quote(c.References()[0], "openai", "fixture-v1", "provider_default", 1, 1, 3); !errors.Is(err, ErrUnavailable) {
		t.Fatal("attempt overflow", err)
	}
}

func TestMonetaryPricing_ConcurrentReuse(t *testing.T) {
	c, err := New([]Manifest{fixture()})
	if err != nil {
		t.Fatal(err)
	}
	r := c.References()[0]
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if got, err := c.Quote(r, "openai", "fixture-v1", "provider_default", 1, 1, 2); err != nil || got != 36 {
					t.Errorf("quote=%d %v", got, err)
				}
			}
		}()
	}
	wg.Wait()
}
