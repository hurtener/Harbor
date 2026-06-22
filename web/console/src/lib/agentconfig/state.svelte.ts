// Harbor Console — agent-config control-panel reactive state controller
// (the consolidated agent-config consumer). Svelte 5 runes mode (D-092).
//
// `AgentConfigPanelState` owns the panel's reactive state; the `.svelte`
// components read it and call its actions, never touching the Protocol
// client directly (CONVENTIONS.md §6). The panel is a pure CONSUMER of the
// `agent_config.*` control plane (D-234 / D-235) — it holds NO config of
// its own (D-061), only the UI form/loading state. Every read is an
// `agent_config.*` snapshot + the live `mcp.connection.*` event stream;
// every write goes through the typed `AgentConfigNamespace` on
// `HarborClient`; there is no hand-rolled `fetch`.
//
// # Admin-gated writes (CONVENTIONS.md §5, the 92b precedent)
//
// Every write method on the control plane is admin-scoped (D-235). The
// panel reads `hasAdminScope` from the resolved connection's verified
// scope claims and renders every write control disabled-with-tooltip when
// it is false (exactly like the 92b `TenantDefaultOverridesCard`). The
// runtime ALSO gates — a forged call fails closed with a `scope_mismatch`
// 403 the area's error state surfaces. Reads use the four-state
// `<PageState>` contract (CONVENTIONS.md §4).
//
// # The selected agent
//
// The panel targets ONE agent at a time. The selector defaults to the
// connected runtime's agent (in dev that is `harbor-dev-agent`) and can be
// overridden by the operator (or a `?agent=` deep-link). The agent-config
// methods key off the `agent_id` directly and do NOT require the agent to
// be a registered AgentRegistry row, so the panel works against the dev
// runtime's synthetic default agent.

import { resolveConnection, hasScope, type RuntimeConnection } from '$lib/connection.js';
import { HarborClient, type ProtocolClient } from '$lib/protocol/harbor.js';
import { ProtocolError } from '$lib/protocol/errors.js';
import { EventsSubscription } from '$lib/events/subscription.svelte.js';
import type { PageStatus } from '$lib/components/ui/PageState.svelte';
import type {
	AgentConfigRevisionView,
	AgentConfigSkillSummary,
	AgentConfigSkillInput,
	AgentConfigDiff,
	AgentConfigMCPConnectionDescriptor,
	AgentConfigLLMParams,
	AgentConfigPayload
} from '$lib/protocol/agentconfig.js';

/** The default agent the panel targets when no `?agent=` is supplied — the
 * dev runtime's synthetic agent (the validation harness boots under it). */
export const DEFAULT_AGENT_ID = 'harbor-dev-agent';

/** The canonical `mcp.connection.*` lifecycle event types the panel tails
 * so the MCP-policy + add-connection areas reflect pause/resume + the
 * pending/failed/auth-required attach lifecycle live. */
export const MCP_CONNECTION_EVENT_TYPES = [
	'mcp.connection.pending',
	'mcp.connection.added',
	'mcp.connection.failed',
	'mcp.connection.auth_required',
	'mcp.connection.paused',
	'mcp.connection.resumed'
] as const;

/**
 * The six control-plane areas the panel exposes, in display order. The
 * panel renders a left sub-nav rail of these (the Settings page's
 * single-section vocabulary — CONVENTIONS.md §3): exactly ONE area is in
 * view at a time. The ids match the per-section `data-testid`s the e2e
 * spec asserts. This is a pure UI nav descriptor — no Protocol surface.
 */
export const AGENT_CONFIG_AREAS = [
	{ id: 'revisions', label: 'Revision history' },
	{ id: 'prompt', label: 'Layered prompt' },
	{ id: 'llm', label: 'Model & sampling' },
	{ id: 'skills', label: 'Skills' },
	{ id: 'mcp', label: 'MCP policy' },
	{ id: 'add-connection', label: 'Add connection' }
] as const;

/** The id of one of the five control-plane areas (drives the rail + pane). */
export type AgentConfigAreaId = (typeof AGENT_CONFIG_AREAS)[number]['id'];

/** A page-friendly error projection (mirrors the Settings page's shape). */
export interface PageError {
	code: string;
	message: string;
}

/** The per-area async phase shared by every write surface (mirrors the
 * 92b `TenantDefaultOverridesState` four-state vocabulary). */
export type AreaPhase = 'idle' | 'loading' | 'ready' | 'saving' | 'error';

/**
 * describeError renders a `ProtocolError` into a page-friendly message,
 * keeping the canonical code visible so the operator knows the recovery.
 * The admin-scope rejections (`scope_mismatch` / `identity_scope_required`)
 * get a human sentence; everything else passes the runtime message through.
 */
