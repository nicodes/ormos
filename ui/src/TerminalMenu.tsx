import { createSignal, For, onCleanup, onMount, Show } from "solid-js";
import { Portal } from "solid-js/web";

export default function TerminalMenu(props: { enabled: boolean; focus: () => void; type: (data: string) => void }) {
  const [open, setOpen] = createSignal(false);
  const [position, setPosition] = createSignal({ top: "0px", right: "8px" });
  let trigger!: HTMLButtonElement;
  let panel: HTMLDivElement | undefined;
  const keys = [
    { label: "Esc", data: "\x1b" }, { label: "Tab", data: "\t" }, { label: "Ctrl C", data: "\x03" },
    { label: "Ctrl D", data: "\x04" }, { label: "↑", data: "\x1b[A", name: "Up arrow" }, { label: "↓", data: "\x1b[B", name: "Down arrow" },
    { label: "←", data: "\x1b[D", name: "Left arrow" }, { label: "→", data: "\x1b[C", name: "Right arrow" },
  ];
  const place = () => {
    const rect = trigger.getBoundingClientRect();
    setPosition({ top: `${Math.max(8, Math.min(rect.bottom + 4, (window.visualViewport?.height ?? window.innerHeight) - 280))}px`, right: `${Math.max(8, window.innerWidth - rect.right)}px` });
  };
  const toggle = () => {
    if (open()) { setOpen(false); return; }
    place(); setOpen(true);
    panel?.querySelector<HTMLButtonElement>("button")?.focus();
  };
  onMount(() => {
    const outside = (event: PointerEvent) => {
      if (!trigger.contains(event.target as Node) && !panel?.contains(event.target as Node)) setOpen(false);
    };
    const escape = (event: KeyboardEvent) => {
      if (open() && event.key === "Escape") { event.preventDefault(); setOpen(false); trigger.focus(); }
    };
    const resize = () => { if (open()) place(); };
    document.addEventListener("pointerdown", outside); document.addEventListener("keydown", escape);
    window.addEventListener("resize", resize); window.visualViewport?.addEventListener("resize", resize);
    onCleanup(() => {
      document.removeEventListener("pointerdown", outside); document.removeEventListener("keydown", escape);
      window.removeEventListener("resize", resize); window.visualViewport?.removeEventListener("resize", resize);
    });
  });
  return <>
    <button ref={trigger} class="header-icon terminal-menu-toggle" type="button" aria-label="Terminal controls" title="Terminal controls" aria-haspopup="dialog" aria-expanded={open()} aria-controls="terminal-controls" onClick={toggle}>
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" aria-hidden="true"><path d="M4 6h16M4 12h16M4 18h16" /></svg>
    </button>
    <Show when={open()}><Portal>
      <div ref={panel} id="terminal-controls" class="terminal-menu" role="dialog" aria-label="Terminal shortcuts" style={position()}>
        <button disabled={!props.enabled} onClick={() => { setOpen(false); props.focus(); }}>Keyboard</button>
        <div class="terminal-menu-keys"><For each={keys}>{key => <button disabled={!props.enabled} aria-label={key.name ?? key.label} onPointerDown={e => e.preventDefault()} onClick={() => props.type(key.data)}>{key.label}</button>}</For></div>
      </div>
    </Portal></Show>
  </>;
}
