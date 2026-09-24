import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { SELECTORS, createSender, pageFill, pageProbe, pageSubmit } from '../send.js';
import { createRunner, helloMessage, EXTENSION_FILES, RELOAD_RETRY_MS, RELOAD_MAX_WAIT_MS } from '../ops.js';

// ---- A fake DOM, just enough for the page functions.

class FakeEvent {
  constructor(type, init = {}) {
    this.type = type;
    Object.assign(this, init);
  }
}

class FakeDataTransfer {
  constructor() {
    this.data = {};
  }
  setData(k, v) {
    this.data[k] = v;
  }
  getData(k) {
    return this.data[k] ?? '';
  }
}

class El {
  constructor(page, tagName, text = '') {
    this.page = page;
    this.tagName = tagName;
    this.text = text;
    this.disabled = false;
    this.attrs = {};
    this.listeners = {};
    this.onclick = null;
  }
  get innerText() {
    return this.text;
  }
  get textContent() {
    return this.text;
  }
  set textContent(v) {
    if (!this.page.opts.readOnly) this.text = String(v);
  }
  focus() {
    this.page.doc.activeElement = this;
  }
  click() {
    if (this.onclick) this.onclick();
  }
  getAttribute(n) {
    return Object.hasOwn(this.attrs, n) ? this.attrs[n] : null;
  }
  addEventListener(type, fn) {
    (this.listeners[type] ||= []).push(fn);
  }
  dispatchEvent(ev) {
    for (const fn of this.listeners[ev.type] || []) fn(ev);
    return true;
  }
}

let convSeq = 0;

// FakeSite simulates one chatgpt.com or claude.ai page. opts.match picks
// which selector in the table the page answers to for each role, so tests
// can exercise the fallbacks.
class FakeSite {
  constructor(site, url, opts = {}) {
    this.site = site;
    this.sel = SELECTORS[site];
    this.opts = { execWorks: true, pasteWorks: false, streamTicks: 3, loadTicks: 1, ...opts };
    this.match = { composer: 0, send: 0, stop: 0, streaming: 0, assistant: 0, user: 0, login: 0, ...(opts.match || {}) };
    this.href = this.opts.redirectTo || url;
    this.loadLeft = this.opts.loadTicks;
    this.messages = [];
    const m = /\/(?:c|chat)\/([A-Za-z0-9_-]+)/.exec(this.href);
    if (m) this.messages.push({ role: 'user', text: 'earlier question' }, { role: 'assistant', text: 'old answer' });
    this.generating = false;
    this.submitted = [];
    this.doc = this.makeDoc();
    this.composer = new El(this, this.opts.textarea ? 'TEXTAREA' : 'DIV');
    this.composer.addEventListener('paste', (ev) => {
      if (this.opts.pasteWorks) this.composer.text += ev.clipboardData.getData('text/plain');
    });
    this.composer.addEventListener('keydown', (ev) => {
      if (ev.key === 'Enter') this.submit();
    });
    this.sendBtn = new El(this, 'BUTTON');
    this.sendBtn.onclick = () => this.submit();
    if (this.opts.sendDisabled) this.sendBtn.disabled = true;
  }
  get location() {
    const u = new URL(this.href);
    return { href: u.href, pathname: u.pathname };
  }
  role(r) {
    const s = this.sel;
    const els = [];
    switch (r) {
      case 'composer':
        if (!this.opts.noComposer) els.push(this.composer);
        break;
      case 'send':
        if (!this.opts.noSendButton) els.push(this.sendBtn);
        break;
      case 'stop':
        if (this.generating && this.opts.stopButton !== false) els.push(new El(this, 'BUTTON'));
        break;
      case 'streaming':
        if (this.generating && this.opts.streamingMarker) els.push(new El(this, 'DIV'));
        break;
      case 'login':
        if (this.opts.loggedOut) els.push(new El(this, 'A'));
        break;
      case 'assistant':
      case 'user':
        for (const m of this.messages) if (m.role === r) els.push(new El(this, 'DIV', m.text));
        break;
    }
    return s[r][this.match[r]] ? els : [];
  }
  lookup(sel) {
    for (const r of ['composer', 'send', 'stop', 'streaming', 'assistant', 'user', 'login']) {
      if (this.sel[r][this.match[r]] === sel) return this.role(r);
    }
    return [];
  }
  makeDoc() {
    const page = this;
    return {
      activeElement: null,
      querySelector: (s) => page.lookup(s)[0] || null,
      querySelectorAll: (s) => page.lookup(s),
      execCommand(cmd, _ui, val) {
        if (!page.opts.execWorks || page.opts.readOnly) return false;
        const t = this.activeElement;
        if (t !== page.composer) return false;
        if (cmd === 'insertText') t.text += val;
        if (cmd === 'delete') t.text = '';
        return true;
      },
    };
  }
  submit() {
    const text = this.composer.tagName === 'TEXTAREA' ? this.composer.value : this.composer.text;
    if (!text || this.opts.ignoreSubmit) return;
    this.submitted.push(text);
    this.messages.push({ role: 'user', text });
    this.composer.text = '';
    this.composer.value = '';
    this.generating = true;
    this.streamLeft = this.opts.streamTicks;
  }
  tick() {
    if (this.loadLeft > 0) this.loadLeft--;
    if (!this.generating) return;
    this.ticksSinceSubmit = (this.ticksSinceSubmit || 0) + 1;
    if (!/\/(?:c|chat)\//.test(this.href) && !this.opts.noId && this.ticksSinceSubmit > (this.opts.idDelayTicks || 0)) {
      const id = `new-conv-${++convSeq}`;
      this.newID = id;
      this.href = this.site === 'chatgpt' ? `https://chatgpt.com/c/${id}` : `https://claude.ai/chat/${id}`;
    }
    const last = this.messages.at(-1);
    if (last.role !== 'assistant') this.messages.push({ role: 'assistant', text: 'Part' });
    else last.text += ' more';
    if (this.opts.neverFinish) return;
    if (--this.streamLeft <= 0) {
      this.generating = false;
      this.messages.at(-1).text += ' final.';
    }
  }
}

