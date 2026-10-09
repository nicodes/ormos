import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
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
  const artifacts = process.env.ORMOS_TEST_ARTIFACTS || home;
  await mkdir(artifacts, { recursive: true });
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
  await t.test('preview cold load avoids terminal code and cancels serial port polling', async () => {
    const context = await browser.newContext();
    try {
      await context.addInitScript(() => localStorage.setItem('ormos.view', 'preview'));
      const page = await context.newPage();
      const assets = [], sockets = [], polls = [], openings = [];
      page.on('request', request => {
        if (request.url().includes('/assets/')) assets.push(request.url());
        if (request.url().endsWith('/api/action') && request.postDataJSON()?.action === 'open') openings.push(request.url());
      });
      page.on('websocket', socket => sockets.push(socket.url()));
      // Hold the first poll longer than the former three-second interval.
      await page.route('**/api/ports', route => { polls.push(route); });
      await page.goto(origin);
      const address = page.getByRole('combobox', { name: 'Preview address' });
      await address.waitFor();
      assert(!assets.some(url => /TerminalPane-/.test(url)), 'Preview must not fetch xterm JavaScript or CSS');
      assert.equal(sockets.length, 0, 'Preview must not replay terminal history');
      assert.equal(openings.length, 0, 'A fresh preview must not create an unused shell');
      await address.click();
      await page.waitForFunction(() => document.querySelector('.preview-history'));
      for (let attempt = 0; polls.length === 0; attempt++) { assert(attempt < 100); await new Promise(resolve => setTimeout(resolve, 10)); }
      await new Promise(resolve => setTimeout(resolve, 3250));
      assert.equal(polls.length, 1, 'Slow port discovery must not start overlapping requests');
      const cancelled = new Promise(resolve => page.on('requestfailed', request => { if (request.url().endsWith('/api/ports')) resolve(); }));
      await page.getByRole('button', { name: 'Show terminal', exact: true }).click();
      await cancelled;
      await page.waitForFunction(() => document.querySelector('.status-dot.online'));
      assert.equal(sockets.length, 1);
      assert.equal(openings.length, 1, 'The first terminal view must open one shell');
      assert(assets.some(url => /TerminalPane-.*\.js$/.test(url)));
      await page.getByRole('button', { name: 'Show preview', exact: true }).click();
      await page.getByRole('button', { name: 'Show terminal', exact: true }).click();
      assert.equal(sockets.length, 1, 'Returning to a mounted terminal must preserve its connection');
      // Only the terminal created by this context is closed.
      await page.getByRole('button', { name: /Close Terminal/ }).click();
      // Closing the final tab automatically opens a replacement; stop it too.
      await page.waitForFunction(() => document.querySelector('.status-dot.online'));
      await page.evaluate(async () => {
        for (const row of JSON.parse(localStorage.getItem('ormos.terminalTabs') || '[]')) await fetch('/api/action', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action: 'kill', id: row.id }) });
      });
    } finally { await context.close(); }
  });
  await t.test('mobile replacements send only the edit and clipboard pastes send once', async () => {
    const context = await browser.newContext({ isMobile: true, hasTouch: true });
    try {
      await context.addInitScript(() => {
        window.testTyped = [];
        const send = WebSocket.prototype.send;
        WebSocket.prototype.send = function(data) {
          const message = JSON.parse(data);
          if (message.type === 'input') window.testTyped.push(message.data);
          return send.call(this, data);
        };
      });
      const page = await context.newPage();
      await page.goto(origin);
      await page.waitForFunction(() => document.querySelector('.status-dot.online'));
      await page.getByRole('button', { name: 'Keyboard', exact: true }).click();
      const edit = async (oldValue, newValue, inputType = 'insertReplacementText', data = null, mobile = true) => {
        await page.evaluate(({ oldValue, newValue, inputType, data, mobile }) => {
          window.testTyped = [];
          const textarea = document.querySelector('.xterm-helper-textarea');
          textarea.value = oldValue;
          textarea.setSelectionRange(oldValue.length, oldValue.length);
          if (mobile) textarea.dispatchEvent(new KeyboardEvent('keydown', { bubbles: true, keyCode: 229, key: 'Unidentified' }));
          textarea.dispatchEvent(new InputEvent('beforeinput', { bubbles: true, inputType, data }));
          textarea.value = newValue;
          textarea.dispatchEvent(new InputEvent('input', { bubbles: true, inputType, data }));
          textarea.dispatchEvent(new KeyboardEvent('keyup', { bubbles: true, keyCode: 229 }));
        }, { oldValue, newValue, inputType, data, mobile });
        // xterm's legacy mobile fallback sends on a timer.
        await page.waitForTimeout(50);
        return page.evaluate(() => window.testTyped.join(''));
      };
      assert.equal(await edit('Please fix teh', 'Please fix the'), '\x7f\x7fhe');
      assert.equal(await edit('Please fix teh', 'Please fix the', 'insertReplacementText', 'the', false), '\x7f\x7fhe');
      assert.equal(await edit('long message so far: ', 'long message so far: word', 'insertText', 'word'), 'word');
      assert.equal(await edit('echo ', 'echo echo ', 'insertText', 'echo '), 'echo ', 'Intentional repeated text must be retained');
      assert.equal(await edit('hello', 'hell', 'deleteContentBackward'), '\x7f');
      assert.equal(await edit('hello', 'hello', 'insertText', 'hello'), '', 'Unchanged textarea must not resend context');
      assert.equal(await edit('message: ', 'message: 😀', 'insertText', '😀'), '😀');
      const unbracket = text => text.startsWith('\x1b[200~') && text.endsWith('\x1b[201~') ? text.slice(6, -6) : text;
      assert.equal(unbracket(await edit('message: ', 'message: clipboard', 'insertFromPaste', null, false)), 'clipboard');
      // Real IMEs must retain xterm's composition lifecycle and commit once.
      await page.evaluate(() => {
        window.testTyped = [];
        const textarea = document.querySelector('.xterm-helper-textarea');
        textarea.value = 'prior context';
        textarea.dispatchEvent(new KeyboardEvent('keydown', { bubbles: true, keyCode: 229 }));
        textarea.dispatchEvent(new CompositionEvent('compositionstart', { bubbles: true, data: '' }));
        textarea.dispatchEvent(new CompositionEvent('compositionupdate', { bubbles: true, data: '終' }));
        textarea.value += '終';
        textarea.dispatchEvent(new InputEvent('input', { bubbles: true, inputType: 'insertCompositionText', data: '終', isComposing: true }));
        textarea.dispatchEvent(new CompositionEvent('compositionend', { bubbles: true, data: '終' }));
        textarea.dispatchEvent(new KeyboardEvent('keyup', { bubbles: true, keyCode: 229 }));
      });
      await page.waitForTimeout(50);
      assert.equal(await page.evaluate(() => window.testTyped.join('')), '終');
      const paste = await page.evaluate(() => {
        window.testTyped = [];
        const textarea = document.querySelector('.xterm-helper-textarea');
        textarea.value = 'old context';
        const clipboardData = new DataTransfer();
        clipboardData.setData('text/plain', 'pasted word');
        const event = new ClipboardEvent('paste', { bubbles: true, cancelable: true, clipboardData });
        textarea.dispatchEvent(event);
        return { cancelled: event.defaultPrevented, text: window.testTyped.join('') };
      });
      assert.deepEqual({ ...paste, text: unbracket(paste.text) }, { cancelled: true, text: 'pasted word' });
      await page.evaluate(async () => {
        for (const row of JSON.parse(localStorage.getItem('ormos.terminalTabs') || '[]')) await fetch('/api/action', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action: 'kill', id: row.id }) });
      });
    } finally { await context.close(); }
  });
  await t.test('saved lists mount only when selected and large prompts obey input credits', async () => {
    const text = '😀終'.repeat(15000), bytes = Buffer.byteLength(text);
    await writeFile(path.join(home, 'paste-app.py'), `import os,tty,termios,hashlib
original = termios.tcgetattr(0)
tty.setraw(0)
print('PASTE_READY', flush=True)
data = bytearray()
try:
    while len(data) < ${bytes}:
        data.extend(os.read(0, min(4096, ${bytes} - len(data))))
    print('PASTE_RESULT_' + str(len(data)) + '_' + hashlib.sha256(data).hexdigest(), flush=True)
finally:
    termios.tcsetattr(0, termios.TCSADRAIN, original)
`);
    const context = await browser.newContext();
    try {
      await context.addInitScript(text => {
        localStorage.setItem('ormos.savedCommands', JSON.stringify([{ id: 'command', title: 'Only commands', command: 'true' }]));
        localStorage.setItem('ormos.savedPrompts', JSON.stringify([{ id: 'prompt', title: 'Large prompt', prompt: text }]));
        window.testSavedReads = [];
        const get = Storage.prototype.getItem;
        Storage.prototype.getItem = function(key) { if (key.startsWith('ormos.saved')) window.testSavedReads.push(key); return get.call(this, key); };
        window.testInputSockets = []; window.testHeldInputAcks = []; window.testHoldInputAcks = false;
        const Native = window.WebSocket;
        window.WebSocket = class extends Native {
          constructor(...args) {
            super(...args);
            if (!String(args[0]).includes('/api/terminal/')) return;
            window.testInputSockets.push(this);
            this.testInputPending = 0; this.testInputMaximum = 0;
            this.addEventListener('message', event => {
              if (typeof event.data !== 'string') return;
              const message = JSON.parse(event.data);
              if (message.type === 'replay') this.testInputWindow = message.inputWindow;
              if (message.type === 'input-ack') {
                if (window.testHoldInputAcks) { window.testHeldInputAcks.push([this, event.data]); event.stopImmediatePropagation(); return; }
                this.testInputPending -= message.bytes;
              }
            });
          }
          send(data) {
            const message = JSON.parse(data);
            if (message.type === 'input') { this.testInputPending += new TextEncoder().encode(message.data).byteLength; this.testInputMaximum = Math.max(this.testInputMaximum, this.testInputPending); }
            super.send(data);
          }
        };
      }, text);
      const page = await context.newPage();
      await page.goto(origin);
      await page.waitForFunction(() => document.querySelector('.status-dot.online'));
      const terminal = page.locator('.terminal-container');
      await terminal.click();
      await page.keyboard.type('python3 -u paste-app.py'); await page.keyboard.press('Enter');
      await page.waitForFunction(() => document.querySelector('.xterm-screen').textContent.includes('PASTE_READY'));
      await page.getByRole('button', { name: 'Terminal controls', exact: true }).click();
      const dialog = page.getByRole('dialog', { name: 'Terminal tools' });
      assert.deepEqual(await page.evaluate(() => window.testSavedReads), [], 'Keyboard menu must not read or render saved lists');
      await dialog.getByRole('tab', { name: 'Saved commands', exact: true }).click();
      await dialog.getByRole('button', { name: 'Run Only commands', exact: true }).waitFor();
      assert.deepEqual(await page.evaluate(() => window.testSavedReads), ['ormos.savedCommands']);
      await dialog.getByRole('tab', { name: 'Saved prompts', exact: true }).click();
      await dialog.getByRole('button', { name: 'Paste Large prompt', exact: true }).waitFor();
      assert.equal(await page.locator('.saved-item-title').count(), 1, 'Inactive saved list must not retain DOM');
      await page.evaluate(() => { window.testHoldInputAcks = true; });
      await dialog.getByRole('button', { name: 'Paste Large prompt', exact: true }).click();
      await page.waitForFunction(() => window.testInputSockets.at(-1).testInputPending > 25000);
      await page.waitForTimeout(100);
      assert(await page.evaluate(() => window.testInputSockets.at(-1).testInputMaximum <= 32768));
      assert(!await terminal.textContent().then(value => value.includes('PASTE_RESULT_')), 'Prompt must pause until PTY byte credits return');
      await page.evaluate(() => {
        window.testHoldInputAcks = false;
        for (const [socket, data] of window.testHeldInputAcks.splice(0)) socket.dispatchEvent(new MessageEvent('message', { data }));
      });
      const marker = `PASTE_RESULT_${bytes}_${createHash('sha256').update(text).digest('hex')}`;
      await page.waitForFunction(marker => document.querySelector('.xterm-screen').textContent.replace(/\s/g, '').includes(marker), marker);
      assert(await page.evaluate(() => window.testInputSockets.at(-1).testInputMaximum <= 32768));
      await page.evaluate(async () => {
        for (const row of JSON.parse(localStorage.getItem('ormos.terminalTabs') || '[]')) await fetch('/api/action', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action: 'kill', id: row.id }) });
      });
    } finally { await context.close(); }
  });
  for (const reduced of [false, true]) {
    await t.test(`hidden terminal pauses paint and retains output${reduced ? ' with reduced motion' : ''}`, async () => {
      await writeFile(path.join(home, 'paint-app.py'), `import time
print('PAINT_STARTED', flush=True)
for i in range(200):
    print('PAINT_ROW_%04d' % i, flush=True)
    time.sleep(0.02)
print('PAINT_FINISHED', flush=True)
`);
      const context = await browser.newContext({ reducedMotion: reduced ? 'reduce' : 'no-preference' });
      try {
        await context.addInitScript(() => {
          const Native = window.WebSocket; window.testPaintSockets = []; window.testPaintBytes = 0; window.testPaintAcks = 0;
          const Observer = window.IntersectionObserver; window.testPaintIntersections = [];
          window.IntersectionObserver = class extends Observer {
            constructor(callback, options) { super((entries, observer) => { for (const entry of entries) if (entry.target.classList.contains('xterm-screen')) window.testPaintIntersections.push(entry.isIntersecting); callback(entries, observer); }, options); }
          };
          window.WebSocket = class extends Native {
            constructor(...args) {
              super(...args); if (!String(args[0]).includes('/api/terminal/')) return;
              window.testPaintSockets.push(this);
              this.addEventListener('message', event => { if (typeof event.data !== 'string') window.testPaintBytes += event.data.byteLength; });
            }
            send(data) { const message = JSON.parse(data); if (message.type === 'ack') window.testPaintAcks += message.bytes; super.send(data); }
          };
        });
        const page = await context.newPage();
        await page.goto(origin);
        await page.waitForFunction(() => document.querySelector('.status-dot.online'));
        await page.locator('.terminal-container').click();
        // Recreate the former opacity-only behavior for a measured comparison.
        const baselineStyle = reduced ? undefined : await page.addStyleTag({ content: '.terminal-pane{transition:opacity 160ms ease!important}.terminal-pane[aria-hidden=true]{display:flex!important}' });
        await page.keyboard.type('python3 -u paint-app.py'); await page.keyboard.press('Enter');
        await page.waitForFunction(() => document.querySelector('.xterm-screen').textContent.includes('PAINT_STARTED'));
        await page.getByRole('button', { name: 'Show preview', exact: true }).click();
        if (baselineStyle) {
          await page.evaluate(() => {
            window.testBaselineMutations = 0;
            window.testBaselineObserver = new MutationObserver(records => { window.testBaselineMutations += records.length; });
            window.testBaselineObserver.observe(document.querySelector('.xterm-rows'), { childList: true, characterData: true, subtree: true });
          });
          await page.waitForFunction(() => window.testBaselineMutations > 0);
          await page.evaluate(() => window.testBaselineObserver.disconnect());
          await baselineStyle.evaluate(element => element.remove());
        }
        await page.waitForFunction(() => getComputedStyle(document.querySelector('.terminal-pane')).display === 'none');
        await page.waitForFunction(() => window.testPaintIntersections.at(-1) === false); // synchronize on xterm's actual visibility notification
        const before = await page.evaluate(() => {
          window.testPaintMutations = 0;
          window.testPaintObserver = new MutationObserver(records => { window.testPaintMutations += records.length; });
          window.testPaintObserver.observe(document.querySelector('.xterm-rows'), { childList: true, characterData: true, subtree: true });
          return { bytes: window.testPaintBytes, acks: window.testPaintAcks };
        });
        await page.waitForFunction(before => window.testPaintBytes > before.bytes + 100 && window.testPaintAcks > before.acks + 100, before);
        assert.equal(await page.evaluate(() => window.testPaintMutations), 0, 'Hidden terminal must receive history without repainting its rows');
        await page.evaluate(() => window.testPaintObserver.disconnect());
        await page.getByRole('button', { name: 'Show terminal', exact: true }).click();
        await page.waitForFunction(() => document.querySelector('.xterm-screen').textContent.includes('PAINT_FINISHED'));
        assert.equal(await page.evaluate(() => window.testPaintSockets.length), 1, 'Showing the terminal must preserve its renderer and connection');
        assert.equal(await page.locator('.terminal-pane').evaluate(element => getComputedStyle(element).transitionDuration), reduced ? '0s' : '0.16s, 0.16s');
        await page.evaluate(async () => {
          for (const row of JSON.parse(localStorage.getItem('ormos.terminalTabs') || '[]')) await fetch('/api/action', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action: 'kill', id: row.id }) });
        });
      } finally { await context.close(); }
    });
  }
  await t.test('terminal code loads in parallel with session discovery', async () => {
    const context = await browser.newContext();
    try {
      const page = await context.newPage();
      let discovery;
      await page.route('**/api/terminals', route => { discovery = route; });
      const terminalCode = page.waitForRequest(request => /\/assets\/TerminalPane-.*\.js$/.test(request.url()), { timeout: 5000 });
      await page.goto(origin);
      await terminalCode;
      assert(discovery, 'Terminal preload must not wait for session discovery to finish');
      await discovery.continue();
      await page.waitForFunction(() => document.querySelector('.status-dot.online'));
      await page.evaluate(async () => {
        for (const row of JSON.parse(localStorage.getItem('ormos.terminalTabs') || '[]')) await fetch('/api/action', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action: 'kill', id: row.id }) });
      });
    } finally { await context.close(); }
  });
  await t.test('missing lazy terminal code leaves preview usable', async () => {
    const context = await browser.newContext();
    try {
      await context.addInitScript(() => localStorage.setItem('ormos.view', 'preview'));
      const page = await context.newPage(), errors = [];
      page.on('pageerror', error => errors.push(String(error)));
      await page.route('**/assets/TerminalPane-*.js', route => route.abort('failed'));
      await page.goto(origin);
      await page.getByRole('button', { name: 'Show terminal', exact: true }).click();
      await page.getByRole('alert').getByText('Terminal unavailable', { exact: true }).waitFor();
      await page.getByRole('button', { name: 'Show preview', exact: true }).click();
      const address = page.getByRole('combobox', { name: 'Preview address' });
      await address.fill(String(firstPort)); await address.press('Enter');
      await page.frameLocator('iframe').getByRole('heading', { name: 'First app' }).waitFor();
      assert.deepEqual(errors, []);
      await page.evaluate(async () => {
        for (const row of JSON.parse(localStorage.getItem('ormos.terminalTabs') || '[]')) await fetch('/api/action', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action: 'kill', id: row.id }) });
      });
    } finally { await context.close(); }
  });
  await t.test('denied browser storage preserves terminal and preview use', async () => {
    const context = await browser.newContext();
    try {
      await context.addInitScript(() => Object.defineProperty(window, 'localStorage', { get() { throw new DOMException('Storage denied', 'SecurityError'); } }));
      const page = await context.newPage(), errors = [];
      page.on('pageerror', error => errors.push(String(error)));
      await page.goto(origin);
      await page.waitForFunction(() => document.querySelector('.status-dot.online'));
      const id = (await page.evaluate(async () => (await (await fetch('/api/terminals')).json()).terminals)).find(row => row.alive).id;
      await page.getByRole('button', { name: 'Terminal controls', exact: true }).click();
      const dialog = page.getByRole('dialog', { name: 'Terminal tools' });
      await dialog.getByRole('tab', { name: 'Saved commands', exact: true }).click();
      await dialog.getByRole('button', { name: 'Add command', exact: true }).click();
      await dialog.getByRole('textbox', { name: 'Command title', exact: true }).fill('Cannot persist');
      await dialog.getByRole('textbox', { name: 'Saved command', exact: true }).fill('echo test');
      await dialog.getByRole('button', { name: 'Save command', exact: true }).click();
      await dialog.getByRole('alert').getByText('Could not save commands in this browser.', { exact: true }).waitFor();
      await page.getByRole('button', { name: 'Terminal controls', exact: true }).click();
      await page.getByRole('button', { name: 'Show preview', exact: true }).click();
      const address = page.getByRole('combobox', { name: 'Preview address' });
      await address.fill(String(firstPort)); await address.press('Enter');
      await page.frameLocator('iframe').getByRole('heading', { name: 'First app' }).waitFor();
      await page.getByRole('button', { name: 'Show terminal', exact: true }).click();
      assert.equal(await page.locator('.xterm').count(), 1);
      assert.deepEqual(errors, []);
      await page.evaluate(async id => fetch('/api/action', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action: 'kill', id }) }), id);
    } finally { await context.close(); }
  });
  for (const touch of [false, true]) {
    await t.test(touch ? 'phone layout' : 'desktop layout', async () => {
      const context = await browser.newContext({ viewport: touch ? { width: 390, height: 844 } : { width: 1440, height: 900 }, hasTouch: touch, isMobile: touch });
      try {
        await context.addInitScript(() => {
          const Native = window.WebSocket;
          window.testTerminalSockets = [];
          window.testTerminalMessages = [];
          window.testHoldAcks = false;
          window.testHeldAcks = [];
          window.testResizeCallbacks = [];
          window.WebSocket = class extends Native {
            constructor(...args) {
              super(...args);
              if (String(args[0]).includes('/api/terminal/')) {
                window.testTerminalSockets.push(this);
                this.testPending = 0; this.testMaximum = 0; this.testWindow = 0;
                this.addEventListener('message', event => {
                  if (typeof event.data === 'string') { const message = JSON.parse(event.data); if (message.type === 'replay') this.testWindow = message.window || 0; }
                  else {
                    this.testPending += event.data.byteLength;
                    this.testMaximum = Math.max(this.testMaximum, this.testPending);
                  }
                });
              }
            }
            send(data) {
              if (this.url.includes('/api/terminal/') && typeof data === 'string') {
                const message = JSON.parse(data);
                window.testTerminalMessages.push(message);
                if (message.type === 'ack') {
                  if (window.testHoldAcks) { window.testHeldAcks.push([this, data]); return; }
                  this.testPending -= message.bytes;
                }
              }
              super.send(data);
            }
          };
          const NativeObserver = window.ResizeObserver;
          window.ResizeObserver = class extends NativeObserver {
            constructor(callback) {
              super(callback);
              this.callback = callback;
            }
            observe(element, ...args) {
              if (element.classList.contains('terminal-container')) window.testResizeCallbacks.push(this.callback);
              super.observe(element, ...args);
            }
          };
        });
        const page = await context.newPage();
        const pressControl = async button => { if (touch) await button.tap(); else await button.click(); };
        const openKeyboard = () => pressControl(page.getByRole('toolbar', { name: 'Quick terminal controls' }).getByRole('button', { name: 'Keyboard', exact: true }));
        const requests = [];
        const errors = [];
        page.on('request', request => requests.push(request.url()));
        page.on('pageerror', error => errors.push(error));
        await page.goto(origin);
        const cdp = await context.newCDPSession(page);
        const installation = await cdp.send('Page.getAppManifest');
        assert.equal(installation.url, `${origin}/manifest.webmanifest`);
        assert.deepEqual(installation.errors, []);
        const manifest = JSON.parse(installation.data);
        assert.equal(manifest.display, 'standalone');
        assert.equal(manifest.start_url, '/');
        assert.equal(await page.locator('link[rel="apple-touch-icon"]').getAttribute('href'), '/icons/apple-touch-icon.png');
        for (const icon of manifest.icons) {
          const dimensions = await page.evaluate(src => new Promise((resolve, reject) => {
            const image = new Image();
            image.onload = () => resolve(`${image.naturalWidth}x${image.naturalHeight}`);
            image.onerror = () => reject(new Error(`Cannot load installation icon ${src}`));
            image.src = src;
          }), icon.src);
          assert.equal(dimensions, icon.sizes);
        }
        const installability = await cdp.send('Page.getInstallabilityErrors');
        assert.deepEqual(installability.installabilityErrors, []);
        await cdp.detach();
        await page.waitForFunction(() => document.querySelector('.status-dot.online'));
        await page.waitForFunction(() => window.testTerminalMessages.some(message => message.type === 'resize'));
        const beforeResize = await page.evaluate(() => window.testTerminalMessages.filter(message => message.type === 'resize').length);
        await page.evaluate(async () => {
          for (let i = 0; i < 100; i++) for (const callback of window.testResizeCallbacks) callback([]);
          await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
        });
        assert.equal(await page.evaluate(() => window.testTerminalMessages.filter(message => message.type === 'resize').length), beforeResize,
          'Layout notifications without a character-grid change must not redraw the PTY');
        await page.setViewportSize({ width: touch ? 330 : 1280, height: touch ? 744 : 800 });
        await page.waitForFunction(count => window.testTerminalMessages.filter(message => message.type === 'resize').length > count, beforeResize);
        await page.setViewportSize({ width: touch ? 390 : 1440, height: touch ? 844 : 900 });
        if (touch) {
          await page.locator('.xterm-screen').tap();
          assert.equal(await page.locator('.xterm-helper-textarea').getAttribute('inputmode'), 'none');
          assert.equal(await page.locator('.xterm-helper-textarea').evaluate(node => node.readOnly), true, 'Terminal taps must not request the software keyboard');
        }
        await openKeyboard();
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
        await openKeyboard();
        await page.keyboard.type('python3 "$HOME/scroll-app.py"');
        await page.keyboard.press('Enter');
        await page.waitForFunction(() => document.querySelector('.xterm-screen').textContent.includes('APP_SCROLL_0100'));
        const appPosition = () => page.evaluate(() => Number(document.querySelector('.xterm-screen').textContent.match(/APP_SCROLL_(\d+)/)?.[1]));
        await scroll(240);
        await page.waitForFunction(() => Number(document.querySelector('.xterm-screen').textContent.match(/APP_SCROLL_(\d+)/)?.[1]) < 100, null, { timeout: 3000 });
        const previous = await appPosition();
        await scroll(-240);
        await page.waitForFunction(previous => Number(document.querySelector('.xterm-screen').textContent.match(/APP_SCROLL_(\d+)/)?.[1]) > previous, previous, { timeout: 3000 });
        const quick = page.getByRole('toolbar', { name: 'Quick terminal controls' });
        assert.equal(await quick.getByRole('button').count(), 8);
        const lastControl = await quick.getByRole('button', { name: 'More terminal keys', exact: true }).boundingBox();
        assert(lastControl.x + lastControl.width <= (touch ? 390 : 1440), 'Toolbar must fit the viewport');
        // Inspect actual WebSocket input while the fixture has a raw PTY,
        // so Ctrl+C and Esc test their bytes without terminating a shell.
        await page.evaluate(() => {
          window.testKeyInput = [];
          const socket = window.testTerminalSockets.at(-1);
          const send = socket.send.bind(socket);
          socket.send = data => { const message = JSON.parse(data); if (message.type === 'input') window.testKeyInput.push(message.data); send(data); };
        });
        const collapsed = await quick.boundingBox();
        assert.equal(await quick.getByRole('button', { name: 'Tab', exact: true }).count(), 0);
        await pressControl(quick.getByRole('button', { name: 'More terminal keys', exact: true }));
        assert.equal(await quick.getByRole('button', { name: 'Fewer terminal keys', exact: true }).getAttribute('aria-expanded'), 'true');
        const expanded = await quick.boundingBox();
        const extras = await page.locator('#extra-terminal-keys').boundingBox();
        const base = await page.locator('.quick-key-bar').boundingBox();
        assert(expanded.height > collapsed.height && expanded.y < collapsed.y, 'More keys must expand upward');
        assert(extras.y + extras.height <= base.y, 'Extra keys must appear above the compact row');
        assert.deepEqual(await page.evaluate(() => window.testKeyInput), [], 'Expanding must not send terminal input');
        await page.screenshot({ path: path.join(artifacts, `expanded-keys-${touch ? 'phone' : 'desktop'}.png`) });
        const keys = [
          ['Tab', '\t'], ['Escape', '\x1b'], ['Left arrow', '\x1b[D'], ['Up arrow', '\x1b[A'],
          ['Down arrow', '\x1b[B'], ['Right arrow', '\x1b[C'], ['Enter', '\r'], ['Ctrl C', '\x03'],
          ['Home', '\x1b[H'], ['End', '\x1b[F'], ['Page up', '\x1b[5~'], ['Page down', '\x1b[6~'],
          ['Backspace', '\x7f'], ['Delete', '\x1b[3~'], ['Clear screen (Ctrl L)', '\x0c'],
          ['Clear line (Ctrl U)', '\x15'], ['Delete word (Ctrl W)', '\x17'], ['Ctrl D', '\x04'],
        ];
        for (const [name] of keys) await pressControl(quick.getByRole('button', { name, exact: true }));
        assert.deepEqual(await page.evaluate(() => window.testKeyInput), keys.map(([, bytes]) => bytes));
        if (touch) {
          assert.equal(await page.locator('.xterm-helper-textarea').getAttribute('inputmode'), 'none');
          assert.equal(await page.locator('.xterm-helper-textarea').evaluate(node => node.readOnly), true);
        }
        await openKeyboard();
        const shift = quick.getByRole('button', { name: 'Shift', exact: true });
        await page.evaluate(() => { window.testKeyInput = []; });
        await pressControl(shift);
        await openKeyboard();
        assert.equal(await shift.getAttribute('aria-pressed'), 'true', 'Opening the keyboard preserves Shift');
        await page.keyboard.type('s');
        assert.equal(await shift.getAttribute('aria-pressed'), 'false');
        await page.keyboard.type('s');
        await pressControl(shift);
        await pressControl(quick.getByRole('button', { name: 'Tab', exact: true }));
        assert.equal(await shift.getAttribute('aria-pressed'), 'false');
        await pressControl(shift);
        await pressControl(shift);
        await page.keyboard.type('s');
        assert.deepEqual(await page.evaluate(() => window.testKeyInput), ['S', 's', '\x1b[Z', 's']);
        await pressControl(quick.getByRole('button', { name: 'Fewer terminal keys', exact: true }));
        assert.equal(await quick.getByRole('button').count(), 8);
        assert.equal(await quick.getByRole('button', { name: 'More terminal keys', exact: true }).getAttribute('aria-expanded'), 'false');
        assert.equal(await page.locator('.xterm-helper-textarea').getAttribute('inputmode'), 'text');
        assert.equal(await page.locator('.xterm-helper-textarea').evaluate(node => node.readOnly), false);
        assert.equal(await page.locator('.xterm-helper-textarea').evaluate(node => document.activeElement === node), true);
        await page.keyboard.type('q');
        await page.waitForFunction(() => document.querySelector('.xterm-screen').textContent.includes('SCROLL_ROW_0200'));
        // More than 64 KiB but fewer than 5,000 display lines: refresh must
        // retain the earliest marker, not only the old tiny server replay.
        await openKeyboard();
        await page.keyboard.type("printf '\\033c'; i=1; while [ \"$i\" -le 3000 ]; do printf 'HISTORY_%04d_abcdefghijklmnop\\n' \"$i\"; i=$((i+1)); done");
        await page.keyboard.press('Enter');
        await page.waitForFunction(() => document.querySelector('.xterm-screen').textContent.includes('HISTORY_3000'));
        await page.reload();
        await page.waitForFunction(() => document.querySelector('.status-dot.online'));
        await page.waitForFunction(() => document.querySelector('.xterm-screen').textContent.includes('HISTORY_3000'));
        const moveScrollbar = async bottom => {
          const track = await page.locator('.xterm .scrollbar.vertical').boundingBox();
          const slider = await page.locator('.xterm .scrollbar.vertical .slider').boundingBox();
          await page.mouse.move(slider.x + slider.width / 2, slider.y + slider.height / 2);
          await page.mouse.down();
          await page.mouse.move(track.x + track.width / 2, bottom ? track.y + track.height : track.y, { steps: 10 });
          await page.mouse.up();
        };
        const showEarliestHistory = async () => {
          await moveScrollbar(false);
          await page.waitForFunction(() => document.querySelector('.xterm-screen').textContent.includes('HISTORY_0001'), null, { timeout: 3000 });
        };
        await showEarliestHistory();
        const reconnects = [];
        page.on('websocket', ws => reconnects.push(ws.url()));
        await page.evaluate(() => window.testTerminalSockets.at(-1).close());
        await page.waitForFunction(() => !document.querySelector('.status-dot.online'));
        await page.waitForFunction(() => document.querySelector('.status-dot.online'));
        assert(reconnects.some(url => url.includes('since=')), 'Reconnect must request only missed bytes');
        // Reconnect must also preserve the current scroll position.
        await page.waitForFunction(() => document.querySelector('.xterm-screen').textContent.includes('HISTORY_0001'));
        await moveScrollbar(true);
        await page.waitForFunction(() => document.querySelector('.xterm-screen').textContent.includes('HISTORY_3000'));
        await page.screenshot({ path: path.join(artifacts, `terminal-${touch ? 'phone' : 'desktop'}.png`) });
        // Saved commands and prompts share the compact row and editor actions.
        for (const kind of ['command', 'prompt']) {
          await page.getByRole('button', { name: 'Terminal controls', exact: true }).click();
          const dialog = page.getByRole('dialog', { name: 'Terminal tools' });
          await dialog.getByRole('tab', { name: kind === 'command' ? 'Saved commands' : 'Saved prompts' }).click();
          await dialog.getByRole('button', { name: `Add ${kind}`, exact: true }).click();
          await dialog.getByRole('textbox', { name: kind === 'command' ? 'Command title' : 'Prompt title' }).fill(`Test ${kind}`);
          await dialog.getByRole('textbox', { name: `Saved ${kind}` }).fill('A longer saved item for the compact title and subtitle layout');
          const form = dialog.locator('.saved-command-form:visible');
          assert.equal(await form.getByRole('button', { name: `Delete ${kind}` }).count(), 0, 'New entries have nothing to delete');
          await form.getByRole('button', { name: `Save ${kind}`, exact: true }).click();
          const row = dialog.locator('.saved-command-list:visible li');
          assert.equal(await row.locator('.saved-command-actions button').count(), 1, 'Only Edit belongs beside the title');
          const titleBox = await row.locator('.saved-item-title').boundingBox();
          const textBox = await row.locator('.saved-item-text').boundingBox();
          assert(textBox.y - titleBox.y - titleBox.height <= 3, 'Subtitle stays close to its title');
          await page.screenshot({ path: path.join(artifacts, `list-${kind}-${touch ? 'phone' : 'desktop'}.png`) });
          await row.getByRole('button', { name: `Edit Test ${kind}`, exact: true }).click();
          const deleteBox = await form.getByRole('button', { name: `Delete ${kind}`, exact: true }).boundingBox();
          const saveBox = await form.getByRole('button', { name: `Save ${kind}`, exact: true }).boundingBox();
          const inputBox = await form.locator('textarea').boundingBox();
          assert(deleteBox.x < saveBox.x && deleteBox.y >= inputBox.y + inputBox.height, 'Delete left, Save right, both below inputs');
          await page.screenshot({ path: path.join(artifacts, `editor-${kind}-${touch ? 'phone' : 'desktop'}.png`) });
          await form.getByRole('button', { name: `Delete ${kind}`, exact: true }).click();
          await dialog.getByRole('status').getByText(`No saved ${kind}s`, { exact: true }).waitFor();
          await page.getByRole('button', { name: 'Terminal controls', exact: true }).click();
        }
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
        await openKeyboard();
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
        assert.equal(await page.locator('.xterm').count(), 0, 'Preview reload must defer retained terminal rendering');
        assert.equal(await page.evaluate(() => window.testTerminalSockets.length), 0);
        assert.equal(errors.length, 0, errors.map(String).join('\n'));
        // Hidden PTYs keep running, but after reload only the selected tab
        // mounts a renderer and transfers its history. Visiting another tab
        // loads it once; later switching preserves that renderer.
        await page.getByRole('button', { name: 'Show terminal', exact: true }).click();
        await page.getByRole('button', { name: 'New terminal tab', exact: true }).click();
        await page.waitForFunction(() => document.querySelectorAll('.xterm').length === 2);
        await page.getByRole('tab', { name: 'Terminal 1', exact: true }).click();
        await page.reload();
        await page.waitForFunction(() => document.querySelector('.status-dot.online'));
        assert.equal(await page.locator('.xterm').count(), 1);
        assert.equal(await page.evaluate(() => window.testTerminalSockets.length), 1);
        await page.getByRole('tab', { name: 'Terminal 2', exact: true }).click();
        await page.waitForFunction(() => window.testTerminalSockets.length === 2);
        assert.equal(await page.locator('.xterm').count(), 2);
        await page.getByRole('tab', { name: 'Terminal 1', exact: true }).click();
        assert.equal(await page.evaluate(() => window.testTerminalSockets.length), 2);
        await page.reload();
        await page.waitForFunction(() => document.querySelector('.status-dot.online'));
        await page.getByRole('button', { name: 'Close Terminal 1', exact: true }).click();
        await page.waitForFunction(() => document.querySelector('input[aria-label="Terminal tab name"]')?.value === 'Terminal 2' && document.querySelector('.tab.selected .status-dot.online'));
        assert.equal(await page.getByRole('textbox', { name: 'Terminal tab name' }).inputValue(), 'Terminal 2');
        assert.equal(await page.locator('.xterm').count(), 1);
        // Suspend only output acknowledgements. The PTY must finish a burst
        // and accept input while this browser's parsed-output window is full.
        await openKeyboard();
        await page.evaluate(() => { window.testHoldAcks = true; });
        await page.keyboard.type(`python3 -c "import sys; sys.stdout.write('x'*1048576+'\\r\\nFLOW_'+'DONE\\r\\n'); sys.stdout.flush()"`);
        await page.keyboard.press('Enter');
        await page.waitForFunction(() => window.testTerminalSockets.at(-1).testPending === 262144);
        await page.keyboard.type("printf 'INPUT_%s\\n' OK");
        await page.keyboard.press('Enter');
        await page.waitForFunction(async () => {
          const id = JSON.parse(localStorage.getItem('ormos.terminalTabs')).at(-1).id;
          const response = await fetch(`/api/terminal/${id}/output`);
          const data = await response.json();
          return data.output.includes('FLOW_DONE') && data.output.includes('INPUT_OK');
        });
        assert.equal(await page.evaluate(() => window.testTerminalSockets.at(-1).testPending), 262144,
          'Server must stop output at the parser window while input remains live');
        await page.evaluate(() => {
          window.testHoldAcks = false;
          for (const [socket, data] of window.testHeldAcks.splice(0)) socket.send(data);
        });
        await page.waitForFunction(() => document.querySelector('.xterm-screen').textContent.includes('INPUT_OK'));
        const windows = await page.evaluate(() => window.testTerminalSockets.filter(socket => socket.testWindow).map(socket => [socket.testMaximum, socket.testWindow]));
        assert(windows.length > 0);
        assert(windows.every(([maximum, window]) => maximum <= window), 'Browser backlog must remain bounded');
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
  await t.test('server shutdown closes connected sockets and reaps owned shells', async () => {
    const context = await browser.newContext();
    try {
      const page = await context.newPage();
      let closed = false;
      page.on('websocket', socket => socket.on('close', () => { closed = true; }));
      await page.goto(origin);
      await page.waitForFunction(() => document.querySelector('.status-dot.online'));
      await page.getByRole('button', { name: 'Keyboard', exact: true }).click();
      await page.keyboard.type("trap '' HUP; printf 'SHUTDOWN_%s\\n' READY; while :; do sleep 1; done");
      await page.keyboard.press('Enter');
      await page.waitForFunction(() => document.querySelector('.xterm-screen').textContent.includes('SHUTDOWN_READY'));
      const exited = once(child, 'exit');
      const began = Date.now();
      child.kill('SIGTERM');
      const timer = setTimeout(() => child.kill('SIGKILL'), 5000);
      try { await exited; } finally { clearTimeout(timer); }
      assert.equal(child.signalCode, null, 'The server must exit normally after its bounded cleanup');
      assert.equal(child.exitCode, 0);
      assert(Date.now() - began < 4000, 'Shutdown must share the grace period across terminals');
      for (let attempt = 0; !closed; attempt++) { assert(attempt < 100, 'Attached socket survived shutdown'); await new Promise(resolve => setTimeout(resolve, 10)); }
    } finally { await context.close(); }
  });
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
