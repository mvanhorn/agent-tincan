import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { validate, createRunner, errorFrame, grantedSites, helloMessage, geminiImageURL, geminiImageURLs, parseBatchexecute, copilotConversation, OpError, OPS, SITE_ACCESS, SITE_ALIASES, CHUNK_BYTES, MAX_FILE_BYTES, MAX_MESSAGE_BYTES, GROK_MAX_PAGES, PERPLEXITY_MAX_PAGES, PERPLEXITY_ENTRY_FIELDS } from '../ops.js';

const fixture = (p) => JSON.parse(readFileSync(new URL('../../internal/history/testdata/' + p, import.meta.url)));

function jsonResponse(body, status = 200, type = 'application/json') {
  return new Response(typeof body === 'string' ? body : JSON.stringify(body), { status, headers: { 'content-type': type } });
}

function bytesResponse(bytes, type = 'image/png') {
  return new Response(bytes, { status: 200, headers: { 'content-type': type, 'content-length': String(bytes.length) } });
}

// fakeFetch routes by exact URL and records every call.
function fakeFetch(routes) {
  const calls = [];
  const fn = async (url, init = {}) => {
    calls.push({ url: String(url), init });
    const h = routes[String(url)];
    if (!h) return jsonResponse({ detail: 'not found' }, 404);
    return typeof h === 'function' ? h(url, init) : h.clone();
  };
  fn.calls = calls;
  return fn;
}

async function run(runner, op, args) {
  const frames = [];
  await runner.run(op, args, (f) => frames.push(f));
  return frames;
}

const SESSION = 'https://chatgpt.com/api/auth/session';
const TOKEN = 'secret-access-token-never-returned';

test('validate accepts only the fixed operation set with exact args', () => {
  assert.deepEqual([...OPS].sort(), ['chatgpt.close', 'chatgpt.detail', 'chatgpt.file', 'chatgpt.list', 'chatgpt.send', 'claudeai.close', 'claudeai.detail', 'claudeai.file', 'claudeai.list', 'claudeai.send', 'copilot.close', 'copilot.detail', 'copilot.list', 'copilot.send', 'dots.close', 'dots.detail', 'dots.send', 'extension.reload', 'gemini.close', 'gemini.detail', 'gemini.file', 'gemini.list', 'gemini.send', 'grok.close', 'grok.detail', 'grok.file', 'grok.list', 'grok.send', 'perplexity.close', 'perplexity.detail', 'perplexity.send', 'chatgpt.input_abort', 'chatgpt.input_begin', 'chatgpt.input_chunk', 'chatgpt.send_images', ...['chatgpt', 'claudeai', 'grok', 'gemini', 'perplexity', 'copilot', 'dots'].map(site => site + '.session')].sort());
  assert.deepEqual(validate({ id: 8, op: 'grok.file', args: { file_id: 'r1_0', conversation_id: 'c1' } }).args, { file_id: 'r1_0', conversation_id: 'c1' });
  assert.deepEqual(validate({ id: 9, op: 'grok.send', args: { message: 'hi', conversation_id: '0e1d0000-0000-4000-8000-000000000001' } }).args.conversation_id, '0e1d0000-0000-4000-8000-000000000001');
  assert.deepEqual(validate({ id: 1, op: 'chatgpt.list', args: { count: 5 } }), { id: 1, op: 'chatgpt.list', args: { count: 5 } });
  validate({ id: 2, op: 'chatgpt.file', args: { file_id: 'file_00000000abcd1234', conversation_id: 'abc-1' } });
  validate({ id: 3, op: 'claudeai.detail', args: { id: 'c1a0d000-0000-4000-8000-000000000001' } });
  assert.deepEqual(validate({ id: 4, op: 'chatgpt.send', args: { message: 'hello' } }).args, { message: 'hello' });
  assert.deepEqual(validate({ id: 5, op: 'claudeai.send', args: { message: 'x'.repeat(MAX_MESSAGE_BYTES), conversation_id: 'abc-1', new_chat: false } }).args.conversation_id, 'abc-1');
  assert.deepEqual(validate({ id: 6, op: 'chatgpt.send', args: { message: 'hi', new_chat: true } }).args, { message: 'hi', new_chat: true });
  assert.deepEqual(validate({ id: 7, op: 'claudeai.close', args: { conversation_id: 'abc-1' } }).args, { conversation_id: 'abc-1' });
  assert.deepEqual(validate({ id: 7, op: 'extension.reload', args: {} }), { id: 7, op: 'extension.reload', args: {} });
  const bad = [
    null,
    'chatgpt.list',
    { id: 1, op: 'chatgpt.eval', args: { count: 1 } },
    { id: 1, op: 'constructor', args: {} },
    { id: -1, op: 'chatgpt.list', args: { count: 1 } },
    { id: 1.5, op: 'chatgpt.list', args: { count: 1 } },
    { id: 1, op: 'chatgpt.list', args: { count: 0 } },
    { id: 1, op: 'chatgpt.list', args: { count: 101 } },
    { id: 1, op: 'chatgpt.list', args: { count: 2.5 } },
    { id: 1, op: 'chatgpt.list', args: { count: '5' } },
    { id: 1, op: 'chatgpt.list', args: { count: 1, code: 'alert(1)' } },
    { id: 1, op: 'chatgpt.detail', args: { id: '../../backend-api/me' } },
    { id: 1, op: 'chatgpt.detail', args: { id: 'a.b' } },
    { id: 1, op: 'chatgpt.detail', args: { id: 'abc?x=1' } },
    { id: 1, op: 'chatgpt.detail', args: { id: 'x'.repeat(129) } },
    { id: 1, op: 'claudeai.file', args: { file_id: 'f1', conversation_id: 'c1' } },
    { id: 1, op: 'claudeai.detail', args: {} },
    { id: 1, op: 'chatgpt.list' },
    { id: 1, op: 'chatgpt.list', args: { count: 1 }, extra: true },
    { id: 1, op: 'chatgpt.send', args: {} },
    { id: 1, op: 'chatgpt.send', args: { message: '' } },
    { id: 1, op: 'chatgpt.send', args: { message: '  \n ' } },
    { id: 1, op: 'chatgpt.send', args: { message: 42 } },
    { id: 1, op: 'chatgpt.send', args: { message: 'x'.repeat(MAX_MESSAGE_BYTES + 1) } },
    { id: 1, op: 'chatgpt.send', args: { message: '\u00e9'.repeat(MAX_MESSAGE_BYTES / 2 + 1) } },
    { id: 1, op: 'chatgpt.send', args: { message: 'hi', conversation_id: '../c/x' } },
    { id: 1, op: 'chatgpt.send', args: { message: 'hi', new_chat: 'yes' } },
    { id: 1, op: 'chatgpt.send', args: { message: 'hi', new_chat: true, conversation_id: 'abc' } },
    { id: 1, op: 'claudeai.send', args: { message: 'hi', code: 'alert(1)' } },
    { id: 1, op: 'claudeai.send', args: { message: 'hi', url: 'https://evil.example/' } },
    { id: 1, op: 'chatgpt.close', args: {} },
    { id: 1, op: 'chatgpt.close', args: { conversation_id: '../c/x' } },
    { id: 1, op: 'claudeai.close', args: { conversation_id: 'abc', tab_id: 5 } },
    { id: 1, op: 'grok.file', args: { file_id: 'r1_0' } },
    { id: 1, op: 'grok.file', args: { file_id: 'r1_0', conversation_id: 'c1', url: 'https://assets.grok.com/x' } },
    { id: 1, op: 'grok.list', args: { count: 1, pageToken: 'x' } },
    { id: 1, op: 'grok.detail', args: { id: '../../rest/app-chat/conversations' } },
    { id: 1, op: 'copilot.file', args: { file_id: 'f1', conversation_id: 'c1' } },
    { id: 1, op: 'copilot.detail', args: { id: 'c0b1107a-0000-4000-8000-000000000001', url: 'https://copilot.com/' } },
    { id: 1, op: 'extension.reload', args: { now: true } },
    { id: 1, op: 'extension.reload' },
  ];
  for (const m of bad) {
    assert.throws(() => validate(m), (e) => e.code === 'bad_request', JSON.stringify(m));
  }
});

test('chatgpt.list gets the token in the worker and never returns it', async () => {
  const list = fixture('chatgpt/conversations.json');
  const f = fakeFetch({
    [SESSION]: jsonResponse({ accessToken: TOKEN, user: { id: 'u' } }),
    'https://chatgpt.com/backend-api/conversations?offset=0&limit=20&order=updated': jsonResponse(list),
  });
  const frames = await run(createRunner({ fetch: f }), 'chatgpt.list', { count: 20 });
  assert.equal(frames.length, 1);
  assert.equal(frames[0].ok, true);
  assert.deepEqual(frames[0].result, list);
  assert.ok(!JSON.stringify(frames).includes(TOKEN));
  assert.equal(f.calls[0].init.credentials, 'include');
  assert.equal(f.calls[1].init.headers.Authorization, 'Bearer ' + TOKEN);
  assert.equal(f.calls[1].init.credentials, 'include');
  assert.equal(f.calls[1].init.method, 'GET');
});

test('chatgpt.detail fetches one conversation', async () => {
  const id = '6a1f0c2e-1111-4a2b-9c3d-000000000001';
  const detail = fixture(`chatgpt/conversation-${id}.json`);
  const f = fakeFetch({
    [SESSION]: jsonResponse({ accessToken: TOKEN }),
    [`https://chatgpt.com/backend-api/conversation/${id}`]: jsonResponse(detail),
  });
  const frames = await run(createRunner({ fetch: f }), 'chatgpt.detail', { id });
  assert.deepEqual(frames[0].result, detail);
});

test('chatgpt not logged in maps to not_logged_in', async () => {
  const f = fakeFetch({ [SESSION]: jsonResponse({}) });
  await assert.rejects(run(createRunner({ fetch: f }), 'chatgpt.list', { count: 1 }), (e) => e.code === 'not_logged_in');
  const f401 = fakeFetch({
    [SESSION]: jsonResponse({ accessToken: TOKEN }),
    'https://chatgpt.com/backend-api/conversations?offset=0&limit=1&order=updated': jsonResponse({ detail: 'expired' }, 401),
  });
  await assert.rejects(run(createRunner({ fetch: f401 }), 'chatgpt.list', { count: 1 }), (e) => e.code === 'not_logged_in' && !e.message.includes(TOKEN));
});

test('error classes: blocked, endpoint changed, not found, rate limited', async () => {
  const listURL = 'https://chatgpt.com/backend-api/conversations?offset=0&limit=1&order=updated';
  const cases = [
    [jsonResponse('<html>Just a moment...</html>', 403, 'text/html'), 'blocked'],
    [jsonResponse({}, 404), 'endpoint_changed'],
    [jsonResponse('<html>not json</html>', 200, 'text/html'), 'endpoint_changed'],
    [jsonResponse({}, 429), 'rate_limited'],
    [jsonResponse({}, 500), 'http_error'],
  ];
  for (const [resp, code] of cases) {
    const f = fakeFetch({ [SESSION]: jsonResponse({ accessToken: TOKEN }), [listURL]: resp });
    await assert.rejects(run(createRunner({ fetch: f }), 'chatgpt.list', { count: 1 }), (e) => e.code === code, code);
  }
  const f = fakeFetch({ [SESSION]: jsonResponse({ accessToken: TOKEN }) });
  await assert.rejects(run(createRunner({ fetch: f }), 'chatgpt.detail', { id: 'missing-1' }), (e) => e.code === 'not_found');
});

