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
    let offset: number | undefined;
    const terminal = new Terminal({
      cursorBlink: true, cursorStyle: "bar", fontSize: 14, scrollback: 5000,
      fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace",
      theme: { background: "#0b0f19", foreground: "#d1d5db", cursor: "#d1d5db", selectionBackground: "#374151" },
    });
    const fit = new FitAddon(); terminal.loadAddon(fit); terminal.open(container);
    // xterm handles wheels, but its custom viewport does not handle touch swipes.
    // Reuse xterm's wheel handling for apps that own scrolling/mouse input.
    // Ordinary shell swipes only scroll the local buffer.
    let touch: { x: number; y: number; scrolling: boolean } | undefined;
    const touchStart = (event: TouchEvent) => {
      touch = event.touches.length === 1
        ? { x: event.touches[0].clientX, y: event.touches[0].clientY, scrolling: false }
        : undefined;
    };
    const touchEnd = () => { touch = undefined; };
    const touchMove = (event: TouchEvent) => {
      if (!touch || event.touches.length !== 1) {
        touchEnd(); return;
      }
      const point = event.touches[0];
      const distance = point.clientY - touch.y;
      if (!touch.scrolling) {
        if (Math.abs(distance) < 8) return;
        if (Math.abs(point.clientX - touch.x) > Math.abs(distance)) { touchEnd(); return; }
        touch.scrolling = true;
      }
      event.preventDefault();
      const height = terminal.element?.querySelector(".xterm-screen")?.getBoundingClientRect().height;
      if (!height) return;
      const lineHeight = height / terminal.rows;
      const lines = Math.trunc(distance / lineHeight);
      if (lines) {
        if (terminal.buffer.active.type === "alternate" || terminal.modes.mouseTrackingMode !== "none") {
          const screen = terminal.element?.querySelector(".xterm-screen");
          for (let i = 0; i < Math.abs(lines); i++) {
            screen?.dispatchEvent(new WheelEvent("wheel", {
              bubbles: true, cancelable: true, deltaMode: WheelEvent.DOM_DELTA_LINE,
              deltaY: -Math.sign(lines), clientX: point.clientX, clientY: point.clientY,
            }));
          }
        } else terminal.scrollLines(-lines);
        touch.y += lines * lineHeight;
      }
    };
    container.addEventListener("touchstart", touchStart, { passive: true });
    container.addEventListener("touchmove", touchMove, { passive: false });
    container.addEventListener("touchend", touchEnd);
    container.addEventListener("touchcancel", touchEnd);
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
      socket?.close(); props.onStatus("Connecting");
      const url = new URL(`/api/terminal/${props.id}/ws`, location.href);
      if (offset !== undefined) url.searchParams.set("since", String(offset));
      url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
      const ws = new WebSocket(url); socket = ws; ws.binaryType = "arraybuffer";
      ws.onopen = () => { if (current === generation) { props.onStatus("Connected"); resize(); } };
      ws.onmessage = event => {
        if (current !== generation) return;
        if (typeof event.data === "string") {
          const message = JSON.parse(event.data);
          if (message.type === "replay") {
            if (message.reset) terminal.reset();
            offset = message.offset;
          }
        } else if (event.data instanceof ArrayBuffer) {
          terminal.write(new Uint8Array(event.data));
          if (offset !== undefined) offset += event.data.byteLength;
        }
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
      container.removeEventListener("touchstart", touchStart);
      container.removeEventListener("touchmove", touchMove);
      container.removeEventListener("touchend", touchEnd);
      container.removeEventListener("touchcancel", touchEnd);
      input.dispose(); terminal.dispose(); props.register(props.id);
    });
  });
  return <div class="terminal-container" ref={container} />;
}
