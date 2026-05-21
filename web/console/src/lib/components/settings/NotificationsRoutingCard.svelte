<script lang="ts">
  // Settings — Notifications Routing card (Phase 73m / D-129).
  //
  // Console-local. Reflects 72h's `notifications_routing` table — the
  // per-operator matrix mapping `notification.*` event classes (72d's
  // topic) onto delivery transports. V1 wires the `in_app` transport
  // only; `email` / `webhook` / `web_push` render disabled-with-tooltip
  // ("Transport delivery is post-V1" — page-settings.md §10). Email +
  // webhook are admin-elevation transports (the runtime never accepts a
  // webhook fan-out from a non-admin operator).
  import type { NotificationRoutingRow } from '$lib/db/index.js';

  let {
    routing,
    hasAdminScope
  }: {
    routing: NotificationRoutingRow[];
    hasAdminScope: boolean;
  } = $props();

  const CLASSES = [
    'governance_budget_exceeded',
    'tool_auth_required',
    'tool_approval_required',
    'task_failed',
    'agent_credentials_expired',
    'runtime_health_degraded'
  ] as const;

  const TRANSPORTS = [
    { id: 'in_app', label: 'In-app', deliverable: true, adminOnly: false },
    { id: 'email', label: 'Email', deliverable: false, adminOnly: true },
    { id: 'webhook', label: 'Webhook', deliverable: false, adminOnly: true },
    { id: 'web_push', label: 'Web push', deliverable: false, adminOnly: false }
  ] as const;

  function enabled(cls: string, transport: string): boolean {
    return routing.some(
      (r) => r.notification_class === cls && r.transport === transport && r.enabled === 1
    );
  }
</script>

<div class="card-body" data-testid="settings-notifications-routing">
  <table class="routing-matrix">
    <thead>
      <tr>
        <th>Notification class</th>
        {#each TRANSPORTS as t (t.id)}
          <th>{t.label}</th>
        {/each}
      </tr>
    </thead>
    <tbody>
      {#each CLASSES as cls (cls)}
        <tr data-testid="routing-row">
          <td class="cls">{cls}</td>
          {#each TRANSPORTS as t (t.id)}
            {@const blocked = !t.deliverable || (t.adminOnly && !hasAdminScope)}
            <td>
              <span
                class="cell"
                class:on={enabled(cls, t.id)}
                class:blocked
                title={!t.deliverable
                  ? 'Transport delivery is post-V1'
                  : t.adminOnly && !hasAdminScope
                    ? 'Email / webhook routing requires the admin scope claim'
                    : ''}
                data-testid="routing-cell-{cls}-{t.id}"
              >
                {enabled(cls, t.id) ? 'on' : '—'}
              </span>
            </td>
          {/each}
        </tr>
      {/each}
    </tbody>
  </table>
  <p class="note">
    V1 delivers the in-app transport only. Email / webhook / web-push rows are
    forward-compat — delivery lands post-V1.
  </p>
</div>

<style>
  .card-body {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    font-size: var(--text-sm);
  }
  .routing-matrix {
    width: 100%;
    border-collapse: collapse;
  }
  th {
    text-align: left;
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    padding-bottom: var(--space-2);
  }
  td {
    padding: var(--space-1) var(--space-2) var(--space-1) var(--space-0);
    color: var(--color-text);
  }
  .cls {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }
  .cell {
    display: inline-block;
    min-width: var(--space-8);
    text-align: center;
    padding: var(--space-0) var(--space-1);
    border-radius: var(--radius-sm);
    color: var(--color-text-muted);
  }
  .cell.on {
    background: var(--color-success-soft);
    color: var(--color-success);
  }
  .cell.blocked {
    opacity: 0.5;
    cursor: not-allowed;
  }
  .note {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
</style>
