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
// Every operation but close first checks that Chrome has granted the
// extension its site's page origins (SITE_ACCESS) and fails permission_missing
// without a tab or a fetch when it has not. The hello lists the granted
// sites, and the worker says hello again whenever a grant changes.
//
// ChatGPT's access token is read from /api/auth/session inside this worker
// and used only for the Authorization header of the next requests. It is
// never returned, logged or stored.
//
// Gemini has no JSON API of its own: the worker fetches the app page
// (/app) for its per-session values (SNlM0e, cfb2h, FdrFJe), keeps them in
// memory for a few minutes, and calls the app's batchexecute endpoint with
// two fixed rpcids, MaZiqc (list, a page at a time) and hNvQHb (read one
// conversation). The decoded inner payloads go back as data; the Go side
// reads their positions. Gemini conversation ids travel as the URL's hex;
// the "c_" batchexecute wants is added here only. Images are fetched by
// gemini.file, which never takes a URL: it reads the conversation again
// and picks the image by response and position (see geminiImageURL), then
// asks the sender to capture it in the tab its send left open, and falls
// back to fetching it here.
//
// Perplexity (www.perplexity.ai) fronts a web agent only: there is no
// list or file operation. perplexity.detail reads one thread with GET
// /rest/thread/<slug>, a page at a time, and returns only the entry fields
// the Go side reads (never the thread's read_write_token). Perplexity
// answers signed-out visitors too, so perplexity.send first asks
// /api/auth/session for a signed-in user and opens no tab without one.
//
// Copilot (copilot.com): a conversation is its page route's JSON (Accept:
// application/json, the owner's cookies, no token), cut down here to the
// fields the Go side reads (copilotConversation). Its chat list has no
// such JSON, so the sender reads it from the page's sidebar in a tab of
// its own.
//
// OpenAI dots (chatgpt.com, under ChatGPT's grant): a dot's DM is read
// from three backend endpoints with the same bearer chatgptAuth reads:
// the dot record by thread (its room and whether it is paused), the room
// (its creator is the owner) and the room's feed (dotRead). dots.send
// refuses a paused dot before any tab opens, and confirms its message by
// polling the feed for it (dotHooks), since the DM page's address never
// changes.

export const NATIVE_HOST = 'com.agenttincan.history';
export const MAX_COUNT = 100;
// No site has live input acceptance yet. Output support is independent.
export const IMAGE_INPUT_SITES = Object.freeze([]);
export const MAX_FILE_BYTES = 10 * 1024 * 1024;
// A multiple of 3 so every chunk is whole base64; 512 KiB encoded per
// message.
export const CHUNK_BYTES = 384 * 1024;
// MAX_MESSAGE_BYTES caps a send op's message (UTF-8 bytes).
export const MAX_MESSAGE_BYTES = 32 * 1024;
// EXTENSION_FILES are the files the hello message reports hashes of, so the
// native host can tell when the unpacked files on disk have changed.
// Gemini: list page size, the most pages one list reads, and how long
// the app page's session values are reused before it is fetched again.
export const GEMINI_PAGE_SIZE = 13;
export const GEMINI_MAX_PAGES = 10;
export const GEMINI_SESSION_MS = 10 * 60 * 1000;
// GEMINI_IMAGE_PREFIX is where Gemini's images are served from; an image
// URL anywhere else is never fetched.
export const GEMINI_IMAGE_PREFIX = 'https://lh3.googleusercontent.com/';
export const EXTENSION_FILES = Object.freeze(['manifest.json', 'background.js', 'ops.js', 'send.js', 'options.html', 'options.js', 'icon16.png', 'icon48.png', 'icon128.png']);

// SITE_ACCESS is each site's host access, keyed by its op prefix: origins
// are all the origins its operations fetch and open tabs on (what the
// options page asks Chrome for), and pageOrigins the site's own pages,
// which must be granted before any of its operations runs. An origin in
// origins but not pageOrigins (ChatGPT's file host) is needed only by the
// fetches that reach it, which fail without it as any fetch to an
// ungranted host does, so withholding it leaves list and read working.
// A required site's origins
// are the manifest's host_permissions, granted at install (the owner can
// still withhold them in Chrome's site access settings); any other site's
// are optional_host_permissions, granted from the options page, so adding
// a site never disables an existing install until the owner accepts it.
export const SITE_ACCESS = Object.freeze({
  chatgpt: Object.freeze({ label: 'ChatGPT', origins: Object.freeze(['https://chatgpt.com/*', 'https://*.oaiusercontent.com/*']), pageOrigins: Object.freeze(['https://chatgpt.com/*']), required: true }),
  claudeai: Object.freeze({ label: 'claude.ai', origins: Object.freeze(['https://claude.ai/*']), pageOrigins: Object.freeze(['https://claude.ai/*']), required: true }),
  grok: Object.freeze({ label: 'Grok', origins: Object.freeze(['https://grok.com/*', 'https://assets.grok.com/*']), pageOrigins: Object.freeze(['https://grok.com/*']), required: false }),
  gemini: Object.freeze({ label: 'Gemini', origins: Object.freeze(['https://gemini.google.com/*', 'https://lh3.googleusercontent.com/*']), pageOrigins: Object.freeze(['https://gemini.google.com/*']), required: false }),
  perplexity: Object.freeze({ label: 'Perplexity', origins: Object.freeze(['https://www.perplexity.ai/*']), pageOrigins: Object.freeze(['https://www.perplexity.ai/*']), required: false }),
  // copilot.microsoft.com is where Copilot starts; it sends the browser on
  // to copilot.com, where the extension's tabs open.
  copilot: Object.freeze({ label: 'Copilot', origins: Object.freeze(['https://copilot.com/*', 'https://copilot.microsoft.com/*']), pageOrigins: Object.freeze(['https://copilot.com/*']), required: false }),
});

// SITE_ALIASES are op prefixes that run on another site's pages and so
// share its grant: an OpenAI dot's DM lives on chatgpt.com. The options
// page and the hello list only SITE_ACCESS keys, so the host checks an
// alias's grant under its site's key (dots under chatgpt).
export const SITE_ALIASES = Object.freeze({ dots: 'chatgpt' });

// siteGranted reports whether permissions (chrome.permissions) holds
// site's page origins, what its operations need, or with all set every
// one of its origins (the options page's full grant); an API failure
// counts as not granted.
export async function siteGranted(permissions, site, { all = false } = {}) {
  const s = SITE_ACCESS[site];
  if (!s) return false;
  try {
    return (await permissions.contains({ origins: [...(all ? s.origins : s.pageOrigins)] })) === true;
  } catch {
    return false;
  }
}

// grantedSites lists the SITE_ACCESS keys whose page origins are granted.
export async function grantedSites(permissions) {
  const out = [];
  for (const site of Object.keys(SITE_ACCESS)) {
    if (await siteGranted(permissions, site)) out.push(site);
  }
  return out;
}

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
const GROK = 'https://grok.com';
const GROK_ASSETS = 'https://assets.grok.com/';
// grok.list reads at most GROK_MAX_PAGES pages of the conversation list.
export const GROK_MAX_PAGES = 5;
// A grok.file id is <response id>_<index in generatedImageUrls>.
const GROK_FILE_RE = /^([A-Za-z0-9][A-Za-z0-9-]{0,99})_(0|[1-9][0-9]?)$/;
const GEMINI = 'https://gemini.google.com';
// GEMINI_ID_RE is a Gemini conversation id as its URL shows it.
const GEMINI_ID_RE = /^[0-9a-f]{8,64}$/;
const PERPLEXITY = 'https://www.perplexity.ai';
// perplexity.detail reads PERPLEXITY_PAGE_SIZE entries a page and at most
// PERPLEXITY_MAX_PAGES pages of one thread.
export const PERPLEXITY_PAGE_SIZE = 10;
export const PERPLEXITY_MAX_PAGES = 20;
// PERPLEXITY_ENTRY_FIELDS are the thread entry fields perplexity.detail
// returns; everything else in an entry (its read_write_token among them)
// stays here.
export const PERPLEXITY_ENTRY_FIELDS = Object.freeze(['uuid', 'backend_uuid', 'status', 'query_str', 'thread_title', 'thread_url_slug', 'text', 'entry_created_datetime', 'entry_updated_datetime', 'updated_datetime', 'blocks']);

