const { JSDOM } = require("jsdom");
const fs = require("node:fs");
const assert = require("node:assert/strict");
const cp = require("node:child_process");
const path = require("node:path");
const root = path.resolve(__dirname, "../..");
const read = (f) => fs.readFileSync(path.join(root, "site", f), "utf8");
const baseline = new JSDOM(
  cp.execFileSync(
    "git",
    ["show", "cce368f6377e02dbd71ba540e2e62c53fa3d6325:site/index.html"],
    { cwd: root, encoding: "utf8" },
  ),
).window.document;
const html = read("index.html");
const parsed = new JSDOM(html).window.document;
for (const node of baseline.querySelectorAll('[id^="prompt-"]'))
  assert.equal(
    parsed.getElementById(node.id).textContent,
    node.textContent,
    node.id,
  );
for (const id of [
  "top",
  "main",
  "why",
  "how",
  "new",
  "council",
  "platforms",
  "history",
  "teammates",
  "trust",
  "get-started",
])
  assert.ok(parsed.getElementById(id), id);
const normalize = (text) => text.trim().replace(/\s+/g, " ");
const platformText = normalize(parsed.querySelector("#platforms").textContent);
for (const old of baseline.querySelectorAll(".platform p, .platform dt, .platform dd"))
  assert.ok(platformText.includes(normalize(old.textContent)), normalize(old.textContent));
const destinations = new Set(Array.from(parsed.querySelectorAll("a[href]"), a => a.getAttribute("href")));
for (const old of baseline.querySelectorAll('a[href^="https://"]'))
  assert.ok(destinations.has(old.getAttribute("href")), old.getAttribute("href"));
for (const a of parsed.querySelectorAll('a[href^="#"]'))
  assert.ok(parsed.getElementById(a.hash.slice(1)), a.hash);
for (const f of [
  "agents.txt",
  "llms.txt",
  "install.sh",
  "privacy.html",
  "vercel.json",
])
  assert.equal(
    read(f),
    cp.execFileSync(
      "git",
      ["show", "cce368f6377e02dbd71ba540e2e62c53fa3d6325:site/" + f],
      { cwd: root, encoding: "utf8" },
    ),
    f,
  );
console.log(
  "PASS exact prompts, stable anchors, local links, unchanged endpoints/policy/security config",
);
async function run({
  reduced = false,
  blocked = false,
  file = "index.html",
} = {}) {
  const dom = new JSDOM(read(file), {
    url: "https://agenttincan.test",
    runScripts: "outside-only",
    pretendToBeVisual: true,
  });
  const w = dom.window;
  w.matchMedia = (q) => ({
    matches: q.includes("reduced-motion") ? reduced : false,
    addEventListener() {},
  });
  w.fetch = () => Promise.reject(new Error("offline"));
  const scrollCalls = [];
  w.scrollTo = (options) => scrollCalls.push(options);
  if (blocked)
    Object.defineProperty(w, "localStorage", {
      get() {
        throw new Error("blocked");
      },
    });
  w.HTMLElement.prototype.scrollIntoView = function () {};
  w.eval(read("script.js"));
  if (file === "index.html") w.eval(read("motion.js"));
  await new Promise((r) => setTimeout(r, 40));
  const d = w.document;
  if (file === "index.html") {
    const tabs = d.querySelectorAll(".demo-tab");
    assert.equal(tabs.length, 4);
    tabs[1].click();
    assert.equal(tabs[1].getAttribute("aria-current"), "step");
    assert.match(d.querySelector("#demo-example-1").textContent, /Luigi/);
    const panels = d.querySelectorAll(".demo-chapter");
    assert.equal(panels.length, 4);
    assert.deepEqual(
      Array.from(panels, (p) => p.querySelectorAll(".msg").length),
      [4, 4, 4, 6],
    );
    if (reduced) {
      panels.forEach((p, i) => {
        p.getBoundingClientRect = () => ({ top: (i - 2) * 1000 });
      });
      w.dispatchEvent(new w.Event("scroll"));
      await new Promise((r) => setTimeout(r, 40));
      assert.equal(tabs[2].getAttribute("aria-current"), "step");
      assert.equal(d.querySelector(".demo-transcripts").open, true);
    } else {
      const track = d.querySelector(".hero-demo");
      const stage = d.querySelector(".demo-stage");
      Object.defineProperty(track, "offsetHeight", { value: 4000 });
      Object.defineProperty(stage, "offsetHeight", { value: 800 });
      for (const i of [0, 1, 2, 3, 2, 1, 0]) {
        track.getBoundingClientRect = () => ({ top: 72 - (i + 0.1) * 800 });
        w.dispatchEvent(new w.Event("scroll"));
        await new Promise((r) => setTimeout(r, 30));
        assert.equal(tabs[i].getAttribute("aria-current"), "step");
      }
    }
    if (!reduced) {
      const probe = d.querySelector(".demo-viewport-probe");
      Object.defineProperty(probe, "offsetHeight", { value: 780 });
      Object.defineProperty(w, "innerHeight", { value: 664, configurable: true });
      w.dispatchEvent(new w.Event("resize"));
      assert.ok(d.querySelector(".hero-demo").classList.contains("demo-pinned"), "browser toolbar must not unpin demo");
      Object.defineProperty(w, "innerHeight", { value: 780, configurable: true });
    }
    const toggle = d.getElementById("motion-toggle");
    assert.equal(toggle.getAttribute("aria-pressed"), String(!reduced));
    toggle.click();
    assert.equal(toggle.getAttribute("aria-pressed"), String(reduced));
    if (!reduced) {
      const transcript = d.querySelector(".demo-transcripts");
      transcript.getBoundingClientRect = () => ({ top: -300, bottom: 2000 });
      const before = scrollCalls.length;
      toggle.click();
      assert.ok(scrollCalls.length > before, "re-enabling motion must restore the visible transcript");
      assert.ok(d.querySelector(".hero-demo").classList.contains("demo-pinned"));
    }
    const copy = d.querySelector('[data-copy="prompt-relay"]');
    copy.click();
    assert.equal(
      w.getSelection().toString(),
      d.getElementById("prompt-relay").textContent,
    );
    const detail = d.querySelector(".platform details");
    detail.open = true;
    detail.querySelector("summary").focus();
    detail.dispatchEvent(
      new w.KeyboardEvent("keydown", { key: "Escape", bubbles: true }),
    );
    assert.equal(detail.open, false);
  }
  d.getElementById("theme-toggle").click();
  assert.equal(d.documentElement.dataset.theme, "dark");
  dom.window.close();
  console.log(
    "PASS DOM interactions",
    JSON.stringify({ reduced, blocked, file }),
  );
}
(async () => {
  await run();
  await run({ reduced: true });
  await run({ blocked: true });
  await run({ file: "privacy.html" });
})().catch((e) => {
  console.error(e);
  process.exitCode = 1;
});
