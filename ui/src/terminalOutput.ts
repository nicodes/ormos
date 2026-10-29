type Size = { cols: number; rows: number };
type Replay = { offset: number; reset: boolean; window?: number; size?: Size };
type Write = (data: Uint8Array, parsed?: () => void) => void;

// Received bytes remain queued in xterm across reconnects. A reset is an
// ordered barrier behind old writes, before the new replay, never synchronous.
export function createTerminalOutput(write: Write, reset: () => void, schedule = queueMicrotask, geometry?: {
  resize: (size: Size) => void;
  ready: () => void;
}) {
  let offset: number | undefined;
  let epoch = 0;
  let disposed = false;
  let pending = 0;
  let unparsed = 0;
  let window = 0;
  let scheduled = false;
  let acknowledge: ((bytes: number) => void) | undefined;
  const validateSize = (size: Size) => {
    if (!size || !Number.isInteger(size.cols) || !Number.isInteger(size.rows) ||
        size.cols < 2 || size.rows < 2 || size.cols > 1000 || size.rows > 1000) throw new Error("Invalid terminal size");
  };
  const barrier = (action: () => void) => write(new Uint8Array(), () => { if (!disposed) action(); });
  return {
    offset: () => offset,
    begin(message: Replay, ack: (bytes: number) => void) {
      if (disposed) return;
      if (!Number.isSafeInteger(message.offset) || message.offset < 0 || typeof message.reset !== "boolean" ||
          (message.window !== undefined && (!Number.isInteger(message.window) || message.window <= 0 || message.window > 1 << 20))) {
        throw new Error("Invalid terminal replay");
      }
      if (message.size !== undefined) validateSize(message.size);
      epoch++; pending = 0; scheduled = false;
      unparsed = 0; window = message.window ?? 0;
      offset = message.offset;
      // Older servers ignore flow=1 and omit window; they must not receive ACKs.
      acknowledge = message.window ? ack : undefined;
      if (message.reset || message.size) barrier(() => {
        if (message.reset) reset();
        if (message.size) geometry?.resize(message.size);
      });
      if (!message.size) geometry?.ready(); // Compatibility with older servers.
    },
    resize(size: Size) {
      validateSize(size);
      barrier(() => geometry?.resize(size));
    },
    complete() {
      const current = epoch;
      barrier(() => { if (current === epoch) geometry?.ready(); });
    },
    receive(data: Uint8Array) {
      if (disposed) return;
      if (offset === undefined) throw new Error("Terminal output preceded replay metadata");
      if (window && unparsed + data.byteLength > window) throw new Error("Terminal exceeded its output window");
      if (window) unparsed += data.byteLength;
      offset += data.byteLength;
      const current = epoch;
      write(data, acknowledge ? () => {
        if (disposed || current !== epoch) return;
        unparsed -= data.byteLength;
        pending += data.byteLength;
        if (scheduled) return;
        scheduled = true;
        schedule(() => {
          if (disposed || current !== epoch) return;
          scheduled = false;
          const bytes = pending; pending = 0;
          if (bytes) acknowledge?.(bytes);
        });
      } : undefined);
    },
    disconnect() { epoch++; acknowledge = undefined; },
    dispose() { disposed = true; epoch++; acknowledge = undefined; },
  };
}
