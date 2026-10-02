/** Exact-task text-input receipt shapes, mirroring the canonical Go wire types. */
import type { IdentityScope } from './memory-types.js';

export interface ControlReceipt {
	event_id: string;
	task_id: string;
	input_revision: number;
	/** accepted | applied | declined | terminal; unknown future values are not success. */
	status: string;
	reason?: string;
	accepted_at?: number;
	applied_at?: number;
	terminal_at?: number;
}

export interface ControlReceiptRequest {
	identity: IdentityScope;
	event_id: string;
}

export interface ControlReceiptResponse {
	receipt: ControlReceipt;
	protocol_version: string;
}

export interface ControlResponse {
	accepted: boolean;
	method: string;
	protocol_version: string;
	receipt?: ControlReceipt;
}
