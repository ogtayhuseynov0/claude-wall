// Claude Wall service worker — Web Push for the installed PWA.
self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (e) => e.waitUntil(self.clients.claim()));

self.addEventListener('push', (event) => {
  let d = {};
  try { d = event.data ? event.data.json() : {}; } catch (_) {}
  const title = d.title || 'Claude Wall';
  const opts = {
    body: d.body || '',
    tag: d.tag || 'claude-wall',
    renotify: true,
    icon: '/icon-192.png',
    badge: '/icon-192.png',
    data: { url: d.url || '/m.html' },
    vibrate: d.status === 'permission' ? [80, 40, 80] : [40],
  };
  event.waitUntil(self.registration.showNotification(title, opts));
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const url = (event.notification.data && event.notification.data.url) || '/m.html';
  event.waitUntil(
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((wins) => {
      for (const c of wins) {
        // reuse an open app window if we have one
        if ('focus' in c) { c.navigate ? c.navigate(url) : null; return c.focus(); }
      }
      return self.clients.openWindow(url);
    })
  );
});