// perplexityEntry keeps only PERPLEXITY_ENTRY_FIELDS of a thread entry.
export function perplexityEntry(e) {
  if (!isPlainObject(e)) throw new OpError('endpoint_changed', 'unexpected thread entry');
  const out = {};
  for (const k of PERPLEXITY_ENTRY_FIELDS) {
    if (Object.hasOwn(e, k)) out[k] = e[k];
  }
  return out;
}

// COPILOT_ID_RE is a Copilot conversation id: the UUID in its URL.
const COPILOT_ID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const COPILOT = 'https://copilot.com';
// COPILOT_MAX_SOURCES caps the sources kept per Copilot message.
const COPILOT_MAX_SOURCES = 50;
// DOT_FEED_LIMIT is how many of the latest feed messages dots.detail
// reads, DOT_CONFIRM_LIMIT how many a send's confirmation polls read.
// DOT_SKEW_MS is how much earlier than the send the server's created_at
// may say, for clock skew.
export const DOT_FEED_LIMIT = 32;
const DOT_CONFIRM_LIMIT = 20;
const DOT_SKEW_MS = 2 * 60 * 1000;
// DOT_ROOM_TTL_MS is how long dots.detail reuses a thread's dot record and
// room (so its paused flag is at most this stale; dots.send reads fresh).
const DOT_ROOM_TTL_MS = 5 * 60 * 1000;
// DOT_ROOM_RE is a messaging room id as it goes into a URL path.
const DOT_ROOM_RE = /^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/;

// Argument kinds: 'count' is a required integer 1..MAX_COUNT, 'id' a
// required id, 'id?' an optional id, 'bool?' an optional boolean and
// 'message' a required non-blank string of at most MAX_MESSAGE_BYTES.
const SEND_SPEC = Object.freeze({ message: 'message', conversation_id: 'id?', new_chat: 'bool?' });
const CLOSE_SPEC = Object.freeze({ conversation_id: 'id' });
const SPEC = Object.freeze({
  ...Object.fromEntries(['chatgpt', 'claudeai', 'grok', 'gemini', 'perplexity', 'copilot', 'dots'].map(site => [site + '.session', Object.freeze({})])),
  'chatgpt.list': Object.freeze({ count: 'count' }),
  'chatgpt.detail': Object.freeze({ id: 'id' }),
  'chatgpt.file': Object.freeze({ file_id: 'id', conversation_id: 'id?' }),
  'claudeai.list': Object.freeze({ count: 'count' }),
  'claudeai.detail': Object.freeze({ id: 'id' }),
  'claudeai.file': Object.freeze({ file_id: 'id' }),
  'grok.list': Object.freeze({ count: 'count' }),
  'grok.detail': Object.freeze({ id: 'id' }),
  'grok.file': Object.freeze({ file_id: 'id', conversation_id: 'id' }),
  'chatgpt.send': SEND_SPEC,
  'claudeai.send': SEND_SPEC,
  'grok.send': SEND_SPEC,
  'chatgpt.close': CLOSE_SPEC,
  'claudeai.close': CLOSE_SPEC,
  'grok.close': CLOSE_SPEC,
  'gemini.list': Object.freeze({ count: 'count' }),
  'gemini.detail': Object.freeze({ id: 'id' }),
  'gemini.file': Object.freeze({ file_id: 'id', conversation_id: 'id' }),
  'gemini.send': SEND_SPEC,
  'gemini.close': CLOSE_SPEC,
  'perplexity.detail': Object.freeze({ id: 'id' }),
  'perplexity.send': SEND_SPEC,
  'perplexity.close': CLOSE_SPEC,
  'copilot.list': Object.freeze({ count: 'count' }),
  'copilot.detail': Object.freeze({ id: 'id' }),
  'copilot.send': SEND_SPEC,
  'copilot.close': CLOSE_SPEC,
  'dots.detail': Object.freeze({ id: 'id' }),
  'dots.send': SEND_SPEC,
  'dots.close': CLOSE_SPEC,
  'extension.reload': Object.freeze({}),
});

const INPUT_OPS = Object.freeze(['chatgpt.input_begin', 'chatgpt.input_chunk', 'chatgpt.input_abort', 'chatgpt.send_images']);
export const INPUT_CHUNK_BYTES = 96 * 1024;
const INPUT_TOTAL_BYTES = 20 * 1024 * 1024;

function inputFiles(files) {
  if (!Array.isArray(files) || files.length < 1 || files.length > 4) throw bad('expected one to four images');
  let total = 0;
  for (const f of files) {
    if (!isPlainObject(f) || Object.keys(f).sort().join(',') !== 'mime,name,sha256,size') throw bad('invalid image metadata');
    if (typeof f.name !== 'string' || new TextEncoder().encode(f.name).length > 255 || !f.name || /[\x00-\x1f\x7f/\\]/.test(f.name)) throw bad('invalid image name');
    if (!['image/png', 'image/jpeg'].includes(f.mime)) throw bad('expected PNG or JPEG');
    if (!Number.isSafeInteger(f.size) || f.size < 1 || f.size > MAX_FILE_BYTES) throw bad('image exceeds 10 MiB');
    if (typeof f.sha256 !== 'string' || !/^[a-f0-9]{64}$/.test(f.sha256)) throw bad('invalid image checksum');
    total += f.size;
  }
  if (total > INPUT_TOTAL_BYTES) throw bad('images exceed 20 MiB');
  return total;
}

function validateInput(op, input) {
  if (!isPlainObject(input)) throw bad('missing image payload');
  const verb = op.split('.')[1];
  const allowed = verb === 'input_begin' ? ['connection', 'files'] : verb === 'input_chunk' ? ['connection', 'token', 'seq', 'data'] : ['connection', 'token'];
  if (Object.keys(input).some((k) => !allowed.includes(k))) throw bad('unexpected image argument');
  if (typeof input.connection !== 'string' || !ID_RE.test(input.connection)) throw bad('invalid connection');
  if (verb === 'input_begin') inputFiles(input.files);
  else if (!(verb === 'input_abort' && input.token === undefined) && (typeof input.token !== 'string' || !/^[a-f0-9]{32}$/.test(input.token))) throw bad('invalid transfer token');
  if (verb === 'input_chunk') {
    if (input.seq === undefined) input = { ...input, seq: 0 };
    if (!Number.isSafeInteger(input.seq) || input.seq < 0) throw bad('invalid chunk sequence');
    if (typeof input.data !== 'string' || !input.data || input.data.length > 4 * Math.ceil(INPUT_CHUNK_BYTES / 3) || !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(input.data)) throw bad('invalid chunk encoding');
  }
  return input;
}

