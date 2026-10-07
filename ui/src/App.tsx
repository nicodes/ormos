import { createSignal, For, onCleanup, onMount, Show } from "solid-js";
import TerminalPane, { type TerminalControls } from "./TerminalPane";
import PreviewPane from "./PreviewPane";
import TerminalQuickControls from "./TerminalQuickControls";
import TerminalMenu from "./TerminalMenu";
import { APIError, request, type TerminalRow } from "./api";

type TerminalTab = { id: string; label: string };
function savedTerminals(): TerminalTab[] {
  try {
    const rows = JSON.parse(localStorage.getItem("ormos.terminalTabs") ?? "null");
    if (Array.isArray(rows) && rows.every(row => typeof row.id === "string" && typeof row.label === "string")) return rows.map(row => ({ ...row, label: row.label.slice(0, 24) }));
  } catch { /* start with a fresh terminal if stored state is invalid */ }
  const old = localStorage.getItem("ormos.terminal");
  return old ? [{ id: old, label: "Terminal 1" }] : [];
}

export default function App() {
  const [tabs, setTabs] = createSignal<TerminalTab[]>([]);
  const [active, setActive] = createSignal("");
  const [editing, setEditing] = createSignal("");
  const [draftName, setDraftName] = createSignal("");
  const [statuses, setStatuses] = createSignal<Record<string, string>>({});
  const [error, setError] = createSignal("");
  const [busy, setBusy] = createSignal(true);
  const [view, setView] = createSignal<"terminal" | "preview">(localStorage.getItem("ormos.view") === "preview" ? "preview" : "terminal");
  const [headerMount, setHeaderMount] = createSignal<HTMLDivElement>();
  const switchView = () => {
    saveName();
    setView(previous => previous === "terminal" ? "preview" : "terminal");
    localStorage.setItem("ormos.view", view());
  };
  const controls = new Map<string, TerminalControls>();
  let nextNumber = 1;
  let disposed = false;
  const persist = () => {
    localStorage.setItem("ormos.terminalTabs", JSON.stringify(tabs()));
    localStorage.setItem("ormos.activeTerminal", active());
  };
  const rename = (tab: TerminalTab) => {
    setDraftName(tab.label); setEditing(tab.id);
  };
  const saveName = () => {
    const id = editing(); if (!id) return;
    const label = draftName().trim().slice(0, 24); setEditing("");
    if (label) setTabs(rows => rows.map(tab => tab.id === id ? { ...tab, label } : tab));
    persist();
  };
  const select = (id: string) => { saveName(); setActive(id); persist(); };
  const addTerminal = async () => {
    if (busy()) return;
    setBusy(true); setError("");
    try {
      // Omit cwd: the server opens new tabs in the user's home by default.
      const row = await request<{ id: string }>("/api/action", { action: "open" });
      if (disposed) return;
      setTabs(rows => [...rows, { id: row.id, label: `Terminal ${nextNumber++}` }]); select(row.id);
    } catch (e) { if (!disposed) setError(String(e)); }
    finally { if (!disposed) setBusy(false); }
  };
  const closeTerminal = async (id: string) => {
    saveName();
    try {
      await request("/api/action", { action: "kill", id });
    } catch (e) {
      if (!(e instanceof APIError && e.status === 404)) { setError(String(e)); return; }
    }
    if (disposed) return;
    const rows = tabs(); const index = rows.findIndex(tab => tab.id === id);
    setTabs(rows.filter(tab => tab.id !== id));
    if (active() === id) setActive(tabs()[Math.max(0, index - 1)]?.id ?? "");
    setStatuses(previous => { const next = { ...previous }; delete next[id]; return next; });
    persist();
    if (!tabs().length) await addTerminal();
  };
  const setStatus = (id: string, status: string) => setStatuses(previous => ({ ...previous, [id]: status }));
  onMount(() => {
    const viewport = window.visualViewport;
    const updateHeight = () => document.documentElement.style.setProperty("--viewport-height", `${viewport?.height ?? window.innerHeight}px`);
    viewport?.addEventListener("resize", updateHeight); updateHeight();
    void (async () => {
      try {
        const terminals = await request<{ terminals: TerminalRow[] }>("/api/terminals");
        if (disposed) return;
        const saved = savedTerminals();
        nextNumber = Math.max(0, ...saved.map(tab => Number(/\d+$/.exec(tab.label)?.[0] ?? 0))) + 1;
        const retained = saved.filter(tab => terminals.terminals.some(row => row.id === tab.id && row.alive));
        setTabs(retained);
        const previous = localStorage.getItem("ormos.activeTerminal");
        setActive(retained.find(tab => tab.id === previous)?.id ?? retained[0]?.id ?? "");
        setBusy(false);
        if (!retained.length) await addTerminal(); else persist();
      } catch (e) { if (!disposed) { setError(String(e)); setBusy(false); } }
    })();
    onCleanup(() => { disposed = true; viewport?.removeEventListener("resize", updateHeight); });
  });
  return (
    <main class="app">
      <Show when={error()}><div class="error" role="alert">{error()}<button aria-label="Dismiss error" onClick={() => setError("")}>×</button></div></Show>
      <div class="workspace">
        <header class="pane-header" aria-label="Workspace controls">
          <button class="header-icon" type="button" aria-label={view() === "terminal" ? "Show preview" : "Show terminal"} title={view() === "terminal" ? "Show preview" : "Show terminal"} aria-controls={view() === "terminal" ? "preview-content" : "terminal-content"} onClick={switchView}>
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
              <rect x="3" y="4" width="18" height="16" rx="2" />
              <Show when={view() === "terminal"} fallback={<path d="m7 8 4 4-4 4m6 0h4" />}><path d="M3 9h18M6 6.5h.01M9 6.5h.01" /></Show>
            </svg>
          </button>
          <div class="header-controls">
          <div class="terminal-toolbar" hidden={view() !== "terminal"}>
          <Show when={view() === "terminal"}>
          <div class="tabbar" role="tablist" aria-label="Terminal tabs">
            <For each={tabs()}>{tab => <div class="tab" classList={{ selected: active() === tab.id }} role={active() === tab.id ? "tab" : undefined} aria-selected={active() === tab.id ? true : undefined}>
              <Show when={active() === tab.id} fallback={<button class="tab-title" role="tab" aria-selected={false} title={statuses()[tab.id] ?? "Connecting"} onClick={() => select(tab.id)}>
                <i class="status-dot" classList={{ online: statuses()[tab.id] === "Connected", exited: statuses()[tab.id] === "Exited" }} />{tab.label}
              </button>}>
                <div class="tab-title">
                  <i class="status-dot" classList={{ online: statuses()[tab.id] === "Connected", exited: statuses()[tab.id] === "Exited" }} />
                  <span class="tab-name-wrap"><span class="tab-name-size" aria-hidden="true">{editing() === tab.id ? draftName() || " " : tab.label}</span>
                    <input class="tab-name" aria-label="Terminal tab name" maxlength={24} value={editing() === tab.id ? draftName() : tab.label} onFocus={() => rename(tab)} onInput={event => setDraftName(event.currentTarget.value)} onBlur={saveName} onKeyDown={event => { if (event.key === "Enter") { event.preventDefault(); saveName(); controls.get(active())?.focus(); } else if (event.key === "Escape") { event.preventDefault(); setEditing(""); controls.get(active())?.focus(); } }} />
                  </span>
                </div>
              </Show>
              <button class="header-icon tab-close" classList={{ "inactive-close": active() !== tab.id }} aria-hidden={active() !== tab.id} tabindex={active() === tab.id ? 0 : -1} disabled={active() !== tab.id} aria-label={`Close ${tab.label}`} onClick={() => void closeTerminal(tab.id)}>×</button>
            </div>}</For>
            <button class="header-icon tab-add" aria-label="New terminal tab" disabled={busy()} onClick={() => void addTerminal()}>+</button>
          </div>
          <TerminalMenu enabled={!!active()} keyboard={() => controls.get(active())?.keyboard()} focus={() => controls.get(active())?.focus()} type={data => controls.get(active())?.type(data)} paste={data => controls.get(active())?.paste(data)} />
          </Show>
          </div>
          <div class="preview-toolbar" ref={setHeaderMount} />
          </div>
        </header>
        <div class="workspace-body">
          <section class="pane preview-pane" aria-label="App preview" aria-hidden={view() !== "preview"} inert={view() !== "preview"}>
            <Show when={headerMount()}>{mount => <PreviewPane onError={setError} header={mount()} visible={view() === "preview"} />}</Show>
          </section>
          <section class="pane terminal-pane" aria-label="Terminal" aria-hidden={view() !== "terminal"} inert={view() !== "terminal"}>
          <div id="terminal-content" class="pane-content">
          <div class="terminal-stack">
            <For each={tabs().map(tab => tab.id)}>{id => <div class="terminal-session" hidden={active() !== id}>
              <TerminalPane id={id} onStatus={status => setStatus(id, status)} register={(id, value) => value ? controls.set(id, value) : controls.delete(id)} />
            </div>}</For>
          </div>
          <TerminalQuickControls enabled={statuses()[active()] === "Connected"} keyboard={() => controls.get(active())?.keyboard()} type={data => controls.get(active())?.type(data)} />
          </div>
          </section>
        </div>
      </div>
    </main>
  );
}
