type Keyboard = EventTarget & { overlaysContent: boolean; boundingRect: DOMRectReadOnly };

// Explicit, client-only diagnostics. Do not read terminal text, input values,
// storage, cookies or connection identifiers, and never transmit a report.
export function installViewportReport() {
  const keyboard = (navigator as Navigator & { virtualKeyboard?: Keyboard }).virtualKeyboard;
  const viewport = window.visualViewport;
  const probe = (prefix: string) => {
    const element = document.createElement("div");
    element.setAttribute("aria-hidden", "true");
    element.style.cssText = `position:fixed;visibility:hidden;pointer-events:none;left:0;top:0;width:0;height:0;overflow:hidden;padding:env(${prefix}-top,0px) env(${prefix}-right,0px) env(${prefix}-bottom,0px) env(${prefix}-left,0px)`;
    document.body.append(element);
    return element;
  };
  const safe = probe("safe-area-inset"), maximum = probe("safe-area-max-inset"), keyboardInset = probe("keyboard-inset");
  const insets = (element: HTMLElement) => {
    const style = getComputedStyle(element);
    return { top: parseFloat(style.paddingTop), right: parseFloat(style.paddingRight), bottom: parseFloat(style.paddingBottom), left: parseFloat(style.paddingLeft) };
  };
  const rect = (element?: Element | DOMRectReadOnly | null) => {
    if (!element) return null;
    const box = element instanceof Element ? element.getBoundingClientRect() : element;
    return { top: box.top, bottom: box.bottom, left: box.left, width: box.width, height: box.height };
  };
  const snapshot = () => {
    const app = document.querySelector(".app"), bar = document.querySelector(".terminal-quick-controls");
    const appStyle = app ? getComputedStyle(app) : null;
    const focused = document.activeElement;
    return {
      time: Math.round(performance.now()),
      window: { innerHeight, innerWidth, outerHeight, outerWidth, scrollX, scrollY, layoutHeight: document.documentElement.clientHeight, layoutWidth: document.documentElement.clientWidth },
      visualViewport: viewport ? { height: viewport.height, width: viewport.width, offsetTop: viewport.offsetTop, offsetLeft: viewport.offsetLeft, pageTop: viewport.pageTop, scale: viewport.scale } : null,
      keyboard: keyboard ? { overlaysContent: keyboard.overlaysContent, bounds: rect(keyboard.boundingRect) } : null,
      safeArea: insets(safe), maximumSafeArea: insets(maximum), keyboardInsets: insets(keyboardInset),
      app: rect(app), toolbar: rect(bar),
      paddingBottom: appStyle?.paddingBottom,
      viewportVariables: ["--viewport-height", "--viewport-top", "--viewport-bottom-occlusion", "--safe-area-bottom"].map(name => [name, getComputedStyle(document.documentElement).getPropertyValue(name)]),
      focused: focused ? { tag: focused.tagName, bounds: rect(focused), fontSize: getComputedStyle(focused).fontSize, inputMode: focused instanceof HTMLElement ? focused.inputMode : undefined, readOnly: focused instanceof HTMLTextAreaElement ? focused.readOnly : undefined } : null,
    };
  };
  const initial = snapshot();
  const summarize = (value: ReturnType<typeof snapshot>) => ({
    time: value.time, innerHeight: value.window.innerHeight, layoutHeight: value.window.layoutHeight,
    viewportHeight: value.visualViewport?.height, viewportTop: value.visualViewport?.offsetTop, scale: value.visualViewport?.scale,
    keyboardTop: value.keyboard?.bounds?.top, keyboardHeight: value.keyboard?.bounds?.height,
    safeBottom: value.safeArea.bottom, maximumSafeBottom: value.maximumSafeArea.bottom,
    paddingBottom: value.paddingBottom, appBottom: value.app?.bottom, toolbarBottom: value.toolbar?.bottom,
  });
  let recent: ReturnType<typeof summarize>[] = [];
  const sample = () => { place(); recent.push(summarize(snapshot())); recent = recent.slice(-6); };
  const panel = document.createElement("aside");
  panel.setAttribute("aria-label", "Keyboard layout diagnostics");
  panel.style.cssText = "position:fixed;top:4px;right:4px;z-index:2147483647;max-width:calc(100vw - 8px);font-size:12px";
  const button = document.createElement("button");
  button.type = "button"; button.textContent = "Copy layout report";
  button.setAttribute("aria-label", "Copy keyboard layout report");
  button.style.cssText = "min-height:44px;background:#1f2937;color:white;border:1px solid #9ca3af;padding:8px";
  button.addEventListener("pointerdown", event => event.preventDefault());
  const output = document.createElement("pre");
  output.hidden = true; output.tabIndex = 0;
  output.setAttribute("aria-label", "Keyboard layout report");
  output.style.cssText = "position:fixed;top:54px;right:4px;left:4px;max-height:35vh;overflow:auto;white-space:pre-wrap;background:#111827;color:white;padding:8px;font-size:10px";
  button.addEventListener("click", () => {
    // Snapshot before any clipboard permission UI can affect keyboard focus.
    const report = JSON.stringify({
      schema: 1, marker: "ormos-keyboard-layout-report-1", browser: navigator.userAgent,
      secureContext: isSecureContext, devicePixelRatio,
      screen: { width: screen.width, height: screen.height, availHeight: screen.availHeight, orientation: screen.orientation?.type },
      standalone: matchMedia("(display-mode: standalone)").matches,
      script: document.querySelector<HTMLScriptElement>('script[type="module"][src]')?.getAttribute("src"),
      initial, current: snapshot(), recent,
    }, null, 2);
    output.textContent = report;
    if (navigator.clipboard?.writeText) {
      void navigator.clipboard.writeText(report).then(() => { button.textContent = "Copied — paste into chat"; }, () => { output.hidden = false; button.textContent = "Select and copy report below"; });
    } else { output.hidden = false; button.textContent = "Select and copy report below"; }
  });
  panel.append(button, output); document.body.append(panel);
  function place() {
    const top = Math.max(0, viewport?.offsetTop ?? 0), left = Math.max(0, viewport?.offsetLeft ?? 0);
    const width = viewport?.width ?? innerWidth;
    panel.style.top = `${top + 4}px`; panel.style.right = "auto";
    panel.style.maxWidth = `${width - 8}px`;
    panel.style.left = `${Math.max(left + 4, left + width - panel.getBoundingClientRect().width - 4)}px`;
    output.style.top = `${top + 54}px`; output.style.left = `${left + 4}px`;
  }
  place();
  const timer = window.setInterval(sample, 250);
  viewport?.addEventListener("resize", sample);
  viewport?.addEventListener("scroll", sample);
  keyboard?.addEventListener("geometrychange", sample);
  return () => {
    clearInterval(timer); viewport?.removeEventListener("resize", sample); viewport?.removeEventListener("scroll", sample); keyboard?.removeEventListener("geometrychange", sample);
    panel.remove(); safe.remove(); maximum.remove(); keyboardInset.remove();
  };
}
