// Theme toggle. Loaded in <head> so a saved choice applies before first paint.
(function () {
  var root = document.documentElement;
  var KEY = "tincan-theme";

  function saved() {
    try {
      return localStorage.getItem(KEY);
    } catch (e) {
      return null;
    }
  }
  function current() {
    var t = root.getAttribute("data-theme");
    if (t) return t;
    return window.matchMedia &&
      window.matchMedia("(prefers-color-scheme: dark)").matches
      ? "dark"
      : "light";
  }
  function label(theme) {
    var btn = document.getElementById("theme-toggle");
    if (btn) {
      var text =
        theme === "dark" ? "Switch to light theme" : "Switch to dark theme";
      btn.setAttribute("aria-label", text);
      btn.setAttribute("title", text);
    }
  }
  function apply(theme) {
    root.setAttribute("data-theme", theme);
    label(theme);
  }

  var s = saved();
  if (s === "light" || s === "dark") root.setAttribute("data-theme", s);

  document.addEventListener("DOMContentLoaded", function () {
    var btn = document.getElementById("theme-toggle");
    if (!btn) return;
    label(current());
    if (window.matchMedia) {
      var mq = window.matchMedia("(prefers-color-scheme: dark)");
      var onChange = function () {
        if (!root.getAttribute("data-theme")) label(current());
      };
      if (mq.addEventListener) mq.addEventListener("change", onChange);
    }
    btn.addEventListener("click", function () {
      var next = current() === "dark" ? "light" : "dark";
      apply(next);
      try {
        localStorage.setItem(KEY, next);
      } catch (e) {}
    });
  });
})();

// GitHub star count. Shows the count when the API answers; otherwise the
// badge stays as it is, with no count and no error text.
(function () {
  var API = "https://api.github.com/repos/mvanhorn/agent-tincan";

  function formatCount(n) {
    if (n < 1000) return String(n);
    if (n < 1000000) {
      var k = Math.round(n / 100) / 10;
      if (k < 1000) return (k % 1 === 0 ? k.toFixed(0) : k.toFixed(1)) + "k";
    }
    var m = Math.round(n / 100000) / 10;
    return (m % 1 === 0 ? m.toFixed(0) : m.toFixed(1)) + "m";
  }

  function show(count) {
    var text = formatCount(count);
    try {
      localStorage.setItem("tincan-stars", String(count));
    } catch (_) {}
    var slots = document.querySelectorAll("[data-gh-stars]");
    for (var i = 0; i < slots.length; i++) {
      var num = slots[i].querySelector("[data-gh-count]");
      if (num) num.textContent = text;
      slots[i].hidden = false;
    }
  }

  function load() {
    if (
      !document.querySelector("[data-gh-stars]") ||
      typeof fetch !== "function"
    )
      return;
    try {
      var cached = localStorage.getItem("tincan-stars");
      if (cached !== null && /^\d+$/.test(cached)) show(Number(cached));
    } catch (_) {}
    fetch(API, { credentials: "omit" })
      .then(function (r) {
        return r.ok ? r.json() : null;
      })
      .then(function (data) {
        if (
          data &&
          typeof data.stargazers_count === "number" &&
          data.stargazers_count >= 0
        ) {
          show(data.stargazers_count);
        }
      })
      .catch(function () {});
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", load);
  } else {
    load();
  }
})();