export function describeError(e: unknown): PageError {
	if (e instanceof ProtocolError) {
		switch (e.code) {
			case 'identity_scope_required':
			case 'scope_mismatch':
				return { code: e.code, message: 'This action requires the admin scope claim.' };
			case 'identity_required':
				return {
					code: e.code,
					message: 'Identity scope is incomplete — re-attach to the runtime.'
				};
			default:
				return { code: e.code, message: e.message };
		}
	}
	if (e instanceof Error) {
		return { code: 'runtime_error', message: e.message };
	}
	return { code: 'runtime_error', message: 'Unknown error' };
}

/** shortRevision renders a copy-friendly short revision id (first 12 chars). */
export function shortRevision(id: string): string {
	return id.length > 12 ? `${id.slice(0, 12)}…` : id;
}

/** The display label for each config section, used by the derived
 * per-revision change summary. */
const SECTION_LABELS: { key: keyof AgentConfigPayload; label: string }[] = [
	{ key: 'prompt_layers', label: 'Prompt' },
	{ key: 'llm_params', label: 'Model & sampling' },
	{ key: 'skills', label: 'Skills' },
	{ key: 'tool_exposure', label: 'MCP policy' },
	{ key: 'connections', label: 'Connections' }
];

/**
 * changedSectionLabels compares two config payloads section-by-section and
 * returns the display labels of the sections that differ, in display order.
 * The payloads are the server's canonical (normalised, sorted) form, so a
 * stable JSON encoding is an exact section-equality test. A nil section on
 * one side and a present section on the other counts as a change. Pure;
 * exported for the unit suite that pins each section's mapping.
 */
export function changedSectionLabels(
	from: AgentConfigPayload | undefined,
	to: AgentConfigPayload | undefined
): string[] {
	const a = from ?? {};
	const b = to ?? {};
	const out: string[] = [];
	for (const { key, label } of SECTION_LABELS) {
		if (JSON.stringify(a[key] ?? null) !== JSON.stringify(b[key] ?? null)) {
			out.push(label);
		}
	}
	return out;
}

/**
 * AgentConfigPanelState owns the consolidated panel's reactive state. It
 * exposes a primary `PageStatus` (the panel-level four-state boundary) plus
 * the five feature areas, each with its own data + write action + per-area
 * busy / error / saved flag. The components are dumb views over this
 * controller (CONVENTIONS.md §6); the panel composes them.
 */
export class AgentConfigPanelState {
	/* ---- connection + client (CONVENTIONS.md §6) ------------------- */
	connection = $state<RuntimeConnection | null>(null);
	/** The selected agent id (the selector value; `?agent=` overrides). */
	agentId = $state<string>(DEFAULT_AGENT_ID);

	/** True when the resolved connection carries the `admin` scope claim. */
	get hasAdminScope(): boolean {
		return hasScope(this.connection, 'admin');
	}
	/** True when the Console is not attached to a Runtime. */
	get disconnected(): boolean {
		return this.connection === null;
	}

	/* ---- primary panel async state (the revision + active config) -- */
	status = $state<PageStatus>('loading');
	error = $state<PageError | null>(null);
	/**
	 * The friendly explanation rendered when `status === 'info'`. The
	 * agent-config READ surface is itself admin-scoped (D-235 — reading an
	 * agent's control-plane config is privileged), so a non-admin caller
	 * cannot load the panel at all. Rather than a scary red "Request failed"
	 * with a meaningless Retry (retrying as a non-admin re-fails), a
	 * `scope_mismatch` on load routes to the not-applicable `info` branch
	 * (the §83w/D-164 pattern), naming the missing scope.
	 */
	info = $state<{ headline: string; detail: string } | null>(null);

	/* ---- revision history + diff + rollback (92a) ------------------ */
	revisions = $state<AgentConfigRevisionView[]>([]);
	activeRevisionId = $state<string>('');
	/** The two-revision diff selection (the compare control). */
	fromRevision = $state<string>('');
	toRevision = $state<string>('');
	diff = $state<AgentConfigDiff | null>(null);
	diffPhase = $state<AreaPhase>('idle');
	diffError = $state<PageError | null>(null);
	rollbackBusy = $state<string | null>(null);
	rollbackError = $state<PageError | null>(null);
	rolledBackTo = $state<string | null>(null);

	/* ---- skills (92c) ---------------------------------------------- */
	skills = $state<AgentConfigSkillSummary[]>([]);
	skillsPhase = $state<AreaPhase>('idle');
	skillsError = $state<PageError | null>(null);
	/** The add-skill form fields. */
	skillName = $state<string>('');
	skillTitle = $state<string>('');
	skillTrigger = $state<string>('');
	skillSteps = $state<string>('');
	skillBusy = $state<boolean>(false);