// Bytes live only in this worker, reserved before allocation, with no URLs,
// file paths or page selectors accepted from the native host.
export function createInputTransfers({ now = Date.now } = {}) {
  const pending = new Map();
  function sweep() {
    for (const [token, p] of pending) if (!p.consumed && (now() - p.touched >= 120000 || now() - p.created >= 600000)) { p.cancelled = true; pending.delete(token); }
  }
  function cancel(token, p) {
    p.cancelled = true;
    p.bytes = null;
    if (p.outputFiles) p.outputFiles.length = 0;
    // A queued/active send may still hold serialized arguments. Keep its
    // reservation until its finally block runs, even across reconnect.
    if (!p.consumed) pending.delete(token);
  }
  function get(connection, site, token) {
    sweep();
    const p = pending.get(token);
    if (!p || p.consumed || p.connection !== connection || p.site !== site) throw bad('unknown image transfer');
    p.touched = now();
    return p;
  }
  return {
    begin(connection, site, files) {
      sweep();
      const size = inputFiles(files);
      const used = [...pending.values()].reduce((n, p) => n + p.size, 0);
      if (pending.size >= 4 || used + size > 40 * 1024 * 1024) throw bad('image transfer capacity reached');
      const token = Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) => b.toString(16).padStart(2, '0')).join('');
      pending.set(token, { connection, site, files: files.map((f) => ({ ...f })), size, bytes: new Uint8Array(size), offset: 0, seq: 0, created: now(), touched: now() });
      return { token };
    },
    chunk(connection, site, a) {
      validateInput(`${site}.input_chunk`, { ...a, connection });
      const p = get(connection, site, a.token);
      if (a.seq !== p.seq) throw bad('image chunk out of order');
      const raw = atob(a.data);
      if (btoa(raw) !== a.data || raw.length > INPUT_CHUNK_BYTES || p.offset + raw.length > p.size) throw bad('image chunk exceeds size or is not canonical base64');
      p.bytes.set(Uint8Array.from(raw, (c) => c.charCodeAt(0)), p.offset);
      p.offset += raw.length;
      p.seq++;
      return { seq: a.seq };
    },
    async consume(connection, site, token) {
      const p = get(connection, site, token);
      p.consumed = true; // retained reservation bounds queued and active sends
      try {
        if (p.offset !== p.size) throw bad('incomplete image transfer');
        const files = [];
        p.outputFiles = files;
        let offset = 0;
        for (const f of p.files) {
          const bytes = p.bytes.subarray(offset, offset + f.size);
          offset += f.size;
          const hash = await sha256Hex(bytes);
          if (p.cancelled) throw bad('image transfer disconnected');
          if (hash !== f.sha256) throw bad('image checksum mismatch');
          let bin = '';
          for (let i = 0; i < bytes.length; i += 8192) bin += String.fromCharCode(...bytes.subarray(i, i + 8192));
          files.push({ ...f, data: btoa(bin) });
        }
        p.bytes = null;
        return files;
      } catch (e) {
        pending.delete(token);
        throw e;
      }
    },
    active(token) { const p = pending.get(token); return !!p && !p.cancelled; },
    release(connection, site, token) {
      const p = pending.get(token);
      if (p?.connection === connection && p.site === site) { p.cancelled = true; pending.delete(token); }
    },
    abort(connection, site, token) {
      for (const [key, p] of pending) if (p.connection === connection && p.site === site && (token === undefined || token === key)) cancel(key, p);
      return { aborted: true };
    },
    busy() { sweep(); return pending.size > 0; },
    clear() { for (const [token, p] of pending) cancel(token, p); },
  };
}

export const OPS = new Set([...Object.keys(SPEC), ...INPUT_OPS]);

// MAX_RETRY_AFTER_S caps a Retry-After reported to the host, in seconds.
export const MAX_RETRY_AFTER_S = 3600;

export class OpError extends Error {
  // retryAfter, for rate_limited, is the site's Retry-After in whole
  // seconds when it sent one. clicked, set by a send, means the send
  // button had been clicked before the failure, so the message may have
  // been sent.
  constructor(code, message, retryAfter) {
    super(message);
    this.code = code;
    if (Number.isSafeInteger(retryAfter) && retryAfter >= 0) this.retryAfter = retryAfter;
  }
}

// errorFrame is the failure frame for an error thrown by an operation:
// its code and message (retry_after too for a rate limit, clicked for a
// send that failed after its click), or a bare
// internal error for anything that is not an OpError, so no unexpected
// detail leaves the extension.
export function errorFrame(e) {
  if (!(e instanceof OpError)) return { ok: false, error: { code: 'internal', message: 'internal error' } };
  const error = { code: e.code, message: e.message };
  if (e.retryAfter !== undefined) error.retry_after = e.retryAfter;
  if (e.clicked === true) error.clicked = true;
  if (e.uploaded === true) error.uploaded = true;
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
    if (k !== 'id' && k !== 'op' && k !== 'args' && k !== 'input') throw bad('unexpected field');
  }
  const { id, op, args } = msg;
  if (!Number.isSafeInteger(id) || id < 0) throw bad('invalid request id');
  if (INPUT_OPS.includes(op)) {
    const input = validateInput(op, msg.input);
    const base = validate({ id, op: op.endsWith('.send_images') ? 'chatgpt.send' : 'extension.reload', args });
    return { ...base, op, input };
  }
  if (msg.input !== undefined) throw bad('unexpected image payload');
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