// fakeChrome gives chrome.tabs and chrome.scripting over fake sites. Tab 1
// is the user's own chatgpt.com tab, which must never be touched.
function fakeChrome(makeSite) {
  const tabs = new Map();
  const userTab = new FakeSite('chatgpt', 'https://chatgpt.com/c/users-own-chat');
  tabs.set(1, userTab);
  let next = 100;
  const log = { created: [], removed: [], scripts: [], live: 0, maxLive: 0 };
  const install = (page) => {
    const saved = { document: globalThis.document, location: globalThis.location };
    globalThis.document = page.doc;
    globalThis.location = page.location;
    return () => {
      globalThis.document = saved.document;
      globalThis.location = saved.location;
    };
  };
  globalThis.Event = globalThis.Event || FakeEvent;
  globalThis.KeyboardEvent = FakeEvent;
  globalThis.ClipboardEvent = FakeEvent;
  globalThis.DataTransfer = FakeDataTransfer;
  const chrome = {
    tabs: {
      async create(props) {
        const id = next++;
        log.created.push({ id, ...props });
        tabs.set(id, makeSite(props.url));
        log.live++;
        log.maxLive = Math.max(log.maxLive, log.live);
        return { id, url: props.url, status: 'loading' };
      },
      async get(id) {
        const p = tabs.get(id);
        if (!p) throw new Error('No tab with id: ' + id);
        return { id, url: p.href, status: p.loadLeft > 0 ? 'loading' : 'complete' };
      },
      async remove(id) {
        log.removed.push(id);
        if (tabs.delete(id)) log.live--;
      },
    },
    scripting: {
      async executeScript(inj) {
        log.scripts.push(inj);
        const p = tabs.get(inj.target.tabId);
        if (!p) throw new Error('No tab');
        const undo = install(p);
        try {
          return [{ result: inj.func(...(inj.args || [])) }];
        } finally {
          undo();
        }
      },
    },
  };
  const pages = () => [...tabs.values()];
  let clock = 0;
  const sleep = async (ms) => {
    clock += ms;
    for (const p of pages()) p.tick();
    await null;
  };
  return { chrome, log, tabs, userTab, sleep, now: () => clock, pageFor: (id) => tabs.get(id) };
}

