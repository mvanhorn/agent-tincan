// Tincan relay setup console. It reads and acts only through the relay's
// own admin routes, as the CLI on an admin device does. Everything the
// relay returns (agent names, good-at lines, relay URLs, invite codes) is
// untrusted text and reaches the page only through textContent, never as
// markup.
"use strict";

const API_TIMEOUT_MS = 15000;

const state = {
  agents: [],
  kinds: [],
  relayURLs: [],
  relayVersion: "",
  offline: false,
  loaded: false,
};

const $ = (id) => document.getElementById(id);
const NAME_RE = /^[a-z0-9][a-z0-9-]{0,31}$/;

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

// api calls the relay. A body is sent as JSON, which the relay requires of
// every state-changing browser request. It throws, as when offline, if the
// relay has not answered within API_TIMEOUT_MS; the error has timeout set.
async function api(method, path, body) {
  const ctl = new AbortController();
  const stall = setTimeout(() => ctl.abort(), API_TIMEOUT_MS);
  const init = { method, credentials: "same-origin", cache: "no-store", headers: {}, signal: ctl.signal };
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

function fail(msg, err) {
  return msg + ": " + ((err && err.message) || "offline");
}

// ---- Relay section ----

function renderRelay() {
  $("relay-version").textContent = "Version: " + (state.relayVersion || "unknown");
  const ul = $("relay-urls");
  ul.replaceChildren();
  if (state.relayURLs.length === 0) {
    ul.append(el("li", "", "No relay addresses reported."));
    return;
  }
  for (const u of state.relayURLs) ul.append(el("li", "", u));
}

// ---- Add an agent ----

// fillKinds rebuilds the kind select from the relay's kind list, keeping
// the selection when it is still there.
function fillKinds() {
  const sel = $("add-kind");
  const current = sel.value;
  sel.replaceChildren();
  for (const k of state.kinds) {
    const opt = el("option", "", k === "" ? "(no kind)" : k);
    opt.value = k;
    sel.append(opt);
  }
  if (state.kinds.includes(current)) sel.value = current;
}

async function invite() {
  const err = $("add-error");
  err.hidden = true;
  const name = $("add-name").value.trim().toLowerCase();
  if (!NAME_RE.test(name)) {
    err.textContent = "Agent names are 1-32 lowercase letters, digits and dashes, starting with a letter or digit.";
    err.hidden = false;
    return;
  }
  const kind = $("add-kind").value;
  const btn = $("add-button");
  btn.disabled = true;
  try {
    const res = await api("POST", "/v1/admin/invite", { name, kind, new_only: $("add-new-only").checked });
    if (res.status !== 200 || !res.data || !res.data.code) {
      throw new Error((res.data && res.data.error) || ("invite failed (" + res.status + ")"));
    }
    const relay = state.relayURLs.length > 0 ? state.relayURLs[0] : "http://tincan-relay";
    $("invite-name").textContent = res.data.name;
    $("invite-expires").textContent = res.data.expires_in || "10m";
    $("invite-install").textContent = INSTALL_CMD;
    $("invite-command").textContent = "tincan join " + res.data.code + " --relay " + relay;
    $("invite-result").hidden = false;
    $("invite-copied").hidden = true;
    announce("Invite minted for " + res.data.name + ".");
    await refreshAgents();
  } catch (e) {
    err.textContent = fail("Could not mint invite", e);
    err.hidden = false;
  } finally {
    btn.disabled = false;
  }
}

// INSTALL_CMD is how a fresh machine gets tincan, as the quickstart
// documents. The agent's machine may not have it yet.
const INSTALL_CMD = "curl -fsSL https://agenttincan.com/install.sh | sh";

async function copyId(id) {
  const text = $(id).textContent;
  try {
    await navigator.clipboard.writeText(text);
  } catch (_) {
    // Clipboard needs a secure context; fall back to selecting the text.
    const range = document.createRange();
    range.selectNodeContents($(id));
    const sel = getSelection();
    if (sel) {
      sel.removeAllRanges();
      sel.addRange(range);
    }
    return;
  }
  $("invite-copied").hidden = false;
}

// ---- Agents section ----

function statusLine(a) {
  const bits = [];
  if (a.status) bits.push(a.status);
  if (a.status_reason) bits.push(a.status_reason);
  if (a.wake) bits.push("wake: " + a.wake);
  if (a.kind) bits.push("kind: " + a.kind);
  return bits.join(" · ") || "joined";
}

function renderAgents() {
  const list = $("agents-list");
  list.replaceChildren();
  $("agents-empty").hidden = state.agents.length !== 0;
  for (const a of state.agents) {
    const li = el("li", "");
    li.append(el("h3", "", a.name));
    li.append(el("p", "meta", statusLine(a)));
    if (a.good_at) li.append(el("p", "", a.good_at));

    const facts = el("dl", "grid");
    if (a.last_active) {
      facts.append(el("dt", "", "Last active"));
      facts.append(el("dd", "", new Date(a.last_active).toLocaleString()));
    }
    if (a.version) {
      facts.append(el("dt", "", "Build"));
      facts.append(el("dd", "", a.version));
    }
    if (a.fallback) {
      facts.append(el("dt", "", "Fallback"));
      facts.append(el("dd", "", a.fallback));
    }
    if (facts.children.length > 0) li.append(facts);

    li.append(configDetails(a));
    list.append(li);
  }
}

// configDetails is the per-agent editor: kind, good-at line, webhook wake
// and removal. Each save reads its own fields and reports its own error,
// so one agent's edit never touches another's.
function configDetails(a) {
  const d = document.createElement("details");
  d.append(el("summary", "", "Configure " + a.name));
  const wrap = el("div", "");

  const kindLabel = el("label", "", "Kind");
  kindLabel.htmlFor = "kind-" + a.name;
  const kindSel = document.createElement("select");
  kindSel.id = "kind-" + a.name;
  kindSel.className = "field";
  for (const k of state.kinds) {
    const opt = el("option", "", k === "" ? "(no kind)" : k);
    opt.value = k;
    kindSel.append(opt);
  }
  kindSel.value = a.kind || "";
  if (!state.kinds.includes(a.kind || "")) {
    const opt = el("option", "", (a.kind || "(unknown kind)") + " (relay reports)");
    opt.value = a.kind || "";
    kindSel.append(opt);
    kindSel.value = a.kind || "";
  }

  const goodLabel = el("label", "", "Good-at line");
  goodLabel.htmlFor = "goodat-" + a.name;
  const goodIn = document.createElement("input");
  goodIn.id = "goodat-" + a.name;
  goodIn.className = "field";
  goodIn.type = "text";
  goodIn.value = a.good_at || "";
  goodIn.placeholder = "What this agent is good at";

  const saveMeta = el("p", "meta", "Kind and good-at save together.");
  const saveBtn = el("button", "primary", "Save kind and good-at");
  saveBtn.type = "button";
  const saveErr = el("p", "error", "");
  saveErr.hidden = true;
  saveErr.setAttribute("role", "alert");
  saveBtn.addEventListener("click", async () => {
    saveErr.hidden = true;
    saveBtn.disabled = true;
    try {
      const k = await api("PUT", "/v1/agents/" + encodeURIComponent(a.name) + "/kind", { kind: kindSel.value });
      if (k.status !== 200) throw new Error((k.data && k.data.error) || ("saving kind failed (" + k.status + ")"));
      const g = await api("PUT", "/v1/agents/" + encodeURIComponent(a.name) + "/good-at", { good_at: goodIn.value });
      if (g.status !== 200) throw new Error((g.data && g.data.error) || ("saving good-at failed (" + g.status + ")"));
      announce(a.name + " updated.");
      await refreshAgents();
    } catch (e) {
      saveErr.textContent = fail("Could not save", e);
      saveErr.hidden = false;
    } finally {
      saveBtn.disabled = false;
    }
  });

  const wakeLabel = el("label", "", "Webhook URL");
  wakeLabel.htmlFor = "webhook-" + a.name;
  const wakeIn = document.createElement("input");
  wakeIn.id = "webhook-" + a.name;
  wakeIn.className = "field";
  wakeIn.type = "url";
  wakeIn.autocapitalize = "none";
  wakeIn.autocomplete = "off";
  wakeIn.spellcheck = false;
  wakeIn.placeholder = "https://agent-machine:8787/wake";
  const tokenLabel = el("label", "", "Bearer token (optional)");
  tokenLabel.htmlFor = "token-" + a.name;
  const tokenIn = document.createElement("input");
  tokenIn.id = "token-" + a.name;
  tokenIn.className = "field";
  tokenIn.type = "password";
  tokenIn.autocomplete = "off";
  const wakeMeta = el("p", "meta", "Sets a webhook wake for this agent. Leave the token blank to keep the current one.");
  const wakeBtn = el("button", "", "Save webhook");
  wakeBtn.type = "button";
  const wakeErr = el("p", "error", "");
  wakeErr.hidden = true;
  wakeErr.setAttribute("role", "alert");
  wakeBtn.addEventListener("click", async () => {
    wakeErr.hidden = true;
    wakeBtn.disabled = true;
    try {
      if (!wakeIn.value.trim()) throw new Error("a webhook URL is required");
      const body = { webhook: wakeIn.value.trim() };
      if (tokenIn.value) body.bearer_token = tokenIn.value;
      const res = await api("PUT", "/v1/admin/wake-config/" + encodeURIComponent(a.name), body);
      if (res.status !== 200) throw new Error((res.data && res.data.error) || ("saving webhook failed (" + res.status + ")"));
      tokenIn.value = "";
      announce("Webhook saved for " + a.name + ".");
      await refreshAgents();
    } catch (e) {
      wakeErr.textContent = fail("Could not save webhook", e);
      wakeErr.hidden = false;
    } finally {
      wakeBtn.disabled = false;
    }
  });

  const removeBtn = el("button", "danger", "Remove " + a.name);
  removeBtn.type = "button";
  const removeErr = el("p", "error", "");
  removeErr.hidden = true;
  removeErr.setAttribute("role", "alert");
  removeBtn.addEventListener("click", async () => {
    removeErr.hidden = true;
    if (!window.confirm("Remove " + a.name + "? Its tokens are revoked and its queued requests cancelled.")) return;
    removeBtn.disabled = true;
    try {
      const res = await api("POST", "/v1/admin/remove", { name: a.name });
      if (res.status !== 200) throw new Error((res.data && res.data.error) || ("remove failed (" + res.status + ")"));
      announce(a.name + " removed.");
      await refreshAgents();
    } catch (e) {
      removeErr.textContent = fail("Could not remove", e);
      removeErr.hidden = false;
    } finally {
      removeBtn.disabled = false;
    }
  });

  wrap.append(kindLabel, kindSel, goodLabel, goodIn, saveMeta, saveBtn, saveErr);
  wrap.append(el("h3", "", "Wake"));
  wrap.append(wakeLabel, wakeIn, tokenLabel, tokenIn, wakeMeta, wakeBtn, wakeErr);
  wrap.append(el("h3", "", "Danger"));
  wrap.append(removeBtn, removeErr);
  d.append(wrap);
  return d;
}

// ---- Loading ----

async function refreshAgents() {
  const res = await api("GET", "/v1/agents");
  if (res.status !== 200 || !res.data) throw new Error((res.data && res.data.error) || ("agents (" + res.status + ")"));
  state.agents = res.data.agents || [];
  if (res.data.relay_version) state.relayVersion = res.data.relay_version;
  renderAgents();
  renderRelay();
}

async function refreshAll() {
  const banner = $("banner");
  try {
    const [agents, urls, kinds] = await Promise.all([
      api("GET", "/v1/agents"),
      api("GET", "/v1/admin/urls"),
      api("GET", "/v1/admin/kinds"),
    ]);
    if (agents.status !== 200 || !agents.data) throw new Error((agents.data && agents.data.error) || ("agents (" + agents.status + ")"));
    state.agents = agents.data.agents || [];
    if (agents.data.relay_version) state.relayVersion = agents.data.relay_version;
    if (urls.status === 200 && urls.data) state.relayURLs = urls.data.relay_urls || [];
    if (kinds.status === 200 && kinds.data) state.kinds = ["", ...(kinds.data.kinds || [])];
    else state.kinds = [""];
    state.loaded = true;
    state.offline = false;
    banner.hidden = true;
    fillKinds();
    renderAgents();
    renderRelay();
    $("conn").textContent = "Connected";
  } catch (e) {
    state.offline = true;
    banner.textContent = fail("The relay is not answering", e);
    banner.hidden = false;
    $("conn").textContent = "Offline";
  }
  if (!state.loaded) setTimeout(refreshAll, 3000);
}

$("add-button").addEventListener("click", invite);
$("invite-install-copy").addEventListener("click", () => copyId("invite-install"));
$("invite-copy").addEventListener("click", () => copyId("invite-command"));
refreshAll();
