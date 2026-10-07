import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { mkdtemp, rm, mkdir, writeFile, readFile } from 'node:fs/promises';
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
test('isolated localhost previews retain navigation, terminal state and scrolling', { timeout: 90000 }, async t => {
  assert(process.env.ORMOS_TEST_BINARY, 'Set ORMOS_TEST_BINARY to a built Ormos executable.');
  const home = await mkdtemp(path.join(tmpdir(), 'ormos-direct-'));
  t.after(() => rm(home, { recursive: true, force: true }));
  // A local fullscreen app exercises the same alternate-screen/mouse protocol
  // as interactive coding tools, without credentials or API calls.
  await writeFile(path.join(home, 'scroll-app.py'), `import sys, tty, termios, re
original = termios.tcgetattr(0)
tty.setraw(0)
position = 100
pending = ''
try:
    sys.stdout.write('\\x1b[?1049h\\x1b[?1000h\\x1b[?1006h\\x1b[2J\\x1b[HAPP_SCROLL_0100')
    sys.stdout.flush()
    while True:
        pending += sys.stdin.read(1)
        if 'q' in pending: break
        match = re.search(r'\\x1b\\[<(64|65);(\\d+);(\\d+)M', pending)
        if match:
            position += -1 if match[1] == '64' else 1
            pending = pending[match.end():]
            sys.stdout.write('\\x1b[H\\x1b[2KAPP_SCROLL_%04d' % position)
            sys.stdout.flush()
finally:
    sys.stdout.write('\\x1b[?1006l\\x1b[?1000l\\x1b[?1049l')
    sys.stdout.flush()
    termios.tcsetattr(0, termios.TCSADRAIN, original)
`);
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
        await page.keyboard.type("i=1; while [ \"$i\" -le 200 ]; do printf 'SCROLL_ROW_%04d\\n' \"$i\"; i=$((i+1)); done");
        await page.keyboard.press('Enter');
        await page.waitForFunction(() => document.querySelector('.xterm-screen').textContent.includes('SCROLL_ROW_0200'));
        const screen = await page.locator('.xterm-screen').boundingBox();
        const x = screen.x + screen.width / 2;
        const y = screen.y + screen.height / 2;
        const scroll = async distance => {
          if (!touch) { await page.mouse.move(x, y); await page.mouse.wheel(0, -distance); return; }
          const session = await context.newCDPSession(page);
          try {
            await session.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x, y }] });
            for (let step = 1; step <= 10; step++) {
              await session.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x, y: y + distance * step / 10 }] });
            }
            await session.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
          } finally { await session.detach(); }
        };
        await scroll(240);
        await page.waitForFunction(() => {
          const lines = document.querySelector('.xterm-screen').textContent.match(/SCROLL_ROW_(\d+)/g) || [];
          return lines.length > 0 && !lines.includes('SCROLL_ROW_0200');
        }, null, { timeout: 3000 });
        assert.equal(await page.evaluate(() => window.scrollY), 0, 'Scrolling must stay inside the terminal');
        await scroll(-240);
        await page.waitForFunction(() => document.querySelector('.xterm-screen').textContent.includes('SCROLL_ROW_0200'));
        await page.locator('.xterm-helper-textarea').focus();
        await page.keyboard.type('python3 "$HOME/scroll-app.py"');
        await page.keyboard.press('Enter');
        await page.waitForFunction(() => document.querySelector('.xterm-screen').textContent.includes('APP_SCROLL_0100'));
        const appPosition = () => page.evaluate(() => Number(document.querySelector('.xterm-screen').textContent.match(/APP_SCROLL_(\d+)/)?.[1]));
        await scroll(240);
        await page.waitForFunction(() => Number(document.querySelector('.xterm-screen').textContent.match(/APP_SCROLL_(\d+)/)?.[1]) < 100, null, { timeout: 3000 });
        const previous = await appPosition();
        await scroll(-240);
        await page.waitForFunction(previous => Number(document.querySelector('.xterm-screen').textContent.match(/APP_SCROLL_(\d+)/)?.[1]) > previous, previous, { timeout: 3000 });
        await page.keyboard.type('q');
        await page.waitForFunction(() => document.querySelector('.xterm-screen').textContent.includes('SCROLL_ROW_0200'));
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
        const firstURL = await page.locator('iframe').getAttribute('src');
        assert.notEqual(new URL(firstURL).port, String(firstPort));
        assert.notEqual(new URL(firstURL).origin, origin);
        assert.equal(new URL(firstURL).pathname + new URL(firstURL).search + new URL(firstURL).hash, '/?entered=1#part');
        assert(!requests.some(url => url.includes('__ormos')));
        assert(requests.some(url => url.endsWith('/api/preview')), 'Every app must go through an isolated preview');
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
        assert.equal(opened.url(), await page.locator('iframe').getAttribute('src'));
        assert.notEqual(new URL(opened.url()).port, String(secondPort));
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


