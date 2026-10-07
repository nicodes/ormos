import { createEffect, createSignal, For, on, onCleanup, Show } from "solid-js";

export type SavedEditor = { editing: boolean };
type SavedItem = { id: string; title: string; text: string };
const load = (storageKey: string, kind: "command" | "prompt"): SavedItem[] => {
  try {
    const rows = JSON.parse(localStorage.getItem(storageKey) ?? "[]");
    if (Array.isArray(rows)) return rows.filter(row => row && typeof row.id === "string" && typeof row.title === "string" && row.title.trim() && typeof row[kind] === "string" && row[kind].trim()).map(row => ({ id: row.id, title: row.title, text: row[kind] }));
  } catch { /* start with an empty list if saved data is invalid */ }
  return [];
};

export default function SavedItems(props: { kind: "command" | "prompt"; createRequest: number; visible: boolean; registerEditor: (editor?: SavedEditor) => void; enabled: boolean; use: (text: string) => void }) {
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
  createEffect(on(() => props.visible, visible => { if (!visible) { setEditing(null); setError(""); } }));
  onCleanup(() => props.registerEditor());
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
  const remove = () => {
    const id = editing(); if (!id) return;
    if (persist(commands().filter(row => row.id !== id))) setEditing(null);
  };
  createEffect(() => props.registerEditor({ editing: editing() !== null }));
  return <div class="saved-commands">
    <Show when={error()}><p class="saved-command-error" role="alert">{error()}</p></Show>
    <Show when={editing() !== null} fallback={<>
      <Show when={commands().length} fallback={<div class="empty-message saved-command-empty" role="status"><strong>{`No saved ${kind}s`}</strong><p>{`Use + to save a ${kind}.`}</p></div>}>
        <ul class="saved-command-list"><For each={commands()}>{row => <li>
          <div class="saved-command-row"><div class="saved-item-copy"><button class="saved-item-title" type="button" disabled={!props.enabled} aria-label={`${action} ${row.title}`} title={`${action} ${row.title}`} onClick={() => props.use(row.text)}><strong>{row.title}</strong></button>
          <button class="saved-item-text" type="button" disabled={!props.enabled} aria-label={`${action} ${kind}: ${row.title}`} title={row.text} onClick={() => props.use(row.text)}><code>{row.text}</code></button></div>
          <div class="saved-command-actions">
            <button type="button" aria-label={`Edit ${row.title}`} title={`Edit ${kind}`} onClick={() => edit(row)}><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m16 3 5 5-12 12-6 1 1-6L16 3Zm-2 2 5 5" /></svg></button>

          </div></div>
        </li>}</For></ul>
      </Show>
    </>}>
      <form class="saved-command-form" onSubmit={event => { event.preventDefault(); save(); }}>
        <label>Title<input ref={element => { titleInput = element; }} aria-label={kind === "command" ? "Command title" : "Prompt title"} maxlength={80} required value={title()} onInput={event => setTitle(event.currentTarget.value)} /></label>
        <label>{kind === "command" ? "Command" : "Prompt"}<textarea aria-label={`Saved ${kind}`} rows={4} maxlength={kind === "command" ? 8192 : 65536} required spellcheck={false} autocapitalize="none" autocomplete="off" value={command()} onInput={event => setCommand(event.currentTarget.value)} /></label>
        <div class="saved-editor-actions">
          <Show when={editing()}><button class="saved-editor-icon" type="button" aria-label={`Delete ${kind}`} title={`Delete ${kind}`} onClick={remove}>
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 6h18M9 6V3h6v3M5 6l1 15h12l1-15M10 10v7m4-7v7" /></svg>
          </button></Show>
          <button class="saved-editor-icon saved-editor-save" type="submit" aria-label={`Save ${kind}`} title={`Save ${kind}`} disabled={!title().trim() || !command().trim()}>
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m5 12 4 4L19 6" /></svg>
          </button>
        </div>
      </form>
    </Show>
  </div>;
}
