package steering

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/sessions"
)

// DefaultNamingTimeout bounds the detached session auto-naming dispatch (the
// RecordCompletedTurn write, the eligibility read, the ONE Complete call, and
// the SetTitleAuto write) when the trigger fires. Like the run-completion
// hook, naming runs under a context detached from the (possibly cancelled) run
// ctx but bounded by this timeout — identity values preserved, cancellation
// detached — so a cancelled run still names and a wedged LLM can never leak
// the naming goroutine.
const DefaultNamingTimeout = 10 * time.Second

// naming digest bounds. The digest is a deterministically bounded slice of the
// completion-boundary transcript — the initial goal plus the tail of the
// conversation — with a per-entry cap and a hard total cap FAR below the
// context-safety threshold, so ErrContextLeak is unreachable by construction.
// Pinned by a unit test.
const (
	namingDigestMaxBytes      = 4096
	namingDigestPerEntryBytes = 512
	namingDigestTailEntries   = 4
	// namingMaxOutputTokens caps the naming Complete call's output — a title
	// is a single short line, so a small cap keeps the call cheap and bounded.
	namingMaxOutputTokens = 64
)

// Naming failure / skip classes for session.naming_failed. Stable and
// low-cardinality — NEVER the transcript, prompt, model output, or raw error.
const (
	namingClassLLMError          = "llm_error"
	namingClassTimeout           = "timeout"
	namingClassEmptyTitle        = "empty_title"
	namingClassGovernanceBlocked = "governance_blocked"
	namingClassManualTitle       = "manual_title"
	namingClassInternal          = "internal"
)

// NamingPolicy is the resolved session auto-naming policy for a run —
// per-run immutable input pinned into the run's snapshot at run start
// (agent-config `naming` section over the yaml `runtime.naming` default over
// off; next-turn projection). All defaults are already applied (withDefaults),
// so the trigger consumes concrete values.
type NamingPolicy struct {
	// AfterTurns is the completed-run count at which the first auto-name fires.
	AfterTurns int
	// RepeatEvery, when > 0, re-names every N turns after the first; 0 names
	// once only.
	RepeatEvery int
	// MaxRepetitions caps the TOTAL auto-namings (including the first). Only
	// consulted when RepeatEvery > 0; validated ≥ 1 there (no unlimited value).
	MaxRepetitions int
	// MaxTitleLen bounds the auto title in runes (the trigger clamps the model
	// output to it).
	MaxTitleLen int
}

// NamingCompleter is the narrow LLM seam the naming trigger calls — exactly
// the run's already-wrapped llm.LLMClient (governance outermost, keying the
// session identity from ctx; safety net; corrections; retry; downgrade). The
// trigger makes ONE Complete call through it; a second bespoke naming client
// or an unwrapped driver call is the rejected parallel implementation.
type NamingCompleter interface {
	Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error)
}

// SessionTitler is the narrow session-registry seam the naming trigger writes
// through: increment the turn counter, read the eligibility state, and apply
// the auto title (refusing a manual title). Satisfied by *sessions.Registry.
type SessionTitler interface {
	RecordCompletedTurn(ctx context.Context, id string, ident identity.Identity) (int, error)
	AutoNamingState(ctx context.Context, id string, ident identity.Identity) (sessions.AutoNamingState, error)
	SetTitleAuto(ctx context.Context, id string, ident identity.Identity, title string) error
}

