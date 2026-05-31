/**
 * EventsSubscription SSE-ingest tests (Phase 108c regression).
 *
 * The Harbor Runtime emits NAMED SSE frames (`event: <type>`); the browser
 * `EventSource.onmessage` fires ONLY for unnamed `message` frames. A regression
 * where the subscription set only `onmessage` meant the stream connected (200)
 * but no event was ever ingested — the Overview cost/activity panels + the
 * Events page stayed silently empty. These tests pin that named-event frames
 * (the real wire shape) ARE ingested, and that unnamed frames still work.
 */
import { describe, expect, it } from 'vitest';
import { EventsSubscription, type EventSourceLike } from '../subscription.svelte.js';
import type { EventsNamespace } from '$lib/protocol/harbor.js';
import type { Event } from '$lib/protocol/events.js';

/** A controllable EventSource stub: records named listeners + exposes dispatch. */
class FakeSource implements EventSourceLike {
	onopen: ((this: unknown, ev: unknown) => void) | null = null;
	onerror: ((this: unknown, ev: unknown) => void) | null = null;
	onmessage: ((this: unknown, ev: { data: string }) => void) | null = null;
	readonly named = new Map<string, (ev: { data: string }) => void>();
	closed = false;
	addEventListener(type: string, handler: (ev: { data: string }) => void): void {
		this.named.set(type, handler);
	}
	close(): void {
		this.closed = true;
	}
	/** Simulate a NAMED SSE frame (`event: type` + data). */
	emitNamed(type: string, ev: Event): void {
		this.named.get(type)?.({ data: JSON.stringify(ev) });
	}
}

function frame(type: string, sequence: number): Event {
	return {
		type,
		sequence,
		occurred_at: '2026-05-31T12:00:00Z',
		tenant: 'dev',
		user: 'dev',
		session: 'dev'
	} as Event;
}

// Minimal EventsNamespace stub — open() only needs subscribeURL.
const ns = {
	subscribeURL: () => 'http://127.0.0.1:18080/v1/events?type=task.completed'
} as unknown as EventsNamespace;

describe('EventsSubscription named-event ingest (108c regression)', () => {
	it('ingests NAMED SSE frames for each subscribed type', () => {
		let made: FakeSource | null = null;
		const sub = new EventsSubscription(ns, () => {
			made = new FakeSource();
			return made;
		});
		sub.open({ eventTypes: ['task.completed', 'llm.cost.recorded'] });
		expect(made).not.toBeNull();

		made!.onopen?.call(null, {});
		expect(sub.state).toBe('open');

		// Named frames (the real Runtime wire shape) must be ingested.
		made!.emitNamed('task.completed', frame('task.completed', 1));
		made!.emitNamed('llm.cost.recorded', frame('llm.cost.recorded', 2));

		expect(sub.events).toHaveLength(2);
		expect(sub.events[0].type).toBe('llm.cost.recorded'); // newest-first
		expect(sub.events[1].type).toBe('task.completed');
	});

	it('still ingests an unnamed `message` frame (test-factory path)', () => {
		let made: FakeSource | null = null;
		const sub = new EventsSubscription(ns, () => {
			made = new FakeSource();
			return made;
		});
		sub.open({ eventTypes: ['task.completed'] });
		made!.onmessage?.call(null, { data: JSON.stringify(frame('task.completed', 7)) });
		expect(sub.events).toHaveLength(1);
		expect(sub.events[0].sequence).toBe(7);
	});

	it('does not register named listeners when no eventTypes are given', () => {
		let made: FakeSource | null = null;
		const sub = new EventsSubscription(ns, () => {
			made = new FakeSource();
			return made;
		});
		sub.open({});
		expect(made!.named.size).toBe(0);
	});
});
