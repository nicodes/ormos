import assert from "node:assert/strict";
import { test } from "node:test";
import { createTerminalOutput } from "../src/terminalOutput.ts";

function fixture() {
  const queued: { data: Uint8Array; parsed?: () => void }[] = [];
  const microtasks: (() => void)[] = [];
  const screen: number[] = [];
  const output = createTerminalOutput((data, parsed) => queued.push({ data, parsed }), () => { screen.length = 0; }, task => microtasks.push(task));
  const parse = () => { const next = queued.shift()!; screen.push(...next.data); next.parsed?.(); };
  const flush = () => { while (microtasks.length) microtasks.shift()!(); };
  return { output, queued, screen, parse, flush };
}

test("acknowledge parsed bytes, batching callbacks without delaying input", () => {
  const f = fixture(); const ack: number[] = [];
  f.output.begin({ offset: 12, reset: false, window: 262144 }, count => ack.push(count));
  f.output.receive(new Uint8Array([1, 2])); f.output.receive(new Uint8Array([3]));
  assert.equal(f.output.offset(), 15, "reconnect retains bytes already queued in xterm");
  f.flush(); assert.deepEqual(ack, [], "socket receipt is not parser progress");
  f.parse(); f.parse(); f.flush();
  assert.deepEqual(ack, [3]);
});

test("reset barriers follow queued old output and precede new replay", () => {
  const f = fixture(); const oldAck: number[] = [], newAck: number[] = [];
  f.output.begin({ offset: 0, reset: false, window: 262144 }, count => oldAck.push(count));
  f.output.receive(new Uint8Array([1]));
  f.output.begin({ offset: 100, reset: true, window: 262144 }, count => newAck.push(count));
  f.output.receive(new Uint8Array([2]));
  f.parse(); assert.deepEqual(f.screen, [1]);
  f.parse(); assert.deepEqual(f.screen, [], "old output must be parsed before reset");
  f.parse(); f.flush();
  assert.deepEqual(f.screen, [2]);
  assert.deepEqual(oldAck, [], "old parser callbacks must not credit a new connection");
  assert.deepEqual(newAck, [1]);
  assert.equal(f.output.offset(), 101);
});

test("old ACK microtasks, legacy servers and disposal cannot leak acknowledgements", () => {
  const f = fixture(); const ack: number[] = [];
  f.output.begin({ offset: 0, reset: false, window: 262144 }, count => ack.push(count));
  f.output.receive(new Uint8Array([1])); f.parse();
  f.output.begin({ offset: 1, reset: false }, count => ack.push(count));
  f.output.receive(new Uint8Array([2])); f.parse(); f.flush();
  assert.deepEqual(ack, [], "servers without window negotiation must not get ACKs");
  f.output.begin({ offset: 2, reset: true, window: 262144 }, count => ack.push(count));
  f.output.receive(new Uint8Array([3])); f.output.dispose();
  while (f.queued.length) f.parse();
  f.flush(); assert.deepEqual(ack, []);
});

test("reject malformed replay metadata and output without a replay boundary", () => {
  const f = fixture();
  assert.throws(() => f.output.receive(new Uint8Array([1])));
  for (const message of [
    { offset: -1, reset: true }, { offset: Number.MAX_SAFE_INTEGER + 1, reset: false },
    { offset: 0, reset: false, window: -1 }, { offset: 0, reset: false, window: 2 ** 22 },
  ]) assert.throws(() => f.output.begin(message, () => {}));
  f.output.begin({ offset: 0, reset: false, window: 4 }, () => {});
  f.output.receive(new Uint8Array(4));
  assert.throws(() => f.output.receive(new Uint8Array(1)), "protect the renderer if a peer exceeds negotiated credits");
  f.parse();
  assert.doesNotThrow(() => f.output.receive(new Uint8Array(1)));
});
