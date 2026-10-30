type KeyboardGeometry = EventTarget & { overlaysContent: boolean; readonly boundingRect: DOMRectReadOnly };

// Prefer the keyboard's actual bounds on Android. Some browser/OS combinations
// report a visual viewport that still extends beneath the keyboard/system bar.
export function bindAppViewport() {
  const viewport = window.visualViewport;
  let keyboard = (navigator as Navigator & { virtualKeyboard?: KeyboardGeometry }).virtualKeyboard;
  const previousOverlay = keyboard?.overlaysContent;
  if (keyboard) {
    try { keyboard.overlaysContent = true; } catch { /* Use native viewport resizing when unavailable. */ }
    if (!keyboard.overlaysContent) keyboard = undefined;
  }
  const style = document.documentElement.style;
  const properties = ["--viewport-height", "--viewport-top", "--viewport-bottom-occlusion", "--keyboard-safe-area-bottom"];
  const previous = properties.map(property => style.getPropertyValue(property));
  let unzoomed = { height: window.innerHeight, top: 0 }, keyboardWasVisible = false;
  // Chrome can clear both live and maximum insets when the keyboard opens.
  // Preserve a measured closed-keyboard inset, scoped to the viewport width.
  const closedInsets = new Map<number, number>();
  const sensor = document.createElement("div");
  sensor.setAttribute("aria-hidden", "true");
  sensor.style.cssText = "position:fixed;visibility:hidden;pointer-events:none;width:0;height:0;left:0;top:0;padding-bottom:var(--safe-area-bottom,0px)";
  document.body.append(sensor);
  const update = () => {
    const rect = keyboard?.boundingRect;
    const keyboardVisible = !!rect && rect.height > 0 && rect.width > 0;
    const keyboardClosed = keyboardWasVisible && !keyboardVisible;
    keyboardWasVisible = keyboardVisible;
    const width = document.documentElement.clientWidth;
    const liveInset = parseFloat(getComputedStyle(sensor).paddingBottom) || 0;
    // Do not replace a positive measurement with the transient zero emitted
    // just before geometrychange. Closed layout still uses the live CSS inset.
    if (!keyboardVisible && liveInset > 0) closedInsets.set(width, liveInset);
    if (keyboardVisible) style.setProperty("--keyboard-safe-area-bottom", `${Math.max(liveInset, closedInsets.get(width) ?? 0)}px`);
    else style.removeProperty("--keyboard-safe-area-bottom");
    const zoomed = !!viewport && Math.abs(viewport.scale - 1) > 0.01;
    // Preserve pinch zoom while browsing, but never ignore an actual keyboard
    // boundary. Closing it while zoomed restores the last unzoomed layout.
    if (zoomed && !keyboardVisible && !keyboardClosed) return;
    const bounds = zoomed && keyboardClosed ? unzoomed : {
      height: viewport?.height ?? window.innerHeight,
      top: Math.max(0, viewport?.offsetTop ?? 0),
    };
    if (!zoomed) unzoomed = bounds;
    const { top } = bounds;
    const bottom = keyboardVisible ? Math.min(top + bounds.height, rect.top) : top + bounds.height;
    const height = Math.max(0, bottom - top);
    // Native viewport clipping already consumes the system inset. An overlay
    // keyboard does not: preserve the browser-reported safe area above its
    // bounds. Using the keyboard-clamped bottom here erased that padding.
    const occluded = Math.max(0, document.documentElement.clientHeight - (top + bounds.height));
    style.setProperty("--viewport-height", `${height}px`);
    style.setProperty("--viewport-top", `${top}px`);
    style.setProperty("--viewport-bottom-occlusion", `${occluded}px`);
  };
  // Insets can change without a viewport event as Chrome's controls move.
  const insetObserver = new ResizeObserver(update);
  insetObserver.observe(sensor, { box: "border-box" });
  keyboard?.addEventListener("geometrychange", update);
  viewport?.addEventListener("resize", update);
  viewport?.addEventListener("scroll", update);
  window.addEventListener("resize", update);
  window.addEventListener("pageshow", update);
  update();
  return () => {
    insetObserver.disconnect(); sensor.remove();
    keyboard?.removeEventListener("geometrychange", update);
    viewport?.removeEventListener("resize", update);
    viewport?.removeEventListener("scroll", update);
    window.removeEventListener("resize", update);
    window.removeEventListener("pageshow", update);
    if (keyboard && previousOverlay !== undefined) {
      try { keyboard.overlaysContent = previousOverlay; } catch { /* The browser may revoke access. */ }
    }
    properties.forEach((property, index) => {
      if (previous[index]) style.setProperty(property, previous[index]);
      else style.removeProperty(property);
    });
  };
}