// fakeTimers stands in for setTimeout: timers fire only when a test says.
function fakeTimers() {
  const timers = new Map();
  let seq = 0;
  return {
    setTimer: (fn, ms) => {
      const t = ++seq;
      timers.set(t, { fn, ms });
      return t;
    },
    clearTimer: (t) => timers.delete(t),
    pending: () => [...timers.values()],
    fireAll: () => {
      for (const [t, v] of [...timers]) {
        timers.delete(t);
        v.fn();
      }
    },
  };
}

function sender(fc, extra = {}) {
  const timers = extra.timers || fakeTimers();
  return createSender({ tabs: fc.chrome.tabs, scripting: fc.chrome.scripting, sleep: fc.sleep, now: fc.now, pollMs: 1000, setTimer: timers.setTimer, clearTimer: timers.clearTimer, ...extra });
}

// Every injection is one of the fixed page functions, in the isolated
// world, with the message only ever as an argument.
function assertOnlyFixedScripts(log) {
  for (const inj of log.scripts) {
    assert.ok([pageProbe, pageFill, pageSubmit].includes(inj.func), 'unknown injected function');
    assert.equal(inj.world, 'ISOLATED');
    assert.equal(inj.code, undefined);
    assert.equal(inj.files, undefined);
    assert.notEqual(inj.target.tabId, 1, "the user's tab was scripted");
  }
}

test('chatgpt new chat: fills the composer, clicks send, returns the id from the URL without waiting for the reply', async () => {
  let page;
  const fc = fakeChrome((url) => (page = new FakeSite('chatgpt', url, { neverFinish: true })));
  const s = sender(fc);
  const r = await s.send('chatgpt', { message: 'Write a haiku about tin cans', new_chat: true });
  assert.equal(fc.log.created.length, 1);
  assert.deepEqual(fc.log.created[0].url, 'https://chatgpt.com/');
  assert.equal(fc.log.created[0].active, false, 'background tab');
  assert.deepEqual(page.submitted, ['Write a haiku about tin cans']);
  assert.equal(r.conversation_id, page.newID);
  assert.equal(r.url, `https://chatgpt.com/c/${page.newID}`);
  assert.ok(Number.isSafeInteger(r.submitted_at) && r.submitted_at <= fc.now(), `submitted_at ${r.submitted_at}`);
  assert.equal(r.reply_text, undefined, 'the reply is read by the Go side, not the page');
  assert.equal(page.generating, true, 'returned while the page still shows a stop button');
  assert.ok(fc.now() <= 5000, `returned after ${fc.now()}ms`);
  assert.deepEqual(fc.log.removed, [], 'tab kept open until close');
  assert.ok(fc.tabs.has(1), "user's tab left open");
  assertOnlyFixedScripts(fc.log);
  const fills = fc.log.scripts.filter((x) => x.func === pageFill);
  assert.equal(fills.length, 1);
  assert.deepEqual(fills[0].args[1], 'Write a haiku about tin cans');

  assert.deepEqual(await s.close('chatgpt', 'some-other-id'), { closed: 0 });
  assert.deepEqual(await s.close('claudeai', r.conversation_id), { closed: 0 }, 'close is per site');
  assert.deepEqual(fc.log.removed, []);
  assert.deepEqual(await s.close('chatgpt', r.conversation_id), { closed: 1 });
  assert.deepEqual(fc.log.removed, [100]);
  assert.deepEqual(await s.close('chatgpt', r.conversation_id), { closed: 0 }, 'closed once');
  assert.deepEqual(await s.close('chatgpt', 'users-own-chat'), { closed: 0 }, "never the user's tab");
  assert.ok(fc.tabs.has(1));
});

