package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/drivers/bifrost"
	"github.com/hurtener/Harbor/internal/llm/provider"
)

const (
	codeLLMProviderInvalid  = "llm_provider_invalid"
	codeLLMProviderConfig   = "llm_provider_config_invalid"
	codeLLMProviderInternal = "llm_provider_internal_error"
)

type llmProviderFlags struct {
	config   string
	provider string
	discover bool
	validate bool
	timeout  time.Duration
	pageSize int
	maxPages int
}

// newLLMCmd builds the provider-neutral LLM inspection surface. It is a
// read-only operator query: it describes technical capabilities and, when
// explicitly requested, asks the configured runtime-origin Bifrost account to
// validate or discover models. It never prints credentials or raw provider
// responses.
func newLLMCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "llm",
		Short: "inspect LLM provider capabilities",
	}
	cmd.AddCommand(newLLMProvidersCmd())
	return cmd
}

func newLLMProvidersCmd() *cobra.Command {
	flags := llmProviderFlags{}
	cmd := &cobra.Command{
		Use:   "providers",
		Short: "list provider descriptors and optionally probe the configured provider",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLLMProviders(cmd, flags)
		},
	}
	cmd.Flags().StringVar(&flags.config, "config", DefaultDevConfig, "path to harbor.yaml (required for --discover or --validate)")
	cmd.Flags().StringVar(&flags.provider, "provider", "", "show one provider id")
	cmd.Flags().BoolVar(&flags.discover, "discover", false, "discover normalized models from the configured runtime provider")
	cmd.Flags().BoolVar(&flags.validate, "validate", false, "validate the configured runtime provider endpoint and credential")
	cmd.Flags().DurationVar(&flags.timeout, "timeout", 30*time.Second, "maximum runtime-origin probe duration")
	cmd.Flags().IntVar(&flags.pageSize, "page-size", 100, "maximum models requested per discovery page")
	cmd.Flags().IntVar(&flags.maxPages, "max-pages", 20, "maximum discovery pages")
	return cmd
}

type llmProvidersOutput struct {
	// RuntimeOrigin is false because this command runs outside a booted
	// Harbor process. Its optional probes use the local CLI adapter and must
	// never be presented as evidence about a remote runtime.
	RuntimeOrigin bool                          `json:"runtime_origin"`
	Descriptors   []provider.ProviderDescriptor `json:"descriptors"`
	Validation    *provider.ValidationResult    `json:"validation,omitempty"`
	Discovery     *provider.DiscoveryResult     `json:"discovery,omitempty"`
}

