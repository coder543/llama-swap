<script lang="ts">
	import { cn, type WithElementRef } from "$lib/utils.js";
	import type { HTMLAttributes } from "svelte/elements";
	import { useSidebar } from "./context.svelte.js";

	let {
		ref = $bindable(null),
		class: className,
		children,
		inert,
		...restProps
	}: WithElementRef<HTMLAttributes<HTMLElement>> = $props();

	const sidebar = useSidebar();
</script>

<main
	bind:this={ref}
	data-slot="sidebar-inset"
	inert={inert || (sidebar.isMobile && sidebar.openMobile)}
	class={cn("bg-background md:peer-data-[variant=inset]:m-2 md:peer-data-[variant=inset]:ml-0 md:peer-data-[variant=inset]:rounded-xl md:peer-data-[variant=inset]:shadow-sm md:peer-data-[variant=inset]:peer-data-[state=collapsed]:ml-2 relative flex w-full flex-1 flex-col", className)}
	{...restProps}
>
	{@render children?.()}
</main>
