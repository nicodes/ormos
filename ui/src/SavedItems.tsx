import { createEffect, createSignal, For, on, Show } from "solid-js";

type SavedItem = { id: string; title: string; text: string };
const load = (storageKey: string, kind: "command" | "prompt"): SavedItem[] => {
  try {
    const rows = JSON.parse(localStorage.getItem(storageKey) ?? "[]");
    if (Array.isArray(rows)) return rows.filter(row => row && typeof row.id === "string" && typeof row.title === "string" && row.title.trim() && typeof row[kind] === "string" && row[kind].trim()).map(row => ({ id: row.id, title: row.title, text: row[kind] }));
  } catch { /* start with an empty list if saved data is invalid */ }
  return [];
};

export default function SavedItems(props: { kind: "command" | "prompt"; createRequest: number; enabled: boolean; use: (text: string) => void }) {
  const kind = props.kind;
  const storageKey = kind === "command" ? "ormos.savedCommands" : "ormos.savedPrompts";
  const action = kind === "command" ? "Run" : "Paste";
  const [commands, setCommands] = createSignal(load(storageKey, kind));
  const [editing, setEditing] = createSignal<string | null>(null);
  const [title, setTitle] = createSignal("");
  const [command, setCommand] = createSignal("");
  const [error, setError] = createSignal("");
  let titleInput: HTMLInputElement | undefined;
  const edit = (row?: SavedItem) => {
    setTitle(row?.title ?? ""); setCommand(row?.text ?? ""); setError(""); setEditing(row?.id ?? "");
    titleInput?.focus();
  };
  createEffect(on(() => props.createRequest, () => edit(), { defer: true }));
  const persist = (rows: SavedItem[]) => {
    try { localStorage.setItem(storageKey, JSON.stringify(rows.map(row => ({ id: row.id, title: row.title, [kind]: row.text })))); setCommands(rows); setError(""); return true; }
    catch { setError(`Could not save ${kind}s in this browser.`); return false; }
  };
  const save = () => {
    const id = editing(); if (id === null || !title().trim() || !command().trim()) return;
    const row = { id: id || `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`, title: title().trim(), text: command().trim() };
    const rows = id ? commands().map(previous => previous.id === id ? row : previous) : [...commands(), row];
    if (persist(rows)) setEditing(null);
  };
  return <div class="saved-commands">
    <Show when={error()}><p class="saved-command-error" role="alert">{error()}</p></Show>
    <Show when={editing() !== null} fallback={<>
      <Show when={commands().length} fallback={<p class="saved-command-empty">{`No saved ${kind}s`}</p>}>
        <ul class="saved-command-list"><For each={commands()}>{row => <li>
          <div class="saved-command-row"><strong>{row.title}</strong>
          <div class="saved-command-actions">
            <button type="button" disabled={!props.enabled} aria-label={`${action} ${row.title}`} title={action} onClick={() => props.use(row.text)}><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><Show when={kind === "command"} fallback={<><rect x="6" y="5" width="14" height="16" rx="2" /><path d="M9 5V3H3v14h3M10 10h6M10 14h6" /></>}><path d="m8 4 12 8-12 8V4Z" /></Show></svg></button>
            <button type="button" aria-label={`Edit ${row.title}`} title={`Edit ${kind}`} onClick={() => edit(row)}><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m16 3 5 5-12 12-6 1 1-6L16 3Zm-2 2 5 5" /></svg></button>
            <button type="button" aria-label={`Delete ${row.title}`} title={`Delete ${kind}`} onClick={() => persist(commands().filter(item => item.id !== row.id))}><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 6h18M9 6V3h6v3M5 6l1 15h12l1-15M10 10v7m4-7v7" /></svg></button>
          </div></div><code title={row.text}>{row.text}</code>
        </li>}</For></ul>
      </Show>
    </>}>
      <form class="saved-command-form" onSubmit={event => { event.preventDefault(); save(); }}>
        <label>Title<input ref={element => { titleInput = element; }} aria-label={kind === "command" ? "Command title" : "Prompt title"} maxlength={80} required value={title()} onInput={event => setTitle(event.currentTarget.value)} /></label>
        <label>{kind === "command" ? "Command" : "Prompt"}<textarea aria-label={`Saved ${kind}`} rows={4} maxlength={kind === "command" ? 8192 : 65536} required spellcheck={false} autocapitalize="none" autocomplete="off" value={command()} onInput={event => setCommand(event.currentTarget.value)} /></label>
        <div class="saved-command-actions"><button type="submit" disabled={!title().trim() || !command().trim()}>{`Save ${kind}`}</button><button type="button" onClick={() => { setEditing(null); setError(""); }}>Cancel</button></div>
      </form>
    </Show>
  </div>;
}
