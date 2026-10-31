/* Scroll is the timeline. No wheel interception, background loops, or remote code. */
(function () {
  "use strict";
  var root = document.documentElement;
  var preference = window.matchMedia("(prefers-reduced-motion: reduce)");
  var compact = window.matchMedia("(max-width: 899px), (max-height: 820px)");
  var button = document.getElementById("motion-toggle");
  var signal = document.getElementById("send-signal");
  var hero = document.querySelector(".hero-sculpture");
  var scenes = Array.from(document.querySelectorAll("[data-scene]"));
  var override = null;
  try {
    override = localStorage.getItem("tincan-motion");
  } catch (_) {}
  var frame = 0;
  var animations = [];
  var geometry = [];
  var enabled = false;
  var clamp = function (n) {
    return Math.max(0, Math.min(1, n));
  };
  var ease = function (n) {
    n = clamp(n);
    return n * n * (3 - 2 * n);
  };
  function value(node, name, n) {
    node.style.setProperty(name, n.toFixed(4));
  }

  function render() {
    frame = 0;
    if (!enabled || document.hidden) return;
    var y = window.scrollY;
    root.classList.toggle("has-scrolled", y > 40);
    if (hero && y < window.innerHeight)
      hero.style.transform = "translateY(" + (y * 0.14).toFixed(1) + "px)";
    if (compact.matches) return;
    geometry.forEach(function (g) {
      // Also resolve endpoints after an anchor jump or restored history position.
      var p = clamp((y - g.top) / g.travel);
      var node = g.node;
      if (node.dataset.scene === "request") {
        var phase = Math.min(3, Math.floor(p * 4));
        node.querySelectorAll(".scene-captions li").forEach(function (item, i) {
          item.classList.toggle("is-active", phase === i);
        });
        value(node, "--found", ease((p - 0.46) / 0.15));
        value(node, "--reply", ease((p - 0.73) / 0.15));
        var world = node.querySelector(".network-world");
        var close =
          p < 0.25
            ? 1 - ease(p / 0.25)
            : p > 0.72
              ? ease((p - 0.72) / 0.28)
              : Math.sin(((p - 0.25) / 0.47) * Math.PI) * 0.45;
        var direction = p > 0.25 && p < 0.72 ? -1 : 1;
        world.style.transform =
          "translateX(" +
          (direction * close * 12).toFixed(2) +
          "%) scale(" +
          (1 + close * 0.38).toFixed(4) +
          ")";
        var pulse = node.querySelector(".network-pulse");
        pulse.style.strokeDashoffset = String(
          -ease(p < 0.7 ? p / 0.7 : 1 - (p - 0.7) / 0.3) * 710,
        );
      } else if (node.dataset.scene === "council") {
        value(node, "--answers", ease(p / 0.23));
        value(node, "--blind", ease((p - 0.28) / 0.2));
        value(node, "--verdict", ease((p - 0.68) / 0.22));
        node.classList.toggle("is-blind", p >= 0.4);
      } else {
        value(node, "--return", ease((p - 0.08) / 0.8));
      }
    });
  }
  function schedule() {
    if (enabled && !document.hidden && !frame)
      frame = requestAnimationFrame(render);
  }
  function measure() {
    geometry = scenes.map(function (node) {
      return {
        node: node,
        top: node.getBoundingClientRect().top + window.scrollY,
        travel: Math.max(1, node.offsetHeight - window.innerHeight),
      };
    });
    schedule();
  }
  function cancelEffects() {
    animations.forEach(function (animation) {
      animation.cancel();
    });
    animations = [];
  }
  function apply(preservePosition) {
    var anchor = scenes.find(function (node) {
      var rect = node.getBoundingClientRect();
      return (
        rect.top < window.innerHeight / 2 &&
        rect.bottom > window.innerHeight / 2
      );
    });
    var readingAnchor = null;
    var readingTop = 0;
    if (preservePosition && !anchor) {
      var sections = Array.from(
        document.querySelectorAll("main section, .start-step, .platform"),
      );
      readingAnchor = sections.reverse().find(function (node) {
        var rect = node.getBoundingClientRect();
        return (
          rect.top < window.innerHeight / 2 &&
          rect.bottom > window.innerHeight / 2
        );
      });
      if (readingAnchor) readingTop = readingAnchor.getBoundingClientRect().top;
    }
    enabled = override === "on" || (override !== "off" && !preference.matches);
    cancelEffects();
    root.classList.toggle("motion-ready", enabled);
    root.classList.toggle("motion-off", !enabled);
    if (button) {
      button.hidden = false;
      button.textContent = enabled ? "Motion on" : "Motion off";
      button.setAttribute("aria-pressed", String(enabled));
      button.setAttribute(
        "aria-label",
        enabled ? "Turn animations off" : "Turn animations on",
      );
    }
    if (!enabled || compact.matches) {
      cancelAnimationFrame(frame);
      frame = 0;
      if (hero) hero.style.removeProperty("transform");
      scenes.forEach(function (node) {
        var world = node.querySelector(".network-world");
        if (world) world.style.removeProperty("transform");
        node.classList.remove("is-blind");
        node.querySelectorAll(".is-active").forEach(function (item) {
          item.classList.remove("is-active");
        });
      });
    }
    document.dispatchEvent(new Event("tincan:motionchange"));
    measure();
    if (preservePosition && anchor)
      window.scrollTo({
        top: anchor.getBoundingClientRect().top + window.scrollY - 72,
        behavior: "instant",
      });
    else if (readingAnchor && !readingAnchor.querySelector(".hero-demo"))
      window.scrollTo({
        top:
          window.scrollY +
          readingAnchor.getBoundingClientRect().top -
          readingTop,
        behavior: "instant",
      });
  }
  if (button)
    button.addEventListener("click", function () {
      override = enabled ? "off" : "on";
      try {
        localStorage.setItem("tincan-motion", override);
      } catch (_) {}
      apply(true);
    });
  if (signal)
    signal.addEventListener("click", function () {
      cancelEffects();
      signal.textContent = "Signal received ✓";
      window.setTimeout(function () {
        signal.textContent = "Send a signal ↗";
      }, 1600);
      if (!enabled || typeof Element.prototype.animate !== "function") return;
      var cable = document.querySelector(".cable-signal");
      animations.push(
        cable.animate(
          [
            { opacity: 0, strokeDasharray: "90 2000", strokeDashoffset: 0 },
            { opacity: 1, offset: 0.1 },
            { opacity: 1, offset: 0.85 },
            { opacity: 0, strokeDasharray: "90 2000", strokeDashoffset: -1600 },
          ],
          { duration: 1700, easing: "ease-in-out" },
        ),
      );
      document.querySelectorAll(".can-orbit img").forEach(function (can, i) {
        animations.push(
          can.animate(
            [
              { transform: "rotate(0deg)" },
              { transform: "rotate(-5deg)" },
              { transform: "rotate(3deg)" },
              { transform: "rotate(0deg)" },
            ],
            { duration: 650, delay: i * 800, easing: "ease-in-out" },
          ),
        );
      });
    });
  document.addEventListener("keydown", function (event) {
    if (event.key !== "Escape") return;
    var open =
      document.activeElement &&
      document.activeElement.closest(".platform details[open]");
    if (open) {
      open.open = false;
      open.querySelector("summary").focus();
    }
  });
  window.addEventListener("scroll", schedule, { passive: true });
  window.addEventListener("resize", measure, { passive: true });
  window.addEventListener("pageshow", measure);
  document.addEventListener("visibilitychange", function () {
    if (document.hidden) {
      cancelAnimationFrame(frame);
      frame = 0;
      cancelEffects();
    } else measure();
  });
  preference.addEventListener("change", function () {
    apply(true);
  });
  compact.addEventListener("change", function () {
    apply(true);
  });
  document.addEventListener("toggle", measure, true);
  if (typeof ResizeObserver === "function") {
    var content = document.querySelector("main");
    if (content) new ResizeObserver(measure).observe(content);
  }
  apply(false);
  if (document.fonts) document.fonts.ready.then(measure);
})();
