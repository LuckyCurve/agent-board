function safeNext(next) {
  if (!next || next[0] !== '/' || next.slice(0, 2) === '//' || next.includes('\\')) return '/';
  return next;
}

const form = document.getElementById('loginForm');
const input = document.getElementById('loginInput');
const err = document.getElementById('loginError');

form.addEventListener('submit', async e => {
  e.preventDefault();
  err.textContent = '';
  try {
    const res = await fetch('/api/login', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password: input.value }),
    });
    if (res.status === 429) {
      err.textContent = '尝试过于频繁，稍后再试';
      return;
    }
    if (!res.ok) {
      err.textContent = '口令不对';
      input.select();
      return;
    }
    const next = new URLSearchParams(location.search).get('next');
    location.replace(safeNext(next));
  } catch (_) {
    err.textContent = '连接失败，检查服务是否在运行';
  }
});
