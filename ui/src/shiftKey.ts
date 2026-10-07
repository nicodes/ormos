// Terminal protocols represent shifted letters as text and navigation as CSI.
// Multi-character text/paste is not a single key and leaves the latch alone.
export function shiftTerminalKey(data: string): string | undefined {
  const navigation: Record<string, string> = {
    "\t": "\x1b[Z", "\x1b[A": "\x1b[1;2A", "\x1b[B": "\x1b[1;2B",
    "\x1b[C": "\x1b[1;2C", "\x1b[D": "\x1b[1;2D",
    "\x1bOA": "\x1b[1;2A", "\x1bOB": "\x1b[1;2B",
    "\x1bOC": "\x1b[1;2C", "\x1bOD": "\x1b[1;2D",
  };
  if (data in navigation) return navigation[data];
  if ([...data].length !== 1) return undefined;
  const plain = "`1234567890-=[]\\;',./";
  const shifted = '~!@#$%^&*()_+{}|:"<>?';
  const index = plain.indexOf(data);
  return index >= 0 ? shifted[index] : data.toUpperCase();
}
