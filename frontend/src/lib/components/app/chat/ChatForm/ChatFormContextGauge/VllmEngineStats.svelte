<script lang="ts">
	import { colorLevelBgClass } from './context-gauge';
	import ContextGaugeDetailRow from './ContextGaugeDetailRow.svelte';
	import { STATS_UNITS } from '$lib/constants';
	import { ColorLevel } from '$lib/enums';
	import { vllmStore } from '$lib/stores';

	// A number the adapter did not report stays null, so its row hides itself.
	// Unknown is not the same as zero here.
	function decimal(value: number | undefined): string | null {
		return typeof value === 'number' ? value.toFixed(1) : null;
	}

	function percentValue(value: number | undefined): string | null {
		const formatted = decimal(value);

		return formatted === null ? null : `${formatted}%`;
	}

	function kvCacheColor(usage: number | undefined): ColorLevel {
		if (usage === undefined) return ColorLevel.NEUTRAL;

		if (usage < 60) return ColorLevel.OK;

		if (usage < 85) return ColorLevel.WARNING;

		return ColorLevel.CRITICAL;
	}

	const title = $derived(
		typeof vllmStore.stats?.title === 'string' && vllmStore.stats.title !== ''
			? vllmStore.stats.title
			: 'vLLM engine'
	);
	const kvLabel = $derived(
		typeof vllmStore.stats?.labels?.kv === 'string' && vllmStore.stats.labels.kv !== ''
			? vllmStore.stats.labels.kv
			: 'KV cache'
	);
	const ttftLabel = $derived(
		typeof vllmStore.stats?.labels?.ttft === 'string' && vllmStore.stats.labels.ttft !== ''
			? vllmStore.stats.labels.ttft
			: 'First token'
	);
	const gauges = $derived(vllmStore.stats?.gauges);
	const rates = $derived(vllmStore.stats?.rates);
	const counters = $derived(vllmStore.stats?.counters);

	const kvCacheUsage = $derived(gauges?.kv_cache_usage_percent);
	const kvCache = $derived(percentValue(kvCacheUsage));
	const kvCacheBgClass = $derived(colorLevelBgClass(kvCacheColor(kvCacheUsage)));
	const outputRate = $derived(decimal(rates?.generation_tokens_per_second));
	const promptRate = $derived(decimal(rates?.prompt_tokens_per_second));
	const requests = $derived(
		typeof gauges?.requests_running === 'number' && typeof gauges?.requests_waiting === 'number'
			? `${gauges.requests_running} / ${gauges.requests_waiting}`
			: null
	);
	const prefixCacheHit = $derived(percentValue(gauges?.prefix_cache_hit_percent));
	const tokensPerStep = $derived(
		(gauges?.tokens_per_step ?? 0) > 0 ? decimal(gauges?.tokens_per_step) : null
	);
	const specDecode = $derived(
		tokensPerStep === null ? null : percentValue(gauges?.spec_decode_accept_percent)
	);
	const firstToken = $derived(
		typeof counters?.avg_ttft_ms === 'number' ? `${Math.round(counters.avg_ttft_ms)} ms` : null
	);
</script>

<div class="mt-3 border-t border-border/50 pt-4 text-xs">
	<div class="mb-2 flex items-baseline justify-between gap-2">
		<h3 class="text-[11px] font-medium uppercase tracking-wide text-muted-foreground/70">
			{title}
		</h3>

		<span
			class="truncate text-[10px] text-muted-foreground/70"
			title="Values describe the whole engine, not this request">engine-wide</span
		>
	</div>

	<div class="flex flex-col gap-2">
		{#if kvCache !== null}
			<div class="grid gap-1.5">
				<ContextGaugeDetailRow label={kvLabel} value={kvCache} />

				<div class="h-1.5 w-full overflow-hidden rounded-full bg-muted">
					<div
						class="h-full rounded-full transition-all duration-300 {kvCacheBgClass}"
						style="width: {kvCacheUsage}%"
					></div>
				</div>
			</div>
		{/if}

		{#if outputRate !== null}
			<ContextGaugeDetailRow
				label="Output"
				value={`${outputRate}${STATS_UNITS.TOKENS_PER_SECOND}`}
			/>
		{/if}

		{#if promptRate !== null}
			<ContextGaugeDetailRow
				label="Prompt"
				value={`${promptRate}${STATS_UNITS.TOKENS_PER_SECOND}`}
			/>
		{/if}

		{#if requests !== null}
			<ContextGaugeDetailRow label="Requests" value={requests} />
		{/if}

		{#if prefixCacheHit !== null}
			<ContextGaugeDetailRow label="Prefix cache" value={prefixCacheHit} />
		{/if}

		{#if specDecode !== null}
			<ContextGaugeDetailRow
				label="Speculative"
				subtitle={`${tokensPerStep} tok/step`}
				value={specDecode}
			/>
		{/if}

		{#if firstToken !== null}
			<ContextGaugeDetailRow label={ttftLabel} value={firstToken} />
		{/if}
	</div>
</div>
