import { test } from 'node:test';
import assert from 'node:assert/strict';

// A fake chrome.runtime native port and alarms API.
function fakeChrome() {
  const listeners = { message: [], disconnect: [], startup: [], installed: [], alarm: [], permAdded: [], permRemoved: [], storage: [] };
  const state = { connects: [], posted: [], alarms: [], port: null, ports: [], reloads: 0, granted: new Set(['https://claude.ai/*']), stored: {} };
  const makePort = () => {
    const port = {
      posted: [],
      disconnected: false,
      disconnect: () => { port.disconnected = true; },
      postMessage: (m) => { const copy = JSON.parse(JSON.stringify(m)); state.posted.push(copy); port.posted.push(copy); },
      onMessage: { addListener: (fn) => listeners.message.push(fn) },
      onDisconnect: { addListener: (fn) => listeners.disconnect.push(fn) },
    };
    state.port = port;
    state.ports.push(port);
    return port;
  };
  globalThis.chrome = {
    runtime: {
      connectNative: (name) => { state.connects.push(name); return makePort(); },
      onStartup: { addListener: (fn) => listeners.startup.push(fn) },
      onInstalled: { addListener: (fn) => listeners.installed.push(fn) },
      lastError: undefined,
      getManifest: () => ({ version: '9.9.9' }),
      getURL: (f) => 'chrome-extension://ciejooalclcpgpapboofdbbddphldhnh/' + f,
      reload: () => state.reloads++,
    },
    tabs: { create: async () => { throw new Error('no tabs in this test'); } },
    scripting: { executeScript: async () => { throw new Error('no scripting in this test'); } },
    permissions: {
      contains: async ({ origins }) => origins.every((o) => state.granted.has(o)),
      onAdded: { addListener: (fn) => listeners.permAdded.push(fn) },
      onRemoved: { addListener: (fn) => listeners.permRemoved.push(fn) },
    },
    storage: {
      local: { get: async (key) => (key in state.stored ? { [key]: state.stored[key] } : {}) },
      onChanged: { addListener: (fn) => listeners.storage.push(fn) },
    },
    alarms: {
      create: (name, info) => state.alarms.push({ name, info }),
      onAlarm: { addListener: (fn) => listeners.alarm.push(fn) },
    },
  };
  return { listeners, state };
}

const settle = () => new Promise((r) => setTimeout(r, 20));

