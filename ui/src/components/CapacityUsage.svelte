<script lang="ts">
  import { link } from "svelte-spa-router";
  import { capacityTotal, models } from "../stores/api";
  import { capacityUsage, formatReservation } from "../lib/capacity";
  import { Popover } from "bits-ui";
  import { Info } from "@lucide/svelte";

  let { showFooter = true }: { showFooter?: boolean } = $props();
  let usage = $derived(capacityUsage($models, $capacityTotal));
</script>

{#if $capacityTotal > 0}
  <section aria-label="Model memory reservations" data-capacity-usage class="space-y-2">
    <div class="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1 text-sm">
      <div class="flex items-center gap-1.5">
        <a href="/models" use:link class="font-medium hover:underline">Model capacity</a>
        <Popover.Root>
          <Popover.Trigger
            aria-label="About model capacity"
            class="text-muted-foreground hover:text-foreground flex size-6 items-center justify-center rounded-full focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
          >
            <Info class="size-3.5" />
          </Popover.Trigger>
          <Popover.Portal>
            <Popover.Content
              sideOffset={6}
              align="start"
              aria-label="About model capacity"
              class="bg-popover text-popover-foreground z-50 w-80 max-w-[calc(100vw-2rem)] space-y-2 rounded-lg border p-3 text-sm shadow-md outline-none"
            >
              <p class="font-medium">Model capacity</p>
              <p>Reservations are configured estimates in GB, not measured memory usage. Models still reserve memory while loading or unloading, until they stop.</p>
              <p class="text-muted-foreground">Actual system and GPU memory usage is on Performance.</p>
            </Popover.Content>
          </Popover.Portal>
        </Popover.Root>
      </div>
      <span class="text-muted-foreground text-xs tabular-nums">
        {formatReservation(usage.reserved)} reserved / {formatReservation($capacityTotal)} total
      </span>
    </div>
    <div class="bg-muted relative flex h-5 w-full rounded-md" role="img" aria-label={`${formatReservation(usage.reserved)} reserved out of ${formatReservation($capacityTotal)} capacity`}>
      <div class="flex size-full overflow-hidden rounded-md">
        {#each usage.segments as segment (segment.model.id)}
          <span
            data-reservation-segment
            aria-hidden="true"
            class="h-full min-w-0 shrink-0 border-r border-background/40"
            style:width="{segment.percent}%"
            style:background-color={segment.color}
          ></span>
        {/each}
      </div>
      {#if usage.overage > 0}
        <div aria-hidden="true" class="pointer-events-none absolute inset-y-0 border-l-2 border-dashed border-foreground" style:left="{usage.budgetPercent}%"></div>
      {/if}
    </div>
    {#if showFooter}
      <div class="flex flex-wrap items-start justify-between gap-x-4 gap-y-1.5 text-xs">
        {#if usage.segments.length > 0}
          <div class="flex min-w-0 flex-wrap gap-x-4 gap-y-1.5">
            {#each usage.segments as segment (segment.model.id)}
              <a href="/models/{encodeURIComponent(segment.model.id)}" use:link class="flex min-w-0 items-center gap-1.5 hover:underline">
                <span aria-hidden="true" class="size-2.5 shrink-0 rounded-sm" style:background-color={segment.color}></span>
                <span class="break-all">{segment.model.name || segment.model.id}</span>
                <span class="text-muted-foreground shrink-0 tabular-nums">{formatReservation(segment.memory)}</span>
              </a>
            {/each}
          </div>
        {/if}
        {#if usage.overage > 0}
          <span class="text-warning ml-auto shrink-0 font-medium tabular-nums">{formatReservation(usage.overage)} over capacity</span>
        {:else}
          <span class="text-muted-foreground ml-auto shrink-0 tabular-nums">{formatReservation(usage.available)} available</span>
        {/if}
      </div>
    {/if}
  </section>
{/if}
