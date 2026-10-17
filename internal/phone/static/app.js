// Tincan phone page. It reads and acts only through the relay's own routes,
// as the CLI on an admin device does. Everything the relay returns (agent
// names, good-at lines, request text, attachment names) is untrusted text and
// reaches the page only through textContent, never as markup.
"use strict";

const POLL_MS = 5000;

const state = {
  held: new Map(), // request id -> {item, li, deciding, note}
  roster: [],
  relayVersion: "",
  updated: null, // Date of the last good poll
  offline: false,
  notAdmin: false,
  loaded: false,
  me: "", // this phone's own agent, once it has joined
  lastHeldCount: -1,
};

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
// every state-changing browser request.
async function api(method, path, body) {
  const init = { method, credentials: "same-origin", cache: "no-store", headers: {} };
  if (state.me) init.headers["X-Tincan-Agent"] = state.me;
  if (body !== undefined) {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(body);
  }
  const resp = await fetch(path, init);
  let data = null;
  try { data = await resp.json(); } catch (_) { data = null; }
  return { status: resp.status, data };
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
  return !state.offline && !state.notAdmin && !entry.deciding && !entry.note && nonceLive(entry.item);
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
  } else if (!nonceLive(entry.item) && !state.offline) {
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

function render() {
  renderConn();
  renderHeld();
  renderTeam();
}

// ---- Polling ----

let polling = false;
async function refresh() {
  if (polling) return;
  polling = true;
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
    state.roster = (agents.data && agents.data.agents) || [];
    state.relayVersion = (agents.data && agents.data.relay_version) || "";
  } catch (_) {
    state.offline = true;
  } finally {
    polling = false;
    render();
  }
}

function mergeHeld(items) {
  const seen = new Set();
  for (const item of items) {
    seen.add(item.id);
    const entry = state.held.get(item.id);
    if (entry) {
      entry.item = item; // fresh nonces; the request itself does not change
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
  if (verb === "approve" && state.me && (item.from === state.me || (item.chain || []).includes(state.me))) {
    if (!window.confirm("This ask came from this phone's own agent (" + state.me + "). Approve it anyway?")) return;
  }
  if (!nonceLive(item)) {
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
    announce("Offline. Nothing was decided.");
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

// ---- Start ----

let timer = null;
function schedule() {
  clearInterval(timer);
  timer = null;
  if (document.visibilityState === "visible") {
    refresh();
    timer = setInterval(refresh, POLL_MS);
  }
}

document.addEventListener("visibilitychange", schedule);

function start() {
  if ("serviceWorker" in navigator) {
    navigator.serviceWorker.register("sw.js", { scope: "./" }).catch(() => {});
  }
  if (location.hash === "#held") $("held").scrollIntoView();
  render();
  schedule();
}

start();
