// Fixed operations for the Agent Tincan history bridge and web agents.
//
// The native host sends {id, op, args}. Only the operations in SPEC exist,
// and their arguments are only validated ids, integer counts, booleans and
// one capped message string. The read operations run the fixed fetch code
// below with the user's own session (credentials: 'include'). The send
// operations hand the message, as data, to the sender (send.js), which
// types it into a background tab the extension opens itself; the close
// operations close that tab once the reply is finished.
// extension.reload asks the worker to reload itself so Chrome re-reads the
// unpacked files after an update. Nothing in a message or a response is
// ever executed; responses are returned as data.
//
// ChatGPT's access token is read from /api/auth/session inside this worker
// and used only for the Authorization header of the next requests. It is
// never returned, logged or stored.

export const NATIVE_HOST = 'com.agenttincan.history';
export const MAX_COUNT = 100;
export const MAX_FILE_BYTES = 10 * 1024 * 1024;
// A multiple of 3 so every chunk is whole base64; 512 KiB encoded per
// message.
export const CHUNK_BYTES = 384 * 1024;
// MAX_MESSAGE_BYTES caps a send op's message (UTF-8 bytes).
export const MAX_MESSAGE_BYTES = 32 * 1024;
// EXTENSION_FILES are the files the hello message reports hashes of, so the
// native host can tell when the unpacked files on disk have changed.
export const EXTENSION_FILES = Object.freeze(['manifest.json', 'background.js', 'ops.js', 'send.js', 'icon16.png', 'icon48.png', 'icon128.png']);

// extension.reload waits while the sender has tabs (a reload would lose
// track of them), checking every RELOAD_RETRY_MS for at most
// RELOAD_MAX_WAIT_MS. At the cap it closes the finished tabs and reloads
// anyway; a send still typing at that point is abandoned (its tab stays
// open and the Go side's wait for it times out).
export const RELOAD_RETRY_MS = 5000;
export const RELOAD_MAX_WAIT_MS = 5 * 60 * 1000;

const ID_RE = /^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/;
const CHATGPT = 'https://chatgpt.com';
const CLAUDE = 'https://claude.ai';

// Argument kinds: 'count' is a required integer 1..MAX_COUNT, 'id' a
// required id, 'id?' an optional id, 'bool?' an optional boolean and
// 'message' a required non-blank string of at most MAX_MESSAGE_BYTES.
const SEND_SPEC = Object.freeze({ message: 'message', conversation_id: 'id?', new_chat: 'bool?' });
const CLOSE_SPEC = Object.freeze({ conversation_id: 'id' });
const SPEC = Object.freeze({
  'chatgpt.list': Object.freeze({ count: 'count' }),
  'chatgpt.detail': Object.freeze({ id: 'id' }),
  'chatgpt.file': Object.freeze({ file_id: 'id', conversation_id: 'id?' }),
  'claudeai.list': Object.freeze({ count: 'count' }),
  'claudeai.detail': Object.freeze({ id: 'id' }),
  'claudeai.file': Object.freeze({ file_id: 'id' }),
  'chatgpt.send': SEND_SPEC,
  'claudeai.send': SEND_SPEC,
  'chatgpt.close': CLOSE_SPEC,
  'claudeai.close': CLOSE_SPEC,
  'extension.reload': Object.freeze({}),
});

export const OPS = new Set(Object.keys(SPEC));

// MAX_RETRY_AFTER_S caps a Retry-After reported to the host, in seconds.
export const MAX_RETRY_AFTER_S = 3600;

export class OpError extends Error {
  // retryAfter, for rate_limited, is the site's Retry-After in whole
  // seconds when it sent one.
  constructor(code, message, retryAfter) {
    super(message);
    this.code = code;
    if (Number.isSafeInteger(retryAfter) && retryAfter >= 0) this.retryAfter = retryAfter;
  }
}

// errorFrame is the failure frame for an error thrown by an operation:
// its code and message (retry_after too for a rate limit), or a bare
// internal error for anything that is not an OpError, so no unexpected
// detail leaves the extension.
export function errorFrame(e) {
  if (!(e instanceof OpError)) return { ok: false, error: { code: 'internal', message: 'internal error' } };
  const error = { code: e.code, message: e.message };
  if (e.retryAfter !== undefined) error.retry_after = e.retryAfter;
  return { ok: false, error };
}

// retryAfterSeconds reads a Retry-After header: delay seconds or an
// HTTP date. It returns whole seconds (capped at MAX_RETRY_AFTER_S), or
// undefined when the header is missing, malformed or in the past.
function retryAfterSeconds(res) {
  const v = (res.headers.get('retry-after') || '').trim();
  if (v === '') return undefined;
  let s;
  if (/^\d+$/.test(v)) {
    s = Number(v);
  } else {
    const at = Date.parse(v);
    if (Number.isNaN(at)) return undefined;
    s = Math.ceil((at - Date.now()) / 1000);
    if (s < 0) return undefined;
  }
  return Math.min(s, MAX_RETRY_AFTER_S);
}

