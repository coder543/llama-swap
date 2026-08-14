import type { Model } from "./types";
import { activeLocalModels } from "./modelUtils";

export function formatReservation(gigabytes: number): string {
  return `${gigabytes.toLocaleString(undefined, { maximumFractionDigits: 1 })} GB`;
}

// Stable across state updates, ordering, page changes and browser sessions.
export function reservationColor(modelName: string): string {
  let hash = 2166136261;
  for (const character of modelName) {
    hash = Math.imul(hash ^ character.codePointAt(0)!, 16777619);
  }
  return `hsl(${(hash >>> 0) % 360} 65% 52%)`;
}

export function capacityUsage(models: Model[], total: number) {
  const active = activeLocalModels(models).filter((model) => Number.isFinite(model.memory) && model.memory! > 0);
  const reserved = active.reduce((sum, model) => sum + model.memory!, 0);
  // Oversized models may run alone. Keep every reservation visible and mark
  // the configured budget rather than silently clamping the reported total.
  const scale = Math.max(total, reserved, 1);
  return {
    reserved,
    available: Math.max(0, total - reserved),
    overage: Math.max(0, reserved - total),
    budgetPercent: total / scale * 100,
    segments: active.map((model) => ({
      model,
      memory: model.memory!,
      percent: model.memory! / scale * 100,
      color: reservationColor(model.id),
    })),
  };
}
