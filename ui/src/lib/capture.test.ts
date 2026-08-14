import { describe, expect, it } from "vitest";
import { captureTitle, displayCaptureId, parseSSEChat } from "./capture";

describe("capture labels", () => {
  it("uses the raw API ID", () => {
    expect(displayCaptureId(0)).toBe("0");
    expect(captureTitle(424)).toBe("Capture #424");
  });
});

describe("captured SSE chat", () => {
  it("parses Chat Completions content and reasoning", () => {
    const stream = [
      'data: {"choices":[{"delta":{"reasoning_content":"think "}}]}',
      'data: {"choices":[{"delta":{"content":"answer"}}]}',
      "data: [DONE]",
    ].join("\n\n");

    expect(parseSSEChat(stream)).toEqual({ reasoning: "think ", content: "answer" });
  });

  it("parses direct and wrapped Responses events", () => {
    const stream = [
      'data: {"type":"response.reasoning_summary_text.delta","item_id":"r1","delta":"why"}',
      'data: {"event":"response.output_text.delta","data":{"item_id":"m1","delta":"hello"}}',
      'data: {"type":"response.output_text.delta","item_id":"m1","delta":" world"}',
    ].join("\n\n");

    expect(parseSSEChat(stream)).toEqual({ reasoning: "why", content: "hello world" });
  });

  it("does not repeat delta text when done events arrive", () => {
    const stream = [
      'data: {"type":"response.output_text.delta","item_id":"m1","delta":"hello"}',
      'data: {"type":"response.output_text.done","item_id":"m1","text":"hello"}',
    ].join("\n\n");

    expect(parseSSEChat(stream).content).toBe("hello");
  });

  it("falls back to the completed response when no deltas were captured", () => {
    const stream =
      'data: {"type":"response.completed","response":{"output":[' +
      '{"type":"reasoning","summary":[{"type":"summary_text","text":"why"}]},' +
      '{"type":"message","content":[{"type":"output_text","text":"answer"}]}' +
      ']}}';

    expect(parseSSEChat(stream)).toEqual({ reasoning: "why", content: "answer" });
  });
});
