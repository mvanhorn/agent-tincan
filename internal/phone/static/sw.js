// Tincan phone page service worker. It caches nothing: the page is only
// useful with live data from the relay, and a cached copy could outlive an
// upgrade or a device losing admin. It shows the relay's held-ask alerts,
// which carry no request text, and opens the held list when one is tapped.
"use strict";

self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (event) => event.waitUntil(self.clients.claim()));

const HELD_URL = "./#held";

// Every push shows a notification, as browsers require of a page that
// asked for userVisibleOnly; a payload that cannot be read still shows
// the fixed text.
self.addEventListener("push", (event) => {
  let msg = {};
  try {
    msg = event.data ? event.data.json() : {};
  } catch (_) {
    msg = {};
  }
  const count = Number.isInteger(msg.count) && msg.count > 1 ? msg.count : 1;
  const body = count > 1 ? count + " requests are waiting for your approval" : "A request is waiting for your approval";
  event.waitUntil(self.registration.showNotification("Agent Tincan", {
    body,
    tag: "tincan-held",
    renotify: true,
    icon: "icons/icon-192.png",
    badge: "icons/icon-192.png",
    data: { url: HELD_URL },
  }));
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const target = new URL(HELD_URL, self.registration.scope).href;
  event.waitUntil((async () => {
    const wins = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
    for (const w of wins) {
      if (w.url.startsWith(self.registration.scope)) {
        await w.focus();
        return w.navigate(target);
      }
    }
    return self.clients.openWindow(target);
  })());
});
