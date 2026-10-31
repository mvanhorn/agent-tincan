// Tincan phone page. It reads and acts only through the relay's own routes,
// as the CLI on an admin device does. Everything the relay returns (agent
// names, good-at lines, request text, attachment names) is untrusted text and
// reaches the page only through textContent, never as markup.
"use strict";

const POLL_MS = 5000;
// API_TIMEOUT_MS ends a request the relay has not answered, so one stalled
// fetch cannot hold the poll; a timeout counts as offline.
const API_TIMEOUT_MS = 15000;

const state = {
  held: new Map(), // request id -> {item, li, deciding, note}
  roster: [],
  relayVersion: "",
  updated: null, // Date of the last good poll
  offline: false,
  notAdmin: false,
  loaded: false,
  me: "", // this phone's own agent, once it has joined
  meChecked: false, // whoami has answered at least once
  meNote: "", // why this phone has no agent, when it is not just unjoined
  tab: "team",
  sent: new Map(), // request id -> {result, li, ...} for this phone's asks
  lastHeldCount: -1,
  heldFresh: false, // the held list was read since the page last came to the foreground
  rooms: [], // every room, from the admin listing
  room: "", // the open room
  roomPosts: [], // the open room's posts read so far, oldest first, at most MAX_ROOM_POSTS
  roomCursor: 0, // id of the last post read
  roomMentions: new Map(), // request id -> mention, for the posts in roomPosts
  roomRebuild: true, // the post list must be drawn again, not just added to
  roomGen: 0, // bumped by resetRoom, so a read for an earlier room is dropped
  roomRecheckAt: 0, // where the next recheckMentions starts among open mentions
  roomDrafts: new Map(), // room name -> unsent text, for rooms not open now
};

// sending is true while an ask is on its way, so a render from a poll in
// the meantime keeps Send off.
let sending = false;

const NAME_RE = /^[a-z0-9][a-z0-9-]{0,31}$/;
const DONE = new Set(["answered", "failed", "declined", "cancelled", "expired"]);
const SENT_SHOWN = 20;
const MAX_ROOM_POSTS = 300;
// MAX_ROOM_IDS is how many posts one status re-read names (the relay's cap).
const MAX_ROOM_IDS = 50;

// fillSelect rebuilds sel's options from names, labelled by label, when the
// names changed since the last fill, keeping the selection when its name is
// still there. It reports whether it rebuilt them.
function fillSelect(sel, names, label = (name) => name) {
  const key = names.join(",");
  if (sel.dataset.names === key) return false;
  const current = sel.value;
  sel.replaceChildren();
  names.forEach((name, i) => {
    const opt = el("option", "", label(name, i));
    opt.value = name;
    sel.append(opt);
  });
  sel.dataset.names = key;
  if (names.includes(current)) sel.value = current;
  return true;
}

// The phone's agent name is remembered on the device so the page can name
// itself on a machine that runs other agents too. Storage may be off.
function remembered() {
  try { return localStorage.getItem("tincan-phone-agent") || ""; } catch (_) { return ""; }
}
function remember(name) {
  try {
    if (name) localStorage.setItem("tincan-phone-agent", name);
    else localStorage.removeItem("tincan-phone-agent");
  } catch (_) {
    // The page still works; it asks whoami again next time.
  }
}

const $ = (id) => document.getElementById(id);

// el builds an element whose text, if any, is set as text.
function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined && text !== null) e.textContent = String(text);
  return e;
}

function announce(msg) {
  const live = $("live");
  live.textContent = "";
  // A fresh text node after a tick makes screen readers repeat a message.
  setTimeout(() => { live.textContent = msg; }, 50);
}

