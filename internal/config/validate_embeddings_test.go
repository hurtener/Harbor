package config_test

// Phase 84d (D-191) — the `embeddings` block validator + the
// cross-block invariant: a semantic retrieval mode without a
// configured embeddings block fails loudly at validation, naming
// the missing key.

import (
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
)

func validEmbeddingsBlock() config.EmbeddingsConfig {
	return config.EmbeddingsConfig{
		Provider: "openai",
		Model:    "text-embedding-3-small",
		APIKey:   "env.OPENAI_API_KEY",
	}
}

func TestValidateEmbeddings_BlockShape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(*config.Config)
		wantErr string // "" = valid
	}{
		{"zero block, no semantic mode", func(c *config.Config) {}, ""},
		{"full block valid", func(c *config.Config) {
			c.Embeddings = validEmbeddingsBlock()
		}, ""},
		{"explicit driver valid", func(c *config.Config) {
			c.Embeddings = validEmbeddingsBlock()
			c.Embeddings.Driver = "bifrost"
		}, ""},
		{"unknown driver", func(c *config.Config) {
			c.Embeddings = validEmbeddingsBlock()
			c.Embeddings.Driver = "mock"
		}, "embeddings.driver"},
		{"missing provider", func(c *config.Config) {
			c.Embeddings = validEmbeddingsBlock()
			c.Embeddings.Provider = ""
		}, "embeddings.provider"},
		{"missing model", func(c *config.Config) {
			c.Embeddings = validEmbeddingsBlock()
			c.Embeddings.Model = ""
		}, "embeddings.model"},
		{"missing api_key", func(c *config.Config) {
			c.Embeddings = validEmbeddingsBlock()
			c.Embeddings.APIKey = ""
		}, "embeddings.api_key"},
		{"negative timeout", func(c *config.Config) {
			c.Embeddings = validEmbeddingsBlock()
			c.Embeddings.Timeout = -time.Second
		}, "embeddings.timeout"},
		{"negative dimensions", func(c *config.Config) {
			c.Embeddings = validEmbeddingsBlock()
			c.Embeddings.Dimensions = -1
		}, "embeddings.dimensions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := mustLoadValid(t)
			tc.mutate(cfg)
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate err = %v, want mention of %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateEmbeddings_SemanticModeRequiresBlock(t *testing.T) {
	t.Parallel()

	t.Run("memory semantic without block fails naming the key", func(t *testing.T) {
		t.Parallel()
		cfg := mustLoadValid(t)
		cfg.Memory.Retrieval = "semantic"
		err := cfg.Validate()
		if err == nil {
			t.Fatal("Validate accepted memory.retrieval=semantic with no embeddings block")
		}
		for _, want := range []string{"embeddings", "memory.retrieval", "examples/harbor.yaml"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q must mention %q", err.Error(), want)
			}
		}
	})

	t.Run("skills semantic without block fails", func(t *testing.T) {
		t.Parallel()
		cfg := mustLoadValid(t)
		cfg.Skills.Driver = "localdb"
		cfg.Skills.DSN = ":memory:"
		cfg.Skills.Retrieval = "semantic"
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "skills.retrieval") {
			t.Fatalf("err=%v, want the skills.retrieval-named embeddings requirement", err)
		}
	})

	t.Run("semantic with block passes", func(t *testing.T) {
		t.Parallel()
		cfg := mustLoadValid(t)
		cfg.Memory.Retrieval = "semantic"
		cfg.Memory.RetrievalTopK = 8
		cfg.Embeddings = validEmbeddingsBlock()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("unknown retrieval values rejected", func(t *testing.T) {
		t.Parallel()
		cfg := mustLoadValid(t)
		cfg.Memory.Retrieval = "vibes"
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "memory.retrieval") {
			t.Fatalf("err=%v, want memory.retrieval rejection", err)
		}
		cfg = mustLoadValid(t)
		cfg.Skills.Driver = "localdb"
		cfg.Skills.DSN = ":memory:"
		cfg.Skills.Retrieval = "vibes"
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "skills.retrieval") {
			t.Fatalf("err=%v, want skills.retrieval rejection", err)
		}
	})

	t.Run("skills retrieval without store fields rejected", func(t *testing.T) {
		t.Parallel()
		cfg := mustLoadValid(t)
		cfg.Skills = config.SkillsConfig{Retrieval: "semantic"}
		cfg.Embeddings = validEmbeddingsBlock()
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "skills.driver") {
			t.Fatalf("err=%v, want skills.driver-required rejection", err)
		}
	})

	t.Run("negative retrieval_top_k rejected", func(t *testing.T) {
		t.Parallel()
		cfg := mustLoadValid(t)
		cfg.Memory.RetrievalTopK = -1
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "memory.retrieval_top_k") {
			t.Fatalf("err=%v, want memory.retrieval_top_k rejection", err)
		}
	})
}