	/* ---- MCP pause/resume + per-tool disable (92d) ----------------- */
	/** The desired-state tool-exposure (paused servers + disabled tools),
	 * seeded from the active config and replaced on save. */
	pausedServers = $state<string[]>([]);
	disabledTools = $state<string[]>([]);
	exposurePhase = $state<AreaPhase>('idle');
	exposureError = $state<PageError | null>(null);
	exposureSaved = $state<boolean>(false);

	/* ---- layered prompt (92e) -------------------------------------- */
	promptBase = $state<string>('');
	promptUser = $state<string>('');
	promptPhase = $state<AreaPhase>('idle');
	promptError = $state<PageError | null>(null);
	promptSaved = $state<boolean>(false);

	/* ---- model & sampling — per-agent LLM params (92j/92i) --------- */
	/** The per-agent LLM-params form fields. The numeric fields bind to
	 * `<input type="number">`, so Svelte holds them as `number | null` (an
	 * empty input is `null` — "inherit the next layer": the tenant-wide
	 * baseline, then config). Model + reasoning-effort are text/select. */
	llmModel = $state<string>('');
	llmTemperature = $state<number | null>(null);
	llmMaxTokens = $state<number | null>(null);
	llmReasoningEffort = $state<string>('');
	llmPhase = $state<AreaPhase>('idle');
	llmError = $state<PageError | null>(null);
	llmSaved = $state<boolean>(false);

	/* ---- atomic multi-section staging — "Save all" (92i) ----------- */
	/** Per-form-area dirty flags: an edit in an area sets its flag; a
	 * successful save (per-area or Save-all) or a discard clears it. The
	 * Save-all bar reads {@link stagedAreaCount}. Skills + connections are
	 * managed via discrete list operations (upsert/delete/add), not the
	 * staged form, so they are not part of the Save-all payload. */
	promptDirty = $state<boolean>(false);
	exposureDirty = $state<boolean>(false);
	llmDirty = $state<boolean>(false);
	saveAllPhase = $state<AreaPhase>('idle');
	saveAllError = $state<PageError | null>(null);
	saveAllSaved = $state<boolean>(false);

	/* ---- diff-before-rollback preview (92i) ------------------------ */
	/** The revision a rollback is being previewed against (the modal target),
	 * or null when no preview is open. A rollback NEVER repoints blindly: the
	 * operator confirms against the structured `agent_config.diff` (active →
	 * target) rendered in the preview. */
	rollbackTarget = $state<string | null>(null);
	rollbackPreviewDiff = $state<AgentConfigDiff | null>(null);
	rollbackPreviewPhase = $state<AreaPhase>('idle');
	rollbackPreviewError = $state<PageError | null>(null);

	/* ---- add MCP connection (92f) ---------------------------------- */
	connName = $state<string>('');
	connTransport = $state<'stdio' | 'http'>('stdio');
	connCommand = $state<string>('');
	connUrl = $state<string>('');
	/** Secret auth headers, entered as `Key: value` lines; NEVER rendered
	 * back after submit (the header value is write-only). */
	connHeaders = $state<string>('');
	addConnBusy = $state<boolean>(false);
	addConnError = $state<PageError | null>(null);
	/** The terminal attach state from the add-connection response
	 * ("online" | "failed" | "auth_required"), plus its reason. */
	addConnState = $state<string | null>(null);
	addConnReason = $state<string | null>(null);

	/* ---- live mcp.connection.* event stream (92d/92f) -------------- */
	subscription = $state<EventsSubscription | null>(null);

	#client: ProtocolClient | null = null;

	/* ================================================================ */
	/* Derived projections                                               */
	/* ================================================================ */

	/** The live `mcp.connection.*` advisories for the selected agent,
	 * newest-first — the pause/resume + attach-lifecycle feed. */
	get mcpEvents() {
		return this.subscription?.events ?? [];
	}

	/** Count of form areas with unsaved staged edits (prompt / model &
	 * sampling / MCP exposure). Skills + connections are discrete list
	 * operations (upsert / delete / add), not staged into the Save-all
	 * payload, so they are not counted. */
	get stagedAreaCount(): number {
		return (this.promptDirty ? 1 : 0) + (this.exposureDirty ? 1 : 0) + (this.llmDirty ? 1 : 0);
	}

	/** True when at least one form area has staged, unsaved edits. */
	get hasStagedChanges(): boolean {
		return this.stagedAreaCount > 0;
	}

