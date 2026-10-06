import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { SITE_ACCESS } from '../ops.js';
import { renderOptions, siteStates } from '../options.js';

// A minimal DOM: elements with children, text, attributes and click
// listeners. The page builds everything with createElement and
// textContent, so this is all it needs.
class FakeElement {
  constructor(tag) {
    this.tagName = tag.toUpperCase();
    this.children = [];
    this.textContent = '';
    this.className = '';
    this.disabled = false;
    this.hidden = false;
    this.attrs = {};
    this.listeners = {};
  }
  append(...els) {
    this.children.push(...els);
  }
  replaceChildren(...els) {
    this.children = [...els];
  }
  setAttribute(k, v) {
    this.attrs[k] = String(v);
  }
  addEventListener(type, fn) {
    (this.listeners[type] ||= []).push(fn);
  }
  click() {
    for (const fn of this.listeners.click || []) fn({ type: 'click' });
  }
  all(pred, out = []) {
    for (const c of this.children) {
      if (pred(c)) out.push(c);
      c.all(pred, out);
    }
    return out;
  }
}

function fakeDocument() {
  const root = new FakeElement('ul');
  return {
    root,
    createElement: (tag) => new FakeElement(tag),
    getElementById: (id) => (id === 'sites' ? root : null),
  };
}

function fakePermissions(granted) {
  const listeners = { added: [], removed: [] };
  const p = {
    granted: new Set(granted),
    requests: [],
    // inGesture is true only while a click handler runs: Chrome refuses a
    // request made after the handler returned (after an await).
    inGesture: false,
    answer: true,
    async contains({ origins }) {
      return origins.every((o) => p.granted.has(o));
    },
    request({ origins }) {
      p.requests.push({ origins, inGesture: p.inGesture });
      if (!p.inGesture) return Promise.reject(new Error('This function must be called during a user gesture'));
      if (p.answer) origins.forEach((o) => p.granted.add(o));
      return Promise.resolve(p.answer);
    },
    onAdded: { addListener: (fn) => listeners.added.push(fn) },
    onRemoved: { addListener: (fn) => listeners.removed.push(fn) },
    listeners,
  };
  return p;
}

const settle = () => new Promise((r) => setTimeout(r, 10));

function rows(doc) {
  return doc.root.children.map((li) => {
    const status = li.all((e) => e.className.split(' ').includes('status'))[0];
    const button = li.all((e) => e.tagName === 'BUTTON')[0];
    return { site: li.attrs['data-site'], text: li.all((e) => e.className === 'label')[0].textContent, status: status.textContent, button };
  });
}

test('siteStates reports each site and whether it is granted', async () => {
  const perms = fakePermissions(['https://claude.ai/*']);
  const states = await siteStates(perms);
  assert.deepEqual(states.map((s) => s.site), Object.keys(SITE_ACCESS));
  assert.deepEqual(states.find((s) => s.site === 'claudeai'), { site: 'claudeai', label: 'claude.ai', granted: true, partial: false });
  assert.equal(states.find((s) => s.site === 'chatgpt').granted, false);
  assert.equal(states.find((s) => s.site === 'chatgpt').partial, false);
});

// ChatGPT's page granted but its file host withheld: list, read and send
// work, only files do not. The page says so rather than "Not granted",
// and keeps a button that asks for the rest.
test('a site with its pages granted but not its file host shows as partly granted', async () => {
  const perms = fakePermissions(['https://claude.ai/*', 'https://chatgpt.com/*']);
  const chat = (await siteStates(perms)).find((s) => s.site === 'chatgpt');
  assert.deepEqual(chat, { site: 'chatgpt', label: 'ChatGPT', granted: false, partial: true });

  const doc = fakeDocument();
  const page = renderOptions({ document: doc, permissions: perms });
  await page.ready;
  let r = rows(doc);
  assert.equal(r[0].status, 'Granted (images and files need file access)');
  const status = doc.root.children[0].all((e) => e.className.split(' ').includes('status'))[0];
  assert.equal(status.className, 'status partial');
  assert.equal(r[0].button.hidden, false, 'the button stays to ask for the file host');
  assert.equal(r[0].button.textContent, 'Grant file access');

  perms.inGesture = true;
  r[0].button.click();
  perms.inGesture = false;
  assert.deepEqual(perms.requests[0].origins, [...SITE_ACCESS.chatgpt.origins]);
  await settle();
  r = rows(doc);
  assert.equal(r[0].status, 'Granted');
  assert.equal(r[0].button.hidden, true);
});

