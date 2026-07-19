const CACHE_NAME = 'desktop-remote-v2';
const STATIC_ASSETS = [
  '/manifest.json',
  '/icon-192.png',
  '/icon-512.png',
  '/apple-touch-icon.png'
];

function isNavigationRequest(request) {
  return request.mode === 'navigate' || new URL(request.url).pathname === '/';
}

function isAppScript(request) {
  return new URL(request.url).pathname === '/app.js';
}

self.addEventListener('install', event => {
  event.waitUntil(
    caches.open(CACHE_NAME)
      .then(cache => cache.addAll(STATIC_ASSETS))
      .then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', event => {
  event.waitUntil(
    caches.keys()
      .then(keys => Promise.all(
        keys.filter(key => key !== CACHE_NAME).map(key => caches.delete(key))
      ))
      .then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', event => {
  if (event.request.method !== 'GET') return;

  if (isNavigationRequest(event.request) || isAppScript(event.request)) {
    event.respondWith(
      fetch(event.request)
        .then(networkResponse => {
          if (networkResponse && networkResponse.status === 200) {
            const clone = networkResponse.clone();
            caches.open(CACHE_NAME).then(cache => cache.put(event.request, clone));
          }
          return networkResponse;
        })
        .catch(() => {
          return caches.match(event.request, { ignoreSearch: true }).then(response => response);
        })
    );
  } else {
    event.respondWith(
      caches.match(event.request).then(response => response || fetch(event.request))
    );
  }
});