const bad = (m) => new OpError('bad_request', m);

function isPlainObject(v) {
  return v !== null && typeof v === 'object' && !Array.isArray(v) && Object.getPrototypeOf(v) === Object.prototype;
}

// validate returns {id, op, args} for a well-formed request or throws an
// OpError with code bad_request.
export function validate(msg) {
  if (!isPlainObject(msg)) throw bad('request is not an object');
  for (const k of Object.keys(msg)) {
    if (k !== 'id' && k !== 'op' && k !== 'args') throw bad('unexpected field');
  }
  const { id, op, args } = msg;
  if (!Number.isSafeInteger(id) || id < 0) throw bad('invalid request id');
  if (typeof op !== 'string' || !Object.hasOwn(SPEC, op)) throw bad('unknown operation');
  if (!isPlainObject(args)) throw bad('missing args');
  const spec = SPEC[op];
  const out = {};
  for (const k of Object.keys(args)) {
    if (!Object.hasOwn(spec, k)) throw bad('unexpected argument');
  }
  for (const [k, kind] of Object.entries(spec)) {
    const v = args[k];
    if (kind === 'count') {
      if (!Number.isInteger(v) || v < 1 || v > MAX_COUNT) throw bad(`${k} must be an integer from 1 to ${MAX_COUNT}`);
      out[k] = v;
    } else if (v === undefined && (kind === 'id?' || kind === 'bool?')) {
      continue;
    } else if (kind === 'bool?') {
      if (typeof v !== 'boolean') throw bad(`${k} must be a boolean`);
      out[k] = v;
    } else if (kind === 'message') {
      if (typeof v !== 'string' || v.trim() === '') throw bad(`${k} must be a non-empty string`);
      if (new TextEncoder().encode(v).length > MAX_MESSAGE_BYTES) throw bad(`${k} is over ${MAX_MESSAGE_BYTES} bytes`);
      out[k] = v;
    } else {
      if (typeof v !== 'string' || !ID_RE.test(v)) throw bad(`invalid ${k}`);
      out[k] = v;
    }
  }
  if (out.new_chat === true && out.conversation_id !== undefined) throw bad('new_chat and conversation_id together');
  return { id, op, args: out };
}

async function sha256Hex(bytes) {
  const d = new Uint8Array(await crypto.subtle.digest('SHA-256', bytes));
  return Array.from(d, (b) => b.toString(16).padStart(2, '0')).join('');
}

// hashFiles returns the sha256 of each of EXTENSION_FILES, read through
// fetch; a file that cannot be read is reported as ''. The worker calls it
// once when it starts, so the hashes describe the code Chrome loaded even
// after the unpacked files on disk change.
export async function hashFiles({ getURL, fetch }) {
  const files = {};
  for (const name of EXTENSION_FILES) {
    try {
      const res = await fetch(getURL(name), { cache: 'no-store' });
      files[name] = res.ok ? await sha256Hex(new Uint8Array(await res.arrayBuffer())) : '';
    } catch {
      files[name] = '';
    }
  }
  return files;
}

// helloMessage is what the worker tells the native host when it connects:
// its version, whether it is unpacked (only an unpacked extension picks up
// new files on reload), and the sha256 of each of its files as Chrome
// loaded them: files when given (hashFiles from worker start), else
// hashed now.
export async function helloMessage({ manifest, getURL, fetch, files }) {
  const hashes = files && typeof files === 'object' ? files : await hashFiles({ getURL, fetch });
  const m = manifest && typeof manifest === 'object' ? manifest : {};
  return { id: 0, hello: { version: typeof m.version === 'string' ? m.version : '', unpacked: !('update_url' in m), files: hashes } };
}

function pathOf(url) {
  try {
    return new URL(url).pathname;
  } catch {
    return '';
  }
}

function contentType(res) {
  return (res.headers.get('content-type') || '').split(';')[0].trim().toLowerCase();
}

// check maps an HTTP status to an error class. notFound marks endpoints
// where 404 means the item is gone rather than the API moved.
function check(res, url, notFound) {
  if (res.ok) return;
  const where = `HTTP ${res.status} from ${pathOf(url)}`;
  if (res.status === 401) throw new OpError('not_logged_in', where);
  if (res.status === 403) {
    if (contentType(res) === 'text/html') throw new OpError('blocked', where);
    throw new OpError('not_logged_in', where);
  }
  if (res.status === 404 || res.status === 410) throw new OpError(notFound ? 'not_found' : 'endpoint_changed', where);
  if (res.status === 429) throw new OpError('rate_limited', where, retryAfterSeconds(res));
  throw new OpError('http_error', where);
}

