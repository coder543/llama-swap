import type { Model } from "./types";

export interface GroupedModels {
  local: Model[];
  localMatching: Model[];
  peers: Model[];
}

export function modelServerPath(modelId: string): string {
  if (modelId === "comfyui_auto") return "/comfyui/";
  return `/upstream/${encodeURIComponent(modelId)}/`;
}

// A model is running when its process is loaded (ready), in the middle of
// loading (starting), or in the middle of unloading (stopping). This mirrors
// the backend's RunningModels(), which excludes stopped and shutdown.
export function isRunningModel(m: Model): boolean {
  return m.state === "ready" || m.state === "starting" || m.state === "stopping";
}

// A model is shown even when unlisted models are hidden if it is not unlisted
// at all, or it is currently running. Running unlisted models stay visible
// regardless of the "show unlisted" toggle.
export function visibleWhenUnlistedHidden(m: Model): boolean {
  return !m.unlisted || isRunningModel(m);
}

// Sorts models with running ones first, then alphabetically by name and id.
// Returns a new array, leaving the input untouched.
export function sortModels(models: Model[]): Model[] {
  return [...models].sort((a, b) => {
    const runningA = isRunningModel(a) ? 0 : 1;
    const runningB = isRunningModel(b) ? 0 : 1;
    if (runningA !== runningB) return runningA - runningB;
    return (a.name + a.id).localeCompare(b.name + b.id, undefined, { numeric: true });
  });
}

export function matchesCapabilities(model: Model, required: string[], matchAny = false): boolean {
  if (!required.length) return true;
  if (!model.capabilities) return false;
  const caps = model.capabilities as Record<string, boolean>;
  if (matchAny) {
    return required.some((cap) => caps[cap] === true);
  }
  return required.every((cap) => caps[cap] === true);
}

export function groupModels(models: Model[], capabilities?: string[], matchAny = false): GroupedModels {
  const available = models.filter((m) => !m.unlisted);
  const local = available.filter((m) => !m.peerID);
  const peers = available.filter((m) => m.peerID);

  let localMatching: Model[] = [];
  let localRest: Model[] = [];

  if (capabilities && capabilities.length > 0) {
    for (const model of local) {
      if (matchesCapabilities(model, capabilities, matchAny)) {
        localMatching.push(model);
      } else {
        localRest.push(model);
      }
    }
  } else {
    localRest = local;
  }

  return { local: localRest, localMatching, peers };
}
