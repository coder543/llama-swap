import { get } from "svelte/store";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

function mockBrowser(values: Record<string, string> = {}): Storage {
  const stored = new Map(Object.entries(values));
  const localStorage: Storage = {
    get length() {
      return stored.size;
    },
    clear: vi.fn(() => stored.clear()),
    getItem: vi.fn((key: string) => stored.get(key) ?? null),
    key: vi.fn((index: number) => Array.from(stored.keys())[index] ?? null),
    removeItem: vi.fn((key: string) => stored.delete(key)),
    setItem: vi.fn((key: string, value: string) => stored.set(key, value)),
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

  it("defaults first-time visitors to dark mode", async () => {
    const localStorage = mockBrowser();
    const { isDarkMode, themeMode } = await import("./theme");

    expect(get(themeMode)).toBe("dark");
    expect(get(isDarkMode)).toBe(true);
    expect(localStorage.setItem).toHaveBeenCalledWith("theme-mode", '"dark"');
  });

  it("preserves a saved light-mode preference", async () => {
    mockBrowser({ "theme-mode": '"light"' });
    const { isDarkMode, themeMode } = await import("./theme");

    expect(get(themeMode)).toBe("light");
    expect(get(isDarkMode)).toBe(false);
  });

  it("migrates the legacy boolean preference", async () => {
    const localStorage = mockBrowser({ theme: "false" });
    const { themeMode } = await import("./theme");

    expect(get(themeMode)).toBe("light");
    expect(localStorage.removeItem).toHaveBeenCalledWith("theme");
    expect(localStorage.setItem).toHaveBeenCalledWith("theme-mode", '"light"');
  });
});
