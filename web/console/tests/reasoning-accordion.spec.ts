/**
 * Phase 107a — reasoning-accordion Playwright spec.
 *
 * Asserts: against a real LLM-backed `harbor dev`, sending a prompt that
 * elicits reasoning steps renders a "Reasoning (N steps)" toggle in the
 * agent bubble, and clicking it expands the reasoning list.
 *
 * SKIPs cleanly when no LLM provider key is set — the skip message names
 * the missing env var so operators + CI know why it didn't run.
 */
import { test, expect, consoleSubcommandAvailable } from "./fixtures/page";

const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

test.describe('Reasoning accordion', () => {
	test.skip(
		!CONSOLE_AVAILABLE,
		'SKIP: harbor console subcommand not available (cannot boot the dev stack)'
	);

	test('renders reasoning toggle after completing a multi-step task', async ({ page }) => {
		test.skip(
			!process.env.OPENROUTER_API_KEY && !process.env.ANTHROPIC_API_KEY,
			'SKIP: no LLM provider key (set OPENROUTER_API_KEY or ANTHROPIC_API_KEY)'
		);

		// Navigate to the Playground — the console subcommand fixture
		// sets the correct baseURL and handles the first-attach flow.
		await page.goto('/playground');

		// Wait for the Playground to finish loading.
		await page.waitForSelector('[data-testid="chat-composer-input"]', { timeout: 15000 });

		// Send a prompt that SHOULD elicit reasoning.
		const input = page.locator('[data-testid="chat-composer-input"]');
		await input.fill('List three prime numbers over 100 and explain how you verified each.');
		await page.locator('[data-testid="chat-composer-send"]').click();

		// The pending bubble appears.
		await page.waitForSelector('[data-testid="chat-message-bubble"][data-role="agent"]', {
			timeout: 5000
		});

		// Wait for the task to complete (the bubble's text becomes non-empty).
		await expect
			.poll(
				async () => {
					const bubbles = page.locator(
						'[data-testid="chat-message-bubble"][data-role="agent"]'
					);
					const last = bubbles.last();
					const text = await last.locator('.bubble-text').textContent();
					return text?.length ?? 0;
				},
				{ timeout: 120000, intervals: [2000] }
			)
			.toBeGreaterThan(10);

		// Assert the reasoning accordion toggle is visible.
		const accordion = page.locator('[data-testid="reasoning-accordion"]');
		await expect(accordion).toBeVisible({ timeout: 5000 });

		const toggle = accordion.locator('.reasoning-toggle');
		await expect(toggle).toBeVisible();

		// The toggle label shows at least 1 step.
		const toggleText = await toggle.textContent();
		expect(toggleText).toMatch(/Reasoning \(\d+ steps?\)/);

		// Click to expand.
		await toggle.click();
		await expect(accordion.locator('.reasoning-list')).toBeVisible();

		// Assert at least one trace string is non-empty.
		const traces = accordion.locator('.step-trace');
		const count = await traces.count();
		expect(count).toBeGreaterThan(0);
		const firstTrace = await traces.first().textContent();
		expect(firstTrace?.length).toBeGreaterThan(0);
	});

	test('does not crash for traces-less runs', async ({ page }) => {
		test.skip(
			!process.env.OPENROUTER_API_KEY && !process.env.ANTHROPIC_API_KEY,
			'SKIP: no LLM provider key'
		);

		await page.goto('/playground');
		await page.waitForSelector('[data-testid="chat-composer-input"]', { timeout: 15000 });

		const input = page.locator('[data-testid="chat-composer-input"]');
		await input.fill('What is 2+2? Reply with just the number.');
		await page.locator('[data-testid="chat-composer-send"]').click();

		await page.waitForSelector('[data-testid="chat-message-bubble"][data-role="agent"]', {
			timeout: 5000
		});

		await expect
			.poll(
				async () => {
					const bubbles = page.locator(
						'[data-testid="chat-message-bubble"][data-role="agent"]'
					);
					const last = bubbles.last();
					const text = await last.locator('.bubble-text').textContent();
					return text?.length ?? 0;
				},
				{ timeout: 60000, intervals: [2000] }
			)
			.toBeGreaterThan(0);

		// The page didn't crash — that's the assertion.
		await expect(page.locator('body')).toBeVisible();
	});
});
