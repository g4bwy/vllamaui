/**
 * VllmService - Reads engine-wide stats from the vLLM adapter
 *
 * `./vllm/stats` is an endpoint the adapter adds on top of a vLLM upstream.
 * A plain llama.cpp server answers 404, 405 or 501. No reactive state;
 * consumed by vllmStore.
 */

import type { ApiVllmStats } from '$lib/types';
import { apiFetch } from '$lib/utils';

export class VllmService {
	/**
	 * Fetches engine-wide stats.
	 *
	 * @throws ApiError carrying the HTTP status, so the caller can tell a
	 * missing endpoint from a temporary outage
	 */
	static async fetchStats(): Promise<ApiVllmStats> {
		return apiFetch<ApiVllmStats>('./vllm/stats', { authOnly: true });
	}
}