	/**
	 * revisionSummary derives a human-readable one-line description of what a
	 * revision changed versus its PARENT — purely from the payloads
	 * `list_revisions` already returns (no extra Protocol round-trip, so the
	 * history list is legible without N diff calls). The first revision (no
	 * parent) is "Initial configuration". A revision whose `content_hash`
	 * matches an EARLIER revision in the chain is a revert-by-re-set and is
	 * labelled "Reverted to <short-id>". Otherwise it lists the sections that
	 * changed (e.g. "Prompt + Model & sampling"). An empty change set (only
	 * metadata moved) reads "No section changes".
	 */
	revisionSummary(rev: AgentConfigRevisionView): string {
		if (rev.parent_revision_id === undefined || rev.parent_revision_id === '') {
			return 'Initial configuration';
		}
		// Revert detection: a content_hash equal to a STRICTLY-OLDER ancestor's
		// (NOT the direct parent — that would be a no-op re-set, handled below as
		// "No section changes") means this revision re-pinned that ancestor's
		// exact payload.
		const idx = this.revisions.findIndex((r) => r.revision_id === rev.revision_id);
		if (idx >= 0) {
			for (let j = idx + 1; j < this.revisions.length; j++) {
				const older = this.revisions[j];
				if (
					older.content_hash !== '' &&
					older.content_hash === rev.content_hash &&
					older.revision_id !== rev.parent_revision_id
				) {
					return `Reverted to ${shortRevision(older.revision_id)}`;
				}
			}
		}
		const parent = this.revisions.find((r) => r.revision_id === rev.parent_revision_id);
		if (parent === undefined) {
			// The chain is always fully loaded in the shipped path (listRevisions
			// has no limit), so this is defensive: don't diff against {} (which
			// would over-report every section as changed) — name the gap instead.
			return `Changes from ${shortRevision(rev.parent_revision_id)} (earlier revisions not loaded)`;
		}
		const changed = changedSectionLabels(parent.payload, rev.payload);
		return changed.length === 0 ? 'No section changes' : changed.join(' + ');
	}

	/* ================================================================ */
	/* Boot + loading                                                    */
	/* ================================================================ */

	/** Sets the target agent id (the selector / `?agent=` deep-link). */
	setAgent(id: string): void {
		const next = id.trim();
		this.agentId = next === '' ? DEFAULT_AGENT_ID : next;
	}

