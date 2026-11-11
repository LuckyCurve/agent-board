const CACHE = 'agent-board-v5';
const SHELL = ['/app.css', '/app.js', '/linkify.js', '/login.js', '/icon.svg', '/manifest.webmanifest'];

self.addEventListener('install', e => {
  e.waitUntil(caches.open(CACHE).then(c => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener('activate', e => {
  e.waitUntil(
    caches.keys()
      .then(keys => Promise.all(keys.filter(k => k !== CACHE).map(k => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

self.addEventListener('push', e => {
  let data = {};
  try { data = e.data ? e.data.json() : {}; } catch (_) {}
  e.waitUntil(self.registration.showNotification(data.title || 'Agent Board', {
    body: data.body || '',
    icon: '/icon-192.png',
    badge: '/icon-192.png',
    tag: data.task_id || 'agent-board',
    renotify: true,
    data: { task_id: data.task_id || '' },
  }));
});

self.addEventListener('notificationclick', e => {
  e.notification.close();
  const id = (e.notification.data || {}).task_id || '';
  const url = id ? '/#task=' + encodeURIComponent(id) : '/';
  e.waitUntil(clients.matchAll({ type: 'window', includeUncontrolled: true }).then(list => {
    for (const c of list) {
      if ('focus' in c) {
        c.navigate(url).catch(() => {});
        return c.focus();
      }
    }
    return clients.openWindow(url);
  }));
});

self.addEventListener('fetch', e => {
  const url = new URL(e.request.url);
  if (e.request.method !== 'GET' || url.origin !== location.origin) return;
  // 页面受登录保护：不缓存 HTML，离线也不把看板内容留在共享浏览器里。
  if (e.request.mode === 'navigate' || url.pathname.startsWith('/api/')) return;

  e.respondWith(
    fetch(e.request)
      .then(res => {
        if (res.ok) {
          const copy = res.clone();
          caches.open(CACHE).then(c => c.put(e.request, copy));
        }
        return res;
      })
      .catch(() => caches.match(e.request))
  );
});
