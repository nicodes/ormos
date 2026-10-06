import { onCleanup, onMount } from "solid-js";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { request, type TerminalRow } from "./api";

export type TerminalControls = { focus: () => void; type: (data: string) => void; paste: (data: string) => void };
export default function TerminalPane(props: {
  id: string; onStatus: (status: string) => void;
  register: (id: string, controls?: TerminalControls) => void;
}) {
  let container!: HTMLDivElement;
  onMount(() => {
    let disposed = false;
    let generation = 0;
    let retry: ReturnType<typeof setTimeout> | undefined;
    let socket: WebSocket | undefined;
    const terminal = new Terminal({
      cursorBlink: true, fontSize: 14, scrollback: 5000,
      fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace",
      theme: { background: "#0b0f19", foreground: "#d1d5db", cursor: "#4ade80", selectionBackground: "#374151" },
    });
    const fit = new FitAddon(); terminal.loadAddon(fit); terminal.open(container);
    const send = (message: object) => {
      if (socket?.readyState === WebSocket.OPEN) socket.send(JSON.stringify(message));
    };
    const type = (data: string) => {
      for (let i = 0; i < data.length; i += 2048) send({ type: "input", data: data.slice(i, i + 2048) });
    };
    const resize = () => {
      if (!container.clientWidth || !container.clientHeight) return;
      fit.fit(); send({ type: "resize", cols: terminal.cols, rows: terminal.rows });
    };
    const connect = () => {
      clearTimeout(retry);
      const current = ++generation;
      socket?.close(); terminal.reset(); props.onStatus("Connecting");
      const url = new URL(`/api/terminal/${props.id}/ws`, location.href);
      url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
      const ws = new WebSocket(url); socket = ws; ws.binaryType = "arraybuffer";
      ws.onopen = () => { if (current === generation) { props.onStatus("Connected"); resize(); } };
      ws.onmessage = event => {
        if (current === generation && event.data instanceof ArrayBuffer) terminal.write(new Uint8Array(event.data));
      };
      ws.onclose = () => {
        if (disposed || current !== generation) return;
        props.onStatus("Disconnected");
        retry = setTimeout(async () => {
          try {
            const rows = (await request<{ terminals: TerminalRow[] }>("/api/terminals")).terminals;
            if (disposed || current !== generation) return;
            if (rows.some(row => row.id === props.id && row.alive)) connect();
            else props.onStatus("Exited");
          } catch { if (!disposed && current === generation) connect(); }
        }, 1500);
      };
    };
    const input = terminal.onData(type);
    const observer = new ResizeObserver(resize); observer.observe(container);
    props.register(props.id, { focus: () => terminal.focus(), type, paste: data => terminal.paste(data) });
    connect();
    onCleanup(() => {
      disposed = true; generation++; clearTimeout(retry); observer.disconnect(); socket?.close();
      input.dispose(); terminal.dispose(); props.register(props.id);
    });
  });
  return <div class="terminal" ref={container} />;
}
