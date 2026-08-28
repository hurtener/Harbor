package serve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// mcp_reattacher.go — the RUN-START ATTACH leg: the production concrete that
// re-establishes a runtime-added MCP connection the reconciling owner's active
// config revision DECLARES but the live registry does not carry. It is the
// symmetric twin of mcp_detacher.go and satisfies the driver-agnostic
// projection.ConnectionReattacher seam.
//
// It hangs off *MCPConnectionAttacher deliberately: the leg MUST drive the SAME
// attach lifecycle the admin add verb drives (one implementation, never two),
// and it MUST share the attacher's whole-attach lock and closer chain so a
// re-attached transport drains on teardown and cannot race a concurrent add of
// the same name.
//
// # What it re-applies, and against WHICH policy
//
// Every gate the admin add door applies is re-applied here against CURRENT boot
// policy, not the policy in force when the revision was written:
//
//   - the fail-closed stdio command allowlist (an RCE gate — a command since
//     removed from the allowlist is never spawned);
//   - the fail-closed per-user credential-injection opt-in, as a KILL-SWITCH
//     (the twin of the provider side's wire-descriptor kill-switch);
//   - and, inside the shared attach lifecycle, the `oauth_provider` NAME
//     resolution, the provider's boot-declared downstream-sink allow-list, the
//     reserved-`_meta` annotation rules, the separator-ambiguity check, and the
//     same-owner-only same-name replace.
//
// # What it does NOT touch: the credential plane
//
// The attach path has no token step. An `oauth_provider` binding is a NAME
// resolved against the boot-declared provider set; the bearer is minted per
// outbound CALL, one layer later. So this leg initiates no consent flow and
// mints, holds, refreshes, and exchanges nothing — for either provider family
// (the interactive one that persists a sealed bearer, and the brokered one that
// persists nothing). A connection whose consent is genuinely gone therefore
// still re-attaches; the shortfall surfaces on the FIRST TOOL CALL as the
// shipped typed auth-required error routed onto the unified pause/resume
// primitive. This leg neither duplicates nor triggers that path, and it never
// parks a run: a run-start reconcile has no admin request to resume and no
// consenting principal, so an auth shortfall at attach is a terminal,
// backoff-eligible failure, never a second pause shape (CLAUDE.md §13).
//
// # The one genuinely non-re-establishable shape
//
// A connection whose live transport depended on operator-supplied static auth
// HEADERS is not restart-survivable: the headers are secret and never persisted,
// and persisting them would put a credential on a Protocol-readable, diffable,
// rollback-able spine. The re-attach dials without them; a server that required
// them answers 401 and the outcome is REPORTED as a transport failure. The
// honest claim is therefore "a declared connection whose descriptor is
// self-sufficient is re-established at run start" — never the unscoped
// "connections survive restarts".

// ErrReattachSuppressed — this (owner, name)'s re-attach was SKIPPED because its
// bounded retry window has not elapsed (a retryable failure still backing off),
// or because its last failure was in a class that cannot heal by re-dialling and
// the descriptor has not changed since. It is not a new failure: the attempt was
// counted and the count rides the next emitted lifecycle event, so the
// suppression is bounded and reported rather than silent (CLAUDE.md §13). The
// run-loop caller compares with errors.Is to log it at Debug instead of Error.
var ErrReattachSuppressed = errors.New("serve: run-start mcp re-attach suppressed by its bounded retry window")

// reattachOutcome is what the locked core decides, so the bus publish and the
// caller-visible error happen OUTSIDE the attacher's whole-attach lock (a
// Publish under the lock would let a slow subscriber stall every other owner's
// re-attach).
type reattachOutcome struct {
	// changed reports that this call published a missing or changed descriptor.
	// False is the exact owner+fingerprint no-op.
	changed bool
	// emit reports whether a lifecycle event should be published.
	emit bool
	// eventType is the canonical event to publish when emit is set.
	eventType events.EventType
	// state is the payload's State field: "online" on success, the stable
	// failure CLASS otherwise.
	state string
	// reason is the scrubbed, bounded, operator-facing failure reason (empty on
	// success), with the suppressed-attempt count appended when non-zero.
	reason string
	// err is the error returned to the reconcile caller (nil on success or on an
	// already-live no-op).
	err error
	// suppressedNote carries the Debug-level detail for a suppressed attempt.
	suppressedNote string
}

