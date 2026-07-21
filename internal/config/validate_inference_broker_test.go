package config_test

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

// TestValidate_InferenceBrokers pins the boot validation of the
// inference-plane credential brokers (the D-300 analogue) and the
// brokered-XOR-local rule (D-333): required fields, https / loopback
// credential_url, unique names, and the primary provider's single-source
// invariant (remote requires a resolvable broker + rejects a local key;
// local rejects a broker).
func TestValidate_InferenceBrokers(t *testing.T) {
	validBroker := func() config.InferenceBrokerConfig {
		return config.InferenceBrokerConfig{
			Name:          "openai-broker",
			CredentialURL: "https://coordinator.example.test/provider-key",
			AuthTokenEnv:  "HARBOR_COORDINATOR_TOKEN",
		}
	}

	t.Run("valid brokered primary accepted", func(t *testing.T) {
		cfg := mustLoadValid(t)
		cfg.LLM.APIKey = ""
		cfg.LLM.CredentialSource = "remote"
		cfg.LLM.InferenceBroker = "openai-broker"
		cfg.LLM.InferenceBrokers = []config.InferenceBrokerConfig{validBroker()}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("valid brokered primary must pass, got %v", err)
		}
	})

	t.Run("missing credential_url rejected", func(t *testing.T) {
		cfg := mustLoadValid(t)
		b := validBroker()
		b.CredentialURL = ""
		cfg.LLM.InferenceBrokers = []config.InferenceBrokerConfig{b}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "credential_url") {
			t.Fatalf("want credential_url rejection, got %v", err)
		}
	})

	t.Run("plaintext non-loopback credential_url rejected", func(t *testing.T) {
		cfg := mustLoadValid(t)
		b := validBroker()
		b.CredentialURL = "http://coordinator.example.test/provider-key"
		cfg.LLM.InferenceBrokers = []config.InferenceBrokerConfig{b}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "https") {
			t.Fatalf("want TLS rejection, got %v", err)
		}
	})

	t.Run("loopback http credential_url accepted", func(t *testing.T) {
		cfg := mustLoadValid(t)
		b := validBroker()
		b.CredentialURL = "http://127.0.0.1:9099/provider-key"
		cfg.LLM.APIKey = ""
		cfg.LLM.CredentialSource = "remote"
		cfg.LLM.InferenceBroker = b.Name
		cfg.LLM.InferenceBrokers = []config.InferenceBrokerConfig{b}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("loopback http must pass (dev/fixture case), got %v", err)
		}
	})

	t.Run("missing auth_token_env rejected", func(t *testing.T) {
		cfg := mustLoadValid(t)
		b := validBroker()
		b.AuthTokenEnv = ""
		cfg.LLM.InferenceBrokers = []config.InferenceBrokerConfig{b}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "auth_token_env") {
			t.Fatalf("want auth_token_env rejection, got %v", err)
		}
	})

	t.Run("duplicate broker name rejected", func(t *testing.T) {
		cfg := mustLoadValid(t)
		cfg.LLM.InferenceBrokers = []config.InferenceBrokerConfig{validBroker(), validBroker()}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("want duplicate-name rejection, got %v", err)
		}
	})

	t.Run("remote source with unknown broker rejected", func(t *testing.T) {
		cfg := mustLoadValid(t)
		cfg.LLM.APIKey = ""
		cfg.LLM.CredentialSource = "remote"
		cfg.LLM.InferenceBroker = "does-not-exist"
		cfg.LLM.InferenceBrokers = []config.InferenceBrokerConfig{validBroker()}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "inference_broker") {
			t.Fatalf("want unknown-broker rejection, got %v", err)
		}
	})

	t.Run("remote source with missing broker name rejected", func(t *testing.T) {
		cfg := mustLoadValid(t)
		cfg.LLM.APIKey = ""
		cfg.LLM.CredentialSource = "remote"
		cfg.LLM.InferenceBroker = ""
		cfg.LLM.InferenceBrokers = []config.InferenceBrokerConfig{validBroker()}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "inference_broker") {
			t.Fatalf("want missing-broker rejection, got %v", err)
		}
	})

	t.Run("brokered-XOR-local both set rejected", func(t *testing.T) {
		cfg := mustLoadValid(t)
		cfg.LLM.APIKey = "env.SOME_KEY" // a local key AND a broker → both set
		cfg.LLM.CredentialSource = "remote"
		cfg.LLM.InferenceBroker = "openai-broker"
		cfg.LLM.InferenceBrokers = []config.InferenceBrokerConfig{validBroker()}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "api_key") {
			t.Fatalf("want both-set (brokered XOR local) rejection, got %v", err)
		}
	})

	t.Run("local source with a broker name rejected", func(t *testing.T) {
		cfg := mustLoadValid(t)
		cfg.LLM.CredentialSource = "" // local
		cfg.LLM.InferenceBroker = "openai-broker"
		cfg.LLM.InferenceBrokers = []config.InferenceBrokerConfig{validBroker()}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "inference_broker") {
			t.Fatalf("want local-with-broker rejection, got %v", err)
		}
	})

	t.Run("unknown credential_source rejected", func(t *testing.T) {
		cfg := mustLoadValid(t)
		cfg.LLM.CredentialSource = "carrier-pigeon"
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "credential_source") {
			t.Fatalf("want unknown-source rejection, got %v", err)
		}
	})
}
