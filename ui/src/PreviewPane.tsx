import { createEffect, createMemo, createSignal, For, on, onCleanup, onMount, Show } from "solid-js";

import { Portal } from "solid-js/web";
import { readStorage, writeStorage, removeStorage } from "./storage";
import BrowserMenu from "./BrowserMenu";
import { APIError, request } from "./api";
import { parsePreviewAddress, previewURL, type PreviewTarget } from "./preview";

type PreviewHistory = { history: PreviewTarget[]; index: number };
const addressFor = (target?: PreviewTarget) => target ? `${target.port}${target.path}` : "";

export default function PreviewPane(props: { onError: (error: string) => void; header: HTMLDivElement; visible: boolean }) {
  const workspace = new URL(location.href);
  const parseAddress = (raw: string, currentPort?: number) => parsePreviewAddress(raw, workspace, currentPort);
  const validHistory = (value: unknown): value is PreviewHistory => {
    if (!value || typeof value !== "object") return false;
    const row = value as PreviewHistory;
    return Array.isArray(row.history) && Number.isInteger(row.index) && row.index >= -1 && row.index < row.history.length &&
      row.history.every(item => {
        try { return !!item && typeof item.path === "string" && !!previewURL(item, workspace); }
        catch { return false; }
      });
  };
  const loadHistory = (): PreviewHistory => {
    try {
      const saved = JSON.parse(readStorage("ormos.previewHistory") ?? "null");
      if (validHistory(saved)) return saved;
      // Keep the previously selected preview when migrating away from tabs.
      const tabs = JSON.parse(readStorage("ormos.previewTabs") ?? "null");
      if (Array.isArray(tabs)) {
        const selected = tabs.find(tab => tab?.id === readStorage("ormos.activePreview")) ?? tabs[0];
        if (validHistory(selected)) return { history: selected.history, index: selected.index };
      }
    } catch { /* invalid saved state starts with an empty preview */ }
    const legacy = readStorage("ormos.previewPort");
    if (legacy) {
      try { return { history: [parseAddress(legacy)], index: 0 }; } catch { /* ignore invalid legacy port */ }
    }
    return { history: [], index: -1 };
  };
  const [current, setCurrent] = createSignal(loadHistory());
  const uniqueRecent = (rows: PreviewTarget[]) => {
    const seen = new Set<string>();
    return rows.filter(item => { const key = addressFor(item); if (seen.has(key)) return false; seen.add(key); return true; }).slice(0, 50);
  };
  const loadRecent = () => {
    try {
      const saved = JSON.parse(readStorage("ormos.previewRecent") ?? "null");
      if (Array.isArray(saved) && validHistory({ history: saved, index: -1 })) return uniqueRecent(saved);
    } catch { /* seed recent visits from existing navigation history */ }
    return uniqueRecent([...current().history].reverse());
  };
  const [recent, setRecent] = createSignal(loadRecent());
  const remember = (item?: PreviewTarget) => {
    if (!item) return;
    setRecent(rows => uniqueRecent([item, ...rows]));
    writeStorage("ormos.previewRecent", JSON.stringify(recent()));
  };
  const target = () => current().history[current().index];
  const [address, setAddress] = createSignal(addressFor(target()));
  const [navigation, setNavigation] = createSignal(0);
  let addressInput!: HTMLInputElement;
  let historyPanel: HTMLDivElement | undefined;
  const [addressFocused, setAddressFocused] = createSignal(false);
  const [historyDismissed, setHistoryDismissed] = createSignal(false);
  const [historyIndex, setHistoryIndex] = createSignal(-1);
  const [activePorts, setActivePorts] = createSignal<Set<number> | null>(null);
  const historyShown = () => props.visible && addressFocused() && !historyDismissed();
  const filteredRecent = createMemo(() => {
    const query = address().trim().toLowerCase();
    return recent().filter(item => [addressFor(item), `http://localhost:${addressFor(item)}`, `http://127.0.0.1:${addressFor(item)}`, `http://${location.hostname}:${addressFor(item)}`].some(value => value.toLowerCase().includes(query)));
  });
  const [historyPosition, setHistoryPosition] = createSignal({ top: "0px", left: "0px", width: "200px", "max-height": "240px" });
  const placeHistory = () => {
    const rect = addressInput.getBoundingClientRect();
    const height = window.visualViewport?.height ?? window.innerHeight;
    setHistoryPosition({ top: `${rect.bottom + 4}px`, left: `${rect.left}px`, width: `${rect.width}px`, "max-height": `${Math.max(40, Math.min(240, height - rect.bottom - 12))}px` });
  };
  createEffect(on(historyShown, shown => {
    if (!shown) return;
    placeHistory(); setActivePorts(null);
    let live = true;
    let timer: number | undefined;
    const controller = new AbortController();
    const refreshPorts = async () => {
      try {
        const data = await request<{ ports: { port: number }[] }>("/api/ports", undefined, controller.signal);
        if (live) setActivePorts(new Set(data.ports.map(row => row.port)));
      } catch { if (live) setActivePorts(null); }
      finally { if (live) timer = window.setTimeout(() => void refreshPorts(), 3000); }
    };
    void refreshPorts();
    onCleanup(() => { live = false; window.clearTimeout(timer); controller.abort(); });
  }));
  const persist = () => {
    if (!writeStorage("ormos.previewHistory", JSON.stringify(current()))) return;
    removeStorage("ormos.previewTabs");
    removeStorage("ormos.activePreview");
    removeStorage("ormos.previewPort");
  };
  const record = (next: PreviewTarget) => {
    remember(next);
    const previous = target();
    if (previous?.port === next.port && previous.path === next.path) { setAddress(addressFor(next)); return; }
    setCurrent(row => {
      const history = [...row.history.slice(0, row.index + 1), next].slice(-200);
      return { history, index: history.length - 1 };
    });
    persist(); setAddress(addressFor(next));
  };
  const navigate = () => {
    try {
      const next = parseAddress(address(), target()?.port);
      props.onError(""); record(next); setNavigation(n => n + 1); setHistoryDismissed(true);
    } catch (e) { props.onError(String(e)); }
  };
  const move = (offset: number) => {
    const tab = current(); const index = tab.index + offset;
    if (index < 0 || index >= tab.history.length) return;
    setCurrent(row => ({ ...row, index })); remember(target()); persist(); setAddress(addressFor(target())); setNavigation(n => n + 1);
  };
  const visitRecent = (item: PreviewTarget) => { setAddress(addressFor(item)); navigate(); setHistoryDismissed(true); addressInput.blur(); };
  const historyKey = (event: KeyboardEvent) => {
    if (!historyShown()) return;
    if (event.key === "Escape") { event.preventDefault(); event.stopPropagation(); setHistoryDismissed(true); }
    else if (["ArrowDown", "ArrowUp"].includes(event.key) && filteredRecent().length) {
      event.preventDefault();
      setHistoryIndex(index => (index + (event.key === "ArrowDown" ? 1 : index < 0 ? 0 : -1) + filteredRecent().length) % filteredRecent().length);
      historyPanel?.querySelector('[aria-selected="true"]')?.scrollIntoView({ block: "nearest" });
    } else if (event.key === "Enter" && historyIndex() >= 0) { event.preventDefault(); visitRecent(filteredRecent()[historyIndex()]); }
  };
  const [frameTarget, setFrameTarget] = createSignal<{ url: string }>();
  const [unavailableMessage, setUnavailableMessage] = createSignal("Start your app in the terminal, then refresh.");
  const [availability, setAvailability] = createSignal<"empty" | "checking" | "ready" | "unavailable">("empty");
  createEffect(on(navigation, () => {
    const item = target();
    setFrameTarget(undefined);
    if (!item) { setAvailability("empty"); return; }
    setAvailability("checking");
    const controller = new AbortController();
    let live = true;
    const timeout = window.setTimeout(() => controller.abort(), 20000);
    void (async () => {
      const exposed = await request<{ port: number }>("/api/preview", { port: item.port, scheme: workspace.protocol.slice(0, -1) }, controller.signal);
      const url = previewURL({ ...item, port: exposed.port }, workspace);
      await fetch(url, { method: "HEAD", mode: "no-cors", credentials: "omit", cache: "no-store", signal: controller.signal });
      if (live) { setFrameTarget({ url }); setAvailability("ready"); }
    })()
      .catch(error => { if (live) { setUnavailableMessage(error instanceof APIError ? error.message : "This app is not reachable yet. Check Tailscale access, then refresh."); setAvailability("unavailable"); } })
      .finally(() => window.clearTimeout(timeout));
    onCleanup(() => { live = false; window.clearTimeout(timeout); controller.abort(); });
  }));
  onMount(() => {
    persist();
    const resize = () => { if (historyShown()) placeHistory(); };
    window.addEventListener("resize", resize); window.visualViewport?.addEventListener("resize", resize);
    onCleanup(() => { window.removeEventListener("resize", resize); window.visualViewport?.removeEventListener("resize", resize); });
  });
  return (
    <>
      <Portal mount={props.header} ref={element => { element.className = "preview-toolbar"; }}>
      <form class="browserbar" hidden={!props.visible} onSubmit={e => { e.preventDefault(); navigate(); }}>
        <input ref={addressInput} role="combobox" aria-expanded={historyShown()} aria-controls="preview-history-list" aria-activedescendant={historyShown() && historyIndex() >= 0 ? `preview-history-${historyIndex()}` : undefined} aria-autocomplete="none" aria-label="Preview address" placeholder="Port or localhost URL" enterkeyhint="go" autocomplete="off" autocapitalize="none" spellcheck={false} value={address()} onFocus={() => { setHistoryDismissed(false); setHistoryIndex(-1); setAddressFocused(true); }} onBlur={() => setAddressFocused(false)} onKeyDown={historyKey} onInput={e => { setAddress(e.currentTarget.value); setHistoryDismissed(false); setHistoryIndex(-1); }} />
        <BrowserMenu visible={props.visible} canGoBack={current().index > 0} canGoForward={current().index < current().history.length - 1} canRefresh={!!target()} back={() => move(-1)} forward={() => move(1)} refresh={() => { remember(target()); setNavigation(n => n + 1); }} openURL={frameTarget()?.url ?? ""} />
      </form>
      </Portal>
      <Show when={historyShown()}><Portal>
        <div ref={historyPanel} class="preview-history" style={historyPosition()} onPointerDown={event => event.preventDefault()}>
          <div id="preview-history-list" role="listbox" aria-label="Recent previews">
          <Show when={filteredRecent().length} fallback={<div class="empty-message history-empty" role="status"><strong>{address().trim() ? "No matching history" : "Enter a port"}</strong><p>{address().trim() ? "Press Enter to open a new address." : "Your recent previews will appear here."}</p></div>}>
            <For each={filteredRecent()}>{(item, index) => <button id={`preview-history-${index()}`} type="button" role="option" aria-selected={historyIndex() === index()} onClick={() => visitRecent(item)}>
              <span class="status-dot" classList={{ online: activePorts()?.has(item.port) === true }} role="img" aria-label={activePorts() === null ? "Port status unavailable" : activePorts()!.has(item.port) ? "Port active" : "Port inactive"} title={activePorts() === null ? "Port status unavailable" : activePorts()!.has(item.port) ? "Port active" : "Port inactive"} /><span>{addressFor(item)}</span>
            </button>}</For>
          </Show></div>
        </div>
      </Portal></Show>
      <div id="preview-content" class="pane-content">
      <Show keyed when={frameTarget()} fallback={<div class="empty"><div class="preview-empty-content" role="status">
        <h1>{availability() === "unavailable" ? "App unavailable" : availability() === "checking" ? "Opening app" : address().trim() ? "Ready to preview" : "Enter a port"}</h1>
        <p>{availability() === "unavailable" ? unavailableMessage() : availability() === "checking" ? "Connecting to this port." : address().trim() ? "Press Enter to open your address." : "Open a local app, right here."}</p>
      </div></div>}>
        {item => <iframe title="Local app preview" src={item.url} sandbox="allow-scripts allow-same-origin allow-forms allow-downloads allow-popups" referrerpolicy="no-referrer" allow="" />}
      </Show>
      </div>
    </>
  );
}