// Reattach re-establishes one DECLARED-but-ABSENT runtime-added MCP connection
// under owner, driving the SAME attach lifecycle the admin add verb drives. It
// satisfies projection.ConnectionReattacher. See the file doc for the gate,
// credential-plane, and reporting contract.
//
// Idempotent: a name already registered under owner is a no-op (nil), decided by
// a re-read of the live registry INSIDE the whole-attach lock, which closes the
// stale-view window between the reconcile's AttachedSources read and this call.
// A name registered to a DIFFERENT owner is refused loud and classified
// non-retryable — never renamed, never shadowed, never torn down.
//
// Bounded: the dial + handshake + discovery run under their own timeout, because
// the MCP provider's Connect has no internal one and the reconcile is
// synchronous at run start.
//
// Never fatal: the returned error is for the caller's loud log and its joined
// sweep error. The caller does not fail the run on it.
func (a *MCPConnectionAttacher) Reattach(ctx context.Context, owner toolauth.Owner, id identity.Quadruple, desc agentcfg.MCPConnectionDescriptor) (bool, error) {
	// Fail CLOSED on an incomplete owner, exactly like the add path: a
	// runtime-added registration with no owner tag would be an unreconcilable
	// orphan. The reconcile derives the owner from the run's verified tenant plus
	// the resolved agent, so this cannot fire from the shipped call site; it is
	// the guard that keeps that true for any future caller.
	if owner.Tenant == "" || owner.Agent == "" {
		return false, fmt.Errorf("%w: run-start re-attach of connection %q requires a (tenant, agent) owner (tenant=%q agent=%q)",
			ErrRuntimeAddOwnerMissing, desc.Name, owner.Tenant, owner.Agent)
	}
	if a.registry == nil || a.catalog == nil {
		return false, fmt.Errorf("serve: mcp attacher has no catalog/registry wired for the run-start re-attach of %q", desc.Name)
	}

	key := reattachKey{tenant: owner.Tenant, agent: owner.Agent, user: owner.User, name: desc.Name}
	fingerprint := agentcfg.MCPConnectionFingerprint(desc)

	// The gate pass is PURE and runs before the lock and before any dial: a
	// refused descriptor never reaches a transport.
	gateErr := a.gateReattach(desc)

	var out reattachOutcome
	func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		out = a.reattachLocked(ctx, owner, desc, key, fingerprint, gateErr)
	}()

	if out.suppressedNote != "" && a.logger != nil {
		a.logger.DebugContext(ctx, "mcp: run-start re-attach suppressed by its retry window",
			slog.String("server_id", desc.Name),
			slog.String("tenant_id", owner.Tenant),
			slog.String("agent_id", owner.Agent),
			slog.String("detail", out.suppressedNote))
	}
	if out.emit {
		a.emitReattachLifecycle(ctx, id, owner, desc, out)
	}
	return out.changed, out.err
}

// gateReattach re-applies the CURRENT boot policy gates to a declared
// descriptor. Pure: it reads only construction-time state and the descriptor, so
// it is safe to call before taking the attach lock and it never dials.
func (a *MCPConnectionAttacher) gateReattach(desc agentcfg.MCPConnectionDescriptor) error {
	// The per-user credential-injection KILL-SWITCH. A mapping persisted while
	// the fail-closed opt-in was ON is NOT rebuilt after a restart with it off —
	// the twin of the provider side's wire-descriptor kill-switch.
	if desc.Injection != nil && !a.allowWireInjection {
		return fmt.Errorf("%w (connection=%q, broker=%q)", ErrReattachInjectionDisabled, desc.Name, desc.Injection.Provider)
	}
	// The stdio RCE gate, re-applied against the allowlist in force NOW. An empty
	// (or unthreaded) allowlist refuses every stdio re-attach — fail-closed.
	if desc.Transport == agentcfg.MCPTransportStdio {
		if len(desc.Command) == 0 || desc.Command[0] == "" {
			return fmt.Errorf("%w: declared stdio connection %q has no argv command", agentcfgprotocol.ErrInvalidConnection, desc.Name)
		}
		if _, ok := a.stdioAllowlist[desc.Command[0]]; !ok {
			return fmt.Errorf("%w (connection=%q, command=%q)", ErrReattachStdioNotAllowed, desc.Name, desc.Command[0])
		}
	}
	return nil
}

