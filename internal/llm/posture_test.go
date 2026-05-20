package llm

import (
	"context"
	"sync"
	"testing"
)

func TestPostureProvider_Posture_DefaultMockModeFalse(t *testing.T) {
	// The captured mock flag defaults to false (no dev escape hatch).
	resetMockModeCapturedForTesting()
	t.Cleanup(resetMockModeCapturedForTesting)

	p := NewPostureProvider(ConfigSnapshot{
		Driver:   "bifrost",
		Provider: "openai",
		Model:    "openai/gpt-5.3-chat",
	})
	snap, err := p.Posture(context.Background())
	if err != nil {
		t.Fatalf("Posture: %v", err)
	}
	if snap.MockMode {
		t.Error("MockMode = true, want false — RegisterMockModeCaptured was not called")
	}
	if snap.Provider != "openai" {
		t.Errorf("Provider = %q, want %q", snap.Provider, "openai")
	}
	if snap.Model != "openai/gpt-5.3-chat" {
		t.Errorf("Model = %q, want %q", snap.Model, "openai/gpt-5.3-chat")
	}
}

func TestPostureProvider_Posture_MockModeCaptured(t *testing.T) {
	resetMockModeCapturedForTesting()
	t.Cleanup(resetMockModeCapturedForTesting)

	// Simulate the dev escape hatch firing at boot (D-089).
	RegisterMockModeCaptured(true)

	p := NewPostureProvider(ConfigSnapshot{Driver: "mock"})
	snap, err := p.Posture(context.Background())
	if err != nil {
		t.Fatalf("Posture: %v", err)
	}
	if !snap.MockMode {
		t.Error("MockMode = false, want true — RegisterMockModeCaptured(true) fired")
	}
	// With no explicit Provider, the driver name is the fallback — so
	// the mock-driver boot reports Provider == "mock".
	if snap.Provider != "mock" {
		t.Errorf("Provider = %q, want %q (driver-name fallback)", snap.Provider, "mock")
	}
}

func TestPostureProvider_Posture_ProviderFallbackToDriver(t *testing.T) {
	resetMockModeCapturedForTesting()
	t.Cleanup(resetMockModeCapturedForTesting)

	// Empty Provider AND empty Driver → DefaultDriver ("bifrost").
	p := NewPostureProvider(ConfigSnapshot{})
	snap, err := p.Posture(context.Background())
	if err != nil {
		t.Fatalf("Posture: %v", err)
	}
	if snap.Provider != DefaultDriver {
		t.Errorf("Provider = %q, want %q (empty driver → DefaultDriver)", snap.Provider, DefaultDriver)
	}
}

func TestPostureProvider_Posture_RegionFromBaseURL(t *testing.T) {
	resetMockModeCapturedForTesting()
	t.Cleanup(resetMockModeCapturedForTesting)

	p := NewPostureProvider(ConfigSnapshot{
		Driver:   "bifrost",
		Provider: "custom-eu",
		Model:    "custom/model",
		BaseURL:  "https://eu.example.com/v1",
	})
	snap, err := p.Posture(context.Background())
	if err != nil {
		t.Fatalf("Posture: %v", err)
	}
	if snap.Region != "https://eu.example.com/v1" {
		t.Errorf("Region = %q, want the configured BaseURL", snap.Region)
	}

	// No BaseURL → empty Region (the Console renders an em-dash).
	p2 := NewPostureProvider(ConfigSnapshot{Driver: "bifrost", Provider: "openai", Model: "m"})
	snap2, err := p2.Posture(context.Background())
	if err != nil {
		t.Fatalf("Posture (no BaseURL): %v", err)
	}
	if snap2.Region != "" {
		t.Errorf("Region = %q, want empty when no BaseURL is configured", snap2.Region)
	}
}

func TestRegisterMockModeCaptured_RoundTrip(t *testing.T) {
	resetMockModeCapturedForTesting()
	t.Cleanup(resetMockModeCapturedForTesting)

	if mockModeCaptured.Load() {
		t.Fatal("mockModeCaptured starts true — reset failed")
	}
	RegisterMockModeCaptured(true)
	if !mockModeCaptured.Load() {
		t.Error("after RegisterMockModeCaptured(true): flag is false")
	}
	RegisterMockModeCaptured(false)
	if mockModeCaptured.Load() {
		t.Error("after RegisterMockModeCaptured(false): flag is true")
	}
}

// TestPostureProvider_Posture_ConcurrentReuse pins D-025: one
// PostureProvider shared across N≥100 goroutines sees no data races and
// every goroutine reads the same snapshot. Run with -race.
func TestPostureProvider_Posture_ConcurrentReuse(t *testing.T) {
	resetMockModeCapturedForTesting()
	t.Cleanup(resetMockModeCapturedForTesting)
	RegisterMockModeCaptured(true)

	p := NewPostureProvider(ConfigSnapshot{
		Driver:   "bifrost",
		Provider: "openai",
		Model:    "openai/gpt-5.3-chat",
	})

	const n = 128
	var wg sync.WaitGroup
	wg.Add(n)
	errCh := make(chan string, n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			snap, err := p.Posture(context.Background())
			if err != nil {
				errCh <- "Posture error: " + err.Error()
				return
			}
			if snap.Provider != "openai" || snap.Model != "openai/gpt-5.3-chat" || !snap.MockMode {
				errCh <- "unexpected snapshot under concurrency"
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for msg := range errCh {
		t.Error(msg)
	}
}