test('the id may show up in the URL a while after the send; the wait is bounded', async () => {
  let page;
  const fc = fakeChrome((url) => (page = new FakeSite('claudeai', url, { idDelayTicks: 8, neverFinish: true })));
  const r = await sender(fc).send('claudeai', { message: 'slow id' });
  assert.equal(r.conversation_id, page.newID);
  assert.ok(fc.now() >= 8000, `returned at ${fc.now()}ms, before the id existed`);

  const fc2 = fakeChrome((url) => new FakeSite('chatgpt', url, { noId: true, neverFinish: true }));
  await assert.rejects(sender(fc2, { idWaitMs: 60000 }).send('chatgpt', { message: 'x' }), (e) => e.code === 'timeout' && /no conversation id/.test(e.message));
  assert.ok(fc2.now() >= 60000 && fc2.now() < 80000, `gave up at ${fc2.now()}ms`);
  assert.deepEqual(fc2.log.removed, [100], 'a failed send closes its tab');
});

test('chatgpt continues a conversation: the id is known, so it returns once the page took the message', async () => {
  let page;
  const fc = fakeChrome((url) => (page = new FakeSite('chatgpt', url, { neverFinish: true })));
  const r = await sender(fc).send('chatgpt', { message: 'and a second verse', conversation_id: 'abc-123' });
  assert.equal(fc.log.created[0].url, 'https://chatgpt.com/c/abc-123');
  assert.equal(r.conversation_id, 'abc-123');
  assert.equal(r.url, 'https://chatgpt.com/c/abc-123');
  assert.equal(page.messages.filter((m) => m.role === 'user').length, 2);
  assert.ok(fc.now() <= 3000, `returned after ${fc.now()}ms`);
  assert.deepEqual(fc.log.removed, []);
});

test('never waits on stop buttons or streaming markers', async () => {
  // Pages that stream forever, with either signal, still return promptly.
  for (const opts of [{ stopButton: true }, { stopButton: false, streamingMarker: true }]) {
    const fc = fakeChrome((url) => new FakeSite('claudeai', url, { neverFinish: true, ...opts }));
    const r = await sender(fc, { timeoutMs: 60000 }).send('claudeai', { message: 'x' });
    assert.match(r.conversation_id, /^new-conv-/);
    assert.ok(fc.now() < 10000, `waited ${fc.now()}ms`);
  }
});

test('an unclosed tab is closed after keepMs', async () => {
  const fc = fakeChrome((url) => new FakeSite('claudeai', url, { neverFinish: true }));
  const timers = fakeTimers();
  const s = sender(fc, { timers, keepMs: 600000 });
  const r = await s.send('claudeai', { message: 'x' });
  assert.deepEqual(timers.pending().map((t) => t.ms), [600000]);
  timers.fireAll();
  await null;
  assert.deepEqual(fc.log.removed, [100]);
  assert.deepEqual(await s.close('claudeai', r.conversation_id), { closed: 0 });

  // Closing first cancels the timer.
  const s2 = sender(fc, { timers });
  const r2 = await s2.send('claudeai', { message: 'y' });
  assert.deepEqual(await s2.close('claudeai', r2.conversation_id), { closed: 1 });
  assert.equal(timers.pending().length, 0);
});

test('claude.ai through fallback selectors, paste fallback, streaming marker', async () => {
  let page;
  const fc = fakeChrome(
    (url) =>
      (page = new FakeSite('claudeai', url, {
        execWorks: false,
        pasteWorks: true,
        stopButton: false,
        streamingMarker: true,
        match: { composer: 1, send: 1, assistant: 2, streaming: 0 },
      })),
  );
  const r = await sender(fc).send('claudeai', { message: 'line one\n\nline two' });
  assert.equal(fc.log.created[0].url, 'https://claude.ai/new');
  assert.deepEqual(page.submitted, ['line one\n\nline two']);
  assert.equal(r.conversation_id, page.newID);
  assert.equal(r.url, `https://claude.ai/chat/${page.newID}`);
});