// reattachLocked is the whole-attach-lock body: the retry-window check, the
// under-lock live-registry re-check, the bounded attach, and the backoff update.
// The caller holds a.mu; every read and write of a.reattachBackoff and
// a.closers happens here.
func (a *MCPConnectionAttacher) reattachLocked(ctx context.Context, owner toolauth.Owner, desc agentcfg.MCPConnectionDescriptor, key reattachKey, fingerprint string, gateErr error) reattachOutcome {
	st := a.reattachBackoff[key]
	if st != nil && st.fingerprint != fingerprint {
		// The operator EDITED the descriptor: retry immediately rather than
		// serving out a window opened against the old shape.
		delete(a.reattachBackoff, key)
		st = nil
	}
	if st != nil && (st.terminal || a.now().Before(st.nextAttempt)) {
		st.suppressed++
		detail := fmt.Sprintf("terminal=%t failures=%d suppressed_attempts=%d", st.terminal, st.failures, st.suppressed)
		return reattachOutcome{
			err:            fmt.Errorf("%w: connection %q (%s)", ErrReattachSuppressed, desc.Name, detail),
			suppressedNote: detail,
		}
	}

	if gateErr != nil {
		return a.recordReattachFailureLocked(key, fingerprint, desc, gateErr)
	}

	// The under-lock idempotency re-check (the stale-view window closer). The
	// reconcile's AttachedSources read happened before this call; a concurrent run
	// start for the SAME owner may have attached the name in between. Re-reading
	// here — inside the lock that serialises the whole attach — makes the pair
	// (check, attach) atomic, so two concurrent run starts dial exactly once.
	if priorOwner, liveFingerprint, exists := a.registry.RegistrationIdentityForOwner(desc.Name, owner); exists {
		if priorOwner == owner {
			if liveFingerprint == fingerprint {
				// Exact canonical descriptor already live: no transport churn.
				delete(a.reattachBackoff, key)
				return reattachOutcome{}
			}
			// Same owner/name with a different descriptor is a replacement. The
			// prepared attach keeps the healthy old catalog/provider callable until
			// the catalog publication linearization point.
		}
		if priorOwner != owner {
			// A DIFFERENT owner holds this name. Refuse loud with the shipped
			// sentinel, exactly as the add door does: never evict another owner's
			// live tools and transport, never rename, never shadow.
			return a.recordReattachFailureLocked(key, fingerprint, desc,
				fmt.Errorf("%w: a connection named %q is already registered to a different owner", mcpdrv.ErrConnectionNameOwnerConflict, desc.Name))
		}
	}

	ms := reattachServerConfig(desc)

	// The BOUNDED per-connection context. Load-bearing: the MCP provider's
	// Connect carries no internal timeout (it inherits this ctx) and the
	// reconcile is synchronous at run start, so an unresponsive declared server
	// would otherwise stall every run's start until run cancellation.
	actx, cancel := context.WithTimeout(ctx, a.reattachTimeout)
	defer cancel()

	var local []func(context.Context) error
	deps := a.reattachAttachDeps(owner, &local)
	deps.DescriptorFingerprint = fingerprint
	err := mcpdrv.Attach(actx, ms, deps)
	// Merge whatever closers Attach appended even on failure (a successful
	// Connect appends the provider's Close before a later step can fail — drain
	// it on teardown).
	a.closers = append(a.closers, local...)
	if err != nil {
		return a.recordReattachFailureLocked(key, fingerprint, desc, err)
	}
	delete(a.reattachBackoff, key)
	return reattachOutcome{
		changed:   true,
		emit:      true,
		eventType: agentcfg.EventTypeMCPConnectionReattached,
		state:     string(agentcfgprotocol.ConnectionStateOnline),
	}
}

// reattachAttachDeps assembles the driver dependencies one run-start re-attach
// runs under. Every field is the SAME construction-time collaborator the admin
// add door passes, so the two doors drive one attach lifecycle rather than two.
//
// It is a named method for the same reason reattachServerConfig is a named
// function: a dependency that is silently absent here is not visible in any
// attach outcome — the connection still comes back `online`, just without the
// bound or the capturer the deployment configured.
func (a *MCPConnectionAttacher) reattachAttachDeps(owner toolauth.Owner, closers *[]func(context.Context) error) mcpdrv.AttachDeps {
	return mcpdrv.AttachDeps{
		Catalog:          a.catalog,
		Registry:         a.registry,
		Bus:              a.bus,
		Logger:           a.logger,
		DefaultIdentity:  a.defaultIdentity,
		Closers:          closers,
		OAuthProviders:   a.oauthProviders,
		OAuthProviderSet: a.oauthProviderSet,
		ToolContext:      a.toolContext,
		ArtifactStore:    a.artifactStore,
		Owner:            owner,
		// The deployment's egress ceiling, threaded exactly as the add path
		// threads it. Omitting it silently restores the default ceiling across
		// a restart for a deployment that deliberately lowered the bound. Zero
		// leaves mcpdrv.Attach on its documented default — a real ceiling,
		// never an unbounded one.
		ArtifactEgressMaxBytes: a.artifactEgressMaxBytes,
	}
}

