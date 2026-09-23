package config_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

func TestMemoryRetirement_RejectsSemanticIndexConfiguration(t *testing.T) {
	fixture, err := os.ReadFile(validMinimalFixture)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"retrieval: semantic", "retrieval: ''", "retrieval_top_k: 5", "retrieval_top_k: 0", "retrieval_min_score: 0.4", "retrieval_min_score: 0"} {
		t.Run(field, func(t *testing.T) {
			_, err := config.LoadFromBytes(t.Context(), []byte(string(fixture)+"\nmemory:\n  "+field+"\n"))
			if !errors.Is(err, config.ErrConfigInvalid) || !strings.Contains(err.Error(), strings.SplitN(field, ":", 2)[0]) {
				t.Fatalf("retired field must fail explicitly, including zero values: %v", err)
			}
		})
	}
	for _, key := range []string{"HARBOR_MEMORY_RETRIEVAL", "HARBOR_MEMORY_RETRIEVAL_TOP_K", "HARBOR_MEMORY_RETRIEVAL_MIN_SCORE"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "")
			if _, err := config.Load(t.Context(), validMinimalFixture); !errors.Is(err, config.ErrConfigInvalid) || !strings.Contains(err.Error(), key) {
				t.Fatalf("retired override must not be silently ignored: %v", err)
			}
		})
	}
}
