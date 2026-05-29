<script lang="ts">
  // Settings — Connected Runtimes card (Phase 73m / D-129).
  //
  // The operator's address book of Harbor runtimes the Console knows
  // about (72h's `runtime_registry` Console DB table — D-061: the
  // runtime is unaware it is in this list). The Settings page is the
  // first page where this card has visible meaning — which is why the
  // `harbor console` subcommand (D-091) bundles into this phase.
  //
  // The `+ Add Runtime` action writes a real row through the Console DB
  // driver — never a hand-rolled fetch (CONVENTIONS.md §6). It is NOT a
  // stubbed action (CONVENTIONS.md §5).
  import type { RuntimeRegistryRow } from '$lib/db/index.js';
  import { validateAddRuntimeDraft } from './add-runtime-form.js';

  let {
    runtimes,
    addWarning = null,
    onadd,
    onremove,
    onaddsuccess
  }: {
    runtimes: RuntimeRegistryRow[];
    /**
     * Non-fatal warning surfaced by `addRuntime` when the active-connection
     * write landed in `localStorage` but the address-book write was
     * deferred (Phase 83u / D-163 — the F3 first-attach path). Rendered
     * as a neutral info banner, NEVER a red error.
     */
    addWarning?: string | null;
    /** Writes a new runtime connection. */
    onadd: (
      name: string,
      baseURL: string,
      token: string,
      identity: { tenant: string; user: string; session: string },
      scopes?: string[]
    ) => Promise<void>;
    /** Deletes a runtime row from the Console DB. */
    onremove: (id: string) => Promise<void>;
    /**
     * Fires after a successful Add submit. The Settings page wires this
     * to `window.location.reload()` so the new connection takes effect
     * AND the Console DB opens on the reloaded page, enabling the
     * address-book catch-up (Phase 83u / D-163).
     */
    onaddsuccess?: () => void;
  } = $props();

  let formOpen = $state(false);
  let draftName = $state('');
  let draftURL = $state('');
  let draftToken = $state('');
  let draftTenant = $state('');
  let draftUser = $state('');
  let draftSession = $state('');
  let busy = $state(false);
  let formError = $state<string | null>(null);

  async function submitAdd(): Promise<void> {
    formError = null;
    const validationError = validateAddRuntimeDraft({
      name: draftName,
      url: draftURL,
      token: draftToken,
      tenant: draftTenant,
      user: draftUser,
      session: draftSession
    });
    if (validationError !== null) {
      formError = validationError;
      return;
    }
    busy = true;
    try {
      await onadd(
        draftName.trim(),
        draftURL.trim(),
        draftToken.trim(),
        { tenant: draftTenant.trim(), user: draftUser.trim(), session: draftSession.trim() }
      );
      draftName = '';
      draftURL = '';
      draftToken = '';
      draftTenant = '';
      draftUser = '';
      draftSession = '';
      formOpen = false;
      // Notify the page so it can reload — the new connection only
      // takes effect on the next page mount (every page reads
      // resolveConnection() once via HarborClient — CONVENTIONS.md §6).
      onaddsuccess?.();
    } catch (e) {
      formError = e instanceof Error ? e.message : 'Could not add the runtime.';
    } finally {
      busy = false;
    }
  }
</script>

<div class="card-body" data-testid="settings-connected-runtimes">
  {#if runtimes.length === 0}
    <p class="note" data-testid="connected-runtimes-empty">
      No runtimes attached. Add your first runtime to point the Console at a
      Harbor Runtime.
    </p>
  {:else}
    <ul class="runtime-list">
      {#each runtimes as rt (rt.id)}
        <li class="runtime-row" data-testid="connected-runtime-row">
          <div class="runtime-meta">
            <span class="runtime-name">{rt.name}</span>
            <span class="runtime-url">{rt.base_url}</span>
          </div>
          <div class="runtime-tags">
            {#if rt.is_default === 1}
              <span class="tag">default</span>
            {/if}
            <button
              type="button"
              class="row-action"
              data-testid="remove-runtime"
              onclick={() => void onremove(rt.id)}
            >
              Remove
            </button>
          </div>
        </li>
      {/each}
    </ul>
  {/if}

  {#if addWarning !== null}
    <p class="info-banner" data-testid="add-runtime-warning">{addWarning}</p>
  {/if}

  {#if formOpen}
    <form class="add-form" data-testid="add-runtime-form" onsubmit={(e) => { e.preventDefault(); void submitAdd(); }}>
      <input
        type="text"
        placeholder="Runtime name"
        data-testid="add-runtime-name"
        bind:value={draftName}
      />
      <input
        type="url"
        placeholder="https://runtime.example.com"
        data-testid="add-runtime-url"
        bind:value={draftURL}
      />
      <textarea
        placeholder="Bearer token (JWT)"
        data-testid="add-runtime-token"
        rows={3}
        bind:value={draftToken}
      ></textarea>
      <input
        type="text"
        placeholder="Tenant ID"
        data-testid="add-runtime-tenant"
        bind:value={draftTenant}
      />
      <input
        type="text"
        placeholder="User ID"
        data-testid="add-runtime-user"
        bind:value={draftUser}
      />
      <input
        type="text"
        placeholder="Session ID"
        data-testid="add-runtime-session"
        bind:value={draftSession}
      />
      {#if formError}
        <p class="form-error" data-testid="add-runtime-error">{formError}</p>
      {/if}
      <div class="form-actions">
        <button type="submit" class="primary" data-testid="add-runtime-submit" disabled={busy}>
          {busy ? 'Adding…' : 'Add'}
        </button>
        <button type="button" class="row-action" onclick={() => (formOpen = false)}>
          Cancel
        </button>
      </div>
    </form>
  {:else}
    <button
      type="button"
      class="primary"
      data-testid="add-runtime-open"
      onclick={() => (formOpen = true)}
    >
      + Add Runtime
    </button>
  {/if}
</div>

<style>
  .card-body {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    font-size: var(--text-sm);
  }
  .runtime-list {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .runtime-row {
    display: flex;
    justify-content: space-between;
    align-items: center;
    padding: var(--space-2) var(--space-3);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    background: var(--color-surface);
  }
  .runtime-meta {
    display: flex;
    flex-direction: column;
  }
  .runtime-name {
    color: var(--color-text);
    font-weight: 600;
  }
  .runtime-url {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }
  .runtime-tags {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .tag {
    font-size: var(--text-xs);
    color: var(--color-accent);
  }
  .add-form {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  input {
    padding: var(--space-2);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    background: var(--color-surface);
    color: var(--color-text);
    font-size: var(--text-sm);
  }
  .form-actions {
    display: flex;
    gap: var(--space-2);
  }
  .primary,
  .row-action {
    padding: var(--space-1) var(--space-3);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    font-size: var(--text-sm);
    cursor: pointer;
  }
  .primary {
    background: var(--color-accent);
    border-color: var(--color-accent);
    color: var(--color-bg);
  }
  .primary:disabled {
    opacity: 0.6;
    cursor: not-allowed;
  }
  .row-action {
    background: var(--color-surface);
    color: var(--color-text-muted);
  }
  .form-error {
    color: var(--color-danger);
    font-size: var(--text-xs);
    margin: var(--space-0);
  }
  .note {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
  .info-banner {
    margin: var(--space-0);
    padding: var(--space-2) var(--space-3);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    background: var(--color-surface-raised);
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }
</style>
