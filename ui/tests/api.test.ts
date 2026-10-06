import assert from "node:assert/strict";
import { test } from "node:test";
import { apiURL } from "../src/api.ts";

test("API and terminal URLs stay inside the workspace mount", () => {
  for (const base of ["http://devbox/ormos", "http://devbox/ormos/", "http://devbox/ormos/?q=1", "https://box.test/ormos/"]) {
    const workspace = new URL(base);
    assert.equal(apiURL("/api/terminals", workspace).href, workspace.origin + "/ormos/api/terminals");
    assert.equal(apiURL("/api/terminal/t_123/ws", workspace).href, workspace.origin + "/ormos/api/terminal/t_123/ws");
  }
  for (const base of ["http://127.0.0.1:8481/", "http://devbox:8481/", "http://devbox/ormos-other"]) {
    const workspace = new URL(base);
    assert.equal(apiURL("/api/terminals", workspace).href, workspace.origin + "/api/terminals");
  }
});
