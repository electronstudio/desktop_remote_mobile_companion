// This file exists only to remove service workers installed by older
// versions of the app. The app no longer registers a service worker.
//
// Why: a service worker intercepted top-level navigation requests, which
// prevented iOS Safari from showing its "accept self-signed certificate"
// interstitial when the server's certificate changed (e.g. a regenerated
// cert after the app is run by a different user). With the old cert no
// longer trusted and the SW returning a null response, the phone was
// unable to load the page *or* reach the screen that would let it trust
// the new certificate.
//
// When the browser fetches this file to update an existing registration,
// it unregisters itself and clears any caches, then reloads clients so
// they are no longer controlled by a service worker.
self.addEventListener('install', event => {
  event.waitUntil(
    self.registration.unregister().then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', event => {
  event.waitUntil(
    caches.keys()
      .then(keys => Promise.all(keys.map(k => caches.delete(k))))
      .then(() => self.clients.claim())
      .then(() => self.clients.matchAll({ type: 'window' }))
      .then(clients => clients.forEach(c => {
        try { c.postMessage('reload'); } catch (e) { /* ignore */ }
      }))
  );
});

// Never intercept requests: let the browser handle them so that the
// normal certificate-acceptance interstitial can appear.
self.addEventListener('fetch', () => { /* pass-through */ });
