import { createEffect, createMemo, createSignal, For, on, onCleanup, onMount, Show } from "solid-js";

import { Portal } from "solid-js/web";
import BrowserMenu from "./BrowserMenu";
import { request } from "./api";

export type PreviewTarget = { port: number; path: string };
type PreviewHistory = { history: PreviewTarget[]; index: number };
const addressFor = (target?: PreviewTarget) => target ? `${target.port}${target.path}` : "";

export function parsePreviewAddress(raw: string, currentPort?: number): PreviewTarget {
  const value = raw.trim();
  let port: number;
  let path: string;
  const short = /^:?(\d{1,5})([/?#].*)?$/.exec(value);
  if (short) { port = Number(short[1]); path = short[2] ?? "/"; }
  else if (/^https?:\/\//i.test(value)) {
    const url = new URL(value);
    const allowed = new Set(["localhost", "127.0.0.1", "[::1]", location.hostname]);
    if (!allowed.has(url.hostname) || url.username || url.password) throw new Error("Use a local app port or localhost URL.");
    port = Number(url.port || (url.protocol === "https:" ? 443 : 80));
    if (url.protocol !== "http:") throw new Error("Local app previews currently use HTTP.");
    path = url.pathname + url.search + url.hash;
  } else if (currentPort) { port = currentPort; path = value.startsWith("/") ? value : `/${value}`; }
  else throw new Error("Enter a local port, for example 39123, or a localhost URL.");
  if (!Number.isInteger(port) || port < 1 || port > 65535) throw new Error("Enter a port between 1 and 65535.");
  if (!path.startsWith("/")) path = `/${path}`;
  const normalized = new URL(path, "http://localhost");
  if (normalized.origin !== "http://localhost") throw new Error("Use a local path.");
  return { port, path: normalized.pathname + normalized.search + normalized.hash };
}

export default function PreviewPane(props: { origin: string; onError: (error: string) => void; header: HTMLDivElement; visible: boolean }) {
  const validHistory = (value: unknown): value is PreviewHistory => {
    if (!value || typeof value !== "object") return false;
    const row = value as PreviewHistory;
    return Array.isArray(row.history) && Number.isInteger(row.index) && row.index >= -1 && row.index < row.history.length &&
      row.history.every(item => item && Number.isInteger(item.port) && item.port > 0 && item.port <= 65535 &&
        typeof item.path === "string" && item.path.startsWith("/") && !item.path.startsWith("//"));
  };
  const loadHistory = (): PreviewHistory => {
    try {
      const saved = JSON.parse(localStorage.getItem("ormos.previewHistory") ?? "null");
      if (validHistory(saved)) return saved;
      // Keep the previously selected preview when migrating away from tabs.
      const tabs = JSON.parse(localStorage.getItem("ormos.previewTabs") ?? "null");
      if (Array.isArray(tabs)) {
        const selected = tabs.find(tab => tab?.id === localStorage.getItem("ormos.activePreview")) ?? tabs[0];
        if (validHistory(selected)) return { history: selected.history, index: selected.index };
      }
    } catch { /* invalid saved state starts with an empty preview */ }
    const legacy = localStorage.getItem("ormos.previewPort");
    if (legacy) {
      try { return { history: [parsePreviewAddress(legacy)], index: 0 }; } catch { /* ignore invalid legacy port */ }
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
      const saved = JSON.parse(localStorage.getItem("ormos.previewRecent") ?? "null");
      if (Array.isArray(saved) && validHistory({ history: saved, index: -1 })) return uniqueRecent(saved);
    } catch { /* seed recent visits from existing navigation history */ }
    return uniqueRecent([...current().history].reverse());
  };
  const [recent, setRecent] = createSignal(loadRecent());
  const remember = (item?: PreviewTarget) => {
    if (!item) return;
    setRecent(rows => uniqueRecent([item, ...rows]));
    localStorage.setItem("ormos.previewRecent", JSON.stringify(recent()));
  };
  const target = () => current().history[current().index];
  const [address, setAddress] = createSignal(addressFor(target()));
  const [navigation, setNavigation] = createSignal(0);
  let frame: HTMLIFrameElement | undefined;
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
    const refreshPorts = async () => {
      try {
        const data = await request<{ ports: { port: number }[] }>("/api/ports");
        if (live) setActivePorts(new Set(data.ports.map(row => row.port)));
      } catch { if (live) setActivePorts(null); }
    };
    void refreshPorts(); const timer = window.setInterval(() => void refreshPorts(), 3000);
    onCleanup(() => { live = false; window.clearInterval(timer); });
  }));
  const persist = () => {
    localStorage.setItem("ormos.previewHistory", JSON.stringify(current()));
    localStorage.removeItem("ormos.previewTabs");
    localStorage.removeItem("ormos.activePreview");
    localStorage.removeItem("ormos.previewPort");
  };
  const record = (next: PreviewTarget, replace = false) => {
    remember(next);
    const previous = target();
    if (previous?.port === next.port && previous.path === next.path) { setAddress(addressFor(next)); return; }
    setCurrent(row => {
      const history = row.history.slice(0, row.index + 1);
      if (replace && history.length) history[history.length - 1] = next;
      else history.push(next);
      return { ...row, history, index: history.length - 1 };
    });
    persist(); setAddress(addressFor(next));
  };
  const navigate = () => {
    try {
      const next = parsePreviewAddress(address(), target()?.port);
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
  const [source, setSource] = createSignal("");
  createEffect(on(() => [navigation(), props.origin], () => {
    const item = target();
    setSource(item && props.origin
      ? `${props.origin.replace(/\/$/, "")}/__ormos_preview/${item.port}/?path=${encodeURIComponent(item.path)}&visit=${navigation()}`
      : "");
  }));
  const connectBridge = () => frame?.contentWindow?.postMessage({ type: "ormos:preview-connect" }, new URL(props.origin).origin);
  onMount(() => {
    persist();
    const receive = (event: MessageEvent) => {
      if (!props.origin || event.source !== frame?.contentWindow || event.origin !== new URL(props.origin).origin || event.data?.type !== "ormos:preview-location" || typeof event.data.path !== "string") return;
      const item = target(); if (!item) return;
      try { record(parsePreviewAddress(event.data.path, item.port), event.data.replace === true); } catch { /* never accept a foreign target */ }
    };
    const resize = () => { if (historyShown()) placeHistory(); };
    window.addEventListener("message", receive); window.addEventListener("resize", resize); window.visualViewport?.addEventListener("resize", resize);
    onCleanup(() => { window.removeEventListener("message", receive); window.removeEventListener("resize", resize); window.visualViewport?.removeEventListener("resize", resize); });
  });
  return (
    <>
      <Portal mount={props.header} ref={element => { element.className = "preview-toolbar"; }}>
      <form class="browserbar" hidden={!props.visible} onSubmit={e => { e.preventDefault(); navigate(); }}>
        <input ref={addressInput} role="combobox" aria-expanded={historyShown()} aria-controls="preview-history-list" aria-activedescendant={historyShown() && historyIndex() >= 0 ? `preview-history-${historyIndex()}` : undefined} aria-autocomplete="none" aria-label="Preview address" placeholder="Port or localhost URL" enterkeyhint="go" autocomplete="off" autocapitalize="none" spellcheck={false} value={address()} onFocus={() => { setHistoryDismissed(false); setHistoryIndex(-1); setAddressFocused(true); }} onBlur={() => setAddressFocused(false)} onKeyDown={historyKey} onInput={e => { setAddress(e.currentTarget.value); setHistoryDismissed(false); setHistoryIndex(-1); }} />
        <BrowserMenu visible={props.visible} canGoBack={current().index > 0} canGoForward={current().index < current().history.length - 1} canRefresh={!!target()} back={() => move(-1)} forward={() => move(1)} refresh={() => { remember(target()); setNavigation(n => n + 1); }} openURL={target() && props.origin ? `${props.origin.replace(/\/$/, "")}/__ormos_preview/${target()!.port}/?path=${encodeURIComponent(target()!.path)}` : ""} />
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
      <Show when={source()} fallback={<div class="empty"><div class="preview-empty-content" role="status">
        <h1>{address().trim() ? "Ready to preview" : "Enter a port"}</h1>
        <p>{address().trim() ? "Press Enter to open your address." : "Open a local app, right here."}</p>
      </div></div>}>
        <Show keyed when={source()}>
          {url => <iframe title="Local app preview" ref={element => { frame = element; }} src={url} onLoad={connectBridge} sandbox="allow-scripts allow-same-origin allow-forms allow-downloads allow-popups" referrerpolicy="no-referrer" allow="" />}
        </Show>
      </Show>
      </div>
    </>
  );
}