test('background connects to the native host and answers only valid requests', async () => {
  const { listeners, state } = fakeChrome();
  const calls = [];
  globalThis.fetch = async (url, init) => {
    if (String(url).startsWith('chrome-extension://')) return new Response('file ' + url);
    calls.push(String(url));
    if (String(url) === 'https://chatgpt.com/api/auth/session') return new Response(JSON.stringify({ accessToken: 'tok' }), { headers: { 'content-type': 'application/json' } });
    return new Response(JSON.stringify({ items: [] }), { headers: { 'content-type': 'application/json' } });
  };
  await import('../background.js');
  await settle();
  assert.deepEqual(state.connects, ['com.agenttincan.history']);
  assert.ok(state.alarms.some((a) => a.name === 'tincan-reconnect'));

  // On connect it says hello: version, unpacked, and a hash per file.
  await settle();
  const hello = state.posted.find((m) => m.hello);
  assert.equal(hello.id, 0);
  assert.equal(hello.hello.version, '9.9.9');
  assert.equal(hello.hello.unpacked, true);
  assert.deepEqual(Object.keys(hello.hello.files), ['manifest.json', 'background.js', 'ops.js', 'send.js', 'options.html', 'options.js', 'icon16.png', 'icon48.png', 'icon128.png']);
  assert.match(hello.hello.files['ops.js'], /^[0-9a-f]{64}$/);
  // It lists the sites Chrome has granted.
  assert.deepEqual(hello.hello.granted, ['claudeai']);

  // chatgpt.com's access is withheld: its ops fail before any fetch.
  const sendOp = (m) => listeners.message.forEach((fn) => fn(m));
  sendOp({ id: 7, op: 'chatgpt.list', args: { count: 3 } });
  await settle();
  assert.equal(state.posted.at(-1).error.code, 'permission_missing');
  assert.equal(calls.length, 0);

  // Granting it (the options page's button) sends a new hello on the same
  // port: the host learns the new list without a reconnect.
  state.granted.add('https://chatgpt.com/*');
  state.granted.add('https://*.oaiusercontent.com/*');
  listeners.permAdded.forEach((fn) => fn({ origins: ['https://chatgpt.com/*'] }));
  await settle();
  assert.equal(state.connects.length, 1, 'no reconnect');
  assert.deepEqual(state.posted.filter((m) => m.hello).map((m) => m.hello.granted), [['claudeai'], ['chatgpt', 'claudeai']]);
  state.granted.delete('https://claude.ai/*');
  listeners.permRemoved.forEach((fn) => fn({ origins: ['https://claude.ai/*'] }));
  await settle();
  assert.deepEqual(state.posted.filter((m) => m.hello).at(-1).hello.granted, ['chatgpt']);
  state.granted.add('https://claude.ai/*');

  const send = (m) => listeners.message.forEach((fn) => fn(m));
  send({ id: 1, op: 'chatgpt.list', args: { count: 3 } });
  await settle();
  assert.deepEqual(state.posted.at(-1), { id: 1, ok: true, result: { items: [] } });

  // The fixed reload op answers, then calls chrome.runtime.reload.
  send({ id: 9, op: 'extension.reload', args: {} });
  await settle();
  assert.deepEqual(state.posted.at(-1), { id: 9, ok: true, result: { reloading: true } });
  await new Promise((r) => setTimeout(r, 300));
  assert.equal(state.reloads, 1);

  const before = calls.length;
  send({ id: 2, op: 'chatgpt.eval', args: { code: 'fetch("https://evil")' } });
  send({ id: 3, op: 'chatgpt.detail', args: { id: '../me' } });
  send({ op: 'chatgpt.list', args: { count: 1 } });
  await settle();
  assert.equal(calls.length, before, 'invalid requests made no fetch');
  const errs = state.posted.slice(-3);
  assert.deepEqual(errs.map((e) => [e.id, e.ok, e.error.code]), [[2, false, 'bad_request'], [3, false, 'bad_request'], [0, false, 'bad_request']]);
  assert.ok(!JSON.stringify(state.posted).includes('tok"'), 'token never posted');

  // The files change on disk (an update without a reload yet). Disconnect,
  // then the alarm reconnects: the new hello still reports the hashes of
  // the files this worker loaded, so the host can see the drift.
  const inner = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    if (String(url).startsWith('chrome-extension://')) return new Response('changed on disk ' + url);
    return inner(url, init);
  };
  listeners.disconnect.forEach((fn) => fn());
  listeners.alarm.forEach((fn) => fn({ name: 'tincan-reconnect' }));
  await settle();
  assert.equal(state.connects.length, 2);
  const hellos = state.posted.filter((m) => m.hello);
  assert.equal(hellos.length, 4);
  assert.deepEqual(hellos[3].hello.files, hello.hello.files, 'hello reports the loaded files, not the ones on disk now');
  // Already connected: no duplicate port.
  listeners.alarm.forEach((fn) => fn({ name: 'tincan-reconnect' }));
  listeners.startup.forEach((fn) => fn());
  await settle();
  assert.equal(state.connects.length, 2);
});

// A hello built before a later grant change must not reach the host after
// the later one: the host keeps the last hello it gets, so an older one
// arriving last would leave it reporting a revoked site as granted.
test('an older hello that finishes last is dropped', async () => {
  const { listeners, state } = fakeChrome();
  // contains answers from the grants as they are when it is called, after
  // state.delay ms (a slow permissions query).
  state.delay = 0;
  globalThis.chrome.permissions.contains = ({ origins }) => {
    const answer = origins.every((o) => state.granted.has(o));
    return new Promise((r) => setTimeout(() => r(answer), state.delay));
  };
  globalThis.fetch = async (url) => new Response('file ' + url);
  await import('../background.js?stale-hello');
  await settle();
  assert.deepEqual(state.posted.filter((m) => m.hello).map((m) => m.hello.granted), [['claudeai']]);

  // ChatGPT is granted; that hello's permissions query is slow.
  state.granted.add('https://chatgpt.com/*');
  state.delay = 60;
  listeners.permAdded.forEach((fn) => fn({ origins: ['https://chatgpt.com/*'] }));
  await new Promise((r) => setTimeout(r, 10));
  // Then it is revoked, and this hello's query is fast.
  state.granted.delete('https://chatgpt.com/*');
  state.delay = 0;
  listeners.permRemoved.forEach((fn) => fn({ origins: ['https://chatgpt.com/*'] }));
  await new Promise((r) => setTimeout(r, 200));

  const hellos = state.posted.filter((m) => m.hello).map((m) => m.hello.granted);
  assert.deepEqual(hellos.at(-1), ['claudeai'], `last hello the host saw: ${JSON.stringify(hellos)}`);
  assert.ok(!hellos.slice(1).some((g) => g.includes('chatgpt')), 'the stale hello was not sent');
});

