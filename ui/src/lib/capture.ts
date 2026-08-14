/** Keep the displayed capture ID identical to the ID accepted by the API. */
export function displayCaptureId(id: number): string {
  return String(id);
}

export function captureTitle(id: number): string {
  return `Capture #${displayCaptureId(id)}`;
}

export interface SSEChat {
  reasoning: string;
  content: string;
}

type ResponsePart = { type?: string; text?: string };
type ResponseOutput = {
  type?: string;
  content?: ResponsePart[];
  summary?: ResponsePart[];
};

function appendResponseOutput(result: SSEChat, response: unknown): SSEChat {
  if (!response || typeof response !== "object") return result;

  const record = response as {
    output?: ResponseOutput[];
    choices?: Array<{
      message?: {
        content?: string | ResponsePart[];
        reasoning_content?: string;
        reasoning?: string;
      };
    }>;
    output_text?: string;
  };

  for (const item of record.output ?? []) {
    if (item.type === "reasoning") {
      for (const part of [...(item.summary ?? []), ...(item.content ?? [])]) {
        if (typeof part.text === "string") result.reasoning += part.text;
      }
    } else if (item.type === "message") {
      for (const part of item.content ?? []) {
        if (part.type === "output_text" && typeof part.text === "string") {
          result.content += part.text;
        }
      }
    }
  }

  for (const choice of record.choices ?? []) {
    const message = choice.message;
    if (!message) continue;
    if (typeof message.reasoning_content === "string") {
      result.reasoning += message.reasoning_content;
    } else if (typeof message.reasoning === "string") {
      result.reasoning += message.reasoning;
    }
    if (typeof message.content === "string") {
      result.content += message.content;
    } else {
      for (const part of message.content ?? []) {
        if (typeof part.text === "string") result.content += part.text;
      }
    }
  }

  if (!result.content && typeof record.output_text === "string") {
    result.content = record.output_text;
  }
  return result;
}

function eventPayload(parsed: Record<string, unknown>): Record<string, unknown> {
  const data = parsed.data;
  return data && typeof data === "object" ? (data as Record<string, unknown>) : parsed;
}

/** Convert captured Chat Completions or Responses SSE data into readable text. */
export function parseSSEChat(text: string): SSEChat {
  const result: SSEChat = { reasoning: "", content: "" };
  const reasoningItems = new Set<string>();
  const contentItems = new Set<string>();
  let sawReasoningDelta = false;
  let sawContentDelta = false;

  for (const line of text.split("\n")) {
    const trimmed = line.trim();
    if (!trimmed.startsWith("data:")) continue;
    const data = trimmed.slice(5).trimStart();
    if (!data || data === "[DONE]") continue;

    try {
      const parsed = JSON.parse(data) as Record<string, unknown>;
      const choices = parsed.choices as
        | Array<{ delta?: { content?: string; reasoning_content?: string; reasoning?: string } }>
        | undefined;
      const delta = choices?.[0]?.delta;
      if (typeof delta?.content === "string") result.content += delta.content;
      if (typeof delta?.reasoning_content === "string") {
        result.reasoning += delta.reasoning_content;
      }
      if (typeof delta?.reasoning === "string") result.reasoning += delta.reasoning;

      const payload = eventPayload(parsed);
      const eventType =
        (typeof parsed.event === "string" && parsed.event) ||
        (typeof parsed.type === "string" && parsed.type) ||
        (typeof payload.type === "string" && payload.type) ||
        "";
      const itemID = typeof payload.item_id === "string" ? payload.item_id : "";

      if (eventType === "response.output_text.delta" && typeof payload.delta === "string") {
        result.content += payload.delta;
        sawContentDelta = true;
        if (itemID) contentItems.add(itemID);
      }
      if (
        (eventType === "response.reasoning_text.delta" ||
          eventType === "response.reasoning_summary_text.delta") &&
        typeof payload.delta === "string"
      ) {
        result.reasoning += payload.delta;
        sawReasoningDelta = true;
        if (itemID) reasoningItems.add(itemID);
      }
      if (
        eventType === "response.output_text.done" &&
        typeof payload.text === "string" &&
        (itemID ? !contentItems.has(itemID) : !sawContentDelta)
      ) {
        result.content += payload.text;
      }
      if (
        (eventType === "response.reasoning_text.done" ||
          eventType === "response.reasoning_summary_text.done") &&
        typeof payload.text === "string" &&
        (itemID ? !reasoningItems.has(itemID) : !sawReasoningDelta)
      ) {
        result.reasoning += payload.text;
      }
      if (eventType === "response.completed" && !result.reasoning && !result.content) {
        appendResponseOutput(result, payload.response);
      }
    } catch {
      // Skip malformed event data while preserving readable events around it.
    }
  }
  return result;
}