test('claude.ai continues /chat/<id>; ChatGPT textarea composer and Enter when there is no send button', async () => {
  const fc = fakeChrome((url) => new FakeSite('claudeai', url));
  const r = await sender(fc).send('claudeai', { message: 'hi', conversation_id: 'c1a0d000-0000-4000-8000-000000000001' });
  assert.equal(fc.log.created[0].url, 'https://claude.ai/chat/c1a0d000-0000-4000-8000-000000000001');
  assert.equal(r.conversation_id, 'c1a0d000-0000-4000-8000-000000000001');

  let page;
  const fc2 = fakeChrome((url) => (page = new FakeSite('chatgpt', url, { noSendButton: true, execWorks: false, textarea: true, match: { composer: 2 } })));
  // The fake textarea keeps its value on a plain property.
  const r2 = await sender(fc2).send('chatgpt', { message: 'enter please' });
  assert.deepEqual(page.submitted, ['enter please']);
  assert.equal(r2.conversation_id, page.newID);
});

test('no composer on the page: composer_not_found, tab closed', async () => {
  const fc = fakeChrome((url) => new FakeSite('chatgpt', url, { noComposer: true }));
  await assert.rejects(sender(fc, { loadMs: 10000 }).send('chatgpt', { message: 'x' }), (e) => e.code === 'composer_not_found');
  assert.deepEqual(fc.log.removed, [100]);
});

test('logged out page: not_logged_in, nothing typed', async () => {
  let page;
  const fc = fakeChrome((url) => (page = new FakeSite('claudeai', url, { loggedOut: true })));
  await assert.rejects(sender(fc).send('claudeai', { message: 'x' }), (e) => e.code === 'not_logged_in');
  assert.equal(fc.log.scripts.filter((s) => s.func === pageFill).length, 0);
  assert.deepEqual(page.submitted, []);
  const fc2 = fakeChrome((url) => new FakeSite('claudeai', url, { redirectTo: 'https://claude.ai/login' }));
  await assert.rejects(sender(fc2).send('claudeai', { message: 'x' }), (e) => e.code === 'not_logged_in');
  assert.deepEqual(fc2.log.removed, [100]);
});

test('text that does not land, a send button that stays disabled, a page that ignores the click: send_failed', async () => {
  for (const opts of [{ readOnly: true }, { sendDisabled: true }, { ignoreSubmit: true }]) {
    const fc = fakeChrome((url) => new FakeSite('chatgpt', url, opts));
    await assert.rejects(sender(fc).send('chatgpt', { message: 'x' }), (e) => e.code === 'send_failed', JSON.stringify(opts));
    assert.deepEqual(fc.log.removed, [100]);
  }
});

test('a conversation id that the site redirects away from is not_found, and nothing is sent', async () => {
  let page;
  const fc = fakeChrome((url) => (page = new FakeSite('chatgpt', url, { redirectTo: 'https://chatgpt.com/' })));
  await assert.rejects(sender(fc).send('chatgpt', { message: 'x', conversation_id: 'gone-1' }), (e) => e.code === 'not_found');
  assert.deepEqual(page.submitted, []);
});

test('the message is data: script-looking text is typed verbatim and never run', async () => {
  let ran = false;
  globalThis.__tincanPwned = () => {
    ran = true;
  };
  const message = '__tincanPwned()\n<img src=x onerror="__tincanPwned()"><script>__tincanPwned()</script> ${__tincanPwned()}';
  let page;
  const fc = fakeChrome((url) => (page = new FakeSite('chatgpt', url)));
  await sender(fc).send('chatgpt', { message });
  assert.deepEqual(page.submitted, [message]);
  assert.equal(ran, false);
  assertOnlyFixedScripts(fc.log);
  delete globalThis.__tincanPwned;
});

test('sends to one site run one at a time, each in its own tab', async () => {
  const fc = fakeChrome((url) => new FakeSite('chatgpt', url));
  const s = sender(fc);
  const [a, b] = await Promise.all([s.send('chatgpt', { message: 'one' }), s.send('chatgpt', { message: 'two' })]);
  assert.notEqual(a.conversation_id, b.conversation_id);
  assert.deepEqual(fc.log.created.map((c) => c.id), [100, 101]);
  await s.close('chatgpt', a.conversation_id);
  await s.close('chatgpt', b.conversation_id);
  assert.deepEqual(fc.log.removed, [100, 101]);
});

// ---- Through the runner: the session check comes first.

function jsonResponse(body, status = 200) {
  return new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } });
}

