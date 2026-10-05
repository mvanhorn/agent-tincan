// Send operations for the Agent Tincan web agents (chatgpt.send,
// claudeai.send, grok.send, gemini.send, perplexity.send, copilot.send,
// dots.send) and the matching close operations (chatgpt.close,
// claudeai.close, grok.close, gemini.close, perplexity.close,
// copilot.close, dots.close), plus the
// in-page image capture gemini.file asks for (capture) and the Copilot
// chat list read copilot.list asks for (readList).
//
// ChatGPT, claude.ai and grok.com guard their send endpoints with anti-bot tokens, so
// instead of calling them this module drives the real page UI in a
// background tab the extension opens itself (never one of the user's tabs):
//
//   1. open https://chatgpt.com/ (or /c/<id>), https://claude.ai/new (or
//      /chat/<id>), https://grok.com/ (or /c/<id>) or
//      https://gemini.google.com/app (or /app/<id>) or
//      https://www.perplexity.ai/ (or /search/<slug>) or
//      https://copilot.com/chat (or /chat/conversation/<id>) with
//      chrome.tabs.create({active: false});
//   2. inject the fixed page functions below with chrome.scripting
//      (isolated world, func + args only) to fill the composer, verify the
//      text landed, and click send;
//   3. once the page took the message, wait only for the conversation id in
//      the tab URL (already known when continuing a conversation) and
//      return {conversation_id, url, submitted_at}.
//
// The send does not watch the page for the reply: a background tab is
// throttled, so page-side signals are unreliable. The Go side reads the
// conversation through the existing detail operation until the reply is
// finished, then calls the close operation with the conversation id. The
// tab stays open until then because closing it may stop claude.ai from
// finishing the reply. A tab nobody closes is closed after keepMs anyway.
// A send that fails closes its tab at once.
//
// An OpenAI dot's DM (https://chatgpt.com/dots/<thread>) is the one
// exception to step 3: its address never changes, so the send is
// confirmed by the room feed instead. ops.js hands the sender two hooks:
// ready (optional), called right before the fill, before, called right
// before the click (it notes the feed as it is), and
// confirm, polled after it every feedPollMs for at most feedWaitMs until
// it names the new owner message. A dots send needs its thread id, and
// never types over text already in the composer (keepDraft).
//
// While that tab is still open, capture fetches one of Gemini's images
// from inside it (the isolated world's fetch, with the page's cookies), so
// the image host sees the same request the page's own would make. It never
// draws the page's <img> onto a canvas: a cross-origin image taints it.
//
// readList opens copilot.com/chat in a background tab of its own, reads the
// sidebar's chat links with a fixed isolated-world function (scrolling the
// list for more), and closes the tab. It reads links and titles only,
// never a token or a cookie.
//
// The message is data only: it is passed as an argument to a fixed function
// and inserted as text. Nothing from a message or a page is ever executed.
// Selectors drift, so every one lives in SELECTORS, each with fallbacks.

import { OpError, GEMINI_IMAGE_PREFIX, MAX_FILE_BYTES } from './ops.js';

export const SEND_TIMEOUT_MS = 5 * 60 * 1000;
// ID_WAIT_MS bounds the wait for the conversation id after the send.
export const ID_WAIT_MS = 60 * 1000;
// KEEP_TAB_MS is how long a finished send's tab may stay open waiting for
// its close operation.
export const KEEP_TAB_MS = 10 * 60 * 1000;
// READ_TIMEOUT_MS bounds one Copilot tab read from when it is queued
// (waiting, load and scrolling included), under the Go client's 100 second
// wait for it (TabReadClientTimeout); READ_GAP_MS is the least time between
// the end of one of a site's tab reads and the start of the next.
export const READ_TIMEOUT_MS = 75 * 1000;
export const READ_GAP_MS = 3000;
// FEED_POLL_MS and FEED_WAIT_MS pace and bound a feed-confirmed send's
// wait for its message to show in the room feed.
export const FEED_POLL_MS = 2000;
export const FEED_WAIT_MS = 60 * 1000;

