// Send operations for the Agent Tincan web agents (chatgpt.send,
// claudeai.send).
//
// ChatGPT and claude.ai guard their send endpoints with anti-bot tokens, so
// instead of calling them this module drives the real page UI in a
// background tab the extension opens itself (never one of the user's tabs):
//
//   1. open https://chatgpt.com/ (or /c/<id>) or https://claude.ai/new (or
//      /chat/<id>) with chrome.tabs.create({active: false});
//   2. inject the fixed page functions below with chrome.scripting
//      (isolated world, func + args only) to fill the composer, verify the
//      text landed, and click send;
//   3. probe the page until the reply stops streaming and is stable, with a
//      hard timeout;
//   4. read the conversation id from the tab URL, close the tab, and return
//      it with the reply text as the page shows it. The Go side then reads
//      the conversation through the existing detail and file operations.
//
// The message is data only: it is passed as an argument to a fixed function
// and inserted as text. Nothing from a message or a page is ever executed.
// Selectors drift, so every one lives in SELECTORS, each with fallbacks.

import { OpError } from './ops.js';

export const SEND_TIMEOUT_MS = 5 * 60 * 1000;
export const MAX_REPLY_CHARS = 64 * 1024;

// SELECTORS is the one table of page selectors, tried in order. login and
// loginPaths mean the page is logged out; stop and streaming mean a reply
// is still being written.
export const SELECTORS = Object.freeze({
  chatgpt: Object.freeze({
    composer: ['#prompt-textarea', 'div[contenteditable="true"][id="prompt-textarea"]', 'textarea[data-id="root"]', 'form div[contenteditable="true"]'],
    send: ['[data-testid="send-button"]', '#composer-submit-button', 'button[aria-label*="Send"]'],
    stop: ['[data-testid="stop-button"]', 'button[aria-label*="Stop"]'],
    streaming: ['.result-streaming'],
    assistant: ['[data-message-author-role="assistant"]'],
    user: ['[data-message-author-role="user"]'],
    login: ['[data-testid="login-button"]', 'a[href*="/auth/login"]'],
    loginPaths: ['/auth/login', '/log-in'],
    maxChars: MAX_REPLY_CHARS,
  }),
  claudeai: Object.freeze({
    composer: ['div[contenteditable="true"].ProseMirror', 'fieldset div[contenteditable="true"]', '[contenteditable="true"][aria-label*="prompt" i]', 'div[contenteditable="true"]'],
    send: ['button[aria-label="Send message"]', 'button[aria-label*="Send"]', 'fieldset button[type="submit"]'],
    stop: ['button[aria-label="Stop response"]', 'button[aria-label*="Stop"]'],
    streaming: ['[data-is-streaming="true"]'],
    assistant: ['[data-is-streaming]', '.font-claude-response', '.font-claude-message', '[data-testid="assistant-message"]'],
    user: ['[data-testid="user-message"]'],
    login: ['a[href="/login"]', 'input[type="email"]'],
    loginPaths: ['/login', '/logout'],
    maxChars: MAX_REPLY_CHARS,
  }),
});

