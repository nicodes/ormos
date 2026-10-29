// Mobile keyboards can resize and pan the visual viewport independently of
// the layout viewport. Keep the entire app inside its visible bounds so the
// terminal toolbar sits at the keyboard edge instead of the document edge.
export function bindAppViewport() {
  const viewport = window.visualViewport;
  const style = document.documentElement.style;
  const properties = ["--viewport-height", "--viewport-top", "--viewport-bottom-occlusion"];
  const previous = properties.map(property => style.getPropertyValue(property));
  const update = () => {
    // Let the browser magnify/pan the existing layout during pinch zoom.
    // Resizing the terminal to the zoomed viewport would reflow its content.
    if (viewport && Math.abs(viewport.scale - 1) > 0.01) return;
    const height = viewport?.height ?? window.innerHeight;
    const top = Math.max(0, viewport?.offsetTop ?? 0);
    const occluded = Math.max(0, document.documentElement.clientHeight - top - height);
    style.setProperty("--viewport-height", `${height}px`);
    style.setProperty("--viewport-top", `${top}px`);
    style.setProperty("--viewport-bottom-occlusion", `${occluded}px`);
  };
  viewport?.addEventListener("resize", update);
  viewport?.addEventListener("scroll", update);
  window.addEventListener("resize", update);
  window.addEventListener("pageshow", update);
  update();
  return () => {
    viewport?.removeEventListener("resize", update);
    viewport?.removeEventListener("scroll", update);
    window.removeEventListener("resize", update);
    window.removeEventListener("pageshow", update);
    properties.forEach((property, index) => {
      if (previous[index]) style.setProperty(property, previous[index]);
      else style.removeProperty(property);
    });
  };
}
