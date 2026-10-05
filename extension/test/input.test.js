import test from 'node:test';
import assert from 'node:assert/strict';
import { IMAGE_INPUT_SITES, helloMessage } from '../ops.js';

test('loaded hello advertises transport but no unproven input sites', async () => {
 assert.deepEqual(IMAGE_INPUT_SITES, []);
 const { hello } = await helloMessage({ manifest: {}, files: {} });
 assert.deepEqual(hello.image_input, { version: 1, sites: [] });
});

import { createInputTransfers, validate, createRunner } from '../ops.js';
import { createHash } from 'node:crypto';
const bytes = Buffer.from('image bytes');
const meta = { name: 'fixture.png', mime: 'image/png', size: bytes.length, sha256: createHash('sha256').update(bytes).digest('hex') };
const chunk = { token: '', seq: 0, data: bytes.toString('base64') };
test('transfer is ordered, bound to connection and site, verified and consumed once', async () => {
 const t = createInputTransfers();
 const { token } = t.begin('owner', 'chatgpt', [meta]);
 assert.throws(() => t.chunk('other', 'chatgpt', { ...chunk, token }));
 assert.throws(() => t.chunk('owner', 'grok', { ...chunk, token }));
 assert.throws(() => t.chunk('owner', 'chatgpt', { ...chunk, token, seq: 1 }));
 t.chunk('owner', 'chatgpt', { ...chunk, token });
 assert.throws(() => t.chunk('owner', 'chatgpt', { ...chunk, token }));
 const files = await t.consume('owner', 'chatgpt', token);
 assert.equal(files[0].data, bytes.toString('base64'));
 await assert.rejects(t.consume('owner', 'chatgpt', token));
 assert.equal(t.busy(), true);
 t.release('other', 'chatgpt', token);
 assert.equal(t.busy(), true, 'a different connection cannot release a transfer');
 t.release('owner', 'chatgpt', token);
 assert.equal(t.busy(), false);
});
test('transfer budgets, corrupt bytes, expiry and disconnect fail closed', async () => {
 let now = 0;
 const t = createInputTransfers({ now: () => now });
 assert.throws(() => t.begin('o', 'chatgpt', Array(5).fill(meta)));
 assert.throws(() => t.begin('o', 'chatgpt', [{ ...meta, size: 10 * 1024 * 1024 + 1 }]));
 const { token } = t.begin('o', 'chatgpt', [meta]);
 assert.throws(() => t.chunk('o', 'chatgpt', { ...chunk, token, data: '???' }));
 t.chunk('o', 'chatgpt', { ...chunk, token, data: Buffer.alloc(bytes.length).toString('base64') });
 await assert.rejects(t.consume('o', 'chatgpt', token));
 t.release('o', 'chatgpt', token);
 for (let i = 0; i < 4; i++) t.begin('o', 'chatgpt', [meta]);
 assert.throws(() => t.begin('o', 'chatgpt', [meta]));
 now = 120001;
 assert.equal(t.busy(), false);
 t.begin('o', 'chatgpt', [meta]);
 t.clear();
 assert.equal(t.busy(), false);
});
test('disabled input operations never fetch or open a composer', async () => {
 const r = createRunner({ fetch: () => assert.fail('fetch'), sender: { send: () => assert.fail('send') } });
 await assert.rejects(r.run('chatgpt.input_begin', {}, () => {}), /live acceptance/);
 assert.throws(() => validate({ id: 1, op: 'chatgpt.input_begin', args: {}, input: { files: [meta], connection: 'owner', url: 'https://evil.test' } }));
});

import { confirmedInputMessage, INPUT_CHUNK_BYTES } from '../ops.js';
import { pageInputImages } from '../send.js';

