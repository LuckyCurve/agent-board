// 详情页焦点/选区守卫。
//
// 为什么要有这条：T-24 里点胶囊进详情后，标题被程序化聚焦，浏览器画了一圈焦点环，
// 用户看着就是「标题被自动选中了」。修法不是 user-select:none（那会连复制一起废掉），
// 而是指针打开时不碰焦点、只在键盘打开时聚焦标题；打开时顺手清掉双击留下的选区。
//
// 这条同时守住三件事：
//   1. 指针点胶囊打开 → 标题不聚焦、没有焦点环、没有选区；
//   2. 打开后自己拖选标题文字 → 真的能选中（复制还能用）；
//   3. 键盘回车打开 → 焦点仍在标题上（无障碍不能丢）。
//
// 用法：node web/e2e/detail-focus.mjs <agent-board 可执行文件>
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

const BIN = process.argv[2];
if (!BIN) {
  console.error('usage: node web/e2e/detail-focus.mjs <agent-board binary>');
  process.exit(2);
}
const PORT = 18871 + Math.floor(Math.random() * 200);
const BASE = `http://127.0.0.1:${PORT}`;
const DATA = fs.mkdtempSync(path.join(os.tmpdir(), 'ab-focus-'));
const TOKEN = 'e2e-focus-token';
const PASS = 'e2e-focus-pass';
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

async function api(method, p, body) {
  const r = await fetch(BASE + p, {
    method,
    headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + TOKEN },
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

// 1) 起服务 + 造一张卡和一条带胶囊的消息
server = spawn(BIN, ['serve', '--addr', `127.0.0.1:${PORT}`, '--data', DATA, '--password', PASS, '--token', TOKEN], { stdio: 'ignore' });
for (let i = 0; i < 100; i++) {
  try { if ((await fetch(BASE + '/login')).ok) break; } catch (_) {}
  await sleep(100);
}
if (!(await fetch(BASE + '/login').then(r => r.ok).catch(() => false))) fail('服务没起来');

const { task } = await api('POST', '/api/tasks', { title: '详情标题焦点守卫', column: 'doing' });
await api('POST', `/api/tasks/${task.id}/events`, { kind: 'progress', text: '一行可拖选的正文' });
await api('POST', '/api/message', { text: `看这个 #task=${task.id}`, from: 'user' });

// 2) 起 Chrome 登录
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
for (let i = 0; i < 200; i++) {
  try {
    if ((await fetch(`http://127.0.0.1:${DEBUG_PORT}/json/version`)).ok) break;
  } catch (_) {}
  if (chromeExited) throw new Error('Chrome 退出了: ' + chromeErr.slice(0, 300));
  await sleep(100);
}

const b = await connectCDP(DEBUG_PORT);
await b.send('Page.enable');
await b.send('Runtime.enable');
await b.send('Emulation.setDeviceMetricsOverride', { width: 1440, height: 900, deviceScaleFactor: 1, mobile: false });
await b.send('Page.navigate', { url: BASE + '/login?next=%2F' });
await sleep(600);
await b.evalJS(`(() => {
  const i = document.querySelector('#loginInput');
  i.value = ${JSON.stringify(PASS)};
  document.querySelector('#loginForm').requestSubmit();
  return true;
})()`);
let ready = false;
for (let i = 0; i < 100; i++) {
  await sleep(150);
  if (await b.evalJS(`document.querySelectorAll('.lnk-task').length > 0`)) { ready = true; break; }
}
if (!ready) fail('登录后没看到任务胶囊');

const probe = () => b.evalJS(`(() => {
  const el = document.querySelector('#d-title');
  const cs = el ? getComputedStyle(el) : null;
  const s = window.getSelection();
  const r = s && s.rangeCount ? s.getRangeAt(0) : null;
  const anc = r ? (r.commonAncestorContainer.nodeType === 1 ? r.commonAncestorContainer : r.commonAncestorContainer.parentElement) : null;
  return {
    open: document.querySelector('#detail').open,
    titleFocused: !!(el && document.activeElement === el),
    focusVisible: el ? el.matches(':focus-visible') : null,
    outlineStyle: cs ? cs.outlineStyle : null,
    outlineWidth: cs ? parseFloat(cs.outlineWidth) || 0 : 0,
    selLen: s ? s.toString().length : 0,
    selInTitle: !!(anc && anc.closest && anc.closest('#d-title')),
    userSelect: cs ? cs.getPropertyValue('user-select').trim() : null,
  };
})()`);

const capsule = await b.evalJS(`(() => {
  const el = document.querySelector('.lnk-task');
  const r = el.getBoundingClientRect();
  return { x: Math.round(r.left + r.width / 2), y: Math.round(r.top + r.height / 2) };
})()`);

// 3) 指针点胶囊
await b.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: capsule.x - 30, y: capsule.y - 20 });
await sleep(50);
await b.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: capsule.x, y: capsule.y, button: 'left', clickCount: 1 });
await b.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: capsule.x, y: capsule.y, button: 'left', clickCount: 1 });
await sleep(1200);
const clicked = await probe();
if (!clicked.open) fail('点胶囊没打开详情');
if (clicked.titleFocused) fail('指针打开时标题不该被聚焦（会画焦点环，看着像被选中）');
if (clicked.focusVisible || (clicked.outlineStyle !== 'none' && clicked.outlineWidth > 0)) {
  fail('指针打开后标题上有焦点环：' + JSON.stringify(clicked));
}
if (clicked.selLen !== 0) fail('指针打开后页面上还留着选区：' + JSON.stringify(clicked));
if (clicked.userSelect === 'none') fail('别用 user-select:none 一刀切，复制会一起废掉');

