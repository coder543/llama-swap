import { describe, expect, it } from "vitest";
import { capacityUsage, formatReservation, reservationColor } from "./capacity";
import type { Model } from "./types";

function model(id: string, memory: number, state: Model["state"] = "ready", extra: Partial<Model> = {}): Model {
  return { id, name: id, memory, state, description: "", unlisted: false, peerID: "", ...extra };
}

describe("capacityUsage", () => {
  it("counts active local reservations, including unlisted, loading and unloading processes", () => {
    const usage = capacityUsage([
      model("zeta", 12, "ready", { unlisted: true }),
      model("alpha", 24, "starting"),
      model("beta", 8, "stopping"),
      model("stopped", 40, "stopped"),
      model("shutdown", 40, "shutdown"),
      model("unknown", 40, "unknown"),
      model("peer/remote", 40, "ready", { peerID: "peer" }),
    ], 100);
    expect(usage.reserved).toBe(44);
    expect(usage.available).toBe(56);
    expect(usage.overage).toBe(0);
    expect(usage.segments.map((segment) => segment.model.id)).toEqual(["alpha", "beta", "zeta"]);
    expect(usage.segments.map((segment) => segment.percent)).toEqual([24, 8, 12]);
  });

  it("shows an empty budget without inventing reservations", () => {
    expect(capacityUsage([], 115)).toMatchObject({ reserved: 0, available: 115, overage: 0, segments: [] });
    expect(capacityUsage([model("zero", 0), model("unknown", NaN), model("negative", -5)], 115).segments).toEqual([]);
  });

  it("reports oversized reservations and marks the budget without hiding any model", () => {
    const usage = capacityUsage([model("large", 120), model("small", 30)], 100);
    expect(usage.reserved).toBe(150);
    expect(usage.available).toBe(0);
    expect(usage.overage).toBe(50);
    expect(usage.budgetPercent).toBeCloseTo(100 / 150 * 100);
    expect(usage.segments.map((segment) => segment.percent)).toEqual([80, 20]);
  });

  it("releases reservations after unload and keeps model colors stable", () => {
    const before = capacityUsage([model("alpha", 10), model("beta", 20)], 50);
    const after = capacityUsage([model("beta", 20), model("alpha", 10, "stopped")], 50);
    expect(after.reserved).toBe(20);
    expect(after.segments[0].color).toBe(before.segments[1].color);
    expect(after.segments[0].percent).toBe(40);
  });
});

describe("reservation labels and colors", () => {
  it("uses GB for every reservation size", () => {
    expect(formatReservation(0)).toBe("0 GB");
    expect(formatReservation(115)).toBe("115 GB");
  });

  it("hashes the model identifier deterministically rather than its list position", () => {
    expect(reservationColor("gemma-4-12b")).toBe(reservationColor("gemma-4-12b"));
    expect(reservationColor("gemma-4-12b")).not.toBe(reservationColor("qwen3.8-flash-next"));
    expect(reservationColor("org/モデル")).toMatch(/^hsl\(\d+ 65% 52%\)$/);
  });
});
