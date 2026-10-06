<script lang="ts">
	import { colorLevelBgClass, colorLevelTextClass } from './context-gauge';
	import ContextGaugeDetails from './ContextGaugeDetails.svelte';
	import ContextGaugeLoadModel from './ContextGaugeLoadModel.svelte';
	import {
		gaugeCardEnter,
		gaugeCardLeave,
		gaugePopup,
		gaugePopupClose
	} from './gauge-popup.svelte';
	import VllmEngineStats from './VllmEngineStats.svelte';
	import { VLLM_STATS_POLL_INTERVAL_MS } from '$lib/constants';
	import { useContextGauge } from '$lib/hooks/use-context-gauge.svelte';
	import { vllmStore } from '$lib/stores';
	import { formatParameters } from '$lib/utils/formatters';

	const gauge = useContextGauge();

	// The gauge hook wraps a processing state instance that only follows the
	// live stream while its own monitoring flag is set, so the card instance
	// starts monitoring like the dial does.
	$effect(() => {
		gauge.startMonitoring();
	});

	let cardEl = $state<HTMLElement | null>(null);

	// The card markup only exists while the popup is open, so the vLLM stats
	// probe runs from this component instead: it stays mounted with the chat
	// form, which is the lifetime of the gauge area. Polling stops for good
	// when the answer shows a plain llama.cpp server, which has no stats route.
	$effect(() => {
		if (vllmStore.supported === null) {
			void vllmStore.refresh();

			return;
		}

		if (vllmStore.supported === false) return;

		const timer = setInterval(() => {
			void vllmStore.refresh();
		}, VLLM_STATS_POLL_INTERVAL_MS);

		return () => clearInterval(timer);
	});

	// Any press outside the card and outside the dial closes the card.
	// Presses on the dial are excluded because the dial handles its own
	// toggle; the listener only exists while the card is open.
	$effect(() => {
		if (!gaugePopup.open) return;

		const onPointerDown = (event: PointerEvent) => {
			const target = event.target;

			if (!(target instanceof Node)) return;

			if (cardEl?.contains(target)) return;

			if (target instanceof Element && target.closest('[data-context-gauge-trigger]')) return;

			gaugePopupClose();
		};

		document.addEventListener('pointerdown', onPointerDown, true);

		return () => document.removeEventListener('pointerdown', onPointerDown, true);
	});

	const showProgressBar = $derived(
		gauge.contextTotal !== null &&
			gauge.contextTotal > 0 &&
			(gauge.activeModelId !== null || gauge.isActiveModelLoaded)
	);
</script>

{#if gaugePopup.open}
	<div
		bind:this={cardEl}
		class="absolute z-50 w-64 -translate-x-1/2 rounded-lg border border-border/50 bg-popover p-3 text-sm text-popover-foreground shadow-lg ring-1 ring-foreground/10"
		onpointerenter={gaugeCardEnter}
		onpointerleave={gaugeCardLeave}
		role="status"
		style="left: {gaugePopup.centerX}px; bottom: {gaugePopup.bottom}px"
	>
		<div class="flex flex-col gap-2">
			<div class="flex items-center gap-2">
				<span class="font-medium">Context</span>

				<span class="text-muted-foreground">·</span>

				<span class="font-mono text-muted-foreground">
					{formatParameters(gauge.contextUsed)}
					/ {gauge.contextTotal !== null ? formatParameters(gauge.contextTotal) : '-'}
				</span>
			</div>

			{#if gauge.activeModelId !== null && !gauge.isActiveModelLoaded}
				<ContextGaugeLoadModel
					isLoading={gauge.isActiveModelLoading}
					modelId={gauge.activeModelId}
					onLoad={gauge.loadModel}
				/>
			{:else if showProgressBar}
				<div class="h-1.5 w-full overflow-hidden rounded-full bg-muted">
					<div
						class="h-full rounded-full transition-all duration-300 {colorLevelBgClass(
							gauge.colorLevel
						)}"
						style="width: {gauge.contextPercent}%"
					></div>
				</div>

				<div class="flex justify-between text-xs text-muted-foreground">
					<span>
						<span class={colorLevelTextClass(gauge.colorLevel)}>{gauge.contextPercent}%</span> used
					</span>

					<span>
						{formatParameters(gauge.contextAvailable ?? 0)} remaining
					</span>
				</div>
			{:else}
				<div class="text-xs text-muted-foreground">No context info available</div>
			{/if}

			{#if gauge.hasAnyUsage}
				<ContextGaugeDetails
					averageTokensPerSecond={gauge.averageTokensPerSecond}
					cumulativeCacheTotal={gauge.cumulativeCacheTotal}
					cumulativeOutput={gauge.cumulativeOutput}
					cumulativeRead={gauge.cumulativeRead}
					currentCache={gauge.currentCache}
					currentFresh={gauge.currentFresh}
					currentOutput={gauge.currentOutput}
					currentRead={gauge.currentRead}
					kvTotal={gauge.kvTotal}
					transientDetails={gauge.transientDetails}
				/>
			{/if}

			{#if vllmStore.supported}
				<VllmEngineStats />
			{/if}
		</div>
	</div>
{/if}
