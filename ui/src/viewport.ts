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
  const properties = ["--viewport-height", "--viewport-top", "--viewport-bottom-occlusion"];
  const previous = properties.map(property => style.getPropertyValue(property));
  let unzoomed = { height: window.innerHeight, top: 0 }, keyboardWasVisible = false;
  const update = () => {
    const rect = keyboard?.boundingRect;
    const keyboardVisible = !!rect && rect.height > 0 && rect.width > 0;
    const keyboardClosed = keyboardWasVisible && !keyboardVisible;
    keyboardWasVisible = keyboardVisible;
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
    // The docked keyboard already covers the navigation safe area. Subtract
    // the actual occlusion once, rather than adding that inset above it too.
    const occluded = Math.max(0, document.documentElement.clientHeight - bottom);
    style.setProperty("--viewport-height", `${height}px`);
    style.setProperty("--viewport-top", `${top}px`);
    style.setProperty("--viewport-bottom-occlusion", `${occluded}px`);
  };
  keyboard?.addEventListener("geometrychange", update);
  viewport?.addEventListener("resize", update);
  viewport?.addEventListener("scroll", update);
  window.addEventListener("resize", update);
  window.addEventListener("pageshow", update);
  update();
  return () => {
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
