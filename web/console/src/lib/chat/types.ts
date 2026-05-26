// Harbor Console — the shared chat module's public types (Phase 73n /
// D-130, D-091).
//
// # The encapsulation contract (CLAUDE.md §4.5 #11)
//
// This file — and every file under `$lib/chat/` — imports NOTHING from
// outside the chat module. The chat module is self-contained so that
// the future `git mv $lib/chat → web/shared/chat` (when the packed
// `harbor dev` UI becomes the second consumer) is mechanical.
//
// The chat module does NOT import `$lib/protocol`, `$lib/connection`,
// `$lib/components/ui`, or any other Console internal. Instead it
// declares its OWN minimal, injected `ChatProtocolClient` interface
// here; the CALLER (the Playground page) adapts the Console's
// `HarborClient` onto this interface and injects it. The chat module
// never reaches for a Console-specific singleton.
//
// The renderer registry the chat module's bubbles dispatch through is
// the Phase 73l canonical registry at `$lib/chat/renderers/` — which IS
// inside the chat module, so importing it is allowed. Phase 73n EXTENDS
// that registry with chat-bubble / tool-call / diff renderers; it does
// not fork the dispatch core.

/** The role a chat message was authored by. */
export type ChatRole = 'user' | 'agent' | 'system';

/**
 * An artifact reference carried by a chat message. Heavy content always
 * flows by reference (D-026) — a chat bubble NEVER carries inline heavy
 * bytes. The `src` is a resolved presigned URL the renderer fetches
 * from; the chat module obtains it via {@link ChatProtocolClient.resolveArtifact}.
 */
export interface ChatArtifactRef {
	/** The runtime artifact id. */
	id: string;
	/** The artifact's IANA media type — drives renderer dispatch. */
	mime: string;
	/** A human-facing display name. */
	filename: string;
	/** The artifact's size in bytes, when known (for the by-reference UI). */
	sizeBytes?: number;
}

/** A tool-call trace entry surfaced inside an agent message. */
export interface ChatToolCall {
	/** The tool name the agent invoked. */
	tool: string;
	/** The tool-call status. */
	status: 'invoked' | 'succeeded' | 'failed';
	/** A short, redacted summary of the call (never raw arguments — CLAUDE.md §7). */
	summary: string;
	/** The run id the tool call belongs to, when known. */
	runID?: string;
}

/** A unified-diff hunk surfaced inside an agent message. */
export interface ChatDiff {
	/** The path the diff applies to. */
	path: string;
	/** The unified-diff text. */
	patch: string;
}

/** One message in the chat stream. */
export interface ChatMessage {
	/** A stable per-message id (used as the `{#each}` key). */
	id: string;
	/** Who authored the message. */
	role: ChatRole;
	/** The message's primary text body (markdown source). */
	text: string;
	/** The ISO-8601 instant the message was created. */
	at: string;
	/** Tool-call traces attached to an agent message. */
	toolCalls?: ChatToolCall[];
	/** Diff cards attached to an agent message. */
	diffs?: ChatDiff[];
	/** Artifact references attached to the message (by reference — D-026). */
	artifacts?: ChatArtifactRef[];
	/** True while an agent message is still streaming tokens. */
	streaming?: boolean;
	/** The runtime task ID associated with this agent message (Phase 106). */
	taskID?: string;
	/** True while an agent message is pending — the task has been spawned but hasn't completed yet (Phase 106). */
	pending?: boolean;
}

/**
 * The result of sending a message — the runtime task / run id the
 * caller can correlate subsequent events against.
 */
export interface SendMessageResult {
	/** The runtime task id the message started or continued. */
	taskID: string;
}

/**
 * The next-message override the composer's Controls card records. Every
 * field is optional — an absent field leaves the runtime default in
 * place. Mirrors the `runs.set_overrides` Protocol wire shape, but is
 * declared HERE so the chat module imports no Console internal.
 */
export interface ChatOverrides {
	/** The LLM reasoning-effort hint (`low` / `medium` / `high`). */
	reasoningEffort?: string;
	/** The sampling temperature, in [0, 2]. */
	temperature?: number;
	/** The per-message MaxTokens ceiling. */
	maxTokens?: number;
	/** A one-message system-prompt override. */
	systemPromptOverride?: string;
}

/**
 * The injected Protocol-client interface the chat module depends on.
 * The Playground page constructs a concrete adapter over the Console's
 * `HarborClient` and passes it into `<ChatPanel>` — the chat module
 * NEVER constructs a `HarborClient` itself, never reads
 * `connection.ts`, never calls `fetch` against a Protocol route.
 *
 * Every method is identity-scoped on the caller's side (the adapter
 * carries the resolved connection); the chat module treats this as an
 * opaque, already-scoped surface.
 */
/** The mode the operator picked while a run was active. */
export type ChatSendMode = 'queue' | 'steer';

export interface ChatProtocolClient {
	/**
	 * Send a user message into the session.
	 *
	 * - When no run is active (`mode === undefined`): spawn a fresh
	 *   foreground task via the SHIPPED `start` Protocol method.
	 * - When a run is active and `mode === 'steer'`: inject the message
	 *   into the running task via the SHIPPED `user_message` control verb
	 *   (Phase 54). The runtime's run loop picks up the message on its
	 *   next planner turn.
	 * - When a run is active and `mode === 'queue'`: stash the message
	 *   locally and dispatch it via `start` as soon as the current run
	 *   reaches a terminal state. The adapter is responsible for the
	 *   lifecycle subscription that detects terminal-state.
	 */
	sendMessage(
		text: string,
		artifactIDs: string[],
		mode?: ChatSendMode
	): Promise<SendMessageResult>;
	/**
	 * Record a next-message override. Maps onto the `runs.set_overrides`
	 * Protocol method (Phase 73n).
	 */
	setOverrides(overrides: ChatOverrides): Promise<void>;
	/**
	 * Upload a file and return its artifact reference. Maps onto the
	 * `artifacts.put` Protocol method. The returned ref carries the
	 * resolved presigned URL machinery on the adapter side.
	 */
	uploadArtifact(file: File): Promise<ChatArtifactRef>;
	/**
	 * Resolve an artifact id to a presigned URL the renderer fetches
	 * from (D-026). Maps onto `artifacts.get_ref`.
	 */
	resolveArtifact(id: string): Promise<string>;
	/**
	 * Cancel the active run. Maps onto the SHIPPED `cancel` Protocol
	 * method; `hard` toggles a hard cancel.
	 */
	cancelRun(hard: boolean): Promise<void>;
	/**
	 * Restart the session with the same agent + system prompt. Maps onto
	 * the SHIPPED `start` Protocol method.
	 */
	restartRun(): Promise<SendMessageResult>;
	/** Approve a pending HITL intervention. Maps onto SHIPPED `approve`. */
	approveIntervention(runID: string): Promise<void>;
	/** Reject a pending HITL intervention. Maps onto SHIPPED `reject`. */
	rejectIntervention(runID: string): Promise<void>;
}