// reattachServerConfig rebuilds the driver-level server config from ONE
// declared descriptor. It is the run-start twin of the value the admin add
// door builds, and it exists as a named function so the carry can be asserted
// field-by-field rather than only observed through a live attach.
//
// EVERY declared descriptor field is carried, or is deliberately absent with
// its reason stated here. A field on the descriptor that nothing carries
// forward is INERT: the connection comes back `online`, emits its lifecycle
// event, and is silently missing the surface a sibling change added to it. The
// descriptor's reflected field set is pinned against this list by test, so a
// NEW field cannot land without being carried here or being named deliberate.
//
//	Name                         → carried
//	Transport                    → carried (as TransportMode)
//	Command                      → carried (copied)
//	URL                          → carried
//	OAuthProvider                → carried (a NAME; the bearer is minted per call)
//	MetaAnnotations              → carried (cloned)
//	OAuthDiscoveryAllowedOrigins → carried (copied)
//	Injection                    → carried (kill-switch-gated by the caller)
//	ArtifactByteEligible         → carried
//	ArtifactParams               → carried (deep-copied)
//
// `Headers` is NOT a descriptor field at all, and that is the one deliberate
// absence: operator-supplied static auth headers are secret and never
// persisted, so there is nothing to re-supply. A server that required one
// answers 401 and the outcome is reported as a transport failure — never a
// runtime that believes a header is present when it is not.
func reattachServerConfig(desc agentcfg.MCPConnectionDescriptor) config.MCPServerConfig {
	return config.MCPServerConfig{
		Name:                         desc.Name,
		TransportMode:                transportModeForAdd(desc.Transport),
		URL:                          desc.URL,
		Command:                      append([]string(nil), desc.Command...),
		OAuthProvider:                desc.OAuthProvider,
		MetaAnnotations:              cloneAnnotationsForReattach(desc.MetaAnnotations),
		OAuthDiscoveryAllowedOrigins: append([]string(nil), desc.OAuthDiscoveryAllowedOrigins...),
		Injection:                    injectionToConfig(desc.Injection),
		// The egress-substitution declaration. Without it a byte-eligible
		// connection comes back online and byte-INELIGIBLE, so the artifact id
		// the model authored is handed to the remote server as a literal
		// string on every call after a restart — silent degradation of a
		// declared, versioned surface. NON-SECRET (a boolean plus parameter
		// names); mcpdrv.Attach re-enforces the eligibility and transport
		// rules and Discover re-checks each mapped parameter against the
		// server's own published inputSchema, so the re-attach re-applies
		// these gates against CURRENT policy exactly like every other one.
		ArtifactByteEligible: desc.ArtifactByteEligible,
		ArtifactParams:       config.MCPArtifactParams(cloneArtifactParams(desc.ArtifactParams)),
	}
}

// recordReattachFailureLocked classifies a failure, opens or advances this
// (owner, name)'s retry window, and returns the outcome that reports it. The
// caller holds a.mu.
//
// Emission is bounded but never silent: this function is only reached when the
// window allowed an attempt, so every call EMITS — and the count of attempts
// suppressed since the last emission rides the reason, so an operator reading
// one event knows how many run starts it stands for.
func (a *MCPConnectionAttacher) recordReattachFailureLocked(key reattachKey, fingerprint string, desc agentcfg.MCPConnectionDescriptor, err error) reattachOutcome {
	class, terminal := classifyReattachFailure(err)
	st := a.reattachBackoff[key]
	suppressed := 0
	if st == nil {
		st = &reattachState{fingerprint: fingerprint}
		a.reattachBackoff[key] = st
	} else {
		suppressed = st.suppressed
	}
	st.fingerprint = fingerprint
	st.failures++
	st.terminal = terminal
	st.suppressed = 0
	if terminal {
		// Cannot heal by re-dialling: only an operator action (the other owner's
		// detach, a rename, a boot-policy change plus restart, or an edit to this
		// descriptor) clears it. Report once per attempted descriptor.
		st.nextAttempt = time.Time{}
	} else {
		st.nextAttempt = a.now().Add(reattachBackoffDelay(st.failures))
	}

	reason := agentcfgprotocol.SafeReason(err)
	if suppressed > 0 {
		reason = fmt.Sprintf("%s (suppressed_attempts=%d)", reason, suppressed)
	}
	return reattachOutcome{
		emit:      true,
		eventType: agentcfg.EventTypeMCPConnectionReattachFailed,
		state:     class,
		reason:    reason,
		err:       fmt.Errorf("run-start re-attach of %q refused or unreachable (class=%s): %w", desc.Name, class, err),
	}
}