test('rate_limited carries retry_after seconds from the Retry-After header', async () => {
  const listURL = 'https://chatgpt.com/backend-api/conversations?offset=0&limit=1&order=updated';
  const limited = (headers) => new Response('{}', { status: 429, headers: { 'content-type': 'application/json', ...headers } });
  const soon = new Date(Date.now() + 90_000).toUTCString();
  const cases = [
    [{ 'retry-after': '120' }, 120],
    [{ 'retry-after': soon }, [88, 91]],
    [{ 'retry-after': 'Wed, 21 Oct 2015 07:28:00 GMT' }, undefined],
    [{ 'retry-after': 'soon' }, undefined],
    [{ 'retry-after': '999999' }, 3600],
    [{}, undefined],
  ];
  for (const [headers, want] of cases) {
    const f = fakeFetch({ [SESSION]: jsonResponse({ accessToken: TOKEN }), [listURL]: limited(headers) });
    let caught;
    await assert.rejects(run(createRunner({ fetch: f }), 'chatgpt.list', { count: 1 }), (e) => {
      caught = e;
      return e.code === 'rate_limited';
    });
    const frame = errorFrame(caught);
    assert.equal(frame.ok, false);
    assert.equal(frame.error.code, 'rate_limited');
    assert.match(frame.error.message, /HTTP 429 from \/backend-api\/conversations/);
    if (Array.isArray(want)) {
      assert.ok(frame.error.retry_after >= want[0] && frame.error.retry_after <= want[1], `${JSON.stringify(headers)}: ${frame.error.retry_after}`);
    } else if (want === undefined) {
      assert.ok(!('retry_after' in frame.error), `${JSON.stringify(headers)}: ${JSON.stringify(frame.error)}`);
    } else {
      assert.equal(frame.error.retry_after, want, JSON.stringify(headers));
    }
  }
  // Other errors carry no retry_after; unknown errors are internal.
  assert.deepEqual(errorFrame(new OpError('http_error', 'HTTP 502 from /x')), { ok: false, error: { code: 'http_error', message: 'HTTP 502 from /x' } });
  assert.deepEqual(errorFrame(new Error('secret detail')), { ok: false, error: { code: 'internal', message: 'internal error' } });
});

