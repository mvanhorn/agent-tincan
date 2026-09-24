import { test } from 'node:test';
import assert from 'node:assert/strict';

// A fake chrome.runtime native port and alarms API.
function fakeChrome() {
  const listeners = { message: [], disconnect: [], startup: [], installed: [], alarm: [] };
  const state = { connects: [], posted: [], alarms: [], port: null };
  const makePort = () => {
    const port = {
      postMessage: (m) => state.posted.push(JSON.parse(JSON.stringify(m))),
      onMessage: { addListener: (fn) => listeners.message.push(fn) },
      onDisconnect: { addListener: (fn) => listeners.disconnect.push(fn) },
    };
    state.port = port;
    return port;
  };
  globalThis.chrome = {
    runtime: {
      connectNative: (name) => { state.connects.push(name); return makePort(); },
      onStartup: { addListener: (fn) => listeners.startup.push(fn) },
      onInstalled: { addListener: (fn) => listeners.installed.push(fn) },
      lastError: undefined,
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
    calls.push(String(url));
    if (String(url) === 'https://chatgpt.com/api/auth/session') return new Response(JSON.stringify({ accessToken: 'tok' }), { headers: { 'content-type': 'application/json' } });
    return new Response(JSON.stringify({ items: [] }), { headers: { 'content-type': 'application/json' } });
  };
  await import('../background.js');
  assert.deepEqual(state.connects, ['com.agenttincan.history']);
  assert.ok(state.alarms.some((a) => a.name === 'tincan-reconnect'));

  const send = (m) => listeners.message.forEach((fn) => fn(m));
  send({ id: 1, op: 'chatgpt.list', args: { count: 3 } });
  await settle();
  assert.deepEqual(state.posted.at(-1), { id: 1, ok: true, result: { items: [] } });

  const before = calls.length;
  send({ id: 2, op: 'chatgpt.eval', args: { code: 'fetch("https://evil")' } });
  send({ id: 3, op: 'chatgpt.detail', args: { id: '../me' } });
  send({ op: 'chatgpt.list', args: { count: 1 } });
  await settle();
  assert.equal(calls.length, before, 'invalid requests made no fetch');
  const errs = state.posted.slice(-3);
  assert.deepEqual(errs.map((e) => [e.id, e.ok, e.error.code]), [[2, false, 'bad_request'], [3, false, 'bad_request'], [0, false, 'bad_request']]);
  assert.ok(!JSON.stringify(state.posted).includes('tok"'), 'token never posted');

  // Disconnect, then the alarm reconnects.
  listeners.disconnect.forEach((fn) => fn());
  listeners.alarm.forEach((fn) => fn({ name: 'tincan-reconnect' }));
  assert.equal(state.connects.length, 2);
  // Already connected: no duplicate port.
  listeners.alarm.forEach((fn) => fn({ name: 'tincan-reconnect' }));
  listeners.startup.forEach((fn) => fn());
  assert.equal(state.connects.length, 2);
});
