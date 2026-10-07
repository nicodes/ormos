import { For, Show } from "solid-js";
import { terminalKeys } from "./terminalKeys";

const navigation = ["tab", "escape", "up", "down"].map(id => terminalKeys.find(key => key.id === id)!);
const interrupt = terminalKeys.find(key => key.id === "interrupt")!;

export default function TerminalQuickControls(props: { enabled: boolean; keyboard: () => void; type: (data: string) => void }) {
  return <div class="terminal-quick-controls" role="toolbar" aria-label="Quick terminal controls">
    <button type="button" class="quick-key keyboard-key" aria-label="Keyboard" title="Open keyboard" disabled={!props.enabled} onPointerDown={event => event.preventDefault()} onClick={props.keyboard}>
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" aria-hidden="true"><rect x="2" y="5" width="20" height="14" rx="2" /><path d="M6 9h.01M10 9h.01M14 9h.01M18 9h.01M6 12h.01M10 12h.01M14 12h.01M18 12h.01M7 15h10" /></svg>
    </button>
    <div class="quick-key-group"><For each={navigation}>{key => <button type="button" class="quick-key" aria-label={key.name} title={key.name} disabled={!props.enabled} onPointerDown={event => event.preventDefault()} onClick={() => props.type(key.data)}><Show when={key.id === "up" || key.id === "down"} fallback={key.label}>
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d={key.id === "up" ? "m6 12 6-6 6 6M12 6v12" : "m6 12 6 6 6-6M12 6v12"} /></svg>
    </Show></button>}</For></div>
    <button type="button" class="quick-key interrupt-key" aria-label={interrupt.name} title="Interrupt (Ctrl+C)" disabled={!props.enabled} onPointerDown={event => event.preventDefault()} onClick={() => props.type(interrupt.data)}>{interrupt.label}</button>
  </div>;
}
