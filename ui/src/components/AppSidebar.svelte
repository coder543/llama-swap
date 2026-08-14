<script lang="ts">
  import { link } from "svelte-spa-router";
  import { FerrisWheel, Boxes, Activity, Cat, ScrollText, Gauge, Cpu, Sun, Moon, Monitor, Settings, CircleQuestionMark } from "@lucide/svelte";
  import * as Sidebar from "$lib/components/ui/sidebar/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import { toggleTheme, themeMode, appTitle } from "../stores/theme";
  import { currentRoute } from "../stores/route";
  import { playgroundActivity, docsAgentStreaming } from "../stores/playgroundActivity";
  import { performanceEnabled, models, tailcatStatus } from "../stores/api";
  import { activeLocalModels } from "../lib/modelUtils";
  import type { Model } from "../lib/types";
  import { isComposingKey } from "../lib/ime";
  import ConnectionStatus from "./ConnectionStatus.svelte";
  import MiddleTruncate from "./MiddleTruncate.svelte";

  function handleTitleChange(newTitle: string): void {
    const sanitized = newTitle.replace(/\n/g, "").trim().substring(0, 64) || "llama-swap";
    appTitle.set(sanitized);
  }

  function handleKeyDown(e: KeyboardEvent): void {
    if (e.key === "Enter" && !isComposingKey(e)) {
      e.preventDefault();
      const target = e.currentTarget as HTMLElement;
      handleTitleChange(target.textContent || "(set title)");
      target.blur();
    }
  }

  function handleBlur(e: FocusEvent): void {
    const target = e.currentTarget as HTMLElement;
    handleTitleChange(target.textContent || "(set title)");
  }

  function isActive(path: string, current: string): boolean {
    return path === "/" ? current === "/" : current.startsWith(path);
  }

  let activeModels = $derived(activeLocalModels($models));

  type DotColor = "grey" | "yellow" | "green";
  function statusDotColor(model: Model): DotColor {
    if (model.state === "ready") return "green";
    if (model.state === "starting" || model.state === "stopping") return "yellow";
    return "grey";
  }

  const dotClass: Record<DotColor, string> = {
    grey: "bg-muted-foreground/40",
    yellow: "bg-warning",
    green: "bg-success",
  };
</script>