test('the options page lists sites, shows grants, and grants from the click', async () => {
  const doc = fakeDocument();
  const perms = fakePermissions(['https://claude.ai/*']);
  const page = renderOptions({ document: doc, permissions: perms });
  await page.ready;
  let r = rows(doc);
  assert.deepEqual(r.map((x) => [x.site, x.text, x.status]), [['chatgpt', 'ChatGPT', 'Not granted'], ['claudeai', 'claude.ai', 'Granted'], ['grok', 'Grok', 'Not granted'], ['gemini', 'Gemini', 'Not granted'], ['perplexity', 'Perplexity', 'Not granted'], ['copilot', 'Copilot', 'Not granted']]);
  assert.equal(r[0].button.hidden, false);
  assert.equal(r[1].button.hidden, true, 'no grant button for a granted site');

  // The click asks Chrome right away, inside the gesture.
  perms.inGesture = true;
  r[0].button.click();
  perms.inGesture = false;
  assert.equal(perms.requests.length, 1);
  assert.deepEqual(perms.requests[0], { origins: [...SITE_ACCESS.chatgpt.origins], inGesture: true });
  await settle();
  r = rows(doc);
  assert.equal(r[0].status, 'Granted');
  assert.equal(r[0].button.hidden, true);

  // A revocation made elsewhere shows here; then the owner declines
  // Chrome's prompt: still not granted, and the button works again.
  perms.granted.delete('https://claude.ai/*');
  perms.listeners.removed.forEach((fn) => fn({ origins: ['https://claude.ai/*'] }));
  await settle();
  r = rows(doc);
  assert.equal(r[1].status, 'Not granted', 'a revocation elsewhere refreshes the page');
  perms.answer = false;
  perms.inGesture = true;
  r[1].button.click();
  perms.inGesture = false;
  await settle();
  r = rows(doc);
  assert.equal(r[1].status, 'Not granted');
  assert.equal(r[1].button.disabled, false);

  // Grok, an optional site: one click asks for grok.com and its image
  // host together.
  perms.answer = true;
  perms.inGesture = true;
  r[2].button.click();
  perms.inGesture = false;
  await settle();
  assert.deepEqual(perms.requests.at(-1), { origins: ['https://grok.com/*', 'https://assets.grok.com/*'], inGesture: true });
  r = rows(doc);
  assert.equal(r[2].status, 'Granted');

  // Gemini's grant covers its image host too, in one request.
  perms.answer = true;
  perms.inGesture = true;
  r[3].button.click();
  perms.inGesture = false;
  assert.deepEqual(perms.requests.at(-1), { origins: ['https://gemini.google.com/*', 'https://lh3.googleusercontent.com/*', 'https://lh3.google.com/*'], inGesture: true });
  await settle();
  assert.equal(rows(doc)[3].status, 'Granted');

  // Perplexity: one origin, www.perplexity.ai.
  perms.answer = true;
  perms.inGesture = true;
  rows(doc)[4].button.click();
  perms.inGesture = false;
  assert.deepEqual(perms.requests.at(-1), { origins: ['https://www.perplexity.ai/*'], inGesture: true });
  await settle();
  assert.equal(rows(doc)[4].status, 'Granted');

  // Copilot's grant covers copilot.microsoft.com, where it starts, too.
  perms.inGesture = true;
  rows(doc)[5].button.click();
  perms.inGesture = false;
  assert.deepEqual(perms.requests.at(-1), { origins: ['https://copilot.com/*', 'https://copilot.microsoft.com/*'], inGesture: true });
  await settle();
  assert.equal(rows(doc)[5].status, 'Granted');
});

test('a grant Chrome refuses says so on the page, and a later grant clears it', async () => {
  const doc = fakeDocument();
  const perms = fakePermissions(['https://gemini.google.com/*', 'https://lh3.googleusercontent.com/*']);
  const page = renderOptions({ document: doc, permissions: perms });
  await page.ready;
  assert.equal(rows(doc)[3].status, 'Granted (images and files need file access)');
  // A loaded manifest older than ops.js does not list the new origin.
  perms.request = ({ origins }) => {
    perms.requests.push({ origins, inGesture: perms.inGesture });
    return Promise.reject(new Error('Only permissions specified in the manifest may be requested.'));
  };
  rows(doc)[3].button.click();
  await settle();
  assert.match(rows(doc)[3].status, /^Chrome refused: Only permissions specified in the manifest may be requested\..*Reload the extension/);
  assert.equal(rows(doc)[3].button.disabled, false);
  perms.request = ({ origins }) => {
    origins.forEach((o) => perms.granted.add(o));
    return Promise.resolve(true);
  };
  rows(doc)[3].button.click();
  await settle();
  assert.equal(rows(doc)[3].status, 'Granted');
});

test('the options page is CSP-safe: no inline script, no main-world code', () => {
  const html = readFileSync(new URL('../options.html', import.meta.url), 'utf8');
  const scripts = [...html.matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script>/g)];
  assert.equal(scripts.length, 1);
  assert.match(scripts[0][1], /src="options\.js"/);
  assert.equal(scripts[0][2].trim(), '', 'no inline script');
  assert.ok(!/\son[a-z]+\s*=/i.test(html), 'no inline event handlers');
  const js = readFileSync(new URL('../options.js', import.meta.url), 'utf8');
  for (const banned of [/innerHTML/, /outerHTML/, /insertAdjacentHTML/, /document\.write/, /executeScript/, /chrome\.(tabs|scripting)/]) {
    assert.ok(!banned.test(js), `options.js matches ${banned}`);
  }
});
