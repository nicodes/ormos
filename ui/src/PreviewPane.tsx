import { batch, createEffect, createSignal, on, For, onCleanup, onMount, Show } from "solid-js";

export type PreviewTarget = { port: number; path: string };
type PreviewTab = { id: string; history: PreviewTarget[]; index: number };
let sequence = 0;
const newTab = (): PreviewTab => ({ id: `preview-${Date.now()}-${sequence++}`, history: [], index: -1 });
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

export default function PreviewPane(props: { origin: string; onError: (error: string) => void }) {
  const loadTabs = (): PreviewTab[] => {
    try {
      const parsed = JSON.parse(localStorage.getItem("ormos.previewTabs") ?? "null");
      if (Array.isArray(parsed) && parsed.length && parsed.every(tab =>
        typeof tab.id === "string" && Array.isArray(tab.history) && Number.isInteger(tab.index) &&
        tab.index >= -1 && tab.index < tab.history.length && tab.history.every((item: PreviewTarget) =>
          Number.isInteger(item.port) && item.port > 0 && item.port <= 65535 && typeof item.path === "string" && item.path.startsWith("/") && !item.path.startsWith("//")))) return parsed;
    } catch { /* invalid saved state starts with a fresh tab */ }
    const tab = newTab();
    const legacy = localStorage.getItem("ormos.previewPort");
    if (legacy) {
      try { tab.history = [parsePreviewAddress(legacy)]; tab.index = 0; } catch { /* ignore old invalid port */ }
    }
    return [tab];
  };
  const [tabs, setTabs] = createSignal(loadTabs());
  const [active, setActive] = createSignal(localStorage.getItem("ormos.activePreview") ?? tabs()[0].id);
  if (!tabs().some(tab => tab.id === active())) setActive(tabs()[0].id);
  const current = () => tabs().find(tab => tab.id === active())!;
  const target = () => current().history[current().index];
  const [address, setAddress] = createSignal(addressFor(target()));
  const [navigation, setNavigation] = createSignal(0);
  let frame: HTMLIFrameElement | undefined;
  // Changing tabs mounts only the selected iframe. Its selected local port
  // cannot redirect background requests from a hidden iframe to another app.
  const persist = () => {
    localStorage.setItem("ormos.previewTabs", JSON.stringify(tabs()));
    localStorage.setItem("ormos.activePreview", active());
  };
  const update = (id: string, change: (tab: PreviewTab) => PreviewTab) => {
    setTabs(rows => rows.map(tab => tab.id === id ? change(tab) : tab)); persist();
  };
  const select = (id: string) => batch(() => {
    setActive(id); setAddress(addressFor(target())); setNavigation(n => n + 1); persist();
  });
  const add = () => batch(() => { const tab = newTab(); setTabs(rows => [...rows, tab]); select(tab.id); });
  const close = (id: string) => batch(() => {
    const rows = tabs(); const index = rows.findIndex(tab => tab.id === id);
    const remaining = rows.filter(tab => tab.id !== id);
    setTabs(remaining.length ? remaining : [newTab()]);
    if (active() === id) select(tabs()[Math.max(0, index - 1)]?.id ?? tabs()[0].id);
    else persist();
  });
  const record = (next: PreviewTarget, replace = false) => {
    const tab = current(); const previous = target();
    if (previous?.port === next.port && previous.path === next.path) { setAddress(addressFor(next)); return; }
    update(tab.id, row => {
      const history = row.history.slice(0, row.index + 1);
      if (replace && history.length) history[history.length - 1] = next;
      else history.push(next);
      return { ...row, history, index: history.length - 1 };
    });
    setAddress(addressFor(next));
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
    update(tab.id, row => ({ ...row, index })); setAddress(addressFor(target())); setNavigation(n => n + 1);
  };
  const [source, setSource] = createSignal("");
  createEffect(on(() => [active(), navigation(), props.origin], () => {
    const item = target();
    setSource(item && props.origin
      ? `${props.origin.replace(/\/$/, "")}/__ormos_preview/${item.port}/?path=${encodeURIComponent(item.path)}&visit=${navigation()}`
      : "");
  }));
  const connectBridge = () => frame?.contentWindow?.postMessage({ type: "ormos:preview-connect" }, new URL(props.origin).origin);
  onMount(() => {
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
      <div class="tabbar" role="tablist" aria-label="Preview tabs">
        <For each={tabs()}>{tab => <div class="tab" classList={{ selected: active() === tab.id }}>
          <button role="tab" aria-selected={active() === tab.id} onClick={() => select(tab.id)}>{tab.history[tab.index] ? `:${tab.history[tab.index].port}` : "New tab"}</button>
          <button class="tab-close" aria-label={`Close preview ${tab.history[tab.index]?.port ?? "tab"}`} onClick={() => close(tab.id)}>×</button>
        </div>}</For>
        <button class="tab-add" aria-label="New preview tab" onClick={add}>+</button>
      </div>
      <form class="browserbar" onSubmit={e => { e.preventDefault(); navigate(); }}>
        <button type="button" aria-label="Back" title="Back" disabled={current().index <= 0} onClick={() => move(-1)}>‹</button>
        <button type="button" aria-label="Forward" title="Forward" disabled={current().index >= current().history.length - 1} onClick={() => move(1)}>›</button>
        <button type="button" aria-label="Refresh preview" title="Refresh" disabled={!target()} onClick={() => setNavigation(n => n + 1)}>↻</button>
        <input aria-label="Preview address" placeholder="Port or localhost URL" enterkeyhint="go" autocomplete="off" autocapitalize="none" spellcheck={false} value={address()} onInput={e => setAddress(e.currentTarget.value)} />
      </form>
      <Show when={source()} fallback={<div class="empty"><div class="preview-symbol">↗</div><h1>Your app, right here.</h1><p>Enter a local port or URL above.</p></div>}>
        <Show keyed when={source()}>
          {url => <iframe title="Local app preview" ref={element => { frame = element; }} src={url} onLoad={connectBridge} sandbox="allow-scripts allow-same-origin allow-forms allow-downloads allow-popups" referrerpolicy="no-referrer" allow="" />}
        </Show>
      </Show>
    </>
  );
}
