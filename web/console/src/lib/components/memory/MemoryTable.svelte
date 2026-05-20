<script lang="ts">
  // Memory table — the Memory page's primary surface (Phase 73j /
  // D-118). Renders the `memory.list` rows in the mockup column order.
  //
  // D-065 invariant: there is NO priority column. The Strategy column
  // shows the Phase 24 strategy, never a priority dimension. Svelte 5
  // runes mode (D-092).
  import type { MemoryItem } from '$lib/protocol-memory';

  let {
    items,
    selectedKey,
    checkedKeys,
    onSelect,
    onToggleCheck
  }: {
    items: MemoryItem[];
    selectedKey: string | null;
    checkedKeys: Set<string>;
    onSelect: (key: string) => void;
    onToggleCheck: (key: string) => void;
  } = $props();

  /** Short relative-ish label for a timestamp (no priority surfaced). */
  function shortTime(iso: string): string {
    if (!iso) return '—';
    const d = new Date(iso);
    return Number.isNaN(d.getTime()) ? '—' : d.toISOString().slice(0, 19).replace('T', ' ');
  }

  function ttlLabel(iso: string | undefined): string {
    return iso ? shortTime(iso) : '—';
  }
</script>

<table class="memory-table">
  <thead>
    <tr>
      <th scope="col" class="check"><span class="visually-hidden">Select</span></th>
      <th scope="col">Memory key</th>
      <th scope="col">Strategy</th>
      <th scope="col">Scope</th>
      <th scope="col">Owner</th>
      <th scope="col">Created</th>
      <th scope="col">Last updated</th>
      <th scope="col">TTL / Expires</th>
      <th scope="col">Size</th>
      <th scope="col">Driver</th>
    </tr>
  </thead>
  <tbody>
    {#if items.length === 0}
      <tr>
        <td colspan="10" class="empty">No memory items match this scope.</td>
      </tr>
    {:else}
      {#each items as item (item.key)}
        <tr
          class:selected={item.key === selectedKey}
          onclick={() => onSelect(item.key)}
        >
          <td class="check">
            <input
              type="checkbox"
              checked={checkedKeys.has(item.key)}
              aria-label="Select {item.key}"
              onclick={(e) => e.stopPropagation()}
              onchange={() => onToggleCheck(item.key)}
            />
          </td>
          <td class="key">{item.key}</td>
          <td><span class="chip strategy-{item.strategy}">{item.strategy}</span></td>
          <td><span class="chip">{item.scope}</span></td>
          <td class="owner"
            >{item.identity.user} / {item.identity.session}</td
          >
          <td>{shortTime(item.created_at)}</td>
          <td>{shortTime(item.last_updated_at)}</td>
          <td>{ttlLabel(item.expires_at)}</td>
          <td class="size">
            {item.size_bytes}
            {#if item.heavy_content}
              <span class="heavy" title="Heavy content — value by reference (D-026)"
                >⤓</span
              >
            {/if}
          </td>
          <td><span class="chip driver">{item.driver}</span></td>
        </tr>
      {/each}
    {/if}
  </tbody>
</table>

<style>
  .memory-table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--text-sm);
  }

  thead th {
    text-align: left;
    padding: var(--space-2) var(--space-3);
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    border-bottom: var(--border-width-hairline) solid var(--color-border);
  }

  tbody td {
    padding: var(--space-2) var(--space-3);
    border-bottom: var(--border-width-hairline) solid var(--color-border);
  }

  tbody tr {
    cursor: pointer;
  }

  tbody tr:hover {
    background: var(--color-surface-raised);
  }

  tbody tr.selected {
    background: var(--color-surface-raised);
  }

  .key {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .owner {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .size {
    font-variant-numeric: tabular-nums;
  }

  .heavy {
    color: var(--color-warning);
  }

  .chip {
    font-size: var(--text-xs);
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius-sm);
    background: var(--color-surface-raised);
    border: var(--border-width-hairline) solid var(--color-border);
  }

  .chip.driver {
    font-family: var(--font-mono);
  }

  .chip.strategy-truncation {
    border-color: var(--color-accent);
  }

  .chip.strategy-rolling_summary {
    border-color: var(--color-success);
  }

  .empty {
    text-align: center;
    color: var(--color-text-muted);
    padding: var(--space-8);
  }

  .check {
    width: var(--space-6);
  }

  .visually-hidden {
    position: absolute;
    width: var(--size-px);
    height: var(--size-px);
    overflow: hidden;
    clip-path: inset(50%);
  }
</style>
