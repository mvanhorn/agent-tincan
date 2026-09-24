import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { validate, createRunner, OPS, CHUNK_BYTES, MAX_FILE_BYTES } from '../ops.js';

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
  assert.deepEqual([...OPS].sort(), ['chatgpt.detail', 'chatgpt.file', 'chatgpt.list', 'claudeai.detail', 'claudeai.file', 'claudeai.list']);
  assert.deepEqual(validate({ id: 1, op: 'chatgpt.list', args: { count: 5 } }), { id: 1, op: 'chatgpt.list', args: { count: 5 } });
  validate({ id: 2, op: 'chatgpt.file', args: { file_id: 'file_00000000abcd1234', conversation_id: 'abc-1' } });
  validate({ id: 3, op: 'claudeai.detail', args: { id: 'c1a0d000-0000-4000-8000-000000000001' } });
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
  for (const f of ['../ops.js', '../background.js']) {
    const src = readFileSync(new URL(f, import.meta.url), 'utf8');
    for (const banned of [/\beval\s*\(/, /new\s+Function\s*\(/, /importScripts\s*\(/, /set(Timeout|Interval)\s*\(\s*['"`]/, /chrome\.scripting/, /chrome\.tabs/]) {
      assert.ok(!banned.test(src), `${f} matches ${banned}`);
    }
  }
});
