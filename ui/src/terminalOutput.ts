type Replay = { offset: number; reset: boolean; window?: number };
type Write = (data: Uint8Array, parsed?: () => void) => void;

// Received bytes remain queued in xterm across reconnects. A reset is an
// ordered barrier behind old writes, before the new replay, never synchronous.
export function createTerminalOutput(write: Write, reset: () => void, schedule = queueMicrotask) {
  let offset: number | undefined;
  let epoch = 0;
  let disposed = false;
  let pending = 0;
  let unparsed = 0;
  let window = 0;
  let scheduled = false;
  let acknowledge: ((bytes: number) => void) | undefined;
  return {
    offset: () => offset,
    begin(message: Replay, ack: (bytes: number) => void) {
      if (disposed) return;
      if (!Number.isSafeInteger(message.offset) || message.offset < 0 || typeof message.reset !== "boolean" ||
          (message.window !== undefined && (!Number.isInteger(message.window) || message.window <= 0 || message.window > 1 << 20))) {
        throw new Error("Invalid terminal replay");
      }
      epoch++; pending = 0; scheduled = false;
      unparsed = 0; window = message.window ?? 0;
      offset = message.offset;
      // Older servers ignore flow=1 and omit window; they must not receive ACKs.
      acknowledge = message.window ? ack : undefined;
      if (message.reset) write(new Uint8Array(), () => { if (!disposed) reset(); });
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
    dispose() { disposed = true; epoch++; acknowledge = undefined; },
  };
}
