// 对话页 375 宽零横向溢出的端到端守卫。
//
// 为什么要有这条：T-20 的根因是 .main 在纵向 flex 里带着 margin:auto，
// 宽度退化成内容的 min-content，一条长消息就把右对齐气泡挤出屏幕。
// 只量 document.scrollWidth 不够——真正撑宽的是元素自己的 min-content，
// 所以这里同时检查每个元素的 right 边界和 .send-btn 的右沿。
//
// 用法：node web/e2e/chat-overflow.mjs <agent-board 可执行文件>
// Chrome 路径取 CHROME_PATH，否则按平台常见位置找；找不到就报错退出（CI 上必须有）。
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

const BIN = process.argv[2];
if (!BIN) {
  console.error('usage: node web/e2e/chat-overflow.mjs <agent-board binary>');
  process.exit(2);
}
const PORT = 18871 + Math.floor(Math.random() * 200);
const BASE = `http://127.0.0.1:${PORT}`;
const DATA = fs.mkdtempSync(path.join(os.tmpdir(), 'ab-e2e-'));
const TOKEN = 'e2e-token';
const PASS = 'e2e-pass';
const sleep = ms => new Promise(r => setTimeout(r, ms));

function chromePath() {
  const cands = [
    process.env.CHROME_PATH,
    'C:/Program Files/Google/Chrome/Application/chrome.exe',
    'C:/Program Files (x86)/Google/Chrome/Application/chrome.exe',
    '/usr/bin/google-chrome',
    '/usr/bin/google-chrome-stable',
    '/usr/bin/chromium',
    '/usr/bin/chromium-browser',
    '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  ].filter(Boolean);
  for (const c of cands) {
    try { if (fs.statSync(c).isFile()) return c; } catch (_) {}
  }
  throw new Error('找不到 Chrome，设 CHROME_PATH 指过去');
}

async function api(method, p, body, token) {
  const r = await fetch(BASE + p, {
    method,
    headers: Object.assign({ 'Content-Type': 'application/json' }, token ? { Authorization: 'Bearer ' + token } : {}),
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await r.text();
  if (!r.ok) throw new Error(`${method} ${p}: ${r.status} ${text}`);
  return text ? JSON.parse(text) : {};
}

async function connectCDP(port) {
  const target = await (await fetch(`http://127.0.0.1:${port}/json/new?about:blank`, { method: 'PUT' })).json();
  const ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
  let nextId = 0;
  const pending = new Map();
  ws.onmessage = ev => {
    const m = JSON.parse(ev.data);
    if (m.id && pending.has(m.id)) {
      const { res, rej } = pending.get(m.id);
      pending.delete(m.id);
      m.error ? rej(new Error(JSON.stringify(m.error))) : res(m.result);
    }
  };
  const send = (method, params = {}) => new Promise((res, rej) => {
    const id = ++nextId;
    const timer = setTimeout(() => { pending.delete(id); rej(new Error('CDP timeout: ' + method)); }, 20000);
    pending.set(id, { res: v => { clearTimeout(timer); res(v); }, rej: e => { clearTimeout(timer); rej(e); } });
    ws.send(JSON.stringify({ id, method, params }));
  });
  const evalJS = async expression => {
    const r = await send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
    if (r.exceptionDetails) throw new Error(JSON.stringify(r.exceptionDetails));
    return r.result.value;
  };
  return { send, evalJS, close: () => { try { ws.close(); } catch (_) {} } };
}

let server, chrome;
const cleanup = () => {
  try { server && server.kill(); } catch (_) {}
  try { chrome && chrome.kill(); } catch (_) {}
  try { fs.rmSync(DATA, { recursive: true, force: true }); } catch (_) {}
};
process.on('exit', cleanup);

const fail = msg => { console.error('FAIL: ' + msg); cleanup(); process.exit(1); };

// 1) 起服务
server = spawn(BIN, ['serve', '--addr', `127.0.0.1:${PORT}`, '--data', DATA, '--password', PASS, '--token', TOKEN], { stdio: 'ignore' });
for (let i = 0; i < 100; i++) {
  try { if ((await fetch(BASE + '/login')).ok) break; } catch (_) {}
  await sleep(100);
}
if (!(await fetch(BASE + '/login').then(r => r.ok).catch(() => false))) fail('服务没起来');

// 2) 造数据：长标题的卡 + 挂在卡上的消息（生成 nowrap 的引用条）+ 一条超长裸链接
const longTitle = 'T-20 对话页横向溢出排查：把右对齐气泡挤出屏幕的到底是哪个元素，必须量出来而不是猜';
const { task } = await api('POST', '/api/tasks', { title: longTitle, agent: 'e2e', column: 'doing' }, TOKEN);
await api('POST', '/api/message', { text: '这条挂在卡上，引用条里的标题是 nowrap', from: 'main', task_id: task.id }, TOKEN);
await api('POST', '/api/message', {
  text: '裸链接也要能换行 https://example.com/' + 'a'.repeat(160) + '?x=1', from: 'user',
}, TOKEN);

// 3) 起 Chrome，登录，375 真视口量
// 调试端口自己挑一个固定值再轮询：用 0 让 Chrome 自己选的话，得靠解析 stderr
// 那行 "DevTools listening on"，CI 上偶发读不到就直接超时了。
const DEBUG_PORT = 19000 + Math.floor(Math.random() * 900);
chrome = spawn(chromePath(), [
  '--headless=new', '--remote-debugging-port=' + DEBUG_PORT, '--user-data-dir=' + path.join(DATA, 'chrome'),
  '--no-first-run', '--no-default-browser-check', '--disable-gpu', '--hide-scrollbars',
  'about:blank',
], { stdio: ['ignore', 'ignore', 'pipe'] });
let chromeErr = '';
chrome.stderr.on('data', d => { chromeErr += d.toString(); });
let chromeExited = false;
chrome.on('exit', () => { chromeExited = true; });
const debugPort = await (async () => {
  for (let i = 0; i < 200; i++) {
    try {
      const r = await fetch(`http://127.0.0.1:${DEBUG_PORT}/json/version`);
      if (r.ok) return DEBUG_PORT;
    } catch (_) {}
    if (chromeExited) throw new Error('Chrome 退出了: ' + chromeErr.slice(0, 300));
    await sleep(100);
  }
  throw new Error('Chrome 调试端口没起来: ' + chromeErr.slice(0, 300));
})();

const b = await connectCDP(debugPort);
await b.send('Page.enable');
await b.send('Runtime.enable');
await b.send('Emulation.setDeviceMetricsOverride', { width: 375, height: 812, deviceScaleFactor: 1, mobile: true });
await b.send('Page.navigate', { url: BASE + '/login?next=%2F' });
await sleep(600);
await b.evalJS(`(() => {
  const i = document.querySelector('#loginInput');
  i.value = ${JSON.stringify(PASS)};
  document.querySelector('#loginForm').requestSubmit();
  return true;
})()`);
let ok = false;
for (let i = 0; i < 80; i++) {
  await sleep(150);
  if (await b.evalJS(`!!document.querySelector('.msg-item')`)) { ok = true; break; }
}
if (!ok) fail('登录后对话页没有消息');

const probe = await b.evalJS(`(() => {
  const W = document.documentElement.clientWidth;
  const bad = [];
  for (const e of document.querySelectorAll('body *')) {
    const r = e.getBoundingClientRect();
    const cs = getComputedStyle(e);
    if (cs.display === 'none' || cs.visibility === 'hidden') continue;
    if (r.width === 0 && r.height === 0) continue;
    if (r.right > W + 0.5 || r.left < -0.5) {
      bad.push({
        el: e.tagName.toLowerCase() + (e.id ? '#' + e.id : '') + (e.className ? '.' + String(e.className).split(' ')[0] : ''),
        left: Math.round(r.left), right: Math.round(r.right), width: Math.round(r.width),
      });
    }
  }
  const send = document.querySelector('.send-btn');
  const sr = send && send.getBoundingClientRect();
  return {
    docScrollWidth: document.documentElement.scrollWidth,
    clientWidth: W,
    sendRight: sr ? Math.round(sr.right) : null,
    bad: bad.slice(0, 12),
  };
})()`);
b.close();

console.log(JSON.stringify(probe, null, 2));
if (probe.docScrollWidth > probe.clientWidth) {
  fail(`对话页横向溢出：scrollWidth=${probe.docScrollWidth} > clientWidth=${probe.clientWidth}`);
}
if (probe.bad.length) {
  fail('这些元素越出 375 视口：' + JSON.stringify(probe.bad));
}
if (probe.sendRight === null || probe.sendRight > probe.clientWidth) {
  fail(`发送按钮不在视口内：right=${probe.sendRight}`);
}
console.log('ok: 对话页 375 宽零横向溢出');
cleanup();
process.exit(0);
