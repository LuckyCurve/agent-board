// 首屏主题：在 CSS 加载前就把 dark-theme 挂上，避免闪白。
// 独立成文件是为了让 CSP 的 script-src 收紧到 'self'，不再需要 unsafe-inline。
(function () {
  try {
    var q = new URLSearchParams(location.search).get('theme');
    var saved = q || localStorage.getItem('ab-theme');
    var dark = saved ? saved === 'dark' : matchMedia('(prefers-color-scheme: dark)').matches;
    if (q) localStorage.setItem('ab-theme', q);
    if (dark) document.documentElement.classList.add('dark-theme');
  } catch (e) {}
})();