// NamingSpec is the per-run session auto-naming configuration on
// [RunSpec.Naming]. When set (a naming policy is active for the run),
// [RunLoop.Run] fires the terminal-boundary trigger exactly once after
// (fin, err) settle — a sibling of the run-completion hook, planner-invisible.
// The trigger records the completed turn, then (when a title is due and the
// current title is not manual) makes ONE bounded Complete call over a
// deterministically bounded transcript digest and writes the result through
// SetTitleAuto. A naming failure NEVER alters the run outcome.
//
// It is per-run immutable input resolved once at run start; an edit is
// invisible to an in-flight run by construction (next-turn projection).
//
// # Accepted concurrent-completion race
//
// Two runs of one session completing concurrently can both pass eligibility
// and both make a naming call (one redundant cheap call, one winner). This is
// accepted, not guarded by a cross-run mutex: the registry serializes the
// title writes, so the record is never torn, and the cost is one redundant
// call in a narrow window (the same posture as the connection-reconcile accepted
// window).
//
// # Failure-retry posture and latency envelope
//
// A naming FAILURE does not burn the max-repetitions cap (only a successful
// SetTitleAuto bumps AutoNameCount), so as long as a title is due the trigger
// retries on EVERY subsequent completed run until one succeeds. Deliberate:
// a transient LLM outage must not permanently un-name a session. The flip
// side is worth knowing when operating a naming-on fleet against a DOWN
// naming LLM: every completed run then pays one failing synchronous Complete
// attempt (bounded by DefaultNamingTimeout, 10s) and emits one
// session.naming_failed — indefinitely, until the LLM recovers or the policy
// is switched off. The trigger runs SYNCHRONOUSLY at the terminal boundary,
// AFTER the completion hook (see the ordering note in RunLoop.Run), so the
// worst-case added latency before Run's caller-side bookkeeping completes is
// hook timeout + naming timeout, serialized (default 10s + 10s); the run's
// settled (fin, err) are unaffected either way.
type NamingSpec struct {
	// Policy is the resolved, defaulted auto-naming policy.
	Policy NamingPolicy
	// Titler is the session-registry write/read seam (the run's registry).
	Titler SessionTitler
	// LLM is the run's already-wrapped LLM client the ONE Complete call
	// flows through (governance/safety apply via ctx identity).
	LLM NamingCompleter
	// Model is the resolved effective model the naming call requests: the
	// policy's model when it names one, else the run's effective model, else
	// "" (the client's configured default).
	Model string
}

// steeringCaptureWanted reports whether the run needs applied steering entries
// captured for a terminal-boundary transcript consumer — the run-completion
// hook OR the auto-naming trigger. When neither is configured the capture is
// dead work and is skipped.
func steeringCaptureWanted(spec RunSpec) bool {
	if spec.CompletionHook != nil && spec.CompletionHook.Tool != "" {
		return true
	}
	if spec.Naming != nil && spec.Naming.Titler != nil {
		return true
	}
	return false
}

// WithDefaults fills the zero-valued policy fields with the session
// auto-naming defaults: AfterTurns → 1, MaxTitleLen → 80 (clamped into
// [8,200] when set), and — for a REPEATING policy (RepeatEvery > 0) whose cap
// is unset — MaxRepetitions → 5, the documented default total (including the
// first call). The default keeps the no-unlimited invariant intact for a
// policy constructed programmatically (an embedder building a NamingSpec by
// hand): a repeating policy always reaches the trigger with a concrete cap.
// The wire and yaml edges still REQUIRE an explicit cap ≥ 1 whenever
// repeat_every > 0 (rejected at set/boot time), so the default is
// defence-in-depth there, never a silent unlimited. The run-start projection
// applies WithDefaults so the trigger consumes concrete values.
func (p NamingPolicy) WithDefaults() NamingPolicy {
	out := p
	if out.AfterTurns <= 0 {
		out.AfterTurns = 1
	}
	if out.MaxTitleLen <= 0 {
		out.MaxTitleLen = 80
	}
	if out.MaxTitleLen < 8 {
		out.MaxTitleLen = 8
	}
	if out.MaxTitleLen > 200 {
		out.MaxTitleLen = 200
	}
	if out.RepeatEvery > 0 && out.MaxRepetitions <= 0 {
		out.MaxRepetitions = 5
	}
	return out
}