// 4) 打开后自己拖选标题，必须选得中
const tr = await b.evalJS(`(() => {
  const r = document.querySelector('#d-title').getBoundingClientRect();
  return { x1: Math.round(r.left + 4), x2: Math.round(r.left + Math.min(120, r.width - 4)), y: Math.round(r.top + r.height / 2) };
})()`);
await b.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: tr.x1, y: tr.y, button: 'left', clickCount: 1 });
await b.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: Math.round((tr.x1 + tr.x2) / 2), y: tr.y, button: 'left', buttons: 1 });
await b.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: tr.x2, y: tr.y, button: 'left', buttons: 1 });
await b.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: tr.x2, y: tr.y, button: 'left', clickCount: 1 });
await sleep(200);
const dragged = await probe();
if (dragged.selLen === 0 || !dragged.selInTitle) {
  fail('打开后手动拖选标题选不中，复制废了：' + JSON.stringify(dragged));
}
b.close();

// 5) 键盘回车打开：焦点仍要落到标题
const b2 = await connectCDP(DEBUG_PORT);
await b2.send('Page.enable');
await b2.send('Runtime.enable');
await b2.send('Emulation.setDeviceMetricsOverride', { width: 1440, height: 900, deviceScaleFactor: 1, mobile: false });
await b2.send('Page.navigate', { url: BASE + '/' });
for (let i = 0; i < 100; i++) {
  await sleep(150);
  if (await b2.evalJS(`document.querySelectorAll('.lnk-task').length > 0`)) break;
}
await b2.evalJS(`document.querySelector('.lnk-task').focus()`);
await sleep(100);
await b2.send('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Enter', code: 'Enter', text: '\r', windowsVirtualKeyCode: 13, nativeVirtualKeyCode: 13 });
await b2.send('Input.dispatchKeyEvent', { type: 'keyUp', key: 'Enter', code: 'Enter', windowsVirtualKeyCode: 13, nativeVirtualKeyCode: 13 });
await sleep(1200);
const kb = await b2.evalJS(`(() => {
  const el = document.querySelector('#d-title');
  return {
    open: document.querySelector('#detail').open,
    titleFocused: !!(el && document.activeElement === el),
    focusVisible: el ? el.matches(':focus-visible') : null,
  };
})()`);
b2.close();
if (!kb.open) fail('键盘回车没打开详情');
if (!kb.titleFocused) fail('键盘打开时焦点该落在标题上（无障碍路径不能丢）');
if (!kb.focusVisible) fail('键盘打开时该画焦点环，否则键盘用户看不出焦点在哪');

console.log(JSON.stringify({ clicked, dragged, keyboard: kb }, null, 2));
console.log('ok: 详情标题不再被自动选中，手动拖选与键盘导航都还在');
cleanup();
process.exit(0);
