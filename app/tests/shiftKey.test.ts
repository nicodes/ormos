import assert from "node:assert/strict";
import { test } from "node:test";
import { shiftTerminalKey } from "../src/shiftKey.ts";

test("Shift encodes terminal letters, punctuation and navigation", () => {
  assert.equal(shiftTerminalKey("s"), "S");
  assert.equal(shiftTerminalKey("S"), "S");
  assert.equal(shiftTerminalKey("1"), "!");
  assert.equal(shiftTerminalKey("/"), "?");
  assert.equal(shiftTerminalKey("\t"), "\x1b[Z");
  assert.equal(shiftTerminalKey("\x1b[A"), "\x1b[1;2A");
  assert.equal(shiftTerminalKey("\x1bOB"), "\x1b[1;2B");
  assert.equal(shiftTerminalKey("\x03"), "\x03");
});

test("Multi-character text is not shifted as one key", () => {
  assert.equal(shiftTerminalKey("saved command"), undefined);
  assert.equal(shiftTerminalKey("\x1b[200~s\x1b[201~"), undefined);
  assert.equal(shiftTerminalKey(""), undefined);
});