test('runner: chatgpt.send checks the session first; logged out opens no tab', async () => {
  const fc = fakeChrome((url) => new FakeSite('chatgpt', url));
  const calls = [];
  const out = createRunner({ fetch: async (u) => (calls.push(u), jsonResponse({})), sender: sender(fc) });
  await assert.rejects(out.run('chatgpt.send', { message: 'x' }, () => {}), (e) => e.code === 'not_logged_in');
  assert.deepEqual(calls, ['https://chatgpt.com/api/auth/session']);
  assert.equal(fc.log.created.length, 0);

  const frames = [];
  const ok = createRunner({ fetch: async () => jsonResponse({ accessToken: 'tok' }), sender: sender(fc) });
  await ok.run('chatgpt.send', { message: 'x' }, (f) => frames.push(f));
  assert.equal(frames.length, 1);
  assert.equal(frames[0].ok, true);
  assert.match(frames[0].result.conversation_id, /^new-conv-/);
  assert.ok(!JSON.stringify(frames).includes('tok'));
});

test('runner: claudeai.send needs an organization; without a sender send is unsupported', async () => {
  const fc = fakeChrome((url) => new FakeSite('claudeai', url));
  const r = createRunner({ fetch: async () => jsonResponse([]), sender: sender(fc) });
  await assert.rejects(r.run('claudeai.send', { message: 'x' }, () => {}), (e) => e.code === 'not_logged_in');
  assert.equal(fc.log.created.length, 0);
  const none = createRunner({ fetch: async () => jsonResponse({ accessToken: 't' }) });
  await assert.rejects(none.run('chatgpt.send', { message: 'x' }, () => {}), (e) => e.code === 'unsupported');
});

test('runner: chatgpt.close and claudeai.close close only the tab a send left open', async () => {
  const fc = fakeChrome((url) => new FakeSite('claudeai', url, { neverFinish: true }));
  const s = sender(fc);
  const r = createRunner({ fetch: async () => jsonResponse([{ uuid: 'org-1' }]), sender: s });
  const frames = [];
  await r.run('claudeai.send', { message: 'x' }, (f) => frames.push(f));
  const id = frames[0].result.conversation_id;
  const closed = [];
  await r.run('chatgpt.close', { conversation_id: id }, (f) => closed.push(f));
  assert.deepEqual(closed, [{ ok: true, result: { closed: 0 } }]);
  await r.run('claudeai.close', { conversation_id: id }, (f) => closed.push(f));
  assert.deepEqual(closed[1], { ok: true, result: { closed: 1 } });
  assert.deepEqual(fc.log.removed, [100]);
  const none = createRunner({ fetch: async () => jsonResponse({}) });
  await assert.rejects(none.run('claudeai.close', { conversation_id: id }, () => {}), (e) => e.code === 'unsupported');
});

test('busy reports owned or kept tabs; closeAllKept closes every kept tab and its timer', async () => {
  const fc = fakeChrome((url) => new FakeSite('claudeai', url, { neverFinish: true }));
  const timers = fakeTimers();
  const s = sender(fc, { timers });
  assert.equal(s.busy(), false);
  const pending = s.send('claudeai', { message: 'x' });
  assert.equal(s.busy(), true, 'busy while the send owns its tab');
  const r = await pending;
  assert.equal(s.busy(), true, 'busy while the finished tab waits for close');
  assert.deepEqual(await s.close('claudeai', r.conversation_id), { closed: 1 });
  assert.equal(s.busy(), false);

  await s.send('claudeai', { message: 'y' });
  await s.send('claudeai', { message: 'z' });
  assert.equal(timers.pending().length, 2);
  assert.deepEqual(await s.closeAllKept(), { closed: 2 });
  assert.equal(timers.pending().length, 0, 'keep timers cleared');
  assert.deepEqual(fc.log.removed, [100, 101, 102]);
  assert.equal(s.busy(), false);
  assert.ok(fc.tabs.has(1), "the user's tab is never closed");
});

const flush = () => new Promise((r) => setImmediate(r));

