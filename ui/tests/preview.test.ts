import assert from "node:assert/strict";
import { test } from "node:test";
import { parsePreviewAddress, previewURL } from "../src/preview.ts";

const workspace = new URL("http://devbox:8481");

test("ports and localhost aliases resolve on the machine viewed by the phone", () => {
  for (const address of ["3000/about?tab=2#section", ":3000/about?tab=2#section", "http://localhost:3000/about?tab=2#section", "http://127.0.0.1:3000/about?tab=2#section", "http://[::1]:3000/about?tab=2#section", "http://devbox:3000/about?tab=2#section"]) {
    const target = parsePreviewAddress(address, workspace);
    assert.equal(previewURL(target, workspace), "http://devbox:3000/about?tab=2#section");
  }
});

test("relative paths stay on the current app and normalize dot segments", () => {
  assert.deepEqual(parsePreviewAddress("/one/../two?x=1#part", workspace, 3000), { port: 3000, path: "/two?x=1#part" });
  assert.deepEqual(parsePreviewAddress("about", workspace, 3000), { port: 3000, path: "/about" });
});

test("foreign origins, credentials, origin escapes, empty input and the control port are rejected", () => {
  for (const address of ["", "0", "65536", "8481", "http://localhost:8481", "http://evil.example:3000", "http://user:password@localhost:3000", "//evil.example", "/\\evil.example", "3000//evil.example", "http://localhost:3000/\\evil.example", "https://localhost:3000"]) {
    assert.throws(() => parsePreviewAddress(address, workspace, 3000), address);
  }
  for (const target of [{ port: 8481, path: "/" }, { port: 0, path: "/" }, { port: 3000, path: "//evil.example" }, { port: 3000, path: "/\\evil.example" }, { port: 3000, path: "https://evil.example" }]) {
    assert.throws(() => previewURL(target, workspace));
  }
});

test("HTTPS and IPv6 workspaces preserve the actual host and protocol", () => {
  const https = new URL("https://box.example.ts.net:8481");
  assert.equal(previewURL(parsePreviewAddress("https://localhost:3000/", https), https), "https://box.example.ts.net:3000/");
  assert.deepEqual(parsePreviewAddress("http://localhost:3000", https), { port: 3000, path: "/" });
  const ipv6 = new URL("http://[::1]:8481");
  assert.equal(previewURL(parsePreviewAddress("3000", ipv6), ipv6), "http://[::1]:3000/");
});