// SITES says where each site's pages are and how to read a conversation id
// from a URL.
export const SITES = Object.freeze({
  chatgpt: Object.freeze({
    newURL: 'https://chatgpt.com/',
    convURL: (id) => `https://chatgpt.com/c/${encodeURIComponent(id)}`,
    idFrom: /^https:\/\/chatgpt\.com\/(?:g\/[A-Za-z0-9_-]+\/)?c\/([A-Za-z0-9][A-Za-z0-9_-]{0,127})(?:[/?#]|$)/,
  }),
  claudeai: Object.freeze({
    newURL: 'https://claude.ai/new',
    convURL: (id) => `https://claude.ai/chat/${encodeURIComponent(id)}`,
    idFrom: /^https:\/\/claude\.ai\/chat\/([A-Za-z0-9][A-Za-z0-9_-]{0,127})(?:[/?#]|$)/,
  }),
});

// ---- Page functions. Chrome serializes each one and runs it in the tab's
// isolated world, so they must be self-contained: no closures over module
// state, only their arguments. They return plain data.

export function pageProbe(sel) {
  const q = (list) => {
    for (const s of list) {
      try {
        const el = document.querySelector(s);
        if (el) return el;
      } catch {
        // A selector the page's engine rejects is skipped.
      }
    }
    return null;
  };
  const qa = (list) => {
    for (const s of list) {
      try {
        const els = document.querySelectorAll(s);
        if (els && els.length) return Array.from(els);
      } catch {
        // Skipped, as above.
      }
    }
    return [];
  };
  const textOf = (el) => (el ? String(el.innerText ?? el.textContent ?? '') : '');
  const path = String(location.pathname || '');
  const composer = q(sel.composer);
  const assistant = qa(sel.assistant);
  const last = assistant.length ? assistant[assistant.length - 1] : null;
  const draft = composer ? (composer.tagName === 'TEXTAREA' ? String(composer.value ?? '') : textOf(composer)) : '';
  return {
    href: String(location.href || ''),
    loggedOut: Boolean(q(sel.login)) || sel.loginPaths.some((p) => path === p || path.startsWith(p + '/')),
    composer: Boolean(composer),
    composerEmpty: draft.trim() === '',
    generating: Boolean(q(sel.stop)) || Boolean(q(sel.streaming)),
    assistantCount: assistant.length,
    userCount: qa(sel.user).length,
    lastText: textOf(last).slice(0, sel.maxChars),
  };
}

// pageFill puts message into the composer and checks that it landed. It
// tries typing (execCommand insertText), then a paste event, then setting
// the text directly, clearing the composer between attempts.
export function pageFill(sel, message) {
  const q = (list) => {
    for (const s of list) {
      try {
        const el = document.querySelector(s);
        if (el) return el;
      } catch {
        // Skipped.
      }
    }
    return null;
  };
  const composer = q(sel.composer);
  if (!composer) return { ok: false, code: 'composer_not_found' };
  const isTextarea = composer.tagName === 'TEXTAREA';
  const read = () => (isTextarea ? String(composer.value ?? '') : String(composer.innerText ?? composer.textContent ?? ''));
  // Editors turn blank lines into paragraphs and may eat markdown marks, so
  // the check ignores whitespace and those marks.
  const norm = (s) => String(s).replace(/[\s#*`>_-]+/g, '');
  const landed = () => norm(read()) === norm(message);
  const clear = () => {
    if (read().trim() === '') return;
    if (isTextarea) {
      composer.value = '';
      composer.dispatchEvent(new Event('input', { bubbles: true }));
      return;
    }
    try {
      document.execCommand('selectAll', false);
      document.execCommand('delete', false);
    } catch {
      // Fall through to the direct reset.
    }
    if (read().trim() !== '') {
      composer.textContent = '';
      composer.dispatchEvent(new Event('input', { bubbles: true }));
    }
  };
  const attempts = [
    () => document.execCommand('insertText', false, message),
    () => {
      if (typeof DataTransfer === 'undefined' || typeof ClipboardEvent === 'undefined') return false;
      const dt = new DataTransfer();
      dt.setData('text/plain', message);
      return composer.dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true }));
    },
    () => {
      if (isTextarea) {
        const setter = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(composer), 'value')?.set;
        if (setter) setter.call(composer, message);
        else composer.value = message;
      } else {
        composer.textContent = message;
      }
      return composer.dispatchEvent(new Event('input', { bubbles: true }));
    },
  ];
  const methods = ['insertText', 'paste', 'set'];
  for (let i = 0; i < attempts.length; i++) {
    composer.focus();
    clear();
    try {
      attempts[i]();
    } catch {
      // Try the next method.
    }
    if (landed()) return { ok: true, method: methods[i] };
  }
  clear();
  return { ok: false, code: 'send_failed', message: 'the text did not land in the composer' };
}

// pageSubmit clicks the send button, or presses Enter in the composer when
// no send button exists. A disabled button is reported so the caller can
// retry.
export function pageSubmit(sel) {
  const q = (list) => {
    for (const s of list) {
      try {
        const el = document.querySelector(s);
        if (el) return el;
      } catch {
        // Skipped.
      }
    }
    return null;
  };
  const btn = q(sel.send);
  if (btn) {
    const disabled = btn.disabled === true || (btn.getAttribute && btn.getAttribute('aria-disabled') === 'true');
    if (disabled) return { ok: false, code: 'disabled' };
    btn.click();
    return { ok: true, how: 'button' };
  }
  const composer = q(sel.composer);
  if (!composer) return { ok: false, code: 'composer_not_found' };
  composer.focus();
  composer.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', code: 'Enter', keyCode: 13, which: 13, bubbles: true, cancelable: true }));
  return { ok: true, how: 'enter' };
}

// ---- The sender, run in the service worker.

function str(v, n) {
  return typeof v === 'string' ? v.slice(0, n) : '';
}

// cleanProbe keeps only the expected fields of a page answer, with types
// checked: a page is not trusted to shape the worker's data.
function cleanProbe(r) {
  const o = r && typeof r === 'object' ? r : {};
  return {
    href: str(o.href, 4096),
    loggedOut: o.loggedOut === true,
    composer: o.composer === true,
    composerEmpty: o.composerEmpty === true,
    generating: o.generating === true,
    assistantCount: Number.isSafeInteger(o.assistantCount) ? o.assistantCount : 0,
    userCount: Number.isSafeInteger(o.userCount) ? o.userCount : 0,
    lastText: str(o.lastText, MAX_REPLY_CHARS),
  };
}

// createSender returns {send(site, args)}. tabs and scripting are
// chrome.tabs and chrome.scripting (or fakes). Sends to one site run one
// at a time; each gets its own background tab, closed when done.
export function createSender({
  tabs,
  scripting,
  sleep = (ms) => new Promise((r) => setTimeout(r, ms)),
  now = () => Date.now(),
  timeoutMs = SEND_TIMEOUT_MS,
  pollMs = 1000,
  loadMs = 45000,
  sendConfirmMs = 15000,
  stableProbes = 2,
}) {
  const owned = new Set();
  const queues = {};

  async function inject(tabId, func, args) {
    if (!owned.has(tabId)) throw new OpError('internal', 'refusing to script a tab the extension did not open');
    let res;
    try {
      res = await scripting.executeScript({ target: { tabId }, world: 'ISOLATED', func, args });
    } catch (e) {
      throw new OpError('send_failed', 'could not reach the page: ' + String((e && e.message) || e).slice(0, 200));
    }
    return Array.isArray(res) && res[0] ? res[0].result : undefined;
  }

  async function tabURL(tabId) {
    try {
      const t = await tabs.get(tabId);
      return { url: str(t && t.url, 4096), status: str(t && t.status, 32) };
    } catch {
      throw new OpError('send_failed', 'the worker tab was closed');
    }
  }

  async function run(site, args) {
    const cfg = SITES[site];
    const sel = SELECTORS[site];
    if (!cfg || !sel) throw new OpError('bad_request', 'unknown site');
    const start = now();
    const deadline = start + timeoutMs;
    const late = () => now() >= deadline;
    const target = args.conversation_id && !args.new_chat ? cfg.convURL(args.conversation_id) : cfg.newURL;

    const tab = await tabs.create({ url: target, active: false });
    if (!tab || !Number.isSafeInteger(tab.id)) throw new OpError('send_failed', 'could not open a tab');
    owned.add(tab.id);
    try {
      // 1. Page load, then a composer (or a login page).
      const loadBy = Math.min(deadline, start + loadMs);
      let page = null;
      for (;;) {
        const t = await tabURL(tab.id);
        if (t.status === 'complete') {
          page = cleanProbe(await inject(tab.id, pageProbe, [sel]));
          if (page.loggedOut) throw new OpError('not_logged_in', `logged out of ${new URL(cfg.newURL).host}`);
          if (page.composer) break;
        }
        if (now() >= loadBy) {
          if (late()) throw new OpError('timeout', 'the page did not load in time');
          throw new OpError('composer_not_found', 'no message box on the page');
        }
        await sleep(pollMs);
      }
      if (args.conversation_id && !args.new_chat) {
        const m = cfg.idFrom.exec(page.href);
        if (!m || m[1] !== args.conversation_id) throw new OpError('not_found', 'conversation not found');
      }
      const base = page;

      // 2. Fill and verify.
      const fill = await inject(tab.id, pageFill, [sel, args.message]);
      if (!fill || fill.ok !== true) {
        const code = fill && fill.code === 'composer_not_found' ? 'composer_not_found' : 'send_failed';
        throw new OpError(code, str(fill && fill.message, 200) || 'could not fill the message box');
      }

      // 3. Send: retry while the button is disabled, then confirm the page
      // took the message.
      const confirmBy = Math.min(deadline, now() + sendConfirmMs);
      for (;;) {
        const r = await inject(tab.id, pageSubmit, [sel]);
        if (r && r.ok === true) break;
        if (r && r.code === 'composer_not_found') throw new OpError('composer_not_found', 'the message box went away');
        if (now() >= confirmBy) throw new OpError('send_failed', 'the send button stayed disabled');
        await sleep(pollMs);
      }
      for (;;) {
        await sleep(pollMs);
        const p = cleanProbe(await inject(tab.id, pageProbe, [sel]));
        if (p.loggedOut) throw new OpError('not_logged_in', 'logged out while sending');
        if (p.userCount > base.userCount || p.generating || p.assistantCount > base.assistantCount) break;
        if (now() >= confirmBy) throw new OpError('send_failed', 'the page did not take the message');
      }

      // 4. Wait for the reply to finish: nothing streaming, a new or changed
      // last reply, and the same text on consecutive probes.
      let prev = null;
      let same = 0;
      let last;
      for (;;) {
        if (late()) throw new OpError('timeout', `no finished reply within ${Math.round(timeoutMs / 1000)}s`);
        await sleep(pollMs);
        last = cleanProbe(await inject(tab.id, pageProbe, [sel]));
        if (last.loggedOut) throw new OpError('not_logged_in', 'logged out while waiting for the reply');
        const fresh = last.assistantCount > base.assistantCount || (last.lastText !== base.lastText && last.assistantCount > 0);
        if (last.generating || !fresh || last.lastText.trim() === '') {
          prev = null;
          same = 0;
          continue;
        }
        same = prev === last.lastText ? same + 1 : 0;
        prev = last.lastText;
        if (same >= stableProbes - 1) break;
      }

      // 5. The conversation id, from the tab URL.
      let id = '';
      for (;;) {
        const t = await tabURL(tab.id);
        const m = cfg.idFrom.exec(t.url) || cfg.idFrom.exec(last.href);
        if (m) {
          id = m[1];
          break;
        }
        if (late()) throw new OpError('send_failed', 'the reply finished but the page has no conversation id');
        await sleep(pollMs);
      }
      return { conversation_id: id, url: cfg.convURL(id), reply_text: last.lastText };
    } finally {
      owned.delete(tab.id);
      try {
        await tabs.remove(tab.id);
      } catch {
        // Already closed.
      }
    }
  }

  return {
    // send runs after any earlier send to the same site has finished.
    send(site, args) {
      const prev = queues[site] || Promise.resolve();
      const p = prev.catch(() => {}).then(() => run(site, args));
      queues[site] = p;
      return p;
    },
  };
}
