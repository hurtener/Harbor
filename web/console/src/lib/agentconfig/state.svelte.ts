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
	AgentConfigMCPConnectionDescriptor
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
	/** Confirmation gate for rollback — overridable in tests. */
	confirmRollback: (revisionId: string) => boolean = (revisionId) =>
		globalThis.confirm?.(`Roll the active config back to revision ${revisionId}?`) ?? false;

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

		// Open the live mcp.connection.* advisory stream once per load.
		this.subscription?.close();
		const sub = new EventsSubscription(this.#client.events);
		sub.open({ eventTypes: MCP_CONNECTION_EVENT_TYPES as unknown as string[] });
		this.subscription = sub;

		try {
			const [get, list, skills] = await Promise.all([
				this.#client.agentConfig.get(this.agentId),
				this.#client.agentConfig.listRevisions(this.agentId),
				this.#client.agentConfig.skillsList(this.agentId)
			]);
			this.revisions = list.revisions;
			this.activeRevisionId = get.revision?.revision_id ?? '';
			this.seedFromRevision(get.revision ?? null);
			this.skills = skills.skills;
			this.skillsPhase = 'ready';
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

	/** Seeds the prompt + exposure form fields from an active revision. */
	private seedFromRevision(rev: AgentConfigRevisionView | null): void {
		const payload = rev?.payload;
		this.promptBase = payload?.prompt_layers?.base ?? '';
		this.promptUser = payload?.prompt_layers?.user ?? '';
		this.pausedServers = [...(payload?.tool_exposure?.paused_servers ?? [])];
		this.disabledTools = [...(payload?.tool_exposure?.disabled_tools ?? [])];
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

	/** Rolls the active pointer back to a revision (admin-gated; confirmed). */
	async rollback(revisionId: string): Promise<void> {
		if (this.#client === null || !this.hasAdminScope) return;
		if (this.rollbackBusy !== null) return;
		if (!this.confirmRollback(revisionId)) return;
		this.rollbackBusy = revisionId;
		this.rollbackError = null;
		this.rolledBackTo = null;
		try {
			await this.#client.agentConfig.rollback(this.agentId, revisionId);
			this.rolledBackTo = revisionId;
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
	}

	/* ================================================================ */
	/* MCP pause/resume + per-tool disable (92d)                         */
	/* ================================================================ */

	/** Toggles a server's paused state in the desired-state set (no write
	 * until {@link saveExposure}). */
	toggleServerPaused(server: string): void {
		this.exposureSaved = false;
		this.pausedServers = this.pausedServers.includes(server)
			? this.pausedServers.filter((s) => s !== server)
			: [...this.pausedServers, server];
	}

	/** Toggles a per-tool disable in the desired-state set. Tools are keyed
	 * `<source>_<tool>` (the exposure wire convention). */
	toggleToolDisabled(toolKey: string): void {
		this.exposureSaved = false;
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

	/** Marks the prompt form edited (clears the saved confirmation). */
	markPromptEdited(): void {
		this.promptSaved = false;
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
			this.promptPhase = 'ready';
			await this.reloadRevisions();
		} catch (e) {
			this.promptError = describeError(e);
			this.promptPhase = 'error';
		}
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
