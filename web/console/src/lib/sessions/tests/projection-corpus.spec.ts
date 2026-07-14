import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

import type { StateEvent } from '../../protocol/state.js';
import { reduceHistoryTurns } from '../history.js';
import {
	decodeChunk,
	decodeIntervention,
	decodeInterventionClear,
	decodeToolLifecycle
} from '../../../routes/(console)/playground/[session_id]/wire-events.js';

interface ExpectedConversation {
	run_id: string;
	answer: string;
	reasoning: string;
	terminal: boolean;
	tools: Array<{ tool: string; status: string }>;
}

interface CorpusCase {
	name: string;
	events: StateEvent[];
	expected_conversation: ExpectedConversation[] | null;
}

const here = dirname(fileURLToPath(import.meta.url));
const corpusPath = join(here, '../../../../../../internal/tui/testdata/projection/corpus.json');
const corpus = JSON.parse(readFileSync(corpusPath, 'utf8')) as CorpusCase[];

describe('shared canonical projection corpus through production reducers', () => {
	for (const fixture of corpus) {
		if (fixture.expected_conversation === null) continue;
		it(fixture.name, () => {
			const normalized = reduceHistoryTurns(fixture.events).map((turn) => ({
				run_id: turn.runID,
				answer: turn.answer,
				reasoning: turn.reasoning,
				terminal: turn.terminal,
				tools: turn.toolCalls.map((tool) => ({ tool: tool.tool, status: tool.status }))
			}));
			expect(normalized).toEqual(fixture.expected_conversation);
		});
	}

	it('the production live decoders preserve repeated tools and correlate interventions by PauseToken', () => {
		const frames = corpus[0].events.map((event) => JSON.stringify(event));
		const chunkFrames = corpus[0].events
			.filter((event) => event.type === 'llm.completion.chunk')
			.map((event) => JSON.stringify(event));
		const chunks = chunkFrames.map(decodeChunk).filter((event) => event !== null);
		const tools = frames.map(decodeToolLifecycle).filter((event) => event !== null);
		const requests = frames.map(decodeIntervention).filter((event) => event !== null);
		const resolutions = frames.map(decodeInterventionClear).filter((event) => event !== null);

		expect(chunks.map((event) => event.kind)).toEqual(['content', 'reasoning']);
		expect(tools.map((event) => [event.tool, event.kind])).toEqual([
			['weather', 'invoked'],
			['weather', 'completed'],
			['weather', 'invoked'],
			['weather', 'failed']
		]);
		expect(requests.map((event) => event.pauseToken)).toEqual(['pause-approval', 'pause-auth']);
		expect(resolutions.map((event) => event.pauseToken)).toEqual(['pause-approval']);
	});
});
