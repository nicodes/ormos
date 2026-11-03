import type { IBufferCell, Terminal } from "@xterm/xterm";

// xterm's public scroll API accepts whole rows. Keep its buffer at the row
// above a pixel position, translate the screen by the remaining pixels, and
// draw one extra row so the bottom edge stays filled. This adapter targets the
// pinned DOM renderer; it never changes the terminal/PTY grid or buffer data.
export function bindTerminalPixelScroll(terminal: Terminal) {
  const screen = terminal.element?.querySelector<HTMLElement>(".xterm-screen");
  const rows = screen?.querySelector<HTMLElement>(".xterm-rows");
  let extra: HTMLElement | undefined;
  let extraRow = -1;
  let position: number | undefined, height = 0, expectedRow = 0, renderedRow = terminal.buffer.active.viewportY;
  let ownScroll = false;
  const reset = () => {
    position = undefined;
    extra?.remove(); extra = undefined;
    if (screen) screen.style.removeProperty("transform");
  };
  const paint = (refresh = false) => {
    if (!screen || !rows || position === undefined) return;
    const buffer = terminal.buffer.active;
    if (buffer.type !== "normal" || terminal.modes.mouseTrackingMode !== "none") { reset(); return; }
    // Apply row changes and the pixel offset in the same paint. Updating the
    // transform before xterm renders its new rows causes a backwards flash.
    if (renderedRow !== expectedRow) return;
    const offset = position - expectedRow * height;
    if (offset < 0.001) {
      screen.style.removeProperty("transform"); extra?.remove(); extra = undefined;
      return;
    }
    const template = rows.firstElementChild as HTMLElement | null;
    if (!template) return;
    if (!extra) {
      extra = template.cloneNode(false) as HTMLElement;
      extra.classList.add("terminal-overscan-row");
      extra.setAttribute("aria-hidden", "true");
      rows.append(extra);
      extraRow = -1;
    }
    if (refresh || extraRow !== expectedRow) {
      extra.style.cssText = template.style.cssText;
      const line = buffer.getLine(expectedRow + terminal.rows);
      const fragment = document.createDocumentFragment();
      const width = screen.getBoundingClientRect().width / terminal.cols;
      for (let column = 0; line && column < terminal.cols; column++) {
        const cell = line.getCell(column);
        if (!cell || !cell.getWidth()) continue;
        const span = document.createElement("span");
        span.textContent = cell.isInvisible() ? " " : cell.getChars() || " ";
        span.style.width = `${cell.getWidth() * width}px`;
        if (cell.isBold()) span.classList.add("xterm-bold");
        if (cell.isItalic()) span.classList.add("xterm-italic");
        if (cell.isDim()) span.classList.add("xterm-dim");
        const decorations = [cell.isUnderline() && "underline", cell.isOverline() && "overline", cell.isStrikethrough() && "line-through"].filter(Boolean);
        if (decorations.length) span.style.textDecorationLine = decorations.join(" ");
        color(span, cell, "fg", !!cell.isInverse(), terminal);
        color(span, cell, "bg", !!cell.isInverse(), terminal);
        fragment.append(span);
      }
      extra.replaceChildren(fragment);
      extraRow = expectedRow;
    }
    screen.style.transform = `translateY(${-offset}px)`;
  };
  const render = terminal.onRender(() => { renderedRow = terminal.buffer.active.viewportY; paint(true); });
  const resize = terminal.onResize(reset);
  const change = terminal.onScroll(row => {
    if (!ownScroll && row !== expectedRow) reset();
  });
  return {
    scroll(pixels: number, lineHeight: number) {
      if (!screen || !rows || !lineHeight) return false;
      const buffer = terminal.buffer.active;
      if (position === undefined || height !== lineHeight) {
        height = lineHeight; position = buffer.viewportY * height;
      }
      const next = Math.max(0, Math.min(buffer.baseY * height, position + pixels));
      const moved = next !== position;
      position = next;
      // Multiplying and dividing fractional cell heights can land just below
      // an integer (for example 7 * (590 / 43) / (590 / 43)). Keep exact row
      // boundaries stable without rounding away meaningful pixel movement.
      expectedRow = Math.floor(position / height + 1e-9);
      ownScroll = true;
      try { terminal.scrollToLine(expectedRow); } finally { ownScroll = false; }
      paint();
      return moved;
    },
    reset,
    dispose() { reset(); render.dispose(); resize.dispose(); change.dispose(); },
  };
}

function color(span: HTMLElement, cell: IBufferCell, target: "fg" | "bg", inverse: boolean, terminal: Terminal) {
  const foreground = (target === "fg") !== inverse;
  const value = foreground ? cell.getFgColor() : cell.getBgColor();
  const rgb = foreground ? cell.isFgRGB() : cell.isBgRGB();
  const palette = foreground ? cell.isFgPalette() : cell.isBgPalette();
  const property = target === "fg" ? "color" : "backgroundColor";
  if (rgb) span.style[property] = target === "fg" && cell.isDim()
    ? `rgba(${value >> 16},${value >> 8 & 255},${value & 255},0.5)`
    : `#${value.toString(16).padStart(6, "0")}`;
  else if (palette) {
    const bright = foreground && cell.isBold() && terminal.options.drawBoldTextInBrightColors && value < 8;
    span.classList.add(`xterm-${target}-${bright ? value + 8 : value}`);
  } else if (inverse) {
    span.style[property] = foreground ? terminal.options.theme?.foreground ?? "#ffffff" : terminal.options.theme?.background ?? "#000000";
  }
}