function b64(bytes) {
  let s = '';
  for (let i = 0; i < bytes.length; i += 0x8000) {
    s += String.fromCharCode.apply(null, bytes.subarray(i, i + 0x8000));
  }
  return btoa(s);
}

// createRunner returns the operation runner. sender (send.js) carries out
// the send operations; reload reloads the extension. Either may be absent,
// and its operations then fail as unsupported.
export function createRunner({ fetch, sender = null, reload = null }) {
  let claudeOrg = null;
  let reloadPending = false;

  // scheduleReload reloads once the sender is idle, or at the cap after
  // closing its kept tabs. Timers are looked up at call time so tests can
  // mock them.
  function scheduleReload() {
    if (reloadPending) return;
    reloadPending = true;
    const start = Date.now();
    const attempt = async () => {
      const busy = sender && typeof sender.busy === 'function' && sender.busy();
      if (busy) {
        if (Date.now() - start < RELOAD_MAX_WAIT_MS) {
          setTimeout(attempt, RELOAD_RETRY_MS);
          return;
        }
        try {
          if (typeof sender.closeAllKept === 'function') await sender.closeAllKept();
        } catch {
          // Reload regardless.
        }
      }
      reload();
    };
    setTimeout(attempt, 200);
  }

  async function send(url, init) {
    try {
      return await fetch(url, { method: 'GET', cache: 'no-store', ...init });
    } catch {
      throw new OpError('network', `network error for ${pathOf(url)}`);
    }
  }

  async function getJSON(url, init, notFound = false) {
    const res = await send(url, { credentials: 'include', ...init });
    check(res, url, notFound);
    if (!contentType(res).includes('json')) throw new OpError('endpoint_changed', `non-JSON answer from ${pathOf(url)}`);
    try {
      return await res.json();
    } catch {
      throw new OpError('endpoint_changed', `malformed JSON from ${pathOf(url)}`);
    }
  }

  // readCapped reads the body, throwing too_large as soon as it passes
  // MAX_FILE_BYTES rather than after buffering all of it.
  async function readCapped(res) {
    if (!res.body) return new Uint8Array(0);
    const reader = res.body.getReader();
    const parts = [];
    let total = 0;
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      total += value.length;
      if (total > MAX_FILE_BYTES) {
        reader.cancel().catch(() => {});
        throw new OpError('too_large', `file over ${MAX_FILE_BYTES} bytes`);
      }
      parts.push(value);
    }
    const bytes = new Uint8Array(total);
    let off = 0;
    for (const part of parts) {
      bytes.set(part, off);
      off += part.length;
    }
    return bytes;
  }

  async function emitFile(url, init, emit) {
    const res = await send(url, init);
    check(res, url, true);
    const declared = Number(res.headers.get('content-length') || 0);
    if (declared > MAX_FILE_BYTES) throw new OpError('too_large', `file over ${MAX_FILE_BYTES} bytes`);
    const mime = contentType(res);
    if (!mime.startsWith('image/') && mime !== 'application/octet-stream') {
      throw new OpError('endpoint_changed', `unexpected file type from ${pathOf(url)}`);
    }
    const bytes = await readCapped(res);
    if (bytes.length === 0) throw new OpError('endpoint_changed', 'empty file');
    for (let seq = 0, off = 0; off < bytes.length; seq++, off += CHUNK_BYTES) {
      const end = Math.min(off + CHUNK_BYTES, bytes.length);
      emit({ ok: true, mime, size: bytes.length, chunk: { seq, last: end === bytes.length, data: b64(bytes.subarray(off, end)) } });
    }
  }

  // ChatGPT: the token stays in this function's scope.
  async function chatgptAuth() {
    const s = await getJSON(`${CHATGPT}/api/auth/session`, {});
    if (!s || typeof s.accessToken !== 'string' || s.accessToken === '') {
      throw new OpError('not_logged_in', 'no chatgpt.com session');
    }
    return { credentials: 'include', headers: { Authorization: `Bearer ${s.accessToken}` } };
  }

  // allowedDownload resolves a download_url and says how to fetch it:
  // chatgpt.com URLs with the session, signed file URLs without any
  // credentials. Anything else is refused.
  function allowedDownload(raw) {
    let u;
    try {
      u = new URL(raw, CHATGPT);
    } catch {
      return null;
    }
    if (u.protocol !== 'https:' || u.username || u.password || u.port) return null;
    if (u.hostname === 'chatgpt.com') return { url: u.href, session: true };
    if (u.hostname.endsWith('.oaiusercontent.com')) return { url: u.href, session: false };
    return null;
  }

  async function claudeOrgId() {
    if (claudeOrg) return claudeOrg;
    const orgs = await getJSON(`${CLAUDE}/api/organizations`, {});
    if (!Array.isArray(orgs)) throw new OpError('endpoint_changed', 'unexpected organizations answer');
    if (orgs.length === 0) throw new OpError('not_logged_in', 'no claude.ai organization');
    const chat = orgs.find((o) => o && Array.isArray(o.capabilities) && o.capabilities.includes('chat')) || orgs[0];
    if (!chat || typeof chat.uuid !== 'string' || !ID_RE.test(chat.uuid)) {
      throw new OpError('endpoint_changed', 'unexpected organization id');
    }
    claudeOrg = chat.uuid;
    return claudeOrg;
  }

  const handlers = {
    async 'chatgpt.list'(a) {
      const auth = await chatgptAuth();
      return getJSON(`${CHATGPT}/backend-api/conversations?offset=0&limit=${a.count}&order=updated`, auth);
    },
    async 'chatgpt.detail'(a) {
      const auth = await chatgptAuth();
      return getJSON(`${CHATGPT}/backend-api/conversation/${encodeURIComponent(a.id)}`, auth, true);
    },
    async 'chatgpt.file'(a, emit) {
      const auth = await chatgptAuth();
      const id = encodeURIComponent(a.file_id);
      let metaURL;
      if (a.file_id.startsWith('file-')) {
        metaURL = `${CHATGPT}/backend-api/files/${id}/download`;
      } else {
        const q = new URLSearchParams();
        if (a.conversation_id) q.set('conversation_id', a.conversation_id);
        q.set('inline', 'false');
        metaURL = `${CHATGPT}/backend-api/files/download/${id}?${q}`;
      }
      const meta = await getJSON(metaURL, auth, true);
      if (!meta || meta.status !== 'success' || typeof meta.download_url !== 'string') {
        throw new OpError('endpoint_changed', 'unexpected file download answer');
      }
      const dl = allowedDownload(meta.download_url);
      if (!dl) throw new OpError('endpoint_changed', 'download URL on an unexpected host');
      await emitFile(dl.url, dl.session ? auth : { credentials: 'omit' }, emit);
      return undefined;
    },
    async 'claudeai.list'(a) {
      const org = await claudeOrgId();
      const list = await getJSON(`${CLAUDE}/api/organizations/${org}/chat_conversations?limit=${a.count}`, {});
      return Array.isArray(list) ? list.slice(0, a.count) : list;
    },
    async 'claudeai.detail'(a) {
      const org = await claudeOrgId();
      const id = encodeURIComponent(a.id);
      return getJSON(`${CLAUDE}/api/organizations/${org}/chat_conversations/${id}?tree=True&rendering_mode=messages&render_all_tools=true`, {}, true);
    },
    async 'claudeai.file'(a, emit) {
      const org = await claudeOrgId();
      await emitFile(`${CLAUDE}/api/${org}/files/${encodeURIComponent(a.file_id)}/preview`, { credentials: 'include' }, emit);
      return undefined;
    },
    // The session check runs first, so a logged-out browser never gets a
    // tab and never sends anonymously.
    async 'chatgpt.send'(a) {
      if (!sender) throw new OpError('unsupported', 'this extension build cannot send');
      await chatgptAuth();
      return sender.send('chatgpt', a);
    },
    async 'claudeai.send'(a) {
      if (!sender) throw new OpError('unsupported', 'this extension build cannot send');
      await claudeOrgId();
      return sender.send('claudeai', a);
    },
    // close touches only tabs a send opened and left open.
    async 'chatgpt.close'(a) {
      if (!sender) throw new OpError('unsupported', 'this extension build cannot send');
      return sender.close('chatgpt', a.conversation_id);
    },
    async 'claudeai.close'(a) {
      if (!sender) throw new OpError('unsupported', 'this extension build cannot send');
      return sender.close('claudeai', a.conversation_id);
    },
    // The answer goes out first; the reload follows a moment later, or
    // once no send has a tab open (see RELOAD_MAX_WAIT_MS).
    async 'extension.reload'(_a, emit) {
      if (!reload) throw new OpError('unsupported', 'reload is not available');
      emit({ ok: true, result: { reloading: true } });
      scheduleReload();
      return undefined;
    },
  };

  return {
    // run executes one validated operation, calling emit with each
    // response frame (without the request id). It throws OpError.
    async run(op, args, emit) {
      if (!Object.hasOwn(handlers, op)) throw bad('unknown operation');
      try {
        const result = await handlers[op](args, emit);
        if (result !== undefined) emit({ ok: true, result });
      } catch (e) {
        if (e instanceof OpError && e.code === 'not_logged_in') claudeOrg = null;
        throw e;
      }
    },
  };
}
