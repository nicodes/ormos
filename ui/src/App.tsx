import { createSignal, For, onCleanup, onMount, Show } from "solid-js";
import TerminalPane, { type TerminalControls } from "./TerminalPane";
import PreviewPane from "./PreviewPane";
import PaneToggle from "./PaneToggle";
import { APIError, request, type TerminalRow } from "./api";

type TerminalTab = { id: string; label: string };
function savedTerminals(): TerminalTab[] {
  try {
    const rows = JSON.parse(localStorage.getItem("ormos.terminalTabs") ?? "null");
    if (Array.isArray(rows) && rows.every(row => typeof row.id === "string" && typeof row.label === "string")) return rows;
  } catch { /* start with a fresh terminal if stored state is invalid */ }
  const old = localStorage.getItem("ormos.terminal");
  return old ? [{ id: old, label: "Terminal 1" }] : [];
}

export default function App() {
  const [tabs, setTabs] = createSignal<TerminalTab[]>([]);
  const [active, setActive] = createSignal("");
  const [statuses, setStatuses] = createSignal<Record<string, string>>({});
  const [previewOrigin, setPreviewOrigin] = createSignal("");
  const [error, setError] = createSignal("");
  const [busy, setBusy] = createSignal(true);
  const [terminalCollapsed, setTerminalCollapsed] = createSignal(localStorage.getItem("ormos.terminalCollapsed") === "true");
  const [previewCollapsed, setPreviewCollapsed] = createSignal(localStorage.getItem("ormos.previewCollapsed") === "true");
  const toggleTerminal = () => { setTerminalCollapsed(value => !value); localStorage.setItem("ormos.terminalCollapsed", String(terminalCollapsed())); };
  const togglePreview = () => { setPreviewCollapsed(value => !value); localStorage.setItem("ormos.previewCollapsed", String(previewCollapsed())); };
  const controls = new Map<string, TerminalControls>();
  let nextNumber = 1;
  let disposed = false;
  const persist = () => {
    localStorage.setItem("ormos.terminalTabs", JSON.stringify(tabs()));
    localStorage.setItem("ormos.activeTerminal", active());
  };
  const select = (id: string) => { setActive(id); persist(); };
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
        const [workspace, terminals] = await Promise.all([
          request<{ previewURL: string }>("/api/workspace"), request<{ terminals: TerminalRow[] }>("/api/terminals"),
        ]);
        if (disposed) return;
        setPreviewOrigin(workspace.previewURL);
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
  const keys = [
    { label: "Esc", data: "\x1b" }, { label: "Tab", data: "\t" }, { label: "Ctrl C", data: "\x03" },
    { label: "Ctrl D", data: "\x04" }, { label: "↑", data: "\x1b[A" }, { label: "↓", data: "\x1b[B" },
    { label: "←", data: "\x1b[D" }, { label: "→", data: "\x1b[C" },
  ];
  return (
    <main class="app">
      <Show when={error()}><div class="error" role="alert">{error()}<button aria-label="Dismiss error" onClick={() => setError("")}>×</button></div></Show>
      <div class="workspace">
        <section class="pane preview-pane" classList={{ collapsed: previewCollapsed() }} aria-label="App preview">
          <PreviewPane origin={previewOrigin()} onError={setError} collapsed={previewCollapsed()} onToggle={togglePreview} />
        </section>
        <section class="pane terminal-pane" classList={{ collapsed: terminalCollapsed() }} aria-label="Terminal">
          <div class="tabbar" role="tablist" aria-label="Terminal tabs">
            <PaneToggle name="terminal" collapsed={terminalCollapsed()} onToggle={toggleTerminal} controls="terminal-content" />
            <For each={tabs()}>{tab => <div class="tab" classList={{ selected: active() === tab.id }}>
              <button role="tab" aria-selected={active() === tab.id} title={statuses()[tab.id] ?? "Connecting"} onClick={() => select(tab.id)}>
                <i class="status-dot" classList={{ online: statuses()[tab.id] === "Connected", exited: statuses()[tab.id] === "Exited" }} />{tab.label}
              </button>
              <button class="tab-close" aria-label={`Close ${tab.label}`} onClick={() => void closeTerminal(tab.id)}>×</button>
            </div>}</For>
            <button class="tab-add" aria-label="New terminal tab" disabled={busy()} onClick={() => void addTerminal()}>+</button>
          </div>
          <div id="terminal-content" class="pane-content" hidden={terminalCollapsed()}>
          <div class="terminal-stack">
            <For each={tabs()}>{tab => <div class="terminal-session" hidden={active() !== tab.id}>
              <TerminalPane id={tab.id} onStatus={status => setStatus(tab.id, status)} register={(id, value) => value ? controls.set(id, value) : controls.delete(id)} />
            </div>}</For>
          </div>
          <div class="terminal-keys">
            <button onClick={() => controls.get(active())?.focus()}>Keyboard</button>
            <For each={keys}>{key => <button onPointerDown={e => e.preventDefault()} onClick={() => controls.get(active())?.type(key.data)}>{key.label}</button>}</For>
          </div>
          </div>
        </section>
      </div>
    </main>
  );
}