// helloMessage is what the worker tells the native host when it connects
// and when a site grant changes: its version, whether it is unpacked (only
// an unpacked extension picks up new files on reload), the sha256 of each
// of its files as Chrome loaded them (files when given, hashFiles from
// worker start, else hashed now), and, given permissions, the sites whose
// access Chrome has granted.
export async function helloMessage({ manifest, getURL, fetch, files, permissions }) {
  const hashes = files && typeof files === 'object' ? files : await hashFiles({ getURL, fetch });
  const m = manifest && typeof manifest === 'object' ? manifest : {};
  const hello = { version: typeof m.version === 'string' ? m.version : '', unpacked: !('update_url' in m), files: hashes };
  if (permissions) hello.granted = await grantedSites(permissions);
  hello.image_input = { version: 1, sites: [...IMAGE_INPUT_SITES] };
  return { id: 0, hello };
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

// finalURL is where the response came from after redirects, url when
// the response does not say.
function finalURL(res, url) {
  try {
    return new URL(res.url || url);
  } catch {
    return null;
  }
}

// ANTI_BOT_TEXT marks a challenge or anti-bot refusal in a body: a
// Cloudflare interstitial ("Just a moment...") or a JSON refusal naming
// anti-bot rules or a captcha.
const ANTI_BOT_TEXT = /just a moment\.\.\.|cf-challenge|challenge-platform|anti-?bot|captcha/i;

// BODY_TEXT_BYTES caps how much of a body bodyText reads, and
// BODY_TEXT_MS how long it waits for it: a body that sends a little and
// then stalls is judged on what arrived, so the error reply is not held
// until the request times out.
const BODY_TEXT_BYTES = 64 * 1024;
export const BODY_TEXT_MS = 2000;

// bodyText reads at most BODY_TEXT_BYTES of a copy of res's body, for at
// most ms milliseconds, '' when it cannot. It streams the copy and stops
// at either cap, so a large or stalled page is never waited on whole.
async function bodyText(res, ms = BODY_TEXT_MS) {
  let reader;
  let timer;
  try {
    const body = res.clone().body;
    if (!body) return '';
    reader = body.getReader();
    const expired = new Promise((resolve) => {
      timer = setTimeout(() => resolve({ done: true, value: undefined }), ms);
    });
    const parts = [];
    let total = 0;
    while (total < BODY_TEXT_BYTES) {
      const { done, value } = await Promise.race([reader.read(), expired]);
      if (done) break;
      const part = value.subarray(0, BODY_TEXT_BYTES - total);
      parts.push(part);
      total += part.length;
    }
    const bytes = new Uint8Array(total);
    let off = 0;
    for (const part of parts) {
      bytes.set(part, off);
      off += part.length;
    }
    return new TextDecoder().decode(bytes);
  } catch {
    return '';
  } finally {
    clearTimeout(timer);
    if (reader) reader.cancel().catch(() => {});
  }
}

// antiBot reports whether res is an anti-bot page rather than the site's
// answer: Cloudflare's cf-mitigated header, a redirect to Google's
// /sorry/ interstitial, or (when body is read) a challenge marker.
function antiBot(res, url, body) {
  if ((res.headers.get('cf-mitigated') || '') !== '') return true;
  const u = finalURL(res, url);
  if (u && u.pathname.startsWith('/sorry/')) return true;
  return body !== undefined && ANTI_BOT_TEXT.test(body);
}

// check maps an HTTP status to an error class. notFound marks endpoints
// where 404 means the item is gone rather than the API moved. Anti-bot
// pages are blocked whatever their status, except that a 401 stays
// not_logged_in.
async function check(res, url, notFound, sniffMs = BODY_TEXT_MS) {
  const where = `HTTP ${res.status} from ${pathOf(url)}`;
  if (res.status !== 401 && antiBot(res, url)) throw new OpError('blocked', `anti-bot check (${where})`);
  if (res.ok) return;
  if (res.status === 401) throw new OpError('not_logged_in', where);
  if (res.status === 403) {
    if (contentType(res) === 'text/html') throw new OpError('blocked', where);
    if (antiBot(res, url, await bodyText(res, sniffMs))) throw new OpError('blocked', `anti-bot check (${where})`);
    throw new OpError('not_logged_in', where);
  }
  if (res.status === 404 || res.status === 410) throw new OpError(notFound ? 'not_found' : 'endpoint_changed', where);
  if (res.status === 429) throw new OpError('rate_limited', where, retryAfterSeconds(res));
  throw new OpError('http_error', where);
}

// copilotId checks a Copilot conversation id (the URL's UUID).
function copilotId(id) {
  if (typeof id !== 'string' || !COPILOT_ID_RE.test(id)) throw bad('invalid Copilot conversation id');
  return id;
}

// copilotConversation keeps only what the Go side reads from a Copilot
// conversation page's JSON: the id, title and times, and each user or bot
// message's id, author, text, time and source links. Everything else in
// the answer (reconnectToken, the telemetry and state blobs, which carry
// token-like strings) is dropped here and never returned or logged.
// Messages with a messageType (tool context, search queries, suggestions)
// and other authors are not content and are left out.
export function copilotConversation(raw, id) {
  const r = raw && typeof raw === 'object' && raw.store && typeof raw.store === 'object' ? raw.store.rawConversationResponse : null;
  if (!r || typeof r !== 'object' || !Array.isArray(r.messages)) throw new OpError('endpoint_changed', 'unexpected conversation answer');
  const text = (v, n) => (typeof v === 'string' ? v.slice(0, n) : '');
  const messages = [];
  for (const m of r.messages) {
    if (!m || typeof m !== 'object' || (m.messageType !== undefined && m.messageType !== null)) continue;
    if (m.author !== 'user' && m.author !== 'bot') continue;
    const sources = [];
    const seen = new Set();
    for (const s of Array.isArray(m.sourceAttributions) ? m.sourceAttributions : []) {
      const url = text(s && s.seeMoreUrl, 2048);
      if (!/^https?:\/\//i.test(url) || seen.has(url)) continue;
      seen.add(url);
      sources.push({ title: text(s.providerDisplayName, 300), url });
      if (sources.length >= COPILOT_MAX_SOURCES) break;
    }
    messages.push({
      id: typeof m.messageId === 'string' && ID_RE.test(m.messageId) ? m.messageId : '',
      author: m.author,
      text: text(m.text, 512 * 1024),
      createdAt: text(m.createdAt, 64) || text(m.timestamp, 64),
      sources,
    });
  }
  const cid = typeof r.conversationId === 'string' ? r.conversationId.toLowerCase() : '';
  return {
    conversationId: COPILOT_ID_RE.test(cid) ? cid : id,
    title: text(r.chatName, 300),
    createdAt: text(r.createTimeUtc, 64),
    updatedAt: text(r.updateTimeUtc, 64),
    messages,
  };
}

// dotItems keeps what the Go side reads of a dot room's feed messages:
// id, time, sender, text and how many attachments. A deleted message is
// left out. A message without its id, time or sender is endpoint_changed.
function dotItems(r) {
  if (!isPlainObject(r) || !Array.isArray(r.items)) throw new OpError('endpoint_changed', 'unexpected room messages answer');
  const out = [];
  for (const m of r.items) {
    if (!isPlainObject(m) || typeof m.id !== 'string' || m.id === '' || typeof m.created_at !== 'string' || typeof m.account_user_id !== 'string' || m.account_user_id === '') {
      throw new OpError('endpoint_changed', 'unexpected room message');
    }
    if (m.deleted_at !== undefined && m.deleted_at !== null) continue;
    const c = isPlainObject(m.content) ? m.content : {};
    out.push({
      id: m.id.slice(0, 128),
      at: m.created_at.slice(0, 64),
      from: m.account_user_id.slice(0, 128),
      text: typeof c.text === 'string' ? c.text.slice(0, 512 * 1024) : '',
      attachments: Array.isArray(c.attachments) ? c.attachments.length : 0,
    });
  }
  return out;
}

// sameText compares a sent message with a feed message's text, ignoring
// how whitespace was laid out (the editor turns blank lines into
// paragraphs).
function sameText(a, b) {
  const norm = (s) => String(s).replace(/\s+/g, ' ').trim();
  return norm(a) === norm(b);
}

// geminiId checks a Gemini conversation id (the URL's hex).
function geminiId(id) {
  if (typeof id !== 'string' || !GEMINI_ID_RE.test(id)) throw bad('invalid Gemini conversation id');
  return id;
}

// GEMINI_NOT_FOUND is the error code batchexecute puts in place of an
// hNvQHb payload when the conversation is missing or deleted (live, with
// a BardErrorInfo detail): ["wrb.fr","hNvQHb",null,null,null,[5,...]].
const GEMINI_NOT_FOUND = 5;

// parseBatchexecute reads a batchexecute answer: a ")]}'" guard, then
// length-prefixed chunks, one of which holds [["wrb.fr", rpcid,
// "<inner JSON>", ...]], and returns the decoded inner payload. When the
// site answered with no payload it puts an error code at [5][0] instead
// (3 for a payload it could not take): code 5 on hNvQHb is not_found, and
// anything else (another code, no code, no answer for the rpcid) is
// endpoint_changed. A null payload is never an answer.
export function parseBatchexecute(text, rpcid) {
  if (typeof text !== 'string') throw new OpError('endpoint_changed', `no ${rpcid} answer`);
  const body = text.replace(/^\)\]\}'\s*/, '');
  for (const line of body.split('\n')) {
    const t = line.trim();
    if (!t.startsWith('[')) continue;
    let v;
    try {
      v = JSON.parse(t);
    } catch {
      continue;
    }
    if (!Array.isArray(v)) continue;
    for (const e of v) {
      if (!Array.isArray(e) || e[0] !== 'wrb.fr' || e[1] !== rpcid) continue;
      if (e[2] === null || e[2] === undefined) {
        const code = Array.isArray(e[5]) && Number.isInteger(e[5][0]) ? e[5][0] : null;
        if (code === GEMINI_NOT_FOUND && rpcid === 'hNvQHb') throw new OpError('not_found', 'conversation not found');
        throw new OpError('endpoint_changed', code === null ? `no ${rpcid} payload` : `${rpcid} error ${code}`);
      }
      if (typeof e[2] !== 'string') throw new OpError('endpoint_changed', `unexpected ${rpcid} answer`);
      try {
        return JSON.parse(e[2]);
      } catch {
        throw new OpError('endpoint_changed', `malformed ${rpcid} payload`);
      }
    }
  }
  throw new OpError('endpoint_changed', `no ${rpcid} answer`);
}

// geminiImageURLs lists a response candidate's image URLs the way the Go
// side counts them: every string in its arrays, outside its text at
// position 1, that starts with GEMINI_IMAGE_PREFIX, first occurrence
// first, depth first.
export function geminiImageURLs(cand) {
  const out = [];
  const walk = (v) => {
    if (typeof v === 'string') {
      if (v.startsWith(GEMINI_IMAGE_PREFIX) && !out.includes(v)) out.push(v);
    } else if (Array.isArray(v)) {
      for (const e of v) walk(e);
    }
  };
  if (Array.isArray(cand)) cand.forEach((e, i) => i !== 1 && walk(e));
  return out;
}

