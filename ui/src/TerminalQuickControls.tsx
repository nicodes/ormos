import { For } from "solid-js";
import TerminalKeyIcon from "./TerminalKeyIcon";
import { terminalKeys } from "./terminalKeys";

const navigation = ["escape", "tab", "up", "down", "enter"].map(id => terminalKeys.find(key => key.id === id)!);
const interrupt = terminalKeys.find(key => key.id === "interrupt")!;

export default function TerminalQuickControls(props: { enabled: boolean; shifted: boolean; shift: () => void; keyboard: () => void; type: (data: string) => void }) {
  return <div class="terminal-quick-controls" role="toolbar" aria-label="Quick terminal controls">
    <div class="quick-key-group"><button type="button" class="quick-key keyboard-key" aria-label="Keyboard" title="Open keyboard" disabled={!props.enabled} onPointerDown={event => event.preventDefault()} onClick={props.keyboard}>
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" aria-hidden="true"><rect x="2" y="5" width="20" height="14" rx="2" /><path d="M6 9h.01M10 9h.01M14 9h.01M18 9h.01M6 12h.01M10 12h.01M14 12h.01M18 12h.01M7 15h10" /></svg>
    </button>
    <button type="button" class="quick-key shift-key" aria-label="Shift" aria-pressed={props.shifted} title="Shift next key" disabled={!props.enabled} onPointerDown={event => event.preventDefault()} onClick={props.shift}>
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linejoin="round" aria-hidden="true"><path d="m12 3 9 9h-5v9H8v-9H3Z" /></svg>
    </button></div>
    <div class="quick-key-group"><For each={navigation}>{key => <button type="button" class="quick-key" aria-label={key.name} title={key.name} disabled={!props.enabled} onPointerDown={event => event.preventDefault()} onClick={() => props.type(key.data)}><TerminalKeyIcon label={key.label} icon={"icon" in key ? key.icon : undefined} /></button>}</For></div>
    <button type="button" class="quick-key interrupt-key" aria-label={interrupt.name} title="Interrupt (Ctrl+C)" disabled={!props.enabled} onPointerDown={event => event.preventDefault()} onClick={() => props.type(interrupt.data)}>{interrupt.label}</button>
  </div>;
}
