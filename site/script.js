// Theme toggle. Loaded in <head> so a saved choice applies before first paint.
(function () {
  var root = document.documentElement;
  var KEY = 'tincan-theme';

  function saved() {
    try { return localStorage.getItem(KEY); } catch (e) { return null; }
  }
  function current() {
    var t = root.getAttribute('data-theme');
    if (t) return t;
    return window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  }
  function label(theme) {
    var btn = document.getElementById('theme-toggle');
    if (btn) {
      var text = theme === 'dark' ? 'Switch to light theme' : 'Switch to dark theme';
      btn.setAttribute('aria-label', text);
      btn.setAttribute('title', text);
    }
  }
  function apply(theme) {
    root.setAttribute('data-theme', theme);
    label(theme);
  }

  var s = saved();
  if (s === 'light' || s === 'dark') root.setAttribute('data-theme', s);

  document.addEventListener('DOMContentLoaded', function () {
    var btn = document.getElementById('theme-toggle');
    if (!btn) return;
    label(current());
    if (window.matchMedia) {
      var mq = window.matchMedia('(prefers-color-scheme: dark)');
      var onChange = function () { if (!root.getAttribute('data-theme')) label(current()); };
      if (mq.addEventListener) mq.addEventListener('change', onChange);
    }
    btn.addEventListener('click', function () {
      var next = current() === 'dark' ? 'light' : 'dark';
      apply(next);
      try { localStorage.setItem(KEY, next); } catch (e) {}
    });
  });
})();

// GitHub star count. Shows the count when the API answers; otherwise the
// badge stays as it is, with no count and no error text.
(function () {
  var API = 'https://api.github.com/repos/mvanhorn/agent-tincan';

  function formatCount(n) {
    if (n < 1000) return String(n);
    if (n < 1000000) {
      var k = Math.round(n / 100) / 10;
      if (k < 1000) return (k % 1 === 0 ? k.toFixed(0) : k.toFixed(1)) + 'k';
    }
    var m = Math.round(n / 100000) / 10;
    return (m % 1 === 0 ? m.toFixed(0) : m.toFixed(1)) + 'm';
  }

  function show(count) {
    var text = formatCount(count);
    var slots = document.querySelectorAll('[data-gh-stars]');
    for (var i = 0; i < slots.length; i++) {
      var num = slots[i].querySelector('[data-gh-count]');
      if (num) num.textContent = text;
      slots[i].hidden = false;
    }
  }

  function load() {
    if (!document.querySelector('[data-gh-stars]') || typeof fetch !== 'function') return;
    fetch(API, { credentials: 'omit' })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (data) {
        if (data && typeof data.stargazers_count === 'number' && data.stargazers_count >= 0) {
          show(data.stargazers_count);
        }
      })
      .catch(function () {});
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', load);
  } else {
    load();
  }
})();

