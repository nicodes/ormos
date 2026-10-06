import { createEffect, createSignal, on, onCleanup, onMount, Show } from "solid-js";

import PaneToggle from "./PaneToggle";

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

export default function PreviewPane(props: { origin: string; onError: (error: string) => void; collapsed: boolean; onToggle: () => void }) {
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
  const target = () => current().history[current().index];
  const [address, setAddress] = createSignal(addressFor(target()));
  const [navigation, setNavigation] = createSignal(0);
  let frame: HTMLIFrameElement | undefined;
  const persist = () => {
    localStorage.setItem("ormos.previewHistory", JSON.stringify(current()));
    localStorage.removeItem("ormos.previewTabs");
    localStorage.removeItem("ormos.activePreview");
    localStorage.removeItem("ormos.previewPort");
  };
  const record = (next: PreviewTarget, replace = false) => {
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
      props.onError(""); record(next); setNavigation(n => n + 1);
    } catch (e) { props.onError(String(e)); }
  };
  const move = (offset: number) => {
    const tab = current(); const index = tab.index + offset;
    if (index < 0 || index >= tab.history.length) return;
    setCurrent(row => ({ ...row, index })); persist(); setAddress(addressFor(target())); setNavigation(n => n + 1);
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
    window.addEventListener("message", receive);
    onCleanup(() => window.removeEventListener("message", receive));
  });
  return (
    <>
      <form class="browserbar" onSubmit={e => { e.preventDefault(); navigate(); }}>
        <PaneToggle name="preview" collapsed={props.collapsed} onToggle={props.onToggle} controls="preview-content" />
        <button type="button" aria-label="Back" title="Back" disabled={current().index <= 0} onClick={() => move(-1)}>‹</button>
        <button type="button" aria-label="Forward" title="Forward" disabled={current().index >= current().history.length - 1} onClick={() => move(1)}>›</button>
        <button type="button" aria-label="Refresh preview" title="Refresh" disabled={!target()} onClick={() => setNavigation(n => n + 1)}>↻</button>
        <input aria-label="Preview address" placeholder="Port or localhost URL" enterkeyhint="go" autocomplete="off" autocapitalize="none" spellcheck={false} value={address()} onInput={e => setAddress(e.currentTarget.value)} />
        <a class="preview-open" role="button" aria-label="Open preview in new tab" title="Open in new tab" aria-disabled={!target() || !props.origin} tabindex={target() && props.origin ? 0 : -1} href={target() && props.origin ? `${props.origin.replace(/\/$/, "")}/__ormos_preview/${target()!.port}/?path=${encodeURIComponent(target()!.path)}` : undefined} target="_blank" rel="noopener noreferrer">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M14 3h7v7m0-7L10 14M10 3H5a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-5" /></svg>
        </a>
      </form>
      <div id="preview-content" class="pane-content" hidden={props.collapsed}>
      <Show when={source()} fallback={<div class="empty"><div class="preview-symbol">↗</div><h1>Your app, right here.</h1><p>Enter a local port or URL above.</p></div>}>
        <Show keyed when={source()}>
          {url => <iframe title="Local app preview" ref={element => { frame = element; }} src={url} onLoad={connectBridge} sandbox="allow-scripts allow-same-origin allow-forms allow-downloads allow-popups" referrerpolicy="no-referrer" allow="" />}
        </Show>
      </Show>
      </div>
    </>
  );
}
