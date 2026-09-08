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
    requireInteraction: true,        // heads-up: stay on screen until acted on
    silent: false,
    icon: '/icon-192.png',
    badge: '/badge-96.png',
    data: { url: d.url || '/m.html', target: d.target || '' },
    vibrate: d.status === 'permission' ? [80, 40, 80, 40, 80] : [60, 40, 60],
    actions: d.target
      ? [{ action: 'open', title: 'Open' }, { action: 'mute', title: 'Mute 30m' }]
      : [{ action: 'open', title: 'Open' }],
  };
  // update the app-icon badge (works while the app is closed)
  try {
    if (self.navigator.setAppBadge && typeof d.attn === 'number') {
      d.attn > 0 ? self.navigator.setAppBadge(d.attn) : self.navigator.clearAppBadge();
    }
  } catch (_) {}
  event.waitUntil(self.registration.showNotification(title, opts));
});

self.addEventListener('notificationclick', (event) => {
  const data = event.notification.data || {};
  event.notification.close();

  // Mute action: tell the server to silence this session, don't open the app.
  if (event.action === 'mute' && data.target) {
    event.waitUntil(
      fetch('/api/push/mute', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ target: data.target, minutes: 30 }),
      }).then(() => self.registration.showNotification('🔕 Muted 30 min', {
        body: event.notification.title || '', tag: 'cw-mute', silent: true, requireInteraction: false,
      })).catch(() => {})
    );
    return;
  }

  const url = data.url || '/m.html';
  event.waitUntil(
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((wins) => {
      for (const c of wins) {
        if ('focus' in c) { if (c.navigate) c.navigate(url); return c.focus(); }
      }
      return self.clients.openWindow(url);
    })
  );
});
