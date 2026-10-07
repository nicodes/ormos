import assert from "node:assert/strict";
import { test } from "node:test";
import { APIError, request } from "../src/api.ts";

test("workspace requests bound headers and body waits, with no automatic retry", async t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  let calls = 0;
  t.mock.method(globalThis, "fetch", (_path: string, options: RequestInit) => {
    calls++;
    return new Promise((_resolve, reject) => options.signal!.addEventListener("abort", () => reject(options.signal!.reason), { once: true }));
  });
  const pending = assert.rejects(request("/api/action", { action: "open" }), /Request timed out/);
  t.mock.timers.tick(15000);
  await pending;
  assert.equal(calls, 1, "an uncertain open response must never be retried automatically");
  t.mock.method(globalThis, "fetch", (_path: string, options: RequestInit) => Promise.resolve({ ok: true, json: () => new Promise((_resolve, reject) => options.signal!.addEventListener("abort", () => reject(options.signal!.reason), { once: true })) }));
  const body = assert.rejects(request("/api/terminals"), /Request timed out/);
  await Promise.resolve();
  t.mock.timers.tick(15000);
  await body;
});

test("request cancellation propagates and completed requests retire their timer", async t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  let used: AbortSignal;
  t.mock.method(globalThis, "fetch", (_path: string, options: RequestInit) => {
    used = options.signal!;
    return Promise.resolve({ ok: true, json: async () => ({ terminals: [] }) });
  });
  const controller = new AbortController();
  assert.deepEqual(await request("/api/terminals", undefined, controller.signal), { terminals: [] });
  t.mock.timers.tick(30000); controller.abort();
  assert.equal(used!.aborted, false, "successful request kept an abort listener or timer");
  t.mock.method(globalThis, "fetch", (_path: string, options: RequestInit) => new Promise((_resolve, reject) => {
    if (options.signal!.aborted) reject(options.signal!.reason);
    else options.signal!.addEventListener("abort", () => reject(options.signal!.reason), { once: true });
  }));
  const cancelled = new AbortController();
  const pending = assert.rejects(request("/api/terminals", undefined, cancelled.signal), /cancelled/);
  cancelled.abort(new Error("cancelled")); await pending;
  await assert.rejects(request("/api/terminals", undefined, cancelled.signal), /cancelled/);
});

test("API failures preserve HTTP status and clean up deadline state", async t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  let signal: AbortSignal;
  t.mock.method(globalThis, "fetch", (_path: string, options: RequestInit) => {
    signal = options.signal!;
    return Promise.resolve({ ok: false, status: 429, json: async () => ({ error: "terminal limit" }) });
  });
  await assert.rejects(request("/api/action", { action: "open" }), error => error instanceof APIError && error.status === 429 && error.message === "terminal limit");
  t.mock.timers.tick(30000); assert.equal(signal!.aborted, false);
});
