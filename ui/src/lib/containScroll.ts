/**
 * Keep modal scroll gestures in the modal without disabling pointer events on
 * the body. CSS overscroll containment alone is not sufficient on iOS.
 * Native scrolling (and momentum) is left alone when a scroller has room.
 */
export function containScroll(node: HTMLElement): { destroy(): void } {
  const document = node.ownerDocument;
  const handledTouches = new WeakSet<Event>();
  let previous: { id: number; x: number; y: number } | undefined;

  function canConsume(target: EventTarget | null, dx: number, dy: number): boolean {
    if (!(target instanceof Element) || !node.contains(target)) return false;
    const vertical = Math.abs(dy) >= Math.abs(dx);
    const delta = vertical ? dy : dx;
    if (delta === 0) return true;

    for (let element: Element | null = target; element; element = element.parentElement) {
      if (element instanceof HTMLElement) {
        const style = getComputedStyle(element);
        const overflow = vertical ? style.overflowY : style.overflowX;
        if (overflow === "auto" || overflow === "scroll") {
          const position = vertical ? element.scrollTop : element.scrollLeft;
          const maximum = vertical
            ? element.scrollHeight - element.clientHeight
            : element.scrollWidth - element.clientWidth;
          if (maximum > 0 && (delta < 0 ? position > 0 : position < maximum)) return true;
        }
      }
      if (element === node) break;
    }
    return false;
  }

  function remember(event: TouchEvent): void {
    const touch = event.touches.length === 1 ? event.touches[0] : undefined;
    previous = touch ? { id: touch.identifier, x: touch.clientX, y: touch.clientY } : undefined;
  }

  function onTouchMove(event: TouchEvent): void {
    // Leave pinch zoom and other multi-touch browser gestures intact.
    if (event.touches.length !== 1) {
      previous = undefined;
      return;
    }
    const touch = event.touches[0];
    if (previous?.id === touch.identifier && !canConsume(
      event.target, previous.x - touch.clientX, previous.y - touch.clientY,
    )) event.preventDefault();
    remember(event);
  }

  function onWheel(event: WheelEvent): void {
    if (!event.ctrlKey && !canConsume(event.target, event.deltaX, event.deltaY)) event.preventDefault();
  }

  function onTouch(event: Event): void {
    // The same gesture can reach both the document and drawer listeners.
    if (handledTouches.has(event)) return;
    handledTouches.add(event);
    const touch = event as TouchEvent;
    if (event.type === "touchmove") onTouchMove(touch);
    else remember(touch);
  }

  // Keep real touch handlers on the drawer itself, not only the document:
  // WebKit uses local handlers when building its touch-event regions. The
  // document capture listener also contains backdrop/background gestures.
  // Do not cancel touchstart or click: ordinary links must still work.
  const touchTargets = [
    { target: document, capture: true },
    { target: node, capture: true },
    { target: node, capture: false },
  ];
  const touchTypes = ["touchstart", "touchmove", "touchend", "touchcancel"] as const;
  for (const { target, capture } of touchTargets) {
    for (const type of touchTypes) {
      target.addEventListener(type, onTouch, { capture, passive: type !== "touchmove" });
    }
  }
  document.addEventListener("wheel", onWheel, { capture: true, passive: false });

  return {
    destroy() {
      for (const { target, capture } of touchTargets) {
        for (const type of touchTypes) {
          target.removeEventListener(type, onTouch, { capture });
        }
      }
      document.removeEventListener("wheel", onWheel, { capture: true });
    },
  };
}
