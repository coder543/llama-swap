import { describe, expect, it } from "vitest";
import { captureTitle, displayCaptureId } from "./capture";

describe("capture labels", () => {
  it("uses the raw API ID", () => {
    expect(displayCaptureId(0)).toBe("0");
    expect(displayCaptureId(424)).toBe("424");
  });

  it("uses the raw API ID in dialog titles", () => {
    expect(captureTitle(424)).toBe("Capture #424");
  });
});
