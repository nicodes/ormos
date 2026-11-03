import assert from "node:assert/strict";
import { test } from "node:test";
import { createTerminalInput } from "../src/terminalInput.ts";

function fixture() {
  const messages: string[] = [], errors: string[] = [];
  const timers = new Map<number, () => void>();
  let next = 0;
  const socket = { bufferedAmount: 0, send: (data: string) => { messages.push(JSON.parse(data).data); } };
  const input = createTerminalInput(() => socket, error => errors.push(error), callback => {
    timers.set(++next, callback); return next as unknown as ReturnType<typeof setTimeout>;
  }, timer => { timers.delete(timer as unknown as number); });
  const tick = () => { for (const [id, callback] of [...timers]) { timers.delete(id); callback(); } };
  return { input, socket, messages, errors, timers, tick };
}

test("input credits count UTF-8 bytes, preserve ordering and yield large pastes", () => {
  const f = fixture();
  const data = "x".repeat(2047) + "😀終".repeat(40000);
  f.input.send(data);
  assert.equal(f.messages.length, 0, "input waits for replay negotiation");
  f.input.begin(32768);
  let acked = 0;
  while (f.messages.join("") !== data) {
    const sent = Buffer.byteLength(f.messages.join(""));
    assert(sent - acked <= 32768, "PTY input window was exceeded");
    if (sent > acked) { f.input.ack(sent - acked); acked = sent; }
    f.tick();
  }
  assert.equal(f.messages.join(""), data);
  assert(f.messages.every(row => row.length <= 2048));
  f.input.ack(Buffer.byteLength(data) - acked);
  assert.deepEqual(f.errors, []);
});

test("a full PTY window waits for credits without polling", () => {
  const f = fixture(); f.input.begin(32768); f.input.send("x".repeat(65536));
  assert.equal(f.messages.join("").length, 16384, "a large paste yields between batches");
  f.tick(); f.tick();
  assert.equal(f.messages.join("").length, 32768);
  assert.equal(f.timers.size, 0, "credit-blocked input does not spin a timer");
  f.input.send("\r"); f.input.ack(32768); f.tick(); f.tick();
  assert.equal(f.messages.join("").length, 65536);
  f.input.ack(32768);
  assert.equal(f.messages.join(""), "x".repeat(65536) + "\r");
});

test("legacy servers use bounded wire buffering and small keys stay immediate", () => {
  const f = fixture(); f.input.begin(); f.input.send("s");
  assert.deepEqual(f.messages, ["s"]); assert.equal(f.timers.size, 0);
  f.socket.bufferedAmount = 65536; f.input.send("😀".repeat(4096));
  assert.equal(f.messages.length, 1); assert.equal(f.timers.size, 1);
  f.socket.bufferedAmount = 0; f.tick();
  assert.equal(f.messages.join(""), "s" + "😀".repeat(4096));
  assert.throws(() => f.input.ack(1));
});

test("disconnect cancels unsent input and never repeats it on reconnect", () => {
  const f = fixture(); f.input.begin(32768); f.input.send("x".repeat(65536));
  f.input.disconnect(); const before = f.messages.length;
  assert.equal(f.errors.length, 1); assert.equal(f.timers.size, 0);
  f.input.begin(32768); f.tick();
  assert.equal(f.messages.length, before);
  f.input.send("new"); assert.equal(f.messages.at(-1), "new");
  f.input.dispose(); f.input.send("ignored"); f.tick();
  assert.equal(f.messages.at(-1), "new");
});

test("overfull input rejects the whole submission and invalid credits fail closed", () => {
  const f = fixture(); assert.equal(f.input.send("x".repeat((1 << 20) + 1)), false);
  assert.equal(f.messages.length, 0); assert.equal(f.errors.length, 1);
  f.input.begin(32768);
  for (const bytes of [0, -1, 1, NaN, 1.5]) assert.throws(() => f.input.ack(bytes));
  for (const window of [0, 1, 1.5, NaN, (1 << 20) + 1]) assert.throws(() => f.input.begin(window));
  f.input.send("ok"); assert.deepEqual(f.messages, ["ok"]);
});
