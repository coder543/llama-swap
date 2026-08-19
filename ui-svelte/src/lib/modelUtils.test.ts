import { describe, it, expect } from "vitest";
import { matchesCapabilities, groupModels, modelServerPath, isRunningModel, sortModels } from "./modelUtils";
import type { Model } from "./types";

function makeModel(overrides: Partial<Model> = {}): Model {
  return {
    id: "test-model",
    state: "ready",
    name: "Test Model",
    description: "",
    unlisted: false,
    peerID: "",
    ...overrides,
  };
}

describe("modelServerPath", () => {
  it("uses the ComfyUI endpoint for the reserved model", () => {
    expect(modelServerPath("comfyui_auto")).toBe("/comfyui/");
  });

  it("uses the encoded upstream endpoint for other models", () => {
    expect(modelServerPath("org/model name")).toBe("/upstream/org%2Fmodel%20name/");
  });
});

describe("matchesCapabilities", () => {
  it("returns true when required is empty", () => {
    const model = makeModel();
    expect(matchesCapabilities(model, [])).toBe(true);
  });

  it("returns false when model has no capabilities", () => {
    const model = makeModel();
    expect(matchesCapabilities(model, ["vision"])).toBe(false);
  });

  it("returns false when model has empty capabilities object", () => {
    const model = makeModel({ capabilities: {} });
    expect(matchesCapabilities(model, ["vision"])).toBe(false);
  });

  it("returns true when model has the single required capability", () => {
    const model = makeModel({ capabilities: { vision: true } });
    expect(matchesCapabilities(model, ["vision"])).toBe(true);
  });

  it("returns false when model lacks the required capability", () => {
    const model = makeModel({ capabilities: { vision: true } });
    expect(matchesCapabilities(model, ["audio_transcriptions"])).toBe(false);
  });

  it("AND semantics: returns true only when all required are present", () => {
    const model = makeModel({ capabilities: { vision: true, audio_transcriptions: true } });
    expect(matchesCapabilities(model, ["vision", "audio_transcriptions"])).toBe(true);
    expect(matchesCapabilities(model, ["vision", "reranker"])).toBe(false);
  });

  it("matchAny=true: returns true when at least one required is present", () => {
    const model = makeModel({ capabilities: { vision: true } });
    expect(matchesCapabilities(model, ["vision", "reranker"], true)).toBe(true);
    expect(matchesCapabilities(model, ["audio_transcriptions", "reranker"], true)).toBe(false);
  });

  it("matchAny=true with empty required returns true", () => {
    const model = makeModel();
    expect(matchesCapabilities(model, [], true)).toBe(true);
  });
});

describe("groupModels", () => {
  const models: Model[] = [
    makeModel({ id: "chat-model", capabilities: { vision: true } }),
    makeModel({ id: "audio-model", capabilities: { audio_transcriptions: true } }),
    makeModel({ id: "no-caps-model" }),
    makeModel({ id: "peer1/peer-model", peerID: "peer1" }),
    makeModel({ id: "unlisted-model", unlisted: true, capabilities: { vision: true } }),
  ];

  it("filters out unlisted models", () => {
    const result = groupModels(models);
    expect(result.localMatching.length + result.local.length).toBe(3);
    expect([...result.localMatching, ...result.local].every((m) => !m.unlisted)).toBe(true);
  });

  it("separates peer models into peers", () => {
    const result = groupModels(models);
    expect(result.peers).toHaveLength(1);
    expect(result.peers[0].id).toBe("peer1/peer-model");
  });

  it("without capabilities, all local models go to local (non-matching)", () => {
    const result = groupModels(models);
    expect(result.localMatching).toHaveLength(0);
    expect(result.local).toHaveLength(3);
  });

  it("with capabilities, matching models go to localMatching", () => {
    const result = groupModels(models, ["vision"]);
    expect(result.localMatching).toHaveLength(1);
    expect(result.localMatching[0].id).toBe("chat-model");
    expect(result.local).toHaveLength(2);
  });

  it("with capabilities, models without capabilities go to local", () => {
    const result = groupModels(models, ["vision"]);
    expect(result.local.find((m) => m.id === "no-caps-model")).toBeDefined();
  });

  it("with matchAny, matches models with any listed capability", () => {
    const result = groupModels(models, ["vision", "audio_transcriptions"], true);
    expect(result.localMatching).toHaveLength(2);
    expect(result.localMatching.map((m) => m.id)).toContain("chat-model");
    expect(result.localMatching.map((m) => m.id)).toContain("audio-model");
    expect(result.local).toHaveLength(1);
  });

  it("with empty capabilities array, all local go to local (non-matching)", () => {
    const result = groupModels(models, []);
    expect(result.localMatching).toHaveLength(0);
    expect(result.local).toHaveLength(3);
  });
});

describe("isRunningModel", () => {
  it("treats ready, starting, and stopping as running", () => {
    expect(isRunningModel(makeModel({ state: "ready" }))).toBe(true);
    expect(isRunningModel(makeModel({ state: "starting" }))).toBe(true);
    expect(isRunningModel(makeModel({ state: "stopping" }))).toBe(true);
  });

  it("treats stopped, shutdown, and unknown as not running", () => {
    expect(isRunningModel(makeModel({ state: "stopped" }))).toBe(false);
    expect(isRunningModel(makeModel({ state: "shutdown" }))).toBe(false);
    expect(isRunningModel(makeModel({ state: "unknown" }))).toBe(false);
  });
});

describe("sortModels", () => {
  it("puts running models before non-running ones", () => {
    const models = [
      makeModel({ id: "aaa", name: "aaa", state: "stopped" }),
      makeModel({ id: "ddd", name: "ddd", state: "starting" }),
      makeModel({ id: "ccc", name: "ccc", state: "stopped" }),
      makeModel({ id: "bbb", name: "bbb", state: "ready" }),
    ];
    const sorted = sortModels(models);
    expect(sorted.map((m) => m.id)).toEqual(["bbb", "ddd", "aaa", "ccc"]);
  });

  it("uses numeric-aware ordering for digit-bearing ids", () => {
    const models = [
      makeModel({ id: "model-10", name: "model-10", state: "stopped" }),
      makeModel({ id: "model-2", name: "model-2", state: "stopped" }),
    ];
    const sorted = sortModels(models);
    expect(sorted.map((m) => m.id)).toEqual(["model-2", "model-10"]);
  });

  it("sorts alphabetically by name and id within each group", () => {
    const models = [
      makeModel({ id: "zeta", name: "zeta", state: "stopped" }),
      makeModel({ id: "alpha", name: "alpha", state: "ready" }),
      makeModel({ id: "mid", name: "mid", state: "stopped" }),
      makeModel({ id: "beta", name: "beta", state: "stopping" }),
    ];
    const sorted = sortModels(models);
    expect(sorted.map((m) => m.id)).toEqual(["alpha", "beta", "mid", "zeta"]);
  });

  it("does not mutate the input array", () => {
    const models = [
      makeModel({ id: "bbb", name: "bbb", state: "stopped" }),
      makeModel({ id: "aaa", name: "aaa", state: "ready" }),
    ];
    sortModels(models);
    expect(models.map((m) => m.id)).toEqual(["bbb", "aaa"]);
  });
});
