import { flushSync, mount, unmount } from 'svelte';
import { expect, test } from 'vitest';
import DiffView from './DiffView.svelte';

test('shows a guidance-only revision diff and identifies inherited guidance', async () => {
  const target = document.createElement('div');
  const component = mount(DiffView, {
    target,
    props: {
      diff: {
        from_revision_id: 'before',
        to_revision_id: 'after',
        skills: {},
        tool_exposure: {},
        connections: {},
        oauth_providers: {},
        hooks: {
          section_present_changed: false,
          run_completion_tool_changed: false,
          run_completion_timeout_changed: false
        },
        naming: {
          auto_changed: false,
          after_turns_changed: false,
          repeat_every_changed: false,
          max_repetitions_changed: false,
          max_title_len_changed: false,
          model_changed: false,
          reasoning_mode_changed: false
        },
        extra_system_blocks: { reordered: false },
        llm_params: {
          model_changed: false,
          temperature_changed: false,
          max_tokens_changed: false,
          reasoning_effort_changed: false
        },
        prompt_layers: {
          base_changed: false,
          user_changed: false,
          guidance_changed: true,
          guidance_from: 'Use the runbook.'
        }
      }
    }
  });

  try {
    flushSync();
    const promptDiff = target.querySelector('[data-testid="agentcfg-diff-prompt"]');
    expect(promptDiff?.querySelector('.delta-label')?.textContent).toBe('Operator guidance');
    expect(promptDiff?.querySelector('.from')?.textContent).toBe('Use the runbook.');
    expect(promptDiff?.querySelector('.to')?.textContent).toBe('(inherit)');
    expect(target.querySelector('[data-testid="agentcfg-diff-empty"]')).toBeNull();
  } finally {
    await unmount(component);
  }
});
