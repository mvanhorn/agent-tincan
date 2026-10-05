import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { SELECTORS, SITES, createSender, pageCopilotList, pageDismiss, pageFetchImage, pageFill, pageProbe, pageSubmit } from '../send.js';
import { parseHTML } from './minidom.js';
import { createRunner, errorFrame, helloMessage, EXTENSION_FILES, RELOAD_RETRY_MS, RELOAD_MAX_WAIT_MS } from '../ops.js';

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
    if (!this.page.opts.readOnly && !this.page.trapped()) this.text = String(v);
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

// CONV_PATH matches a conversation page's address on any of the sites.
const CONV_PATH = /\/(?:c|chat|app|search)\/([A-Za-z0-9_-]+)/;

// FakeSite simulates one chatgpt.com or claude.ai page. opts.match picks
// which selector in the table the page answers to for each role, so tests
// can exercise the fallbacks.
class FakeSite {
  constructor(site, url, opts = {}) {
    this.site = site;
    this.sel = SELECTORS[site];
    this.opts = { execWorks: true, pasteWorks: false, streamTicks: 3, loadTicks: 1, ...opts };
    this.match = { composer: 0, send: 0, stop: 0, streaming: 0, assistant: 0, user: 0, login: 0, blocked: 0, signedIn: 0, dialog: 0, ...(opts.match || {}) };
    this.home = url;
    this.href = this.opts.redirectTo || url;
    this.loadLeft = this.opts.loadTicks;
    this.messages = [];
    const m = CONV_PATH.exec(this.href);
    if (m) this.messages.push({ role: 'user', text: 'earlier question' }, { role: 'assistant', text: 'old answer' });
    // answering: the page is still writing an answer when it loads.
    this.generating = Boolean(this.opts.answering);
    this.ticks = 0;
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
    // hiddenSubmit: a hidden button matching the send selector comes first
    // in the DOM; clicking it does nothing.
    this.hiddenClicks = 0;
    this.hiddenBtn = new El(this, 'BUTTON');
    this.hiddenBtn.checkVisibility = () => false;
    this.hiddenBtn.onclick = () => this.hiddenClicks++;
    // dialogs: [{within, buttons}] overlays open on load; each is found by
    // its within selector, and clicking any of its buttons closes it.
    // While one is open the composer takes no text (a focus trap).
    this.clicks = [];
    this.dialogs = (this.opts.dialogs || []).map((d) => {
      const box = { within: d.within, open: true, el: new El(this, 'DIV') };
      const btns = d.buttons.map((t) => {
        const b = new El(this, 'BUTTON', t);
        b.onclick = () => {
          this.clicks.push(t);
          box.open = false;
        };
        return b;
      });
      box.el.querySelectorAll = (q) => (q === 'button' ? btns : []);
      return box;
    });
  }
  trapped() {
    return Boolean(this.dialogs) && this.dialogs.some((d) => d.open);
  }
  get location() {
    const u = new URL(this.href);
    return { href: u.href, pathname: u.pathname, host: u.host };
  }
  get hiddenURL() {
    return Boolean(this.opts.hiddenURL);
  }
  role(r) {
    const s = this.sel;
    const els = [];
    switch (r) {
      case 'composer':
        if (!this.opts.noComposer) els.push(this.composer);
        break;
      case 'send':
        // sendReadyTick: the button stays disabled until that tick.
        if (this.opts.sendReadyTick) this.sendBtn.disabled = this.ticks < this.opts.sendReadyTick;
        // sendAfterText: the button exists only while there is text
        // (grok.com).
        if (this.opts.sendAfterText && !this.composer.text) break;
        if (this.opts.hiddenSubmit) els.push(this.hiddenBtn);
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
      case 'blocked':
        // challengeFromTick / challengeUntilTick: the challenge appears
        // after the page loaded, or passes by itself at that tick.
        if (this.opts.challenge || (this.opts.challengeFromTick && this.ticks >= this.opts.challengeFromTick) || (this.opts.challengeUntilTick && this.ticks < this.opts.challengeUntilTick)) {
          els.push(new El(this, 'FORM'));
        }
        break;
      case 'assistant':
      case 'user':
        for (const m of this.messages) if (m.role === r) els.push(new El(this, 'DIV', m.text));
        break;
      case 'signedIn':
        // signedInFromTick: the account mark renders a while after load.
        if (!this.opts.signedOut && this.ticks >= (this.opts.signedInFromTick || 0)) els.push(new El(this, 'DIV'));
        break;
      case 'dialog':
        // verify: Copilot's human check shows before anything is typed;
        // verifyOnSubmit: it shows instead of sending when Send is clicked.
        if (this.opts.otherDialog) els.push(new El(this, 'DIV', 'Chat settings'));
        if (this.opts.verify || this.verifying) els.push(new El(this, 'DIV', 'Verification required\nVerify you are human'));
        break;
    }
    return s[r] && s[r][this.match[r]] ? els : [];
  }
  lookup(sel) {
    const open = this.dialogs.filter((d) => d.open && d.within === sel).map((d) => d.el);
    if (open.length) return open;
    for (const r of ['composer', 'send', 'stop', 'streaming', 'assistant', 'user', 'login', 'blocked', 'signedIn', 'dialog']) {
      if (this.sel[r] && this.sel[r][this.match[r]] === sel) return this.role(r);
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
        if (!page.opts.execWorks || page.opts.readOnly || page.trapped()) return false;
        const t = this.activeElement;
        if (t !== page.composer) return false;
        if (cmd === 'insertText') {
          t.text += val;
          // answerOnFill: an answer to an earlier message starts while
          // the text is typed.
          if (page.opts.answerOnFill) page.generating = true;
        }
        if (cmd === 'delete') t.text = '';
        return true;
      },
    };
  }
  submit() {
    const text = this.composer.tagName === 'TEXTAREA' ? this.composer.value : this.composer.text;
    if (!text || this.opts.ignoreSubmit) return;
    if (this.opts.verifyOnSubmit) {
      this.verifying = true;
      return;
    }
    this.submitted.push(text);
    this.messages.push({ role: 'user', text });
    this.composer.text = '';
    this.composer.value = '';
    this.generating = true;
    this.streamLeft = this.opts.streamTicks;
  }
  tick() {
    this.ticks++;
    if (this.loadLeft > 0) this.loadLeft--;
    // moveAtTick/moveTo: the site changes the address at that tick (a
    // deleted conversation redirecting, or a fork).
    if (this.opts.moveTo && this.ticks === this.opts.moveAtTick) this.href = this.opts.moveTo;
    // returnAtTick: a redirectTo bounce that comes back to the page.
    if (this.opts.redirectTo && this.ticks === this.opts.returnAtTick) this.href = this.home;
    if (!this.generating) return;
    this.ticksSinceSubmit = (this.ticksSinceSubmit || 0) + 1;
    if (!CONV_PATH.test(this.href) && !this.opts.noId && this.ticksSinceSubmit > (this.opts.idDelayTicks || 0)) {
      const id = this.site === 'gemini' ? (++convSeq).toString(16).padStart(16, '0') : this.site === 'copilot' ? `c0b1107a-0000-4000-8000-${String(++convSeq).padStart(12, '0')}` : `new-conv-${++convSeq}`;
      this.newID = id;
      this.href = { chatgpt: `https://chatgpt.com/c/${id}`, claudeai: `https://claude.ai/chat/${id}`, grok: `https://grok.com/c/${id}`, gemini: `https://gemini.google.com/app/${id}`, perplexity: `https://www.perplexity.ai/search/${id}`, copilot: `https://copilot.com/chat/conversation/${id}` }[this.site];
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
        // A page may hide its address (Chrome does for a host the
        // extension has no access to).
        return { id, url: p.hiddenURL ? '' : p.href, status: p.loadLeft > 0 ? 'loading' : 'complete' };
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
    assert.ok([pageProbe, pageDismiss, pageFill, pageSubmit, pageCopilotList].includes(inj.func), 'unknown injected function');
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

test('a conversation still answering an earlier message is send_failed, nothing typed', async () => {
  // Still answering when the tab opens: refused before the fill, even
  // though the page ignores clicks and keeps showing a stop button, which
  // used to pass for the page taking the message.
  let page;
  const fc = fakeChrome((url) => (page = new FakeSite('chatgpt', url, { answering: true, neverFinish: true, ignoreSubmit: true })));
  await assert.rejects(sender(fc).send('chatgpt', { message: 'x', conversation_id: 'abc-123' }), (e) => e.code === 'send_failed' && /still answering/.test(e.message));
  assert.equal(fc.log.scripts.filter((s) => s.func === pageFill).length, 0, 'nothing typed');
  assert.deepEqual(page.submitted, []);
  assert.deepEqual(fc.log.removed, [100]);

  // An answer that starts between the fill and the click: refused right
  // before the click, so the answering is not taken for this send.
  const fc2 = fakeChrome((url) => (page = new FakeSite('chatgpt', url, { answerOnFill: true, neverFinish: true, ignoreSubmit: true })));
  await assert.rejects(sender(fc2).send('chatgpt', { message: 'x', conversation_id: 'abc-123' }), (e) => e.code === 'send_failed' && /still answering/.test(e.message));
  assert.equal(fc2.log.scripts.filter((s) => s.func === pageSubmit).length, 0, 'never clicked');
  assert.deepEqual(page.submitted, []);
  assert.deepEqual(fc2.log.removed, [100]);
});

test('an existing conversation the site moves away from: not_found before the click, the new address after it', async () => {
  // A deleted conversation shows its composer, then redirects to / while
  // the send button is still disabled. Nothing is sent.
  let page;
  const fc = fakeChrome((url) => (page = new FakeSite('chatgpt', url, { sendReadyTick: 2, moveAtTick: 2, moveTo: 'https://chatgpt.com/' })));
  await assert.rejects(sender(fc).send('chatgpt', { message: 'x', conversation_id: 'gone-1' }), (e) => e.code === 'not_found');
  assert.deepEqual(page.submitted, []);
  assert.deepEqual(fc.log.removed, [100]);

  // The site moves the conversation once the message is in (a fork): the
  // send reports where the message went, not the id it was asked for.
  const fc2 = fakeChrome((url) => (page = new FakeSite('chatgpt', url, { moveAtTick: 2, moveTo: 'https://chatgpt.com/c/forked-2', neverFinish: true })));
  const r = await sender(fc2).send('chatgpt', { message: 'x', conversation_id: 'abc-123' });
  assert.deepEqual(page.submitted, ['x']);
  assert.equal(r.conversation_id, 'forked-2');
  assert.equal(r.url, 'https://chatgpt.com/c/forked-2');
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

test('runner: claudeai.send checks the session fresh, not from the organization a read cached', async () => {
  const fc = fakeChrome((url) => new FakeSite('claudeai', url, { loggedOut: true }));
  const calls = [];
  let orgs = [{ uuid: 'org-1', capabilities: ['chat'] }];
  const r = createRunner({ fetch: async (u) => (calls.push(String(u)), jsonResponse(orgs)), sender: sender(fc) });
  await r.run('claudeai.list', { count: 1 }, () => {});
  orgs = []; // logged out since that read
  await assert.rejects(r.run('claudeai.send', { message: 'x' }, () => {}), (e) => e.code === 'not_logged_in');
  assert.equal(calls.filter((u) => u.endsWith('/api/organizations')).length, 2, 'asked claude.ai again for the send');
  assert.equal(fc.log.created.length, 0, 'no tab while logged out');
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

// ---- grok.com

test('grok new chat: types into the ProseMirror composer, clicks the submit button that appears with the text, returns the /c/<id>', async () => {
  let page;
  const fc = fakeChrome((url) => (page = new FakeSite('grok', url, { sendAfterText: true, neverFinish: true })));
  const s = sender(fc);
  const r = await s.send('grok', { message: 'Draw a fox in a tin can', new_chat: true });
  assert.equal(fc.log.created[0].url, 'https://grok.com/');
  assert.equal(fc.log.created[0].active, false);
  assert.deepEqual(page.submitted, ['Draw a fox in a tin can']);
  assert.equal(r.conversation_id, page.newID);
  assert.equal(r.url, `https://grok.com/c/${page.newID}`);
  assertOnlyFixedScripts(fc.log);
  assert.deepEqual(await s.close('chatgpt', r.conversation_id), { closed: 0 }, 'close is per site');
  assert.deepEqual(await s.close('grok', r.conversation_id), { closed: 1 });
  assert.deepEqual(fc.log.removed, [100]);
});

test('grok continues /c/<id>; with no submit button it presses Enter', async () => {
  const id = '0e1d0000-0000-4000-8000-000000000001';
  const fc = fakeChrome((url) => new FakeSite('grok', url, { sendAfterText: true }));
  const r = await sender(fc).send('grok', { message: 'shorter please', conversation_id: id });
  assert.equal(fc.log.created[0].url, `https://grok.com/c/${id}`);
  assert.equal(r.conversation_id, id);

  let page;
  const fc2 = fakeChrome((url) => (page = new FakeSite('grok', url, { noSendButton: true, match: { composer: 1 } })));
  const r2 = await sender(fc2).send('grok', { message: 'enter please' });
  assert.deepEqual(page.submitted, ['enter please']);
  assert.equal(r2.conversation_id, page.newID);
});

test('grok page showing an anti-bot challenge is blocked: nothing typed, tab closed', async () => {
  let page;
  const fc = fakeChrome((url) => (page = new FakeSite('grok', url, { challenge: true })));
  await assert.rejects(sender(fc).send('grok', { message: 'x' }), (e) => e.code === 'blocked');
  assert.equal(fc.log.scripts.filter((x) => x.func === pageFill).length, 0);
  assert.deepEqual(page.submitted, []);
  assert.deepEqual(fc.log.removed, [100]);
});

test('grok challenge that passes by itself during load: the send goes ahead', async () => {
  let page;
  const fc = fakeChrome((url) => (page = new FakeSite('grok', url, { challengeUntilTick: 4 })));
  const r = await sender(fc, { loadMs: 10000 }).send('grok', { message: 'after the check' });
  assert.deepEqual(page.submitted, ['after the check']);
  assert.equal(r.conversation_id, page.newID);
});

test('grok challenge still showing when the load time runs out: blocked, nothing typed', async () => {
  let page;
  const fc = fakeChrome((url) => (page = new FakeSite('grok', url, { challenge: true })));
  await assert.rejects(sender(fc, { loadMs: 10000 }).send('grok', { message: 'x' }), (e) => e.code === 'blocked');
  assert.ok(fc.log.scripts.filter((x) => x.func === pageProbe).length > 1, 'the challenge was polled, not refused at once');
  assert.equal(fc.log.scripts.filter((x) => x.func === pageFill).length, 0);
  assert.deepEqual(page.submitted, []);
  assert.deepEqual(fc.log.removed, [100]);
});

test('grok challenge appearing after the composer loaded: blocked, not send_failed', async () => {
  // While the send button is still disabled (the submit loop).
  let page;
  const fc = fakeChrome((url) => (page = new FakeSite('grok', url, { sendDisabled: true, challengeFromTick: 2 })));
  await assert.rejects(sender(fc).send('grok', { message: 'x' }), (e) => e.code === 'blocked');
  assert.deepEqual(page.submitted, []);
  assert.deepEqual(fc.log.removed, [100]);
  // After the click, while waiting for the page to take the message.
  const fc2 = fakeChrome((url) => new FakeSite('grok', url, { ignoreSubmit: true, challengeFromTick: 2 }));
  await assert.rejects(sender(fc2).send('grok', { message: 'x' }), (e) => e.code === 'blocked');
  assert.deepEqual(fc2.log.removed, [100]);
});

test('grok logged-out page (sign-in link or /sign-in): not_logged_in, nothing typed, tab closed', async () => {
  let page;
  const fc = fakeChrome((url) => (page = new FakeSite('grok', url, { loggedOut: true })));
  await assert.rejects(sender(fc).send('grok', { message: 'x' }), (e) => e.code === 'not_logged_in');
  assert.deepEqual(page.submitted, []);
  assert.deepEqual(fc.log.removed, [100]);
  const fc2 = fakeChrome((url) => new FakeSite('grok', url, { redirectTo: 'https://grok.com/sign-in?redirect=%2F' }));
  await assert.rejects(sender(fc2).send('grok', { message: 'x' }), (e) => e.code === 'not_logged_in');
  assert.deepEqual(fc2.log.removed, [100]);
});

test('runner: grok.send checks the grok.com session first; logged out or blocked opens no tab', async () => {
  const LIST = 'https://grok.com/rest/app-chat/conversations?pageSize=1';
  for (const [name, res, code] of [
    ['401', () => jsonResponse({ error: 'unauthenticated' }, 401), 'not_logged_in'],
    ['no list', () => jsonResponse({}), 'not_logged_in'],
    ['anti-bot 403', () => jsonResponse({ error: { code: 7, message: 'Request rejected by anti-bot rules.' } }, 403), 'blocked'],
  ]) {
    const fc = fakeChrome((url) => new FakeSite('grok', url));
    const calls = [];
    const r = createRunner({ fetch: async (u) => (calls.push(String(u)), res()), sender: sender(fc) });
    await assert.rejects(r.run('grok.send', { message: 'x' }, () => {}), (e) => e.code === code, name);
    assert.deepEqual(calls, [LIST], name);
    assert.equal(fc.log.created.length, 0, `${name}: no tab`);
  }
  // An empty list is not proof of a sign-in: a tab that opens on the
  // sign-in page is still refused before anything is typed.
  {
    let page;
    const fc = fakeChrome((url) => (page = new FakeSite('grok', url, { loggedOut: true })));
    const r = createRunner({ fetch: async () => jsonResponse({ conversations: [] }), sender: sender(fc) });
    await assert.rejects(r.run('grok.send', { message: 'x' }, () => {}), (e) => e.code === 'not_logged_in', 'empty list, signed-out tab');
    assert.equal(fc.log.scripts.filter((x) => x.func === pageFill).length, 0);
    assert.deepEqual(page.submitted, []);
    assert.deepEqual(fc.log.removed, [100]);
  }
  const fc = fakeChrome((url) => new FakeSite('grok', url, { neverFinish: true }));
  const s = sender(fc);
  const r = createRunner({ fetch: async () => jsonResponse({ conversations: [] }), sender: s });
  const frames = [];
  await r.run('grok.send', { message: 'hi grok' }, (f) => frames.push(f));
  assert.match(frames[0].result.conversation_id, /^new-conv-/);
  const closed = [];
  await r.run('grok.close', { conversation_id: frames[0].result.conversation_id }, (f) => closed.push(f));
  assert.deepEqual(closed, [{ ok: true, result: { closed: 1 } }]);
  assert.deepEqual(fc.log.removed, [100]);
});

// ---- Gemini.

test('gemini new chat: opens /app in a background tab, types into the Quill composer, returns the URL hex id', async () => {
  let page;
  const fc = fakeChrome((url) => (page = new FakeSite('gemini', url, { neverFinish: true })));
  const s = sender(fc);
  const r = await s.send('gemini', { message: 'Draw a fox logo', new_chat: true });
  assert.equal(fc.log.created[0].url, 'https://gemini.google.com/app');
  assert.equal(fc.log.created[0].active, false);
  assert.deepEqual(page.submitted, ['Draw a fox logo']);
  assert.match(r.conversation_id, /^[0-9a-f]{16}$/);
  assert.equal(r.conversation_id, page.newID);
  assert.equal(r.url, `https://gemini.google.com/app/${r.conversation_id}`);
  assertOnlyFixedScripts(fc.log);
  assert.deepEqual(await s.close('gemini', r.conversation_id), { closed: 1 });
});

test('gemini continues /app/<id>; the id is read from /u/<n>/ and /gem/ addresses too', async () => {
  let page;
  const fc = fakeChrome((url) => (page = new FakeSite('gemini', url, { neverFinish: true })));
  const r = await sender(fc).send('gemini', { message: 'and in French', conversation_id: '00000000000000d2' });
  assert.equal(fc.log.created[0].url, 'https://gemini.google.com/app/00000000000000d2');
  assert.equal(r.conversation_id, '00000000000000d2');
  assert.deepEqual(page.submitted, ['and in French']);
  const re = SITES.gemini.idFrom;
  assert.equal(re.exec('https://gemini.google.com/u/1/app/00000000000000d2?hl=en')[1], '00000000000000d2');
  assert.equal(re.exec('https://gemini.google.com/gem/coding-partner/00000000000000d2')[1], '00000000000000d2');
  assert.equal(re.exec('https://gemini.google.com/app'), null);
  assert.equal(re.exec('https://gemini.google.com.evil.example/app/00000000000000d2'), null);
});

test('a send tab sent to a sign-in host is not_logged_in, to /sorry/ is blocked; nothing is typed and the tab closes', async () => {
  for (const [to, code] of [['https://accounts.google.com/v3/signin/identifier?continue=x', 'not_logged_in'], ['https://www.google.com/sorry/index?continue=x', 'blocked']]) {
    let page;
    const fc = fakeChrome((url) => (page = new FakeSite('gemini', url, { redirectTo: to })));
    await assert.rejects(sender(fc).send('gemini', { message: 'hi' }), (e) => e.code === code, to);
    assert.deepEqual(page.submitted, []);
    assert.equal(fc.log.scripts.length, 0, 'no script in a page on another host');
    assert.deepEqual(fc.log.removed, [100]);
  }
});

test('a sign-in bounce that comes back while loading does not fail the send; one that stays does, after a settle', async () => {
  let page;
  let fc = fakeChrome((url) => (page = new FakeSite('gemini', url, { redirectTo: 'https://accounts.google.com/ServiceLogin?continue=x', returnAtTick: 1, neverFinish: true })));
  const r = await sender(fc, { settleMs: 1500 }).send('gemini', { message: 'hi', new_chat: true });
  assert.deepEqual(page.submitted, ['hi']);
  assert.equal(r.conversation_id, page.newID);
  fc = fakeChrome((url) => (page = new FakeSite('gemini', url, { redirectTo: 'https://accounts.google.com/ServiceLogin?continue=x' })));
  await assert.rejects(sender(fc, { settleMs: 1500 }).send('gemini', { message: 'hi' }), (e) => e.code === 'not_logged_in');
  assert.equal(fc.now(), 1500, 'one settle, then it fails');
  assert.deepEqual(page.submitted, []);
});

test('a redirect after the composer was found stops the send: /sorry/ is blocked, a sign-in host not_logged_in', async () => {
  for (const [to, code] of [['https://www.google.com/sorry/index?continue=x', 'blocked'], ['https://accounts.google.com/v3/signin/identifier?continue=x', 'not_logged_in']]) {
    // While the send button is still disabled (before the click).
    let page;
    let fc = fakeChrome((url) => (page = new FakeSite('gemini', url, { sendReadyTick: 5, moveTo: to, moveAtTick: 2 })));
    await assert.rejects(sender(fc).send('gemini', { message: 'hi' }), (e) => e.code === code && !e.clicked && !errorFrame(e).error.clicked, `before the click: ${to}`);
    assert.deepEqual(page.submitted, [], 'nothing sent on another host');
    assert.deepEqual(fc.log.removed, [100]);
    // After the click, while confirming.
    fc = fakeChrome((url) => (page = new FakeSite('gemini', url, { noId: true, streamTicks: 99, moveTo: to, moveAtTick: 2 })));
    // The click happened, so the failure says the message may have gone.
    await assert.rejects(sender(fc).send('gemini', { message: 'hi' }), (e) => e.code === code && e.clicked === true && errorFrame(e).error.clicked === true, `confirming: ${to}`);
    // While waiting for the new chat's id.
    fc = fakeChrome((url) => (page = new FakeSite('gemini', url, { noId: true, neverFinish: true, moveTo: to, moveAtTick: 4 })));
    await assert.rejects(sender(fc).send('gemini', { message: 'hi' }), (e) => e.code === code && e.clicked === true && errorFrame(e).error.clicked === true, `id wait: ${to}`);
    assert.deepEqual(page.submitted, ['hi']);
  }
});

test('capture fetches a Gemini image inside the tab the send left open, and only there', async () => {
  const fc = fakeChrome((url) => new FakeSite('gemini', url, { neverFinish: true }));
  const s = sender(fc);
  const r = await s.send('gemini', { message: 'Draw a fox logo', new_chat: true });
  const IMG = 'https://lh3.googleusercontent.com/gg/dummy-fox-1';
  const png = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 9, 9]);
  const saved = globalThis.fetch;
  const fetched = [];
  globalThis.fetch = async (url, init) => {
    fetched.push([url, init.credentials]);
    return url === IMG ? new Response(png, { status: 200, headers: { 'content-type': 'image/png' } }) : new Response('', { status: 403 });
  };
  try {
    const before = fc.log.scripts.length;
    const got = await s.capture('gemini', r.conversation_id, IMG, 1024);
    assert.deepEqual(got, { ok: true, mime: 'image/png', data: Buffer.from(png).toString('base64') });
    const inj = fc.log.scripts.slice(before);
    assert.equal(inj.length, 1);
    assert.equal(inj[0].func, pageFetchImage);
    assert.equal(inj[0].world, 'ISOLATED');
    assert.equal(inj[0].target.tabId, 100);
    assert.deepEqual(inj[0].args, [IMG, 1024]);
    assert.deepEqual(fetched, [[IMG, 'include']]);
    // The page's fetch failing, too large, another conversation, another
    // host, or after the close: null, and never a throw.
    assert.equal(await s.capture('gemini', r.conversation_id, 'https://lh3.googleusercontent.com/gg/missing', 1024), null);
    assert.equal(await s.capture('gemini', r.conversation_id, IMG, 3), null);
    assert.equal(await s.capture('gemini', '00000000000000ff', IMG), null);
    const n = fc.log.scripts.length;
    assert.equal(await s.capture('gemini', r.conversation_id, 'https://evil.example/x.png'), null);
    assert.equal(fc.log.scripts.length, n, 'no injection for a URL off the image host');
    await s.close('gemini', r.conversation_id);
    assert.equal(await s.capture('gemini', r.conversation_id, IMG), null);
    assert.equal(fc.log.scripts.length, n);
  } finally {
    globalThis.fetch = saved;
  }
});

test('pageFetchImage stops reading once an image passes maxBytes, and refuses a declared size over it unread', async () => {
  const saved = globalThis.fetch;
  let pulled = 0;
  let cancelled = false;
  // An endless image body, 1 KiB a chunk.
  const endless = () =>
    new ReadableStream({
      pull(c) {
        pulled++;
        c.enqueue(new Uint8Array(1024));
      },
      cancel() {
        cancelled = true;
      },
    });
  try {
    globalThis.fetch = async () => new Response(endless(), { status: 200, headers: { 'content-type': 'image/png' } });
    assert.deepEqual(await pageFetchImage('https://lh3.googleusercontent.com/gg/big', 4096), { ok: false, code: 'size' });
    assert.ok(pulled <= 8, `read ${pulled} chunks for a 4 KiB cap`);
    assert.ok(cancelled, 'the body is cancelled');
    pulled = 0;
    globalThis.fetch = async () => new Response(endless(), { status: 200, headers: { 'content-type': 'image/png', 'content-length': '999999' } });
    assert.deepEqual(await pageFetchImage('https://lh3.googleusercontent.com/gg/big', 4096), { ok: false, code: 'size' });
    assert.ok(pulled <= 1, `read ${pulled} chunks of a body declared too large`);
    // One that fits comes back whole, in chunks or not.
    const png = new Uint8Array(3000).map((_, i) => i % 251);
    globalThis.fetch = async () => new Response(png, { status: 200, headers: { 'content-type': 'image/png' } });
    assert.deepEqual(await pageFetchImage('https://lh3.googleusercontent.com/gg/ok', 4096), { ok: true, mime: 'image/png', data: Buffer.from(png).toString('base64') });
    assert.deepEqual(await pageFetchImage('https://lh3.googleusercontent.com/gg/ok', 2999), { ok: false, code: 'size' });
    globalThis.fetch = async () => new Response(new Uint8Array(0), { status: 200, headers: { 'content-type': 'image/png' } });
    assert.deepEqual(await pageFetchImage('https://lh3.googleusercontent.com/gg/empty', 4096), { ok: false, code: 'size' });
  } finally {
    globalThis.fetch = saved;
  }
});

// ---- Perplexity.

test('perplexity new chat: opens www.perplexity.ai in a background tab, types into #ask-input, returns the /search/<slug>', async () => {
  let page;
  const fc = fakeChrome((url) => (page = new FakeSite('perplexity', url, { sendAfterText: true, neverFinish: true })));
  const s = sender(fc);
  const r = await s.send('perplexity', { message: 'What is a tin can telephone?', new_chat: true });
  assert.equal(fc.log.created[0].url, 'https://www.perplexity.ai/');
  assert.equal(fc.log.created[0].active, false);
  assert.deepEqual(page.submitted, ['What is a tin can telephone?']);
  assert.equal(r.conversation_id, page.newID);
  assert.equal(r.url, `https://www.perplexity.ai/search/${page.newID}`);
  assertOnlyFixedScripts(fc.log);
  assert.equal(SELECTORS.perplexity.composer[0], 'div#ask-input[contenteditable="true"]');
  assert.deepEqual(await s.close('grok', r.conversation_id), { closed: 0 }, 'close is per site');
  assert.deepEqual(await s.close('perplexity', r.conversation_id), { closed: 1 });
  assert.deepEqual(fc.log.removed, [100]);
});

test('perplexity startup dialogs: Maybe later on the promo and Decline optional on the cookie banner, before typing; nothing else clicked', async () => {
  const dialogs = [
    { within: '[role="dialog"]', buttons: ['Get started', 'Maybe later'] },
    { within: '[class*="cookie" i]', buttons: ['Got it', 'Decline optional'] },
  ];
  let page;
  const fc = fakeChrome((url) => (page = new FakeSite('perplexity', url, { dialogs, sendAfterText: true, neverFinish: true })));
  const r = await sender(fc).send('perplexity', { message: 'What is the capital of France?', new_chat: true });
  assert.deepEqual(page.clicks, ['Maybe later', 'Decline optional']);
  assert.deepEqual(page.submitted, ['What is the capital of France?']);
  assert.equal(r.conversation_id, page.newID);
  assertOnlyFixedScripts(fc.log);
  const order = fc.log.scripts.map((x) => x.func);
  assert.ok(order.indexOf(pageDismiss) < order.indexOf(pageFill), 'dialogs close before typing');
  // Two passes at most here: one clicked, the next found nothing.
  assert.equal(order.filter((f) => f === pageDismiss).length, 2);

  // A dialog with neither button is left alone; the trapped composer then
  // fails the send before any click.
  let page2;
  const fc2 = fakeChrome((url) => (page2 = new FakeSite('perplexity', url, { dialogs: [{ within: '[role="dialog"]', buttons: ['Get started', 'Close'] }] })));
  await assert.rejects(sender(fc2).send('perplexity', { message: 'x' }), (e) => e.code === 'send_failed');
  assert.deepEqual(page2.clicks, []);
  assert.deepEqual(page2.submitted, []);

  // Other sites have no dismiss list, so pageDismiss is never injected.
  const fc3 = fakeChrome((url) => new FakeSite('chatgpt', url, { neverFinish: true }));
  await sender(fc3).send('chatgpt', { message: 'hi' });
  assert.equal(fc3.log.scripts.filter((x) => x.func === pageDismiss).length, 0);
});

test('pageDismiss clicks only exact button texts inside the listed containers', () => {
  const clicks = [];
  const btn = (t, disabled = false) => ({ innerText: t, disabled, click: () => clicks.push(t) });
  const box = (btns) => ({ querySelectorAll: () => btns });
  const doc = {
    '[role="dialog"]': [box([btn('Get started'), btn('Maybe later now'), btn(' Maybe later ', true)])],
    '[role="region"]': [box([btn('Got it'), btn('Decline optional')])],
    body: [box([btn('Maybe later')])],
  };
  const saved = globalThis.document;
  globalThis.document = { querySelectorAll: (q) => doc[q] || [] };
  try {
    assert.deepEqual(pageDismiss(SELECTORS.perplexity), { clicked: ['Decline optional'] });
    assert.deepEqual(clicks, ['Decline optional']);
    assert.deepEqual(pageDismiss({}), { clicked: [] });
  } finally {
    globalThis.document = saved;
  }
});

test('perplexity clicks the visible Submit button, not a hidden one that matches first', async () => {
  let page;
  const fc = fakeChrome((url) => (page = new FakeSite('perplexity', url, { hiddenSubmit: true, neverFinish: true })));
  const r = await sender(fc).send('perplexity', { message: 'visible only' });
  assert.equal(page.hiddenClicks, 0);
  assert.deepEqual(page.submitted, ['visible only']);
  assert.equal(r.conversation_id, page.newID);
  assert.equal(SELECTORS.perplexity.send[0], 'button[aria-label="Submit"]');
});

test('perplexity continues /search/<slug>; with no submit button it presses Enter', async () => {
  const id = '0e1d0000-0000-4000-8000-0000000000a1';
  const fc = fakeChrome((url) => new FakeSite('perplexity', url, { sendAfterText: true }));
  const r = await sender(fc).send('perplexity', { message: 'and how long can the string be?', conversation_id: id });
  assert.equal(fc.log.created[0].url, `https://www.perplexity.ai/search/${id}`);
  assert.equal(r.conversation_id, id);

  let page;
  const fc2 = fakeChrome((url) => (page = new FakeSite('perplexity', url, { noSendButton: true })));
  const r2 = await sender(fc2).send('perplexity', { message: 'enter please' });
  assert.deepEqual(page.submitted, ['enter please']);
  assert.equal(r2.conversation_id, page.newID);
  assert.match(SITES.perplexity.idFrom.exec(`https://www.perplexity.ai/search/${id}?q=1`)[1], /^0e1d/);
  assert.equal(SITES.perplexity.idFrom.exec(`https://perplexity.ai.example.com/search/${id}`), null);
});

test('perplexity page showing a Cloudflare challenge is blocked: nothing typed, tab closed', async () => {
  let page;
  const fc = fakeChrome((url) => (page = new FakeSite('perplexity', url, { challenge: true })));
  await assert.rejects(sender(fc, { loadMs: 10000 }).send('perplexity', { message: 'x' }), (e) => e.code === 'blocked');
  assert.equal(fc.log.scripts.filter((x) => x.func === pageFill).length, 0);
  assert.deepEqual(page.submitted, []);
  assert.deepEqual(fc.log.removed, [100]);
});

// ---- Copilot.

const COPILOT_CONV = 'c0b1107a-0000-4000-8000-000000000001';

test('copilot new chat: waits for the signed-in mark, types into the composer, clicks Send, returns the conversation UUID', async () => {
  let page;
  const fc = fakeChrome((url) => (page = new FakeSite('copilot', url, { sendAfterText: true, neverFinish: true, signedInFromTick: 3 })));
  const s = sender(fc);
  const r = await s.send('copilot', { message: 'How far does a tin can phone carry?', new_chat: true });
  assert.equal(fc.log.created[0].url, 'https://copilot.com/chat');
  assert.equal(fc.log.created[0].active, false);
  assert.deepEqual(page.submitted, ['How far does a tin can phone carry?']);
  assert.match(r.conversation_id, /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/);
  assert.equal(r.conversation_id, page.newID);
  assert.equal(r.url, `https://copilot.com/chat/conversation/${r.conversation_id}`);
  assert.ok(fc.now() >= 3000, 'nothing was typed before the account showed');
  assertOnlyFixedScripts(fc.log);
  assert.deepEqual(await s.close('copilot', r.conversation_id), { closed: 1 });
  assert.deepEqual(fc.log.removed, [100]);
});

test('copilot continues /chat/conversation/<id>; Enter when there is no Send button; the id pattern is copilot.com only', async () => {
  let page;
  const fc = fakeChrome((url) => (page = new FakeSite('copilot', url, { noSendButton: true, neverFinish: true })));
  const r = await sender(fc).send('copilot', { message: 'and with wire?', conversation_id: COPILOT_CONV });
  assert.equal(fc.log.created[0].url, `https://copilot.com/chat/conversation/${COPILOT_CONV}`);
  assert.equal(r.conversation_id, COPILOT_CONV);
  assert.deepEqual(page.submitted, ['and with wire?']);
  const re = SITES.copilot.idFrom;
  assert.equal(re.exec(`https://copilot.com/chat/conversation/${COPILOT_CONV}?x=1`)[1], COPILOT_CONV);
  assert.equal(re.exec('https://copilot.com/chat'), null);
  assert.equal(re.exec(`https://copilot.com.evil.example/chat/conversation/${COPILOT_CONV}`), null);
  assert.equal(re.exec('https://copilot.com/chat/conversation/not-a-uuid'), null);
});

test('a copilot page with a composer but no signed-in account is not_logged_in: nothing typed, tab closed', async () => {
  let page;
  const fc = fakeChrome((url) => (page = new FakeSite('copilot', url, { signedOut: true })));
  await assert.rejects(sender(fc, { loadMs: 10000 }).send('copilot', { message: 'x' }), (e) => e.code === 'not_logged_in' && /signed-in account/.test(e.message));
  assert.equal(fc.log.scripts.filter((x) => x.func === pageFill).length, 0);
  assert.deepEqual(page.submitted, []);
  assert.deepEqual(fc.log.removed, [100]);
});

test('perplexity signed-out page, sign-in path or a move to another host: not_logged_in, nothing typed, tab closed', async () => {
  for (const opts of [{ loggedOut: true }, { redirectTo: 'https://www.perplexity.ai/auth/signin?redirect=%2F' }, { redirectTo: 'https://accounts.example.com/login' }]) {
    let page;
    const fc = fakeChrome((url) => (page = new FakeSite('perplexity', url, opts)));
    await assert.rejects(sender(fc).send('perplexity', { message: 'x' }), (e) => e.code === 'not_logged_in', JSON.stringify(opts));
    assert.deepEqual(page.submitted, []);
    assert.equal(fc.log.scripts.filter((x) => x.func === pageFill).length, 0);
    assert.deepEqual(fc.log.removed, [100]);
  }
});

test('a copilot send tab sent to a Microsoft sign-in or terms page, or one Chrome hides, is not_logged_in with nothing typed', async () => {
  for (const [opts, want] of [
    [{ redirectTo: 'https://login.live.com/oauth20_authorize.srf?client_id=x' }, /login\.live\.com/],
    [{ redirectTo: 'https://login.microsoftonline.com/common/oauth2/v2.0/authorize?x=1' }, /login\.microsoftonline\.com/],
    [{ redirectTo: 'https://account.live.com/tou/accrue?mkt=en-US' }, /account\.live\.com/],
    [{ redirectTo: 'https://m365.cloud.microsoft/chat' }, /work or school account/],
    [{ hiddenURL: true }, /sign-in or terms page/],
  ]) {
    let page;
    const fc = fakeChrome((url) => (page = new FakeSite('copilot', url, opts)));
    await assert.rejects(sender(fc).send('copilot', { message: 'hi' }), (e) => e.code === 'not_logged_in' && want.test(e.message), JSON.stringify(opts));
    assert.deepEqual(page.submitted, []);
    assert.equal(fc.log.scripts.length, 0, 'no script in a page off copilot.com');
    assert.deepEqual(fc.log.removed, [100]);
  }
});

test('copilot human check: blocked before anything is typed; shown on the Send click, blocked and marked clicked', async () => {
  let page;
  let fc = fakeChrome((url) => (page = new FakeSite('copilot', url, { verify: true })));
  await assert.rejects(sender(fc, { loadMs: 10000 }).send('copilot', { message: 'x' }), (e) => e.code === 'blocked' && !e.clicked);
  assert.equal(fc.log.scripts.filter((x) => x.func === pageFill).length, 0);
  assert.deepEqual(fc.log.removed, [100]);
  fc = fakeChrome((url) => (page = new FakeSite('copilot', url, { verifyOnSubmit: true })));
  await assert.rejects(sender(fc).send('copilot', { message: 'x' }), (e) => e.code === 'blocked' && e.clicked === true && errorFrame(e).error.clicked === true);
  assert.deepEqual(page.submitted, [], 'the check stopped the send');
  assert.deepEqual(fc.log.removed, [100]);
  // Another dialog is not a human check.
  fc = fakeChrome((url) => (page = new FakeSite('copilot', url, { otherDialog: true, neverFinish: true })));
  const r = await sender(fc).send('copilot', { message: 'y' });
  assert.equal(r.conversation_id, page.newID);
});

// HtmlSite is a copilot.com page built from an HTML fixture, for the
// sidebar read. more holds HTML batches the sidebar appends each time a
// link is scrolled into view.
const SIDEBAR = readFileSync(new URL('../../internal/history/testdata/copilot/sidebar.html', import.meta.url), 'utf8');

class HtmlSite {
  constructor(url, html, opts = {}) {
    this.opts = opts;
    this.href = opts.redirectTo || url;
    this.loadLeft = opts.loadTicks ?? 1;
    this.doc = parseHTML(html);
    this.scrolls = 0;
    this.doc.onScrollIntoView = () => {
      this.scrolls++;
      const more = (opts.more || [])[this.scrolls - 1];
      if (more) this.doc.querySelector('#m365-copilot-chats-section').insertHTML(more);
    };
  }
  get location() {
    const u = new URL(this.href);
    return { href: u.href, pathname: u.pathname, host: u.host };
  }
  get hiddenURL() {
    return Boolean(this.opts.hiddenURL);
  }
  tick() {
    if (this.loadLeft > 0) this.loadLeft--;
  }
}

const chatLinks = (from, n) => Array.from({ length: n }, (_, i) => `<a href="/chat/conversation/c0b1107a-0000-4000-8000-${String(from + i).padStart(12, '0')}" aria-label="Chat ${from + i}">Chat ${from + i}</a>`).join('');

test('pageCopilotList reads the sidebar links: ids from copilot.com hrefs, titles from aria-label or text, once each', () => {
  const page = new HtmlSite('https://copilot.com/chat', SIDEBAR);
  const saved = { document: globalThis.document, location: globalThis.location };
  globalThis.document = page.doc;
  globalThis.location = page.location;
  try {
    assert.deepEqual(pageCopilotList(SELECTORS.copilot.read, false), {
      found: true,
      conversations: [
        { id: 'c0b1107a-0000-4000-8000-000000000001', title: 'Tin can telephones' },
        { id: 'c0b1107a-0000-4000-8000-000000000002', title: 'Morse code basics' },
        { id: 'c0b1107a-0000-4000-8000-000000000003', title: 'String phone history' },
      ],
    });
    assert.equal(page.scrolls, 0, 'no scroll unless asked');
    // The probe sees the composer and the signed-in mark on the same page.
    const p = pageProbe(SELECTORS.copilot);
    assert.equal(p.composer, true);
    assert.equal(p.signedIn, true);
    assert.equal(p.loggedOut, false);
    assert.equal(p.blocked, false);
    globalThis.document = parseHTML('<html><body><main>no sidebar</main></body></html>');
    assert.deepEqual(pageCopilotList(SELECTORS.copilot.read, true), { found: false, conversations: [] });
  } finally {
    globalThis.document = saved.document;
    globalThis.location = saved.location;
  }
});

test('copilot readList opens /chat in its own tab, scrolls the sidebar for more, stops at count, and closes the tab', async () => {
  let page;
  const fc = fakeChrome((url) => (page = new HtmlSite(url, SIDEBAR, { more: [chatLinks(10, 4), chatLinks(14, 4)] })));
  const s = sender(fc);
  const r = await s.readList('copilot', 9);
  assert.equal(fc.log.created[0].url, 'https://copilot.com/chat');
  assert.equal(fc.log.created[0].active, false);
  assert.equal(r.conversations.length, 9);
  assert.equal(r.more, undefined);
  assert.deepEqual(r.conversations.slice(0, 4).map((c) => c.title), ['Tin can telephones', 'Morse code basics', 'String phone history', 'Chat 10']);
  assert.ok(page.scrolls >= 2, 'the sidebar was scrolled for more');
  assert.deepEqual(fc.log.removed, [100], 'the read closes its tab');
  assert.equal(s.busy(), false);
  assertOnlyFixedScripts(fc.log);
  // Nothing more loads: the whole list, not flagged as cut short.
  const fc2 = fakeChrome((url) => new HtmlSite(url, SIDEBAR));
  const r2 = await sender(fc2).readList('copilot', 50);
  assert.equal(r2.conversations.length, 3);
  assert.equal(r2.more, undefined);
  // Still growing when the rounds run out: more is set.
  const fc3 = fakeChrome((url) => new HtmlSite(url, SIDEBAR, { more: Array.from({ length: 20 }, (_, i) => chatLinks(100 + i * 2, 2)) }));
  const r3 = await sender(fc3, { listRounds: 3 }).readList('copilot', 50);
  assert.equal(r3.more, true);
  assert.ok(r3.conversations.length < 50);
});

test('copilot readList: no chat list on a signed-in page is an empty list; a signed-out or sign-in page is not_logged_in', async () => {
  const empty = '<html><body><button id="mectrl_main_trigger">Account</button><main>no chats yet</main></body></html>';
  const fc = fakeChrome((url) => new HtmlSite(url, empty));
  assert.deepEqual(await sender(fc, { listWaitMs: 5000 }).readList('copilot', 5), { conversations: [] });
  assert.deepEqual(fc.log.removed, [100]);
  const signedOut = '<html><body><button aria-label="Sign in">Sign in</button></body></html>';
  const fc2 = fakeChrome((url) => new HtmlSite(url, signedOut));
  await assert.rejects(sender(fc2).readList('copilot', 5), (e) => e.code === 'not_logged_in');
  assert.deepEqual(fc2.log.removed, [100]);
  const noAccount = '<html><body><main>welcome</main></body></html>';
  const fc3 = fakeChrome((url) => new HtmlSite(url, noAccount));
  await assert.rejects(sender(fc3, { loadMs: 5000 }).readList('copilot', 5), (e) => e.code === 'not_logged_in' && /signed-in account/.test(e.message));
  for (const opts of [{ redirectTo: 'https://login.live.com/oauth20_authorize.srf' }, { hiddenURL: true }]) {
    const fc4 = fakeChrome((url) => new HtmlSite(url, SIDEBAR, opts));
    await assert.rejects(sender(fc4).readList('copilot', 5), (e) => e.code === 'not_logged_in', JSON.stringify(opts));
    assert.equal(fc4.log.scripts.length, 0);
    assert.deepEqual(fc4.log.removed, [100]);
  }
});

test('copilot reads keep a gap between tabs and run one at a time with sends', async () => {
  const fc = fakeChrome((url) => (url.includes('/conversation/') ? new FakeSite('copilot', url, { neverFinish: true }) : new HtmlSite(url, SIDEBAR)));
  const s = sender(fc, { readGapMs: 3000 });
  const opened = [];
  const origCreate = fc.chrome.tabs.create;
  fc.chrome.tabs.create = async (props) => (opened.push(fc.now()), origCreate(props));
  const a = s.readList('copilot', 3);
  const b = s.send('copilot', { message: 'hi', conversation_id: COPILOT_CONV });
  const c = s.readList('copilot', 3);
  await Promise.all([a, b, c]);
  assert.equal(fc.log.maxLive, 2, 'one tab at a time working, beside the tab the send left open for its reply');
  assert.ok(opened[2] - opened[0] >= 3000, `reads ${opened} too close`);
  await assert.rejects(s.readList('grok', 3), (e) => e.code === 'bad_request');
});

test('copilot readList: a human check that shows while the sidebar is read is blocked, and the tab is closed', async () => {
  const dialog = '<div role="dialog">Verification required<p>Verify you are human</p></div>';
  const fc = fakeChrome((url) => new HtmlSite(url, SIDEBAR, { more: [chatLinks(10, 2), dialog] }));
  await assert.rejects(sender(fc).readList('copilot', 50), (e) => e.code === 'blocked');
  assert.deepEqual(fc.log.removed, [100]);
  assertOnlyFixedScripts(fc.log);
});

test('copilot readList: a read that waited out its time behind a send gives up without opening a tab', async () => {
  let n = 0;
  const fc = fakeChrome((url) => (n++ === 0 ? new FakeSite('copilot', url, { loadTicks: 30, neverFinish: true }) : new HtmlSite(url, SIDEBAR)));
  const s = sender(fc, { readMs: 20000 });
  const a = s.send('copilot', { message: 'hi' });
  const b = s.readList('copilot', 3);
  const r = await a;
  assert.ok(r.conversation_id);
  await assert.rejects(b, (e) => e.code === 'timeout');
  assert.equal(fc.log.created.length, 1, 'the late read opened no tab');
});

test('a send that waited out its time behind a tab read gives up before opening a tab, so it cannot go out after the host gave up', async () => {
  let n = 0;
  const fc = fakeChrome((url) => (n++ === 0 ? new HtmlSite(url, SIDEBAR, { loadTicks: 30 }) : new FakeSite('copilot', url, { neverFinish: true })));
  const s = sender(fc, { timeoutMs: 20000 });
  const a = s.readList('copilot', 3);
  const b = s.send('copilot', { message: 'hi' });
  assert.equal((await a).conversations.length, 3);
  await assert.rejects(b, (e) => e.code === 'timeout' && !e.clicked);
  assert.equal(fc.log.created.length, 1, 'the late send opened no tab');
});

test('runner: copilot.list reads the sidebar through the sender', async () => {
  const fc = fakeChrome((url) => new HtmlSite(url, SIDEBAR));
  const r = createRunner({ fetch: async () => { throw new Error('no fetch'); }, sender: sender(fc) });
  const frames = [];
  await r.run('copilot.list', { count: 2 }, (f) => frames.push(f));
  assert.deepEqual(frames, [{ ok: true, result: { conversations: [{ id: 'c0b1107a-0000-4000-8000-000000000001', title: 'Tin can telephones' }, { id: 'c0b1107a-0000-4000-8000-000000000002', title: 'Morse code basics' }] } }]);
});

test('runner: perplexity.send checks for a signed-in user first; signed out opens no tab even though the page has a composer', async () => {
  const SESSION_URL = 'https://www.perplexity.ai/api/auth/session';
  for (const [name, res, code] of [
    ['signed out {}', () => jsonResponse({}), 'not_logged_in'],
    ['no user id', () => jsonResponse({ user: { email: 'owner@example.com' } }), 'not_logged_in'],
    ['401', () => jsonResponse({}, 401), 'not_logged_in'],
  ]) {
    // The page itself would take an anonymous ask.
    const fc = fakeChrome((url) => new FakeSite('perplexity', url));
    const calls = [];
    const r = createRunner({ fetch: async (u) => (calls.push(String(u)), res()), sender: sender(fc) });
    await assert.rejects(r.run('perplexity.send', { message: 'x' }, () => {}), (e) => e.code === code, name);
    assert.deepEqual(calls, [SESSION_URL], name);
    assert.equal(fc.log.created.length, 0, `${name}: no tab`);
  }
  // Signed in per the session, but the tab shows a signed-out page: the
  // second gate refuses before anything is typed.
  {
    let page;
    const fc = fakeChrome((url) => (page = new FakeSite('perplexity', url, { loggedOut: true })));
    const r = createRunner({ fetch: async () => jsonResponse({ user: { id: 'dummy-user-id' } }), sender: sender(fc) });
    await assert.rejects(r.run('perplexity.send', { message: 'x' }, () => {}), (e) => e.code === 'not_logged_in');
    assert.equal(fc.log.scripts.filter((x) => x.func === pageFill).length, 0);
    assert.deepEqual(page.submitted, []);
    assert.deepEqual(fc.log.removed, [100]);
  }
  const fc = fakeChrome((url) => new FakeSite('perplexity', url, { neverFinish: true }));
  const r = createRunner({ fetch: async () => jsonResponse({ user: { id: 'dummy-user-id' } }), sender: sender(fc) });
  const frames = [];
  await r.run('perplexity.send', { message: 'hi perplexity' }, (f) => frames.push(f));
  assert.match(frames[0].result.conversation_id, /^new-conv-/);
  const closed = [];
  await r.run('perplexity.close', { conversation_id: frames[0].result.conversation_id }, (f) => closed.push(f));
  assert.deepEqual(closed, [{ ok: true, result: { closed: 1 } }]);
  assert.deepEqual(fc.log.removed, [100]);
});

// ---- OpenAI dots: the send types into the dot's DM page and confirms by
// the room feed, since the page's address never changes.

const DOT_THREAD = '0d0d0d0d-1111-7222-8333-000000000001';
const DOT_ROOM = '0123456789abcdef0123456789abcdef';
const DOT_OWNER = 'owner-acct';
const DOT_BOT = 'dot-acct';

// dotFetch answers the session, the dot record, the room and its feed.
// The feed holds an old exchange plus, for each message a dots page in
// fc took, an owner message (after appearAfter feed reads, unless never).
// Every feed read is counted. paused may be a function of the dot record
// read's number (1 for the first), for a dot paused partway through.
function dotFetch(fc, { paused = false, never = false, appearAfter = 1, echo = (t) => t } = {}) {
  const calls = [];
  let feedReads = 0;
  let recordReads = 0;
  const fresh = new Map();
  const fn = async (url) => {
    const u = String(url);
    calls.push(u);
    if (u === 'https://chatgpt.com/api/auth/session') return jsonResponse({ accessToken: 'tok' });
    if (u === `https://chatgpt.com/backend-api/tbo/by-thread/${DOT_THREAD}`) {
      recordReads++;
      const p = typeof paused === 'function' ? paused(recordReads) === true : paused;
      return jsonResponse({ id: 't1', messaging_room_id: DOT_ROOM, is_paused: p, status: p ? 'paused' : 'active' });
    }
    if (u === `https://chatgpt.com/backend-api/messaging/rooms/${DOT_ROOM}`) return jsonResponse({ id: DOT_ROOM, type: 'DM', creator_account_user_id: DOT_OWNER, members: [] });
    if (u.startsWith(`https://chatgpt.com/backend-api/messaging/rooms/${DOT_ROOM}/messages?limit=`)) {
      feedReads++;
      const items = [
        { id: 'old-1', created_at: '2026-09-01T00:00:00Z', account_user_id: DOT_OWNER, content: { text: 'ping', attachments: [] } },
        { id: 'old-2', created_at: '2026-09-01T00:00:05Z', account_user_id: DOT_BOT, content: { text: 'pong', attachments: [] } },
      ];
      const texts = [...fc.tabs.values()].filter((p) => p.site === 'dots').flatMap((p) => p.submitted);
      texts.forEach((t, i) => {
        if (never) return;
        if (!fresh.has(i)) fresh.set(i, { at: new Date().toISOString(), seen: feedReads });
        const m = fresh.get(i);
        if (feedReads - m.seen >= appearAfter) items.push({ id: `new-${i + 1}`, created_at: m.at, account_user_id: DOT_OWNER, content: { text: echo(t), attachments: [] } });
      });
      return jsonResponse({ items, prev_cursor: null, next_cursor: null });
    }
    return jsonResponse({ detail: 'not found' }, 404);
  };
  fn.calls = calls;
  fn.feedReads = () => feedReads;
  return fn;
}

function dotSite(url, opts = {}) {
  return new FakeSite('dots', url, { noId: true, neverFinish: true, ...opts });
}

test('dots.send types into the DM, clicks Send, and returns the new owner message id from the feed', async () => {
  let page;
  const fc = fakeChrome((url) => (page = dotSite(url)));
  const f = dotFetch(fc, { appearAfter: 2, echo: (t) => ` ${t}\n` });
  const r = createRunner({ fetch: f, sender: sender(fc) });
  const frames = [];
  await r.run('dots.send', { message: 'summarize the launch notes', conversation_id: DOT_THREAD }, (x) => frames.push(x));
  assert.equal(fc.log.created.length, 1);
  assert.equal(fc.log.created[0].url, `https://chatgpt.com/dots/${DOT_THREAD}`);
  assert.equal(fc.log.created[0].active, false, 'background tab');
  assert.deepEqual(page.submitted, ['summarize the launch notes']);
  assert.equal(frames.length, 1);
  const res = frames[0].result;
  assert.equal(res.conversation_id, DOT_THREAD);
  assert.equal(res.message_id, 'new-1');
  assert.equal(res.url, `https://chatgpt.com/dots/${DOT_THREAD}`);
  assert.ok(Number.isSafeInteger(res.submitted_at));
  assert.ok(!JSON.stringify(frames).includes('tok'));
  assert.ok(f.feedReads() >= 3, 'polled the feed until the message showed');
  assert.deepEqual(fc.log.removed, [], 'tab kept open until close');
  assertOnlyFixedScripts(fc.log);
  const fills = fc.log.scripts.filter((x) => x.func === pageFill);
  assert.equal(fills.length, 1);
  assert.equal(fills[0].args[1], 'summarize the launch notes');
});

test('dots.send refuses when the composer already has text, and leaves the text alone', async () => {
  let page;
  const fc = fakeChrome((url) => {
    page = dotSite(url);
    page.composer.text = 'owner draft in progress';
    return page;
  });
  const r = createRunner({ fetch: dotFetch(fc), sender: sender(fc) });
  await assert.rejects(r.run('dots.send', { message: 'x', conversation_id: DOT_THREAD }, () => {}), (e) => e.code === 'send_failed' && /already has text/.test(e.message) && e.clicked !== true);
  assert.equal(page.composer.text, 'owner draft in progress');
  assert.deepEqual(page.submitted, []);
  assert.equal(fc.log.scripts.filter((x) => x.func === pageFill || x.func === pageSubmit).length, 0, 'nothing typed or clicked');
  assert.deepEqual(fc.log.removed, [100]);
});

test('pageFill never types over a draft on a site that keeps drafts', () => {
  const page = dotSite(`https://chatgpt.com/dots/${DOT_THREAD}`);
  page.composer.text = 'draft';
  const saved = { document: globalThis.document, location: globalThis.location };
  globalThis.document = page.doc;
  globalThis.location = page.location;
  try {
    page.composer.focus();
    assert.deepEqual(pageFill(SELECTORS.dots, 'x'), { ok: false, code: 'composer_busy', message: 'the message box already has text' });
    assert.equal(page.composer.text, 'draft');
  } finally {
    globalThis.document = saved.document;
    globalThis.location = saved.location;
  }
});

test('dots.send gives up, closing its own tab, when no new owner message shows in the feed', async () => {
  let page;
  const fc = fakeChrome((url) => (page = dotSite(url)));
  const f = dotFetch(fc, { never: true });
  const r = createRunner({ fetch: f, sender: sender(fc) });
  let caught;
  await assert.rejects(r.run('dots.send', { message: 'hello?', conversation_id: DOT_THREAD }, () => {}), (e) => ((caught = e), e.code === 'timeout'));
  assert.equal(errorFrame(caught).error.clicked, true, 'marked as maybe sent');
  assert.match(caught.message, /sent/);
  assert.deepEqual(page.submitted, ['hello?']);
  assert.deepEqual(fc.log.removed, [100]);
  assert.ok(fc.now() >= 60000 && fc.now() < 90000, `gave up at ${fc.now()}ms`);
  assert.ok(f.feedReads() >= 20 && f.feedReads() <= 40, `read the feed ${f.feedReads()} times`);
  // A message with other text is not the one sent.
  const fc2 = fakeChrome((url) => dotSite(url));
  const r2 = createRunner({ fetch: dotFetch(fc2, { echo: () => 'something else' }), sender: sender(fc2) });
  await assert.rejects(r2.run('dots.send', { message: 'hello?', conversation_id: DOT_THREAD }, () => {}), (e) => e.code === 'timeout');
});

test('dots.send refuses a paused dot before opening a tab', async () => {
  const fc = fakeChrome((url) => dotSite(url));
  const r = createRunner({ fetch: dotFetch(fc, { paused: true }), sender: sender(fc) });
  await assert.rejects(r.run('dots.send', { message: 'x', conversation_id: DOT_THREAD }, () => {}), (e) => e.code === 'paused');
  assert.equal(fc.log.created.length, 0);
  // The sender alone refuses a dots send with no thread or no feed check.
  await assert.rejects(sender(fc).send('dots', { message: 'x' }), (e) => e.code === 'bad_request');
  await assert.rejects(sender(fc).send('dots', { message: 'x', conversation_id: DOT_THREAD }), (e) => e.code === 'internal');
  assert.equal(fc.log.created.length, 0);
});

// The early check runs before the send joins the site's queue; a dot
// paused while the send waited or its tab loaded is caught by the recheck
// in the queued send, before anything is typed or clicked.
test('dots.send rechecks pause in the queued send: a dot paused after the early check gets nothing', async () => {
  for (const pausedFrom of [2, 3]) {
    let page;
    const fc = fakeChrome((url) => (page = dotSite(url)));
    const r = createRunner({ fetch: dotFetch(fc, { paused: (n) => n >= pausedFrom }), sender: sender(fc) });
    let caught;
    await assert.rejects(r.run('dots.send', { message: 'x', conversation_id: DOT_THREAD }, () => {}), (e) => ((caught = e), e.code === 'paused'));
    assert.notEqual(errorFrame(caught).error.clicked, true, `paused from read ${pausedFrom}: not marked as maybe sent`);
    assert.equal(fc.log.created.length, 1, 'the early check passed, so the tab opened');
    assert.deepEqual(page.submitted, [], `paused from read ${pausedFrom}: nothing sent`);
    assert.equal(fc.log.scripts.filter((x) => x.func === pageSubmit).length, 0, `paused from read ${pausedFrom}: no click`);
    if (pausedFrom === 2) assert.equal(fc.log.scripts.filter((x) => x.func === pageFill).length, 0, 'paused before the fill: nothing typed');
    assert.deepEqual(fc.log.removed, [100], 'its own tab closed');
  }
});

test('dots.close closes only the tab its send opened', async () => {
  const fc = fakeChrome((url) => dotSite(url));
  fc.tabs.set(2, dotSite(`https://chatgpt.com/dots/${DOT_THREAD}`)); // the owner's own DM tab
  const r = createRunner({ fetch: dotFetch(fc), sender: sender(fc) });
  const frames = [];
  await r.run('dots.send', { message: 'x', conversation_id: DOT_THREAD }, (x) => frames.push(x));
  const closed = [];
  await r.run('dots.close', { conversation_id: 'another-thread' }, (x) => closed.push(x));
  await r.run('chatgpt.close', { conversation_id: DOT_THREAD }, (x) => closed.push(x));
  assert.deepEqual(fc.log.removed, []);
  await r.run('dots.close', { conversation_id: DOT_THREAD }, (x) => closed.push(x));
  await r.run('dots.close', { conversation_id: DOT_THREAD }, (x) => closed.push(x));
  assert.deepEqual(closed.map((c) => c.result.closed), [0, 0, 1, 0]);
  assert.deepEqual(fc.log.removed, [100]);
  assert.ok(fc.tabs.has(1) && fc.tabs.has(2), "the owner's tabs stay open");
});

test('the dots selectors match the DM page markup', () => {
  const html = `<html><body><main><div class="ProseMirror" contenteditable="true" role="textbox" aria-label="Message"><p>draft</p></div><button aria-label="Send">Send</button></main></body></html>`;
  const saved = { document: globalThis.document, location: globalThis.location };
  globalThis.document = parseHTML(html);
  globalThis.location = { href: `https://chatgpt.com/dots/${DOT_THREAD}`, pathname: `/dots/${DOT_THREAD}`, host: 'chatgpt.com' };
  try {
    const p = pageProbe(SELECTORS.dots);
    assert.equal(p.composer, true);
    assert.equal(p.composerEmpty, false);
    assert.equal(p.loggedOut, false);
    assert.ok(document.querySelector(SELECTORS.dots.send[0]));
    assert.equal(SITES.dots.convURL(DOT_THREAD), `https://chatgpt.com/dots/${DOT_THREAD}`);
    assert.equal(SITES.dots.idFrom.exec(`https://chatgpt.com/dots/${DOT_THREAD}?x=1`)[1], DOT_THREAD);
  } finally {
    globalThis.document = saved.document;
    globalThis.location = saved.location;
  }
});

test('copilot session probe closes its owned tab on success and sign-out', async () => {
  const fc = fakeChrome(url => new HtmlSite(url, SIDEBAR));
  assert.deepEqual(await sender(fc).session('copilot'), {});
  assert.deepEqual(fc.log.removed, [100]);
  assert.equal(fc.log.created[0].active, false);
  const signedOut = '<html><body><button aria-label="Sign in">Sign in</button></body></html>';
  const failed = fakeChrome(url => new HtmlSite(url, signedOut));
  await assert.rejects(sender(failed).session('copilot'), e => e.code === 'not_logged_in');
  assert.deepEqual(failed.log.removed, [100]);
});

test('copilot send finishes while an idle session probe is still loading', async () => {
  const fc = fakeChrome(url => url.includes('/conversation/')
    ? new FakeSite('copilot', url, { neverFinish: true }) : new HtmlSite(url, SIDEBAR));
  let release, started;
  const blocked = new Promise(resolve => { release = resolve; });
  const entered = new Promise(resolve => { started = resolve; });
  const get = fc.chrome.tabs.get;
  fc.chrome.tabs.get = async id => {
    if (id === 100) { started(); await blocked; }
    return get(id);
  };
  const s = sender(fc);
  const probe = s.session('copilot');
  await entered;
  let timer;
  try {
    await Promise.race([
      s.send('copilot', { message: 'hi', conversation_id: COPILOT_CONV }),
      new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('send waited for probe')), 1000); }),
    ]);
    assert.equal(fc.log.created.length, 2);
    assert.equal(fc.log.removed.includes(100), false, 'probe is still in flight');
  } finally {
    clearTimeout(timer);
    release();
    await probe;
  }
  assert.equal(fc.log.removed.includes(100), true, 'probe closes its own tab');
});
