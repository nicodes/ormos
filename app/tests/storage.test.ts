import assert from "node:assert/strict";
import { test } from "node:test";
import { readStorage, writeStorage, removeStorage } from "../src/storage.ts";

test("workspace storage is optional while saved-item writes report failures", () => {
  const descriptor = Object.getOwnPropertyDescriptor(globalThis, "localStorage");
  try {
    Object.defineProperty(globalThis, "localStorage", { configurable: true, get() { throw new Error("Storage denied"); } });
    assert.equal(readStorage("state"), null);
    assert.equal(writeStorage("state", "value"), false);
    assert.equal(removeStorage("state"), false);
    const data = new Map<string, string>();
    Object.defineProperty(globalThis, "localStorage", { configurable: true, value: {
      getItem: (key: string) => data.get(key) ?? null,
      setItem: (key: string, value: string) => { if (value.length > 8) throw new Error("Quota exceeded"); data.set(key, value); },
      removeItem: (key: string) => data.delete(key),
    }});
    assert.equal(writeStorage("state", "value"), true);
    assert.equal(readStorage("state"), "value");
    assert.equal(writeStorage("state", "larger than quota"), false);
    assert.equal(readStorage("state"), "value", "a failed save must not erase previous data");
    assert.equal(removeStorage("state"), true);
    assert.equal(readStorage("state"), null);
  } finally {
    if (descriptor) Object.defineProperty(globalThis, "localStorage", descriptor);
    else Reflect.deleteProperty(globalThis, "localStorage");
  }
});
