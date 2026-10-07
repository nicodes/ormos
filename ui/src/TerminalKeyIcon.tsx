import { Show } from "solid-js";

export default function TerminalKeyIcon(props: { label: string; icon?: string }) {
  return <Show when={props.icon} fallback={props.label}>
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d={props.icon} /></svg>
  </Show>;
}
