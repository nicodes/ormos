// Keep each JSON input message bounded without cutting a UTF-16 surrogate pair
// in half (which would turn an emoji into two replacement characters in Go).
const chunkEnd = (data: string, start: number) => {
  let end = Math.min(start + 2048, data.length);
  const last = data.charCodeAt(end - 1), next = data.charCodeAt(end);
  if (last >= 0xd800 && last <= 0xdbff && next >= 0xdc00 && next <= 0xdfff) end--;
  return end;
};
export function* terminalInputChunks(data: string): Generator<string> {
  for (let start = 0; start < data.length;) {
    const end = chunkEnd(data, start);
    yield data.slice(start, end);
    start = end;
  }
}

type InputSocket = { bufferedAmount: number; send: (data: string) => void };
const maxPending = 1 << 20;
const wireLimit = 64 << 10;
const encoder = new TextEncoder();
const ascii = /^[\x00-\x7f]*$/;
const byteLength = (data: string) => ascii.test(data) ? data.length : encoder.encode(data).byteLength;

// Small keystrokes stay synchronous. Large pastes are split lazily and follow
// PTY-consumed byte credits, yielding between batches instead of flooding send().
export function createTerminalInput(
  socket: () => InputSocket | undefined,
  error: (message: string) => void,
  schedule = (callback: () => void) => setTimeout(callback, 16),
  cancel = clearTimeout,
) {
  let rows: { data: string; offset: number; next?: { data: string; bytes: number } }[] = [];
  let pending = 0, inFlight = 0;
  let window: number | undefined;
  let ready = false, disposed = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const pump = () => {
    timer = undefined;
    if (!ready || disposed) return;
    const ws = socket();
    if (!ws) return;
    let batch = 0;
    while (rows.length && batch < 16384) {
      const row = rows[0];
      if (!row.next) {
        const data = row.data.slice(row.offset, chunkEnd(row.data, row.offset));
        row.next = { data, bytes: byteLength(data) };
      }
      const { data, bytes } = row.next;
      if (window !== undefined && bytes > window - inFlight) return;
      if (ws.bufferedAmount >= wireLimit) break;
      ws.send(JSON.stringify({ type: "input", data }));
      if (window !== undefined) inFlight += bytes;
      row.next = undefined; row.offset += data.length;
      pending -= data.length; batch += data.length;
      if (row.offset === row.data.length) rows.shift();
    }
    if (rows.length) timer = schedule(pump);
  };
  return {
    begin(inputWindow?: number) {
      if (inputWindow !== undefined && (!Number.isInteger(inputWindow) || inputWindow < 8192 || inputWindow > 1 << 20)) throw new Error("Invalid terminal input window");
      window = inputWindow; inFlight = 0; ready = true; pump();
    },
    send(data: string) {
      if (disposed || !data.length) return false;
      if (data.length > maxPending - pending) { error("Terminal input queue is full. Wait before sending more text."); return false; }
      const ws = ready ? socket() : undefined;
      if (!rows.length && timer === undefined && ws && data.length <= 2048 && ws.bufferedAmount < wireLimit) {
        const bytes = byteLength(data);
        if (window === undefined || bytes <= window - inFlight) {
          ws.send(JSON.stringify({ type: "input", data }));
          if (window !== undefined) inFlight += bytes;
          return true;
        }
      }
      const tail = rows.at(-1);
      if (tail && !tail.offset && !tail.next && tail.data.length + data.length <= 2048) tail.data += data;
      else rows.push({ data, offset: 0 });
      pending += data.length;
      if (timer === undefined) pump();
      return true;
    },
    ack(bytes: number) {
      if (window === undefined || !Number.isInteger(bytes) || bytes <= 0 || bytes > inFlight) throw new Error("Invalid terminal input acknowledgement");
      inFlight -= bytes;
      if (timer === undefined) pump();
    },
    disconnect() {
      if (timer !== undefined) cancel(timer);
      timer = undefined; ready = false;
      if (pending || inFlight) error("Connection lost. Some terminal input may not have arrived; it was not resent.");
      rows = []; pending = 0; inFlight = 0; window = undefined;
    },
    dispose() { disposed = true; if (timer !== undefined) cancel(timer); rows = []; pending = 0; },
  };
}