{#snippet modelMenuItem(model: Model)}
  <Sidebar.MenuSubItem>
    <Sidebar.MenuSubButton
      isActive={$currentRoute === `/models/${encodeURIComponent(model.id)}`}
    >
      {#snippet child({ props })}
        <a href="/models/{encodeURIComponent(model.id)}" use:link {...props}>
          <span class={`size-2 shrink-0 rounded-full ${dotClass[statusDotColor(model)]}`}></span>
          <MiddleTruncate text={model.id} class="flex-1" />
        </a>
      {/snippet}
    </Sidebar.MenuSubButton>
  </Sidebar.MenuSubItem>
{/snippet}

<Sidebar.Root collapsible="icon">
  <Sidebar.Header>
    <div class="flex items-center gap-2 px-2 py-1.5">
      <div class="flex shrink-0 items-center justify-center">
        <ConnectionStatus />
      </div>
      <h1
        contenteditable="true"
        class="truncate pb-0 text-base font-semibold outline-none rounded-md px-1 hover:bg-sidebar-accent group-data-[collapsible=icon]:hidden"
        onblur={handleBlur}
        onkeydown={handleKeyDown}
      >
        {$appTitle}
      </h1>
    </div>
  </Sidebar.Header>

  <!-- Only the model list owns scrolling; the surrounding navigation stays fixed. -->
  <Sidebar.Content class="overflow-clip">
    <Sidebar.Group class="min-h-0">
      <Sidebar.GroupContent class="flex min-h-0 flex-col">
        <Sidebar.Menu class="gap-1 min-h-0">
          <Sidebar.MenuItem class="shrink-0">
            <Sidebar.MenuButton isActive={$currentRoute === "/" || isActive("/activity", $currentRoute)} tooltipContent="Activity">
              {#snippet child({ props })}
                <a href="/" use:link {...props}>
                  <Activity />
                  <span>Activity</span>
                </a>
              {/snippet}
            </Sidebar.MenuButton>
          </Sidebar.MenuItem>

          {#if $tailcatStatus.enabled}
            <Sidebar.MenuItem class="shrink-0">
              <Sidebar.MenuButton isActive={isActive("/tailcat", $currentRoute)} tooltipContent="Tailcat">
                {#snippet child({ props })}
                  <a href="/tailcat" use:link {...props}>
                    <Cat />
                    <span>Tailcat</span>
                  </a>
                {/snippet}
              </Sidebar.MenuButton>
            </Sidebar.MenuItem>
          {/if}

          <Sidebar.MenuItem class="shrink-0">

            <Sidebar.MenuButton isActive={isActive("/playground", $currentRoute)} tooltipContent="Playground">
              {#snippet child({ props })}
                <a href="/playground" use:link {...props}>
                  <FerrisWheel />
                  <span class={$playgroundActivity ? "activity-link" : ""}>Playground</span>
                </a>
              {/snippet}
            </Sidebar.MenuButton>
          </Sidebar.MenuItem>

          <Sidebar.MenuItem class="flex min-h-8 flex-col">
            <Sidebar.MenuButton
              isActive={$currentRoute.startsWith("/models")}
              tooltipContent="Models"
              class="shrink-0"
            >
              {#snippet child({ props })}
                <a href="/models" use:link {...props}>
                  <Boxes />
                  <span>Models</span>
                </a>
              {/snippet}
            </Sidebar.MenuButton>
            {#if activeModels.length > 0}
              <div class="min-h-0 overflow-x-hidden overflow-y-auto overscroll-y-contain touch-pan-y touch-pinch-zoom [-webkit-overflow-scrolling:touch]">
                <Sidebar.MenuSub>
                  {#each activeModels as model (model.id)}
                    {@render modelMenuItem(model)}
                  {/each}
                </Sidebar.MenuSub>
              </div>
            {/if}
          </Sidebar.MenuItem>

          <Sidebar.MenuItem class="shrink-0">
            <Sidebar.MenuButton isActive={isActive("/logs", $currentRoute)} tooltipContent="Logs">
              {#snippet child({ props })}
                <a href="/logs" use:link {...props}>
                  <ScrollText />
                  <span>Logs</span>
                </a>
              {/snippet}
            </Sidebar.MenuButton>
          </Sidebar.MenuItem>

          {#if $performanceEnabled}
            <Sidebar.MenuItem class="shrink-0">
              <Sidebar.MenuButton isActive={isActive("/performance", $currentRoute)} tooltipContent="Performance">
                {#snippet child({ props })}
                  <a href="/performance" use:link {...props}>
                    <Gauge />
                    <span>Performance</span>
                  </a>
                {/snippet}
              </Sidebar.MenuButton>
            </Sidebar.MenuItem>
          {/if}

          <Sidebar.MenuItem class="shrink-0">
            <Sidebar.MenuButton isActive={isActive("/hardware", $currentRoute)} tooltipContent="Hardware">
              {#snippet child({ props })}
                <a href="/hardware" use:link {...props}>
                  <Cpu />
                  <span>Hardware</span>
                </a>
              {/snippet}
            </Sidebar.MenuButton>
          </Sidebar.MenuItem>
        </Sidebar.Menu>
      </Sidebar.GroupContent>
    </Sidebar.Group>
  </Sidebar.Content>

  <Sidebar.Footer>
    <Sidebar.Menu>
      <Sidebar.MenuItem class="shrink-0">
        <Sidebar.MenuButton isActive={isActive("/help", $currentRoute)} tooltipContent="Help">
          {#snippet child({ props })}
            <a href="/help" use:link {...props}>
              <CircleQuestionMark />
              <span class={$docsAgentStreaming ? "activity-link" : ""}>Help</span>
            </a>
          {/snippet}
        </Sidebar.MenuButton>
      </Sidebar.MenuItem>
    </Sidebar.Menu>

    <div
      class="flex items-center justify-between gap-2 group-data-[collapsible=icon]:flex-col-reverse"
    >
      <Sidebar.MenuButton
        isActive={isActive("/settings", $currentRoute)}
        tooltipContent="Settings"
      >
        {#snippet child({ props })}
          <a href="/settings" use:link {...props}>
            <Settings />
            <span>Settings</span>
          </a>
        {/snippet}
      </Sidebar.MenuButton>
      <Button
        variant="ghost"
        size="icon"
        onclick={toggleTheme}
        title="Toggle theme (current: {$themeMode})"
      >
        {#if $themeMode === "system"}
          <Monitor />
        {:else if $themeMode === "light"}
          <Sun />
        {:else}
          <Moon />
        {/if}
        <span class="sr-only">Toggle theme</span>
      </Button>
    </div>
  </Sidebar.Footer>
  <Sidebar.Rail />
</Sidebar.Root>
