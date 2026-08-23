package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestLLMProvidersStaticJSONIsTechnicalAndRedacted(t *testing.T) {
	root := NewRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"llm", "providers", "--provider", "openai", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v (stderr=%q)", err, stderr.String())
	}
	var output llmProvidersOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode output: %v (%s)", err, stdout.String())
	}
	if len(output.Descriptors) != 1 || output.Descriptors[0].ID != "openai" {
		t.Fatalf("unexpected descriptor output: %+v", output)
	}
	if strings.Contains(stdout.String(), "api_key_value") || !strings.Contains(stdout.String(), `"secret": true`) {
		t.Fatalf("credential field contract is not represented safely: %s", stdout.String())
	}
}
func TestLLMProvidersRequiresProviderForProbe(t *testing.T) {
	root := NewRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"llm", "providers", "--validate", "--json"})
	err := root.Execute()
	var cli CLIError
	if !errors.As(err, &cli) || cli.Code != codeLLMProviderInvalid {
		t.Fatalf("error=%v, want code %q", err, codeLLMProviderInvalid)
	}
	if !strings.Contains(stderr.String(), codeLLMProviderInvalid) || stdout.Len() != 0 {
		t.Fatalf("invalid probe was not structured: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestLLMProvidersProbeConfigFailureIsStable(t *testing.T) {
	root := NewRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"llm", "providers", "--validate", "--provider", "openai", "--config", t.TempDir() + "/missing.yaml", "--json"})
	err := root.Execute()
	var cli CLIError
	if !errors.As(err, &cli) || cli.Code != codeLLMProviderConfig {
		t.Fatalf("error=%v, want code %q", err, codeLLMProviderConfig)
	}
	if strings.Contains(stderr.String(), "password") || !strings.Contains(stderr.String(), codeLLMProviderConfig) {
		t.Fatalf("config failure was not stable/redacted: %q", stderr.String())
	}
}
