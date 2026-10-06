import { createSignal, For, onCleanup, onMount, Show } from "solid-js";
import { Portal } from "solid-js/web";
import SavedCommands from "./SavedCommands";

export default function TerminalMenu(props: { enabled: boolean; focus: () => void; type: (data: string) => void }) {
  const [open, setOpen] = createSignal(false);
  const [tab, setTab] = createSignal<"keyboard" | "saved">("keyboard");
  const [position, setPosition] = createSignal({ top: "0px", right: "8px", "max-height": "280px" });
  let trigger!: HTMLButtonElement;
  let panel: HTMLDivElement | undefined;
  const keys = [
    { label: "Esc", data: "\x1b" }, { label: "Tab", data: "\t" }, { label: "Ctrl C", data: "\x03" },
    { label: "Ctrl D", data: "\x04" }, { label: "↑", data: "\x1b[A", name: "Up arrow" }, { label: "↓", data: "\x1b[B", name: "Down arrow" },
    { label: "←", data: "\x1b[D", name: "Left arrow" }, { label: "→", data: "\x1b[C", name: "Right arrow" },
  ];
  const place = () => {
    const rect = trigger.getBoundingClientRect();
    const height = window.visualViewport?.height ?? window.innerHeight;
    const top = Math.max(8, Math.min(rect.bottom + 4, height - 160));
    setPosition({ top: `${top}px`, right: `${Math.max(8, window.innerWidth - rect.right)}px`, "max-height": `${Math.max(64, height - top - 8)}px` });
  };
  const toggle = () => {
    if (open()) { setOpen(false); return; }
    place(); setOpen(true);
    panel?.querySelector<HTMLButtonElement>('[role="tab"][aria-selected="true"]')?.focus();
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
      <div ref={panel} id="terminal-controls" class="terminal-menu" role="dialog" aria-label="Terminal tools" style={position()}>
        <div class="terminal-menu-tabs" role="tablist" aria-label="Terminal tools tabs" onKeyDown={event => {
          if (["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) {
            event.preventDefault(); setTab(event.key === "Home" ? "keyboard" : event.key === "End" ? "saved" : tab() === "keyboard" ? "saved" : "keyboard");
            panel?.querySelector<HTMLButtonElement>('[role="tab"][aria-selected="true"]')?.focus();
          }
        }}>
          <button id="shortcut-tab" role="tab" aria-label="Keyboard shortcuts" title="Keyboard shortcuts" aria-controls="shortcut-panel" aria-selected={tab() === "keyboard"} tabindex={tab() === "keyboard" ? 0 : -1} onClick={() => setTab("keyboard")}>
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" aria-hidden="true"><rect x="2" y="5" width="20" height="14" rx="2" /><path d="M6 9h.01M10 9h.01M14 9h.01M18 9h.01M6 12h.01M10 12h.01M14 12h.01M18 12h.01M7 15h10" /></svg>
          </button>
          <button id="saved-command-tab" role="tab" aria-label="Saved commands" title="Saved commands" aria-controls="saved-command-panel" aria-selected={tab() === "saved"} tabindex={tab() === "saved" ? 0 : -1} onClick={() => setTab("saved")}>
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 3h12v18l-6-4-6 4V3Z" /></svg>
          </button>
        </div>
        <div id="shortcut-panel" class="shortcut-controls" role="tabpanel" aria-labelledby="shortcut-tab" hidden={tab() !== "keyboard"}>
          <button disabled={!props.enabled} onClick={() => { setOpen(false); props.focus(); }}>Keyboard</button>
          <div class="terminal-menu-keys"><For each={keys}>{key => <button disabled={!props.enabled} aria-label={key.name ?? key.label} onPointerDown={e => e.preventDefault()} onClick={() => props.type(key.data)}>{key.label}</button>}</For></div>
        </div>
        <div id="saved-command-panel" role="tabpanel" aria-labelledby="saved-command-tab" hidden={tab() !== "saved"}>
          <SavedCommands enabled={props.enabled} run={command => { props.type(`${command}\r`); setOpen(false); }} />
        </div>
      </div>
    </Portal></Show>
  </>;
}