// geminiImageURL finds image n of response candidate rc in an hNvQHb
// payload (turns at [0], each turn's candidates at [3][0], a candidate's
// id at [0]), and returns it only when it is a plain https URL on the
// image host. Anything else is null.
export function geminiImageURL(inner, rc, n) {
  const turns = Array.isArray(inner) && Array.isArray(inner[0]) ? inner[0] : [];
  for (const t of turns) {
    const cands = Array.isArray(t) && Array.isArray(t[3]) && Array.isArray(t[3][0]) ? t[3][0] : [];
    for (const c of cands) {
      if (!Array.isArray(c) || c[0] !== rc) continue;
      const raw = geminiImageURLs(c)[n];
      if (!raw) return null;
      let u;
      try {
        u = new URL(raw);
      } catch {
        return null;
      }
      if (u.protocol !== 'https:' || u.hostname !== 'lh3.googleusercontent.com' || u.username || u.password || u.port) return null;
      return u.href;
    }
  }
  return null;
}

// decodeImage checks a captured image (base64 from a page) and returns its
// bytes, or null.
export function decodeImage(r) {
  if (!r || r.ok !== true || typeof r.mime !== 'string' || !r.mime.startsWith('image/') || typeof r.data !== 'string') return null;
  if (r.data.length > Math.ceil(MAX_FILE_BYTES / 3) * 4 + 4) return null;
  let bin;
  try {
    bin = atob(r.data);
  } catch {
    return null;
  }
  if (bin.length === 0 || bin.length > MAX_FILE_BYTES) return null;
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return { bytes, mime: r.mime.split(';')[0].trim().toLowerCase() };
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
//
// permissions (chrome.permissions) is asked before each operation whether
// its site is granted; without it every site counts as granted. sniffMs
// bounds how long an error or non-JSON body is read for anti-bot markers
// (BODY_TEXT_MS unless a test shortens it).
export function createRunner({ fetch, sender = null, reload = null, permissions = null, sniffMs = BODY_TEXT_MS }) {
  const transfers = createInputTransfers();
  let inputEpoch = 0;
  let claudeOrg = null;
  let geminiSession = null;
  // geminiReq is batchexecute's _reqid: a counter, as the app keeps one.
  let geminiReq = Math.floor(Math.random() * 9000) + 1000;
  let reloadPending = false;
  // dotRooms caches dots.detail's dotRoom answer per thread:
  // { room, owner, paused, at }.
  const dotRooms = new Map();

  // scheduleReload reloads once the sender is idle, or at the cap after
  // closing its kept tabs. Timers are looked up at call time so tests can
  // mock them.
  function scheduleReload() {
    if (reloadPending) return;
    reloadPending = true;
    const start = Date.now();
    const attempt = async () => {
      const busy = transfers.busy() || (sender && typeof sender.busy === 'function' && sender.busy());
      if (busy) {
        if (Date.now() - start < RELOAD_MAX_WAIT_MS) {
          setTimeout(attempt, RELOAD_RETRY_MS);
          return;
        }
        try {
          if (sender && typeof sender.closeAllKept === 'function') await sender.closeAllKept();
        } catch {
          // Reload regardless.
        }
      }
      transfers.clear();
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

  // getJSON fetches one of a site's JSON endpoints. An answer that came
  // from another host means the site sent the browser to a sign-in page
  // (the session probes run first, so a logged-out browser never sends).
  async function getJSON(url, init, notFound = false) {
    const res = await send(url, { credentials: 'include', ...init });
    await check(res, url, notFound, sniffMs);
    const at = finalURL(res, url);
    if (at && at.host !== new URL(url).host) throw new OpError('not_logged_in', `redirected to ${at.host}`);
    if (!contentType(res).includes('json')) {
      if (antiBot(res, url, await bodyText(res, sniffMs))) throw new OpError('blocked', `anti-bot check from ${pathOf(url)}`);
      throw new OpError('endpoint_changed', `non-JSON answer from ${pathOf(url)}`);
    }
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

  // emitFile fetches an image and emits it in chunks. With host set, an
  // answer that ended anywhere else after redirects is refused, as
  // getJSON does.
  async function emitFile(url, init, emit, host = null) {
    const res = await send(url, init);
    await check(res, url, true, sniffMs);
    if (host) {
      const at = finalURL(res, url);
      if (!at || at.host !== host) throw new OpError('not_logged_in', `redirected to ${at ? at.host : 'an unknown host'}`);
    }
    const declared = Number(res.headers.get('content-length') || 0);
    if (declared > MAX_FILE_BYTES) throw new OpError('too_large', `file over ${MAX_FILE_BYTES} bytes`);
    const mime = contentType(res);
    if (!mime.startsWith('image/') && mime !== 'application/octet-stream') {
      throw new OpError('endpoint_changed', `unexpected file type from ${pathOf(url)}`);
    }
    const bytes = await readCapped(res);
    if (bytes.length === 0) throw new OpError('endpoint_changed', 'empty file');
    emitBytes(bytes, mime, emit);
  }

  function emitBytes(bytes, mime, emit) {
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

  // grokJSON is the one place grok.com's JSON is fetched: from this worker,
  // with the owner's cookies and no page-set headers (a live check read all
  // three endpoints without x-statsig-id). If grok.com ever refuses reads
  // from the extension's origin, this is the function to swap for a fixed
  // read in the isolated world of an extension-opened grok.com tab, the way
  // send.js runs its page functions; nothing else changes.
  async function grokJSON(url, init, notFound = false) {
    return getJSON(url, init, notFound);
  }

  function grokPost(url, body) {
    return grokJSON(url, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) }, true);
  }

  // grokSession fails not_logged_in unless the conversation list answers
  // for a signed-in account; grok.com otherwise lets a logged-out page
  // chat anonymously, so the send never opens a tab without it. It is the
  // first of two gates: the assumption is that grok.com refuses the list
  // (401) without a session, but a 200 with an empty list carries no
  // account signal of its own, so it is not taken as proof of a sign-in.
  // The send tab's login probe (the sign-in link or /sign-in address in
  // send.js's grok selectors) is the second gate and runs before anything
  // is typed, so a signed-out page that still answered the list is
  // refused there.
  async function grokSession(requireAccount = false) {
    const r = await grokJSON(`${GROK}/rest/app-chat/conversations?pageSize=1`, {});
    if (!r || !Array.isArray(r.conversations)) throw new OpError('not_logged_in', 'no grok.com session');
    if (requireAccount && r.conversations.length === 0) throw new OpError('indeterminate', 'empty Grok list carries no account signal');
  }

  // grokAsset resolves a generatedImageUrls entry (a path on the image host,
  // or a full URL) and allows only grok.com's own hosts.
  function grokAsset(raw) {
    let u;
    try {
      u = new URL(raw, GROK_ASSETS);
    } catch {
      return null;
    }
    if (u.protocol !== 'https:' || u.username || u.password || u.port) return null;
    return u.hostname === 'assets.grok.com' || u.hostname === 'grok.com' ? u.href : null;
  }

  // copilotJSON is the one place copilot.com's JSON is fetched: from this
  // worker, with the owner's cookies and an Accept header asking the
  // app's page route for its data instead of its HTML. No token is read or
  // sent. If copilot.com ever refuses the worker, this is the function to
  // swap for the same fetch in the isolated world of an extension-opened
  // copilot.com tab.
  async function copilotJSON(url) {
    return getJSON(url, { headers: { accept: 'application/json' } }, true);
  }

  // dotRoom reads the dot record for thread and its room: the room id,
  // the owner (the room's creator) and whether the dot is paused.
  async function dotRecord(auth, thread) {
    const rec = await getJSON(`${CHATGPT}/backend-api/tbo/by-thread/${encodeURIComponent(thread)}`, auth, true);
    if (!isPlainObject(rec) || typeof rec.messaging_room_id !== 'string' || !DOT_ROOM_RE.test(rec.messaging_room_id) || typeof rec.is_paused !== 'boolean') {
      throw new OpError('endpoint_changed', 'unexpected dot record');
    }
    return rec;
  }

  const dotPausedErr = () => new OpError('paused', 'the dot is paused; resume it in ChatGPT, then ask again');

  // dotNotPaused reads the dot record again and fails paused if the dot
  // is paused now, or endpoint_changed if its room is no longer room.
  async function dotNotPaused(auth, thread, room) {
    const rec = await dotRecord(auth, thread);
    if (rec.messaging_room_id !== room) throw new OpError('endpoint_changed', 'the dot record names another room');
    if (rec.is_paused) throw dotPausedErr();
  }

  async function dotRoom(auth, thread) {
    const rec = await dotRecord(auth, thread);
    const room = await getJSON(`${CHATGPT}/backend-api/messaging/rooms/${rec.messaging_room_id}`, auth, true);
    if (!isPlainObject(room) || typeof room.creator_account_user_id !== 'string' || room.creator_account_user_id === '') {
      throw new OpError('endpoint_changed', 'unexpected room answer');
    }
    return { room: rec.messaging_room_id, owner: room.creator_account_user_id.slice(0, 128), paused: rec.is_paused };
  }

  // dotFeed reads the latest limit messages of room, oldest first.
  async function dotFeed(auth, room, limit) {
    return dotItems(await getJSON(`${CHATGPT}/backend-api/messaging/rooms/${room}/messages?limit=${limit}`, auth, true));
  }

  // dotHooks are the sender's feed hooks for one send of message into
  // thread: ready, called in the queued send right before the fill, and
  // before, right before each click, both read the dot record again and
  // fail paused if the dot was paused since dots.send checked it (the send
  // may have waited in the site's queue meanwhile); before also notes the
  // feed's message ids; confirm returns
  // the id of the first owner message not among them whose text is the
  // message and whose time is not before the click (less DOT_SKEW_MS),
  // or null while there is none.
  function dotHooks(auth, thread, d, message) {
    let seen = null;
    let from = 0;
    return {
      async ready() {
        await dotNotPaused(auth, thread, d.room);
      },
      async before() {
        await dotNotPaused(auth, thread, d.room);
        seen = new Set((await dotFeed(auth, d.room, DOT_CONFIRM_LIMIT)).map((m) => m.id));
        from = Date.now() - DOT_SKEW_MS;
      },
      async confirm() {
        for (const m of await dotFeed(auth, d.room, DOT_CONFIRM_LIMIT)) {
          if (m.from !== d.owner || (seen && seen.has(m.id)) || !sameText(m.text, message)) continue;
          const at = Date.parse(m.at);
          if (Number.isNaN(at) || at < from) continue;
          return m.id;
        }
        return null;
      },
    };
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

  // geminiAuth returns the app page's session values: the at token, the
  // build label and the session id. They stay in this closure, are never
  // returned, and are fetched again after GEMINI_SESSION_MS or when
  // fresh is set.
  async function geminiAuth(fresh) {
    if (!fresh && geminiSession && Date.now() - geminiSession.t < GEMINI_SESSION_MS) return geminiSession;
    geminiSession = null;
    const url = `${GEMINI}/app`;
    const res = await send(url, { credentials: 'include' });
    await check(res, url, false);
    const at = finalURL(res, url);
    if (at && at.host !== new URL(url).host) throw new OpError('not_logged_in', `redirected to ${at.host}`);
    let html;
    try {
      html = await res.text();
    } catch {
      throw new OpError('network', 'could not read the Gemini app page');
    }
    const token = /"SNlM0e":"([^"\\]{1,512})"/.exec(html);
    if (!token) throw new OpError('not_logged_in', 'no Gemini session on the app page');
    const bl = /"cfb2h":"([^"\\]{1,256})"/.exec(html);
    if (!bl) throw new OpError('endpoint_changed', 'no build label on the Gemini app page');
    const fsid = /"FdrFJe":"(-?\d{1,32})"/.exec(html);
    geminiSession = { at: token[1], bl: bl[1], fsid: fsid ? fsid[1] : '', t: Date.now() };
    return geminiSession;
  }

  // geminiRPC calls one batchexecute rpcid with payload and returns the
  // decoded inner payload (see parseBatchexecute). A 400 or 401 first
  // fetches the session values again, once.
  async function geminiRPC(rpcid, payload) {
    for (let attempt = 0; ; attempt++) {
      const s = await geminiAuth(attempt > 0);
      const q = new URLSearchParams({ rpcids: rpcid, 'source-path': '/app', bl: s.bl });
      if (s.fsid) q.set('f.sid', s.fsid);
      q.set('hl', 'en');
      q.set('_reqid', String(geminiReq));
      geminiReq += 100000;
      q.set('rt', 'c');
      const url = `${GEMINI}/_/BardChatUi/data/batchexecute?${q}`;
      const body = new URLSearchParams({ 'f.req': JSON.stringify([[[rpcid, JSON.stringify(payload), null, 'generic']]]), at: s.at });
      const res = await send(url, {
        method: 'POST',
        credentials: 'include',
        headers: { 'content-type': 'application/x-www-form-urlencoded;charset=UTF-8' },
        body: body.toString(),
      });
      if ((res.status === 400 || res.status === 401) && attempt === 0) {
        geminiSession = null;
        continue;
      }
      await check(res, url, false);
      const at = finalURL(res, url);
      if (at && at.host !== new URL(url).host) throw new OpError('not_logged_in', `redirected to ${at.host}`);
      let text;
      try {
        text = await res.text();
      } catch {
        throw new OpError('network', `could not read the ${rpcid} answer`);
      }
      return parseBatchexecute(text, rpcid);
    }
  }

  // perplexityJSON is the one place www.perplexity.ai's JSON is fetched:
  // from this worker, with the owner's cookies. If Perplexity ever refuses
  // reads from the extension's origin, this is the function to swap for a
  // fixed read in the isolated world of an extension-opened tab, the way
  // send.js runs its page functions; nothing else changes.
  async function perplexityJSON(url, init, notFound = false) {
    return getJSON(url, init, notFound);
  }

  // perplexitySession fails not_logged_in unless /api/auth/session names a
  // signed-in user (an object with an id); signed out it answers {}.
  // Perplexity lets a signed-out page ask anonymously, so the send never
  // opens a tab without it. The user's fields are only checked, never
  // returned or kept. The send tab's login probe (send.js) is the second
  // gate and runs before anything is typed.
  async function perplexitySession() {
    const s = await perplexityJSON(`${PERPLEXITY}/api/auth/session`, {});
    const user = isPlainObject(s) ? s.user : null;
    if (!isPlainObject(user) || typeof user.id !== 'string' || user.id === '') {
      throw new OpError('not_logged_in', 'no www.perplexity.ai session');
    }
  }

  // perplexityThread reads thread slug a page at a time, oldest entry
  // first, following next_cursor while has_next_page says there is more.
  // Stopping at PERPLEXITY_MAX_PAGES with more left sets more.
  async function perplexityThread(slug) {
    const entries = [];
    let cursor = '';
    let more = false;
    for (let page = 0; page < PERPLEXITY_MAX_PAGES; page++) {
      const q = new URLSearchParams({ with_parent_info: 'true', with_schematized_response: 'true', version: '2.18', source: 'default', limit: String(PERPLEXITY_PAGE_SIZE) });
      if (cursor) {
        q.set('cursor', cursor);
      } else {
        q.set('offset', '0');
        q.set('from_first', 'true');
      }
      const r = await perplexityJSON(`${PERPLEXITY}/rest/thread/${encodeURIComponent(slug)}?${q}`, {}, true);
      if (!isPlainObject(r) || !Array.isArray(r.entries)) throw new OpError('endpoint_changed', 'unexpected thread answer');
      for (const e of r.entries) entries.push(perplexityEntry(e));
      const next = typeof r.next_cursor === 'string' ? r.next_cursor : '';
      // A next page with no usable cursor is still more: the newest
      // entries were not read, so the thread is not reported whole.
      more = r.has_next_page === true;
      if (!more || next === '' || next === cursor) break;
      cursor = next;
    }
    const result = { slug, entries };
    if (more) result.more = true;
    return result;
  }

  // geminiRead reads conversation id (URL hex) with hNvQHb.
  async function geminiRead(id) {
    return geminiRPC('hNvQHb', [`c_${geminiId(id)}`, 10, null, 1, [0], [4], null, 1]);
  }

  const handlers = {
    async 'chatgpt.session'() { await chatgptAuth(); return {}; },
    async 'dots.session'() { await chatgptAuth(); return {}; },
    async 'claudeai.session'() { claudeOrg = null; await claudeOrgId(); return {}; },
    async 'grok.session'() { await grokSession(true); return {}; },
    async 'gemini.session'() { await geminiAuth(true); return {}; },
    async 'perplexity.session'() { await perplexitySession(); return {}; },
    async 'copilot.session'() {
      if (!sender || typeof sender.session !== 'function') throw new OpError('unsupported', 'upgrade the extension for session probes');
      return sender.session('copilot');
    },
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
    // grok.com: the list pages through nextPageToken until count
    // conversations are in hand. Stopping at GROK_MAX_PAGES short of count
    // while grok.com still offers a page sets more, so the reader does not
    // take the short list as complete.
    async 'grok.list'(a) {
      const out = [];
      let token = '';
      for (let page = 0; page < GROK_MAX_PAGES && out.length < a.count; page++) {
        const q = new URLSearchParams({ pageSize: String(a.count - out.length) });
        if (token) q.set('pageToken', token);
        const r = await grokJSON(`${GROK}/rest/app-chat/conversations?${q}`, {});
        if (!r || !Array.isArray(r.conversations)) throw new OpError('endpoint_changed', 'unexpected conversations answer');
        out.push(...r.conversations);
        token = typeof r.nextPageToken === 'string' ? r.nextPageToken : '';
        if (!token || r.conversations.length === 0) break;
      }
      const result = { conversations: out.slice(0, a.count) };
      if (token && out.length < a.count) result.more = true;
      return result;
    },
    // The detail is response-node's tree and in-flight list plus
    // load-responses' bodies for every node, as one result.
    async 'grok.detail'(a) {
      const base = `${GROK}/rest/app-chat/conversations/${encodeURIComponent(a.id)}`;
      const tree = await grokJSON(`${base}/response-node?includeThreads=true`, {}, true);
      if (!tree || !Array.isArray(tree.responseNodes)) throw new OpError('endpoint_changed', 'unexpected response-node answer');
      const ids = tree.responseNodes.map((n) => n && n.responseId).filter((id) => typeof id === 'string' && ID_RE.test(id));
      let responses = [];
      if (ids.length > 0) {
        const loaded = await grokPost(`${base}/load-responses`, { responseIds: ids });
        if (!loaded || !Array.isArray(loaded.responses)) throw new OpError('endpoint_changed', 'unexpected load-responses answer');
        responses = loaded.responses;
      }
      const inflight = Array.isArray(tree.inflightResponses) ? tree.inflightResponses : [];
      return { conversationId: a.id, responseNodes: tree.responseNodes, inflightResponses: inflight, responses };
    },
    // A generated image is named by its response and index; the URL is
    // read from grok.com again here, never taken from the request.
    async 'grok.file'(a, emit) {
      const m = GROK_FILE_RE.exec(a.file_id);
      if (!m) throw bad('invalid file_id');
      const base = `${GROK}/rest/app-chat/conversations/${encodeURIComponent(a.conversation_id)}`;
      const loaded = await grokPost(`${base}/load-responses`, { responseIds: [m[1]] });
      if (!loaded || !Array.isArray(loaded.responses)) throw new OpError('endpoint_changed', 'unexpected load-responses answer');
      const r = loaded.responses.find((x) => x && x.responseId === m[1]);
      const urls = r && Array.isArray(r.generatedImageUrls) ? r.generatedImageUrls : [];
      const raw = urls[Number(m[2])];
      if (typeof raw !== 'string' || raw === '') throw new OpError('not_found', 'no such image');
      const url = grokAsset(raw);
      if (!url) throw new OpError('endpoint_changed', 'image URL on an unexpected host');
      await emitFile(url, { credentials: 'include' }, emit);
      return undefined;
    },
    // The session check runs first, so a logged-out browser never gets a
    // tab and never sends anonymously.
    async 'chatgpt.send'(a) {
      if (!sender) throw new OpError('unsupported', 'this extension build cannot send');
      await chatgptAuth();
      return sender.send('chatgpt', a);
    },
    // The read operations keep the organization id cached, so the send
    // drops it and asks claude.ai again: a browser that logged out since
    // the last read must not get a tab.
    async 'claudeai.send'(a) {
      if (!sender) throw new OpError('unsupported', 'this extension build cannot send');
      claudeOrg = null;
      await claudeOrgId();
      return sender.send('claudeai', a);
    },
    async 'grok.send'(a) {
      if (!sender) throw new OpError('unsupported', 'this extension build cannot send');
      await grokSession();
      return sender.send('grok', a);
    },
    // The list reads MaZiqc a page at a time, passing each page's token
    // for the next, until it has count conversations or a page carries
    // no next-page token. An error row on any page is endpoint_changed,
    // never an early end: a partial list is not returned as complete.
    async 'gemini.list'(a) {
      const pages = [];
      let token = null;
      let seen = 0;
      for (let i = 0; i < GEMINI_MAX_PAGES; i++) {
        const inner = await geminiRPC('MaZiqc', [GEMINI_PAGE_SIZE, token, [0, null, 1]]);
        if (!Array.isArray(inner)) throw new OpError('endpoint_changed', 'unexpected MaZiqc payload');
        pages.push(inner);
        seen += Array.isArray(inner[2]) ? inner[2].length : 0;
        token = typeof inner[1] === 'string' && inner[1] !== '' ? inner[1] : null;
        if (!token || seen >= a.count) break;
      }
      return { pages };
    },
    async 'gemini.detail'(a) {
      return geminiRead(a.id);
    },
    // gemini.file: file_id is "<response candidate id>-<n>". The image is
    // captured in the tab the send left open when there is one (the
    // isolated world's fetch, with the page's cookies), else fetched
    // here with the image host's grant. Its <img> is never drawn to a
    // canvas: a cross-origin image taints it.
    async 'gemini.file'(a, emit) {
      const conv = geminiId(a.conversation_id);
      const m = /^([A-Za-z0-9][A-Za-z0-9_-]{0,120})-(\d{1,2})$/.exec(a.file_id);
      if (!m) throw bad('invalid Gemini image id');
      const url = geminiImageURL(await geminiRead(conv), m[1], Number(m[2]));
      if (!url) throw new OpError('not_found', 'image not found in the conversation');
      if (sender && typeof sender.capture === 'function') {
        let got = null;
        try {
          got = decodeImage(await sender.capture('gemini', conv, url, MAX_FILE_BYTES));
        } catch {
          got = null;
        }
        if (got) {
          emitBytes(got.bytes, got.mime, emit);
          return undefined;
        }
      }
      await emitFile(url, { credentials: 'include' }, emit, new URL(GEMINI_IMAGE_PREFIX).host);
      return undefined;
    },
    // The session values are fetched fresh for a send, as for claude.ai.
    async 'gemini.send'(a) {
      if (!sender) throw new OpError('unsupported', 'this extension build cannot send');
      if (a.conversation_id !== undefined) geminiId(a.conversation_id);
      await geminiAuth(true);
      return sender.send('gemini', a);
    },
    async 'perplexity.detail'(a) {
      return perplexityThread(a.id);
    },
    // The session check runs first, so a signed-out browser never gets a
    // tab and never asks anonymously.
    async 'perplexity.send'(a) {
      if (!sender) throw new OpError('unsupported', 'this extension build cannot send');
      await perplexitySession();
      return sender.send('perplexity', a);
    },
    // Copilot's chat list has no JSON the extension may read (the app gets
    // it with a bearer token the extension never touches), so the sender
    // reads it from the sidebar of copilot.com/chat in a background tab of
    // its own, with a fixed isolated-world function.
    async 'copilot.list'(a) {
      if (!sender || typeof sender.readList !== 'function') throw new OpError('unsupported', 'this extension build cannot read Copilot');
      return sender.readList('copilot', a.count);
    },
    async 'copilot.detail'(a) {
      return copilotConversation(await copilotJSON(`${COPILOT}/chat/conversation/${encodeURIComponent(copilotId(a.id))}`), a.id);
    },
    // There is no cookie-only account check to run first, so the send's
    // session gate is its tab: it must stay on copilot.com and show a
    // signed-in account before anything is typed (send.js).
    async 'copilot.send'(a) {
      if (!sender) throw new OpError('unsupported', 'this extension build cannot send');
      if (a.conversation_id !== undefined) copilotId(a.conversation_id);
      return sender.send('copilot', a);
    },
    // The dot record and room are read once per DOT_ROOM_TTL_MS per
    // thread; later reads fetch only the feed. A feed error that suggests
    // the room changed drops the cached pair.
    async 'dots.detail'(a) {
      const auth = await chatgptAuth();
      let d = dotRooms.get(a.id);
      if (!d || Date.now() - d.at >= DOT_ROOM_TTL_MS) {
        dotRooms.delete(a.id);
        d = { ...(await dotRoom(auth, a.id)), at: Date.now() };
        dotRooms.set(a.id, d);
      }
      let items;
      try {
        items = await dotFeed(auth, d.room, DOT_FEED_LIMIT);
      } catch (e) {
        if (e instanceof OpError && (e.code === 'not_found' || e.code === 'endpoint_changed' || (e.code === 'http_error' && /^HTTP 422 /.test(e.message)))) dotRooms.delete(a.id);
        throw e;
      }
      return { thread: a.id, room: d.room, owner: d.owner, paused: d.paused, items };
    },
    // A dot has one DM, so the send needs its thread and has no new chat.
    // A paused dot is refused before any tab opens, and again in the queued
    // send before anything is typed or clicked (dotHooks).
    async 'dots.send'(a) {
      if (!sender) throw new OpError('unsupported', 'this extension build cannot send');
      if (a.conversation_id === undefined || a.new_chat === true) throw bad('a dot send needs its thread id as conversation_id');
      const auth = await chatgptAuth();
      const d = await dotRoom(auth, a.conversation_id);
      if (d.paused) throw dotPausedErr();
      return sender.send('dots', a, dotHooks(auth, a.conversation_id, d, a.message));
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
    async 'grok.close'(a) {
      if (!sender) throw new OpError('unsupported', 'this extension build cannot send');
      return sender.close('grok', a.conversation_id);
    },
    async 'gemini.close'(a) {
      if (!sender) throw new OpError('unsupported', 'this extension build cannot send');
      return sender.close('gemini', a.conversation_id);
    },
    async 'perplexity.close'(a) {
      if (!sender) throw new OpError('unsupported', 'this extension build cannot send');
      return sender.close('perplexity', a.conversation_id);
    },
    async 'copilot.close'(a) {
      if (!sender) throw new OpError('unsupported', 'this extension build cannot send');
      return sender.close('copilot', a.conversation_id);
    },
    async 'dots.close'(a) {
      if (!sender) throw new OpError('unsupported', 'this extension build cannot send');
      return sender.close('dots', a.conversation_id);
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

  // requireGrant fails permission_missing when op's site's page origins
  // are not granted.
  // Closing a tab the extension opened reaches no site, so it runs
  // regardless: a grant revoked while a reply is read still lets the tab
  // close.
  async function requireGrant(op) {
    const [prefix, verb] = op.split('.');
    const site = Object.hasOwn(SITE_ALIASES, prefix) ? SITE_ALIASES[prefix] : prefix;
    if (!permissions || !Object.hasOwn(SITE_ACCESS, site) || verb === 'close') return;
    if (!(await siteGranted(permissions, site))) {
      throw new OpError('permission_missing', `the extension has no access to ${SITE_ACCESS[site].label}; grant it on the extension's options page`);
    }
  }

  return {
    // run executes one validated operation, calling emit with each
    // response frame (without the request id). It throws OpError.
    clearInputs() { inputEpoch++; transfers.clear(); },
    expireInputs() { transfers.busy(); },
    async run(op, args, emit, input) {
      if (INPUT_OPS.includes(op)) {
        const [site, verb] = op.split('.');
        if (!IMAGE_INPUT_SITES.includes(site)) throw new OpError('unsupported', 'image input awaits live acceptance; send text alone');
        input = validate({ id: 0, op, args, input }).input;
        const epoch = inputEpoch;
        if (verb !== 'input_abort') await requireGrant(op);
        if (epoch !== inputEpoch) throw bad('image transfer disconnected');
        let result;
        if (verb === 'input_begin') result = transfers.begin(input.connection, site, input.files);
        else if (verb === 'input_chunk') result = transfers.chunk(input.connection, site, { token: input.token, seq: input.seq, data: input.data });
        else if (verb === 'input_abort') result = transfers.abort(input.connection, site, input.token);
        else {
          let consumed = false;
          try {
            const files = await transfers.consume(input.connection, site, input.token);
            consumed = true;
            if (!sender) throw new OpError('unsupported', 'upgrade the extension for image input');
            await chatgptAuth();
            result = await sender.send(site, args, { images: files, active: () => transfers.active(input.token), confirmImages: async (id, submittedAt) => {
              const raw = await handlers['chatgpt.detail']({ id });
              return confirmedInputMessage(raw, args.message, files, submittedAt);
            } });
          } finally {
            if (consumed) transfers.release(input.connection, site, input.token);
          }
        }
        emit({ ok: true, result });
        return;
      }
      if (!Object.hasOwn(handlers, op)) throw bad('unknown operation');
      await requireGrant(op);
      try {
        const result = await handlers[op](args, emit);
        if (result !== undefined) emit({ ok: true, result });
      } catch (e) {
        if (e instanceof OpError && e.code === 'not_logged_in') claudeOrg = null;
        if (e instanceof OpError && (e.code === 'not_logged_in' || e.code === 'blocked')) geminiSession = null;
        throw e;
      }
    },
  };
}

// The existing ChatGPT reader supplies this shape. Require ordered names,
// MIME types, real image pointers, fresh time and a unique human turn.
export function confirmedInputMessage(raw, message, files, submittedAt) {
  const matches = [];
  const seen = new Set();
  let id = raw?.current_node;
  while (typeof id === 'string' && raw?.mapping?.[id] && !seen.has(id)) {
    seen.add(id);
    const node = raw.mapping[id];
    const m = node.message;
    const parts = m?.content?.parts;
    const attachments = m?.metadata?.attachments;
    if (m?.author?.role === 'user' && ID_RE.test(m.id || '') && Number.isFinite(m.create_time) && m.create_time * 1000 >= submittedAt - 1000 && Array.isArray(parts) && Array.isArray(attachments)) {
      const text = parts.filter((p) => typeof p === 'string').join('\n');
      const pointers = parts.filter((p) => p?.content_type === 'image_asset_pointer' && typeof p.asset_pointer === 'string');
      if (text === message && pointers.length === files.length && attachments.length === files.length && files.every((f, i) => attachments[i].name === f.name && attachments[i].mime_type === f.mime && ID_RE.test(attachments[i].id || '') && [`sediment://${attachments[i].id}`, `file-service://${attachments[i].id}`].includes(pointers[i].asset_pointer))) matches.push(m.id);
    }
    id = node.parent;
  }
  return matches.length === 1 ? matches[0] : '';
}