test('runner: extension.reload waits while a send has tabs, then reloads', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout', 'Date'] });
  let reloads = 0;
  let busy = true;
  let closedAll = 0;
  const fakeSender = { busy: () => busy, closeAllKept: async () => { closedAll++; return { closed: 0 }; } };
  const r = createRunner({ fetch: async () => jsonResponse({}), sender: fakeSender, reload: () => reloads++ });
  const frames = [];
  await r.run('extension.reload', {}, (f) => frames.push(f));
  assert.deepEqual(frames, [{ ok: true, result: { reloading: true } }]);
  t.mock.timers.tick(200);
  await flush();
  assert.equal(reloads, 0, 'deferred while busy');
  // A second request while one is pending does not start a second wait.
  await r.run('extension.reload', {}, (f) => frames.push(f));
  for (let i = 0; i < 10; i++) {
    t.mock.timers.tick(RELOAD_RETRY_MS);
    await flush();
  }
  assert.equal(reloads, 0, 'still deferred');
  busy = false;
  t.mock.timers.tick(RELOAD_RETRY_MS);
  await flush();
  assert.equal(reloads, 1, 'reloads once the sender is idle');
  assert.equal(closedAll, 0, 'nothing to force-close');
  for (let i = 0; i < 5; i++) {
    t.mock.timers.tick(RELOAD_RETRY_MS);
    await flush();
  }
  assert.equal(reloads, 1, 'one reload for both requests');
});

test('runner: extension.reload at the cap closes kept tabs, then reloads anyway', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout', 'Date'] });
  let reloads = 0;
  const order = [];
  const fakeSender = { busy: () => true, closeAllKept: async () => { order.push('closeAllKept'); return { closed: 1 }; } };
  const r = createRunner({ fetch: async () => jsonResponse({}), sender: fakeSender, reload: () => { reloads++; order.push('reload'); } });
  await r.run('extension.reload', {}, () => {});
  t.mock.timers.tick(200);
  await flush();
  let waited = 200;
  while (waited < RELOAD_MAX_WAIT_MS - RELOAD_RETRY_MS) {
    t.mock.timers.tick(RELOAD_RETRY_MS);
    waited += RELOAD_RETRY_MS;
    await flush();
  }
  assert.equal(reloads, 0, `reloaded after ${waited}ms, before the cap`);
  for (let i = 0; i < 3; i++) {
    t.mock.timers.tick(RELOAD_RETRY_MS);
    await flush();
  }
  assert.deepEqual(order, ['closeAllKept', 'reload']);
});

test('runner: extension.reload answers, then reloads', async () => {
  let reloads = 0;
  const frames = [];
  const r = createRunner({ fetch: async () => jsonResponse({}), reload: () => reloads++ });
  await r.run('extension.reload', {}, (f) => frames.push(f));
  assert.deepEqual(frames, [{ ok: true, result: { reloading: true } }]);
  assert.equal(reloads, 0, 'not before the answer is out');
  await new Promise((res) => setTimeout(res, 300));
  assert.equal(reloads, 1);
});

test('helloMessage reports the version, unpacked, and the sha256 of each file', async () => {
  const read = (f) => readFileSync(new URL('../' + f, import.meta.url));
  const fetch = async (url) => {
    const name = url.replace('chrome-extension://id/', '');
    return new Response(read(name));
  };
  const manifest = JSON.parse(read('manifest.json'));
  const m = await helloMessage({ manifest, getURL: (f) => 'chrome-extension://id/' + f, fetch });
  assert.equal(m.id, 0);
  assert.equal(m.hello.version, manifest.version);
  assert.equal(m.hello.unpacked, true);
  assert.deepEqual(Object.keys(m.hello.files), [...EXTENSION_FILES]);
  for (const f of EXTENSION_FILES) {
    assert.equal(m.hello.files[f], createHash('sha256').update(read(f)).digest('hex'), f);
  }
  const store = await helloMessage({ manifest: { ...manifest, update_url: 'https://clients2.google.com/service/update2/crx' }, getURL: (f) => f, fetch: async () => { throw new Error('x'); } });
  assert.equal(store.hello.unpacked, false);
  assert.equal(store.hello.files['ops.js'], '');
});
