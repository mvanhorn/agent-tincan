// Agent Tincan history bridge: background service worker.
//
// Connects to the native host `tincan history native-host` and answers its
// requests with the fixed operations in ops.js (sends go through send.js).
// On connect it says hello with its version and file hashes, so the host
// can ask for a reload when the unpacked files on disk are newer. An open
// native port keeps the worker alive; if the host is missing or exits, an
// alarm retries.

import { NATIVE_HOST, OpError, createRunner, hashFiles, helloMessage, validate } from './ops.js';
import { createSender } from './send.js';

const RECONNECT_ALARM = 'tincan-reconnect';
const runner = createRunner({
  fetch: (url, init) => fetch(url, init),
  sender: createSender({ tabs: chrome.tabs, scripting: chrome.scripting }),
  reload: () => chrome.runtime.reload(),
});
let port = null;
// The files are hashed once, when this worker starts: every hello reports
// the code Chrome loaded, not whatever is on disk at reconnect time, so
// the host can tell when an update is waiting for a reload.
const loadedFiles = hashFiles({ getURL: (f) => chrome.runtime.getURL(f), fetch: (url, init) => fetch(url, init) });

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
  loadedFiles
    .then((files) => helloMessage({ manifest: chrome.runtime.getManifest(), files }))
    .then((m) => {
      if (port === p) post(m);
    })
    .catch(() => {});
}

chrome.runtime.onStartup.addListener(connect);
chrome.runtime.onInstalled.addListener(connect);
chrome.alarms.create(RECONNECT_ALARM, { periodInMinutes: 1 });
chrome.alarms.onAlarm.addListener((alarm) => {
  if (alarm.name === RECONNECT_ALARM) connect();
});
connect();
