import { derived, writable } from "svelte/store";
import { loadModel, models, unloadSingleModel } from "./api";
import type { Model } from "../lib/types";

export const pendingLoads = writable<Record<string, boolean>>({});
const loadControllers = new Map<string, AbortController>();

export async function handleLoadModel(id: string): Promise<void> {
  if (isPending(id)) return;

  const controller = new AbortController();
  loadControllers.set(id, controller);
  pendingLoads.update((p) => ({ ...p, [id]: true }));
  try {
    await loadModel(id, controller.signal);
  } catch (e) {
    console.error(e);
  } finally {
    loadControllers.delete(id);
    pendingLoads.update((p) => {
      const next = { ...p };
      delete next[id];
      return next;
    });
  }
}

export function cancelLoad(id: string): void {
  loadControllers.get(id)?.abort();
}

export function isPending(id: string): boolean {
  let val = false;
  pendingLoads.subscribe((p) => (val = !!p[id]))();
  return val;
}

export function onToggleLoad(m: Model): void {
  if (m.state === "stopped" && isPending(m.id)) {
    cancelLoad(m.id);
  } else if (m.state === "stopped") {
    void handleLoadModel(m.id);
  } else if (m.state === "ready") {
    void unloadSingleModel(m.id);
  }
}

// Tick only while a subscribed view has an estimated startup to animate.
export const loadingNow = derived(models, ($models, set) => {
  set(Date.now());
  if (!$models.some(isEstimatedLoading)) return;
  const timer = setInterval(() => set(Date.now()), 200);
  return () => clearInterval(timer);
}, Date.now());

export function isEstimatedLoading(m: Model | undefined): boolean {
  return m?.state === "starting"
    && !Number.isFinite(m.loadingProgress)
    && Number.isFinite(m.estimatedLoadMs) && (m.estimatedLoadMs ?? 0) > 0
    && Number.isFinite(m.loadingStartedAt);
}

// Prefer upstream progress; otherwise estimate from recent successful starts.
// Readiness still comes from the health check, so a running bar stops at 99%.
export function loadingPercent(m: Model | undefined, now = Date.now()): number | undefined {
  if (m?.state !== "starting") return undefined;
  if (m.loadingProgress !== undefined && Number.isFinite(m.loadingProgress)) {
    return Math.min(99, Math.floor(Math.max(m.loadingProgress, 0) * 100));
  }
  if (!isEstimatedLoading(m)) return undefined;
  const fraction = Math.max(0, now - m.loadingStartedAt!) / m.estimatedLoadMs!;
  return Math.min(99, Math.floor(fraction * 100));
}

export function loadingMessage(m: Model | undefined, now = Date.now()): string {
  if (m?.state !== "starting") return "";
  if (m.loadingMessage) return m.loadingMessage;
  if (!isEstimatedLoading(m)) return "";
  const total = Math.ceil(m.estimatedLoadMs! / 1000);
  const remaining = Math.ceil((m.estimatedLoadMs! - Math.max(0, now - m.loadingStartedAt!)) / 1000);
  return remaining > 0
    ? `Estimated startup: ~${total}s · about ${remaining}s remaining`
    : `Taking longer than the ~${total}s startup estimate`;
}

export function statusDotColor(m: Model | undefined): string {
  if (!m) return "bg-muted-foreground/40";
  if (m.state === "ready") return "bg-success";
  if (m.state === "starting" || m.state === "stopping") return "bg-warning";
  return "bg-muted-foreground/40";
}
