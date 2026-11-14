// Agent Tincan history bridge: background service worker.
//
// Connects to the native host `tincan history native-host` and answers its
// requests with the fixed operations in ops.js (sends go through send.js).
// On connect it says hello with its version and file hashes, so the host
// can ask for a reload when the unpacked files on disk are newer, and the
// sites it has access to; a grant or revocation (from the options page or
// Chrome's site settings) sends the hello again on the same port. An open
// native port keeps the worker alive; if the host is missing or exits, an
// alarm retries.
//
// Before the hello, the first frame on every port names this Chrome
// profile (the owner's name from the options page, kept in
// chrome.storage.local, or null): the host serves that profile's socket,
// so web agents for two accounts in two profiles never mix. A changed name
// reconnects, and the new host serves the new socket.

import { NATIVE_HOST, PROFILE_KEY, createRunner, errorFrame, hashFiles, helloMessage, profileFrame, storedProfile, validate } from './ops.js';
import { createSender } from './send.js';

const RECONNECT_ALARM = 'tincan-reconnect';
const runner = createRunner({
  fetch: (url, init) => fetch(url, init),
  sender: createSender({ tabs: chrome.tabs, scripting: chrome.scripting }),
  reload: () => chrome.runtime.reload(),
  permissions: chrome.permissions,
});
let port = null;
// Native connection keeps the worker alive; promptly expire idle buffers.
const inputExpiry = setInterval(() => runner.expireInputs(), 5000);
inputExpiry.unref?.(); // Node tests need not stay alive for the worker timer.
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

async function onHostMessage(msg, p) {
  const reply = (frame) => { if (port === p) post(frame); };
  let req;
  try {
    req = validate(msg);
  } catch (e) {
    reply({ id: requestId(msg), ok: false, error: { code: 'bad_request', message: e.message } });
    return;
  }
  try {
    await runner.run(req.op, req.args, (frame) => reply({ id: req.id, ...frame }), req.input);
  } catch (e) {
    reply({ id: req.id, ...errorFrame(e) });
  }
}

// portProfile is the profile name the open port was started with.
let portProfile = null;
let connecting = false;
// profileGen counts profile changes, so a connect whose storage read
// started before a change reads again rather than opening with the old
// name.
let profileGen = 0;

// readProfile is the stored profile name, null when none is stored. It
// throws when storage cannot be read: guessing unnamed would let a named
// profile's host serve the unnamed profile's socket from this account.
async function readProfile() {
  const got = await chrome.storage.local.get(PROFILE_KEY);
  return storedProfile(got && got[PROFILE_KEY]);
}

async function connect() {
  if (port || connecting) return;
  connecting = true;
  let profile;
  try {
    let gen;
    do {
      gen = profileGen;
      profile = await readProfile();
    } while (gen !== profileGen);
  } catch {
    // Storage could not be read: stay disconnected; the alarm retries.
    return;
  } finally {
    connecting = false;
  }
  if (port) return;
  let p;
  try {
    p = chrome.runtime.connectNative(NATIVE_HOST);
  } catch {
    return;
  }
  port = p;
  portProfile = profile;
  p.onMessage.addListener((msg) => {
    if (port === p) onHostMessage(msg, p);
  });
  p.onDisconnect.addListener(() => {
    // Reading lastError marks it handled (host not installed, or exited).
    void chrome.runtime.lastError;
    if (port === p) { port = null; runner.clearInputs(); }
  });
  // The profile frame goes first, before the files are hashed: the host
  // binds no socket until it arrives.
  post(profileFrame(profile));
  hello(p);
}

// profileChanged reconnects when the options page saves a new name, so a
// new host serves the new name's socket (a host never moves sockets).
function profileChanged(changes, area) {
  if (area !== 'local' || !changes || !(PROFILE_KEY in changes)) return;
  profileGen++;
  if (port && storedProfile(changes[PROFILE_KEY].newValue) === portProfile) return;
  const p = port;
  port = null;
  runner.clearInputs();
  if (p) {
    try {
      p.disconnect();
    } catch {
      // Already gone.
    }
  }
  connect();
}

// helloSeq numbers the hellos this worker starts. The host keeps the last
// hello it receives, and building one awaits Chrome's permissions API, so
// two grant changes in quick succession can finish out of order; only the
// newest hello started is posted, so an older one (a grant since revoked)
// never overwrites a newer one.
let helloSeq = 0;

// hello tells the host on port p who this worker is and which sites it
// may serve.
function hello(p) {
  const seq = ++helloSeq;
  const profile = portProfile;
  loadedFiles
    .then((files) => helloMessage({ manifest: chrome.runtime.getManifest(), files, permissions: chrome.permissions, profile }))
    .then((m) => {
      if (port === p && seq === helloSeq) post(m);
    })
    .catch(() => {});
}

function grantsChanged() {
  if (port) hello(port);
}

chrome.runtime.onStartup.addListener(connect);
chrome.runtime.onInstalled.addListener(connect);
chrome.permissions.onAdded.addListener(grantsChanged);
chrome.permissions.onRemoved.addListener(grantsChanged);
chrome.storage.onChanged.addListener(profileChanged);
chrome.alarms.create(RECONNECT_ALARM, { periodInMinutes: 1 });
chrome.alarms.onAlarm.addListener((alarm) => {
  if (alarm.name === RECONNECT_ALARM) connect();
});
connect();