// classifyReattachFailure maps an attach failure onto its stable, non-secret
// class and reports whether the class can heal by re-dialling. The classes are
// the CLOSED set carried on the failure event; a class no operator surface can
// distinguish is the same as a silent failure.
func classifyReattachFailure(err error) (class string, terminal bool) {
	switch {
	case errors.Is(err, ErrReattachStdioNotAllowed):
		return agentcfg.MCPReattachClassStdioNotAllowed, true
	case errors.Is(err, ErrReattachInjectionDisabled):
		return agentcfg.MCPReattachClassInjectionDisabled, true
	case errors.Is(err, mcpdrv.ErrConnectionNameOwnerConflict):
		return agentcfg.MCPReattachClassOwnerConflict, true
	case errors.Is(err, mcpdrv.ErrAmbiguousServerID):
		return agentcfg.MCPReattachClassAmbiguousServerID, true
	case errors.Is(err, mcpdrv.ErrOAuthBinding), errors.Is(err, agentcfgprotocol.ErrInvalidConnection):
		return agentcfg.MCPReattachClassOAuthBinding, true
	default:
		// Dial / initialize handshake / discovery / registration. This is the
		// class a header-authenticated connection lands in (401 without its
		// never-persisted header) and the only RETRYABLE one.
		return agentcfg.MCPReattachClassTransportFailed, false
	}
}

// reattachBackoffDelay is the exponential delay for the nth consecutive
// retryable failure, capped so a long-down third party is still retried
// periodically rather than abandoned.
func reattachBackoffDelay(failures int) time.Duration {
	d := reattachBackoffBase
	for i := 1; i < failures; i++ {
		d *= 2
		if d >= reattachBackoffMax {
			return reattachBackoffMax
		}
	}
	if d > reattachBackoffMax {
		return reattachBackoffMax
	}
	return d
}

// emitReattachLifecycle publishes one run-start re-attach lifecycle event. A nil
// bus is a no-op (the reconcile still completes — emission is observability). A
// publish failure is logged loud, never swallowed. NO secret is ever carried: the
// descriptor is non-secret by construction and the reason is scrubbed + bounded
// through the SAME scrubber the admin add path uses.
//
// The payload's Author carries the reconciling RUN's quadruple. Its non-empty
// RunID is the machine-readable discriminator between this family and the admin
// add family (whose RunID is empty).
func (a *MCPConnectionAttacher) emitReattachLifecycle(ctx context.Context, id identity.Quadruple, owner toolauth.Owner, desc agentcfg.MCPConnectionDescriptor, out reattachOutcome) {
	if a.bus == nil {
		return
	}
	now := time.Now().UTC()
	ev := events.Event{
		Type:       out.eventType,
		Identity:   id,
		OccurredAt: now,
		Payload: agentcfg.MCPConnectionLifecyclePayload{
			Author:    id,
			AgentID:   owner.Agent,
			ServerID:  desc.Name,
			Transport: string(desc.Transport),
			State:     out.state,
			Reason:    out.reason,
			// RevisionID is deliberately empty: the reconcile reads the ACTIVE
			// revision, it does not record one. PauseToken is likewise empty — a
			// run-start re-attach never parks.
			OccurredAt: now,
		},
	}
	if err := a.bus.Publish(ctx, ev); err != nil && a.logger != nil {
		a.logger.WarnContext(ctx, "mcp: publish run-start re-attach lifecycle event failed",
			slog.String("event_type", string(out.eventType)),
			slog.String("server_id", desc.Name),
			slog.String("agent_id", owner.Agent),
			slog.String("state", out.state),
			slog.String("err", err.Error()))
	}
}

// cloneAnnotationsForReattach returns a defensive copy of the declared
// annotation set so the config the driver builds cannot alias the revision's map.
func cloneAnnotationsForReattach(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