// A foreground CLI double exercises process lifetime without mutating the test
// machine's Tailscale configuration. Real Serve behavior is checked on devbox.
test('remote preview provisions one temporary route and preserves existing routes on shutdown', { timeout: 30000 }, async t => {
  const home = await mkdtemp(path.join(tmpdir(), 'ormos-auto-'));
  t.after(() => rm(home, { recursive: true, force: true }));
  const bin = path.join(home, 'bin');
  await mkdir(bin);
  const state = path.join(home, 'serve.json');
  const calls = path.join(home, 'calls.jsonl');
  const existing = { TCP: { '8443': { HTTPS: true } }, Web: { 'existing.test:8443': { Handlers: { '/': { Proxy: 'http://127.0.0.1:7000' } } } } };
  await writeFile(state, JSON.stringify(existing));
  await writeFile(path.join(bin, 'tailscale'), `#!${process.execPath}
const fs = require('node:fs');
const args = process.argv.slice(2);
fs.appendFileSync(process.env.TEST_SERVE_CALLS, JSON.stringify(args)+'\\n');
if (args.join(' ') === 'serve status --json') {
 process.stdout.write(fs.readFileSync(process.env.TEST_SERVE_STATE));
} else {
 if (args.length !== 3 || !/^--http=\\d+$/.test(args[1])) process.exit(2);
 const port = args[1].slice(7);
 if (!args[2].startsWith('http://127.0.0.1:') || !Number(args[2].split(':').at(-1))) process.exit(3);
 const cfg = JSON.parse(fs.readFileSync(process.env.TEST_SERVE_STATE));
 cfg.Foreground = { [process.pid]: { TCP: { [port]: { HTTP: true } }, Web: { ['box.test:'+port]: { Handlers: { '/': { Proxy: args[2] } } } } } };
 fs.writeFileSync(process.env.TEST_SERVE_STATE, JSON.stringify(cfg));
 process.on('SIGINT', () => {
  const cfg = JSON.parse(fs.readFileSync(process.env.TEST_SERVE_STATE));
  delete cfg.Foreground[process.pid];
  if (!Object.keys(cfg.Foreground).length) delete cfg.Foreground;
  fs.writeFileSync(process.env.TEST_SERVE_STATE, JSON.stringify(cfg));
  process.exit(0);
 });
 process.stdout.write('Available within your tailnet:\\n');
 setInterval(() => {}, 1000);
}
`, { mode: 0o700 });
  const target = await app('Automatic app');
  t.after(() => close(target));
  const targetPort = target.address().port;
  const placeholder = await listen((_req, res) => res.end());
  const port = placeholder.address().port;
  await close(placeholder);
  const child = spawn(process.env.ORMOS_TEST_BINARY, ['ui', '--port', String(port), '--hosts', 'box.test:'+port], {
    env: { ...process.env, HOME: home, XDG_CONFIG_HOME: home, SHELL: '/bin/sh', PATH: bin+path.delimiter+process.env.PATH, TEST_SERVE_STATE: state, TEST_SERVE_CALLS: calls },
    stdio: 'ignore',
  });
  const stop = async () => {
    if (child.exitCode === null && child.signalCode === null) {
      const exited = once(child, 'exit'); child.kill('SIGTERM'); await exited;
    }
  };
  t.after(stop);
  const origin = `http://127.0.0.1:${port}`;
  for (let n = 0; ; n++) {
    try { await fetch(origin); break; }
    catch { assert(n < 100 && child.exitCode === null); await new Promise(resolve => setTimeout(resolve, 50)); }
  }
  // Exercise a remote hostname against the real CLI watcher without changing
  // this machine's Tailscale state. Local previews need no CLI at all.
  const open = () => new Promise((resolve, reject) => {
    const request = http.request(origin+'/api/preview', {
      method: 'POST', headers: { Host: 'box.test:'+port, Origin: 'http://box.test:'+port, 'Content-Type': 'application/json' },
    }, response => {
      let body = ''; response.on('data', data => { body += data; });
      response.on('end', () => { try { assert.equal(response.statusCode, 200, body); resolve(JSON.parse(body)); } catch (error) { reject(error); } });
    });
    request.on('error', reject);
    request.end(JSON.stringify({ port: targetPort, scheme: 'http' }));
  });
  const exposed = await open();
  assert.notEqual(exposed.port, targetPort);
  const active = JSON.parse(await readFile(state, 'utf8'));
  assert.deepEqual(active.TCP, existing.TCP);
  assert.deepEqual(active.Web, existing.Web);
  assert.equal(Object.keys(active.Foreground).length, 1);
  const backend = Object.values(active.Foreground)[0].Web['box.test:'+exposed.port].Handlers['/'].Proxy;
  const body = await new Promise((resolve, reject) => { http.get(backend, { headers: { Host: 'box.test:'+exposed.port } }, response => { let body=''; response.on('data', data => { body += data; }); response.on('end', () => resolve(body)); }).on('error', reject); });
  assert.match(body, /Automatic app/);
  assert.deepEqual(await open(), exposed);
  const commands = (await readFile(calls, 'utf8')).trim().split('\n').map(JSON.parse);
  assert.deepEqual(commands, [['serve', 'status', '--json'], ['serve', '--http='+exposed.port, backend]]);
  await stop();
  assert.deepEqual(JSON.parse(await readFile(state, 'utf8')), existing);
});