func runLLMProviders(cmd *cobra.Command, flags llmProviderFlags) error {
	if flags.discover && flags.validate {
		return emitCLIError(cmd, CLIError{Subcommand: "llm providers", Message: "--discover and --validate are mutually exclusive", Code: codeLLMProviderInvalid})
	}
	if flags.timeout <= 0 || flags.pageSize <= 0 || flags.maxPages <= 0 {
		return emitCLIError(cmd, CLIError{Subcommand: "llm providers", Message: "--timeout, --page-size, and --max-pages must be positive", Code: codeLLMProviderInvalid})
	}
	providerID := strings.TrimSpace(flags.provider)
	if !flags.discover && !flags.validate {
		output := llmProvidersOutput{Descriptors: offlineProviderDescriptors(bifrost.StaticProviderDescriptors(nil))}
		if providerID != "" {
			output.Descriptors = filterProviderDescriptors(output.Descriptors, providerID)
			if len(output.Descriptors) == 0 {
				return emitCLIError(cmd, CLIError{Subcommand: "llm providers", Message: "provider is not in the Harbor provider registry", Code: codeLLMProviderInvalid})
			}
		}
		return writeLLMProvidersOutput(cmd.OutOrStdout(), resolveJSONMode(cmd), output)
	}
	if providerID == "" {
		return emitCLIError(cmd, CLIError{Subcommand: "llm providers", Message: "--provider is required for --discover or --validate", Code: codeLLMProviderInvalid})
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), flags.timeout)
	defer cancel()
	cfg, err := config.Load(ctx, flags.config)
	if err != nil {
		return emitCLIError(cmd, CLIError{Subcommand: "llm providers", Message: "provider config could not be loaded", Code: codeLLMProviderConfig, Hint: "pass --config to a validated Harbor configuration"})
	}
	snapshot := llm.SnapshotFromConfig(cfg.LLM, cfg.Artifacts)
	catalog, err := bifrost.NewProviderCatalog(snapshot)
	if err != nil {
		return emitCLIError(cmd, CLIError{Subcommand: "llm providers", Message: "runtime provider catalog could not be initialized", Code: codeLLMProviderInternal})
	}
	defer func() { _ = catalog.Close(context.Background()) }()
	output := llmProvidersOutput{Descriptors: offlineProviderDescriptors(filterProviderDescriptors(catalog.Descriptors(ctx), providerID))}
	if len(output.Descriptors) == 0 {
		return emitCLIError(cmd, CLIError{Subcommand: "llm providers", Message: "provider is not configured or not in the Harbor provider registry", Code: codeLLMProviderInvalid})
	}
	if flags.validate {
		result := catalog.Validate(ctx, provider.ValidationRequest{ProviderID: providerID})
		result.Outcome.RuntimeOrigin = false
		output.Validation = &result
	} else {
		result, discoverErr := catalog.Discover(ctx, provider.DiscoveryRequest{ProviderID: providerID, PageSize: flags.pageSize, MaxPages: flags.maxPages})
		if discoverErr != nil {
			return emitCLIError(cmd, CLIError{Subcommand: "llm providers", Message: "provider discovery request was invalid", Code: codeLLMProviderInternal})
		}
		result.Outcome.RuntimeOrigin = false
		output.Discovery = &result
	}
	return writeLLMProvidersOutput(cmd.OutOrStdout(), resolveJSONMode(cmd), output)
}

func offlineProviderDescriptors(in []provider.ProviderDescriptor) []provider.ProviderDescriptor {
	out := append([]provider.ProviderDescriptor(nil), in...)
	for i := range out {
		out[i].Validation.RuntimeOrigin = false
		out[i].Discovery.RuntimeOrigin = false
	}
	return out
}

func filterProviderDescriptors(descriptors []provider.ProviderDescriptor, providerID string) []provider.ProviderDescriptor {
	if providerID == "" {
		return descriptors
	}
	out := make([]provider.ProviderDescriptor, 0, 1)
	for _, descriptor := range descriptors {
		if descriptor.ID == providerID {
			out = append(out, descriptor)
		}
	}
	return out
}

func writeLLMProvidersOutput(w io.Writer, jsonMode bool, output llmProvidersOutput) error {
	if jsonMode {
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(output)
	}
	write := func(format string, args ...any) error {
		_, err := fmt.Fprintf(w, format, args...)
		return err
	}
	if err := write("Providers: %d\n", len(output.Descriptors)); err != nil {
		return err
	}
	// This command is deliberately offline: make the provenance visible in
	// the human output as an explicit stable fact, not a formatter-dependent
	// implication.
	if err := write("runtime_origin=false (offline CLI; not a booted runtime)\n"); err != nil {
		return err
	}
	for _, descriptor := range output.Descriptors {
		if err := write("%s (%s) validation=%s discovery=%s custom_endpoint=%s\n", descriptor.ID, descriptor.Kind, descriptor.Validation.State, descriptor.Discovery.State, descriptor.CustomEndpoint); err != nil {
			return err
		}
	}
	if output.Validation != nil {
		if err := write("Validation %s: %s (%s)\n", output.Validation.ProviderID, output.Validation.Outcome.State, output.Validation.Outcome.Code); err != nil {
			return err
		}
	}
	if output.Discovery != nil {
		if err := write("Discovery %s: %s (%s), models=%d, pages=%d\n", output.Discovery.ProviderID, output.Discovery.Outcome.State, output.Discovery.Outcome.Code, output.Discovery.ModelCount, output.Discovery.Pages); err != nil {
			return err
		}
	}
	return nil
}
