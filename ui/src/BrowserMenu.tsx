import { createEffect, createSignal, onCleanup, onMount, Show } from "solid-js";
import { Portal } from "solid-js/web";

export default function BrowserMenu(props: {
  visible: boolean; canGoBack: boolean; canGoForward: boolean; canRefresh: boolean; openURL: string;
  back: () => void; forward: () => void; refresh: () => void;
}) {
  const [open, setOpen] = createSignal(false);
  const [position, setPosition] = createSignal({ top: "0px", right: "8px", "max-height": "280px" });
  let trigger!: HTMLButtonElement;
  let panel: HTMLDivElement | undefined;
  const place = () => {
    const rect = trigger.getBoundingClientRect();
    const height = window.visualViewport?.height ?? window.innerHeight;
    const top = Math.max(8, Math.min(rect.bottom + 4, height - 160));
    setPosition({ top: `${top}px`, right: `${Math.max(8, window.innerWidth - rect.right)}px`, "max-height": `${Math.max(64, height - top - 8)}px` });
  };
  const act = (action: () => void) => { setOpen(false); action(); trigger.focus(); };
  createEffect(() => { if (!props.visible) setOpen(false); });
  onMount(() => {
    const outside = (event: PointerEvent) => {
      if (!trigger.contains(event.target as Node) && !panel?.contains(event.target as Node)) setOpen(false);
    };
    const escape = (event: KeyboardEvent) => {
      if (open() && event.key === "Escape") { event.preventDefault(); setOpen(false); trigger.focus(); }
    };
    const blur = () => { if (document.activeElement instanceof HTMLIFrameElement) setOpen(false); };
    const resize = () => { if (open()) place(); };
    document.addEventListener("pointerdown", outside); document.addEventListener("keydown", escape);
    window.addEventListener("blur", blur); window.addEventListener("resize", resize); window.visualViewport?.addEventListener("resize", resize);
    onCleanup(() => {
      document.removeEventListener("pointerdown", outside); document.removeEventListener("keydown", escape);
      window.removeEventListener("blur", blur); window.removeEventListener("resize", resize); window.visualViewport?.removeEventListener("resize", resize);
    });
  });
  return <>
    <button ref={trigger} class="header-icon" type="button" aria-label="Browser controls" title="Browser controls" aria-haspopup="dialog" aria-expanded={open()} aria-controls="browser-controls" onClick={() => {
      if (open()) { setOpen(false); return; }
      place(); setOpen(true);
      panel?.querySelector<HTMLElement>('button:not(:disabled), a[href]')?.focus();
    }}>
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" aria-hidden="true"><path d="M4 6h16M4 12h16M4 18h16" /></svg>
    </button>
    <Show when={open()}><Portal>
      <div ref={panel} id="browser-controls" class="terminal-menu browser-menu" role="dialog" aria-label="Browser tools" style={position()}>
        <button type="button" aria-label="Back" disabled={!props.canGoBack} onClick={() => act(props.back)}>
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m14 6-6 6 6 6" /></svg>Back
        </button>
        <button type="button" aria-label="Forward" disabled={!props.canGoForward} onClick={() => act(props.forward)}>
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m10 6 6 6-6 6" /></svg>Forward
        </button>
        <button type="button" aria-label="Refresh preview" disabled={!props.canRefresh} onClick={() => act(props.refresh)}>
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M20 7v5h-5M20 12a8 8 0 1 0-2 6M20 7l-3-3" /></svg>Refresh
        </button>
        <a role="button" aria-label="Open preview in new tab" aria-disabled={!props.openURL} tabindex={props.openURL ? 0 : -1} href={props.openURL || undefined} target="_blank" rel="noopener noreferrer" onClick={() => setOpen(false)}>
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M14 3h7v7m0-7L10 14M10 3H5a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-5" /></svg>Open in new tab
        </a>
      </div>
    </Portal></Show>
  </>;
}