test('maximum chunks fit native frames and metadata is strict', () => {
 const req = { id: 1, op: 'chatgpt.input_chunk', args: {}, input: { connection: 'owner', token: 'a'.repeat(32), seq: 0, data: Buffer.alloc(INPUT_CHUNK_BYTES).toString('base64') } };
 assert.ok(Buffer.byteLength(JSON.stringify(validate(req))) < 256 * 1024);
 assert.throws(() => validate({ ...req, input: { ...req.input, data: req.input.data + 'AAAA' } }));
 for (const f of [{ ...meta, name: '../x.png' }, { ...meta, mime: 'image/svg+xml' }, { ...meta, sha256: 'bad' }]) {
  assert.throws(() => validate({ id: 1, op: 'chatgpt.input_begin', args: {}, input: { connection: 'owner', files: [f] } }));
 }
});
test('memory reservations include consumed sends, absolute expiry and cancellation', async () => {
 let now = 0;
 const t = createInputTransfers({ now: () => now });
 const large = { ...meta, size: 10 * 1024 * 1024 };
 t.begin('o', 'chatgpt', [large, large]);
 t.begin('o', 'chatgpt', [large, large]);
 assert.throws(() => t.begin('o', 'chatgpt', [meta]));
 t.clear();
 const { token } = t.begin('o', 'chatgpt', [meta]);
 t.chunk('o', 'chatgpt', { ...chunk, token });
 await t.consume('o', 'chatgpt', token);
 assert.equal(t.active(token), true);
 t.abort('o', 'chatgpt');
 assert.equal(t.active(token), false);
 assert.equal(t.busy(), true, 'cancelled active send retains its reservation until release');
 t.release('o', 'chatgpt', token);
 assert.equal(t.busy(), false);
 const idle = t.begin('o', 'chatgpt', [{ ...meta, size: 100 }]);
 for (let seq = 0; seq < 5; seq++) { now += 110000; t.chunk('o', 'chatgpt', { token: idle.token, seq, data: 'YQ==' }); }
 now = 600000;
 assert.throws(() => t.chunk('o', 'chatgpt', { token: idle.token, seq: 5, data: 'YQ==' }));
});
test('human image evidence is required, ordered, unique and on the current branch', () => {
 const msg = (id) => ({ id, author: { role: 'user' }, create_time: 10, content: { parts: ['look', { content_type: 'image_asset_pointer', asset_pointer: 'sediment://file-1' }] }, metadata: { attachments: [{ id: 'file-1', name: meta.name, mime_type: meta.mime }] } });
 const raw = { current_node: 'n1', mapping: { n1: { message: msg('m1') } } };
 assert.equal(confirmedInputMessage(raw, 'look', [meta], 10000), 'm1');
 assert.equal(confirmedInputMessage(raw, 'look', [meta], 20000), '');
 assert.equal(confirmedInputMessage(raw, 'look', [{ ...meta, name: 'other.png' }], 10000), '');
 raw.mapping.n1.message.content.parts.pop();
 assert.equal(confirmedInputMessage(raw, 'look', [meta], 10000), '');
 raw.mapping.n1.message = msg('m1');
 raw.mapping.n1.parent = 'n2'; raw.mapping.n2 = { message: msg('m2') };
 assert.equal(confirmedInputMessage(raw, 'look', [meta], 10000), '');
 delete raw.mapping.n1.parent;
 assert.equal(confirmedInputMessage(raw, 'look', [meta], 10000), 'm1');
});
test('candidate page refuses drafts and does not count FileList as ready', () => {
 const previous = globalThis.document;
 let previews = [];
 let pending = false;
 const input = { files: [] };
 const composer = { innerText: '', closest: () => form };
 const form = { querySelector: (s) => s === 'input[type="file"]' ? input : s === 'button[data-testid="send-button"]' ? { disabled: false } : s.includes('progressbar') && pending ? {} : null, querySelectorAll: () => previews };
 globalThis.document = { querySelector: () => composer };
 try {
  input.files = [{}];
  assert.equal(pageInputImages([meta], true).ok, false);
  assert.equal(pageInputImages([meta]).ready, false);
  input.files = []; composer.innerText = 'owner draft';
  assert.equal(pageInputImages([meta], true).ok, false);
  composer.innerText = '';
  previews = [{ complete: true, naturalWidth: 10, alt: meta.name }];
  assert.equal(pageInputImages([meta]).ready, true);
  assert.equal(pageInputImages([meta], true).ok, false);
  pending = true;
  assert.equal(pageInputImages([meta]).ready, false);
 } finally { globalThis.document = previous; }
});

import { createSender } from '../send.js';
function imageSenderHarness(failure = '') {
 let now = 0, clicked = false, probes = 0, reads = 0;
 let url = 'https://chatgpt.com/';
 const events = [];
 const sender = createSender({
  tabs: { create: async () => ({ id: 1 }), get: async () => ({ url, status: 'complete' }), remove: async () => events.push('close') },
  scripting: { executeScript: async ({ func, args, world, target }) => {
   assert.equal(world, 'ISOLATED'); assert.equal(target.tabId, 1);
   let result;
   switch (func.name) {
   case 'pageProbe':
    probes++;
    result = { href: url, signedIn: true, composer: true, composerEmpty: failure !== 'draft', userCount: clicked ? 1 : 0, loggedOut: failure === 'logout' && probes > 1 };
    break;
   case 'pageInputImages':
    if (args[1]) { events.push('attach'); result = { ok: true }; }
    else { reads++; events.push('ready-check'); result = { ok: failure !== 'rejected', ready: failure !== 'timeout' && !(failure === 'lost-ready' && reads > 1) }; }
    break;
   case 'pageFill': events.push('fill'); result = { ok: true }; break;
   case 'pageSubmit':
    events.push('click'); clicked = true; url = 'https://chatgpt.com/c/c1';
    if (failure === 'lost-click') throw new Error('response lost');
    result = { ok: true }; break;
   default: assert.fail(func.name);
   }
   return [{ result }];
  } },
  now: () => now, sleep: async (ms) => { now += ms; }, setTimer: () => 0, clearTimer: () => {}, timeoutMs: 30, pollMs: 1,
 });
 const hooks = { images: [meta, { ...meta, name: 'second.png' }], active: () => failure !== 'disconnect', confirmImages: async () => { events.push('evidence'); return failure === 'no-evidence' ? '' : 'u1'; } };
 return { sender, hooks, events };
}
test('queued image sender waits before typing, rechecks and submits exactly once', async () => {
 const h = imageSenderHarness();
 const result = await h.sender.send('chatgpt', { message: 'look' }, h.hooks);
 assert.deepEqual(h.events, ['attach', 'ready-check', 'fill', 'ready-check', 'click', 'evidence']);
 assert.equal(result.message_id, 'u1'); assert.equal(result.input_count, 2);
 await h.sender.close('chatgpt', 'c1');
 assert.equal(h.events.at(-1), 'close');
});
test('image send failures preserve drafts, stop text, close owned tabs and retain uncertainty', async () => {
 for (const failure of ['draft', 'rejected', 'timeout', 'logout', 'lost-ready', 'lost-click', 'no-evidence', 'disconnect']) {
  const h = imageSenderHarness(failure);
  await assert.rejects(h.sender.send('chatgpt', { message: 'look' }, h.hooks), (e) => {
   assert.equal(e.clicked === true, ['lost-click', 'no-evidence'].includes(failure), failure);
   return true;
  });
  if (!['lost-ready', 'lost-click', 'no-evidence'].includes(failure)) assert.ok(!h.events.includes('fill'), failure);
  assert.ok(h.events.filter((e) => e === 'click').length <= 1);
  if (failure !== 'disconnect') assert.equal(h.events.at(-1), 'close');
 }
});