// SELECTORS is the one table of page selectors, tried in order. keepDraft
// means a composer that already holds text is never typed into or
// cleared: the send is refused instead. login and
// loginPaths mean the page is logged out; blocked, where a site has it,
// means the page is an anti-bot check. stop and streaming refuse a send
// into a conversation that is still answering, and with assistant and user
// they help confirm that the page took the message. None of them is ever
// used to decide that a reply is finished. dismiss, where a site has it,
// lists the only buttons a send may click besides send: each is found by
// its exact text inside one of the within containers (pageDismiss).
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
  }),
  // grok.com's composer is a tiptap (ProseMirror) editor inside a form; its
  // submit button appears only once there is text, and Enter submits when
  // it does not. A logged-out grok.com still offers anonymous chat, so the
  // session probe in ops.js runs before any tab opens.
  grok: Object.freeze({
    composer: ['div[contenteditable="true"][aria-label="Ask Grok anything"]', 'form div.ProseMirror[contenteditable="true"]', 'div[contenteditable="true"].ProseMirror', 'textarea[aria-label*="Ask Grok"]'],
    send: ['form button[type="submit"][aria-label="Submit"]', 'button[aria-label="Submit"]', 'form button[type="submit"]'],
    stop: ['button[aria-label="Stop model response"]', 'button[aria-label*="Stop"]'],
    streaming: ['[data-streaming="true"]'],
    assistant: ['div[id^="response-"].items-start'],
    user: ['div[id^="response-"].items-end'],
    login: ['a[href^="/sign-in"]', 'a[href*="accounts.x.ai/sign-in"]', 'a[href*="/sign-up"]'],
    loginPaths: ['/sign-in', '/sign-up'],
    blocked: ['#challenge-form', 'iframe[src*="challenges.cloudflare.com"]', '#cf-challenge-running'],
  }),
  // Gemini's composer is a Quill editor inside rich-textarea; the send
  // button only appears once there is text.
  gemini: Object.freeze({
    composer: ['div.ql-editor[aria-label="Enter a prompt for Gemini"]', 'rich-textarea div.ql-editor[contenteditable="true"]', 'div.ql-editor[contenteditable="true"]'],
    send: ['button[aria-label*="Send" i]', 'button.send-button'],
    stop: ['button[aria-label*="Stop" i]'],
    streaming: [],
    assistant: ['model-response', 'message-content'],
    user: ['user-query'],
    login: ['a[href*="accounts.google.com/ServiceLogin"]', 'a[href*="accounts.google.com/v3/signin"]'],
    loginPaths: [],
  }),
  // Perplexity's composer is the contenteditable #ask-input; text lands
  // through execCommand insertText (synthetic key events do not), and a
  // visible Submit button then appears enabled (both checked live). Enter
  // submits when no button is found. A signed-out page still offers
  // anonymous asks, so the session probe in ops.js runs before any tab
  // opens; login here is the second gate. On load the page can cover the
  // composer with a promo dialog ("Maybe later" closes it; "Get started"
  // is never clicked) and a cookie banner ("Decline optional" keeps
  // optional cookies off).
  perplexity: Object.freeze({
    composer: ['div#ask-input[contenteditable="true"]', '#ask-input[contenteditable="true"]', 'textarea#ask-input'],
    send: ['button[aria-label="Submit"]', 'button[data-testid="submit-button"]', 'button[aria-label*="Submit" i]'],
    stop: ['button[aria-label*="Stop" i]', 'button[data-testid="stop-generating-response-button"]'],
    streaming: [],
    assistant: ['[id^="markdown-content-"]'],
    user: ['[data-testid="user-query"]', 'h1[class*="query"]'],
    login: ['a[href^="/auth/signin"]', 'a[href^="/login"]', 'button[data-testid="login-button"]'],
    loginPaths: ['/auth/signin', '/auth/signup', '/login'],
    blocked: ['#challenge-form', 'iframe[src*="challenges.cloudflare.com"]', '#cf-challenge-running'],
    dismiss: [
      Object.freeze({ text: 'Maybe later', within: ['[role="dialog"]', '[role="alertdialog"]'] }),
      Object.freeze({ text: 'Decline optional', within: ['[role="dialog"]', '[role="alertdialog"]', '[role="region"]', '[aria-label*="cookie" i]', '[id*="cookie" i]', '[class*="cookie" i]', '[id*="consent" i]', '[class*="consent" i]'] }),
    ],
  }),
  // copilot.com (where copilot.microsoft.com sends a personal Microsoft
  // account): the composer is a contenteditable span that takes
  // insertText, and a Send button appears once there is text (Enter is the
  // fallback). A signed-out or unfinished session goes to a Microsoft
  // sign-in or terms page on another host; signedIn must also show before
  // anything is typed, so a page that offers the composer without an
  // account is never used. Its chat list has no JSON the extension may
  // read, so readList reads the sidebar in a tab of its own.
  copilot: Object.freeze({
    composer: ['#m365-chat-editor-target-element', '[role="textbox"][aria-label="Message Copilot"]', 'span[contenteditable="true"][aria-label*="Copilot" i]'],
    send: ['button[aria-label="Send"]', 'button[data-testid="sendButton"]', 'button[aria-label="Submit message"]'],
    stop: ['button[aria-label*="Stop generating" i]', 'button[aria-label="Stop"]'],
    streaming: [],
    assistant: ['[data-testid="markdown-reply"]'],
    user: ['[data-testid="chatQuestion"]'],
    login: ['a[href*="login.live.com"]', 'a[href*="login.microsoftonline.com"]', 'button[aria-label="Sign in"]'],
    loginPaths: ['/signin', '/login'],
    signedIn: ['#m365-copilot-chats-section', '#mectrl_main_trigger', 'button[aria-label*="Account manager" i]'],
    // Copilot's human check is a "Verification required" dialog around a
    // Turnstile widget; it is detected, never touched.
    blocked: ['iframe[src*="challenges.cloudflare.com"]'],
    dialog: ['[role="dialog"]', '[role="alertdialog"]'],
    blockedText: ['Verification required', 'Verify you are human'],
    // read is what readList looks for: the sidebar's chat list and its
    // links.
    read: Object.freeze({
      chats: ['#m365-copilot-chats-section'],
      chat: ['a[href*="/chat/conversation/"]'],
    }),
  }),
  // An OpenAI dot's DM on chatgpt.com: a ProseMirror composer and a Send
  // button (both seen live). The owner may be typing in the DM too, so a
  // composer with text in it is left alone (keepDraft). The page has no
  // answering marks; the reply is read from the room feed by the Go side.
  dots: Object.freeze({
    composer: ['div[role="textbox"][aria-label="Message"]', 'div.ProseMirror[contenteditable="true"][aria-label="Message"]'],
    send: ['button[aria-label="Send"]'],
    stop: [],
    streaming: [],
    assistant: [],
    user: [],
    login: ['[data-testid="login-button"]', 'a[href*="/auth/login"]'],
    loginPaths: ['/auth/login', '/log-in'],
    keepDraft: true,
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
  grok: Object.freeze({
    newURL: 'https://grok.com/',
    convURL: (id) => `https://grok.com/c/${encodeURIComponent(id)}`,
    idFrom: /^https:\/\/grok\.com\/c\/([A-Za-z0-9][A-Za-z0-9_-]{0,127})(?:[/?#]|$)/,
  }),
  // A Gemini id is the hex in /app/<id>; a Gem's chat shows it as
  // /gem/<name>/<id>, another signed-in account under /u/<n>/.
  gemini: Object.freeze({
    newURL: 'https://gemini.google.com/app',
    convURL: (id) => `https://gemini.google.com/app/${encodeURIComponent(id)}`,
    idFrom: /^https:\/\/gemini\.google\.com\/(?:u\/\d{1,2}\/)?(?:app|gem\/[A-Za-z0-9_-]{1,128})\/([0-9a-f]{8,64})(?:[/?#]|$)/,
  }),
  // A Perplexity thread is /search/<slug>; new threads' slugs are
  // uuid-shaped.
  perplexity: Object.freeze({
    newURL: 'https://www.perplexity.ai/',
    convURL: (id) => `https://www.perplexity.ai/search/${encodeURIComponent(id)}`,
    idFrom: /^https:\/\/www\.perplexity\.ai\/search\/([A-Za-z0-9][A-Za-z0-9_-]{0,127})(?:[/?#]|$)/,
  }),
  // A Copilot id is the conversation's UUID. hiddenIsAway: a loaded tab
  // whose address Chrome hides is on a host the extension has no access
  // to (a Microsoft sign-in or terms page), so it is off the site.
  // workHosts are where a work or school account lands.
  copilot: Object.freeze({
    newURL: 'https://copilot.com/chat',
    convURL: (id) => `https://copilot.com/chat/conversation/${encodeURIComponent(id)}`,
    idFrom: /^https:\/\/copilot\.com\/chat\/conversation\/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})(?:[/?#]|$)/,
    hiddenIsAway: true,
    workHosts: Object.freeze(['cloud.microsoft', 'office.com', 'microsoft365.com']),
  }),
  // A dot has one DM, its thread: there is no new chat, so a send needs
  // the thread id (needsConversation), and newURL only names the host.
  // The address never changes after a send, so the room feed confirms it
  // (confirmByFeed).
  dots: Object.freeze({
    newURL: 'https://chatgpt.com/',
    convURL: (id) => `https://chatgpt.com/dots/${encodeURIComponent(id)}`,
    idFrom: /^https:\/\/chatgpt\.com\/dots\/([A-Za-z0-9][A-Za-z0-9_-]{0,127})(?:[/?#]|$)/,
    needsConversation: true,
    confirmByFeed: true,
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
  const draft = composer ? (composer.tagName === 'TEXTAREA' ? String(composer.value ?? '') : textOf(composer)) : '';
  return {
    href: String(location.href || ''),
    loggedOut: Boolean(q(sel.login)) || sel.loginPaths.some((p) => path === p || path.startsWith(p + '/')),
    signedIn: !Array.isArray(sel.signedIn) || Boolean(q(sel.signedIn)),
    blocked:
      (Array.isArray(sel.blocked) && (Boolean(q(sel.blocked)) || /^just a moment/i.test(String(document.title || '')))) ||
      (Array.isArray(sel.blockedText) && qa(sel.dialog || []).some((d) => sel.blockedText.some((t) => textOf(d).includes(t)))),
    composer: Boolean(composer),
    composerEmpty: draft.trim() === '',
    generating: Boolean(q(sel.stop)) || Boolean(q(sel.streaming)),
    assistantCount: qa(sel.assistant).length,
    userCount: qa(sel.user).length,
  };
}

// pageDismiss closes the dialogs a site can put over its composer on load.
// For each of sel.dismiss it clicks at most one enabled button whose text
// is exactly rule.text inside one of rule.within; nothing else is clicked.
// It returns the texts it clicked.
export function pageDismiss(sel) {
  const rules = Array.isArray(sel.dismiss) ? sel.dismiss : [];
  const clicked = [];
  for (const rule of rules) {
    const scopes = [];
    for (const s of rule.within) {
      try {
        scopes.push(...document.querySelectorAll(s));
      } catch {
        // Skipped.
      }
    }
    let btn = null;
    for (const scope of scopes) {
      let btns = [];
      try {
        btns = Array.from(scope.querySelectorAll('button'));
      } catch {
        // Skipped.
      }
      btn = btns.find((b) => String(b.innerText ?? b.textContent ?? '').trim() === rule.text && b.disabled !== true) || null;
      if (btn) break;
    }
    if (btn) {
      btn.click();
      clicked.push(rule.text);
    }
  }
  return { clicked };
}

// Candidate ChatGPT file flow. These fixed selectors and readiness signals
// require live acceptance before IMAGE_INPUT_SITES may include chatgpt.
// The returned state never contains page URLs, tokens or user draft text.
export function pageInputImages(files, attach = false) {
  const composer = document.querySelector('#prompt-textarea');
  const form = composer && composer.closest('form');
  const input = form && form.querySelector('input[type="file"]');
  if (!form || !input) return { ok: false, error: 'image file control not found' };
  const previews = [...form.querySelectorAll('img')];
  if (attach) {
    if ((composer.innerText || composer.value || '').trim() || input.files?.length || previews.length) return { ok: false, error: 'existing text or image draft was left untouched' };
    const transfer = new DataTransfer();
    for (const f of files) {
      const bytes = Uint8Array.from(atob(f.data), (c) => c.charCodeAt(0));
      transfer.items.add(new File([bytes], f.name, { type: f.mime }));
    }
    input.files = transfer.files;
    input.dispatchEvent(new Event('change', { bubbles: true }));
    return { ok: true, ready: false };
  }
  if (form.querySelector('[role="alert"]')) return { ok: false, error: 'the composer reported an upload error' };
  // FileList assignment alone is never readiness. Require a loaded preview
  // for every filename, no upload progress and an enabled send button.
  const button = form.querySelector('button[data-testid="send-button"]');
  const ready = previews.length === files.length && files.every((f, i) => {
    const img = previews[i];
    return img.complete && img.naturalWidth > 0 && (img.alt === f.name || img.closest('[title]')?.getAttribute('title') === f.name);
  }) && !form.querySelector('[role="progressbar"], [aria-busy="true"]') && button && !button.disabled;
  return { ok: true, ready: !!ready };
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
  if (sel.keepDraft === true && read().trim() !== '') return { ok: false, code: 'composer_busy', message: 'the message box already has text' };
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
// no send button exists. A visible button is preferred over a hidden one
// that matches first. A disabled button is reported so the caller can
// retry.
export function pageSubmit(sel, inputNames = null, message = null) {
  if (inputNames) {
    const composer = document.querySelector('#prompt-textarea');
    const form = composer?.closest('form');
    const previews = form ? [...form.querySelectorAll('img')] : [];
    const button = form?.querySelector('button[data-testid="send-button"]');
    const ready = previews.length === inputNames.length && inputNames.every((name, i) => {
      const img = previews[i];
      return img.complete && img.naturalWidth > 0 && (img.alt === name || img.closest('[title]')?.getAttribute('title') === name);
    });
    if (!ready || !button || button.disabled || button.getAttribute('aria-disabled') === 'true' || form.querySelector('[role="alert"], [role="progressbar"], [aria-busy="true"]') || (composer.innerText || composer.value || '').trim() !== message.trim()) return { ok: false, code: 'disabled' };
    button.click();
    return { ok: true, how: 'button' };
  }
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
  const shown = (el) => {
    if (typeof el.checkVisibility === 'function') return el.checkVisibility() !== false;
    if (typeof el.getClientRects === 'function') return el.getClientRects().length > 0;
    return true;
  };
  const pick = () => {
    let first = null;
    for (const s of sel.send) {
      let els = [];
      try {
        els = Array.from(document.querySelectorAll(s));
      } catch {
        // Skipped.
      }
      for (const el of els) {
        if (!first) first = el;
        if (shown(el)) return el;
      }
    }
    return first;
  };
  const btn = pick();
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

// pageFetchImage fetches url with the page's cookies and returns it as
// base64, or why it could not. Only an image under maxBytes is returned:
// a declared size over it is refused unread, and the body is read a chunk
// at a time and cancelled as soon as it passes maxBytes, so a large answer
// never sits whole in the tab's memory. It runs in the page, so it uses
// nothing from this module.
export async function pageFetchImage(url, maxBytes) {
  try {
    const res = await fetch(url, { credentials: 'include', cache: 'no-store' });
    if (!res.ok) return { ok: false, status: res.status };
    const mime = String(res.headers.get('content-type') || '').split(';')[0].trim().toLowerCase();
    if (!mime.startsWith('image/')) return { ok: false, code: 'not_image' };
    if (!res.body) return { ok: false, code: 'size' };
    const reader = res.body.getReader();
    const tooBig = async () => {
      try {
        await reader.cancel();
      } catch {
        // Already closed.
      }
      return { ok: false, code: 'size' };
    };
    if (Number(res.headers.get('content-length') || 0) > maxBytes) return tooBig();
    const parts = [];
    let total = 0;
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      total += value.length;
      if (total > maxBytes) return tooBig();
      parts.push(value);
    }
    if (total === 0) return { ok: false, code: 'size' };
    const buf = new Uint8Array(total);
    for (let off = 0, i = 0; i < parts.length; off += parts[i].length, i++) buf.set(parts[i], off);
    let bin = '';
    for (let i = 0; i < buf.length; i += 0x8000) bin += String.fromCharCode.apply(null, buf.subarray(i, i + 0x8000));
    return { ok: true, mime, data: btoa(bin) };
  } catch {
    return { ok: false, code: 'network' };
  }
}

// pageCopilotList reads the sidebar's chat list: each link to
// /chat/conversation/<id>, with its title (the link's aria-label, else its
// text). found is false while the list is not on the page. With scroll set
// it then scrolls the last link into view, so more chats load for the
// next read.
export function pageCopilotList(sel, scroll) {
  let box = null;
  for (const s of sel.chats) {
    try {
      box = document.querySelector(s);
    } catch {
      box = null;
    }
    if (box) break;
  }
  if (!box) return { found: false, conversations: [] };
  let links = [];
  for (const s of sel.chat) {
    try {
      links = Array.from(box.querySelectorAll(s));
    } catch {
      links = [];
    }
    if (links.length) break;
  }
  const conversations = [];
  const seen = new Set();
  for (const a of links) {
    let path = '';
    try {
      const u = new URL(a.getAttribute('href') || '', location.href);
      if (u.host === location.host) path = u.pathname;
    } catch {
      continue;
    }
    const m = /^\/chat\/conversation\/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\/?$/.exec(path);
    if (!m || seen.has(m[1])) continue;
    seen.add(m[1]);
    const title = String(a.getAttribute('aria-label') || a.textContent || '').replace(/\s+/g, ' ').trim();
    conversations.push({ id: m[1], title });
  }
  if (scroll && links.length && typeof links[links.length - 1].scrollIntoView === 'function') {
    links[links.length - 1].scrollIntoView({ block: 'end' });
  }
  return { found: true, conversations };
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
    signedIn: o.signedIn === true,
    blocked: o.blocked === true,
    composer: o.composer === true,
    composerEmpty: o.composerEmpty === true,
    generating: o.generating === true,
    assistantCount: Number.isSafeInteger(o.assistantCount) ? o.assistantCount : 0,
    userCount: Number.isSafeInteger(o.userCount) ? o.userCount : 0,
  };
}

// elsewhere returns url as a URL when it is an http(s) address on another
// host than site's, else null (about:blank and the like while loading).
function elsewhere(url, site) {
  let u;
  try {
    u = new URL(url);
  } catch {
    return null;
  }
  if (u.protocol !== 'https:' && u.protocol !== 'http:') return null;
  return u.host === new URL(site).host ? null : u;
}

// awayError is the error for a tab the site sent off its host, or null
// while it is on it (or still loading): Google's /sorry/ anti-bot page is
// blocked, a work or school account's landing (cfg.workHosts) and any
// other host (a sign-in or terms page) not_logged_in. On a site with
// hiddenIsAway, a loaded tab whose address Chrome hides counts as another
// host too: the extension can see the address of its granted hosts only.
function awayError(t, cfg) {
  const host = new URL(cfg.newURL).host;
  const away = elsewhere(t.url, cfg.newURL);
  if (away) {
    if (away.pathname.startsWith('/sorry/')) return new OpError('blocked', `the page went to ${away.host}`);
    const work = (cfg.workHosts || []).some((h) => away.hostname === h || away.hostname.endsWith('.' + h));
    if (work) return new OpError('not_logged_in', `work or school account: the page went to ${away.host}`);
    return new OpError('not_logged_in', `the page went to ${away.host}`);
  }
  if (cfg.hiddenIsAway && t.url === '' && t.status === 'complete') {
    return new OpError('not_logged_in', `the page left ${host} for a sign-in or terms page`);
  }
  return null;
}

// cleanList keeps a Copilot sidebar read's conversations.
function cleanList(r) {
  const o = r && typeof r === 'object' ? r : {};
  const out = [];
  for (const c of Array.isArray(o.conversations) ? o.conversations.slice(0, 1000) : []) {
    const id = str(c && c.id, 64);
    if (/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(id)) out.push({ id, title: str(c.title, 300) });
  }
  return { found: o.found === true, conversations: out };
}

// createSender returns {send(site, args, hooks), close(site, conversationId),
// capture(site, conversationId, url), busy(), closeAllKept()}.
// tabs and scripting are chrome.tabs and chrome.scripting (or fakes). Sends
// to one site run one at a time, each in its own background tab. A
// successful send leaves its tab open for close (or the keepMs timer).
export function createSender({
  tabs,
  scripting,
  sleep = (ms) => new Promise((r) => setTimeout(r, ms)),
  now = () => Date.now(),
  setTimer = (fn, ms) => setTimeout(fn, ms),
  clearTimer = (t) => clearTimeout(t),
  timeoutMs = SEND_TIMEOUT_MS,
  pollMs = 1000,
  loadMs = 45000,
  sendConfirmMs = 15000,
  idWaitMs = ID_WAIT_MS,
  keepMs = KEEP_TAB_MS,
  settleMs = 1500,
  readMs = READ_TIMEOUT_MS,
  readGapMs = READ_GAP_MS,
  feedPollMs = FEED_POLL_MS,
  feedWaitMs = FEED_WAIT_MS,
  listRounds = 10,
  listWaitMs = 10000,
}) {
  // owned: tabs being driven by a send, the only ones that may be
  // scripted. kept: finished sends' tabs, tab id -> {site, id, timer},
  // the only ones close may remove.
  const owned = new Set();
  const kept = new Map();
  const queues = {};
  // lastRead is when each site's last tab read ended.
  const lastRead = {};
  // inflight counts sends and tab reads accepted and not yet settled,
  // queued ones too.
  let inflight = 0;

  // inject runs func in tabId's isolated world. Only a tab a send is
  // driving may be scripted, or, with keptOK, a finished send's tab.
  async function inject(tabId, func, args, keptOK = false) {
    if (!owned.has(tabId) && !(keptOK && kept.has(tabId))) throw new OpError('internal', 'refusing to script a tab the extension did not open');
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

  async function removeTab(tabId) {
    try {
      await tabs.remove(tabId);
    } catch {
      // Already closed.
    }
  }

  function keep(tabId, site, id) {
    const timer = setTimer(() => {
      if (kept.get(tabId)?.timer !== timer) return;
      kept.delete(tabId);
      removeTab(tabId);
    }, keepMs);
    kept.set(tabId, { site, id, timer });
  }

  async function closeKept(match) {
    let closed = 0;
    for (const [tabId, k] of [...kept]) {
      if (!match(k)) continue;
      kept.delete(tabId);
      clearTimer(k.timer);
      await removeTab(tabId);
      closed++;
    }
    return { closed };
  }

  // onSite reads tabId's address and fails when the site sent the tab to
  // another host (awayError). Nothing is typed or read there. Every poll
  // after the page loaded goes through it, so a redirect in the middle of
  // a send or a read stops it too.
  async function onSite(tabId, cfg) {
    const t = await tabURL(tabId);
    const err = awayError(t, cfg);
    if (err) throw err;
    return t;
  }

  // load waits for tabId's page to load and be usable: signed in (on a
  // site with signedIn selectors, their mark is on the page), not an
  // anti-bot check, and ready(page). A page that shows its login marks is
  // not_logged_in at once; one that never shows the signed-in mark is
  // not_logged_in when loadBy passes; one that is never ready fails with
  // notReady().
  async function load(tabId, cfg, sel, { loadBy, late, ready, notReady }) {
    const host = new URL(cfg.newURL).host;
    // An anti-bot interstitial can be transient (a challenge that passes
    // by itself and reloads the page), so a blocked page is polled like a
    // loading one and is an error only if it is still blocked when the
    // load time runs out.
    let page = null;
    let blocked = false;
    for (;;) {
      // While loading, a tab on another host gets one short settle before
      // it fails: a logged-in Google session can pass through
      // accounts.google.com and come straight back.
      if (awayError(await tabURL(tabId), cfg)) await sleep(settleMs);
      const t = await onSite(tabId, cfg);
      blocked = false;
      if (t.status === 'complete') {
        page = cleanProbe(await inject(tabId, pageProbe, [sel]));
        blocked = page.blocked;
        if (!blocked && page.loggedOut) throw new OpError('not_logged_in', `logged out of ${host}`);
        if (!blocked && page.signedIn && ready(page)) return page;
      }
      if (now() >= loadBy) {
        if (blocked) throw new OpError('blocked', `anti-bot check on ${host}`);
        if (page && !page.signedIn) throw new OpError('not_logged_in', `no signed-in account on ${host}`);
        if (late()) throw new OpError('timeout', 'the page did not load in time');
        throw notReady();
      }
      await sleep(pollMs);
    }
  }

  // run drives one send. timeoutMs counts from accepted, when the send
  // was queued, so a send that waited behind others to the same site
  // still ends inside the host's bound; one with no time left opens no
  // tab, so it cannot go out after the host has reported it failed.
  async function run(site, args, accepted, hooks) {
    const cfg = SITES[site];
    const images = hooks?.images;
    const inputActive = () => { if (images && hooks.active && !hooks.active()) throw new OpError('send_failed', 'image transfer disconnected'); };
    inputActive();
    const sel = images ? { ...SELECTORS[site], keepDraft: true } : SELECTORS[site];
    if (images && (site !== 'chatgpt' || typeof hooks.confirmImages !== 'function')) throw new OpError('unsupported', 'image submission confirmation is unavailable');
    if (!cfg || !sel) throw new OpError('bad_request', 'unknown site');
    const start = now();
    const deadline = (accepted ?? start) + timeoutMs;
    const late = () => now() >= deadline;
    if (late()) throw new OpError('timeout', 'the send waited too long behind other requests to the site');
    const existing = args.conversation_id && !args.new_chat ? args.conversation_id : '';
    if (cfg.needsConversation && !existing) throw new OpError('bad_request', 'this site has no new chat; the send needs its conversation id');
    const feed = cfg.confirmByFeed === true;
    if (feed && !(hooks && typeof hooks.before === 'function' && typeof hooks.confirm === 'function')) {
      throw new OpError('internal', 'a feed-confirmed send needs its feed hooks');
    }
    const target = existing ? cfg.convURL(existing) : cfg.newURL;

    const tab = await tabs.create({ url: target, active: false });
    if (!tab || !Number.isSafeInteger(tab.id)) throw new OpError('send_failed', 'could not open a tab');
    owned.add(tab.id);
    // urlId is the conversation id in the tab's address right now, '' when
    // there is none; it checks the host first (onSite).
    const urlId = async () => {
      const m = cfg.idFrom.exec((await onSite(tab.id, cfg)).url);
      return m ? m[1] : '';
    };
    let done = false;
    // clicked: the page took a click on the send button. Any failure after
    // it (a redirect to a sign-in page or /sorry/, no confirmation, no id)
    // is marked clicked: the message may have been sent, and sending it
    // again could post it twice.
    let clicked = false;
    let uploaded = false;
    try {
      // 1. Page load, then a composer (or a login page).
      const blockedErr = () => new OpError('blocked', `anti-bot check on ${new URL(cfg.newURL).host}`);
      const page = await load(tab.id, cfg, sel, {
        loadBy: Math.min(deadline, start + loadMs),
        late,
        ready: (p) => p.composer,
        notReady: () => new OpError('composer_not_found', 'no message box on the page'),
      });
      if (existing) {
        const m = cfg.idFrom.exec(page.href);
        if (!m || m[1] !== existing) throw new OpError('not_found', 'conversation not found');
      }
      // A conversation still answering an earlier message will not take
      // another (the site trades its send button for a stop button), and
      // the answering itself would look like the page taking this one. So
      // it is refused before anything is typed, and again right before the
      // click, in case an answer started meanwhile.
      const answering = () => new OpError('send_failed', 'the conversation is still answering an earlier message');
      if (page.generating) throw answering();
      // Text already in the composer (the owner typing) is never typed
      // over or cleared; pageFill checks again right before it types.
      const busy = () => new OpError('send_failed', 'the message box already has text; it was left as it is');
      if (sel.keepDraft === true && !page.composerEmpty) throw busy();

      // 2. Close any startup dialog over the composer (a bounded number of
      // passes, fixed buttons only), then fill and verify.
      if (Array.isArray(sel.dismiss)) {
        for (let i = 0; i < 3; i++) {
          const d = await inject(tab.id, pageDismiss, [sel]);
          if (!d || !Array.isArray(d.clicked) || d.clicked.length === 0) break;
          await sleep(pollMs);
          await onSite(tab.id, cfg);
        }
      }
      // A feed-confirmed site's ready hook runs its last checks (a dot
      // paused while this send waited) before anything is typed.
      if (feed && typeof hooks.ready === 'function') await hooks.ready();
      if (images) {
        inputActive();
        // Mark possible exposure before injection, since a lost injection
        // response cannot establish that change was never dispatched.
        uploaded = true;
        const attached = await inject(tab.id, pageInputImages, [images, true]);
        if (!attached?.ok) throw new OpError('send_failed', attached?.error || 'image upload failed');
        for (;;) {
          await onSite(tab.id, cfg);
          const p = cleanProbe(await inject(tab.id, pageProbe, [sel]));
          if (p.loggedOut) throw new OpError('not_logged_in', 'logged out during image upload');
          if (p.blocked) throw blockedErr();
          inputActive();
          const state = await inject(tab.id, pageInputImages, [images, false]);
          if (!state?.ok) throw new OpError('send_failed', state?.error || 'image upload failed');
          if (state.ready) break;
          if (late()) throw new OpError('timeout', 'image upload readiness was not confirmed');
          await sleep(pollMs);
        }
      }
      const fill = await inject(tab.id, pageFill, [sel, args.message]);
      if (fill && fill.code === 'composer_busy') throw busy();
      if (!fill || fill.ok !== true) {
        const code = fill && fill.code === 'composer_not_found' ? 'composer_not_found' : 'send_failed';
        throw new OpError(code, str(fill && fill.message, 200) || 'could not fill the message box');
      }

      // 3. Send: retry while the button is disabled, then confirm the page
      // took the message. The page is probed right before each click, so
      // the confirmation compares with the page as it was then, not as it
      // was when the tab opened. An existing conversation's address is
      // checked then too: a deleted conversation shows its composer first
      // and redirects a moment later, and the message must not go into
      // whatever page is there by then. A new chat's URL gaining an id
      // also confirms the send.
      const confirmBy = Math.min(deadline, now() + sendConfirmMs);
      let submittedAt;
      let base;
      for (;;) {
        const cur = await urlId();
        if (existing && cur !== existing) throw new OpError('not_found', 'conversation not found');
        base = cleanProbe(await inject(tab.id, pageProbe, [sel]));
        if (base.loggedOut) throw new OpError('not_logged_in', 'logged out while sending');
        if (base.blocked) throw blockedErr();
        if (base.generating) throw answering();
        if (feed) await hooks.before();
        if (images) {
          inputActive();
          const state = await inject(tab.id, pageInputImages, [images, false]);
          if (!state?.ok || !state.ready) throw new OpError('send_failed', 'image readiness was lost before submission');
        }
        submittedAt = now();
        // Image sends treat any lost click response as uncertain.
        inputActive();
        if (images) clicked = true;
        const r = await inject(tab.id, pageSubmit, images ? [sel, images.map((f) => f.name), args.message] : [sel]);
        if (images && r?.ok !== true) {
          clicked = false;
          throw new OpError('send_failed', 'image send button was not ready');
        }
        if (r && r.ok === true) {
          clicked = true;
          break;
        }
        if (r && r.code === 'composer_not_found') throw new OpError('composer_not_found', 'the message box went away');
        if (now() >= confirmBy) throw new OpError('send_failed', 'the send button stayed disabled');
        await sleep(pollMs);
      }
      // 3b. A feed-confirmed send waits for its message in the room feed:
      // the page's address says nothing. The tab stays on the site
      // meanwhile, and nothing is typed or clicked again.
      if (feed) {
        const feedBy = Math.min(deadline, now() + feedWaitMs);
        for (;;) {
          await sleep(feedPollMs);
          await onSite(tab.id, cfg);
          const messageId = await hooks.confirm();
          if (typeof messageId === 'string' && messageId !== '') {
            done = true;
            keep(tab.id, site, existing);
            return { conversation_id: existing, url: cfg.convURL(existing), submitted_at: submittedAt, message_id: messageId };
          }
          if (now() >= feedBy) throw new OpError('timeout', 'the message was sent but did not show in the feed in time');
        }
      }
      for (;;) {
        await sleep(pollMs);
        const cur = await urlId();
        if (!existing && cur) break;
        const p = cleanProbe(await inject(tab.id, pageProbe, [sel]));
        if (p.loggedOut) throw new OpError('not_logged_in', 'logged out while sending');
        if (p.blocked) throw blockedErr();
        if (p.userCount > base.userCount || p.generating || p.assistantCount > base.assistantCount) break;
        if (now() >= confirmBy) throw new OpError('send_failed', 'the page did not take the message');
      }

      // 4. The conversation id, from the tab URL as it is now: a new chat's
      // once it appears, or wherever the site put the message if it moved
      // an existing conversation after the click. Nothing waits for the
      // reply.
      let id = await urlId();
      const idBy = Math.min(deadline, now() + idWaitMs);
      while (!id) {
        if (now() >= idBy) throw new OpError('timeout', 'the message was sent but no conversation id appeared in time');
        await sleep(pollMs);
        id = await urlId();
      }
      let messageId;
      if (images) {
        for (;;) {
          await onSite(tab.id, cfg);
          inputActive();
          messageId = await hooks.confirmImages(id, submittedAt);
          if (messageId) break;
          if (late()) throw new OpError('send_failed', 'submitted image evidence was not confirmed; do not retry automatically');
          await sleep(pollMs);
        }
      }
      done = true;
      keep(tab.id, site, id);
      return { conversation_id: id, url: cfg.convURL(id), submitted_at: submittedAt, ...(images ? { message_id: messageId, input_count: images.length } : {}) };
    } catch (e) {
      if (images && !(e instanceof OpError)) e = new OpError('send_failed', 'image send failed');
      if (clicked && e instanceof OpError) e.clicked = true;
      if (uploaded && e instanceof OpError) e.uploaded = true;
      throw e;
    } finally {
      owned.delete(tab.id);
      if (!done) await removeTab(tab.id);
    }
  }

  // enqueue runs fn after everything queued for site before it has
  // settled: a site's sends and tab reads run one at a time.
  function enqueue(site, fn) {
    const prev = queues[site] || Promise.resolve();
    inflight++;
    const p = prev
      .catch(() => {})
      .then(fn)
      .finally(() => {
        inflight--;
      });
    queues[site] = p;
    return p;
  }

  // readTab opens url in a background tab of the extension's own, waits
  // for the page to load signed in, then calls step(tabId, late) once a
  // poll until it returns {result}, and closes the tab. Each read starts
  // at least readGapMs after the site's last one ended, so reads keep a
  // human pace. readMs counts from accepted, when the read was queued, so
  // time spent behind a send counts against it and the answer still
  // comes before the Go client's wait ends; a read with no time left
  // opens no tab. The page is checked for the site's human check every
  // poll: one that shows while the list is read is blocked, not a short
  // list.
  async function readTab(site, url, step, accepted) {
    const cfg = SITES[site];
    const sel = SELECTORS[site];
    const deadline = accepted + readMs;
    const late = () => now() >= deadline;
    const wait = (lastRead[site] ?? -Infinity) + readGapMs - now();
    if (wait > 0) await sleep(wait);
    if (late()) throw new OpError('timeout', 'the read waited too long behind other requests to the site');
    const start = now();
    const tab = await tabs.create({ url, active: false });
    if (!tab || !Number.isSafeInteger(tab.id)) throw new OpError('timeout', 'could not open a tab');
    owned.add(tab.id);
    try {
      await load(tab.id, cfg, sel, {
        loadBy: Math.min(deadline, start + loadMs),
        late,
        ready: () => true,
        notReady: () => new OpError('timeout', 'the page did not load in time'),
      });
      for (;;) {
        await onSite(tab.id, cfg);
        const p = cleanProbe(await inject(tab.id, pageProbe, [sel]));
        if (p.blocked) throw new OpError('blocked', `anti-bot check on ${new URL(cfg.newURL).host}`);
        if (p.loggedOut) throw new OpError('not_logged_in', `logged out of ${new URL(cfg.newURL).host}`);
        const r = await step(tab.id, late);
        if (r) return r.result;
        await sleep(pollMs);
      }
    } finally {
      owned.delete(tab.id);
      lastRead[site] = now();
      await removeTab(tab.id);
    }
  }

  // readList reads up to count conversations from the sidebar's chat
  // list on copilot.com/chat, scrolling it to load more until it has count,
  // nothing more loads, or listRounds. more is set when it stopped at
  // listRounds or the time limit while the list was still growing. A
  // signed-in page with no chat list after listWaitMs has no chats.
  async function readList(site, count) {
    const sel = SELECTORS[site];
    const cfg = SITES[site];
    const accepted = now();
    return enqueue(site, () => {
      const seen = new Map();
      let still = 0;
      let rounds = 0;
      let since = -1;
      const step = async (tabId, late) => {
        const r = cleanList(await inject(tabId, pageCopilotList, [sel.read, true]));
        if (!r.found) {
          if (since < 0) since = now();
          if (now() - since >= listWaitMs || late()) return { result: { conversations: [] } };
          return null;
        }
        const before = seen.size;
        for (const c of r.conversations) if (!seen.has(c.id)) seen.set(c.id, c);
        rounds++;
        still = seen.size === before ? still + 1 : 0;
        const list = [...seen.values()];
        if (list.length >= count || still >= 2) return { result: { conversations: list.slice(0, count) } };
        if (rounds >= listRounds || late()) return { result: { conversations: list, more: true } };
        return null;
      };
      return readTab(site, cfg.newURL, step, accepted);
    });
  }

  return {
    // send runs after any earlier send to the same site has finished.
    // hooks ({before, confirm}) are for a feed-confirmed site (dots).
    send(site, args, hooks) {
      const accepted = now();
      return enqueue(site, () => run(site, args, accepted, hooks));
    },
    // A probe uses the same signed-in gate and owned-tab cleanup as a read.
    session(site) {
      if (site !== 'copilot') return Promise.reject(new OpError('bad_request', 'unknown site'));
      const accepted = now();
      // Probes own a separate tab and must not queue an incoming send.
      return readTab(site, SITES[site].newURL, async () => ({ result: {} }), accepted);
    },
    // readList reads Copilot's chat list from its rendered sidebar (see
    // above).
    readList(site, count) {
      if (site !== 'copilot') return Promise.reject(new OpError('bad_request', 'unknown site'));
      return readList(site, count);
    },
    // close closes the tabs a finished send to conversationId left open,
    // and only those. It reports how many it closed.
    async close(site, conversationId) {
      return closeKept((k) => k.site === site && k.id === conversationId);
    },
    // capture fetches an image from inside the tab a finished send to
    // conversationId left open (pageFetchImage), for the image host that
    // expects the page's own request. It returns the page's answer
    // ({ok, mime, data}) or null when there is no such tab or the page
    // could not fetch it; it never throws. Only Gemini image URLs are
    // fetched.
    async capture(site, conversationId, url, maxBytes = MAX_FILE_BYTES) {
      if (typeof url !== 'string' || !url.startsWith(GEMINI_IMAGE_PREFIX)) return null;
      const entry = [...kept].find(([, k]) => k.site === site && k.id === conversationId);
      if (!entry) return null;
      try {
        const r = await inject(entry[0], pageFetchImage, [url, maxBytes], true);
        return r && r.ok === true ? r : null;
      } catch {
        return null;
      }
    },
    // busy reports whether a send is queued or driving a tab, or a
    // finished send's tab is still waiting for its close: state a reload
    // would lose.
    busy() {
      return inflight > 0 || owned.size > 0 || kept.size > 0;
    },
    // closeAllKept closes every tab finished sends left open (used before
    // a forced reload, which would otherwise orphan them).
    async closeAllKept() {
      return closeKept(() => true);
    },
  };
}
