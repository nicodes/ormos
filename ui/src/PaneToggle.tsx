import { Show } from "solid-js";

export default function PaneToggle(props: { name: string; collapsed: boolean; onToggle: () => void; controls: string }) {
  const label = () => `${props.collapsed ? "Expand" : "Collapse"} ${props.name}`;
  return <button class="pane-toggle" aria-label={label()} title={label()} aria-expanded={!props.collapsed} aria-controls={props.controls} onClick={props.onToggle}>
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
      <path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7-10-7-10-7Z" />
      <circle cx="12" cy="12" r="3" />
      <Show when={props.collapsed}><path d="m3 3 18 18" /></Show>
    </svg>
  </button>;
}