// namingDue reports whether an auto-naming is due given the resolved policy and
// the session's current naming state, per the eligibility rule:
//
//	TitleSource != manual AND
//	( (AutoNameCount == 0 AND TurnCount >= after_turns)
//	  OR (repeat_every > 0 AND AutoNameCount < max_repetitions
//	      AND TurnCount >= LastAutoNamedTurn + repeat_every) )
//
// The manual check is the caller's responsibility (it emits the manual_title
// skip class); namingDue assumes a non-manual title and evaluates the count /
// cadence gates.
func namingDue(p NamingPolicy, st sessions.AutoNamingState) bool {
	if st.AutoNameCount == 0 {
		return st.TurnCount >= p.AfterTurns
	}
	if p.RepeatEvery > 0 && st.AutoNameCount < p.MaxRepetitions {
		return st.TurnCount >= st.LastAutoNamedTurn+p.RepeatEvery
	}
	return false
}

// fireNaming is the run loop's terminal-boundary auto-naming trigger. It runs
// SYNCHRONOUSLY from a deferred closure over Run's named returns AFTER
// (fin, err) are settled — and after the completion hook has fired (the
// hook-first ordering keeps the hook's timestamps accurate; see RunLoop.Run)
// — and NEVER alters them. When a naming policy is active it always records
// the completed turn (the only counter write); then, if a title is due and
// the current title is not manual, it makes ONE bounded Complete call over a
// transcript digest and writes the result through SetTitleAuto. Every
// failure or skip emits session.naming_failed with a stable class + a Warn
// log; success is signalled by the registry's content-free
// session.title_changed(source=auto). A recover() contains any panic so a
// wedged dependency can never replace the settled run result.
//
// A failure does NOT burn the cap (only a successful SetTitleAuto bumps
// AutoNameCount), so a still-due title is retried at every subsequent
// completed run until one succeeds — see the NamingSpec godoc's
// "Failure-retry posture and latency envelope" for the operational
// consequences on a naming-on fleet with a down LLM.
func (rl *RunLoop) fireNaming(runCtx context.Context, spec RunSpec, q identity.Quadruple, steering []steeringEntry, initialGoal string, fin planner.Finish) {
	ns := spec.Naming
	if ns == nil || ns.Titler == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			rl.namingFailed(runCtx, q, namingClassInternal)
		}
	}()

	// Detach cancellation (a cancelled run still names) while preserving ctx
	// VALUES (the identity quadruple keeps flowing so governance keys the
	// session identity), bounded by the naming timeout.
	nctx, cancel := context.WithTimeout(context.WithoutCancel(runCtx), DefaultNamingTimeout)
	defer cancel()

	ident := q.Identity
	sid := q.SessionID

	// Always record the completed turn when a policy is active (the only
	// counter write; counting starts at enablement).
	if _, err := ns.Titler.RecordCompletedTurn(nctx, sid, ident); err != nil {
		rl.namingFailed(runCtx, q, namingClassInternal)
		return
	}

	st, err := ns.Titler.AutoNamingState(nctx, sid, ident)
	if err != nil {
		rl.namingFailed(runCtx, q, namingClassInternal)
		return
	}
	if st.TitleSource == sessions.TitleSourceManual {
		// A human title exists — manual wins. A skip, emitted loud.
		rl.namingFailed(runCtx, q, namingClassManualTitle)
		return
	}
	if !namingDue(ns.Policy, st) {
		// Not yet due (count / cadence gate) — a normal non-event, no LLM call.
		return
	}

	digest := buildNamingDigest(initialGoal, spec.Base.Trajectory, steering, fin, st.CurrentTitle)
	req := namingCompleteRequest(ns.Model, ns.Policy.MaxTitleLen, digest, st.CurrentTitle != "")

	resp, err := ns.LLM.Complete(nctx, req)
	if err != nil {
		rl.namingFailed(runCtx, q, classifyNamingErr(err))
		return
	}
	title := clampNamingTitle(resp.Content, ns.Policy.MaxTitleLen)
	if title == "" {
		rl.namingFailed(runCtx, q, namingClassEmptyTitle)
		return
	}
	if err := ns.Titler.SetTitleAuto(nctx, sid, ident, title); err != nil {
		if errors.Is(err, sessions.ErrManualTitle) {
			// A manual title raced in between the read and the write.
			rl.namingFailed(runCtx, q, namingClassManualTitle)
			return
		}
		rl.namingFailed(runCtx, q, namingClassInternal)
		return
	}
	// Success: SetTitleAuto published the content-free session.title_changed
	// (source=auto) — no additional event on the happy path.
}