	/**
	 * load fans out the panel's reads against the selected agent:
	 * `agent_config.get` (the active revision → seeds the prompt + exposure
	 * forms), `agent_config.list_revisions` (the history), and
	 * `agent_config.skills.list`. When the Console is not attached it goes
	 * to the Disconnected state — never Error (CONVENTIONS.md §8). `injected`
	 * is an optional in-page client the harness / tests supply.
	 */
	async load(injected?: ProtocolClient): Promise<void> {
		const connection = resolveConnection();
		this.connection = connection;
		if (connection === null) {
			this.#client = null;
			this.status = 'disconnected';
			this.subscription?.close();
			this.subscription = null;
			return;
		}
		this.#client = injected ?? new HarborClient({ connection });
		this.status = 'loading';
		this.error = null;
		this.info = null;

		// The whole agent_config READ surface is admin-scoped (D-235 — reading
		// an agent's control-plane config is privileged). A non-admin caller
		// cannot load the panel at all, so route straight to the not-applicable
		// info branch WITHOUT a doomed read (no scary error / Retry, no wasted
		// 403 round-trip). The runtime still gates server-side; the catch below
		// keeps the same info treatment as a fallback if a stale client scope
		// claims admin but the token does not.
		if (!this.hasAdminScope) {
			this.subscription?.close();
			this.subscription = null;
			this.info = {
				headline: 'Admin scope required',
				detail:
					'Viewing and managing an agent’s configuration requires the admin scope claim. Reconnect with an admin token to use this panel.'
			};
			this.status = 'info';
			return;
		}

		// Open the live mcp.connection.* advisory stream once per load.
		this.subscription?.close();
		const sub = new EventsSubscription(this.#client.events);
		sub.open({ eventTypes: MCP_CONNECTION_EVENT_TYPES as unknown as string[] });
		this.subscription = sub;

		try {
			// The CORE reads (the active revision + the history) gate the panel —
			// these are the agent-config control plane proper. Skills control is
			// an OPTIONAL surface layered on top (a SkillStore must be wired), so
			// it is fetched SEPARATELY and degrades on its own without sinking the
			// whole panel (the revisions / prompt / model-&-sampling / MCP areas
			// don't need it — §17.6: a live runtime without skills wired must
			// still show the revision UX).
			const [get, list] = await Promise.all([
				this.#client.agentConfig.get(this.agentId),
				this.#client.agentConfig.listRevisions(this.agentId)
			]);
			this.revisions = list.revisions;
			this.activeRevisionId = get.revision?.revision_id ?? '';
			this.seedFromRevision(get.revision ?? null);
			try {
				const skills = await this.#client.agentConfig.skillsList(this.agentId);
				this.skills = skills.skills;
				this.skillsPhase = 'ready';
			} catch (e) {
				// Skills control not wired (unknown_method) — surface it in the
				// skills area only, keep the rest of the panel live.
				this.skills = [];
				this.skillsError = describeError(e);
				this.skillsPhase = 'error';
			}
			// Default the diff selection to the two newest revisions.
			if (this.revisions.length >= 2) {
				this.toRevision = this.revisions[0].revision_id;
				this.fromRevision = this.revisions[1].revision_id;
			} else if (this.revisions.length === 1) {
				this.toRevision = this.revisions[0].revision_id;
				this.fromRevision = this.revisions[0].revision_id;
			}
			this.status = 'ready';
		} catch (e) {
			if (e instanceof ProtocolError && e.code === 'scope_mismatch') {
				// The panel's reads are admin-scoped; a non-admin cannot view it.
				// Route to the not-applicable info branch (no scary error, no
				// meaningless Retry) rather than the error branch.
				this.info = {
					headline: 'Admin scope required',
					detail:
						'Viewing and managing an agent’s configuration requires the admin scope claim. Reconnect with an admin token to use this panel.'
				};
				this.status = 'info';
				return;
			}
			this.error = describeError(e);
			this.status = 'error';
		}
	}

	/** Re-loads the panel after a write so the rendered config + history
	 * reflect the new revision (the next-turn projection — D-025). */
	async reload(): Promise<void> {
		await this.load(this.#client ?? undefined);
	}

	/** Closes the live subscription on unmount. */
	close(): void {
		this.subscription?.close();
		this.subscription = null;
	}

	/** Seeds the prompt + exposure + model-&-sampling form fields from an
	 * active revision and clears the staged-edit dirty flags (the form now
	 * mirrors the persisted active revision). */
	private seedFromRevision(rev: AgentConfigRevisionView | null): void {
		const payload = rev?.payload;
		this.promptBase = payload?.prompt_layers?.base ?? '';
		this.promptUser = payload?.prompt_layers?.user ?? '';
		this.pausedServers = [...(payload?.tool_exposure?.paused_servers ?? [])];
		this.disabledTools = [...(payload?.tool_exposure?.disabled_tools ?? [])];
		const llm = payload?.llm_params;
		this.llmModel = llm?.model ?? '';
		this.llmTemperature = llm?.temperature ?? null;
		this.llmMaxTokens = llm?.max_tokens ?? null;
		this.llmReasoningEffort = llm?.reasoning_effort ?? '';
		this.promptDirty = false;
		this.exposureDirty = false;
		this.llmDirty = false;
	}

	/* ================================================================ */
	/* Revision history + diff + rollback (92a)                          */
	/* ================================================================ */

	/** Computes the server-side diff between the two selected revisions. */
	async computeDiff(): Promise<void> {
		if (this.#client === null) return;
		if (this.fromRevision === '' || this.toRevision === '') return;
		this.diffPhase = 'loading';
		this.diffError = null;
		try {
			const resp = await this.#client.agentConfig.diff(
				this.agentId,
				this.fromRevision,
				this.toRevision
			);
			this.diff = resp.diff;
			this.diffPhase = 'ready';
		} catch (e) {
			this.diff = null;
			this.diffError = describeError(e);
			this.diffPhase = 'error';
		}
	}

	/**
	 * requestRollback opens the diff-before-rollback preview: it fetches the
	 * structured `agent_config.diff` (active → target) and surfaces it for
	 * explicit confirmation. The rollback NEVER repoints blindly — it only
	 * fires from {@link confirmRollbackPreview} after the operator has seen the
	 * exact delta. Admin-gated; a no-op for the already-active revision or when
	 * the agent has no active revision to diff against.
	 */
	async requestRollback(revisionId: string): Promise<void> {
		if (this.#client === null || !this.hasAdminScope) return;
		if (revisionId === this.activeRevisionId || this.activeRevisionId === '') return;
		this.rollbackTarget = revisionId;
		this.rollbackPreviewDiff = null;
		this.rollbackPreviewError = null;
		this.rollbackError = null;
		this.rolledBackTo = null;
		this.rollbackPreviewPhase = 'loading';
		try {
			// active → target: the delta reverting to `target` will apply.
			const resp = await this.#client.agentConfig.diff(
				this.agentId,
				this.activeRevisionId,
				revisionId
			);
			this.rollbackPreviewDiff = resp.diff;
			this.rollbackPreviewPhase = 'ready';
		} catch (e) {
			this.rollbackPreviewError = describeError(e);
			this.rollbackPreviewPhase = 'error';
		}
	}

	/** Cancels the rollback preview without writing — a pure no-op (the
	 * active pointer is untouched). */
	cancelRollbackPreview(): void {
		this.rollbackTarget = null;
		this.rollbackPreviewDiff = null;
		this.rollbackPreviewPhase = 'idle';
		this.rollbackPreviewError = null;
	}

	/**
	 * confirmRollbackPreview repoints the active pointer to the previewed
	 * target revision — the only path that actually rolls back, reached only
	 * after the operator confirmed the rendered diff (never a blind repoint).
	 * Admin-gated.
	 */
	async confirmRollbackPreview(): Promise<void> {
		const revisionId = this.rollbackTarget;
		if (this.#client === null || !this.hasAdminScope || revisionId === null) return;
		if (this.rollbackBusy !== null) return;
		this.rollbackBusy = revisionId;
		this.rollbackError = null;
		this.rolledBackTo = null;
		try {
			await this.#client.agentConfig.rollback(this.agentId, revisionId);
			this.rolledBackTo = revisionId;
			this.cancelRollbackPreview();
			await this.reload();
		} catch (e) {
			this.rollbackError = describeError(e);
		} finally {
			this.rollbackBusy = null;
		}
	}

	/* ================================================================ */
	/* Skills (92c)                                                      */
	/* ================================================================ */

	/** Upserts the add-skill form as a new skill; records a revision.
	 * The pack-overwrite refusal surfaces as a clear inline error. */
	async addSkill(): Promise<void> {
		if (this.#client === null || !this.hasAdminScope) return;
		if (this.skillBusy) return;
		if (this.skillName.trim() === '' || this.skillTrigger.trim() === '') {
			this.skillsError = { code: 'invalid_input', message: 'Name and trigger are required.' };
			return;
		}
		this.skillBusy = true;
		this.skillsError = null;
		const skill: AgentConfigSkillInput = {
			name: this.skillName.trim(),
			title: this.skillTitle.trim() || undefined,
			trigger: this.skillTrigger.trim(),
			steps: this.skillSteps
				.split('\n')
				.map((s) => s.trim())
				.filter((s) => s.length > 0),
			origin: 'generated',
			scope: 'project'
		};
		try {
			await this.#client.agentConfig.skillsUpsert(this.agentId, skill);
			this.skillName = '';
			this.skillTitle = '';
			this.skillTrigger = '';
			this.skillSteps = '';
			await this.reloadSkills();
			await this.reloadRevisions();
		} catch (e) {
			this.skillsError = describeError(e);
		} finally {
			this.skillBusy = false;
		}
	}

	/** Deletes a skill by name; records a revision. */
	async deleteSkill(name: string): Promise<void> {
		if (this.#client === null || !this.hasAdminScope) return;
		this.skillsError = null;
		try {
			await this.#client.agentConfig.skillsDelete(this.agentId, name);
			await this.reloadSkills();
			await this.reloadRevisions();
		} catch (e) {
			this.skillsError = describeError(e);
		}
	}

	private async reloadSkills(): Promise<void> {
		if (this.#client === null) return;
		const resp = await this.#client.agentConfig.skillsList(this.agentId);
		this.skills = resp.skills;
	}

	private async reloadRevisions(): Promise<void> {
		if (this.#client === null) return;
		const resp = await this.#client.agentConfig.listRevisions(this.agentId);
		this.revisions = resp.revisions;
		// Every caller of reloadRevisions just RECORDED a new revision (a per-area
		// save / skill upsert / connection add), which the registry makes the
		// active pointer. listRevisions is newest-first, so revisions[0] is that
		// new active revision — keep activeRevisionId in lockstep so a later
		// Save-all bases its whole-envelope merge on the TRUE active payload (not
		// a stale pre-write one, which would DROP a just-added skill/connection),
		// and so the active badge / rollback baseline / diff direction are
		// correct. (A rollback REPOINTS to an existing revision without adding a
		// row, so it goes through the full reload() — which re-reads the active
		// pointer via agent_config.get — not this path.)
		this.activeRevisionId = resp.revisions[0]?.revision_id ?? this.activeRevisionId;
	}

	/* ================================================================ */
	/* MCP pause/resume + per-tool disable (92d)                         */
	/* ================================================================ */

	/** Toggles a server's paused state in the desired-state set (no write
	 * until {@link saveExposure}). */
	toggleServerPaused(server: string): void {
		this.exposureSaved = false;
		this.exposureDirty = true;
		this.saveAllSaved = false;
		this.pausedServers = this.pausedServers.includes(server)
			? this.pausedServers.filter((s) => s !== server)
			: [...this.pausedServers, server];
	}

	/** Toggles a per-tool disable in the desired-state set. Tools are keyed
	 * `<source>_<tool>` (the exposure wire convention). */
	toggleToolDisabled(toolKey: string): void {
		this.exposureSaved = false;
		this.exposureDirty = true;
		this.saveAllSaved = false;
		this.disabledTools = this.disabledTools.includes(toolKey)
			? this.disabledTools.filter((t) => t !== toolKey)
			: [...this.disabledTools, toolKey];
	}

	/** Persists the desired-state tool-exposure (replaces ONLY the
	 * exposure section); records a revision + emits `mcp.connection.*`. */
	async saveExposure(): Promise<void> {
		if (this.#client === null || !this.hasAdminScope) return;
		this.exposurePhase = 'saving';
		this.exposureError = null;
		this.exposureSaved = false;
		try {
			await this.#client.agentConfig.setToolExposure(this.agentId, {
				paused_servers: this.pausedServers,
				disabled_tools: this.disabledTools
			});
			this.exposureSaved = true;
			this.exposureDirty = false;
			this.exposurePhase = 'ready';
			await this.reloadRevisions();
		} catch (e) {
			this.exposureError = describeError(e);
			this.exposurePhase = 'error';
		}
	}

	/* ================================================================ */
	/* Layered prompt (92e)                                              */
	/* ================================================================ */

	/** Marks the prompt form edited (clears the saved confirmation + stages
	 * the prompt area for Save-all). */
	markPromptEdited(): void {
		this.promptSaved = false;
		this.promptDirty = true;
		this.saveAllSaved = false;
	}

	/** Persists the operator base layer (and the user layer, read-only in
	 * the operator UI but preserved); records a revision. */
	async savePrompt(): Promise<void> {
		if (this.#client === null || !this.hasAdminScope) return;
		this.promptPhase = 'saving';
		this.promptError = null;
		this.promptSaved = false;
		try {
			await this.#client.agentConfig.setPromptLayers(this.agentId, {
				base: this.promptBase,
				user: this.promptUser
			});
			this.promptSaved = true;
			this.promptDirty = false;
			this.promptPhase = 'ready';
			await this.reloadRevisions();
		} catch (e) {
			this.promptError = describeError(e);
			this.promptPhase = 'error';
		}
	}

	/* ================================================================ */
	/* Model & sampling — per-agent LLM params (92j/92i)                 */
	/* ================================================================ */

	/** Marks the model-&-sampling form edited (stages the area for Save-all). */
	markLlmEdited(): void {
		this.llmSaved = false;
		this.llmDirty = true;
		this.saveAllSaved = false;
	}

	/**
	 * buildLLMParams projects the model-&-sampling form fields onto the wire
	 * shape. An empty field is OMITTED (inherit the next layer — the
	 * tenant-wide baseline, then config). The numeric fields bind to number
	 * inputs (so they are already `number | null`); the sampling RANGES are
	 * validated server-side at set time (ErrInvalidLLMParams → the inline
	 * error), so the client does not re-validate. An all-empty form returns
	 * `undefined`.
	 */
	private buildLLMParams(): AgentConfigLLMParams | undefined {
		const params: AgentConfigLLMParams = {};
		const model = this.llmModel.trim();
		if (model !== '') params.model = model;
		const re = this.llmReasoningEffort.trim();
		if (re !== '') params.reasoning_effort = re;
		if (this.llmTemperature !== null) params.temperature = this.llmTemperature;
		if (this.llmMaxTokens !== null) params.max_tokens = this.llmMaxTokens;
		return Object.keys(params).length > 0 ? params : undefined;
	}

	/** Persists the model-&-sampling section alone (`set_llm_params`); records
	 * a revision. A set model + the sampling ranges are validated server-side
	 * at set time (the rejection surfaces inline — never silently passed
	 * through). */
	async saveLlmParams(): Promise<void> {
		if (this.#client === null || !this.hasAdminScope) return;
		this.llmPhase = 'saving';
		this.llmError = null;
		this.llmSaved = false;
		try {
			await this.#client.agentConfig.setLlmParams(this.agentId, this.buildLLMParams() ?? {});
			this.llmSaved = true;
			this.llmDirty = false;
			this.llmPhase = 'ready';
			await this.reloadRevisions();
		} catch (e) {
			this.llmError = describeError(e);
			this.llmPhase = 'error';
		}
	}

	/* ================================================================ */
	/* Atomic multi-section save — "Save all" (92i)                      */
	/* ================================================================ */

	/** The active revision's payload (or {} when none) — the base the
	 * Save-all merge carries the non-form sections (skills / connections)
	 * forward from. */
	private activePayload(): AgentConfigPayload {
		const active = this.revisions.find((r) => r.revision_id === this.activeRevisionId);
		return active?.payload ?? {};
	}

	/**
	 * saveAll commits the staged form edits across every area as ONE
	 * `set_revision` (the full merged payload) — one revision, one
	 * `agent.config.revised` event, one diffable unit (the operator's "change
	 * prompt + temperature + model in one revision"). `set_revision` is a
	 * whole-envelope replace, so the merged payload is built from the CURRENT
	 * form values for the three form sections (prompt / model-&-sampling / MCP
	 * exposure) PLUS the non-form sections (skills / connections) carried
	 * forward from the active revision — so unedited sections are preserved.
	 * A failed write keeps the staged edits + surfaces the error (no silent
	 * drop — §13); a confirmed success re-seeds the forms (clearing dirty).
	 */
	async saveAll(): Promise<void> {
		if (this.#client === null || !this.hasAdminScope) return;
		if (!this.hasStagedChanges) return;
		const base = this.activePayload();
		const payload: AgentConfigPayload = {
			skills: base.skills,
			connections: base.connections
		};
		if (this.promptBase !== '' || this.promptUser !== '') {
			payload.prompt_layers = {
				base: this.promptBase || undefined,
				user: this.promptUser || undefined
			};
		}
		if (this.pausedServers.length > 0 || this.disabledTools.length > 0) {
			payload.tool_exposure = {
				paused_servers: this.pausedServers,
				disabled_tools: this.disabledTools
			};
		}
		const llm = this.buildLLMParams();
		if (llm !== undefined) {
			payload.llm_params = llm;
		}
		this.saveAllPhase = 'saving';
		this.saveAllError = null;
		this.saveAllSaved = false;
		try {
			await this.#client.agentConfig.setRevision(this.agentId, payload);
			this.saveAllSaved = true;
			this.saveAllPhase = 'ready';
			// Re-seed the forms + clear every dirty flag from the new active
			// revision (the next-turn projection — D-025).
			await this.reload();
		} catch (e) {
			this.saveAllError = describeError(e);
			this.saveAllPhase = 'error';
		}
	}

	/** discardStaged drops all staged form edits, re-seeding the forms from
	 * the active revision (no write). */
	discardStaged(): void {
		const active = this.revisions.find((r) => r.revision_id === this.activeRevisionId) ?? null;
		this.seedFromRevision(active);
		this.saveAllSaved = false;
		this.saveAllError = null;
		this.saveAllPhase = 'idle';
		this.promptSaved = false;
		this.exposureSaved = false;
		this.llmSaved = false;
	}

	/* ================================================================ */
	/* Add MCP connection (92f)                                          */
	/* ================================================================ */

	/** Parses the `Key: value` header lines into a header map. The values
	 * are sent for the live attach only and are NEVER persisted (D-235). */
	private parseHeaders(): Record<string, string> | undefined {
		const out: Record<string, string> = {};
		for (const line of this.connHeaders.split('\n')) {
			const idx = line.indexOf(':');
			if (idx <= 0) continue;
			const key = line.slice(0, idx).trim();
			const value = line.slice(idx + 1).trim();
			if (key !== '') out[key] = value;
		}
		return Object.keys(out).length > 0 ? out : undefined;
	}

	/** Adds a new MCP server connection; surfaces the terminal attach
	 * state ("online" | "failed" | "auth_required") and reason. The secret
	 * headers are cleared from the form on submit (write-only). */
	async addConnection(): Promise<void> {
		if (this.#client === null || !this.hasAdminScope) return;
		if (this.addConnBusy) return;
		if (this.connName.trim() === '') {
			this.addConnError = { code: 'invalid_input', message: 'A connection name is required.' };
			return;
		}
		this.addConnBusy = true;
		this.addConnError = null;
		this.addConnState = null;
		this.addConnReason = null;
		const descriptor: AgentConfigMCPConnectionDescriptor = {
			name: this.connName.trim(),
			transport: this.connTransport,
			command:
				this.connTransport === 'stdio'
					? this.connCommand
							.split(/\s+/)
							.map((s) => s.trim())
							.filter((s) => s.length > 0)
					: undefined,
			url: this.connTransport === 'http' ? this.connUrl.trim() || undefined : undefined
		};
		const headers = this.parseHeaders();
		try {
			const resp = await this.#client.agentConfig.addMcpConnection(
				this.agentId,
				descriptor,
				headers
			);
			this.addConnState = resp.state;
			this.addConnReason = resp.reason ?? null;
			// Drop the secret header material from the form immediately.
			this.connHeaders = '';
			if (resp.state === 'online' || resp.state === 'auth_required') {
				await this.reloadRevisions();
			}
		} catch (e) {
			this.addConnError = describeError(e);
		} finally {
			this.addConnBusy = false;
		}
	}
}
