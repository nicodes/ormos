type Size = { cols: number; rows: number };

// DOM layout may change many times during a keyboard animation. Fit once per
// frame and notify the PTY only when its character grid actually changes.
export function createTerminalResize(
  measure: () => Size | undefined,
  send: (size: Size) => boolean,
  schedule = requestAnimationFrame,
  cancel = cancelAnimationFrame,
) {
  let frame: number | undefined;
  let last: Size | undefined;
  let disposed = false;
  const flush = () => {
    if (frame !== undefined) cancel(frame);
    frame = undefined;
    if (disposed) return;
    const size = measure();
    if (!size || (last?.cols === size.cols && last.rows === size.rows)) return;
    if (send(size)) last = { ...size };
  };
  return {
    request() {
      if (!disposed && frame === undefined) frame = schedule(flush);
    },
    reconnect() { last = undefined; flush(); },
    dispose() { disposed = true; if (frame !== undefined) cancel(frame); frame = undefined; },
  };
}