// classifyNamingErr maps a naming Complete error to a stable, low-cardinality,
// redaction-safe class. A governance PreCall block (budget / rate / max-tokens)
// is governance_blocked; a deadline is timeout; everything else is llm_error.
// The raw error message is never used — it may quote caller data (§7).
func classifyNamingErr(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, governance.ErrBudgetExceeded),
		errors.Is(err, governance.ErrRateLimited),
		errors.Is(err, governance.ErrMaxTokensExceeded):
		return namingClassGovernanceBlocked
	case errors.Is(err, context.DeadlineExceeded):
		return namingClassTimeout
	case errors.Is(err, context.Canceled):
		// The detached ctx is only cancelled by our own timeout's cancel on
		// return; a Canceled here is effectively a timeout-class deadline miss.
		return namingClassTimeout
	default:
		return namingClassLLMError
	}
}

// namingFailed emits the canonical session.naming_failed event (SafePayload:
// session id + stable class ONLY — never content) AND Warn-logs the CLASS
// (never the raw error / title / transcript). A naming failure never alters
// the run outcome (§13 — fail loud, never silent, never escalated).
func (rl *RunLoop) namingFailed(ctx context.Context, q identity.Quadruple, class string) {
	rl.logger.WarnContext(ctx, "steering: session auto-naming skipped/failed — run outcome unchanged",
		slog.String("tenant_id", q.TenantID),
		slog.String("user_id", q.UserID),
		slog.String("session_id", q.SessionID),
		slog.String("run_id", q.RunID),
		slog.String("error_class", class))
	if rl.bus == nil {
		return
	}
	ev := events.Event{
		Type:     EventTypeSessionNamingFailed,
		Identity: q,
		Payload: SessionNamingFailedPayload{
			SessionID:  q.SessionID,
			ErrorClass: class,
		},
	}
	if err := rl.bus.Publish(ctx, ev); err != nil {
		rl.logger.WarnContext(ctx, "steering: session.naming_failed publish failed",
			slog.String("run_id", q.RunID),
			slog.Any("error", err))
	}
}

// namingSystemPromptf is the fixed titling instruction template. maxLen is
// interpolated so the model targets the policy bound (the runtime clamps
// regardless).
const namingSystemPromptf = "You name conversations. Read the conversation digest and reply with a single, " +
	"concise, human-readable title of at most %d characters that captures its topic. " +
	"Reply with the title ONLY — no quotes, no preamble, no trailing punctuation, one line."

// namingCompleteRequest builds the bounded naming Complete request: the fixed
// system instruction + the digest as the user message, the resolved model, and
// a small output-token cap. It carries no tools and no response format — a
// plain text completion.
func namingCompleteRequest(model string, maxTitleLen int, digest string, rename bool) llm.CompleteRequest {
	sys := fmt.Sprintf(namingSystemPromptf, maxTitleLen)
	if rename {
		sys += " A working title already exists in the digest; refine it if the conversation has moved on, otherwise keep it."
	}
	sysText := sys
	userText := digest
	maxTok := namingMaxOutputTokens
	return llm.CompleteRequest{
		Model: model,
		Messages: []llm.ChatMessage{
			{Role: llm.RoleSystem, Content: llm.Content{Text: &sysText}},
			{Role: llm.RoleUser, Content: llm.Content{Text: &userText}},
		},
		MaxTokens: &maxTok,
	}
}

