import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { mkdtemp, rm } from 'node:fs/promises';
import http from 'node:http';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { test } from 'node:test';
import { chromium } from 'playwright';

const listen = async handler => {
  const server = http.createServer(handler);
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  return server;
};
const app = title => listen((req, res) => {
  // Some dev servers reject HEAD; any HTTP response still proves reachability.
  if (req.method === 'HEAD') { res.writeHead(405); res.end(); return; }
  res.setHeader('Content-Type', 'text/html');
  res.end(`<html><head><style>:root{color-scheme:dark}body{background:#0b0f19;color:white}</style></head><body><h1>${title}</h1><button onclick="this.textContent='Kept state'">Change state</button><a href="/inside">Inside link</a></body></html>`);
});
const close = server => new Promise((resolve, reject) => server.close(error => error ? reject(error) : resolve()));

// Uses the already-built release binary. Tests own ephemeral app ports and PTYs.
test('direct previews work without app CORS, proxy cookies or rewritten HTML', { timeout: 90000 }, async t => {
  assert(process.env.ORMOS_TEST_BINARY, 'Set ORMOS_TEST_BINARY to a built Ormos executable.');
  const home = await mkdtemp(path.join(tmpdir(), 'ormos-direct-'));
  t.after(() => rm(home, { recursive: true, force: true }));
  const first = await app('First app');
  const second = await app('Second app');
  const unused = await listen((_req, res) => res.end());
  const port = unused.address().port;
  await close(unused);
  const firstPort = first.address().port;
  const secondPort = second.address().port;
  t.after(async () => { await close(first); await close(second); });
  const child = spawn(globalThis.process.env.ORMOS_TEST_BINARY, ['ui', '--port', String(port)], {
    env: { ...globalThis.process.env, HOME: home, XDG_CONFIG_HOME: home, SHELL: '/bin/sh' },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let output = '';
  child.stdout.on('data', chunk => { output += chunk; });
  child.stderr.on('data', chunk => { output += chunk; });
  t.after(async () => {
    if (child.exitCode === null && child.signalCode === null) {
      const exited = once(child, 'exit');
      child.kill('SIGTERM');
      await exited;
    }
  });
  const origin = `http://127.0.0.1:${port}`;
  for (let attempt = 0; ; attempt++) {
    try { await fetch(origin); break; }
    catch {
      assert(attempt < 100 && child.exitCode === null, output || 'Ormos did not start');
      await new Promise(resolve => setTimeout(resolve, 50));
    }
  }
  const browser = await chromium.launch();
  t.after(() => browser.close());
  for (const touch of [false, true]) {
    await t.test(touch ? 'phone layout' : 'desktop layout', async () => {
      const context = await browser.newContext({ viewport: touch ? { width: 390, height: 844 } : { width: 1440, height: 900 }, hasTouch: touch, isMobile: touch });
      try {
        const page = await context.newPage();
        const requests = [];
        const errors = [];
        page.on('request', request => requests.push(request.url()));
        page.on('pageerror', error => errors.push(error));
        await page.goto(origin);
        await page.waitForFunction(() => document.querySelector('.status-dot.online'));
        await page.locator('.xterm-helper-textarea').focus();
        await page.keyboard.type("DIRECT_STATE=kept; printf 'DIRECT_%s\\n' OK");
        await page.keyboard.press('Enter');
        await page.waitForFunction(() => document.querySelector('.xterm-screen').textContent.includes('DIRECT_OK'));
        await page.getByRole('button', { name: 'Show preview', exact: true }).click();
        const address = page.getByRole('combobox', { name: 'Preview address' });
        const navigate = async value => { await address.fill(value); await address.press('Enter'); };
        const action = async name => {
          await page.getByRole('button', { name: 'Browser controls', exact: true }).click();
          await page.getByRole('dialog', { name: 'Browser tools' }).getByRole('button', { name, exact: true }).click();
        };
        const frame = () => page.frameLocator('iframe');
        await navigate(`http://localhost:${firstPort}/?entered=1#part`);
        await frame().getByRole('heading', { name: 'First app' }).waitFor();
        assert.equal(await page.locator('iframe').getAttribute('src'), `http://127.0.0.1:${firstPort}/?entered=1#part`);
        assert(!requests.some(url => url.includes('__ormos')));
        assert((await context.cookies()).every(cookie => cookie.name !== 'ormos_preview_port'));
        await frame().getByRole('button', { name: 'Change state' }).click();
        await page.getByRole('button', { name: 'Show terminal', exact: true }).click();
        await page.locator('.xterm-helper-textarea').focus();
        await page.keyboard.type("printf 'STATE_%s\\n' \"$DIRECT_STATE\"");
        await page.keyboard.press('Enter');
        await page.waitForFunction(() => document.querySelector('.xterm-screen').textContent.includes('STATE_kept'));
        await page.getByRole('button', { name: 'Show preview', exact: true }).click();
        await frame().getByRole('button', { name: 'Kept state' }).waitFor();
        await frame().getByRole('link', { name: 'Inside link' }).click();
        assert.equal(await address.inputValue(), `${firstPort}/?entered=1#part`);
        await navigate(String(secondPort));
        await frame().getByRole('heading', { name: 'Second app' }).waitFor();
        await action('Back');
        await frame().getByRole('heading', { name: 'First app' }).waitFor();
        await action('Forward');
        await frame().getByRole('heading', { name: 'Second app' }).waitFor();
        const other = await context.newPage();
        await other.goto(origin);
        const otherAddress = other.getByRole('combobox', { name: 'Preview address' });
        await otherAddress.fill(String(firstPort));
        await otherAddress.press('Enter');
        await other.frameLocator('iframe').getByRole('heading', { name: 'First app' }).waitFor();
        await frame().getByRole('heading', { name: 'Second app' }).waitFor();
        await other.close();
        await page.getByRole('button', { name: 'Browser controls', exact: true }).click();
        const popup = page.waitForEvent('popup');
        await page.getByRole('button', { name: 'Open preview in new tab', exact: true }).click();
        const opened = await popup;
        await opened.waitForLoadState('domcontentloaded');
        assert.equal(opened.url(), `http://127.0.0.1:${secondPort}/`);
        assert.equal(await opened.evaluate(() => !!window.opener), false);
        await opened.close();
        const stopped = await listen((_req, res) => res.end());
        const stoppedPort = stopped.address().port;
        await close(stopped);
        await navigate(String(stoppedPort));
        await page.getByRole('heading', { name: 'App unavailable', exact: true }).waitFor();
        assert.equal(await page.locator('iframe').count(), 0);
        const empty = await page.locator('.preview-empty-content').evaluate(element => {
          const rect = element.getBoundingClientRect(), parent = element.parentElement.getBoundingClientRect();
          return { children: Array.from(element.children, child => child.tagName), x: Math.abs(rect.x + rect.width / 2 - parent.x - parent.width / 2), y: Math.abs(rect.y + rect.height / 2 - parent.y - parent.height / 2) };
        });
        assert.deepEqual(empty.children, ['H1', 'P']);
        assert(empty.x < 1 && empty.y < 1);
        await navigate(String(port));
        await page.getByRole('alert').filter({ hasText: 'Choose an app port' }).waitFor();
        assert.equal(await page.locator('iframe').count(), 0);
        await page.getByRole('button', { name: 'Dismiss error' }).click();
        await navigate(String(firstPort));
        await frame().getByRole('heading', { name: 'First app' }).waitFor();
        await page.reload();
        await frame().getByRole('heading', { name: 'First app' }).waitFor();
        assert.equal(errors.length, 0, errors.map(String).join('\n'));
        // These IDs come only from this browser context's newly created tabs.
        await page.evaluate(async () => {
          for (const terminal of JSON.parse(localStorage.getItem('ormos.terminalTabs') || '[]')) {
            await fetch('/api/action', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action: 'kill', id: terminal.id }) });
          }
        });
      } finally { await context.close(); }
    });
  }
});
