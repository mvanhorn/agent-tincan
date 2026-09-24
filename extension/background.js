// Agent Tincan history bridge: background service worker.
//
// Connects to the native host `tincan history native-host` and answers its
// requests with the fixed operations in ops.js. An open native port keeps
// the worker alive; if the host is missing or exits, an alarm retries.

import { NATIVE_HOST, OpError, createRunner, validate } from './ops.js';

const RECONNECT_ALARM = 'tincan-reconnect';
const runner = createRunner({ fetch: (url, init) => fetch(url, init) });
let port = null;

function post(msg) {
  if (!port) return;
  try {
    port.postMessage(msg);
  } catch {
    // The host went away; the alarm reconnects.
  }
}

function requestId(msg) {
  return msg && Number.isSafeInteger(msg.id) && msg.id >= 0 ? msg.id : 0;
}

async function onHostMessage(msg) {
  let req;
  try {
    req = validate(msg);
  } catch (e) {
    post({ id: requestId(msg), ok: false, error: { code: 'bad_request', message: e.message } });
    return;
  }
  try {
    await runner.run(req.op, req.args, (frame) => post({ id: req.id, ...frame }));
  } catch (e) {
    const known = e instanceof OpError;
    post({ id: req.id, ok: false, error: { code: known ? e.code : 'internal', message: known ? e.message : 'internal error' } });
  }
}

function connect() {
  if (port) return;
  let p;
  try {
    p = chrome.runtime.connectNative(NATIVE_HOST);
  } catch {
    return;
  }
  port = p;
  p.onMessage.addListener((msg) => {
    onHostMessage(msg);
  });
  p.onDisconnect.addListener(() => {
    // Reading lastError marks it handled (host not installed, or exited).
    void chrome.runtime.lastError;
    if (port === p) port = null;
  });
}

chrome.runtime.onStartup.addListener(connect);
chrome.runtime.onInstalled.addListener(connect);
chrome.alarms.create(RECONNECT_ALARM, { periodInMinutes: 1 });
chrome.alarms.onAlarm.addListener((alarm) => {
  if (alarm.name === RECONNECT_ALARM) connect();
});
connect();
