/**
 * vllmStore - vLLM engine stats
 *
 * Holds the payload of `./vllm/stats`, an endpoint only the vLLM adapter
 * serves. `supported` is null until the first probe: a plain llama.cpp server
 * has no such route, and then every vLLM block in the UI stays hidden.
 */

import { ApiError } from '$lib/utils';
import { VllmService } from '$lib/services/vllm.service';
import type { ApiVllmStats } from '$lib/types';

/** Statuses meaning "this backend has no stats route", which is final. */
const MISSING_ENDPOINT_STATUSES = [404, 405, 501];

/** Failed probes tolerated before deciding the backend is not vLLM. */
const PROBE_RETRIES = 4;

class VllmStore {
	stats = $state<ApiVllmStats | null>(null);
	supported = $state<boolean | null>(null);
	private refreshing = false;
	private probeFailures = 0;

	/**
	 * Refresh the stats. A failed fetch keeps the last values, so an outage
	 * neither blanks nor hides a block that already showed data.
	 */
	async refresh(): Promise<void> {
		if (this.refreshing) return;

		this.refreshing = true;

		try {
			this.stats = await VllmService.fetchStats();
			this.supported = true;
			this.probeFailures = 0;
		} catch (error) {
			if (error instanceof ApiError && MISSING_ENDPOINT_STATUSES.includes(error.status)) {
				this.supported = false;

				return;
			}

			// 502 from the adapter means the upstream is unreachable right now:
			// retry a few times before concluding this is not a vLLM backend
			if (this.stats === null && ++this.probeFailures >= PROBE_RETRIES) {
				this.supported = false;
			}
		} finally {
			this.refreshing = false;
		}
	}
}

export const vllmStore = new VllmStore();