function pngBytes(n) {
  const b = new Uint8Array(n);
  b.set([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);
  for (let i = 8; i < n; i++) b[i] = i % 251;
  return b;
}

function reassemble(frames) {
  const parts = frames.map((f, i) => {
    assert.equal(f.ok, true);
    assert.equal(f.chunk.seq, i);
    assert.equal(f.chunk.last, i === frames.length - 1);
    return Buffer.from(f.chunk.data, 'base64');
  });
  return Buffer.concat(parts);
}

test('chatgpt.file (file-service) follows download_url on an allowed host, in bounded chunks', async () => {
  const img = pngBytes(CHUNK_BYTES * 2 + 17);
  const signed = 'https://files.oaiusercontent.com/file-Sk3tchAbc123?se=2026&sig=fake';
  const f = fakeFetch({
    [SESSION]: jsonResponse({ accessToken: TOKEN }),
    'https://chatgpt.com/backend-api/files/file-Sk3tchAbc123/download': jsonResponse({ status: 'success', download_url: signed, file_name: 'sketch.png' }),
    [signed]: bytesResponse(img),
  });
  const frames = await run(createRunner({ fetch: f }), 'chatgpt.file', { file_id: 'file-Sk3tchAbc123' });
  assert.equal(frames.length, 3);
  for (const fr of frames) {
    assert.ok(fr.chunk.data.length <= Math.ceil(CHUNK_BYTES / 3) * 4);
    assert.equal(fr.mime, 'image/png');
    assert.equal(fr.size, img.length);
  }
  assert.deepEqual(new Uint8Array(reassemble(frames)), img);
  const signedCall = f.calls.find((c) => c.url === signed);
  assert.equal(signedCall.init.credentials, 'omit');
  assert.equal(signedCall.init.headers, undefined);
});

test('chatgpt.file (sediment) uses the newer download endpoint with the conversation id', async () => {
  const img = pngBytes(100);
  const u = 'https://chatgpt.com/backend-api/files/download/file_00000000abcd1234?conversation_id=conv-1&inline=false';
  const f = fakeFetch({
    [SESSION]: jsonResponse({ accessToken: TOKEN }),
    [u]: jsonResponse({ status: 'success', download_url: '/backend-api/estuary/content?id=file_00000000abcd1234&sig=x' }),
    'https://chatgpt.com/backend-api/estuary/content?id=file_00000000abcd1234&sig=x': bytesResponse(img),
  });
  const frames = await run(createRunner({ fetch: f }), 'chatgpt.file', { file_id: 'file_00000000abcd1234', conversation_id: 'conv-1' });
  assert.deepEqual(new Uint8Array(reassemble(frames)), img);
  const est = f.calls.at(-1);
  assert.equal(est.init.credentials, 'include');
});

test('chatgpt.file refuses download_url on a host outside the allowlist', async () => {
  for (const bad of ['https://evil.example/x.png', 'http://files.oaiusercontent.com/x', 'https://oaiusercontent.com.evil.example/x', 'javascript:alert(1)']) {
    const f = fakeFetch({
      [SESSION]: jsonResponse({ accessToken: TOKEN }),
      'https://chatgpt.com/backend-api/files/file-A1/download': jsonResponse({ status: 'success', download_url: bad }),
    });
    await assert.rejects(run(createRunner({ fetch: f }), 'chatgpt.file', { file_id: 'file-A1' }), (e) => e.code === 'endpoint_changed', bad);
    assert.equal(f.calls.length, 2, 'no fetch of ' + bad);
  }
});

test('file size cap and content type', async () => {
  const big = new Response(new Uint8Array(8), { status: 200, headers: { 'content-type': 'image/png', 'content-length': String(MAX_FILE_BYTES + 1) } });
  const org = fixture('claudeai/organizations.json');
  const f = fakeFetch({
    'https://claude.ai/api/organizations': jsonResponse(org),
    'https://claude.ai/api/0rg00000-0000-4000-8000-000000000000/files/f1/preview': big,
    'https://claude.ai/api/0rg00000-0000-4000-8000-000000000000/files/f2/preview': bytesResponse(new Uint8Array(10), 'text/html'),
  });
  const r = createRunner({ fetch: f });
  await assert.rejects(run(r, 'claudeai.file', { file_id: 'f1' }), (e) => e.code === 'too_large');
  await assert.rejects(run(r, 'claudeai.file', { file_id: 'f2' }), (e) => e.code === 'endpoint_changed');
});

test('file size cap applies to a streamed body with no content-length', async () => {
  let pulled = 0;
  let cancelled = false;
  const chunk = new Uint8Array(1024 * 1024);
  const body = new ReadableStream({
    pull(c) {
      pulled += chunk.length;
      c.enqueue(chunk);
      if (pulled > MAX_FILE_BYTES * 4) c.close();
    },
    cancel() {
      cancelled = true;
    },
  });
  const res = new Response(body, { status: 200, headers: { 'content-type': 'image/png' } });
  assert.equal(res.headers.get('content-length'), null);
  const org = fixture('claudeai/organizations.json');
  const f = fakeFetch({
    'https://claude.ai/api/organizations': jsonResponse(org),
    'https://claude.ai/api/0rg00000-0000-4000-8000-000000000000/files/f3/preview': () => res,
  });
  const frames = [];
  await assert.rejects(
    createRunner({ fetch: f }).run('claudeai.file', { file_id: 'f3' }, (fr) => frames.push(fr)),
    (e) => e.code === 'too_large' && e.message === `file over ${MAX_FILE_BYTES} bytes`,
  );
  assert.equal(frames.length, 0);
  assert.ok(pulled <= MAX_FILE_BYTES + 2 * chunk.length, `read ${pulled} bytes before refusing`);
  assert.ok(cancelled, 'body stream cancelled');
});

test('a streamed body with no content-length under the cap is emitted whole', async () => {
  const img = pngBytes(CHUNK_BYTES + 5);
  const body = new ReadableStream({
    start(c) {
      c.enqueue(img.subarray(0, 100));
      c.enqueue(img.subarray(100));
      c.close();
    },
  });
  const org = fixture('claudeai/organizations.json');
  const f = fakeFetch({
    'https://claude.ai/api/organizations': jsonResponse(org),
    'https://claude.ai/api/0rg00000-0000-4000-8000-000000000000/files/f4/preview': () => new Response(body, { status: 200, headers: { 'content-type': 'image/png' } }),
  });
  const frames = await run(createRunner({ fetch: f }), 'claudeai.file', { file_id: 'f4' });
  assert.equal(frames.length, 2);
  assert.equal(frames[0].size, img.length);
  assert.deepEqual(new Uint8Array(reassemble(frames)), img);
});

test('claudeai list, detail and file use the organization from /api/organizations', async () => {
  const org = '0rg00000-0000-4000-8000-000000000000';
  const list = fixture('claudeai/chat_conversations.json');
  const cid = 'c1a0d000-0000-4000-8000-000000000001';
  const detail = fixture(`claudeai/conversation-${cid}.json`);
  const img = pngBytes(500);
  const f = fakeFetch({
    'https://claude.ai/api/organizations': jsonResponse(fixture('claudeai/organizations.json')),
    [`https://claude.ai/api/organizations/${org}/chat_conversations?limit=2`]: jsonResponse(list),
    [`https://claude.ai/api/organizations/${org}/chat_conversations/${cid}?tree=True&rendering_mode=messages&render_all_tools=true`]: jsonResponse(detail),
    [`https://claude.ai/api/${org}/files/f11e0000-0000-4000-8000-0000000000aa/preview`]: bytesResponse(img, 'image/png'),
  });
  const r = createRunner({ fetch: f });
  const l = await run(r, 'claudeai.list', { count: 2 });
  assert.equal(l[0].result.length, 2, 'list trimmed to count');
  const d = await run(r, 'claudeai.detail', { id: cid });
  assert.deepEqual(d[0].result, detail);
  const fr = await run(r, 'claudeai.file', { file_id: 'f11e0000-0000-4000-8000-0000000000aa' });
  assert.deepEqual(new Uint8Array(reassemble(fr)), img);
  for (const c of f.calls) assert.equal(c.init.credentials, 'include');
  assert.equal(f.calls.filter((c) => c.url.endsWith('/api/organizations')).length, 1, 'org cached');
});

test('claudeai with no organization is not logged in', async () => {
  const f = fakeFetch({ 'https://claude.ai/api/organizations': jsonResponse([]) });
  await assert.rejects(run(createRunner({ fetch: f }), 'claudeai.list', { count: 1 }), (e) => e.code === 'not_logged_in');
  const f403 = fakeFetch({ 'https://claude.ai/api/organizations': jsonResponse({ error: { type: 'permission_error' } }, 403) });
  await assert.rejects(run(createRunner({ fetch: f403 }), 'claudeai.list', { count: 1 }), (e) => e.code === 'not_logged_in');
});

test('message content is data: a detail containing script text is returned untouched and never run', async () => {
  const id = 'evil-1';
  let ran = false;
  globalThis.__tincanPwned = () => { ran = true; };
  const detail = { title: 'x', mapping: { a: { message: { content: { parts: ['__tincanPwned()', '<script>__tincanPwned()</script>'] } } } } };
  const f = fakeFetch({
    [SESSION]: jsonResponse({ accessToken: TOKEN }),
    [`https://chatgpt.com/backend-api/conversation/${id}`]: jsonResponse(detail),
  });
  const frames = await run(createRunner({ fetch: f }), 'chatgpt.detail', { id });
  assert.deepEqual(frames[0].result, detail);
  assert.equal(ran, false);
  delete globalThis.__tincanPwned;
});

test('worker code has no dynamic code execution', () => {
  const src = (f) => readFileSync(new URL(f, import.meta.url), 'utf8');
  for (const f of ['../ops.js', '../background.js', '../send.js', '../options.js']) {
    for (const banned of [/\beval\s*\(/, /new\s+Function\s*\(/, /importScripts\s*\(/, /set(Timeout|Interval)\s*\(\s*['"`]/, /chrome\.(debugger|webRequest|cookies|downloads)/]) {
      assert.ok(!banned.test(src(f)), `${f} matches ${banned}`);
    }
  }
  // ops.js stays fetch-only; tabs and scripting are reached only through
  // the sender background.js builds.
  const code = (f) => src(f).replace(/^\s*\/\/.*$/gm, '');
  assert.ok(!/chrome\./.test(code('../ops.js')), 'ops.js uses no chrome API');
  assert.ok(!/chrome\./.test(code('../send.js')), 'send.js gets tabs and scripting injected');
  const bg = src('../background.js');
  assert.deepEqual(bg.match(/chrome\.(tabs|scripting)\b/g), ['chrome.tabs', 'chrome.scripting']);
  // Every injection is a fixed function with its data as args, and only
  // send.js injects.
  assert.ok(!/executeScript/.test(bg) && !/executeScript/.test(src('../ops.js')));
  const send = src('../send.js');
  const calls = send.match(/executeScript\([^)]*\)/g);
  assert.deepEqual(calls, ['executeScript({ target: { tabId }, world: \'ISOLATED\', func, args })']);
  const injected = [...send.matchAll(/await inject\((?:tab\.id|tabId|entry\[0\]), (\w+),/g)].map((m) => m[1]);
  assert.equal(injected.length, [...send.matchAll(/await inject\(/g)].length, 'every injection is listed');
  for (const f of injected) {
    assert.ok(['pageInputImages', 'pageProbe', 'pageDismiss', 'pageFill', 'pageSubmit', 'pageFetchImage', 'pageCopilotList'].includes(f), f);
  }
});

// ---- Site access: every operation but close needs its site's grant.

// fakePermissions answers chrome.permissions.contains from a set of
// granted origins.
function fakePermissions(granted) {
  const asked = [];
  return {
    asked,
    granted: new Set(granted),
    async contains({ origins }) {
      asked.push(origins);
      return origins.every((o) => this.granted.has(o));
    },
  };
}

const ALL_ORIGINS = Object.values(SITE_ACCESS).flatMap((s) => s.origins);

test('the manifest asks for exactly the origins in SITE_ACCESS', () => {
  const m = JSON.parse(readFileSync(new URL('../manifest.json', import.meta.url), 'utf8'));
  const required = Object.values(SITE_ACCESS).filter((s) => s.required).flatMap((s) => s.origins);
  const optional = Object.values(SITE_ACCESS).filter((s) => !s.required).flatMap((s) => s.origins);
  // ChatGPT and claude.ai stay required, so an upgrade asks for nothing new;
  // Grok, Gemini, Perplexity and Copilot are optional, granted from the options page.
  assert.deepEqual(m.host_permissions, ['https://chatgpt.com/*', 'https://*.oaiusercontent.com/*', 'https://claude.ai/*']);
  assert.deepEqual(m.optional_host_permissions, ['https://grok.com/*', 'https://assets.grok.com/*', 'https://gemini.google.com/*', 'https://lh3.googleusercontent.com/*', 'https://www.perplexity.ai/*', 'https://copilot.com/*', 'https://copilot.microsoft.com/*']);
  assert.equal(SITE_ACCESS.grok.required, false);
  assert.equal(SITE_ACCESS.gemini.required, false);
  assert.equal(SITE_ACCESS.perplexity.required, false);
  assert.equal(SITE_ACCESS.copilot.required, false);
  assert.deepEqual(SITE_ACCESS.copilot.pageOrigins, ['https://copilot.com/*']);
  assert.ok(m.description.length <= 132, 'the store caps the description at 132 characters');
  assert.deepEqual(m.host_permissions, required);
  assert.deepEqual(m.optional_host_permissions || [], optional);
  assert.equal(m.options_ui.page, 'options.html');
  for (const s of Object.values(SITE_ACCESS)) {
    assert.ok(typeof s.label === 'string' && s.label !== '');
    assert.ok(s.origins.length > 0 && s.origins.every((o) => /^https:\/\/[a-z0-9.*-]+\/\*$/.test(o)), s.label);
    assert.ok(s.pageOrigins.length > 0 && s.pageOrigins.every((o) => s.origins.includes(o)), s.label);
  }
  // Every op prefix but extension.* has a site entry.
  for (const op of OPS) {
    const prefix = op.split('.')[0];
    if (prefix !== 'extension') assert.ok(Object.hasOwn(SITE_ACCESS, prefix) || Object.hasOwn(SITE_ACCESS, SITE_ALIASES[prefix]), op);
  }
});

test('an op for a site without its grant fails permission_missing and opens no tab or fetch', async () => {
  const f = fakeFetch({ [SESSION]: jsonResponse({ accessToken: TOKEN }) });
  const sent = [];
  const closed = [];
  const sender = {
    send: async (site, a) => (sent.push(site), { conversation_id: 'c1', url: '', submitted_at: 1 }),
    close: async (site, id) => (closed.push([site, id]), { closed: 1 }),
  };
  const perms = fakePermissions(['https://claude.ai/*']);
  const r = createRunner({ fetch: f, sender, permissions: perms });
  for (const [op, args] of [['chatgpt.send', { message: 'hi' }], ['chatgpt.list', { count: 1 }], ['chatgpt.detail', { id: 'c1' }], ['chatgpt.file', { file_id: 'file-1' }]]) {
    let caught;
    await assert.rejects(run(r, op, args), (e) => ((caught = e), e.code === 'permission_missing'), op);
    assert.match(errorFrame(caught).error.message, /options page/);
  }
  assert.equal(f.calls.length, 0, 'no fetch without the grant');
  assert.deepEqual(sent, [], 'no tab without the grant');
  // Closing a tab the extension opened needs no site access: a grant
  // revoked while a reply is read still lets its tab close.
  assert.deepEqual(await run(r, 'chatgpt.close', { conversation_id: 'c1' }), [{ ok: true, result: { closed: 1 } }]);
  assert.deepEqual(closed, [['chatgpt', 'c1']]);
  assert.deepEqual(perms.asked[0], [...SITE_ACCESS.chatgpt.pageOrigins]);

  // Granted: the op runs.
  perms.granted.add('https://chatgpt.com/*');
  perms.granted.add('https://*.oaiusercontent.com/*');
  const frames = await run(r, 'chatgpt.send', { message: 'hi' });
  assert.equal(frames[0].result.conversation_id, 'c1');
  assert.deepEqual(sent, ['chatgpt']);

  // A permissions API that throws counts as not granted.
  const broken = createRunner({ fetch: f, sender, permissions: { contains: async () => { throw new Error('x'); } } });
  await assert.rejects(run(broken, 'claudeai.list', { count: 1 }), (e) => e.code === 'permission_missing');
});

// ChatGPT's file host (*.oaiusercontent.com) is not needed to list or
// read conversations: withholding it leaves those ops working, the site
// counts as granted in the hello, and a file fetch from that host still
// fails as it would without the grant.
test('a site with only its page origin granted passes list and detail', async () => {
  const listURL = 'https://chatgpt.com/backend-api/conversations?offset=0&limit=1&order=updated';
  const detailURL = 'https://chatgpt.com/backend-api/conversation/c1';
  const f = fakeFetch({
    [SESSION]: jsonResponse({ accessToken: TOKEN }),
    [listURL]: jsonResponse({ items: [] }),
    [detailURL]: jsonResponse({ id: 'c1', mapping: {} }),
  });
  const perms = fakePermissions(['https://chatgpt.com/*']);
  const r = createRunner({ fetch: f, permissions: perms });
  await run(r, 'chatgpt.list', { count: 1 });
  await run(r, 'chatgpt.detail', { id: 'c1' });
  assert.ok(f.calls.some((c) => c.url === listURL), 'list fetched');
  assert.ok(f.calls.some((c) => c.url === detailURL), 'detail fetched');
  assert.deepEqual(await grantedSites(perms), ['chatgpt']);
  // The page origin alone withheld: permission_missing.
  perms.granted = new Set(['https://*.oaiusercontent.com/*']);
  await assert.rejects(run(r, 'chatgpt.list', { count: 1 }), (e) => e.code === 'permission_missing');
});

test('the hello lists the granted sites', async () => {
  const files = { 'manifest.json': 'x' };
  const perms = fakePermissions(['https://claude.ai/*']);
  const h = await helloMessage({ manifest: { version: '1.0.0' }, files, permissions: perms });
  assert.deepEqual(h, { id: 0, hello: { version: '1.0.0', unpacked: true, files, granted: ['claudeai'], image_input: { version: 1, sites: [] } } });
  perms.granted = new Set(ALL_ORIGINS);
  assert.deepEqual((await grantedSites(perms)).sort(), Object.keys(SITE_ACCESS).sort());
});

// The anti-bot sniff reads at most 64 KiB of a body, not all of it.
test('a large non-JSON answer is read only up to the sniff cap', async () => {
  const listURL = 'https://chatgpt.com/backend-api/conversations?offset=0&limit=1&order=updated';
  let pulled = 0;
  const big = () => new Response(new ReadableStream({
    pull(c) {
      if (pulled >= 8 * 1024 * 1024) return c.close();
      pulled += 16 * 1024;
      c.enqueue(new Uint8Array(16 * 1024).fill(0x61));
    },
  }, { highWaterMark: 0 }), { status: 200, headers: { 'content-type': 'text/html' } });
  const f = fakeFetch({ [SESSION]: jsonResponse({ accessToken: TOKEN }), [listURL]: big });
  const r = createRunner({ fetch: f });
  await assert.rejects(run(r, 'chatgpt.list', { count: 1 }), (e) => e.code === 'endpoint_changed');
  assert.ok(pulled <= 256 * 1024, `read ${pulled} bytes`);
});

// The sniff is bounded by time too: a body that sends a little and then
// stalls is classified from what arrived, not left to hang the reply.
test('a body that stalls mid-sniff is classified from what arrived', async () => {
  const listURL = 'https://chatgpt.com/backend-api/conversations?offset=0&limit=1&order=updated';
  const stalled = (text, status, type) => () => new Response(new ReadableStream({
    start(c) {
      c.enqueue(new TextEncoder().encode(text));
    },
    pull() {
      return new Promise(() => {});
    },
  }), { status, headers: { 'content-type': type } });
  const cases = [
    ['403 JSON that stalls', stalled('{"error":{"type":"permission_error"', 403, 'application/json'), 'not_logged_in'],
    ['403 JSON with an anti-bot marker that stalls', stalled('{"message":"Request rejected by anti-bot rules.', 403, 'application/json'), 'blocked'],
    ['200 non-JSON that stalls', stalled('<html><body>', 200, 'text/html'), 'endpoint_changed'],
  ];
  for (const [name, resp, code] of cases) {
    const f = fakeFetch({ [SESSION]: jsonResponse({ accessToken: TOKEN }), [listURL]: resp });
    const started = Date.now();
    let timer;
    const hang = new Promise((_, reject) => {
      timer = setTimeout(() => reject(new Error(`${name}: sniff did not return`)), 2000);
    });
    try {
      await assert.rejects(Promise.race([run(createRunner({ fetch: f, sniffMs: 50 }), 'chatgpt.list', { count: 1 }), hang]), (e) => e.code === code, name);
    } finally {
      clearTimeout(timer);
    }
    assert.ok(Date.now() - started < 1000, `${name}: took ${Date.now() - started}ms`);
  }
});

test('anti-bot pages are blocked; a plain 401 or a 403 permission error is not_logged_in', async () => {
  const listURL = 'https://chatgpt.com/backend-api/conversations?offset=0&limit=1&order=updated';
  const redirected = (res, url) => {
    Object.defineProperty(res, 'url', { value: url });
    Object.defineProperty(res, 'redirected', { value: true });
    return res;
  };
  const cases = [
    ['cloudflare header on a 403', () => new Response('{}', { status: 403, headers: { 'content-type': 'application/json', 'cf-mitigated': 'challenge' } }), 'blocked'],
    ['cloudflare header on a 503', () => new Response('<html></html>', { status: 503, headers: { 'content-type': 'text/html', 'cf-mitigated': 'challenge' } }), 'blocked'],
    ['challenge page served 200', () => jsonResponse('<!DOCTYPE html><title>Just a moment...</title>', 200, 'text/html'), 'blocked'],
    ['403 JSON with an anti-bot marker', () => jsonResponse({ error: { code: 7, message: 'Request rejected by anti-bot rules.' } }, 403), 'blocked'],
    ['google /sorry/ interstitial', () => redirected(jsonResponse('<html>unusual traffic</html>', 429, 'text/html'), 'https://www.google.com/sorry/index?continue=x'), 'blocked'],
    ['google /sorry/ served 200', () => redirected(jsonResponse('<html></html>', 200, 'text/html'), 'https://www.google.com/sorry/index'), 'blocked'],
    ['plain 401', () => jsonResponse({ detail: 'expired' }, 401), 'not_logged_in'],
    ['401 HTML', () => jsonResponse('<html>Just a moment...</html>', 401, 'text/html'), 'not_logged_in'],
    ['403 permission error', () => jsonResponse({ error: { type: 'permission_error' } }, 403), 'not_logged_in'],
  ];
  for (const [name, resp, code] of cases) {
    const f = fakeFetch({ [SESSION]: jsonResponse({ accessToken: TOKEN }), [listURL]: () => resp() });
    await assert.rejects(run(createRunner({ fetch: f }), 'chatgpt.list', { count: 1 }), (e) => e.code === code, name);
  }
});

test('a session probe redirected to another host is not_logged_in and nothing is sent', async () => {
  const sent = [];
  const sender = { send: async (site) => (sent.push(site), { conversation_id: 'c1' }) };
  const moved = (url) => () => {
    const res = jsonResponse('<html>Log in</html>', 200, 'text/html');
    Object.defineProperty(res, 'url', { value: url });
    Object.defineProperty(res, 'redirected', { value: true });
    return res;
  };
  const chat = createRunner({ fetch: fakeFetch({ [SESSION]: moved('https://auth.openai.com/log-in') }), sender });
  await assert.rejects(run(chat, 'chatgpt.send', { message: 'hi' }), (e) => e.code === 'not_logged_in');
  const claude = createRunner({ fetch: fakeFetch({ 'https://claude.ai/api/organizations': moved('https://accounts.google.com/v3/signin/identifier') }), sender });
  await assert.rejects(run(claude, 'claudeai.send', { message: 'hi' }), (e) => e.code === 'not_logged_in');
  assert.deepEqual(sent, []);
  // A same-host redirect is followed as before.
  const same = fakeFetch({
    [SESSION]: () => {
      const res = jsonResponse({ accessToken: TOKEN });
      Object.defineProperty(res, 'url', { value: 'https://chatgpt.com/api/auth/session?x=1' });
      Object.defineProperty(res, 'redirected', { value: true });
      return res;
    },
  });
  await run(createRunner({ fetch: same, sender }), 'chatgpt.send', { message: 'hi' });
  assert.deepEqual(sent, ['chatgpt']);
});

// ---- grok.com

const GROK_CONV = '0e1d0000-0000-4000-8000-000000000001';
const GROK_BASE = `https://grok.com/rest/app-chat/conversations/${GROK_CONV}`;

test('grok.list reads the conversation list with the session cookies and pages through nextPageToken', async () => {
  const list = fixture('grok/conversations.json');
  const [c1, c2, c3] = list.conversations;
  const f = fakeFetch({
    'https://grok.com/rest/app-chat/conversations?pageSize=3': jsonResponse({ conversations: [c1, c2], nextPageToken: 'tok+/2' }),
    'https://grok.com/rest/app-chat/conversations?pageSize=1&pageToken=tok%2B%2F2': jsonResponse({ conversations: [c3], nextPageToken: 'tok3' }),
  });
  const frames = await run(createRunner({ fetch: f }), 'grok.list', { count: 3 });
  assert.deepEqual(frames, [{ ok: true, result: { conversations: [c1, c2, c3] } }]);
  assert.equal(f.calls.length, 2, 'stops once count conversations are in');
  assert.equal(f.calls[0].init.credentials, 'include');
  assert.equal(f.calls[0].init.method, 'GET');
  assert.equal(f.calls[0].init.headers, undefined, 'no page-set headers such as x-statsig-id');
  // One page answers a small count.
  const one = fakeFetch({ 'https://grok.com/rest/app-chat/conversations?pageSize=2': jsonResponse(list) });
  assert.equal((await run(createRunner({ fetch: one }), 'grok.list', { count: 2 }))[0].result.conversations.length, 2);
  // Not a list: the API changed.
  const bad = fakeFetch({ 'https://grok.com/rest/app-chat/conversations?pageSize=1': jsonResponse({ items: [] }) });
  await assert.rejects(run(createRunner({ fetch: bad }), 'grok.list', { count: 1 }), (e) => e.code === 'endpoint_changed');
});

test('grok.list stops after GROK_MAX_PAGES pages even while the site keeps offering more', async () => {
  const [c1] = fixture('grok/conversations.json').conversations;
  const calls = [];
  const f = async (url) => {
    calls.push(String(url));
    return jsonResponse({ conversations: [{ ...c1, conversationId: `0e1d0000-0000-4000-8000-${String(calls.length).padStart(12, '0')}` }], nextPageToken: `tok${calls.length}` });
  };
  const frames = await run(createRunner({ fetch: f }), 'grok.list', { count: 50 });
  assert.equal(GROK_MAX_PAGES, 5);
  assert.equal(calls.length, GROK_MAX_PAGES);
  assert.equal(frames[0].result.conversations.length, GROK_MAX_PAGES);
  assert.equal(frames[0].result.more, true, 'a list cut short at the page cap says more exist');
});

test('grok.detail combines response-node and load-responses (POST JSON) into one result', async () => {
  const nodes = fixture(`grok/response-node-${GROK_CONV}.json`);
  const loaded = fixture(`grok/load-responses-${GROK_CONV}.json`);
  const f = fakeFetch({
    [`${GROK_BASE}/response-node?includeThreads=true`]: jsonResponse(nodes),
    [`${GROK_BASE}/load-responses`]: (_url, init) => {
      assert.equal(init.method, 'POST');
      assert.equal(init.credentials, 'include');
      assert.equal(init.headers['content-type'], 'application/json');
      assert.deepEqual(Object.keys(init.headers), ['content-type'], 'no page-set headers such as x-statsig-id');
      assert.deepEqual(JSON.parse(init.body), { responseIds: nodes.responseNodes.map((n) => n.responseId) });
      return jsonResponse(loaded);
    },
  });
  const frames = await run(createRunner({ fetch: f }), 'grok.detail', { id: GROK_CONV });
  assert.deepEqual(frames, [{ ok: true, result: { conversationId: GROK_CONV, responseNodes: nodes.responseNodes, inflightResponses: [], responses: loaded.responses } }]);

  // A conversation with no nodes yet needs no body read.
  const empty = fakeFetch({ [`${GROK_BASE}/response-node?includeThreads=true`]: jsonResponse({ responseNodes: [], inflightResponses: [{ responseId: 'r1' }] }) });
  const e = await run(createRunner({ fetch: empty }), 'grok.detail', { id: GROK_CONV });
  assert.deepEqual(e[0].result, { conversationId: GROK_CONV, responseNodes: [], inflightResponses: [{ responseId: 'r1' }], responses: [] });
  assert.equal(empty.calls.length, 1);

  // A deleted conversation is not_found; a moved API endpoint_changed; a 429
  // rate_limited with its Retry-After.
  await assert.rejects(run(createRunner({ fetch: fakeFetch({}) }), 'grok.detail', { id: GROK_CONV }), (x) => x.code === 'not_found');
  const shape = fakeFetch({ [`${GROK_BASE}/response-node?includeThreads=true`]: jsonResponse({ nodes: [] }) });
  await assert.rejects(run(createRunner({ fetch: shape }), 'grok.detail', { id: GROK_CONV }), (x) => x.code === 'endpoint_changed');
  const limited = fakeFetch({ [`${GROK_BASE}/response-node?includeThreads=true`]: () => new Response('{}', { status: 429, headers: { 'content-type': 'application/json', 'retry-after': '120' } }) });
  await assert.rejects(run(createRunner({ fetch: limited }), 'grok.detail', { id: GROK_CONV }), (x) => x.code === 'rate_limited' && x.retryAfter === 120);
});

test('grok.file looks the image URL up again by response and index and fetches it from assets.grok.com', async () => {
  const rid = '5e5f0000-0000-4000-8000-000000000015';
  const loaded = fixture(`grok/load-responses-${GROK_CONV}.json`);
  const img = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 1, 2, 3]);
  const asset = `https://assets.grok.com/${loaded.responses.find((r) => r.responseId === rid).generatedImageUrls[0]}`;
  const route = (responses) => (_url, init) => {
    assert.deepEqual(JSON.parse(init.body), { responseIds: [rid] });
    return jsonResponse({ responses });
  };
  const f = fakeFetch({ [`${GROK_BASE}/load-responses`]: route(loaded.responses), [asset]: bytesResponse(img) });
  const frames = await run(createRunner({ fetch: f }), 'grok.file', { file_id: `${rid}_0`, conversation_id: GROK_CONV });
  assert.equal(frames.length, 1);
  assert.equal(frames[0].mime, 'image/png');
  assert.deepEqual(Buffer.from(frames[0].chunk.data, 'base64'), Buffer.from(img));
  assert.equal(f.calls[1].url, asset);
  assert.equal(f.calls[1].init.credentials, 'include');

  // No image at that index, or the response is gone: not_found.
  await assert.rejects(run(createRunner({ fetch: f }), 'grok.file', { file_id: `${rid}_1`, conversation_id: GROK_CONV }), (e) => e.code === 'not_found');
  const gone = fakeFetch({ [`${GROK_BASE}/load-responses`]: route([]) });
  await assert.rejects(run(createRunner({ fetch: gone }), 'grok.file', { file_id: `${rid}_0`, conversation_id: GROK_CONV }), (e) => e.code === 'not_found');
  // An image URL on any other host is refused without a fetch.
  for (const u of ['https://evil.example/x.png', 'http://assets.grok.com/x.png', 'https://assets.grok.com.evil.example/x.png', 'https://u:p@assets.grok.com/x.png']) {
    const other = fakeFetch({ [`${GROK_BASE}/load-responses`]: route([{ responseId: rid, generatedImageUrls: [u] }]) });
    await assert.rejects(run(createRunner({ fetch: other }), 'grok.file', { file_id: `${rid}_0`, conversation_id: GROK_CONV }), (e) => e.code === 'endpoint_changed', u);
    assert.equal(other.calls.length, 1, u);
  }
  // A file id that is not <response>_<index>.
  for (const bad of ['r1', 'r1_x', 'r1_01', 'r1_100', '_0']) {
    await assert.rejects(run(createRunner({ fetch: fakeFetch({}) }), 'grok.file', { file_id: bad, conversation_id: GROK_CONV }), (e) => e.code === 'bad_request', bad);
  }
});

test('grok ops need the grok.com grant; ChatGPT and claude.ai do not change', async () => {
  const f = fakeFetch({ 'https://grok.com/rest/app-chat/conversations?pageSize=1': jsonResponse({ conversations: [] }) });
  const sent = [];
  const sender = { send: async (site) => (sent.push(site), { conversation_id: 'c1' }), close: async () => ({ closed: 1 }) };
  const perms = fakePermissions(['https://chatgpt.com/*', 'https://*.oaiusercontent.com/*', 'https://claude.ai/*', 'https://assets.grok.com/*']);
  const r = createRunner({ fetch: f, sender, permissions: perms });
  // The image host alone is not enough: grok.com is Grok's page origin.
  for (const [op, args] of [['grok.send', { message: 'hi' }], ['grok.list', { count: 1 }], ['grok.detail', { id: 'c1' }], ['grok.file', { file_id: 'r1_0', conversation_id: 'c1' }]]) {
    await assert.rejects(run(r, op, args), (e) => e.code === 'permission_missing' && /Grok/.test(e.message), op);
  }
  assert.equal(f.calls.length, 0);
  assert.deepEqual(sent, []);
  assert.deepEqual(await run(r, 'grok.close', { conversation_id: 'c1' }), [{ ok: true, result: { closed: 1 } }]);
  assert.deepEqual(await grantedSites(perms), ['chatgpt', 'claudeai']);
  // grok.com without its image host is granted: only image downloads need assets.grok.com.
  perms.granted.delete('https://assets.grok.com/*');
  perms.granted.add('https://grok.com/*');
  await run(r, 'grok.send', { message: 'hi' });
  assert.deepEqual(sent, ['grok']);
  assert.deepEqual(await grantedSites(perms), ['chatgpt', 'claudeai', 'grok']);
});

// ---- Gemini: the app page's session values, then batchexecute.

const GEMINI_APP = 'https://gemini.google.com/app';
const GEMINI_RPC = 'https://gemini.google.com/_/BardChatUi/data/batchexecute';
const APP_HTML = '<html><script>window.WIZ_global_data = {"SNlM0e":"dummy-at-token","cfb2h":"boq_dummy_20260927.00_p0","FdrFJe":"-1234567890"};</script></html>';
const GEMINI_GRANT = ['https://gemini.google.com/*', 'https://lh3.googleusercontent.com/*'];

// batchChunks renders rows as batchexecute's answer: the guard, then
// length-prefixed chunks, the wrb.fr one first.
function batchChunks(row) {
  const chunk = (v) => {
    const line = JSON.stringify(v);
    return `${line.length}\n${line}\n`;
  };
  return ")]}'\n\n" + chunk([row, ['di', 42], ['af.httprm', 41, '-1', 7]]) + chunk([['e', 4, null, null, 120]]);
}

// The error rows batchexecute sent live (2026-09-27) in place of a
// payload: a missing or deleted conversation, and a payload it could not
// take.
const NOT_FOUND_ROW = (rpcid) => ['wrb.fr', rpcid, null, null, null, [5, null, [['type.googleapis.com/assistant.boq.bard.application.BardErrorInfo', [1167]]]], 'generic'];
const MALFORMED_ROW = (rpcid) => ['wrb.fr', rpcid, null, null, null, [3], 'generic'];

// batchResponse renders inner as batchexecute's answer for rpcid; null
// renders the malformed-payload error row.
function batchResponse(rpcid, inner, status = 200) {
  const row = inner === null ? MALFORMED_ROW(rpcid) : ['wrb.fr', rpcid, JSON.stringify(inner), null, null, null, 'generic'];
  return new Response(batchChunks(row), { status, headers: { 'content-type': 'application/json; charset=utf-8' } });
}

function batchRow(row) {
  return new Response(batchChunks(row), { status: 200, headers: { 'content-type': 'application/json; charset=utf-8' } });
}

// geminiFetch answers the app page and batchexecute: rpc(rpcid, payload,
// call) returns the Response for each batchexecute call.
function geminiFetch({ app = () => new Response(APP_HTML, { status: 200, headers: { 'content-type': 'text/html' } }), rpc, other = {} }) {
  const calls = [];
  const fn = async (url, init = {}) => {
    const u = new URL(String(url));
    calls.push({ url: String(url), init });
    if (u.origin + u.pathname === GEMINI_APP) return app();
    if (u.origin + u.pathname === GEMINI_RPC) {
      const form = new URLSearchParams(init.body);
      const req = JSON.parse(form.get('f.req'));
      const [rpcid, payload] = req[0][0];
      return rpc(rpcid, JSON.parse(payload), { url: u, form });
    }
    const h = other[String(url)];
    if (h) return h();
    return jsonResponse({}, 404);
  };
  fn.calls = calls;
  return fn;
}

function redirectedTo(res, url) {
  Object.defineProperty(res, 'url', { value: url });
  Object.defineProperty(res, 'redirected', { value: true });
  return res;
}

test('gemini.list reads MaZiqc page by page with the app page session, which never leaves the worker', async () => {
  const pages = fixture('gemini/list.json').pages;
  const payloads = [];
  const f = geminiFetch({
    rpc: (rpcid, payload, { url, form }) => {
      assert.equal(rpcid, 'MaZiqc');
      assert.equal(form.get('at'), 'dummy-at-token');
      assert.equal(url.searchParams.get('bl'), 'boq_dummy_20260927.00_p0');
      assert.equal(url.searchParams.get('f.sid'), '-1234567890');
      assert.equal(url.searchParams.get('rpcids'), 'MaZiqc');
      assert.equal(url.searchParams.get('source-path'), '/app');
      assert.equal(url.searchParams.get('rt'), 'c');
      payloads.push(payload);
      return batchResponse('MaZiqc', payload[1] === null ? pages[0] : pages[1]);
    },
  });
  const r = createRunner({ fetch: f });
  const frames = await run(r, 'gemini.list', { count: 10 });
  // Exactly the list fixture the Go reader parses.
  assert.deepEqual(frames, [{ ok: true, result: { pages } }]);
  assert.deepEqual(payloads, [[13, null, [0, null, 1]], [13, 'dummy-page-token-2', [0, null, 1]]]);
  assert.ok(!JSON.stringify(frames).includes('dummy-at-token'));
  const rpcCalls = f.calls.filter((c) => c.url.startsWith(GEMINI_RPC));
  for (const c of rpcCalls) {
    assert.equal(c.init.method, 'POST');
    assert.equal(c.init.credentials, 'include');
    assert.match(c.init.headers['content-type'], /^application\/x-www-form-urlencoded/);
  }
  assert.notEqual(new URL(rpcCalls[0].url).searchParams.get('_reqid'), new URL(rpcCalls[1].url).searchParams.get('_reqid'));
  // A small count stops after the first page; the session is reused.
  const again = await run(r, 'gemini.list', { count: 2 });
  assert.equal(again[0].result.pages.length, 1);
  assert.equal(f.calls.filter((c) => c.url === GEMINI_APP).length, 1, 'the app page is fetched once');
});

test('gemini.list: an error row on any page is endpoint_changed; only a page with no token ends the list', async () => {
  const pages = fixture('gemini/list.json').pages;
  const rows = [MALFORMED_ROW('MaZiqc'), NOT_FOUND_ROW('MaZiqc'), ['wrb.fr', 'MaZiqc', null, null, null, null, 'generic']];
  for (const row of rows) {
    let f = geminiFetch({ rpc: () => batchRow(row) });
    await assert.rejects(run(createRunner({ fetch: f }), 'gemini.list', { count: 10 }), (e) => e.code === 'endpoint_changed', JSON.stringify(row));
    // A later page's error row never turns the earlier pages into a
    // complete list.
    f = geminiFetch({ rpc: (_rpcid, payload) => (payload[1] === null ? batchResponse('MaZiqc', pages[0]) : batchRow(row)) });
    await assert.rejects(run(createRunner({ fetch: f }), 'gemini.list', { count: 100 }), (e) => e.code === 'endpoint_changed', JSON.stringify(row));
  }
  // A page with no next-page token is the genuine end.
  const f = geminiFetch({ rpc: (_rpcid, payload) => batchResponse('MaZiqc', payload[1] === null ? pages[0] : pages[1]) });
  assert.deepEqual(await run(createRunner({ fetch: f }), 'gemini.list', { count: 100 }), [{ ok: true, result: { pages } }]);
  assert.equal(f.calls.filter((c) => c.url.startsWith(GEMINI_RPC)).length, 2);
});

test('gemini.detail reads hNvQHb with the c_ id; a missing conversation is not_found', async () => {
  const inner = fixture('gemini/conversation-00000000000000a1.json');
  const f = geminiFetch({
    rpc: (rpcid, payload) => {
      assert.equal(rpcid, 'hNvQHb');
      if (payload[0] === 'c_00000000000000a1') return batchResponse('hNvQHb', inner);
      if (payload[0] === 'c_00000000000000ee') return batchResponse('hNvQHb', null);
      return batchRow(NOT_FOUND_ROW('hNvQHb'));
    },
  });
  const r = createRunner({ fetch: f });
  assert.deepEqual(await run(r, 'gemini.detail', { id: '00000000000000a1' }), [{ ok: true, result: inner }]);
  const sent = JSON.parse(JSON.parse(new URLSearchParams(f.calls.at(-1).init.body).get('f.req'))[0][0][1]);
  assert.deepEqual(sent, ['c_00000000000000a1', 10, null, 1, [0], [4], null, 1]);
  await assert.rejects(run(r, 'gemini.detail', { id: '00000000000000ff' }), (e) => e.code === 'not_found');
  // A payload the site could not take (code 3) is not a missing conversation.
  await assert.rejects(run(r, 'gemini.detail', { id: '00000000000000ee' }), (e) => e.code === 'endpoint_changed');
  await assert.rejects(run(r, 'gemini.detail', { id: 'c_00000000000000a1' }), (e) => e.code === 'bad_request');
  await assert.rejects(run(r, 'gemini.detail', { id: 'abc-1' }), (e) => e.code === 'bad_request');
});

test('parseBatchexecute: anything but a wrb.fr answer for the rpcid is endpoint_changed', () => {
  const ok = ")]}'\n\n40\n" + JSON.stringify([['wrb.fr', 'X', '[1,2]', null]]) + '\n';
  assert.deepEqual(parseBatchexecute(ok, 'X'), [1, 2]);
  // The live error rows: code 5 on hNvQHb is not_found; code 3, code 5
  // on another rpcid, or no payload and no code is endpoint_changed.
  assert.throws(() => parseBatchexecute(batchChunks(NOT_FOUND_ROW('hNvQHb')), 'hNvQHb'), (e) => e.code === 'not_found');
  assert.throws(() => parseBatchexecute(batchChunks(MALFORMED_ROW('hNvQHb')), 'hNvQHb'), (e) => e.code === 'endpoint_changed' && /3/.test(e.message));
  assert.throws(() => parseBatchexecute(batchChunks(MALFORMED_ROW('MaZiqc')), 'MaZiqc'), (e) => e.code === 'endpoint_changed');
  assert.throws(() => parseBatchexecute(batchChunks(NOT_FOUND_ROW('MaZiqc')), 'MaZiqc'), (e) => e.code === 'endpoint_changed');
  assert.throws(() => parseBatchexecute(JSON.stringify([['wrb.fr', 'X', null]]), 'X'), (e) => e.code === 'endpoint_changed');
  for (const text of ['', ")]}'\n", '<html>nope</html>', JSON.stringify([['wrb.fr', 'Y', '[1]']]), JSON.stringify([['wrb.fr', 'X', '{bad json']]), JSON.stringify([['wrb.fr', 'X', 5]]), undefined]) {
    assert.throws(() => parseBatchexecute(text, 'X'), (e) => e.code === 'endpoint_changed', String(text));
  }
});

test('gemini: /sorry/ is blocked, a sign-in redirect or no session is not_logged_in, a stale token is refetched once', async () => {
  const html = (body = APP_HTML) => new Response(body, { status: 200, headers: { 'content-type': 'text/html' } });
  const cases = [
    ['app page on /sorry/', { app: () => redirectedTo(html('<html>unusual traffic</html>'), 'https://www.google.com/sorry/index?continue=x') }, 'blocked'],
    ['batchexecute on /sorry/', { rpc: () => redirectedTo(new Response('<html></html>', { status: 429, headers: { 'content-type': 'text/html' } }), 'https://www.google.com/sorry/index') }, 'blocked'],
    ['sign-in redirect', { app: () => redirectedTo(html('<html>Sign in</html>'), 'https://accounts.google.com/v3/signin/identifier') }, 'not_logged_in'],
    ['no session on the page', { app: () => html('<html>{"cfb2h":"boq_x"}</html>') }, 'not_logged_in'],
    ['no build label', { app: () => html('<html>{"SNlM0e":"tok"}</html>') }, 'endpoint_changed'],
    ['rate limited', { rpc: () => new Response('', { status: 429, headers: { 'retry-after': '30' } }) }, 'rate_limited'],
    ['odd payload', { rpc: () => new Response(")]}'\n\n5\n[[1]]\n", { status: 200, headers: { 'content-type': 'application/json' } }) }, 'endpoint_changed'],
  ];
  for (const [name, routes, code] of cases) {
    const f = geminiFetch({ rpc: () => batchResponse('MaZiqc', [null, null, []]), ...routes });
    await assert.rejects(run(createRunner({ fetch: f }), 'gemini.list', { count: 1 }), (e) => e.code === code, name);
  }
  let n = 0;
  const f = geminiFetch({ rpc: () => (++n === 1 ? new Response('', { status: 400 }) : batchResponse('MaZiqc', [null, null, []])) });
  assert.deepEqual(await run(createRunner({ fetch: f }), 'gemini.list', { count: 1 }), [{ ok: true, result: { pages: [[null, null, []]] } }]);
  assert.equal(f.calls.filter((c) => c.url === GEMINI_APP).length, 2, 'the session is fetched again after a 400');
});

test('geminiImageURL finds the n-th image of a response on the image host only', () => {
  const inner = fixture('gemini/conversation-00000000000000a1.json');
  assert.equal(geminiImageURL(inner, 'rc_00000000000000b2', 0), 'https://lh3.googleusercontent.com/gg/dummy-star-1');
  assert.equal(geminiImageURL(inner, 'rc_00000000000000b2', 1), null);
  assert.equal(geminiImageURL(inner, 'rc_00000000000000b1', 0), null);
  assert.equal(geminiImageURL(inner, 'rc_missing', 0), null);
  const cand = ['rc_9', ['text https://lh3.googleusercontent.com/in-text'], ['https://evil.example/x.png', 'https://lh3.googleusercontent.com/a', ['https://lh3.googleusercontent.com/b', 'https://lh3.googleusercontent.com/a'], 'https://lh3.googleusercontent.com:8443/c']];
  const odd = [[[['c_1', 'r_1'], null, [['hi']], [[cand]]]]];
  assert.deepEqual(geminiImageURLs(cand), ['https://lh3.googleusercontent.com/a', 'https://lh3.googleusercontent.com/b']);
  assert.equal(geminiImageURL(odd, 'rc_9', 1), 'https://lh3.googleusercontent.com/b');
  assert.equal(geminiImageURL('x', 'rc_9', 0), null);
});

test('gemini.file captures the image in the send tab first, then fetches it in the worker, and never takes a URL', async () => {
  const inner = fixture('gemini/conversation-00000000000000a1.json');
  const png = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 1, 2, 3, 4]);
  const IMG = 'https://lh3.googleusercontent.com/gg/dummy-star-1';
  const args = { file_id: 'rc_00000000000000b2-0', conversation_id: '00000000000000a1' };
  const mk = (capture, image) => {
    const captured = [];
    const sender = { capture: async (site, conv, url, max) => (captured.push([site, conv, url, max]), capture()) };
    const f = geminiFetch({ rpc: () => batchResponse('hNvQHb', inner), other: { [IMG]: image } });
    return { r: createRunner({ fetch: f, sender }), f, captured };
  };
  // 1. Captured in the page.
  let t = mk(() => ({ ok: true, mime: 'image/png', data: Buffer.from(png).toString('base64') }), () => bytesResponse(png));
  let frames = await run(t.r, 'gemini.file', args);
  assert.deepEqual(t.captured, [['gemini', '00000000000000a1', IMG, MAX_FILE_BYTES]]);
  assert.equal(frames.length, 1);
  assert.deepEqual(Buffer.from(frames[0].chunk.data, 'base64'), Buffer.from(png));
  assert.equal(frames[0].mime, 'image/png');
  assert.ok(!t.f.calls.some((c) => c.url === IMG), 'no worker fetch once captured');
  // 2. The page could not: the worker fetches it with the image host's grant.
  t = mk(() => null, () => bytesResponse(png));
  frames = await run(t.r, 'gemini.file', args);
  assert.deepEqual(Buffer.from(frames[0].chunk.data, 'base64'), Buffer.from(png));
  assert.equal(t.f.calls.find((c) => c.url === IMG).init.credentials, 'include');
  // A capture answer that is not an image is not trusted.
  t = mk(() => ({ ok: true, mime: 'text/html', data: 'PGh0bWw+' }), () => bytesResponse(png));
  await run(t.r, 'gemini.file', args);
  assert.ok(t.f.calls.some((c) => c.url === IMG));
  // 3. Neither: the op fails, and the Go side notes the lost image.
  t = mk(() => null, () => new Response('', { status: 403, headers: { 'content-type': 'text/plain' } }));
  await assert.rejects(run(t.r, 'gemini.file', args), (e) => e instanceof OpError);
  // The worker fetch must end on the image host: a redirect to a sign-in
  // page is not_logged_in, and nothing is emitted.
  t = mk(() => null, () => redirectedTo(bytesResponse(png), 'https://accounts.google.com/ServiceLogin'));
  await assert.rejects(run(t.r, 'gemini.file', args), (e) => e.code === 'not_logged_in');
  t = mk(() => null, () => redirectedTo(bytesResponse(png), 'https://lh3.googleusercontent.com/gg/dummy-star-1=s0'));
  frames = await run(t.r, 'gemini.file', args);
  assert.deepEqual(Buffer.from(frames[0].chunk.data, 'base64'), Buffer.from(png));
  // An image that is not in the conversation is not_found, with no capture.
  t = mk(() => null, () => bytesResponse(png));
  await assert.rejects(run(t.r, 'gemini.file', { ...args, file_id: 'rc_00000000000000b2-3' }), (e) => e.code === 'not_found');
  assert.deepEqual(t.captured, []);
  assert.throws(() => validate({ id: 1, op: 'gemini.file', args: { file_id: 'rc_1-0' } }), (e) => e.code === 'bad_request', 'conversation id required');
  assert.throws(() => validate({ id: 1, op: 'gemini.file', args: { file_id: 'https://lh3.googleusercontent.com/x', conversation_id: '00000000000000a1' } }), (e) => e.code === 'bad_request');
  await assert.rejects(run(t.r, 'gemini.file', { ...args, file_id: 'rc_1' }), (e) => e.code === 'bad_request');
});

test('gemini.send checks the session fresh first; logged out or blocked opens no tab', async () => {
  const sent = [];
  const sender = { send: async (site, a) => (sent.push([site, a]), { conversation_id: '00000000000000d1', url: '', submitted_at: 1 }) };
  const ok = geminiFetch({ rpc: () => batchResponse('MaZiqc', [null, null, []]) });
  const r = createRunner({ fetch: ok, sender });
  await run(r, 'gemini.list', { count: 1 });
  const frames = await run(r, 'gemini.send', { message: 'hi', conversation_id: '00000000000000d1' });
  assert.equal(frames[0].result.conversation_id, '00000000000000d1');
  assert.deepEqual(sent, [['gemini', { message: 'hi', conversation_id: '00000000000000d1' }]]);
  assert.equal(ok.calls.filter((c) => c.url === GEMINI_APP).length, 2, 'the send asked the app page again');
  await assert.rejects(run(r, 'gemini.send', { message: 'hi', conversation_id: 'abc-1' }), (e) => e.code === 'bad_request');
  const out = geminiFetch({ app: () => redirectedTo(new Response('<html></html>', { status: 200, headers: { 'content-type': 'text/html' } }), 'https://accounts.google.com/ServiceLogin') });
  await assert.rejects(run(createRunner({ fetch: out, sender }), 'gemini.send', { message: 'hi' }), (e) => e.code === 'not_logged_in');
  const sorry = geminiFetch({ app: () => redirectedTo(new Response('', { status: 429, headers: { 'content-type': 'text/html' } }), 'https://www.google.com/sorry/index') });
  await assert.rejects(run(createRunner({ fetch: sorry, sender }), 'gemini.send', { message: 'hi' }), (e) => e.code === 'blocked');
  assert.equal(sent.length, 1);
});

test('gemini ops need the Gemini page grant; the options page grants the image host with it', async () => {
  const f = geminiFetch({ rpc: () => batchResponse('MaZiqc', [null, null, []]) });
  const perms = fakePermissions(['https://chatgpt.com/*', 'https://*.oaiusercontent.com/*', 'https://claude.ai/*', 'https://lh3.googleusercontent.com/*']);
  const r = createRunner({ fetch: f, permissions: perms });
  await assert.rejects(run(r, 'gemini.list', { count: 1 }), (e) => e.code === 'permission_missing' && /Gemini/.test(e.message));
  assert.equal(f.calls.length, 0);
  perms.granted.delete('https://lh3.googleusercontent.com/*');
  perms.granted.add('https://gemini.google.com/*');
  await run(r, 'gemini.list', { count: 1 });
  assert.deepEqual(SITE_ACCESS.gemini.origins, GEMINI_GRANT);
  assert.deepEqual(SITE_ACCESS.gemini.pageOrigins, ['https://gemini.google.com/*']);
  assert.equal(SITE_ACCESS.gemini.required, false, 'an optional site: upgrading asks for nothing');
});

// ---- Copilot.

const COPILOT_ID = 'c0b1107a-0000-4000-8000-000000000001';
const COPILOT_PAGE = `https://copilot.com/chat/conversation/${COPILOT_ID}`;

test('copilot.detail reads the conversation page JSON with the cookies and returns only the fields Go reads', async () => {
  const page = fixture(`copilot/page-${COPILOT_ID}.json`);
  const f = fakeFetch({ [COPILOT_PAGE]: jsonResponse(page) });
  const r = createRunner({ fetch: f });
  const frames = await run(r, 'copilot.detail', { id: COPILOT_ID });
  assert.equal(f.calls.length, 1);
  assert.equal(f.calls[0].url, COPILOT_PAGE);
  assert.equal(f.calls[0].init.credentials, 'include');
  assert.equal(f.calls[0].init.headers.accept, 'application/json');
  assert.equal(f.calls[0].init.headers.authorization, undefined, 'no token is sent');
  assert.deepEqual(frames, [{ ok: true, result: fixture(`copilot/detail-${COPILOT_ID}.json`) }]);
  // The reconnect token and the token-bearing telemetry never leave.
  const out = JSON.stringify(frames);
  for (const secret of ['reconnect', 'AccessToken', 'dummy-access-token', 'telemetry', 'requestId', 'Suggestion', 'InternalSearchQuery', 'javascript:']) {
    assert.ok(!out.includes(secret), `result carries ${secret}`);
  }
  assert.deepEqual(copilotConversation(page, COPILOT_ID), frames[0].result);
});

test('copilot.detail: a bad id is refused before any fetch; sign-in redirects, 404s and HTML are classified', async () => {
  const f = fakeFetch({});
  const r = createRunner({ fetch: f });
  await assert.rejects(run(r, 'copilot.detail', { id: 'not-a-uuid' }), (e) => e.code === 'bad_request');
  await assert.rejects(run(r, 'copilot.detail', { id: COPILOT_ID.toUpperCase() }), (e) => e.code === 'bad_request');
  assert.equal(f.calls.length, 0);
  const cases = [
    ['a sign-in redirect', () => Object.defineProperty(jsonResponse({}), 'url', { value: 'https://login.live.com/oauth20_authorize.srf?x=1' }), 'not_logged_in'],
    ['the terms page', () => Object.defineProperty(jsonResponse('<html>terms</html>', 200, 'text/html'), 'url', { value: 'https://account.live.com/tou/accrue?x=1' }), 'not_logged_in'],
    ['a 401', () => jsonResponse({}, 401), 'not_logged_in'],
    ['a 404', () => jsonResponse({}, 404), 'not_found'],
    ['the app HTML', () => jsonResponse('<html>app</html>', 200, 'text/html'), 'endpoint_changed'],
    ['JSON without a conversation', () => jsonResponse({ store: {} }), 'endpoint_changed'],
    ['a 429', () => jsonResponse({}, 429), 'rate_limited'],
  ];
  for (const [name, res, code] of cases) {
    const r2 = createRunner({ fetch: fakeFetch({ [COPILOT_PAGE]: res }) });
    await assert.rejects(run(r2, 'copilot.detail', { id: COPILOT_ID }), (e) => e.code === code, name);
  }
});

test('copilotConversation keeps user and bot content only, with http(s) sources deduped and capped', () => {
  const many = Array.from({ length: 60 }, (_, i) => ({ providerDisplayName: `S${i}`, seeMoreUrl: `https://s${i}.example/` }));
  const got = copilotConversation(
    {
      store: {
        rawConversationResponse: {
          conversationId: COPILOT_ID.toUpperCase(),
          messages: [
            null,
            { author: 'user', text: 'q', messageId: '../x', createdAt: 5 },
            { author: 'bot', text: 'a', messageId: 'b1', sourceAttributions: many },
            { author: 'bot', messageType: 'Progress', text: 'searching' },
            { author: 'system', text: 'hidden' },
          ],
        },
      },
    },
    COPILOT_ID,
  );
  assert.equal(got.conversationId, COPILOT_ID, 'ids are lowercased');
  assert.deepEqual(got.messages.map((m) => [m.author, m.text, m.id, m.createdAt]), [['user', 'q', '', ''], ['bot', 'a', 'b1', '']]);
  assert.equal(got.messages[1].sources.length, 50);
  assert.throws(() => copilotConversation({ store: { rawConversationResponse: {} } }, COPILOT_ID), (e) => e.code === 'endpoint_changed');
  assert.throws(() => copilotConversation(null, COPILOT_ID), (e) => e.code === 'endpoint_changed');
});

test('copilot.list and copilot.send go to the sender; nothing is fetched and ids are checked', async () => {
  const f = fakeFetch({});
  const calls = [];
  const sender = {
    readList: async (site, count) => (calls.push(['list', site, count]), { conversations: [] }),
    send: async (site, a) => (calls.push(['send', site, a]), { conversation_id: COPILOT_ID, url: '', submitted_at: 1 }),
    close: async (site, id) => (calls.push(['close', site, id]), { closed: 1 }),
  };
  const r = createRunner({ fetch: f, sender });
  assert.deepEqual(await run(r, 'copilot.list', { count: 7 }), [{ ok: true, result: { conversations: [] } }]);
  await run(r, 'copilot.send', { message: 'hi', conversation_id: COPILOT_ID });
  await assert.rejects(run(r, 'copilot.send', { message: 'hi', conversation_id: 'abc' }), (e) => e.code === 'bad_request');
  await run(r, 'copilot.close', { conversation_id: COPILOT_ID });
  assert.deepEqual(calls, [['list', 'copilot', 7], ['send', 'copilot', { message: 'hi', conversation_id: COPILOT_ID }], ['close', 'copilot', COPILOT_ID]]);
  assert.equal(f.calls.length, 0);
  // An older sender without readList: the list is unsupported.
  const old = createRunner({ fetch: f, sender: { send: sender.send, close: sender.close } });
  await assert.rejects(run(old, 'copilot.list', { count: 1 }), (e) => e.code === 'unsupported');
});

test('copilot ops need the copilot.com grant; the options page grants copilot.microsoft.com with it', async () => {
  const f = fakeFetch({ [COPILOT_PAGE]: jsonResponse(fixture(`copilot/page-${COPILOT_ID}.json`)) });
  const sent = [];
  const sender = { readList: async () => (sent.push('list'), {}), send: async () => (sent.push('send'), {}), close: async () => ({ closed: 0 }) };
  const perms = fakePermissions(['https://chatgpt.com/*', 'https://claude.ai/*', 'https://copilot.microsoft.com/*']);
  const r = createRunner({ fetch: f, sender, permissions: perms });
  for (const [op, args] of [['copilot.detail', { id: COPILOT_ID }], ['copilot.list', { count: 1 }], ['copilot.send', { message: 'hi' }]]) {
    await assert.rejects(run(r, op, args), (e) => e.code === 'permission_missing' && /Copilot/.test(e.message), op);
  }
  assert.equal(f.calls.length, 0);
  assert.deepEqual(sent, []);
  assert.deepEqual(await run(r, 'copilot.close', { conversation_id: COPILOT_ID }), [{ ok: true, result: { closed: 0 } }]);
  perms.granted.add('https://copilot.com/*');
  assert.equal((await run(r, 'copilot.detail', { id: COPILOT_ID }))[0].ok, true);
  assert.deepEqual(await grantedSites(perms), ['chatgpt', 'claudeai', 'copilot']);
  assert.deepEqual(SITE_ACCESS.copilot.origins, ['https://copilot.com/*', 'https://copilot.microsoft.com/*']);
});

// ---- Perplexity: a signed-in session check, then /rest/thread reads.

const PX = 'https://www.perplexity.ai';
const PX_SESSION = `${PX}/api/auth/session`;
const PX_SLUG = '0e1d0000-0000-4000-8000-0000000000a1';
const PX_FIRST = `${PX}/rest/thread/${PX_SLUG}?with_parent_info=true&with_schematized_response=true&version=2.18&source=default&limit=10&offset=0&from_first=true`;
const pxPage = (cursor) => `${PX}/rest/thread/${PX_SLUG}?with_parent_info=true&with_schematized_response=true&version=2.18&source=default&limit=10&cursor=${cursor}`;
const PX_USER = { expires: '2026-12-31T00:00:00.000Z', user: { id: 'dummy-user-id', email: 'owner@example.com', username: 'dummy' } };

test('perplexity.detail reads the thread page by page and returns only the entry fields the Go side reads', async () => {
  const thread = fixture(`perplexity/thread-${PX_SLUG}.json`);
  const [e1, e2] = thread.entries;
  const f = fakeFetch({
    [PX_FIRST]: jsonResponse({ status: 'success', entries: [{ ...e1, read_write_token: 'dummy-thread-token', author_username: 'dummy' }], has_next_page: true, next_cursor: 'cur+/1' }),
    [pxPage('cur%2B%2F1')]: jsonResponse({ status: 'success', entries: [e2], has_next_page: false, next_cursor: null }),
  });
  const frames = await run(createRunner({ fetch: f }), 'perplexity.detail', { id: PX_SLUG });
  assert.equal(frames.length, 1);
  const r = frames[0].result;
  assert.equal(r.slug, PX_SLUG);
  assert.equal(r.more, undefined);
  assert.deepEqual(r.entries, [e1, e2].map((e) => Object.fromEntries(Object.entries(e).filter(([k]) => PERPLEXITY_ENTRY_FIELDS.includes(k)))));
  assert.ok(!JSON.stringify(r).includes('dummy-thread-token'), 'the thread token never leaves the worker');
  assert.equal(f.calls.length, 2);
  assert.equal(f.calls[0].init.credentials, 'include');
  assert.equal(f.calls[0].init.method, 'GET');

  // A deleted thread is not_found; a moved API endpoint_changed; a 429
  // rate_limited; a Cloudflare challenge blocked.
  await assert.rejects(run(createRunner({ fetch: fakeFetch({}) }), 'perplexity.detail', { id: PX_SLUG }), (x) => x.code === 'not_found');
  for (const body of [{ status: 'success' }, { entries: {} }, { entries: ['e1'] }, [1]]) {
    const shape = fakeFetch({ [PX_FIRST]: jsonResponse(body) });
    await assert.rejects(run(createRunner({ fetch: shape }), 'perplexity.detail', { id: PX_SLUG }), (x) => x.code === 'endpoint_changed', JSON.stringify(body));
  }
  const limited = fakeFetch({ [PX_FIRST]: () => new Response('{}', { status: 429, headers: { 'content-type': 'application/json', 'retry-after': '60' } }) });
  await assert.rejects(run(createRunner({ fetch: limited }), 'perplexity.detail', { id: PX_SLUG }), (x) => x.code === 'rate_limited' && x.retryAfter === 60);
  const cf = fakeFetch({ [PX_FIRST]: () => new Response('<title>Just a moment...</title>', { status: 403, headers: { 'content-type': 'text/html', 'cf-mitigated': 'challenge' } }) });
  await assert.rejects(run(createRunner({ fetch: cf }), 'perplexity.detail', { id: PX_SLUG }), (x) => x.code === 'blocked');
  // Only valid ids reach a URL.
  assert.throws(() => validate({ id: 1, op: 'perplexity.detail', args: { id: '../x' } }), (e) => e.code === 'bad_request');
  assert.throws(() => validate({ id: 1, op: 'perplexity.list', args: { count: 1 } }), (e) => e.code === 'bad_request');
  assert.throws(() => validate({ id: 1, op: 'perplexity.file', args: { file_id: 'x' } }), (e) => e.code === 'bad_request');
});

test('perplexity.detail stops after PERPLEXITY_MAX_PAGES pages and says more exist', async () => {
  const [e1] = fixture(`perplexity/thread-${PX_SLUG}.json`).entries;
  const calls = [];
  const f = async (url) => {
    calls.push(String(url));
    return jsonResponse({ entries: [{ ...e1, uuid: `e-${calls.length}` }], has_next_page: true, next_cursor: `c${calls.length}` });
  };
  const frames = await run(createRunner({ fetch: f }), 'perplexity.detail', { id: PX_SLUG });
  assert.equal(calls.length, PERPLEXITY_MAX_PAGES);
  assert.equal(frames[0].result.entries.length, PERPLEXITY_MAX_PAGES);
  assert.equal(frames[0].result.more, true);
  // A cursor that does not move, or none, ends the read, still marked
  // more: the newest entries were not read.
  const stuck = [];
  const s = async (url) => (stuck.push(String(url)), jsonResponse({ entries: [], has_next_page: true, next_cursor: 'same' }));
  const st = await run(createRunner({ fetch: s }), 'perplexity.detail', { id: PX_SLUG });
  assert.equal(stuck.length, 2);
  assert.equal(st[0].result.more, true);
  const none = async () => jsonResponse({ entries: [], has_next_page: true, next_cursor: null });
  assert.equal((await run(createRunner({ fetch: none }), 'perplexity.detail', { id: PX_SLUG }))[0].result.more, true);
});

test('perplexity.send needs a signed-in user first; signed out, a sign-in redirect or a challenge opens no tab', async () => {
  const sent = [];
  const sender = { send: async (site, a) => (sent.push([site, a]), { conversation_id: PX_SLUG, url: '', submitted_at: 1 }), close: async () => ({ closed: 1 }) };
  const ok = fakeFetch({ [PX_SESSION]: jsonResponse(PX_USER) });
  const frames = await run(createRunner({ fetch: ok, sender }), 'perplexity.send', { message: 'hi', conversation_id: PX_SLUG });
  assert.equal(frames[0].result.conversation_id, PX_SLUG);
  assert.ok(!JSON.stringify(frames).includes('owner@example.com') && !JSON.stringify(frames).includes('dummy-user-id'), 'the session answer is never returned');
  assert.deepEqual(sent, [['perplexity', { message: 'hi', conversation_id: PX_SLUG }]]);
  assert.equal(ok.calls[0].init.credentials, 'include');
  // Signed out Perplexity answers {} (and still offers an anonymous composer).
  for (const body of [{}, { user: null }, { user: {} }, { user: { id: '' } }, { user: 'x' }, []]) {
    const out = fakeFetch({ [PX_SESSION]: jsonResponse(body) });
    await assert.rejects(run(createRunner({ fetch: out, sender }), 'perplexity.send', { message: 'hi' }), (e) => e.code === 'not_logged_in', JSON.stringify(body));
  }
  const away = fakeFetch({ [PX_SESSION]: () => redirectedTo(jsonResponse({ user: { id: 'x' } }), 'https://accounts.example.com/login') });
  await assert.rejects(run(createRunner({ fetch: away, sender }), 'perplexity.send', { message: 'hi' }), (e) => e.code === 'not_logged_in');
  const cf = fakeFetch({ [PX_SESSION]: () => new Response('<title>Just a moment...</title>', { status: 403, headers: { 'content-type': 'text/html', 'cf-mitigated': 'challenge' } }) });
  await assert.rejects(run(createRunner({ fetch: cf, sender }), 'perplexity.send', { message: 'hi' }), (e) => e.code === 'blocked');
  assert.equal(sent.length, 1);
  assert.deepEqual(await run(createRunner({ fetch: ok, sender }), 'perplexity.close', { conversation_id: PX_SLUG }), [{ ok: true, result: { closed: 1 } }]);
});

test('perplexity ops need the www.perplexity.ai grant', async () => {
  const f = fakeFetch({ [PX_SESSION]: jsonResponse(PX_USER) });
  const sent = [];
  const sender = { send: async (site) => (sent.push(site), { conversation_id: PX_SLUG }), close: async () => ({ closed: 1 }) };
  const perms = fakePermissions(['https://chatgpt.com/*', 'https://*.oaiusercontent.com/*', 'https://claude.ai/*']);
  const r = createRunner({ fetch: f, sender, permissions: perms });
  for (const [op, args] of [['perplexity.send', { message: 'hi' }], ['perplexity.detail', { id: PX_SLUG }]]) {
    await assert.rejects(run(r, op, args), (e) => e.code === 'permission_missing' && /Perplexity/.test(e.message), op);
  }
  assert.equal(f.calls.length, 0);
  assert.deepEqual(sent, []);
  perms.granted.add('https://www.perplexity.ai/*');
  await run(r, 'perplexity.send', { message: 'hi' });
  assert.deepEqual(sent, ['perplexity']);
  assert.deepEqual(await grantedSites(perms), ['chatgpt', 'claudeai', 'perplexity']);
  assert.deepEqual(SITE_ACCESS.perplexity.origins, ['https://www.perplexity.ai/*']);
  assert.deepEqual(SITE_ACCESS.perplexity.pageOrigins, ['https://www.perplexity.ai/*']);
});

// ---- OpenAI dots: the owner's DM with the dot, read from the room feed.

const DOT_THREAD = '0d0d0d0d-1111-7222-8333-000000000001';
const DOT_ROOM = '0123456789abcdef0123456789abcdef';
const DOT_OWNER = 'user-owner__acct';
const DOT_BOT = 'user-dot__acct';
const DOT_TBO = `https://chatgpt.com/backend-api/tbo/by-thread/${DOT_THREAD}`;
const DOT_ROOM_URL = `https://chatgpt.com/backend-api/messaging/rooms/${DOT_ROOM}`;
const DOT_FEED = `${DOT_ROOM_URL}/messages?limit=32`;

function dotMessage(id, from, text, at, extra = {}) {
  return { id, created_at: at, updated_at: at, role: 'user', account_user_id: from, content: { text, attachments: [] }, reply_to: null, deleted_at: null, ...extra };
}

function dotRoutes(over = {}) {
  return {
    [SESSION]: jsonResponse({ accessToken: TOKEN }),
    [DOT_TBO]: jsonResponse({ id: 'tbo-1', messaging_room_id: DOT_ROOM, is_paused: false, status: 'active', last_check_in_at: '2026-09-29T17:00:00Z' }),
    [DOT_ROOM_URL]: jsonResponse({ id: DOT_ROOM, type: 'DM', creator_account_user_id: DOT_OWNER, members: [{ account_user_id: DOT_OWNER, role: 'user' }, { account_user_id: DOT_BOT, role: 'user' }] }),
    [DOT_FEED]: jsonResponse({
      items: [
        dotMessage('m1', DOT_OWNER, 'hello dot', '2026-09-29T17:01:00Z'),
        dotMessage('m2', DOT_BOT, 'hi, what do you need?', '2026-09-29T17:01:05Z', { content: { text: 'hi, what do you need?', attachments: [{ id: 'a1' }, { id: 'a2' }] } }),
        dotMessage('m3', DOT_OWNER, 'gone', '2026-09-29T17:01:06Z', { deleted_at: '2026-09-29T17:01:07Z' }),
      ],
      prev_cursor: null,
      next_cursor: null,
    }),
    ...over,
  };
}

test('dots.detail maps the dot record, room and feed, marking the owner from creator_account_user_id', async () => {
  const f = fakeFetch(dotRoutes());
  const frames = await run(createRunner({ fetch: f }), 'dots.detail', { id: DOT_THREAD });
  assert.deepEqual(frames, [
    {
      ok: true,
      result: {
        thread: DOT_THREAD,
        room: DOT_ROOM,
        owner: DOT_OWNER,
        paused: false,
        items: [
          { id: 'm1', at: '2026-09-29T17:01:00Z', from: DOT_OWNER, text: 'hello dot', attachments: 0 },
          { id: 'm2', at: '2026-09-29T17:01:05Z', from: DOT_BOT, text: 'hi, what do you need?', attachments: 2 },
        ],
      },
    },
  ]);
  assert.deepEqual(f.calls.map((c) => c.url), [SESSION, DOT_TBO, DOT_ROOM_URL, DOT_FEED]);
  for (const c of f.calls.slice(1)) assert.equal(c.init.headers.Authorization, 'Bearer ' + TOKEN);
  assert.ok(!JSON.stringify(frames).includes(TOKEN));
});

test('dots.detail: 401 or no token is not_logged_in, 429 is rate_limited with Retry-After, missing fields or non-JSON are endpoint_changed', async () => {
  await assert.rejects(run(createRunner({ fetch: fakeFetch(dotRoutes({ [SESSION]: jsonResponse({}) })) }), 'dots.detail', { id: DOT_THREAD }), (e) => e.code === 'not_logged_in');
  await assert.rejects(run(createRunner({ fetch: fakeFetch(dotRoutes({ [DOT_FEED]: jsonResponse({ detail: 'expired' }, 401) })) }), 'dots.detail', { id: DOT_THREAD }), (e) => e.code === 'not_logged_in');
  const limited = new Response('{}', { status: 429, headers: { 'content-type': 'application/json', 'retry-after': '42' } });
  let caught;
  await assert.rejects(run(createRunner({ fetch: fakeFetch(dotRoutes({ [DOT_TBO]: limited })) }), 'dots.detail', { id: DOT_THREAD }), (e) => ((caught = e), e.code === 'rate_limited' && e.retryAfter === 42));
  assert.equal(errorFrame(caught).error.retry_after, 42);
  const broken = [
    { [DOT_TBO]: jsonResponse({ id: 'tbo-1', is_paused: false }) },
    { [DOT_TBO]: jsonResponse({ id: 'tbo-1', messaging_room_id: DOT_ROOM }) },
    { [DOT_TBO]: jsonResponse({ id: 'tbo-1', messaging_room_id: '../../me', is_paused: false }) },
    { [DOT_ROOM_URL]: jsonResponse({ id: DOT_ROOM, members: [] }) },
    { [DOT_FEED]: jsonResponse({ messages: [] }) },
    { [DOT_FEED]: jsonResponse({ items: [{ id: 'm1', created_at: 'x', content: { text: 'no sender' } }] }) },
    { [DOT_FEED]: jsonResponse('<html>not json</html>', 200, 'text/html') },
  ];
  for (const over of broken) {
    await assert.rejects(run(createRunner({ fetch: fakeFetch(dotRoutes(over)) }), 'dots.detail', { id: DOT_THREAD }), (e) => e.code === 'endpoint_changed', JSON.stringify(Object.keys(over)));
  }
  // An unknown thread is not_found.
  await assert.rejects(run(createRunner({ fetch: fakeFetch(dotRoutes({ [DOT_TBO]: jsonResponse({}, 404) })) }), 'dots.detail', { id: DOT_THREAD }), (e) => e.code === 'not_found');
});

test('dots.detail caches the dot record and room per thread; not_found clears the cache', async () => {
  const routes = dotRoutes();
  const f = fakeFetch(routes);
  const r = createRunner({ fetch: f });
  const first = await run(r, 'dots.detail', { id: DOT_THREAD });
  assert.deepEqual(f.calls.map((c) => c.url), [SESSION, DOT_TBO, DOT_ROOM_URL, DOT_FEED]);
  f.calls.length = 0;
  const second = await run(r, 'dots.detail', { id: DOT_THREAD });
  assert.deepEqual(f.calls.map((c) => c.url), [SESSION, DOT_FEED], 'a second read of the same thread fetches only the feed');
  assert.deepEqual(second, first);
  // The room is gone: the feed is not_found and the cache is dropped.
  routes[DOT_FEED] = jsonResponse({}, 404);
  f.calls.length = 0;
  await assert.rejects(run(r, 'dots.detail', { id: DOT_THREAD }), (e) => e.code === 'not_found');
  assert.deepEqual(f.calls.map((c) => c.url), [SESSION, DOT_FEED]);
  routes[DOT_FEED] = dotRoutes()[DOT_FEED];
  f.calls.length = 0;
  assert.equal((await run(r, 'dots.detail', { id: DOT_THREAD }))[0].ok, true);
  assert.deepEqual(f.calls.map((c) => c.url), [SESSION, DOT_TBO, DOT_ROOM_URL, DOT_FEED], 'the next read fetches the dot record and room again');
});

test('dots.send refuses a paused dot, or a send with no thread, before any tab opens', async () => {
  const sent = [];
  const sender = { send: async (site, a) => (sent.push([site, a]), { conversation_id: DOT_THREAD, url: '', submitted_at: 1 }), close: async () => ({ closed: 0 }) };
  const paused = fakeFetch(dotRoutes({ [DOT_TBO]: jsonResponse({ id: 'tbo-1', messaging_room_id: DOT_ROOM, is_paused: true, status: 'paused' }) }));
  let caught;
  await assert.rejects(run(createRunner({ fetch: paused, sender }), 'dots.send', { message: 'hi', conversation_id: DOT_THREAD }), (e) => ((caught = e), e.code === 'paused'));
  assert.match(errorFrame(caught).error.message, /paused/);
  const f = fakeFetch(dotRoutes());
  const r = createRunner({ fetch: f, sender });
  await assert.rejects(run(r, 'dots.send', { message: 'hi' }), (e) => e.code === 'bad_request' && /thread/.test(e.message));
  await assert.rejects(run(r, 'dots.send', { message: 'hi', new_chat: true }), (e) => e.code === 'bad_request');
  await assert.rejects(run(createRunner({ fetch: fakeFetch(dotRoutes({ [SESSION]: jsonResponse({}) })), sender }), 'dots.send', { message: 'hi', conversation_id: DOT_THREAD }), (e) => e.code === 'not_logged_in');
  assert.deepEqual(sent, [], 'no send reached the sender');
  assert.equal(f.calls.length, 0, 'a send with no thread fetches nothing');
  // Without a sender the send is unsupported.
  await assert.rejects(run(createRunner({ fetch: fakeFetch(dotRoutes()) }), 'dots.send', { message: 'hi', conversation_id: DOT_THREAD }), (e) => e.code === 'unsupported');
});

test('dots ops need the ChatGPT grant; dots.close goes to the sender for its own site', async () => {
  const f = fakeFetch(dotRoutes());
  const closed = [];
  const sender = { send: async () => ({}), close: async (site, id) => (closed.push([site, id]), { closed: 1 }) };
  const perms = fakePermissions(['https://claude.ai/*']);
  const r = createRunner({ fetch: f, sender, permissions: perms });
  for (const [op, args] of [['dots.detail', { id: DOT_THREAD }], ['dots.send', { message: 'hi', conversation_id: DOT_THREAD }]]) {
    await assert.rejects(run(r, op, args), (e) => e.code === 'permission_missing' && /ChatGPT/.test(e.message), op);
  }
  assert.equal(f.calls.length, 0);
  assert.deepEqual(await run(r, 'dots.close', { conversation_id: DOT_THREAD }), [{ ok: true, result: { closed: 1 } }]);
  assert.deepEqual(closed, [['dots', DOT_THREAD]]);
  assert.deepEqual(await grantedSites(perms), ['claudeai']);
  perms.granted.add('https://chatgpt.com/*');
  assert.deepEqual(await grantedSites(perms), ['chatgpt', 'claudeai'], 'the hello names the site, not the alias');
  assert.equal((await run(r, 'dots.detail', { id: DOT_THREAD }))[0].ok, true);
  assert.deepEqual(validate({ id: 1, op: 'dots.send', args: { message: 'hi', conversation_id: DOT_THREAD } }).args, { message: 'hi', conversation_id: DOT_THREAD });
});


test('session operations are argument-free and return no credentials', async () => {
  for (const site of ['chatgpt', 'claudeai', 'grok', 'gemini', 'perplexity', 'copilot', 'dots']) {
    validate({ id: 1, op: site + '.session', args: {} });
    assert.throws(() => validate({ id: 1, op: site + '.session', args: { message: 'hello' } }));
  }
  for (const site of ['chatgpt', 'dots']) {
    const fetch = fakeFetch({ [SESSION]: jsonResponse({ accessToken: TOKEN }) });
    const frames = await run(createRunner({ fetch }), site + '.session', {});
    assert.deepEqual(frames, [{ ok: true, result: {} }]);
    assert.equal(JSON.stringify(frames).includes(TOKEN), false);
    await assert.rejects(run(createRunner({ fetch: fakeFetch({ [SESSION]: jsonResponse({}) }) }), site + '.session', {}), e => e.code === 'not_logged_in');
  }
});

for (const [site, url, signedIn, signedOut] of [
  ['claudeai', 'https://claude.ai/api/organizations', () => jsonResponse(fixture('claudeai/organizations.json')), () => jsonResponse([])],
  ['grok', 'https://grok.com/rest/app-chat/conversations?pageSize=1', () => jsonResponse({ conversations: [{ conversationId: 'account-conversation' }] }), () => jsonResponse({}, 401)],
  ['gemini', 'https://gemini.google.com/app', () => new Response(APP_HTML), () => new Response('<html>Sign in</html>')],
  ['perplexity', 'https://www.perplexity.ai/api/auth/session', () => jsonResponse({ user: { id: 'private-user-id' } }), () => jsonResponse({})],
]) {
  test(`${site}.session executes fresh signed-in and signed-out checks`, async () => {
    let authenticated = true;
    const fetch = fakeFetch({ [url]: () => authenticated ? signedIn() : signedOut() });
    const runner = createRunner({ fetch });
    assert.deepEqual(await run(runner, site + '.session', {}), [{ ok: true, result: {} }]);
    authenticated = false;
    await assert.rejects(run(runner, site + '.session', {}), e => e.code === 'not_logged_in');
    assert.equal(fetch.calls.length, 2, 'each probe fetches fresh evidence');
  });
}

test('grok.session treats an empty successful list as indeterminate', async () => {
  const fetch = fakeFetch({ 'https://grok.com/rest/app-chat/conversations?pageSize=1': jsonResponse({ conversations: [] }) });
  await assert.rejects(run(createRunner({ fetch }), 'grok.session', {}), e => e.code === 'indeterminate');
});
