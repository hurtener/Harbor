// Package config is the public SDK facade over Harbor's
// internal/config package — the operator configuration schema,
// loader, defaults, and validation surface (RFC §3.6, §10).
// Alias-based re-exports only: no behavior lives here. What this
// package omits from the internal surface (deprecation plumbing,
// policy projection internals) is deliberately private.
package config

import (
	internal "github.com/hurtener/Harbor/internal/config"
)

// Config is the root operator configuration. Methods (ValidateCore,
// Validate, ...) come with the alias; see internal/config.
type Config = internal.Config

// LoadOption customises Load / LoadFromBytes.
type LoadOption = internal.LoadOption

// Section types — the named blocks of Config, re-exported so an
// embedder can hand-build a configuration without YAML.
type (
	// A2APeerConfig declares one A2A remote peer.
	A2APeerConfig = internal.A2APeerConfig
	// ArtifactsConfig configures the artifact store.
	ArtifactsConfig = internal.ArtifactsConfig
	// AuditConfig configures audit redaction.
	AuditConfig = internal.AuditConfig
	// CLIConfig configures CLI-side behaviour.
	CLIConfig = internal.CLIConfig
	// CustomToolConfig declares one operator-defined tool entry body.
	CustomToolConfig = internal.CustomToolConfig
	// DevHotReloadConfig configures the dev-loop hot-reload policy.
	DevHotReloadConfig = internal.DevHotReloadConfig
	// DistributedConfig configures the MessageBus / RemoteTransport.
	DistributedConfig = internal.DistributedConfig
	// EventsConfig configures the typed event bus.
	EventsConfig = internal.EventsConfig
	// GovernanceConfig configures cost ceilings / rate limits / MaxTokens.
	GovernanceConfig = internal.GovernanceConfig
	// GovernanceRateLimitConfig is one tier's rate-limit block.
	GovernanceRateLimitConfig = internal.GovernanceRateLimitConfig
	// GovernanceTierConfig is one identity tier's enforcement block.
	GovernanceTierConfig = internal.GovernanceTierConfig
	// IdentityConfig configures identity handling.
	IdentityConfig = internal.IdentityConfig
	// LLMConfig configures the LLM client.
	LLMConfig = internal.LLMConfig
	// LLMExternalGrantConfig configures signed external-grant verification and
	// the accepted provider-route mode.
	LLMExternalGrantConfig = internal.LLMExternalGrantConfig
	// LLMProviderRouteConfig configures the optional external provider-route resolver.
	LLMProviderRouteConfig = internal.LLMProviderRouteConfig
	// ExternalGrantCoordinatorConfig configures the stock authenticated
	// receipt transport. Stock lease top-up is intentionally unsupported.
	ExternalGrantCoordinatorConfig = internal.ExternalGrantCoordinatorConfig
	// LLMCorrectionsConfig configures the provider-correction layer.
	LLMCorrectionsConfig = internal.LLMCorrectionsConfig
	// LLMCorrectionsProfileConfig is a per-model corrections override.
	LLMCorrectionsProfileConfig = internal.LLMCorrectionsProfileConfig
	// LLMCostOverridesConfig overrides per-1M token cost rates.
	LLMCostOverridesConfig = internal.LLMCostOverridesConfig
	// LLMCustomProviderConfig declares one OpenAI-compatible endpoint.
	LLMCustomProviderConfig = internal.LLMCustomProviderConfig
	// LLMModelProfileConfig declares one model's context window / knobs.
	LLMModelProfileConfig = internal.LLMModelProfileConfig
	// LLMNetworkDefaults carries provider network fallback knobs.
	LLMNetworkDefaults = internal.LLMNetworkDefaults
	// MCPServerConfig declares one southbound MCP server attachment.
	MCPServerConfig = internal.MCPServerConfig
	// MemoryConfig configures the memory subsystem.
	MemoryConfig = internal.MemoryConfig
	// PauseResumeConfig configures the unified pause/resume primitive.
	PauseResumeConfig = internal.PauseResumeConfig
	// PlannerConfig configures the planner concrete + caps.
	PlannerConfig = internal.PlannerConfig
	// PlannerPlanningHintsCfg configures operator planning hints.
	PlannerPlanningHintsCfg = internal.PlannerPlanningHintsCfg
	// ProjectedToolPolicy is the config→runtime tool-policy projection.
	ProjectedToolPolicy = internal.ProjectedToolPolicy
	// ProtocolConfig configures the Harbor Protocol surface.
	ProtocolConfig = internal.ProtocolConfig
	// RuntimeConfig configures the orchestration kernel.
	RuntimeConfig = internal.RuntimeConfig
	// ServerConfig configures the Protocol server's listener.
	ServerConfig = internal.ServerConfig
	// SessionsConfig configures the session manager.
	SessionsConfig = internal.SessionsConfig
	// SkillsConfig configures the skills subsystem.
	SkillsConfig = internal.SkillsConfig
	// SkillsDirectoryConfig configures the skills Directory view.
	SkillsDirectoryConfig = internal.SkillsDirectoryConfig
	// StateConfig configures the StateStore.
	StateConfig = internal.StateConfig
	// TasksConfig configures the TaskService.
	TasksConfig = internal.TasksConfig
	// TelemetryConfig configures logging / traces / metrics.
	TelemetryConfig = internal.TelemetryConfig
	// ToolApprovalConfig configures HITL tool-approval gates.
	ToolApprovalConfig = internal.ToolApprovalConfig
	// ToolEntryConfig declares one operator tool entry.
	ToolEntryConfig = internal.ToolEntryConfig
	// ToolOAuthConfig configures tool-side OAuth.
	ToolOAuthConfig = internal.ToolOAuthConfig
	// ToolOAuthProviderConfig declares one OAuth provider.
	ToolOAuthProviderConfig = internal.ToolOAuthProviderConfig
	// ToolPolicyConfig declares one tool's timeout/retry/validation policy.
	ToolPolicyConfig = internal.ToolPolicyConfig
	// ToolsConfig configures the tool catalog.
	ToolsConfig = internal.ToolsConfig
)

// Re-exported sentinel errors callers compare via errors.Is.
var (
	// ErrConfigInvalid — the configuration failed validation.
	ErrConfigInvalid = internal.ErrConfigInvalid
	// ErrConfigNotFound — the configuration file does not exist.
	ErrConfigNotFound = internal.ErrConfigNotFound
)

// Defaults returns the canonical baseline configuration — the same
// baseline Load applies to a harbor.yaml, so a hand-built config
// behaves exactly like a loaded one.
var Defaults = internal.Defaults

// Load reads, defaults, and validates a harbor.yaml from disk.
var Load = internal.Load

// LoadFromBytes reads, defaults, and validates a config from memory.
var LoadFromBytes = internal.LoadFromBytes

// WithOverrides applies dotted-key overrides onto a Config.
var WithOverrides = internal.WithOverrides

// WithLogger threads a logger into Load / LoadFromBytes.
var WithLogger = internal.WithLogger

// IsValidationError reports whether err is a config validation error.
var IsValidationError = internal.IsValidationError