// Hero demo: plays real use cases as a chat. You talk to Grok Bot; the
// dashed cards are agents asking each other through the relay. Pauses on
// hover or focus; with reduced motion each story shows whole, no autoplay.
(function () {
  var SCENES = [
    {
      tab: 'Your phone',
      steps: [
        { kind: 'you', text: 'What did ChatGPT tell me about the lease last night? Send me the screenshot.' },
        { kind: 'hop', from: 'Grok Bot', to: 'history', text: "Find last night's ChatGPT chat about the lease, with the screenshot." },
        { kind: 'back', from: 'history', to: 'Grok Bot', text: 'Found "Lease renewal questions" from 11:42 PM.', attach: 'lease-clause.png' },
        { kind: 'agent', who: 'Grok Bot', text: "ChatGPT said the early-termination fee is two months' rent. Here's the screenshot.", attach: 'lease-clause.png' }
      ]
    },
    {
      tab: 'Errands',
      steps: [
        { kind: 'you', text: 'Can you get me a table for two at Luigi’s tonight around 7?' },
        { kind: 'hop', from: 'Grok Bot', to: 'Muse', text: "Call Luigi's and book a table for two at 7pm tonight. Don't pay anything." },
        { kind: 'back', from: 'Muse', to: 'Grok Bot', text: "Called. 7:00 was full, booked 7:15 for two under your name. They hold it 15 minutes." },
        { kind: 'agent', who: 'Grok Bot', text: "You're booked at Luigi's at 7:15 for two. 7:00 was taken." }
      ]
    },
    {
      tab: 'Code',
      steps: [
        { kind: 'you', text: 'Did the automation PR merge? If CI failed, fix it.' },
        { kind: 'hop', from: 'Grok Bot', to: 'Codex', text: 'Check the automation PR. If CI is red, fix it and merge.' },
        { kind: 'back', from: 'Codex', to: 'Grok Bot', text: 'CI failed on lint. Pushed a fix, checks are green, merged #212.' },
        { kind: 'agent', who: 'Grok Bot', text: 'Merged. Codex fixed a lint failure first. PR #212 is in.' }
      ]
    },
    {
      tab: 'Second opinion',
      steps: [
        { kind: 'you', text: 'Ask ChatGPT and Claude which tagline is better, and show me both answers.' },
        { kind: 'hop', from: 'Grok Bot', to: 'chatgpt-web', text: 'Which tagline is better, A or B? One line of reasoning.' },
        { kind: 'hop', from: 'Grok Bot', to: 'claude-web', text: 'Which tagline is better, A or B? One line of reasoning.' },
        { kind: 'back', from: 'chatgpt-web', to: 'Grok Bot', text: 'B. It says what the product does in five words.' },
        { kind: 'back', from: 'claude-web', to: 'Grok Bot', text: 'B, narrowly. A is catchier but vaguer.' },
        { kind: 'agent', who: 'Grok Bot', text: 'Both pick B. ChatGPT: clearer. Claude: A is catchier but vaguer.' }
      ]
    }
  ];

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text) n.textContent = text;
    return n;
  }

  function bubble(step) {
    var li;
    if (step.kind === 'you' || step.kind === 'agent') {
      li = el('li', 'msg ' + (step.kind === 'you' ? 'msg-you' : 'msg-agent'));
      li.appendChild(el('span', 'who', step.kind === 'you' ? 'You' : step.who));
    } else {
      li = el('li', 'msg msg-hop' + (step.kind === 'back' ? ' msg-back' : ''));
      var route = el('span', 'hop-route');
      route.appendChild(el('b', '', step.from));
      var str = el('span', 'hop-string');
      str.setAttribute('aria-hidden', 'true');
      route.appendChild(str);
      route.appendChild(el('b', '', step.to));
      li.appendChild(route);
    }
    li.appendChild(el('p', '', step.text));
    if (step.attach) li.appendChild(el('span', 'attach', step.attach));
    return li;
  }

  function typing(hop) {
    var t = el('li', 'typing enter' + (hop ? ' typing-hop' : ''));
    t.setAttribute('aria-hidden', 'true');
    t.appendChild(el('i'));
    t.appendChild(el('i'));
    t.appendChild(el('i'));
    return t;
  }

  function init() {
    var feed = document.querySelector('[data-demo-feed]');
    var tabs = document.querySelector('.demo-tabs');
    var sceneLabel = document.querySelector('[data-demo-scene]');
    var root = document.querySelector('.hero-demo');
    if (!feed || !tabs || !root) return;
    var still = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;

    var current = 0, step = 0, timer = null, paused = false;
    var buttons = SCENES.map(function (sc, i) {
      var b = el('button', 'demo-tab', sc.tab);
      b.type = 'button';
      b.setAttribute('role', 'tab');
      b.addEventListener('click', function () { show(i); });
      tabs.appendChild(b);
      return b;
    });

    function clear() { if (timer) { clearTimeout(timer); timer = null; } }
    function later(fn, ms) { clear(); timer = setTimeout(function () { timer = null; if (paused) { later(fn, 400); } else { fn(); } }, ms); }

    function show(i) {
      clear();
      current = i;
      step = 0;
      buttons.forEach(function (b, j) { b.setAttribute('aria-selected', j === i ? 'true' : 'false'); });
      if (sceneLabel) sceneLabel.textContent = SCENES[i].tab;
      feed.textContent = '';
      if (still) {
        SCENES[i].steps.forEach(function (s) { feed.appendChild(bubble(s)); });
        return;
      }
      next();
    }

    function next() {
      var steps = SCENES[current].steps;
      if (step >= steps.length) {
        later(function () { show((current + 1) % SCENES.length); }, 3800);
        return;
      }
      var s = steps[step];
      var add = function () {
        var dots = feed.querySelector('.typing');
        if (dots) dots.remove();
        var b = bubble(s);
        b.classList.add('enter');
        feed.appendChild(b);
        step++;
        later(next, s.kind === 'you' ? 900 : 1300);
      };
      if (s.kind === 'you') {
        later(add, step === 0 ? 500 : 700);
      } else {
        feed.appendChild(typing(s.kind !== 'agent'));
        later(add, 1100);
      }
    }

    root.addEventListener('mouseenter', function () { paused = true; });
    root.addEventListener('mouseleave', function () { paused = false; });
    root.addEventListener('focusin', function () { paused = true; });
    root.addEventListener('focusout', function () { paused = false; });

    show(0);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
