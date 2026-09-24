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
