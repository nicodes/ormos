import { createSignal, For, onCleanup, onMount, Show } from "solid-js";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";

type TerminalRow = { id: string; alive: boolean; cwd: string };
type PortRow = { port: number; allowed: boolean };

async function request<T>(path: string, body?: object): Promise<T> {
  const response = await fetch(path, body ? {
    method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body),
  } : undefined);
  const data = await response.json();
  if (!response.ok) throw new Error(data.error ?? `Request failed (${response.status})`);
  return data as T;
}

export default function App() {
  const [hostname, setHostname] = createSignal("devbox");
  const [status, setStatus] = createSignal("Connecting");
  const [error, setError] = createSignal("");
  const [cwd, setCwd] = createSignal("");
  const [ports, setPorts] = createSignal<PortRow[]>([]);
  const [port, setPort] = createSignal(localStorage.getItem("ormos.previewPort") ?? "");
  const [previewOrigin, setPreviewOrigin] = createSignal("");
  const [preview, setPreview] = createSignal("");
  const [previewKey, setPreviewKey] = createSignal(0);
  const [busy, setBusy] = createSignal(false);
  const [id, setId] = createSignal("");
  let container!: HTMLDivElement;
  let terminal: Terminal;
  let fit: FitAddon;
  let socket: WebSocket | undefined;
  let retry: ReturnType<typeof setTimeout> | undefined;
  let disposed = false;
  let generation = 0;

  const send = (message: object) => {
    if (socket?.readyState === WebSocket.OPEN) socket.send(JSON.stringify(message));
  };
  const resize = () => {
    if (!terminal || !container.clientWidth || !container.clientHeight) return;
    fit.fit();
    send({ type: "resize", cols: terminal.cols, rows: terminal.rows });
  };
  const refreshPorts = async () => {
    try { setPorts((await request<{ ports: PortRow[] }>("/api/ports")).ports.filter(p => p.allowed)); }
    catch { /* the terminal status reports connection failures */ }
  };
  const connect = (terminalID: string) => {
    clearTimeout(retry);
    const current = ++generation;
    socket?.close();
    terminal.reset();
    const url = new URL(`/api/terminal/${terminalID}/ws`, location.href);
    url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
    const ws = new WebSocket(url);
    socket = ws;
    ws.binaryType = "arraybuffer";
    ws.onopen = () => {
      if (current !== generation) return;
      setStatus("Connected"); setError(""); resize();
    };
    ws.onmessage = event => {
      if (current !== generation) return;
      if (event.data instanceof ArrayBuffer) terminal.write(new Uint8Array(event.data));
    };
    ws.onclose = () => {
      if (disposed || current !== generation) return;
      setStatus("Disconnected");
      retry = setTimeout(async () => {
        try {
          const rows = (await request<{ terminals: TerminalRow[] }>("/api/terminals")).terminals;
          if (disposed || current !== generation) return;
          if (rows.some(row => row.id === terminalID && row.alive)) connect(terminalID);
          else { setStatus("Terminal exited"); setError("Start a new terminal to continue."); }
        } catch { if (!disposed && current === generation) connect(terminalID); }
      }, 1500);
    };
  };
  const newTerminal = async () => {
    if (busy()) return;
    setBusy(true); setError("");
    try {
      // Replacing this pane closes its previous terminal; reload/reconnect keeps it.
      if (id()) await request("/api/action", { action: "kill", id: id() });
      const row = await request<{ id: string }>("/api/action", { action: "open", cwd: cwd() || undefined });
      if (disposed) return;
      setId(row.id); localStorage.setItem("ormos.terminal", row.id); connect(row.id);
      void refreshPorts();
    } catch (e) { setError(String(e)); }
    finally { setBusy(false); }
  };
  const openPreview = () => {
    const value = Number(port());
    if (!Number.isInteger(value) || value < 1 || value > 65535) { setError("Enter a local app port between 1 and 65535."); return; }
    if (!previewOrigin()) { setError("The preview listener is unavailable."); return; }
    setError(""); localStorage.setItem("ormos.previewPort", String(value));
    setPreview(`${previewOrigin().replace(/\/$/, "")}/__ormos_preview/${value}/`);
    setPreviewKey(k => k + 1);
  };

  onMount(() => {
    terminal = new Terminal({
      cursorBlink: true, fontSize: 14, scrollback: 5000, fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace",
      theme: { background: "#0b0f19", foreground: "#d1d5db", cursor: "#4ade80", selectionBackground: "#374151" },
    });
    fit = new FitAddon(); terminal.loadAddon(fit); terminal.open(container);
    const input = terminal.onData(data => send({ type: "input", data }));
    const observer = new ResizeObserver(resize); observer.observe(container);
    const viewport = window.visualViewport;
    const updateHeight = () => {
      document.documentElement.style.setProperty("--viewport-height", `${viewport?.height ?? window.innerHeight}px`);
      resize();
    };
    viewport?.addEventListener("resize", updateHeight); updateHeight();
    const poll = setInterval(() => void refreshPorts(), 5000);
    void (async () => {
      try {
        const [system, workspace, rows] = await Promise.all([
          request<{ hostname: string }>("/api/system"), request<{ previewURL: string; defaultCwd: string }>("/api/workspace"),
          request<{ terminals: TerminalRow[] }>("/api/terminals"),
        ]);
        if (disposed) return;
        setHostname(system.hostname); setPreviewOrigin(workspace.previewURL); setCwd(workspace.defaultCwd ?? "");
        const saved = localStorage.getItem("ormos.terminal");
        const existing = rows.terminals.find(row => row.id === saved && row.alive);
        if (existing) { setId(existing.id); setCwd(existing.cwd); connect(existing.id); }
        else await newTerminal();
        await refreshPorts();
        if (!disposed && port()) openPreview();
      } catch (e) { if (!disposed) { setStatus("Unavailable"); setError(String(e)); } }
    })();
    onCleanup(() => {
      disposed = true; generation++; clearTimeout(retry); clearInterval(poll);
      observer.disconnect(); viewport?.removeEventListener("resize", updateHeight);
      socket?.close(); input.dispose(); terminal.dispose();
    });
  });

  const keys = [
    { label: "Esc", data: "\x1b" }, { label: "Tab", data: "\t" }, { label: "Ctrl C", data: "\x03" },
    { label: "Ctrl D", data: "\x04" }, { label: "↑", data: "\x1b[A" }, { label: "↓", data: "\x1b[B" },
    { label: "←", data: "\x1b[D" }, { label: "→", data: "\x1b[C" },
  ];

  return (
    <main class="app">
      <header class="topbar">
        <div class="brand">ormos <span>{hostname()}</span></div>
        <span class="connection" classList={{ online: status() === "Connected" }}><i />{status()}</span>
      </header>
      <Show when={error()}><div class="error" role="alert">{error()}</div></Show>
      <div class="workspace">
        <section class="pane terminal-pane" aria-label="Terminal">
          <div class="toolbar">
            <span class="pane-title">Terminal</span>
            <input aria-label="Working directory" placeholder="Working directory (optional)" value={cwd()} onInput={e => setCwd(e.currentTarget.value)} />
            <button disabled={busy()} onClick={() => void newTerminal()}>New terminal</button>
          </div>
          <div class="terminal" ref={container} />
          <div class="terminal-keys">
            <button onClick={() => terminal.focus()}>Keyboard</button>
            <For each={keys}>{key => <button onPointerDown={e => e.preventDefault()} onClick={() => send({ type: "input", data: key.data })}>{key.label}</button>}</For>
          </div>
        </section>
        <section class="pane preview-pane" aria-label="App preview">
          <form class="toolbar" onSubmit={e => { e.preventDefault(); openPreview(); }}>
            <span class="pane-title">Preview</span>
            <input aria-label="App port" inputmode="numeric" list="ports" placeholder="Port, e.g. 3000" value={port()} onInput={e => setPort(e.currentTarget.value)} />
            <datalist id="ports"><For each={ports()}>{row => <option value={row.port} />}</For></datalist>
            <button type="submit">Open</button>
            <Show when={preview()}>
              <button type="button" aria-label="Reload preview" onClick={() => setPreviewKey(k => k + 1)}>↻</button>
              <a href={preview()} target="_blank" rel="noopener noreferrer" aria-label="Open preview in a new tab">↗</a>
            </Show>
          </form>
          <Show when={preview()} fallback={<div class="empty"><div class="preview-symbol">↗</div><h1>Your app, right here.</h1><p>Start a web app in the terminal,<br />then enter its local port above.</p></div>}>
            <Show keyed when={`${preview()}#${previewKey()}`}>
              {_key => <iframe title="Local app preview" src={preview()} sandbox="allow-scripts allow-same-origin allow-forms allow-downloads allow-popups" />}
            </Show>
          </Show>
        </section>
      </div>
    </main>
  );
}
