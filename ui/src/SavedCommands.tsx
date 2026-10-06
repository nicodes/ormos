import { createSignal, For, Show } from "solid-js";

type SavedCommand = { id: string; title: string; command: string };
const load = (): SavedCommand[] => {
  try {
    const rows = JSON.parse(localStorage.getItem("ormos.savedCommands") ?? "[]");
    if (Array.isArray(rows)) return rows.filter(row => row && typeof row.id === "string" && typeof row.title === "string" && row.title.trim() && typeof row.command === "string" && row.command.trim());
  } catch { /* start with an empty list if saved data is invalid */ }
  return [];
};

export default function SavedCommands(props: { enabled: boolean; run: (command: string) => void }) {
  const [commands, setCommands] = createSignal(load());
  const [editing, setEditing] = createSignal<string | null>(null);
  const [title, setTitle] = createSignal("");
  const [command, setCommand] = createSignal("");
  const [error, setError] = createSignal("");
  let titleInput: HTMLInputElement | undefined;
  const edit = (row?: SavedCommand) => {
    setTitle(row?.title ?? ""); setCommand(row?.command ?? ""); setError(""); setEditing(row?.id ?? "");
    titleInput?.focus();
  };
  const persist = (rows: SavedCommand[]) => {
    try { localStorage.setItem("ormos.savedCommands", JSON.stringify(rows)); setCommands(rows); setError(""); return true; }
    catch { setError("Could not save commands in this browser."); return false; }
  };
  const save = () => {
    const id = editing(); if (id === null || !title().trim() || !command().trim()) return;
    const row = { id: id || `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`, title: title().trim(), command: command().trim() };
    const rows = id ? commands().map(previous => previous.id === id ? row : previous) : [...commands(), row];
    if (persist(rows)) setEditing(null);
  };
  return <div class="saved-commands">
    <Show when={error()}><p class="saved-command-error" role="alert">{error()}</p></Show>
    <Show when={editing() !== null} fallback={<>
      <div class="saved-command-heading"><span>Saved in this browser</span><button type="button" onClick={() => edit()}>Add command</button></div>
      <Show when={commands().length} fallback={<p class="saved-command-empty">No saved commands yet.</p>}>
        <ul class="saved-command-list"><For each={commands()}>{row => <li>
          <strong>{row.title}</strong><code>{row.command}</code>
          <div class="saved-command-actions">
            <button type="button" disabled={!props.enabled} aria-label={`Run ${row.title}`} onClick={() => props.run(row.command)}>Run</button>
            <button type="button" aria-label={`Edit ${row.title}`} title="Edit command" onClick={() => edit(row)}><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m16 3 5 5-12 12-6 1 1-6L16 3Zm-2 2 5 5" /></svg></button>
            <button type="button" aria-label={`Delete ${row.title}`} title="Delete command" onClick={() => persist(commands().filter(item => item.id !== row.id))}><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 6h18M9 6V3h6v3M5 6l1 15h12l1-15M10 10v7m4-7v7" /></svg></button>
          </div>
        </li>}</For></ul>
      </Show>
    </>}>
      <form class="saved-command-form" onSubmit={event => { event.preventDefault(); save(); }}>
        <label>Title<input ref={element => { titleInput = element; }} aria-label="Command title" maxlength={80} required value={title()} onInput={event => setTitle(event.currentTarget.value)} /></label>
        <label>Command<textarea aria-label="Saved command" rows={4} maxlength={8192} required spellcheck={false} autocapitalize="none" autocomplete="off" value={command()} onInput={event => setCommand(event.currentTarget.value)} /></label>
        <div class="saved-command-actions"><button type="submit" disabled={!title().trim() || !command().trim()}>Save command</button><button type="button" onClick={() => { setEditing(null); setError(""); }}>Cancel</button></div>
      </form>
    </Show>
  </div>;
}