// Scroll through real conversations. Dashed cards show agent-to-agent requests.
(function () {
  var SCENES = [
    {
      tab: "Your phone",
      steps: [
        {
          kind: "you",
          text: "What did ChatGPT tell me about the lease last night? Send me the screenshot.",
        },
        {
          kind: "hop",
          from: "Grok Bot",
          to: "history",
          text: "Find last night's ChatGPT chat about the lease, with the screenshot.",
        },
        {
          kind: "back",
          from: "history",
          to: "Grok Bot",
          text: 'Found "Lease renewal questions" from 11:42 PM.',
          attach: "lease-clause.png",
        },
        {
          kind: "agent",
          who: "Grok Bot",
          text: "ChatGPT said the early-termination fee is two months' rent. Here's the screenshot.",
          attach: "lease-clause.png",
        },
      ],
    },
    {
      tab: "Errands",
      steps: [
        {
          kind: "you",
          text: "Can you get me a table for two at Luigi’s tonight around 7?",
        },
        {
          kind: "hop",
          from: "Grok Bot",
          to: "Muse",
          text: "Call Luigi's and book a table for two at 7pm tonight. Don't pay anything.",
        },
        {
          kind: "back",
          from: "Muse",
          to: "Grok Bot",
          text: "Called. 7:00 was full, booked 7:15 for two under your name. They hold it 15 minutes.",
        },
        {
          kind: "agent",
          who: "Grok Bot",
          text: "You're booked at Luigi's at 7:15 for two. 7:00 was taken.",
        },
      ],
    },
    {
      tab: "Code",
      steps: [
        {
          kind: "you",
          text: "Did the automation PR merge? If CI failed, fix it.",
        },
        {
          kind: "hop",
          from: "Grok Bot",
          to: "Codex",
          text: "Check the automation PR. If CI is red, fix it and merge.",
        },
        {
          kind: "back",
          from: "Codex",
          to: "Grok Bot",
          text: "CI failed on lint. Pushed a fix, checks are green, merged #212.",
        },
        {
          kind: "agent",
          who: "Grok Bot",
          text: "Merged. Codex fixed a lint failure first. PR #212 is in.",
        },
      ],
    },
    {
      tab: "Second opinion",
      steps: [
        {
          kind: "you",
          text: "Ask ChatGPT and Claude which tagline is better, and show me both answers.",
        },
        {
          kind: "hop",
          from: "Grok Bot",
          to: "chatgpt-web",
          text: "Which tagline is better, A or B? One line of reasoning.",
        },
        {
          kind: "hop",
          from: "Grok Bot",
          to: "claude-web",
          text: "Which tagline is better, A or B? One line of reasoning.",
        },
        {
          kind: "back",
          from: "chatgpt-web",
          to: "Grok Bot",
          text: "B. It says what the product does in five words.",
        },
        {
          kind: "back",
          from: "claude-web",
          to: "Grok Bot",
          text: "B, narrowly. A is catchier but vaguer.",
        },
        {
          kind: "agent",
          who: "Grok Bot",
          text: "Both pick B. ChatGPT: clearer. Claude: A is catchier but vaguer.",
        },
      ],
    },
  ];

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text) n.textContent = text;
    return n;
  }

  function bubble(step) {
    var li;
    if (step.kind === "you" || step.kind === "agent") {
      li = el("li", "msg " + (step.kind === "you" ? "msg-you" : "msg-agent"));
      li.appendChild(el("span", "who", step.kind === "you" ? "You" : step.who));
    } else {
      li = el("li", "msg msg-hop" + (step.kind === "back" ? " msg-back" : ""));
      var route = el("span", "hop-route");
      route.appendChild(el("b", "", step.from));
      var str = el("span", "hop-string");
      str.setAttribute("aria-hidden", "true");
      route.appendChild(str);
      route.appendChild(el("b", "", step.to));
      li.appendChild(route);
    }
    li.appendChild(el("p", "", step.text));
    if (step.attach) li.appendChild(el("span", "attach", step.attach));
    return li;
  }

  function init() {
    var feed = document.querySelector("[data-demo-feed]");
    var tabs = document.querySelector(".demo-tabs");
    var root = document.querySelector(".hero-demo");
    if (!feed || !tabs || !root) return;
    var headings = [
      "Your memory.\nWithin reach.",
      "Your evening.\nAlready sorted.",
      "Your code.\nMoving forward.",
      "Two perspectives.\nOne clearer answer.",
    ];
    var descriptions = [
      "The conversation you need, back on your phone.",
      "A quick message becomes a real reservation.",
      "Check the work. Fix the problem. Keep going.",
      "Let your agents compare notes for you.",
    ];
    tabs.setAttribute("role", "navigation");
    var template = root.querySelector(".demo-window");
    var transcript = el("details", "demo-transcripts");
    transcript.open = true;
    transcript.appendChild(el("summary", "", "Read the full conversations"));
    var panels = SCENES.map(function (scene, i) {
      var panel = template.cloneNode(true);
      panel.id = "demo-example-" + i;
      panel.classList.add("demo-chapter");
      panel.setAttribute("role", "region");
      panel.setAttribute("aria-label", scene.tab + " example");
      panel.querySelector("[data-demo-scene]").textContent = scene.tab;
      var messages = panel.querySelector("[data-demo-feed]");
      messages.removeAttribute("aria-live");
      messages.textContent = "";
      scene.steps.forEach(function (step) {
        messages.appendChild(bubble(step));
      });
      transcript.appendChild(panel);
      return panel;
    });
    template.remove();
    root.parentNode.appendChild(transcript);
    var stage = el("div", "demo-stage");
    stage.appendChild(tabs);
    var composition = el("div", "demo-composition");
    var copy = el("div", "demo-story-copy");
    var counter = el("span", "eyebrow");
    var heading = el("h3", "demo-story-title");
    var description = el("p");
    copy.append(
      counter,
      heading,
      description,
      el("span", "demo-scroll-hint", "Scroll to the next connection ↓"),
    );
    var device = el("div", "demo-device");
    var art = el("img", "demo-device-art");
    art.src = "assets/scenes/phone.webp";
    art.alt = "";
    art.width = 1024;
    art.height = 1536;
    art.loading = "lazy";
    var screen = el("div", "demo-device-screen");
    screen.id = "demo-phone-screen";
    screen.appendChild(el("div", "demo-contact", "Grok Bot"));
    var conversation = el("div", "demo-conversation");
    screen.appendChild(conversation);
    screen.appendChild(el("div", "demo-composer", "Message your agent…"));
    device.append(art, screen);
    composition.append(copy, device);
    stage.appendChild(composition);
    root.appendChild(stage);
    var viewport = el("div", "demo-viewport-probe");
    root.appendChild(viewport);
    var pinned = false;
    var selected = -1;
    var buttons = SCENES.map(function (scene, i) {
      var b = el("button", "demo-tab", scene.tab);
      b.type = "button";
      b.setAttribute("aria-controls", "demo-phone-screen " + panels[i].id);
      b.addEventListener("click", function () {
        if (pinned) {
          var top = root.getBoundingClientRect().top + window.scrollY - 72;
          var travel = root.offsetHeight - stage.offsetHeight;
          window.scrollTo({
            top: top + (travel * (i + 0.12)) / SCENES.length,
            behavior: "instant",
          });
        } else
          panels[i].scrollIntoView({ behavior: "instant", block: "start" });
        select(i);
      });
      tabs.appendChild(b);
      return b;
    });
    function select(i) {
      if (selected === i) return;
      buttons.forEach(function (button, j) {
        if (j === i) button.setAttribute("aria-current", "step");
        else button.removeAttribute("aria-current");
      });
      selected = i;
      var scene = SCENES[i];
      counter.textContent = "0" + (i + 1) + " / 04 — " + scene.tab;
      heading.textContent = headings[i];
      description.textContent = descriptions[i];
      conversation.textContent = "";
      conversation.appendChild(el("p", "phone-question", scene.steps[0].text));
      var route = el("div", "phone-route");
      route.appendChild(
        el("span", "phone-route-label", "CONNECTED THROUGH TINCAN"),
      );
      var teammates = scene.steps
        .filter(function (step) {
          return step.kind === "hop";
        })
        .map(function (step) {
          return step.to;
        });
      route.appendChild(el("b", "", "Grok Bot → " + teammates.join(" + ")));
      conversation.appendChild(route);
      conversation.appendChild(el("span", "phone-reply-label", "Grok Bot"));
      var last = scene.steps[scene.steps.length - 1];
      conversation.appendChild(el("p", "phone-answer", last.text));
      if (last.attach)
        conversation.appendChild(
          el("span", "phone-attachment", "↗ " + last.attach),
        );
    }
    var pending = false;
    function update() {
      pending = false;
      if (pinned) {
        var travel = Math.max(1, root.offsetHeight - stage.offsetHeight);
        var progress = Math.max(
          0,
          Math.min(0.9999, (72 - root.getBoundingClientRect().top) / travel),
        );
        select(Math.floor(progress * SCENES.length));
        device.style.transform =
          "translateY(" +
          (Math.sin(progress * Math.PI * 4) * -8).toFixed(2) +
          "px) rotate(" +
          (Math.sin(progress * Math.PI * 2) * 2).toFixed(2) +
          "deg)";
      } else {
        var active = 0;
        panels.forEach(function (panel, i) {
          if (panel.getBoundingClientRect().top <= window.innerHeight * 0.45)
            active = i;
        });
        select(active);
      }
    }
    function schedule() {
      if (!pending) {
        pending = true;
        requestAnimationFrame(update);
      }
    }
    function layout() {
      var rect = (pinned ? root : transcript).getBoundingClientRect();
      var inDemo = rect.top < 120 && rect.bottom > window.innerHeight / 2;
      var next =
        document.documentElement.classList.contains("motion-ready") &&
        (viewport.offsetHeight || window.innerHeight) >= 700;
      if (next !== pinned) {
        pinned = next;
        root.classList.toggle("demo-pinned", pinned);
        transcript.open = !pinned;
        if (!pinned) device.style.removeProperty("transform");
        if (inDemo) {
          var destination = pinned
            ? root.getBoundingClientRect().top +
              window.scrollY -
              72 +
              ((root.offsetHeight - stage.offsetHeight) * (selected + 0.12)) /
                SCENES.length
            : panels[selected].getBoundingClientRect().top +
              window.scrollY -
              140;
          window.scrollTo({ top: destination, behavior: "instant" });
        }
      }
      schedule();
    }
    window.addEventListener("scroll", schedule, { passive: true });
    window.addEventListener("resize", layout, { passive: true });
    window.addEventListener("pageshow", layout);
    document.addEventListener("tincan:motionchange", layout);
    transcript.addEventListener("toggle", function () {
      if (!pinned && !transcript.open) transcript.open = true;
    });
    select(0);
    layout();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();

// Copy buttons on the paste-this prompts.
(function () {
  function init() {
    var buttons = document.querySelectorAll("[data-copy]");
    Array.prototype.forEach.call(buttons, function (btn) {
      btn.addEventListener("click", function () {
        var src = document.getElementById(btn.getAttribute("data-copy"));
        if (!src) return;
        var text = src.textContent;
        var done = function () {
          btn.textContent = "Copied";
          setTimeout(function () {
            btn.textContent = "Copy";
          }, 1600);
        };
        var fallback = function () {
          var r = document.createRange();
          r.selectNodeContents(src);
          var sel = window.getSelection();
          sel.removeAllRanges();
          sel.addRange(r);
          btn.textContent = /Mac|iPhone|iPad/.test(navigator.platform)
            ? "Press ⌘C"
            : "Press Ctrl+C";
        };
        if (navigator.clipboard && navigator.clipboard.writeText) {
          navigator.clipboard.writeText(text).then(done, fallback);
        } else {
          fallback();
        }
      });
    });
  }
  if (document.readyState === "loading")
    document.addEventListener("DOMContentLoaded", init);
  else init();
})();
