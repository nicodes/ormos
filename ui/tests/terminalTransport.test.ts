import assert from "node:assert/strict";
import { test } from "node:test";
import { createTerminalResize } from "../src/terminalResize.ts";
import { terminalInputChunks } from "../src/terminalInput.ts";

test("resize bursts coalesce, unchanged grids are skipped and reconnect resends", () => {
  let size: { cols: number; rows: number } | undefined = { cols: 80, rows: 24 };
  let measurements = 0;
  const sent: object[] = [];
  const frames = new Map<number, FrameRequestCallback>();
  let id = 0;
  let connected = false;
  const resize = createTerminalResize(() => { measurements++; return size; }, next => {
    if (!connected) return false;
    sent.push(next); return true;
  }, callback => { frames.set(++id, callback); return id; }, id => { frames.delete(id); });
  const tick = () => { for (const callback of [...frames.values()]) callback(0); };
  for (let i = 0; i < 100; i++) resize.request();
  assert.equal(frames.size, 1);
  tick();
  assert.equal(measurements, 1);
  assert.equal(sent.length, 0);
  connected = true;
  resize.reconnect();
  resize.request(); tick();
  assert.deepEqual(sent, [{ cols: 80, rows: 24 }]);
  size = { cols: 91, rows: 27 };
  for (let i = 0; i < 100; i++) resize.request();
  tick();
  assert.equal(sent.length, 2);
  size = undefined; resize.request(); tick();
  assert.equal(sent.length, 2, "hidden terminals must not resize the PTY");
  size = { cols: 91, rows: 27 };
  resize.reconnect();
  assert.equal(sent.length, 3, "a new connection needs the current grid");
  resize.request(); resize.dispose();
  const before = measurements;
  resize.request(); tick(); resize.reconnect();
  assert.equal(frames.size, 0);
  assert.equal(measurements, before, "cleanup cancels pending layout work");
});

test("bounded terminal input preserves Unicode at chunk boundaries", () => {
  for (const data of ["", "s", "\x1b[A", "x".repeat(2047) + "😀" + "終".repeat(4096), "😀".repeat(4096)]) {
    const chunks = [...terminalInputChunks(data)];
    assert.equal(chunks.join(""), data);
    assert(chunks.every(chunk => chunk.length <= 2048));
    // Model each chunk's independent Go JSON decoding and UTF-8 PTY write.
    assert.equal(chunks.map(chunk => Buffer.from(JSON.parse(JSON.stringify(chunk))).toString()).join(""), data);
  }
});
