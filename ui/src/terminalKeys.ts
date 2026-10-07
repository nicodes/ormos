export const terminalKeys = [
  { id: "escape", label: "Esc", data: "\x1b", name: "Escape" },
  { id: "tab", label: "Tab", data: "\t", name: "Tab", icon: "M4 12h12m-4-4 4 4-4 4M20 4v16" },
  { id: "enter", label: "Enter", data: "\r", name: "Enter", icon: "M20 5v7H4m5-5-5 5 5 5" },
  { id: "interrupt", label: "Ctrl+C", data: "\x03", name: "Ctrl C" },
  { id: "eof", label: "Ctrl+D", data: "\x04", name: "Ctrl D" },
  { id: "up", label: "↑", data: "\x1b[A", name: "Up arrow", icon: "m6 12 6-6 6 6M12 6v12" },
  { id: "down", label: "↓", data: "\x1b[B", name: "Down arrow", icon: "m6 12 6 6 6-6M12 6v12" },
  { id: "left", label: "←", data: "\x1b[D", name: "Left arrow", icon: "m12 6-6 6 6 6M6 12h12" },
  { id: "right", label: "→", data: "\x1b[C", name: "Right arrow", icon: "m12 6 6 6-6 6M6 12h12" },
] as const;
