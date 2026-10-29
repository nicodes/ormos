import { onCleanup, onMount } from "solid-js";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { request, type TerminalRow } from "./api";
import { shiftTerminalKey } from "./shiftKey";
import { createTerminalResize } from "./terminalResize";
import { createTerminalInput } from "./terminalInput";
import { createTerminalOutput } from "./terminalOutput";
import { bindTerminalEdits } from "./terminalEdits";
import { bindTerminalTouch } from "./terminalTouch";

export type TerminalControls = { focus: () => void; keyboard: () => void; type: (data: string) => void; paste: (data: string) => void; scrollToBottom: () => void };
export default function TerminalPane(props: {
  id: string; onError: (message: string) => void; onStatus: (status: string) => void;
  register: (id: string, controls?: TerminalControls) => void;
  shifted: () => boolean; clearShift: () => void;
}) {
  let container!: HTMLDivElement;
  onMount(() => {
    let disposed = false;
    let generation = 0;
    let retry: ReturnType<typeof setTimeout> | undefined;
    let socket: WebSocket | undefined;
    let discovery: AbortController | undefined;
    let replaying = true;
    const terminal = new Terminal({
      cursorBlink: true, cursorStyle: "bar", fontSize: 14, scrollback: 100000,
      fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace",
      theme: { background: "#0b0f19", foreground: "#d1d5db", cursor: "#d1d5db", selectionBackground: "#374151" },
    });
    const fit = new FitAddon(); terminal.loadAddon(fit); terminal.open(container);
    const output = createTerminalOutput((data, parsed) => terminal.write(data, parsed), () => terminal.reset(), queueMicrotask, {
      resize: size => terminal.resize(size.cols, size.rows),
      ready: () => { replaying = false; resize.reconnect(); },
    });
    const textarea = terminal.textarea!;
    const touchKeyboard = () => window.matchMedia("(pointer: coarse)").matches;
    const dismissKeyboard = () => {
      textarea.inputMode = "none";
      textarea.readOnly = touchKeyboard();
    };
    dismissKeyboard();
    const focus = () => { touchScroll.stop(); dismissKeyboard(); terminal.focus(); };
    const keyboard = () => {
      touchScroll.stop();
      // A fresh focus inside the button's user gesture opens mobile keyboards.
      textarea.blur(); textarea.readOnly = false; textarea.inputMode = "text";
      terminal.focus();
    };
    const touchPointer = (event: PointerEvent) => {
      if (event.pointerType !== "touch") return;
      terminal.blur(); textarea.inputMode = "none"; textarea.readOnly = true;
    };
    textarea.addEventListener("blur", dismissKeyboard);
    container.addEventListener("pointerdown", touchPointer, true);
    const touchScroll = bindTerminalTouch(container, terminal);
    const send = (message: object) => {
      if (socket?.readyState !== WebSocket.OPEN) return false;
      socket.send(JSON.stringify(message));
      return true;
    };
    const transportInput = createTerminalInput(() => socket?.readyState === WebSocket.OPEN ? socket : undefined, props.onError);
    let pasting = false;
    const type = (data: string) => {
      if (!pasting && props.shifted()) {
        const shifted = shiftTerminalKey(data);
        if (shifted !== undefined) { data = shifted; props.clearShift(); }
      }
      transportInput.send(data);
    };
    const resize = createTerminalResize(() => {
      if (replaying || !container.clientWidth || !container.clientHeight) return;
      fit.fit();
      return { cols: terminal.cols, rows: terminal.rows };
    }, size => { touchScroll.stop(); return send({ type: "resize", ...size }); });
    const connect = () => {
      clearTimeout(retry);
      const current = ++generation;
      replaying = true;
      socket?.close(); props.onStatus("Connecting");
      const url = new URL(`/api/terminal/${props.id}/ws`, location.href);
      url.searchParams.set("flow", "1");
      url.searchParams.set("input", "1");
      url.searchParams.set("geometry", "1");
      const offset = output.offset();
      if (offset !== undefined) url.searchParams.set("since", String(offset));
      url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
      const ws = new WebSocket(url); socket = ws; ws.binaryType = "arraybuffer";
      let replayStarted = false;
      ws.onmessage = event => {
        if (disposed || current !== generation) return;
        try {
          if (typeof event.data === "string") {
            const message = JSON.parse(event.data);
            if (message.type === "replay") {
              if (replayStarted) throw new Error("Duplicate terminal replay");
              replayStarted = true; touchScroll.stop();
              output.begin(message, bytes => {
                if (!disposed && current === generation && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "ack", bytes }));
              });
              transportInput.begin(message.inputWindow);
              props.onStatus("Connected");
            } else if (message.type === "size") output.resize(message);
            else if (message.type === "replay-end") output.complete();
            else if (message.type === "input-ack") transportInput.ack(message.bytes);
          } else if (event.data instanceof ArrayBuffer) {
            output.receive(new Uint8Array(event.data));
          }
        } catch {
          ws.close(1002, "Invalid terminal stream");
        }
      };
      ws.onclose = () => {
        if (disposed || current !== generation) return;
        touchScroll.stop(); transportInput.disconnect();
        output.disconnect();
        replaying = true;
        props.onStatus("Disconnected");
        retry = setTimeout(async () => {
          const controller = new AbortController(); discovery = controller;
          try {
            const rows = (await request<{ terminals: TerminalRow[] }>("/api/terminals", undefined, controller.signal)).terminals;
            if (disposed || current !== generation) return;
            if (rows.some(row => row.id === props.id && row.alive)) connect();
            else props.onStatus("Exited");
          } catch { if (!disposed && current === generation) connect(); }
          finally { if (discovery === controller) discovery = undefined; }
        }, 1500);
      };
    };
    const input = terminal.onData(type);
    const observer = new ResizeObserver(resize.request); observer.observe(container);
    const paste = (data: string) => {
      pasting = true;
      try { terminal.paste(data); } finally { pasting = false; }
    };
    const unbindEdits = bindTerminalEdits(container, textarea, type, paste);
    props.register(props.id, { focus, keyboard, type, paste, scrollToBottom: () => { touchScroll.stop(); terminal.scrollToBottom(); } });
    connect();
    onCleanup(() => {
      disposed = true; generation++; clearTimeout(retry); discovery?.abort(); observer.disconnect(); resize.dispose(); output.dispose(); transportInput.dispose(); socket?.close();
      touchScroll.dispose();
      textarea.removeEventListener("blur", dismissKeyboard);
      container.removeEventListener("pointerdown", touchPointer, true);
      unbindEdits(); input.dispose(); terminal.dispose(); props.register(props.id);
    });
  });
  return <div class="terminal-container" ref={container} />;
}
