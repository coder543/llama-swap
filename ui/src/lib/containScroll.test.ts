import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { containScroll } from "./containScroll";

// Layout metrics are explicit here; native browser checks cover real layout.
class TestElement extends EventTarget {
  parentElement: TestElement | null;
  ownerDocument: EventTarget;
  scrollTop = 0;
  scrollLeft = 0;
  scrollHeight = 100;
  scrollWidth = 100;
  clientHeight = 100;
  clientWidth = 100;
  computed = { overflowX: "visible", overflowY: "visible" };

  constructor(document: EventTarget, parent: TestElement | null = null) {
    super();
    this.ownerDocument = document;
    this.parentElement = parent;
  }

  contains(target: TestElement | null): boolean {
    for (let element = target; element; element = element.parentElement) {
      if (element === this) return true;
    }
    return false;
  }
}

describe("containScroll", () => {
  let document: EventTarget;
  let drawer: TestElement;
  let list: TestElement;
  let link: TestElement;
  let background: TestElement;
  let action: ReturnType<typeof containScroll>;

  function touch(type: string, target: TestElement | EventTarget, x: number, y: number, count = 1,
    receiver: EventTarget = document): Event {
    const event = new Event(type, { cancelable: true });
    Object.defineProperties(event, {
      target: { value: target },
      touches: { value: Array.from({ length: count }, (_, id) => ({ identifier: id, clientX: x, clientY: y })) },
    });
    receiver.dispatchEvent(event);
    return event;
  }

  function swipe(target: TestElement | EventTarget, dx: number, dy: number): Event {
    touch("touchstart", target, 100, 100);
    return touch("touchmove", target, 100 - dx, 100 - dy);
  }

  function wheel(target: TestElement, dx: number, dy: number, ctrlKey = false): Event {
    const event = new Event("wheel", { cancelable: true });
    Object.defineProperties(event, {
      target: { value: target }, deltaX: { value: dx }, deltaY: { value: dy }, ctrlKey: { value: ctrlKey },
    });
    document.dispatchEvent(event);
    return event;
  }

  beforeEach(() => {
    vi.stubGlobal("Element", TestElement);
    vi.stubGlobal("HTMLElement", TestElement);
    vi.stubGlobal("getComputedStyle", (element: TestElement) => element.computed);
    document = new EventTarget();
    drawer = new TestElement(document);
    list = new TestElement(document, drawer);
    list.scrollHeight = 400;
    list.scrollTop = 100;
    list.computed.overflowY = "auto";
    link = new TestElement(document, list);
    background = new TestElement(document);
    background.scrollHeight = 1_000;
    background.computed.overflowY = "auto";
    action = containScroll(drawer as unknown as HTMLElement);
  });

  afterEach(() => {
    action.destroy();
    vi.unstubAllGlobals();
  });

  it("allows native scrolling in both directions on model links", () => {
    expect(swipe(link, 0, 40).defaultPrevented).toBe(false);
    expect(swipe(link, 0, -40).defaultPrevented).toBe(false);
    // The action must not replace native scrolling with JavaScript scrolling.
    expect(list.scrollTop).toBe(100);
  });

  it("handles touches delivered directly to the drawer without document dispatch", () => {
    list.scrollTop = 0;
    touch("touchstart", link, 100, 100, 1, drawer);
    expect(touch("touchmove", link, 100, 120, 1, drawer).defaultPrevented).toBe(true);
    expect(touch("touchmove", link, 100, 110, 1, drawer).defaultPrevented).toBe(false);
    touch("touchcancel", link, 100, 110, 0, drawer);
    expect(touch("touchmove", link, 100, 120, 1, drawer).defaultPrevented).toBe(false);
  });

  it("processes a touch only once as it passes through document and drawer listeners", () => {
    touch("touchstart", link, 100, 100);
    const event = new Event("touchmove", { cancelable: true });
    const touches = vi.fn(() => [{ identifier: 0, clientX: 100, clientY: 60 }]);
    Object.defineProperties(event, { target: { value: link }, touches: { get: touches } });
    document.dispatchEvent(event);
    const reads = touches.mock.calls.length;
    expect(reads).toBeGreaterThan(0);
    drawer.dispatchEvent(event);
    expect(touches).toHaveBeenCalledTimes(reads);
    expect(event.defaultPrevented).toBe(false);
  });

  it("blocks gestures over the background and backdrop even when they can scroll", () => {
    expect(swipe(background, 0, 40).defaultPrevented).toBe(true);
    expect(swipe(document, 0, 40).defaultPrevented).toBe(true);
  });

  it("blocks gestures on fixed navigation and non-overflowing lists", () => {
    expect(swipe(drawer, 0, 40).defaultPrevented).toBe(true);
    list.scrollHeight = list.clientHeight;
    expect(swipe(link, 0, 40).defaultPrevented).toBe(true);
  });

  it("blocks outward movement at each boundary but allows scrolling back into the list", () => {
    list.scrollTop = 0;
    expect(swipe(link, 0, -40).defaultPrevented).toBe(true);
    expect(swipe(link, 0, 40).defaultPrevented).toBe(false);
    list.scrollTop = list.scrollHeight - list.clientHeight;
    expect(swipe(link, 0, 40).defaultPrevented).toBe(true);
    expect(swipe(link, 0, -40).defaultPrevented).toBe(false);
  });

  it("handles WebKit rubber-band positions beyond either scroll boundary", () => {
    list.scrollTop = -20;
    expect(swipe(link, 0, -40).defaultPrevented).toBe(true);
    expect(swipe(link, 0, 40).defaultPrevented).toBe(false);
    list.scrollTop = 320;
    expect(swipe(link, 0, 40).defaultPrevented).toBe(true);
    expect(swipe(link, 0, -40).defaultPrevented).toBe(false);
  });

  it("allows an inner scroller to hand off to a scrollable ancestor inside the drawer", () => {
    const inner = new TestElement(document, list);
    inner.scrollHeight = 200;
    inner.scrollTop = 100;
    inner.computed.overflowY = "auto";
    expect(swipe(inner, 0, 40).defaultPrevented).toBe(false);
    list.scrollTop = 300;
    expect(swipe(inner, 0, 40).defaultPrevented).toBe(true);
  });

  it("does not treat overflow:hidden as a user-scrollable region", () => {
    list.computed.overflowY = "hidden";
    expect(swipe(link, 0, 40).defaultPrevented).toBe(true);
  });

  it("contains horizontal gestures independently of vertical scrolling", () => {
    expect(swipe(link, 40, 5).defaultPrevented).toBe(true);
    list.scrollWidth = 400;
    list.scrollLeft = 100;
    list.computed.overflowX = "scroll";
    expect(swipe(link, 40, 5).defaultPrevented).toBe(false);
    list.scrollLeft = 300;
    expect(swipe(link, 40, 5).defaultPrevented).toBe(true);
  });

  it("keeps taps and multi-touch pinch zoom available", () => {
    expect(touch("touchstart", link, 100, 100).defaultPrevented).toBe(false);
    expect(touch("touchend", link, 100, 100, 0).defaultPrevented).toBe(false);
    touch("touchstart", link, 100, 100, 2);
    expect(touch("touchmove", link, 50, 50, 2).defaultPrevented).toBe(false);
  });

  it("tracks consecutive movement and direction reversals", () => {
    list.scrollTop = 0;
    touch("touchstart", link, 100, 100);
    expect(touch("touchmove", link, 100, 120).defaultPrevented).toBe(true);
    expect(touch("touchmove", link, 100, 110).defaultPrevented).toBe(false);
  });

  it("clears tracking on cancellation and when touch identity changes", () => {
    touch("touchstart", link, 100, 100);
    touch("touchcancel", link, 100, 100, 0);
    expect(touch("touchmove", link, 100, 120).defaultPrevented).toBe(false);
    touch("touchstart", link, 100, 100, 2);
    expect(touch("touchmove", link, 100, 120).defaultPrevented).toBe(false);
  });

  it("contains wheel scrolling but allows trackpad pinch zoom", () => {
    expect(wheel(link, 0, 40).defaultPrevented).toBe(false);
    expect(wheel(background, 0, 40).defaultPrevented).toBe(true);
    list.scrollTop = 300;
    expect(wheel(link, 0, 40).defaultPrevented).toBe(true);
    expect(wheel(background, 0, 40, true).defaultPrevented).toBe(false);
  });

  it("removes every listener when the drawer closes", () => {
    action.destroy();
    expect(swipe(background, 0, 40).defaultPrevented).toBe(false);
    expect(wheel(background, 0, 40).defaultPrevented).toBe(false);
    touch("touchstart", drawer, 100, 100, 1, drawer);
    expect(touch("touchmove", drawer, 100, 60, 1, drawer).defaultPrevented).toBe(false);
  });
});