test('Vite accepts localhost forwarding and hot reloads through the preview WebSocket', { timeout: 45000 }, async t => {
  const { createServer } = await import('vite');
  const home = await mkdtemp(path.join(tmpdir(), 'ormos-vite-preview-'));
  t.after(() => rm(home, { recursive: true, force: true }));
  const root = path.join(home, 'app'); await mkdir(root);
  await writeFile(path.join(root, 'index.html'), `<html><body><h1 id="message"></h1><script type="module">
import { message } from '/message.js';
document.querySelector('h1').textContent = message;
if (import.meta.hot) import.meta.hot.accept('/message.js', module => { document.querySelector('h1').textContent = module.message; });
</script></body></html>`);
  await writeFile(path.join(root, 'message.js'), `export const message = 'Before live reload';\n`);
  let vite = await createServer({ root, configFile: false, server: { host: '127.0.0.1', port: 0 } });
  await vite.listen(); t.after(() => vite.close());
  const appPort = vite.httpServer.address().port;
  const placeholder = await listen((_req, res) => res.end());
  const port = placeholder.address().port; await close(placeholder);
  const child = spawn(process.env.ORMOS_TEST_BINARY, ['ui', '--port', String(port)], {
    env: { ...process.env, HOME: home, XDG_CONFIG_HOME: home, SHELL: '/bin/sh' }, stdio: 'ignore',
  });
  t.after(async () => { if (child.exitCode === null && child.signalCode === null) { const exited = once(child, 'exit'); child.kill('SIGTERM'); await exited; } });
  const origin = `http://127.0.0.1:${port}`;
  for (let attempt=0; ; attempt++) {
    try { await fetch(origin); break; }
    catch { assert(attempt < 100 && child.exitCode === null); await new Promise(resolve => setTimeout(resolve, 50)); }
  }
  const browser = await chromium.launch(); t.after(() => browser.close());
  const page = await browser.newPage({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
  const errors = []; const sockets = [];
  page.on('pageerror', error => errors.push(String(error)));
  page.on('websocket', socket => sockets.push(socket.url()));
  await page.goto(origin);
  await page.getByRole('button', { name: 'Show preview', exact: true }).click();
  const address = page.getByRole('combobox', { name: 'Preview address' });
  await address.fill(String(appPort)); await address.press('Enter');
  const frame = page.frameLocator('iframe');
  await frame.getByRole('heading', { name: 'Before live reload', exact: true }).waitFor();
  const preview = new URL(await page.locator('iframe').getAttribute('src'));
  assert.notEqual(preview.origin, origin);
  assert.notEqual(preview.port, String(appPort));
  await writeFile(path.join(root, 'message.js'), `export const message = 'After live reload';\n`);
  await frame.getByRole('heading', { name: 'After live reload', exact: true }).waitFor();
  assert(sockets.some(socket => new URL(socket).port === preview.port), 'HMR must use the preview proxy, not the app port');
  await vite.close();
  vite = await createServer({ root, configFile: false, server: { host: '127.0.0.1', port: appPort, strictPort: true } });
  await vite.listen();
  await page.getByRole('button', { name: 'Browser controls', exact: true }).click();
  await page.getByRole('dialog', { name: 'Browser tools' }).getByRole('button', { name: 'Refresh preview', exact: true }).click();
  await frame.getByRole('heading', { name: 'After live reload', exact: true }).waitFor();
  assert.equal(new URL(await page.locator('iframe').getAttribute('src')).port, preview.port, 'Restarting the app must retain its preview route');
  assert.equal(errors.length, 0, errors.join('\n'));
});