function hhmm(d) {
  return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

// age matches the CLI's QueueAge: minutes, then hours, then days.
function age(since) {
  const ms = Math.max(0, Date.now() - new Date(since).getTime());
  const h = ms / 3600000;
  if (h >= 48) return Math.floor(h / 24) + "d";
  if (h >= 1) return Math.floor(h) + "h";
  return Math.floor(ms / 60000) + "m";
}

function plural(n, word) {
  return n + " " + word + (n === 1 ? "" : "s");
}

// api calls the relay. A body is sent as JSON, which the relay requires of
// every state-changing browser request. It throws, as when offline, if the
// relay has not answered within API_TIMEOUT_MS; the error has timeout set.
async function api(method, path, body, agent) {
  const ctl = new AbortController();
  const stall = setTimeout(() => ctl.abort(), API_TIMEOUT_MS);
  const init = { method, credentials: "same-origin", cache: "no-store", headers: {}, signal: ctl.signal };
  const name = agent === undefined ? state.me : agent;
  if (name) init.headers["X-Tincan-Agent"] = name;
  if (body !== undefined) {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(body);
  }
  const timedOut = () => Object.assign(new Error("the relay did not answer in time"), { timeout: true });
  try {
    let resp;
    try {
      resp = await fetch(path, init);
    } catch (e) {
      throw ctl.signal.aborted ? timedOut() : e;
    }
    let data = null;
    try {
      data = await resp.json();
    } catch (_) {
      // A body cut off by the timeout is not an empty answer.
      if (ctl.signal.aborted) throw timedOut();
      data = null;
    }
    return { status: resp.status, data };
  } finally {
    clearTimeout(stall);
  }
}

// ---- Attention order, the same score as tincan top ----

const releaseRE = /^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$/;
const devRE = /(^|[.-])(dev|dirty)([.-]|$)|-[0-9]+-g[0-9a-f]+/;
const numeric = (s) => /^[0-9]+$/.test(s);
function cmpNum(a, b) {
  if (a.length !== b.length) return a.length < b.length ? -1 : 1;
  return a < b ? -1 : a > b ? 1 : 0;
}

// ahead reports whether release a sorts after release b (client.Ahead).
function ahead(a, b) {
  const av = releaseRE.exec(a || ""), bv = releaseRE.exec(b || "");
  if (!av || !bv || devRE.test(a) || devRE.test(b)) return false;
  for (const v of [av, bv]) {
    for (const id of (v[4] || "").split(".")) {
      if (numeric(id) && id.length > 1 && id[0] === "0") return false;
    }
  }
  for (let i = 1; i <= 3; i++) {
    const c = cmpNum(av[i], bv[i]);
    if (c !== 0) return c > 0;
  }
  const ap = av[4] || "", bp = bv[4] || "";
  if (ap === bp) return false;
  if (ap === "") return true;
  if (bp === "") return false;
  const as = ap.split("."), bs = bp.split(".");
  for (let i = 0; i < Math.min(as.length, bs.length); i++) {
    const x = as[i], y = bs[i];
    if (x === y) continue;
    const xn = numeric(x), yn = numeric(y);
    if (xn && yn) return cmpNum(x, y) > 0;
    if (xn !== yn) return !xn;
    return x > y;
  }
  return as.length > bs.length;
}

function attention(a) {
  let score = 0;
  if (a.status === "stuck") score += 8;
  if (a.signed_out_site) score += 6;
  if (ahead(state.relayVersion, a.version)) score += 1;
  return score;
}

function statusText(a) {
  const word = a.status || (a.online ? "online" : "offline");
  let detail = a.status_reason || "";
  if (a.status === "busy" && a.busy_for) {
    detail = a.busy_for + "'s request" + (a.busy_since ? ", " + age(a.busy_since) : "");
  }
  return detail ? word + " (" + detail + ")" : word;
}

// ---- Rendering ----

function renderConn() {
  const conn = $("conn");
  if (state.notAdmin) {
    conn.textContent = "Not an admin device";
  } else if (state.offline) {
    conn.textContent = state.updated ? "Offline, last updated " + hhmm(state.updated) : "Offline";
  } else if (state.updated) {
    conn.textContent = "Updated " + hhmm(state.updated);
  } else {
    conn.textContent = "Loading";
  }
  const banner = $("banner");
  if (state.notAdmin) {
    banner.textContent = "This device is not an admin. Only the machines the relay lists in --admin can see held asks or approve them.";
    banner.hidden = false;
  } else {
    banner.hidden = true;
  }
}

function nonceLive(item) {
  return item.nonce_expires_at && Date.now() < new Date(item.nonce_expires_at).getTime() - 5000;
}

function buttonsEnabled(entry) {
  return state.heldFresh && !state.offline && !state.notAdmin && !entry.deciding && !entry.note && nonceLive(entry.item);
}

function heldCard(entry) {
  const item = entry.item;
  const li = el("li", "card");
  li.dataset.id = item.id;
  const target = item.to_kind ? item.to + " (" + item.to_kind + ")" : item.to;
  li.append(el("p", "route", item.from + " asks " + target));
  const when = item.created_at ? "held " + age(item.created_at) + " ago" : "held";
  li.append(el("p", "meta", (item.kind || "ask") + ", " + when + ", request " + item.id));
  if (item.chain && item.chain.length > 1) {
    li.append(el("p", "meta", "chain: " + item.chain.join(" > ")));
  }
  li.append(el("p", "body", item.body));
  if (item.then) {
    li.append(el("p", "meta", "Then, once answered, asks " + item.then.to + ":"));
    li.append(el("p", "body", item.then.body));
  }
  const files = item.attachments || [];
  if (files.length) {
    li.append(el("p", "meta", plural(files.length, "attachment") + ":"));
    const ul = el("ul", "files");
    for (const f of files) {
      ul.append(el("li", "", (f.name || f.id) + (f.size ? " (" + Math.ceil(f.size / 1024) + " KB)" : "")));
    }
    li.append(ul);
  }
  const note = el("p", "meta");
  note.hidden = true;
  li.append(note);
  const actions = el("div", "actions");
  const deny = el("button", "deny", "Deny");
  deny.type = "button";
  deny.setAttribute("aria-label", "Deny " + item.from + "'s ask to " + item.to);
  deny.addEventListener("click", () => decide(item.id, "deny"));
  const approve = el("button", "approve", "Approve");
  approve.type = "button";
  approve.setAttribute("aria-label", "Approve " + item.from + "'s ask to " + item.to);
  approve.addEventListener("click", () => decide(item.id, "approve"));
  actions.append(deny, approve);
  li.append(actions);
  entry.li = li;
  entry.noteEl = note;
  entry.buttons = [deny, approve];
  return li;
}

function updateCard(entry) {
  const on = buttonsEnabled(entry);
  for (const b of entry.buttons) b.disabled = !on;
  if (entry.note) {
    entry.noteEl.textContent = entry.note;
    entry.noteEl.hidden = false;
  } else if ((!state.heldFresh || !nonceLive(entry.item)) && !state.offline) {
    entry.noteEl.textContent = "Refreshing this ask before it can be decided.";
    entry.noteEl.hidden = false;
  } else {
    entry.noteEl.hidden = true;
  }
}

function renderHeld() {
  const list = $("held-list");
  const live = [...state.held.values()];
  for (const entry of live) {
    if (!entry.li) list.append(heldCard(entry));
    updateCard(entry);
  }
  const pending = live.filter((e) => !e.note).length;
  $("held-count").textContent = String(pending);
  $("held-empty").hidden = live.length > 0 || !state.loaded || state.notAdmin;
}

function renderTeam() {
  const list = $("team-list");
  const agents = [...state.roster].sort((a, b) => {
    const d = attention(b) - attention(a);
    return d !== 0 ? d : a.name < b.name ? -1 : a.name > b.name ? 1 : 0;
  });
  list.replaceChildren();
  for (const a of agents) {
    const li = el("li", "agent");
    const head = el("div", "agent-head");
    head.append(el("span", "agent-name", a.name));
    head.append(el("span", "status " + (a.status || (a.online ? "idle" : "offline")), statusText(a)));
    li.append(head);
    const facts = [];
    if (a.kind) facts.push(a.kind);
    if (a.queued) facts.push(a.queued + " queued");
    if (a.signed_out_site) facts.push("signed out of " + a.signed_out_site);
    if (ahead(state.relayVersion, a.version)) facts.push("old build " + a.version);
    if (facts.length) li.append(el("p", "meta", facts.join(", ")));
    if (a.good_at) li.append(el("p", "meta", "good at: " + a.good_at));
    if (a.working_on) li.append(el("p", "meta", "working on: " + a.working_on));
    list.append(li);
  }
  $("team-empty").hidden = agents.length > 0 || !state.loaded || state.notAdmin;
}

// ---- Ask: join this phone, ask a teammate, follow the answers ----

function renderTabs() {
  for (const name of ["team", "ask", "rooms"]) {
    const on = state.tab === name;
    $("tab-" + name).setAttribute("aria-selected", on ? "true" : "false");
    $(name).hidden = !on;
  }
}

function renderAsk() {
  const joined = !!state.me;
  $("join").hidden = joined || !state.meChecked || state.notAdmin;
  $("ask-form").hidden = !joined;
  if (state.meNote) $("join-text").textContent = state.meNote;
  if (!joined) return;
  $("ask-me").textContent = "Asking as " + state.me + ".";
  const select = $("ask-to");
  const targets = state.roster.filter((a) => a.name !== state.me && a.kind !== "phone").sort((a, b) => (a.name < b.name ? -1 : 1));
  const label = (name, i) => name + " (" + (targets[i].status || (targets[i].online ? "online" : "offline")) + ")";
  fillSelect(select, targets.map((a) => a.name), label);
  const disabled = state.offline || state.notAdmin;
  $("ask-send").disabled = sending || disabled || targets.length === 0;
  // A sent-list refresh in flight renders the list when it lands.
  if (!sentPolling) renderSent();
}

const sentTime = (e) => new Date(e.result.request.created_at || 0).getTime();
const newestFirst = (a, b) => sentTime(b) - sentTime(a);

function sentLabel(res) {
  switch (res.status) {
    case "held": return "held for your approval";
    case "queued":
    case "delivered": return "waiting for " + res.request.to;
    case "claimed": return res.request.to + " is working on it" + (res.progress && res.progress.note ? ": " + res.progress.note : "");
    case "needs_input": return res.request.to + " needs your input";
    case "answered": return "answered by " + res.request.to;
    case "failed": return res.request.to + " could not do it";
    case "declined": return "declined";
    default: return res.status;
  }
}

function sentCard(entry) {
  const li = el("li", "card");
  entry.route = el("p", "route");
  entry.statusEl = el("p", "meta");
  entry.askEl = el("p", "body");
  entry.replyEl = el("p", "body");
  entry.answerBox = el("div", "answer");
  li.append(entry.route, entry.statusEl, entry.askEl, entry.replyEl, entry.answerBox);
  entry.li = li;
  return li;
}

function answerBox(entry, question) {
  const box = entry.answerBox;
  if (box.dataset.question === question) return;
  box.replaceChildren();
  box.dataset.question = question;
  const id = "answer-" + entry.result.request.id;
  box.append(el("p", "meta", "Question: " + question));
  const label = el("label", "", "Your answer");
  label.htmlFor = id;
  const input = el("textarea", "field");
  input.id = id;
  input.rows = 2;
  const send = el("button", "primary", "Send answer");
  send.type = "button";
  send.addEventListener("click", async () => {
    const body = input.value.trim();
    if (!body) return;
    send.disabled = true;
    const res = await api("POST", "/v1/requests/" + encodeURIComponent(entry.result.request.id) + "/answer", { body }).catch(() => null);
    send.disabled = false;
    if (res && res.status === 200) {
      box.replaceChildren();
      delete box.dataset.question;
      announce("Answer sent to " + entry.result.request.to + ".");
      refreshSent();
    } else {
      announce("Could not send the answer: " + ((res && res.data && res.data.error) || "offline"));
    }
  });
  box.append(label, input, send);
}

function renderSent() {
  const list = $("sent-list");
  const entries = [...state.sent.values()].sort(newestFirst).slice(0, SENT_SHOWN);
  const shown = new Set();
  entries.forEach((entry, i) => {
    const res = entry.result;
    if (!entry.li) sentCard(entry);
    entry.route.textContent = "To " + res.request.to;
    entry.statusEl.textContent = sentLabel(res) + (res.request.created_at ? ", asked " + age(res.request.created_at) + " ago" : "");
    entry.askEl.textContent = res.request.body;
    // A question arrives as the reply body too; the answer box shows it.
    const reply = res.status !== "needs_input" && res.reply && res.reply.body;
    entry.replyEl.hidden = !reply;
    entry.replyEl.textContent = reply || "";
    const open = (res.exchanges || []).filter((x) => !x.answer);
    if (res.status === "needs_input" && open.length) {
      answerBox(entry, open[open.length - 1].question);
    } else if (entry.answerBox.dataset.question) {
      entry.answerBox.replaceChildren();
      delete entry.answerBox.dataset.question;
    }
    // Cards are updated in place and moved only when out of place, so a
    // focused answer box stays attached and the keyboard stays open.
    shown.add(entry.li);
    const at = list.children[i];
    if (at !== entry.li) list.insertBefore(entry.li, at || null);
  });
  for (const li of [...list.children]) {
    if (!shown.has(li)) li.remove();
  }
  $("sent-empty").hidden = entries.length > 0;
}

// whoami finds this phone's agent: the name it joined as, else the one
// agent on this device if that is a phone. Only a 200 or a 403 settles it;
// an error or any other answer leaves it unchecked, so the next poll asks
// again. The remembered name is kept when this device has no phone agent,
// so the page can rejoin under it after the phone's node is replaced.
async function whoami() {
  const saved = remembered();
  let res;
  try {
    res = await api("GET", "/v1/whoami", undefined, saved);
    if (res.status === 403 && saved) res = await api("GET", "/v1/whoami", undefined, "");
  } catch (_) {
    return;
  }
  if (res.status !== 200 && res.status !== 403) return;
  state.meChecked = true;
  state.meNote = "";
  if (res.status === 200 && res.data && res.data.kind === "phone") {
    state.me = res.data.name;
    remember(state.me);
  } else {
    state.me = "";
    if (res.status === 200 && res.data) {
      state.meNote = "This device already runs " + res.data.name + ". Join a phone agent beside it to ask from here.";
    }
  }
}

async function join() {
  const name = $("join-name").value.trim();
  const err = $("join-error");
  err.hidden = true;
  if (!NAME_RE.test(name)) {
    err.textContent = "Use 1 to 32 lowercase letters, digits or dashes.";
    err.hidden = false;
    return;
  }
  // An invite for a name that exists moves that agent to this device, so
  // only a free name or this device's own earlier phone agent may be used.
  const taken = state.roster.find((a) => a.name === name);
  if (taken && taken.kind !== "phone") {
    err.textContent = name + " is another agent on the team. Pick a different name.";
    err.hidden = false;
    return;
  }
  if (taken && name !== state.me && name !== remembered()) {
    err.textContent = name + " is another phone's agent. Pick a different name, or remove it first with tincan remove " + name + ".";
    err.hidden = false;
    return;
  }
  $("join-button").disabled = true;
  try {
    // new_only has the relay refuse a name another device's agent holds,
    // which the roster above may not show yet. The remembered name may
    // move this phone's own agent back after its node was replaced.
    const inv = await api("POST", "/v1/admin/invite", { name, kind: "phone", new_only: name !== remembered() });
    if (inv.status === 409) throw new Error(name + " is already an agent on another device. Pick a different name.");
    if (inv.status !== 200 || !inv.data || !inv.data.code) throw new Error((inv.data && inv.data.error) || "invite failed");
    const joined = await api("POST", "/v1/join", { code: inv.data.code });
    if (joined.status !== 200 || !joined.data) throw new Error((joined.data && joined.data.error) || "join failed");
    remember(joined.data.name);
    await whoami();
    announce("This phone joined as " + joined.data.name + ".");
    refresh();
  } catch (e) {
    err.textContent = "Could not join: " + e.message;
    err.hidden = false;
  } finally {
    $("join-button").disabled = false;
  }
}

async function sendAsk() {
  const to = $("ask-to").value;
  const body = $("ask-body").value.trim();
  const err = $("ask-error");
  err.hidden = true;
  if (!to || !body || sending) return;
  sending = true;
  $("ask-send").disabled = true;
  try {
    const res = await api("POST", "/v1/send", { to, body, kind: "ask" });
    if (res.status === 403) {
      await whoami();
      throw new Error((res.data && res.data.error) || "this phone is not joined");
    }
    if (res.status !== 201 || !res.data) throw new Error((res.data && res.data.error) || "error " + res.status);
    const status = res.data.status === "held" ? "held" : "queued";
    state.sent.set(res.data.id, { result: { request: res.data, status } });
    $("ask-body").value = "";
    announce(status === "held" ? "Sent to " + to + ", held for your approval." : "Sent to " + to + ".");
  } catch (e) {
    err.textContent = e.timeout
      ? "No answer from the relay in time. It may have been sent; check Your asks before sending again."
      : "Not sent: " + e.message;
    err.hidden = false;
  } finally {
    sending = false;
    render();
  }
}

// refreshSent rebuilds this phone's asks from the relay: its recent chains
// name them, and each open one is read again for its status and reply. It
// renders the sent list when done.
let sentPolling = false;
async function refreshSent() {
  if (!state.me || sentPolling) return;
  sentPolling = true;
  try {
    // from filters before the limit, so newer team traffic cannot hide an
    // open ask of this phone's.
    const recent = await api("GET", "/v1/trace?limit=200&exclude_pings=true&from=" + encodeURIComponent(state.me));
    if (recent.status === 200 && recent.data) {
      for (const r of recent.data.traces || []) {
        if (r.request.from !== state.me) continue;
        const entry = state.sent.get(r.request.id);
        if (!entry) state.sent.set(r.request.id, { result: r, fetched: false });
        else if (!entry.fetched) entry.result = r;
      }
    }
    const newest = [...state.sent.values()].sort(newestFirst).slice(0, SENT_SHOWN);
    await Promise.all(newest.filter((e) => !e.fetched || !DONE.has(e.result.status)).map(async (e) => {
      const res = await api("GET", "/v1/requests/" + encodeURIComponent(e.result.request.id));
      if (res.status === 200 && res.data) {
        e.result = res.data;
        e.fetched = true;
      }
    }));
  } catch (_) {
    // The main poll reports the relay as offline.
  } finally {
    sentPolling = false;
    renderSent();
  }
}

// sentNeeded reports whether a poll should refresh the sent list: the Ask
// tab is showing, or one of this phone's asks is still open.
function sentNeeded() {
  if (state.tab === "ask") return true;
  for (const e of state.sent.values()) {
    if (!DONE.has(e.result.status)) return true;
  }
  return false;
}

// ---- Rooms: the owner picks members; the phone reads and posts as its agent ----

// Members of these kinds send what they read to an outside vendor, so the
// page asks before adding one. The relay also warns for any agent connected
// through its gateway, whatever its kind.
const VENDOR_KINDS = new Set(["chatgpt-web", "claude-web", "grok-web", "gemini-web", "perplexity-web", "copilot-web", "chatgpt", "sesame"]);
const roomPath = (room) => "/v1/rooms/" + encodeURIComponent(room);
const memberPath = (room) => "/v1/admin/rooms/" + encodeURIComponent(room) + "/members";

function roomError(msg) {
  const err = $("room-error");
  err.textContent = msg || "";
  err.hidden = !msg;
}

// questionTag names the request a question back on a mention is answered
// on, so the poster has the id without digging in the inbox.
const questionTag = (id) => (id ? "question, answer with request " + id : "question");

function postTag(p) {
  switch (p.status) {
    case "needs_input": return ", " + questionTag(p.reply_to);
    case "failed":
    case "declined": return ", " + p.status;
    default: return "";
  }
}

function postCard(p) {
  const li = el("li", "card");
  li.append(el("p", "route", "#" + p.id + " " + p.author));
  li.append(el("p", "meta", hhmm(new Date(p.created_at)) + postTag(p)));
  li.append(el("p", "body", p.body));
  for (const m of p.mentions || []) {
    if (["answered", "failed", "declined"].includes(m.status)) continue;
    const gone = m.status === "expired" || m.status === "cancelled";
    let line = "waiting on " + m.agent;
    if (gone) line = m.agent + " did not answer (" + m.status + ")";
    else if (m.status === "needs_input") line += " (" + questionTag(m.request_id) + ")";
    li.append(el("p", "meta", line));
  }
  return li;
}

// openRoom is the open room's entry in the admin listing, if any.
const openRoom = () => state.rooms.find((r) => r.name === state.room);

function renderRooms() {
  if (state.tab !== "rooms") return;
  const pick = $("room-pick");
  fillSelect(pick, state.rooms.map((r) => r.name));
  if (state.room) pick.value = state.room;
  $("rooms-empty").hidden = state.rooms.length > 0 || !state.loaded;
  $("rooms-me").textContent = state.me ? "Reading and posting as " + state.me + "." : "Join this phone on the Ask tab to read and post in rooms.";
  $("room-view").hidden = !state.room;
  if (!state.room) return;
  const room = openRoom();
  const roomMembers = (room && room.members) || [];
  const roomMember = !!state.me && roomMembers.includes(state.me);
  $("room-title").textContent = state.room;
  const members = $("room-members");
  members.replaceChildren();
  for (const name of roomMembers) {
    const li = el("li", "", name + " ");
    const remove = el("button", "", "Remove");
    remove.type = "button";
    remove.setAttribute("aria-label", "Remove " + name + " from " + state.room);
    remove.addEventListener("click", () => removeMember(name));
    li.append(remove);
    members.append(li);
  }
  const candidates = state.roster.filter((a) => !roomMembers.includes(a.name)).map((a) => a.name).sort();
  fillSelect($("room-add-agent"), candidates);
  $("room-add").disabled = candidates.length === 0 || state.offline;
  const join = $("room-join");
  join.hidden = !state.me || roomMember;
  join.textContent = state.me ? "This phone (" + state.me + ") is not in this room. Add it to read and post." : "";
  $("room-form").hidden = !roomMember;
  // New posts are added to the list; it is drawn again only for another
  // room, a settled mention, or posts let go past MAX_ROOM_POSTS.
  const list = $("room-posts");
  if (state.roomRebuild) {
    list.replaceChildren();
    list.dataset.shown = "0";
    state.roomRebuild = false;
  }
  const shown = Number(list.dataset.shown || 0);
  for (const p of state.roomPosts) {
    if (p.id > shown) list.append(postCard(p));
  }
  if (state.roomPosts.length) list.dataset.shown = String(state.roomPosts[state.roomPosts.length - 1].id);
}

// refreshRooms reads every room (an admin listing) and, for the open room,
// the posts after the cursor as this phone's agent, then where earlier
// posts' open mentions stand. A call made while one runs is not dropped:
// the running one goes again once it is done.
let roomsPolling = false;
let roomsAgain = false;
async function refreshRooms() {
  if (roomsPolling) {
    roomsAgain = true;
    return;
  }
  roomsPolling = true;
  try {
    do {
      roomsAgain = false;
      await refreshRoomsOnce();
    } while (roomsAgain);
  } finally {
    roomsPolling = false;
    renderRooms();
  }
}

async function refreshRoomsOnce() {
  try {
    const gen = state.roomGen;
    const all = await api("GET", "/v1/admin/rooms");
    if (all.status === 200 && all.data) state.rooms = all.data.rooms || [];
    // Only a room that is gone, or none picked yet, falls back to the
    // first; a room picked or made while the listing was on its way may not
    // be in it yet, and the call that picked it reads again.
    if (gen === state.roomGen && (!state.room || (all.status === 200 && !openRoom()))) {
      switchRoom(state.rooms.length ? state.rooms[0].name : "");
    }
    const room = openRoom();
    if (room && state.me && (room.members || []).includes(state.me)) {
      await readRoom();
      await recheckMentions();
    }
  } catch (_) {
    // The main poll reports the relay as offline.
  }
}

// switchRoom opens room name. The box's unsent text stays with the room
// being left and the next room shows its own draft, or nothing, so a
// draft is never posted to a room it was not written for.
function switchRoom(name) {
  const box = $("room-body");
  if (state.room && box.value) state.roomDrafts.set(state.room, box.value);
  else if (state.room) state.roomDrafts.delete(state.room);
  state.room = name;
  box.value = state.roomDrafts.get(name) || "";
  state.roomDrafts.delete(name);
  resetRoom();
}

function resetRoom() {
  state.roomPosts = [];
  state.roomCursor = 0;
  state.roomMentions.clear();
  state.roomRecheckAt = 0;
  state.roomRebuild = true;
  state.roomGen++;
}

// readRoom reads the open room's posts after the cursor. A room switched
// meanwhile (resetRoom) drops the answer, which is the old room's.
async function readRoom() {
  const room = state.room;
  const gen = state.roomGen;
  const res = await api("GET", roomPath(room) + "/posts?after=" + state.roomCursor);
  if (res.status !== 200 || !res.data || room !== state.room || gen !== state.roomGen) return;
  for (const p of res.data.posts || []) {
    // A mirrored reply settles the mention it answers in the earlier post.
    const m = p.reply_to && p.status !== "needs_input" ? state.roomMentions.get(p.reply_to) : null;
    if (m && m.status !== p.status) {
      m.status = p.status;
      state.roomRebuild = true;
    }
    for (const q of p.mentions || []) state.roomMentions.set(q.request_id, q);
    state.roomPosts.push(p);
  }
  if (state.roomPosts.length > MAX_ROOM_POSTS) {
    for (const p of state.roomPosts.splice(0, state.roomPosts.length - MAX_ROOM_POSTS)) {
      for (const q of p.mentions || []) state.roomMentions.delete(q.request_id);
    }
    state.roomRebuild = true;
  }
  state.roomCursor = res.data.next || state.roomCursor;
  const open = openRoom();
  if (open && res.data.members) open.members = res.data.members;
}

// recheckMentions re-reads, by id, loaded posts that still have an open
// mention, so one cancelled, denied or expired, which leaves no reply in
// the room, stops showing as waiting. With more such posts than one read
// takes, each read takes the next MAX_ROOM_IDS of them, newest first, and
// wraps around, so every one is checked in turn.
async function recheckMentions() {
  const open = [];
  for (let i = state.roomPosts.length - 1; i >= 0; i--) {
    const p = state.roomPosts[i];
    if ((p.mentions || []).some((m) => !DONE.has(m.status))) open.push(p.id);
  }
  if (!open.length) return;
  let ids = open;
  if (open.length > MAX_ROOM_IDS) {
    const from = state.roomRecheckAt % open.length;
    ids = open.slice(from, from + MAX_ROOM_IDS);
    if (ids.length < MAX_ROOM_IDS) ids = ids.concat(open.slice(0, MAX_ROOM_IDS - ids.length));
    state.roomRecheckAt = from + MAX_ROOM_IDS;
  }
  const room = state.room;
  const gen = state.roomGen;
  const res = await api("GET", roomPath(room) + "/posts?ids=" + ids.join(","));
  if (res.status !== 200 || !res.data || room !== state.room || gen !== state.roomGen) return;
  for (const p of res.data.posts || []) {
    for (const q of p.mentions || []) {
      const m = state.roomMentions.get(q.request_id);
      if (m && m.status !== q.status) {
        m.status = q.status;
        state.roomRebuild = true;
      }
    }
  }
}

async function createRoom() {
  const name = $("room-new").value.trim();
  roomError("");
  if (!NAME_RE.test(name)) {
    roomError("Use 1 to 32 lowercase letters, digits or dashes.");
    return;
  }
  $("room-create").disabled = true;
  try {
    const res = await api("POST", "/v1/admin/rooms", { name });
    if (res.status !== 201) throw new Error((res.data && res.data.error) || "error " + res.status);
    // The phone takes part in the room it made, as its own agent.
    if (state.me) await api("POST", memberPath(name), { agent: state.me });
    $("room-new").value = "";
    switchRoom(name);
    announce("Created room " + name + ".");
  } catch (e) {
    roomError("Could not create the room: " + e.message);
  } finally {
    $("room-create").disabled = false;
    refreshRooms();
  }
}

async function addMember() {
  const agent = $("room-add-agent").value;
  if (!agent || !state.room) return;
  roomError("");
  const a = state.roster.find((x) => x.name === agent);
  if (a && VENDOR_KINDS.has(a.kind) &&
      !window.confirm(agent + " is a " + a.kind + " agent: the posts it reads or is sent go to that vendor, under your account there. Add it to " + state.room + "?")) {
    return;
  }
  const res = await api("POST", memberPath(state.room), { agent }).catch(() => null);
  if (!res || res.status !== 200) {
    roomError("Not added: " + ((res && res.data && res.data.error) || "offline"));
    return;
  }
  if (res.data && res.data.warning && !(a && VENDOR_KINDS.has(a.kind))) roomError("Added. Note: " + res.data.warning + ".");
  announce("Added " + agent + " to " + state.room + ".");
  refreshRooms();
}

async function removeMember(agent) {
  if (!state.room || !window.confirm("Remove " + agent + " from " + state.room + "?")) return;
  roomError("");
  const res = await api("DELETE", memberPath(state.room), { agent }).catch(() => null);
  if (!res || res.status !== 200) {
    roomError("Not removed: " + ((res && res.data && res.data.error) || "offline"));
    return;
  }
  announce("Removed " + agent + " from " + state.room + ".");
  refreshRooms();
}

async function postToRoom() {
  const draft = $("room-body").value;
  const body = draft.trim();
  const room = state.room;
  const gen = state.roomGen;
  if (!body || !room) return;
  roomError("");
  $("room-send").disabled = true;
  const notices = $("room-notices");
  notices.replaceChildren();
  // The answer may come after the owner opened another room or typed a
  // new draft: only the draft that was sent is cleared, and the notices
  // are shown only while the room they are about is still open.
  const stillOpen = () => room === state.room && gen === state.roomGen;
  try {
    const res = await api("POST", roomPath(room) + "/posts", { body });
    if (res.status !== 201 || !res.data) throw new Error((res.data && res.data.error) || "error " + res.status);
    // The sent draft is cleared wherever it now waits: in the box while
    // its room is open, or kept for that room after a switch.
    if (room === state.room && $("room-body").value === draft) $("room-body").value = "";
    else if (state.roomDrafts.get(room) === draft) state.roomDrafts.delete(room);
    if (stillOpen()) {
      for (const m of res.data.post.mentions || []) notices.append(el("li", "", "Asked " + m.agent + "; the reply will be posted here."));
      for (const n of res.data.notices || []) notices.append(el("li", "", n));
    }
    announce("Posted to " + room + ".");
  } catch (e) {
    roomError("Not posted to " + room + ": " + e.message);
  } finally {
    $("room-send").disabled = false;
    refreshRooms();
  }
}

function render() {
  renderConn();
  renderHeld();
  renderTabs();
  renderTeam();
  renderAsk();
  renderRooms();
}

// ---- Polling ----

let polling = false;
async function refresh() {
  if (polling) return;
  polling = true;
  const gen = heldGen;
  try {
    const [held, agents] = await Promise.all([api("GET", "/v1/admin/held"), api("GET", "/v1/agents")]);
    if (held.status === 403) {
      state.notAdmin = true;
      state.held.clear();
      $("held-list").replaceChildren();
      state.loaded = true;
      return;
    }
    if (held.status !== 200 || agents.status !== 200) throw new Error("relay answered " + held.status + "/" + agents.status);
    state.notAdmin = false;
    state.offline = false;
    state.loaded = true;
    state.updated = new Date();
    mergeHeld(held.data || []);
    // A listing that started before the page last hid does not count.
    state.heldFresh = gen === heldGen;
    state.roster = (agents.data && agents.data.agents) || [];
    state.relayVersion = (agents.data && agents.data.relay_version) || "";
    if (!state.meChecked) await whoami();
    if (sentNeeded()) refreshSent();
    if (state.tab === "rooms") refreshRooms();
  } catch (_) {
    state.offline = true;
  } finally {
    polling = false;
    render();
    if (gen !== heldGen && document.visibilityState === "visible") refresh();
  }
}

// heldText is what a held card shows the owner before they decide.
function heldText(item) {
  return JSON.stringify([item.body, item.then && item.then.body]);
}

function mergeHeld(items) {
  const seen = new Set();
  for (const item of items) {
    seen.add(item.id);
    const entry = state.held.get(item.id);
    if (entry) {
      // Fresh nonces, and text the relay cut can come back whole, so a card
      // never offers a fresh nonce for text it no longer matches.
      const old = entry.item;
      entry.item = item;
      if (entry.li && heldText(old) !== heldText(item)) entry.li.replaceWith(heldCard(entry));
    } else {
      state.held.set(item.id, { item, li: null, deciding: false, note: "", noteAt: 0 });
    }
  }
  for (const [id, entry] of state.held) {
    // A card this page decided stays a few seconds so its result can be read.
    const showing = entry.note && Date.now() - entry.noteAt < 8000;
    if (!seen.has(id) && !entry.deciding && !showing) {
      if (entry.li) entry.li.remove();
      state.held.delete(id);
    }
  }
  const n = items.length;
  if (n !== state.lastHeldCount) {
    if (state.lastHeldCount >= 0 || n > 0) announce(n === 0 ? "No held asks." : plural(n, "held ask") + " waiting.");
    state.lastHeldCount = n;
  }
}

// ---- Deciding ----

async function decide(id, verb) {
  const entry = state.held.get(id);
  if (!entry || !buttonsEnabled(entry)) return;
  const item = entry.item;
  if (verb === "approve" && !state.meChecked) {
    // whoami has not answered, so this ask may be this phone's own.
    if (!window.confirm("This page has not yet confirmed which agent is this phone's, so this ask may be its own. Approve it anyway?")) return;
  } else if (verb === "approve" && state.me && (item.from === state.me || (item.chain || []).includes(state.me))) {
    if (!window.confirm("This ask came from this phone's own agent (" + state.me + "). Approve it anyway?")) return;
  }
  if (!buttonsEnabled(entry)) {
    refresh();
    return;
  }
  entry.deciding = true;
  updateCard(entry);
  const nonce = verb === "approve" ? item.approve_nonce : item.deny_nonce;
  let res;
  try {
    res = await api("POST", "/v1/admin/requests/" + encodeURIComponent(id) + "/" + verb, { nonce });
  } catch (_) {
    entry.deciding = false;
    state.offline = true;
    render();
    announce("Offline. This ask may not have been decided; the held list shows it once the relay answers.");
    return;
  }
  entry.deciding = false;
  if (res.status === 200 && res.data) {
    if (res.data.already_decided) {
      entry.note = "Already decided: " + res.data.status + ".";
    } else {
      entry.note = verb === "approve" ? "Approved." : "Denied.";
    }
    entry.noteAt = Date.now();
    announce(entry.note);
  } else if (res.status === 409) {
    announce("That button expired. Refreshing.");
  } else if (res.status === 403) {
    state.notAdmin = true;
  } else {
    entry.note = "";
    announce("Could not " + verb + ": " + ((res.data && res.data.error) || "error " + res.status));
  }
  render();
  refresh();
}

// ---- Push alerts ----
// The relay pushes a content-free alert for each held ask to every
// subscribed admin phone. The page asks for permission only from a tap,
// re-sends its subscription on every open, and says which state applies.

function b64urlBytes(s) {
  const b64 = (s + "=".repeat((4 - (s.length % 4)) % 4)).replace(/-/g, "+").replace(/_/g, "/");
  return Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
}

function standalone() {
  return window.matchMedia("(display-mode: standalone)").matches || navigator.standalone === true;
}

// alertsOff reads, or with on given sets, this device's choice to keep
// alerts off, so opening the page again does not subscribe it anew.
// Storage may be off; then the choice lasts only while the page is open.
let alertsOffHere = false;
function alertsOff(on) {
  if (on !== undefined) alertsOffHere = on;
  try {
    if (on === true) localStorage.setItem("tincan-phone-alerts-off", "1");
    else if (on === false) localStorage.removeItem("tincan-phone-alerts-off");
    else return alertsOffHere || localStorage.getItem("tincan-phone-alerts-off") === "1";
  } catch (_) {
    // Keep the in-page choice.
  }
  return alertsOffHere;
}

// unsubscribe removes this browser's subscription from the relay and the
// push service.
async function unsubscribe(reg) {
  const sub = await reg.pushManager.getSubscription();
  if (sub) {
    await api("DELETE", "/v1/admin/push/subscription", { endpoint: sub.endpoint });
    await sub.unsubscribe();
  }
}

function setAlerts(text, label, onClick) {
  $("alerts-text").textContent = text;
  const b = $("alerts-button");
  b.hidden = !label;
  b.disabled = false;
  b.textContent = label || "";
  b.onclick = onClick || null;
}

// subscribe returns this browser's push subscription for the relay's
// current key: the existing one when it was made with that key (subscribe
// hands it back), else a new one replacing a subscription made with an
// older key, which the relay is told to forget.
async function subscribe(reg, publicKey) {
  const opts = { userVisibleOnly: true, applicationServerKey: b64urlBytes(publicKey) };
  try {
    return await reg.pushManager.subscribe(opts);
  } catch (_) {
    const old = await reg.pushManager.getSubscription();
    if (old) {
      await api("DELETE", "/v1/admin/push/subscription", { endpoint: old.endpoint }).catch(() => {});
      await old.unsubscribe();
    }
    return reg.pushManager.subscribe(opts);
  }
}

async function pushState() {
  try {
    if (!window.isSecureContext || location.protocol !== "https:") {
      setAlerts("Alerts need HTTPS on your tailnet. Until then, the approval notify teammate is the alert path.");
      return;
    }
    if (!("serviceWorker" in navigator) || !("PushManager" in window) || !("Notification" in window)) {
      setAlerts(standalone() ? "This browser cannot show alerts." : "For alerts, add this page to your Home Screen and open it from there.");
      return;
    }
    if (Notification.permission === "denied") {
      setAlerts("Alerts are blocked. Allow notifications for Tincan in Settings.");
      return;
    }
    if (alertsOff()) {
      // Turned off on this device: never subscribe again until Turn on
      // alerts. A removal that failed earlier is tried again quietly.
      if (Notification.permission === "granted") {
        navigator.serviceWorker.ready.then(unsubscribe).catch(() => {});
      }
      setAlerts("Alerts are off on this phone.", "Turn on alerts", turnOnAlerts);
      return;
    }
    if (Notification.permission === "granted") {
      const key = await api("GET", "/v1/admin/push/key");
      if (key.status === 404) {
        setAlerts("Push alerts are off on this relay.");
        return;
      }
      const reg = await navigator.serviceWorker.ready;
      // Even with a subscription already here, check it against the relay's
      // current key: one made with a replaced key gets no alerts.
      const sub = key.status === 200 && key.data ? await subscribe(reg, key.data.public_key) : null;
      if (sub) {
        const res = await api("PUT", "/v1/admin/push/subscription", sub.toJSON());
        if (res.status === 200) {
          setAlerts("Alerts are on for held asks.", "Turn off", turnOffAlerts);
          return;
        }
        if (res.status === 404) {
          setAlerts("Push alerts are off on this relay.");
          return;
        }
      }
    }
    setAlerts("Get an alert on this phone when an ask is held for you.", "Turn on alerts", turnOnAlerts);
  } catch (_) {
    setAlerts("Could not set up alerts. Reopen the page to try again.");
  }
}

async function turnOnAlerts() {
  $("alerts-button").disabled = true;
  alertsOff(false);
  try {
    await Notification.requestPermission();
  } catch (_) {
    // Older Safari takes a callback; the state check below covers it.
  }
  await pushState();
  announce($("alerts-text").textContent);
}

async function turnOffAlerts() {
  $("alerts-button").disabled = true;
  alertsOff(true);
  try {
    await unsubscribe(await navigator.serviceWorker.ready);
    setAlerts("Alerts are off on this phone.", "Turn on alerts", turnOnAlerts);
  } catch (_) {
    setAlerts("Could not turn alerts off. Try again.", "Turn off", turnOffAlerts);
  }
  announce($("alerts-text").textContent);
}

// ---- Start ----

let timer = null;
// heldGen counts hides and returns; a held listing read across one is not
// fresh.
let heldGen = 0;
function schedule() {
  clearInterval(timer);
  timer = null;
  // Decision codes outlive a background stay, so the cards stay off until
  // a held listing read after this lands.
  heldGen++;
  state.heldFresh = false;
  renderHeld();
  if (document.visibilityState === "visible") {
    refresh();
    timer = setInterval(refresh, POLL_MS);
  }
}

document.addEventListener("visibilitychange", schedule);
// A tapped alert opens #held, in this window if it is already open.
window.addEventListener("hashchange", () => {
  if (location.hash === "#held") $("held").scrollIntoView();
});

$("tab-team").addEventListener("click", () => { state.tab = "team"; render(); });
$("tab-ask").addEventListener("click", () => { state.tab = "ask"; refreshSent(); render(); });
$("tab-rooms").addEventListener("click", () => { state.tab = "rooms"; render(); refreshRooms(); });
$("room-pick").addEventListener("change", () => {
  switchRoom($("room-pick").value);
  $("room-notices").replaceChildren();
  roomError("");
  refreshRooms();
});
$("room-create").addEventListener("click", createRoom);
$("room-add").addEventListener("click", addMember);
$("room-send").addEventListener("click", postToRoom);
$("join-button").addEventListener("click", join);
$("ask-send").addEventListener("click", sendAsk);

function start() {
  if ("serviceWorker" in navigator) {
    navigator.serviceWorker.register("sw.js", { scope: "./" }).catch(() => {});
  }
  if (location.hash === "#held") $("held").scrollIntoView();
  render();
  schedule();
  pushState();
}

start();
