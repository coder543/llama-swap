import { get } from "svelte/store";
import { beforeEach, afterEach, describe, expect, it, vi } from "vitest";

function mockBrowser(savedTheme: boolean | null): Storage {
  const values = new Map<string, string>();
  if (savedTheme !== null) {
    values.set("theme", JSON.stringify(savedTheme));
  }

  const localStorage: Storage = {
    get length() {
      return values.size;
    },
    clear: vi.fn(() => values.clear()),
    getItem: vi.fn((key: string) => values.get(key) ?? null),
    key: vi.fn((index: number) => Array.from(values.keys())[index] ?? null),
    removeItem: vi.fn((key: string) => values.delete(key)),
    setItem: vi.fn((key: string, value: string) => values.set(key, value)),
  };

  vi.stubGlobal("window", {});
  vi.stubGlobal("localStorage", localStorage);
  return localStorage;
}

describe("theme", () => {
  beforeEach(() => {
    vi.resetModules();
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("defaults to dark mode when no preference is saved", async () => {
    const localStorage = mockBrowser(null);
    const { isDarkMode } = await import("./theme");

    expect(get(isDarkMode)).toBe(true);
    expect(localStorage.setItem).toHaveBeenCalledWith("theme", "true");
  });

  it("preserves an explicitly saved light mode preference", async () => {
    const localStorage = mockBrowser(false);
    const { isDarkMode } = await import("./theme");

    expect(get(isDarkMode)).toBe(false);
    expect(localStorage.setItem).toHaveBeenCalledWith("theme", "false");
  });
});
