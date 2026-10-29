import type { Terminal } from "@xterm/xterm";

type Point = { x: number; y: number };

// Finger movement is immediate; only a released flick decelerates. Integrate
// decay over elapsed time so 60 Hz and 120 Hz screens travel the same distance.
export function createTouchScroll(
  scroll: (pixels: number, point: Point) => boolean,
  options: {
    schedule?: (callback: FrameRequestCallback) => number;
    cancel?: (frame: number) => void;
    now?: () => number;
    momentum?: () => boolean;
  } = {},
) {
  const schedule = options.schedule ?? requestAnimationFrame;
  const cancel = options.cancel ?? cancelAnimationFrame;
  const now = options.now ?? (() => performance.now());
  const momentum = options.momentum ?? (() => true);
  let gesture: { origin: Point; previous: Point; time: number; scrolling: boolean } | undefined;
  let point: Point = { x: 0, y: 0 };
  let frame: number | undefined;
  let pending = 0, velocity = 0, lastMove = 0, lastFrame = 0;
  let disposed = false;
  const stop = () => {
    if (frame !== undefined) cancel(frame);
    frame = undefined; gesture = undefined; pending = 0; velocity = 0;
  };
  const request = () => { if (frame === undefined && !disposed) frame = schedule(tick); };
  const tick = () => {
    frame = undefined;
    if (disposed) return;
    const time = now(), elapsed = time - lastFrame;
    lastFrame = time;
    let pixels = pending; pending = 0;
    if (!gesture) {
      // A suspended or stalled page must not resume with a large jump.
      if (elapsed > 80 || !momentum()) velocity = 0;
      const decay = Math.exp(-Math.min(34, Math.max(0, elapsed)) / 220);
      pixels += velocity * 220 * (1 - decay);
      velocity *= decay;
    }
    if (pixels && !scroll(pixels, point)) velocity = 0;
    if (!gesture && Math.abs(velocity) >= 0.02) request();
  };
  return {
    start(next: Point) {
      stop();
      if (disposed) return;
      point = next;
      lastMove = now();
      gesture = { origin: next, previous: next, time: lastMove, scrolling: false };
    },
    move(next: Point) {
      if (!gesture || disposed) return false;
      const time = now();
      if (!gesture.scrolling) {
        const x = next.x - gesture.origin.x, y = next.y - gesture.origin.y;
        if (Math.hypot(x, y) < 6) return false;
        if (Math.abs(x) > Math.abs(y)) { stop(); return false; }
        gesture.scrolling = true;
      }
      const distance = gesture.previous.y - next.y;
      const elapsed = Math.max(1, time - gesture.time);
      const speed = Math.max(-3, Math.min(3, distance / elapsed));
      velocity = elapsed > 80 || Math.sign(speed) !== Math.sign(velocity)
        ? speed : velocity + (speed - velocity) * elapsed / (20 + elapsed);
      point = next; pending += distance; lastMove = time;
      gesture.previous = next; gesture.time = time;
      request();
      return true;
    },
    end() {
      if (!gesture || disposed) return;
      if (!gesture.scrolling || now() - lastMove > 80 || !momentum()) velocity = 0;
      gesture = undefined; lastFrame = now();
      if (pending || Math.abs(velocity) >= 0.02) request();
    },
    stop,
    dispose() { stop(); disposed = true; },
  };
}

export function bindTerminalTouch(container: HTMLElement, terminal: Terminal) {
  const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)");
  const mode = () => `${terminal.buffer.active.type}/${terminal.modes.mouseTrackingMode}`;
  let gestureMode = "", lineHeight = 0, remainder = 0;
  let identifier: number | undefined;
  const visible = () => document.visibilityState !== "hidden" && !container.closest("[hidden], [inert]");
  const scroll = createTouchScroll((pixels, point) => {
    if (!visible() || mode() !== gestureMode || !lineHeight) return false;
    if (terminal.buffer.active.type === "alternate" || terminal.modes.mouseTrackingMode !== "none") {
      // Application mouse/alternate-screen protocols are line based. Preserve
      // fractional travel, and let xterm encode the app's active protocol.
      remainder += pixels;
      const lines = Math.trunc(remainder / lineHeight);
      remainder -= lines * lineHeight;
      const screen = terminal.element?.querySelector(".xterm-screen");
      for (let i = 0; i < Math.abs(lines); i++) screen?.dispatchEvent(new WheelEvent("wheel", {
        bubbles: true, cancelable: true, deltaMode: WheelEvent.DOM_DELTA_LINE,
        deltaY: Math.sign(lines), clientX: point.x, clientY: point.y,
      }));
    } else {
      const buffer = terminal.buffer.active;
      if (pixels < 0 ? buffer.viewportY <= 0 : buffer.viewportY >= buffer.baseY) return false;
      // xterm's public API renders whole rows. Carry fractional travel across
      // frames rather than losing slow finger movement or using private APIs.
      remainder += pixels;
      const lines = Math.trunc(remainder / lineHeight);
      remainder -= lines * lineHeight;
      if (lines) terminal.scrollLines(lines);
    }
    return true;
  }, { momentum: () => !reducedMotion.matches });
  const stop = () => { identifier = undefined; remainder = 0; scroll.stop(); };
  const start = (event: TouchEvent) => {
    stop();
    if (event.touches.length !== 1 || !visible() || (event.target as Element).closest(".scrollbar")) return;
    const height = terminal.element?.querySelector(".xterm-screen")?.getBoundingClientRect().height;
    if (!height) return;
    lineHeight = height / terminal.rows; gestureMode = mode();
    const point = event.touches[0]; identifier = point.identifier;
    scroll.start({ x: point.clientX, y: point.clientY });
  };
  const move = (event: TouchEvent) => {
    if (event.touches.length !== 1) { stop(); return; }
    const point = event.touches[0];
    if (point.identifier !== identifier) return;
    if (scroll.move({ x: point.clientX, y: point.clientY })) event.preventDefault();
  };
  const end = (event: TouchEvent) => {
    if (event.touches.length) { stop(); return; }
    identifier = undefined; scroll.end();
  };
  const wheel = (event: WheelEvent) => { if (event.isTrusted) stop(); };
  const resize = terminal.onResize(stop);
  container.addEventListener("touchstart", start, { passive: true });
  container.addEventListener("touchmove", move, { passive: false });
  container.addEventListener("touchend", end);
  container.addEventListener("touchcancel", stop);
  // Any new interaction takes over from a fling, including the toolbar and
  // tab/view controls outside this container. Synthetic app wheels are ours.
  document.addEventListener("pointerdown", stop, true);
  document.addEventListener("keydown", stop, true);
  document.addEventListener("paste", stop, true);
  document.addEventListener("wheel", wheel, { capture: true, passive: true });
  document.addEventListener("visibilitychange", stop);
  window.addEventListener("blur", stop);
  return {
    stop,
    dispose() {
      scroll.dispose();
      resize.dispose();
      container.removeEventListener("touchstart", start);
      container.removeEventListener("touchmove", move);
      container.removeEventListener("touchend", end);
      container.removeEventListener("touchcancel", stop);
      document.removeEventListener("pointerdown", stop, true);
      document.removeEventListener("keydown", stop, true);
      document.removeEventListener("paste", stop, true);
      document.removeEventListener("wheel", wheel, true);
      document.removeEventListener("visibilitychange", stop);
      window.removeEventListener("blur", stop);
    },
  };
}
