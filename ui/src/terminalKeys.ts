export const terminalKeys = [
  { id: "escape", label: "Esc", data: "\x1b", name: "Escape" },
  { id: "tab", label: "Tab", data: "\t", name: "Tab" },
  { id: "interrupt", label: "Ctrl+C", data: "\x03", name: "Ctrl C" },
  { id: "eof", label: "Ctrl+D", data: "\x04", name: "Ctrl D" },
  { id: "up", label: "↑", data: "\x1b[A", name: "Up arrow" },
  { id: "down", label: "↓", data: "\x1b[B", name: "Down arrow" },
  { id: "left", label: "←", data: "\x1b[D", name: "Left arrow" },
  { id: "right", label: "→", data: "\x1b[C", name: "Right arrow" },
] as const;
