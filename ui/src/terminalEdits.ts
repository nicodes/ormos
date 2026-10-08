// xterm's keyCode=229 fallback compares whole textarea strings. Replacing a
// word can therefore resend all prior context, and an input event can also send
// the same insertion before that fallback runs. Own only non-composing mobile
// edits; leave hardware keys and IME composition to xterm.
export function bindTerminalEdits(
  element: HTMLElement, textarea: HTMLTextAreaElement,
  send: (text: string) => void, paste: (text: string) => void,
): () => void {
  let before: string | undefined;
  let composing = false;
  let finishing: ReturnType<typeof setTimeout> | undefined;
  const keydown = (event: KeyboardEvent) => {
    if (event.target !== textarea) return;
    before = undefined;
    if (event.keyCode !== 229 || composing) return;
    before = textarea.value;
    // Capture on the ancestor, before xterm installs its delayed fallback.
    event.stopImmediatePropagation();
  };
  const beforeinput = (event: InputEvent) => {
    if (event.target === textarea && !composing && !event.isComposing &&
        (before !== undefined || event.inputType === "insertReplacementText" || event.inputType === "insertFromPaste")) {
      before = textarea.value;
    }
  };
  const input = (event: InputEvent) => {
    if (event.target !== textarea || composing || event.isComposing || before === undefined) return;
    const old = Array.from(before), next = Array.from(textarea.value);
    before = undefined;
    event.stopImmediatePropagation();
    let prefix = 0;
    while (prefix < old.length && prefix < next.length && old[prefix] === next[prefix]) prefix++;
    const deleted = "\x7f".repeat(old.length - prefix);
    const inserted = next.slice(prefix).join("");
    if (deleted) send(deleted);
    if (inserted) {
      if (event.inputType === "insertFromPaste") paste(inserted);
      else send(inserted);
    }
  };
  const start = () => { clearTimeout(finishing); composing = true; before = undefined; };
  const end = () => {
    // xterm finishes compositions on a zero-delay timer. Do not take over an
    // input event that belongs to that final composition commit.
    finishing = setTimeout(() => { composing = false; }, 0);
  };
  const reset = () => { before = undefined; };
  const clipboard = (event: ClipboardEvent) => {
    if (event.target !== textarea || !event.clipboardData) return;
    before = undefined;
    // Browser default insertion plus xterm's paste handler must not both run.
    event.preventDefault(); event.stopImmediatePropagation();
    paste(event.clipboardData.getData("text/plain"));
  };
  element.addEventListener("keydown", keydown, true);
  element.addEventListener("beforeinput", beforeinput, true);
  element.addEventListener("input", input, true);
  element.addEventListener("paste", clipboard, true);
  textarea.addEventListener("compositionstart", start);
  textarea.addEventListener("compositionend", end);
  textarea.addEventListener("blur", reset);
  return () => {
    clearTimeout(finishing);
    element.removeEventListener("keydown", keydown, true);
    element.removeEventListener("beforeinput", beforeinput, true);
    element.removeEventListener("input", input, true);
    element.removeEventListener("paste", clipboard, true);
    textarea.removeEventListener("compositionstart", start);
    textarea.removeEventListener("compositionend", end);
    textarea.removeEventListener("blur", reset);
  };
}