// buildNamingDigest assembles a deterministically bounded text digest of the
// completion-boundary transcript: the initial goal (first entry) plus the tail
// of the conversation (final answer + the entries preceding it), each entry
// truncated to namingDigestPerEntryBytes and the whole digest hard-cut to
// namingDigestMaxBytes. On a re-name the current title is prepended. The byte
// bound is guaranteed by construction (pinned by a unit test) so the
// context-safety net can never fire on the naming call.
func buildNamingDigest(initialGoal string, traj *planner.Trajectory, steering []steeringEntry, fin planner.Finish, currentTitle string) string {
	finalAnswer, hasFinal := renderFinalAnswer(fin)
	entries := assembleTranscript(initialGoal, traj, steering, finalAnswer, hasFinal)

	// Select the first entry (the goal) and the last namingDigestTailEntries
	// entries by position, de-duplicated, preserving original order.
	seen := make(map[int]struct{}, namingDigestTailEntries+1)
	if len(entries) > 0 {
		seen[0] = struct{}{}
	}
	for i := len(entries) - namingDigestTailEntries; i < len(entries); i++ {
		if i >= 0 {
			seen[i] = struct{}{}
		}
	}
	ordered := make([]TranscriptEntry, 0, len(seen))
	for i := range entries {
		if _, ok := seen[i]; ok {
			ordered = append(ordered, entries[i])
		}
	}

	var b strings.Builder
	if t := strings.TrimSpace(currentTitle); t != "" {
		b.WriteString("Current title: ")
		b.WriteString(truncateRunes(t, namingDigestPerEntryBytes))
		b.WriteString("\n")
	}
	for _, e := range ordered {
		content := truncateRunes(strings.TrimSpace(e.Content), namingDigestPerEntryBytes)
		if content == "" {
			continue
		}
		b.WriteString(e.Kind)
		b.WriteString(": ")
		b.WriteString(content)
		b.WriteString("\n")
	}
	return hardCutBytes(b.String(), namingDigestMaxBytes)
}

// truncateRunes cuts s to at most maxBytes bytes on a rune boundary (never
// splitting a multi-byte rune). It appends nothing — the cut is silent (the
// digest is a lossy summary by design).
func truncateRunes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// Walk runes until adding the next would exceed maxBytes.
	out := make([]byte, 0, maxBytes)
	for _, r := range s {
		rb := utf8.RuneLen(r)
		if len(out)+rb > maxBytes {
			break
		}
		out = utf8.AppendRune(out, r)
	}
	return string(out)
}

// hardCutBytes guarantees the digest never exceeds maxBytes bytes, on a rune
// boundary. It is the last-resort total-bound enforcement (per-entry caps
// already bound each line).
func hardCutBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return truncateRunes(s, maxBytes)
}

// clampNamingTitle deterministically post-processes the naming model output
// into a candidate title: the first non-empty line, newline/CR characters
// removed, whitespace- and quote-trimmed, cut to maxTitleLen runes. General
// control-character stripping is NOT done here — the registry's defensive
// re-clamp (SetTitleAuto) strips control characters before the record is
// saved, so a stray one never persists. An all-empty result is "" (the
// empty_title skip). This is the trusted-internal clamp — the asymmetry with
// SetTitle's reject-on-oversize is intentional.
func clampNamingTitle(raw string, maxTitleLen int) string {
	line := raw
	for _, cand := range strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if strings.TrimSpace(cand) != "" {
			line = cand
			break
		}
	}
	line = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, line)
	line = strings.TrimSpace(line)
	line = strings.Trim(line, "\"'`")
	line = strings.TrimSpace(line)
	if maxTitleLen > 0 && utf8.RuneCountInString(line) > maxTitleLen {
		runes := []rune(line)
		line = strings.TrimSpace(string(runes[:maxTitleLen]))
	}
	return line
}
