// Tincan phone page service worker. It caches nothing: the page is only
// useful with live data from the relay, and a cached copy could outlive an
// upgrade or a device losing admin.
"use strict";

self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (event) => event.waitUntil(self.clients.claim()));