// The first frame on every port names the Chrome profile, before the
// hello (whose files take a while to hash): the native host binds no
// socket until it comes. Unnamed is null and the hello carries no profile.
test('the profile frame is the first message on a new port', async () => {
  const { state } = fakeChrome();
  let release;
  const slow = new Promise((r) => { release = r; });
  globalThis.fetch = async (url) => { await slow; return new Response('file ' + url); };
  await import('../background.js?unnamed');
  await settle();
  assert.deepEqual(state.ports[0].posted, [{ id: 0, profile: null }], 'only the profile frame before hashing ends');
  release();
  await settle();
  const hello = state.ports[0].posted[1];
  assert.ok(hello.hello);
  assert.ok(!('profile' in hello.hello));
});

test('a named profile is in the first frame and the hello; a rename reconnects', async () => {
  const { listeners, state } = fakeChrome();
  state.stored.profile = 'work';
  globalThis.fetch = async (url) => new Response('file ' + url);
  await import('../background.js?named');
  await settle();
  assert.deepEqual(state.ports[0].posted[0], { id: 0, profile: 'work' });
  assert.equal(state.ports[0].posted[1].hello.profile, 'work');

  // A change to another key, or to the same name, keeps the port.
  listeners.storage.forEach((fn) => fn({ other: { newValue: 1 } }, 'local'));
  listeners.storage.forEach((fn) => fn({ profile: { oldValue: 'work', newValue: 'work' } }, 'local'));
  await settle();
  assert.equal(state.connects.length, 1);

  // The options page clears the name: the port is closed and a new one
  // says unnamed, so a new host serves host.sock.
  delete state.stored.profile;
  listeners.storage.forEach((fn) => fn({ profile: { oldValue: 'work' } }, 'local'));
  await settle();
  assert.equal(state.ports[0].disconnected, true);
  assert.equal(state.connects.length, 2);
  assert.deepEqual(state.ports[1].posted[0], { id: 0, profile: null });
  assert.ok(!('profile' in state.ports[1].posted[1].hello));

  // A stored value that is not a valid name is passed on for the host to
  // refuse, never turned into unnamed.
  state.stored.profile = 'Work!';
  listeners.storage.forEach((fn) => fn({ profile: { newValue: 'Work!' } }, 'local'));
  await settle();
  assert.deepEqual(state.ports[2].posted[0], { id: 0, profile: 'Work!' });
});

// A storage read that fails never connects as unnamed (that would serve
// the unnamed profile's socket from this account); the alarm retries.
test('a failed storage read does not connect', async () => {
  const { listeners, state } = fakeChrome();
  let fail = true;
  globalThis.chrome.storage.local.get = async (key) => {
    if (fail) throw new Error('storage unavailable');
    return { [key]: 'work' };
  };
  globalThis.fetch = async (url) => new Response('file ' + url);
  await import('../background.js?storage-fails');
  await settle();
  assert.equal(state.connects.length, 0);
  assert.deepEqual(state.posted, []);
  fail = false;
  listeners.alarm.forEach((fn) => fn({ name: 'tincan-reconnect' }));
  await settle();
  assert.equal(state.connects.length, 1);
  assert.deepEqual(state.ports[0].posted[0], { id: 0, profile: 'work' });
});

// A name saved while a connect is still reading storage is not lost: the
// connect reads again and opens with the new name.
test('a rename during an in-flight connect opens with the new name', async () => {
  const { listeners, state } = fakeChrome();
  let release;
  const gate = new Promise((r) => { release = r; });
  let reads = 0;
  globalThis.chrome.storage.local.get = async (key) => {
    reads++;
    if (reads === 1) { await gate; return {}; }
    return { [key]: 'work' };
  };
  globalThis.fetch = async (url) => new Response('file ' + url);
  await import('../background.js?rename-in-flight');
  await settle();
  listeners.storage.forEach((fn) => fn({ profile: { newValue: 'work' } }, 'local'));
  release();
  await settle();
  assert.equal(state.connects.length, 1);
  assert.deepEqual(state.ports[0].posted[0], { id: 0, profile: 'work' });
});
