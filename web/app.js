/* Agent Board front-end — vanilla JS, no framework. */

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

const COLUMNS = [
  { key: 'todo', label: '待办' },
  { key: 'doing', label: '进行中' },
  { key: 'blocked', label: '阻塞' },
  { key: 'done', label: '已完成' },
];

const state = {
  tasks: [],
  decisions: [],
  chat: [],
  activity: [],
  view: 'inbox',
  loaded: false,
};

// ?static=1 关闭 SSE 与 service worker，供无头截图与弱网排查使用。
const STATIC = new URLSearchParams(location.search).has('static');

let openTaskId = null;
let detailFromSeq = null;  // 从对话点进来的消息 seq，用于返回上下文
let replyTo = null;        // seq of the event being replied to
let draftText = '';
let freeDraft = '';
let refreshTimer = null;
let es = null;
let chatPinned = true;     // 消息流是否贴着底部；用户往上翻时关闭
let pendingOpen = false;   // 「等你决定」列表是否展开
let flashSeq = 0;          // 刚从待拍板跳进来的消息，渲染后闪一下
let systemNote = '';       // /状态 等本地提示，不落库

function chatNearBottom(el) {
  return el.scrollHeight - el.scrollTop - el.clientHeight <= 60;
}

function scrollChatBottom() {
  const sc = $('#inboxScroll');
  if (sc) sc.scrollTop = sc.scrollHeight;
}

/* ---------- utils ---------- */

function esc(v) {
  return String(v == null ? '' : v).replace(/[&<>"']/g, c => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[c]));
}

function uid() {
  if (window.crypto && crypto.randomUUID) return crypto.randomUUID();
  return 'k' + Date.now().toString(36) + Math.random().toString(36).slice(2);
}

function trunc(s, n) {
  s = String(s || '').replace(/\s+/g, ' ').trim();
  return s.length > n ? s.slice(0, n) + '…' : s;
}

const pad = n => String(n).padStart(2, '0');
const ymd = d => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;

function rel(iso) {
  const t = new Date(iso);
  if (isNaN(t)) return '';
  const d = Date.now() - t.getTime();
  if (d < 45000) return '刚刚';
  if (d < 3600000) return Math.max(1, Math.floor(d / 60000)) + ' 分钟前';
  if (d < 86400000) return Math.floor(d / 3600000) + ' 小时前';
  if (d < 172800000) return '昨天';
  if (d < 604800000) return Math.floor(d / 86400000) + ' 天前';
  return `${t.getMonth() + 1}月${t.getDate()}日`;
}

function hm(iso) {
  const t = new Date(iso);
  return isNaN(t) ? '' : `${pad(t.getHours())}:${pad(t.getMinutes())}`;
}

function dayLabel(iso) {
  const t = new Date(iso);
  if (isNaN(t)) return '';
  const now = new Date();
  const a = ymd(t), b = ymd(now);
  const y = new Date(now); y.setDate(now.getDate() - 1);
  if (a === b) return '今天';
  if (a === ymd(y)) return '昨天';
  return `${t.getMonth() + 1}月${t.getDate()}日`;
}

function toast(msg, kind = '') {
  let el = $('#toast');
  if (!el) {
    el = document.createElement('div');
    el.id = 'toast';
    document.body.appendChild(el);
  }
  el.className = 'toast show ' + kind;
  el.textContent = msg;
  clearTimeout(toast._t);
  toast._t = setTimeout(() => { el.className = 'toast'; }, 2200);
}

/* ---------- api ---------- */

const API_TOKEN = (document.querySelector('meta[name="api-token"]') || {}).content || '';

function authHeaders(extra) {
  return Object.assign(API_TOKEN ? { Authorization: 'Bearer ' + API_TOKEN } : {}, extra || {});
}

async function api(path, { method = 'GET', body } = {}) {
  const res = await fetch(path, {
    method,
    credentials: 'same-origin',
    headers: authHeaders(body ? { 'Content-Type': 'application/json' } : {}),
    body: body ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401) {
    const next = location.pathname + location.hash;
    location.replace('/login?next=' + encodeURIComponent(next));
    throw new Error('unauthorized');
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || ('HTTP ' + res.status));
  return data;
}

/* ---------- status glyphs ---------- */

function checkIcon(cls = '') {
  return `<svg class="g ${cls}" viewBox="0 0 16 16" aria-hidden="true"><path d="M3.2 8.6l3 3 6.6-6.8"/></svg>`;
}

function statusGlyph(status) {
  if (status === 'processing') return '<span class="pulse" aria-hidden="true"></span>';
  if (status === 'received') return checkIcon('pending');
  if (status === 'done') return checkIcon('ok');
  return '<span class="bang" aria-hidden="true">!</span>';
}

function statusLabel(r) {
  switch (r.status) {
    case 'received': return '已接到';
    case 'processing': return r.agent ? r.agent + ' 执行中' : '执行中';
    case 'done': return '已完成';
    case 'failed': return '未处理';
    case 'timeout': return '超时';
    default: return r.status || '';
  }
}

function statusBadge(r) {
  if (!r) return '';
  const title = [statusLabel(r), r.note].filter(Boolean).join(' · ');
  return `<span class="rstatus ${esc(r.status)}" title="${esc(title)}">${statusGlyph(r.status)}<span class="rs-label">${esc(statusLabel(r))}</span></span>`;
}

/* ---------- data ---------- */

async function refreshTasks() {
  const data = await api('/api/tasks');
  state.tasks = data.tasks || [];
  state.loaded = true;
  if (state.view === 'board') renderBoard();
}

async function refreshDecisions() {
  const data = await api('/api/decisions?status=pending');
  state.decisions = data.decisions || [];
  updateBadge();
  if (state.view === 'inbox') renderInbox();
}

async function refreshChat() {
  const data = await api('/api/chat?recent=80');
  state.chat = data.messages || [];
  if (state.view === 'inbox') renderInbox();
}

async function refreshActivity() {
  const data = await api('/api/activity?days=84');
  state.activity = data.days || [];
  if (state.view === 'calendar') renderCalendar();
}

function updateBadge() {
  const n = state.decisions.length;
  const el = $('#navBadge');
  if (!el) return;
  el.hidden = n === 0;
  el.textContent = n > 9 ? '9+' : String(n);
  const c = $('#inboxCount');
  if (c) c.textContent = String(n);
}

function taskById(id) {
  return state.tasks.find(t => t.id === id) || null;
}

/* ---------- board ---------- */

function lastTextOf(e) {
  if (!e) return '';
  if (e.type === 'task.moved') return '移到' + colLabel(e.body);
  return e.body || '';
}

function cardHTML(t) {
  const last = t.last_event;
  const lastText = trunc(lastTextOf(last), 96);
  const r = t.reply;
  const alert = taskAlert(t);
  const strip = r && (r.status === 'received' || r.status === 'processing')
    ? `<div class="reply-strip ${esc(r.status)}">${statusBadge(r)}</div>` : '';
  const alertHTML = alert && alert.kind === 'failed'
    ? `<div class="card-alert"><b>${esc(alert.label)}</b><span>${esc(trunc(alert.reason, 64))}</span></div>` : '';
  return `<article class="card" data-id="${esc(t.id)}" tabindex="0" role="button" aria-label="${esc(t.title)}">
    <div class="card-top">
      <span class="status-dot ${esc(t.column)}"></span>
      <span class="task-id">${esc(t.id)}</span>
      <time class="card-time">${rel(t.updated_at)}</time>
    </div>
    <h3 class="card-title">${esc(t.title)}</h3>
    ${lastText ? `<p class="card-last">${esc(lastText)}</p>` : ''}
    ${alertHTML}
    ${strip}
    <div class="card-meta">
      ${t.project ? `<span class="chip">${esc(t.project)}</span>` : ''}
      ${t.agent ? `<span class="chip ghost">${esc(t.agent)}</span>` : ''}
      ${t.pending_decisions > 0 ? `<span class="badge warn">待拍板 ${t.pending_decisions}</span>` : ''}
    </div>
  </article>`;
}

function renderBoard() {
  const board = $('#board');
  if (!board) return;
  board.innerHTML = COLUMNS.map(col => {
    const items = state.tasks.filter(t => t.column === col.key);
    return `<section class="column" data-col="${col.key}">
      <header class="col-head">
        <span class="col-dot ${col.key}"></span>
        <h2>${col.label}</h2>
        <span class="col-count">${items.length}</span>
      </header>
      <div class="col-body">
        ${items.length ? items.map(cardHTML).join('') : '<div class="col-empty">暂无任务</div>'}
      </div>
    </section>`;
  }).join('');
}

function skeletonBoard() {
  const board = $('#board');
  board.innerHTML = COLUMNS.map(col => `<section class="column">
    <header class="col-head"><span class="col-dot ${col.key}"></span><h2>${col.label}</h2></header>
    <div class="col-body"><div class="skeleton"></div><div class="skeleton"></div></div>
  </section>`).join('');
}

/* ---------- detail ---------- */

function eventKind(e) {
  if (e.type === 'task.created') return { label: '创建', cls: 'created' };
  if (e.type === 'task.moved') return { label: '移到' + colLabel(e.body), cls: 'moved' };
  if (e.type === 'decision.created') return { label: '待拍板', cls: 'decision' };
  if (e.type === 'decision.answered') return { label: '已拍板', cls: 'decision' };
  const k = e.kind || 'note';
  const map = {
    progress: ['进展', 'progress'],
    note: ['备注', 'note'],
    tool: ['工具', 'tool'],
    artifact: ['产物', 'artifact'],
    conclusion: ['结论', 'conclusion'],
    blocked: ['阻塞', 'blocked'],
    dispatch: ['派发', 'dispatch'],
    user_reply: ['你的回复', 'reply'],
    user_reaction: ['回应', 'reply'],
  };
  const m = map[k] || [k, 'note'];
  return { label: m[0], cls: m[1] };
}

function colLabel(key) {
  const c = COLUMNS.find(x => x.key === key);
  return c ? c.label : (key || '');
}

function quoteHTML(e, bySeq) {
  if (e.kind !== 'user_reply' && e.kind !== 'user_reaction') return '';
  let to = null;
  try { to = JSON.parse(e.data || '{}').reply_to_seq; } catch (_) {}
  if (!to) return '';
  const orig = bySeq.get(to);
  return `<div class="tl-quote">↳ #${to}${orig ? ' · ' + esc(trunc(orig.body, 56)) : ''}</div>`;
}

function timelineHTML(events, replies) {
  const bySeq = new Map(events.map(e => [e.seq, e]));
  const repBySeq = new Map(replies.map(r => [r.seq, r]));
  let out = '';
  let lastDay = '';
  for (const e of events) {
    const day = dayLabel(e.created_at);
    if (day !== lastDay) {
      out += `<li class="tl-day">${esc(day)}</li>`;
      lastDay = day;
    }
    const k = eventKind(e);
    const r = repBySeq.get(e.seq);
    out += `<li class="tl-item ${esc(k.cls)}" data-seq="${e.seq}">
      <span class="tl-dot ${k.cls}"></span>
      <div class="tl-main">
        <div class="tl-head">
          <span class="tl-kind">${esc(k.label)}</span>
          <span class="tl-right">
            <time>${hm(e.created_at)}</time>
            <button class="tl-reply" data-reply="${e.seq}" aria-label="回复这条" title="回复这条">
              <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M10 9V5l-7 7 7 7v-4c5 0 8 1.6 10 5-1-5-4-9-10-11z"/></svg>
            </button>
          </span>
        </div>
        <p class="tl-body">${esc(e.body)}</p>
        ${quoteHTML(e, bySeq)}
        ${r ? `<div class="tl-status">${statusBadge(r)}</div>` : ''}
        ${e.agent && !r ? `<span class="tl-agent">${(e.kind === 'user_reply' || e.kind === 'user_reaction') ? '→ ' : ''}${esc(e.agent)}</span>` : ''}
      </div>
    </li>`;
  }
  return out;
}

function pendingHTML(decisions) {
  const pend = decisions.filter(d => d.status === 'pending');
  if (!pend.length) return '';
  return pend.map(d => {
    let opts = [];
    try { opts = JSON.parse(d.options || '[]'); } catch (_) {}
    return `<div class="pending-box" data-decision="${esc(d.id)}">
      <div class="pb-label">待你拍板</div>
      <div class="pb-q">${esc(d.question)}</div>
      <div class="pb-opts">
        ${opts.map(o => `<button class="opt tap" data-answer="${esc(d.id)}" data-value="${esc(o)}">${esc(o)}</button>`).join('')}
      </div>
      <form class="pb-free" data-decision="${esc(d.id)}">
        <input type="text" placeholder="或直接打字回答" aria-label="直接回答">
        <button class="mini-send" type="submit" aria-label="发送">
          <svg viewBox="0 0 24 24"><path d="M12 19V5M6 11l6-6 6 6"/></svg>
        </button>
      </form>
    </div>`;
  }).join('');
}

async function openTask(id, fromSeq) {
  openTaskId = id;
  detailFromSeq = fromSeq || null;
  replyTo = null;
  draftText = '';
  const dlg = $('#detail');
  if (!dlg.open) dlg.showModal();
  await refreshDetail(id, false);
  // 默认从标题看起；#task=T-1&bottom=1 直接落到底部的回复状态（深链/截图用）。
  const body = $('#sheetBody');
  if (body) body.scrollTop = location.hash.includes('bottom=1') ? body.scrollHeight : 0;
}

function backToChat(seq) {
  closeSheet();
  setView('inbox');
  setTimeout(() => {
    const item = document.querySelector(`.msg-item[data-seq="${seq}"]`);
    if (!item) return;
    chatPinned = false;
    item.scrollIntoView({ block: 'center' });
    item.classList.add('flash');
    setTimeout(() => item.classList.remove('flash'), 1600);
  }, 350);
}

async function refreshDetail(id, keepScroll) {
  if (!openTaskId) return;
  let data;
  try {
    data = await api('/api/tasks/' + encodeURIComponent(id));
  } catch (e) {
    if (String(e.message).includes('not found')) { closeSheet(); }
    return;
  }
  const { task, events, decisions, replies } = data;
  const body = $('#sheetBody');
  const atBottom = !keepScroll || (body.scrollHeight - body.scrollTop - body.clientHeight < 48);

  captureDraft();

  $('#d-id').textContent = task.id;
  $('#d-status').textContent = colLabel(task.column);
  $('#d-status').className = 'status-pill col-' + task.column;
  $('#d-title').textContent = task.title;
  $('#d-meta').innerHTML = [
    task.project ? `<span class="chip">${esc(task.project)}</span>` : '',
    task.agent ? `<span class="chip ghost">${esc(task.agent)}</span>` : '',
    `<span class="chip ghost">更新于 ${rel(task.updated_at)}</span>`,
  ].join('');
  const alert = taskAlert(task);
  let actions = '';
  if (alert && alert.kind === 'failed') {
    actions += `<div class="detail-alert"><b>${esc(alert.label)}</b><span>${esc(trunc(alert.reason, 120))}</span></div>`;
  }
  if (detailFromSeq) {
    actions += `<button class="btn ghost d-back-chat" id="dBackChat" data-seq="${detailFromSeq}">← 回到对话 #${detailFromSeq}</button>`;
  }
  $('#d-actions').innerHTML = actions + pendingHTML(decisions || []);
  $('#d-timeline').innerHTML = timelineHTML(events || [], replies || []);

  restoreDraft();
  updateComposeCtx();

  if (!keepScroll || atBottom) body.scrollTop = body.scrollHeight;
}

function closeSheet() {
  const dlg = $('#detail');
  if (dlg.open) dlg.close();
  openTaskId = null;
  detailFromSeq = null;
  replyTo = null;
  draftText = '';
  if (location.hash.includes('task=')) history.replaceState(null, '', location.pathname);
}

function captureDraft() {
  const el = $('#composeInput');
  if (el) draftText = el.value;
}

function restoreDraft() {
  const el = $('#composeInput');
  if (el && draftText) {
    el.value = draftText;
    autogrow(el);
  }
}

function updateComposeCtx() {
  const ctx = $('#composeCtx');
  const label = $('#composeCtxLabel');
  if (!ctx) return;
  if (replyTo) {
    ctx.hidden = false;
    label.textContent = '回复 #' + replyTo;
  } else {
    ctx.hidden = true;
  }
}

/* ---------- composing ---------- */

function autogrow(el) {
  el.style.height = 'auto';
  el.style.height = Math.min(el.scrollHeight, 140) + 'px';
}

async function sendReply() {
  const input = $('#composeInput');
  const text = (input.value || '').trim();
  if (!text || !openTaskId) return;
  const btn = $('#composeSend');
  btn.disabled = true;
  btn.classList.add('sending');
  const data = replyTo ? JSON.stringify({ reply_to_seq: replyTo }) : '{}';
  try {
    await api('/api/tasks/' + encodeURIComponent(openTaskId) + '/events', {
      method: 'POST',
      body: { kind: 'user_reply', text, agent: '', data, key: uid() },
    });
    input.value = '';
    draftText = '';
    autogrow(input);
    replyTo = null;
    updateComposeCtx();
    toast('已收到，交给主会话');
    await Promise.all([refreshTasks(), refreshDecisions(), refreshDetail(openTaskId, false)]);
    if (state.view === 'board') renderBoard();
  } catch (e) {
    if (e.message !== 'unauthorized') toast('发送失败：' + e.message, 'bad');
  } finally {
    btn.disabled = false;
    btn.classList.remove('sending');
    // 刷新可能换掉输入框，重新取一次再聚焦。
    const live = $('#composeInput');
    if (live) live.focus();
    else input.focus();
  }
}

async function answerDecision(id, value) {
  if (!value) return;
  try {
    await api('/api/decisions/' + encodeURIComponent(id) + '/answer', {
      method: 'POST',
      body: { answer: value, key: uid() },
    });
    toast('已回复，交给 ' + (openTaskId ? '负责 Agent' : '主会话'));
    await Promise.all([refreshTasks(), refreshDecisions()]);
    if (state.view === 'inbox') renderInbox();
    if (openTaskId) await refreshDetail(openTaskId, true);
  } catch (e) {
    if (e.message !== 'unauthorized') toast('回复失败：' + e.message, 'bad');
  }
}

function clearFreeInput() {
  freeDraft = '';
  chatPinned = true;
  const el = $('#freeInput');
  if (el) { el.value = ''; autogrow(el); el.focus(); }
}

function statusSummary() {
  const doing = state.tasks.filter(t => t.column === 'doing').length;
  const blocked = state.tasks.filter(t => t.column === 'blocked').length;
  const done = state.tasks.filter(t => t.column === 'done').length;
  const failed = state.tasks.filter(t => {
    const a = taskAlert(t);
    return a && a.kind === 'failed';
  }).length;
  return `进行中 ${doing} · 待拍板 ${state.decisions.length} · 阻塞/失败 ${failed || blocked} · 已完成 ${done}`;
}

async function sendFreeMessage(text) {
  text = (text || '').trim();
  if (!text) return;
  if (text[0] === '/') {
    await runCommand(text);
    return;
  }
  try {
    await api('/api/message', { method: 'POST', body: { text, key: uid() } });
    // 先清掉草稿和当前框里的字，刷新后 renderInbox 才不会把旧内容填回去。
    systemNote = '';
    freeDraft = '';
    chatPinned = true;
    const sent = $('#freeInput');
    if (sent) { sent.value = ''; autogrow(sent); }
    toast('已发出，主会话已接到');
    await Promise.all([refreshChat(), refreshDecisions()]);
    const after = $('#freeInput');
    if (after) { after.value = ''; autogrow(after); after.focus(); }
  } catch (e) {
    const fi = $('#freeInput');
    if (fi) fi.focus();
    if (e.message !== 'unauthorized') toast('发送失败：' + e.message, 'bad');
  }
}

// 输入框指令只做三件事：看状态、建卡派人、挂待拍板。
async function runCommand(text) {
  const m = /^\/(状态|派|拍板)\s*([\s\S]*)$/.exec(text);
  if (!m) {
    toast('未知指令。可用：/状态、/派 X …、/拍板 T-1 …', 'bad');
    return;
  }
  const cmd = m[1];
  const rest = m[2].trim();
  try {
    if (cmd === '状态') {
      systemNote = statusSummary();
      renderInbox();
      requestAnimationFrame(scrollChatBottom);
      clearFreeInput();
      return;
    }

    if (cmd === '派') {
      const p = /^(\S+)\s+([\s\S]+)$/.exec(rest);
      if (!p) { toast('用法：/派 Agent名 任务标题', 'bad'); return; }
      const agent = p[1];
      const title = p[2].trim();
      const res = await api('/api/tasks', {
        method: 'POST',
        body: { title, agent, column: 'todo' },
      });
      const id = res.task.id;
      await api('/api/message', {
        method: 'POST',
        body: { text: `派给 ${agent}：${title}`, task_id: id, key: uid() },
      });
      toast(`已建卡 ${id}，派给 ${agent}`);
      systemNote = '';
      await refreshTasks();
      await refreshChat();
      clearFreeInput();
      return;
    }

    // /拍板 T-1 问题；只有一张进行中/阻塞卡时可以省略卡号。
    let taskID = '';
    let question = rest;
    const t = /^(T-\d+)\s+([\s\S]+)$/i.exec(rest);
    if (t) {
      taskID = t[1].toUpperCase();
      question = t[2].trim();
    } else {
      const open = state.tasks.filter(x => x.column === 'doing' || x.column === 'blocked');
      if (open.length === 1 && rest) taskID = open[0].id;
    }
    if (!taskID || !question) {
      toast('用法：/拍板 T-1 要决定什么', 'bad');
      return;
    }
    await api('/api/decisions', {
      method: 'POST',
      body: { task_id: taskID, question, options: ['同意', '不同意'], key: uid() },
    });
    await api('/api/message', {
      method: 'POST',
      body: { text: `待拍板：${question}`, task_id: taskID, key: uid() },
    });
    toast(`已挂到 ${taskID}，等你拍板`);
    systemNote = '';
    await Promise.all([refreshTasks(), refreshDecisions()]);
    await refreshChat();
    clearFreeInput();
  } catch (e) {
    if (e.message !== 'unauthorized') toast(`指令失败：${e.message}`, 'bad');
    const fi = $('#freeInput');
    if (fi) fi.focus();
  }
}

/* ---------- inbox ---------- */

function parseOpts(s) {
  try { const a = JSON.parse(s || '[]'); return Array.isArray(a) ? a : []; } catch (_) { return []; }
}

function decisionHTML(d) {
  const t = taskById(d.task_id);
  const opts = parseOpts(d.options);
  return `<article class="decision-card">
    <div class="dc-top">
      <span class="badge warn">待拍板</span>
      <button class="dc-task" data-task="${esc(d.task_id)}">
        <b>${esc(d.task_id)}</b>${t ? ' · ' + esc(t.title) : ''}
      </button>
    </div>
    <p class="dc-q">${esc(d.question)}</p>
    <div class="dc-options">
      ${opts.map(o => `<button class="opt tap" data-answer="${esc(d.id)}" data-value="${esc(o)}">${esc(o)}</button>`).join('')}
    </div>
    <form class="dc-free" data-decision="${esc(d.id)}">
      <input type="text" placeholder="或直接打字回复" aria-label="直接回答">
      <button class="mini-send" type="submit" aria-label="发送">
        <svg viewBox="0 0 24 24"><path d="M12 19V5M6 11l6-6 6 6"/></svg>
      </button>
    </form>
  </article>`;
}

function parseData(s) {
  try { return JSON.parse(s || '{}') || {}; } catch (_) { return {}; }
}

// 一张卡目前最需要被看见的状态：等人拍板 > 阻塞 > 失败/超时 > 已交付 > 进行中。
function taskAlert(t) {
  if (!t) return null;
  if (t.pending_decisions > 0) {
    const d = state.decisions.find(x => x.task_id === t.id && x.status === 'pending');
    return { kind: 'decision', label: '等你拍板', reason: d ? d.question : '' };
  }
  if (t.column === 'blocked') {
    const e = t.last_event;
    return { kind: 'failed', label: '阻塞', reason: e && e.kind === 'blocked' ? e.body : '任务已阻塞' };
  }
  const r = t.reply;
  if (r && (r.status === 'failed' || r.status === 'timeout')) {
    return { kind: 'failed', label: r.status === 'timeout' ? '超时' : '失败', reason: r.note || 'Agent 没有收尾' };
  }
  if (t.column === 'done') return { kind: 'done', label: '已交付', reason: '' };
  return { kind: 'doing', label: '进行中', reason: '' };
}

function messageHTML(m) {
  const agent = m.author && m.author !== 'user';
  const who = agent ? m.author : '你';
  const t = m.link_task ? taskById(m.link_task) : null;
  const sig = t ? taskAlert(t) : null;
  const chip = sig
    ? `<button class="msg-task ${sig.kind}" data-task="${esc(t.id)}" data-msg="${m.seq}" title="打开 ${esc(t.id)}">
         <span class="mt-id">${esc(t.id)}</span><span class="mt-label">${esc(sig.label)}</span>
       </button>`
    : '';
  const alert = sig && sig.kind === 'failed' && sig.reason
    ? `<div class="msg-alert"><b>${esc(sig.label)}</b><span>${esc(trunc(sig.reason, 90))}</span></div>`
    : '';
  return `<li class="msg-item${agent ? ' agent' : ''}${sig && sig.kind === 'failed' ? ' danger' : ''}" data-seq="${m.seq}">
    <div class="msg-who">${esc(who)}</div>
    <div class="msg-text">${esc(m.body)}</div>
    ${alert}
    <div class="msg-foot">${chip}<time>${rel(m.created_at)}</time></div>
  </li>`;
}

// 待拍板常驻条：在消息区外面，滚动不会把它带走；没事时整条隐藏。
function renderPendingBar() {
  const bar = $('#pendingBar');
  if (!bar) return;
  const pending = state.decisions;
  bar.hidden = pending.length === 0;
  if (!pending.length) {
    pendingOpen = false;
  }
  const count = $('#pendingCount');
  if (count) count.textContent = String(pending.length);
  const toggle = $('#pendingToggle');
  if (toggle) toggle.setAttribute('aria-expanded', pendingOpen ? 'true' : 'false');
  const list = $('#pendingList');
  if (list) {
    list.hidden = !pendingOpen;
    list.innerHTML = pending.map(d => {
      const t = taskById(d.task_id);
      const msg = state.chat.find(m => m.link_task === d.task_id);
      const source = t ? `${d.task_id} · ${t.title}` : (d.task_id || '主会话');
      const seq = msg ? ` data-seq="${msg.seq}"` : '';
      return `<button class="pb-row" data-task="${esc(d.task_id)}"${seq}>
        <span class="pb-row-main">
          <span class="pb-row-q">${esc(d.question)}</span>
          <span class="pb-row-meta">${esc(source)} · ${esc(rel(d.created_at))}</span>
        </span>
        <svg class="pb-row-chev" viewBox="0 0 24 24" aria-hidden="true"><path d="M9 5l7 7-7 7"/></svg>
      </button>`;
    }).join('');
  }
}

function closePendingBar() {
  pendingOpen = false;
  const toggle = $('#pendingToggle');
  if (toggle) toggle.setAttribute('aria-expanded', 'false');
  const list = $('#pendingList');
  if (list) list.hidden = true;
}

// 能回到那条对话就回对话并高亮，找不到对应消息就打开卡片。
function jumpFromPending(taskId, seq) {
  closePendingBar();
  const msg = seq ? state.chat.find(m => m.seq === seq) : state.chat.find(m => m.link_task === taskId);
  if (!msg) {
    openTask(taskId);
    return;
  }
  chatPinned = false;
  flashSeq = msg.seq;
  if (state.view !== 'inbox') setView('inbox');
  else renderInbox();
}

function renderInbox() {
  const el = $('#inbox');
  if (!el) return;
  renderPendingBar();
  const oldScroll = $('#inboxScroll');
  const oldTop = oldScroll ? oldScroll.scrollTop : 0;
  const freeInput = $('#freeInput');
  if (freeInput) freeDraft = freeInput.value;

  const pending = state.decisions;
  const decisionsHTML = pending.length
    ? pending.map(decisionHTML).join('')
    : `<div class="empty"><svg viewBox="0 0 24 24"><path d="M4 13.5V6a2 2 0 0 1 2-2h12a2 2 0 0 1 2 2v7.5"/><path d="M4 13.5h4.2l1.4 2.2h4.8l1.4-2.2H20V18a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2z"/></svg>
        <div class="e-title">没有待拍板项</div><div class="e-sub">需要你决定的事会出现在这里</div></div>`;

  // 接口按新到旧返回；聊天流按 IM 习惯旧在上、新在下。
  const msgs = state.chat.slice().reverse();
  const msgList = (msgs.length ? msgs.map(messageHTML).join('') : '<li class="msg-empty">还没有消息</li>')
    + (systemNote ? `<li class="msg-system">${esc(systemNote)}</li>` : '');
  el.innerHTML = `
    <div class="inbox-scroll" id="inboxScroll">
      <section class="inbox-section">
        <h2 class="sec-h">待拍板 <span>${pending.length}</span></h2>
        ${decisionsHTML}
      </section>
      <section class="inbox-section chat-section">
        <h2 class="sec-h">跟主会话说一句</h2>
        <ul class="msg-list" id="msgList">${msgList}</ul>
      </section>
    </div>
    <form class="free-compose" id="freeForm">
      <textarea id="freeInput" rows="1" enterkeyhint="send" placeholder="发消息…（/ 开头看指令）">${esc(freeDraft)}</textarea>
      <button class="send-btn" type="submit" aria-label="发送">
        <svg viewBox="0 0 24 24"><path d="M12 19V5M6 11l6-6 6 6"/></svg>
      </button>
    </form>`;
  const fi = $('#freeInput');
  if (fi) autogrow(fi);

  const sc = $('#inboxScroll');
  if (sc) {
    sc.addEventListener('scroll', () => { chatPinned = chatNearBottom(sc); }, { passive: true });
    // 默认贴底；用户翻历史时保留原来的位置，不被新消息拽走。
    requestAnimationFrame(() => {
      const target = flashSeq ? sc.querySelector(`.msg-item[data-seq="${flashSeq}"]`) : null;
      if (target) {
        target.scrollIntoView({ block: 'center' });
        target.classList.add('flash');
        setTimeout(() => target.classList.remove('flash'), 1600);
        flashSeq = 0;
        return;
      }
      if (chatPinned) sc.scrollTop = sc.scrollHeight;
      else sc.scrollTop = oldTop;
    });
  }
}

/* ---------- calendar ---------- */

const MONTHS = ['1月', '2月', '3月', '4月', '5月', '6月', '7月', '8月', '9月', '10月', '11月', '12月'];

function addDays(d, n) {
  const x = new Date(d);
  x.setDate(x.getDate() + n);
  return x;
}

function renderCalendar() {
  const map = new Map(state.activity.map(d => [d.date, d.count]));
  const total = state.activity.reduce((a, d) => a + d.count, 0);
  const activeDays = state.activity.filter(d => d.count > 0).length;

  // streak ending today (or yesterday)
  let streak = 0;
  const today = new Date();
  today.setHours(0, 0, 0, 0);
  let cursor = new Date(today);
  if (!(map.get(ymd(cursor)) > 0)) cursor = addDays(cursor, -1);
  while (map.get(ymd(cursor)) > 0) {
    streak++;
    cursor = addDays(cursor, -1);
  }

  $('#stats').innerHTML = `
    <div class="stat"><div class="num">${total}</div><div class="lbl">事件总数</div></div>
    <div class="stat"><div class="num">${activeDays}</div><div class="lbl">活跃天数</div></div>
    <div class="stat"><div class="num">${streak}</div><div class="lbl">连续天数</div></div>`;

  const weeks = 12;
  const mondayOffset = (today.getDay() + 6) % 7;
  const start = addDays(today, -mondayOffset - (weeks - 1) * 7);

  let cells = '';
  const months = [];
  let lastMonth = -1;
  for (let w = 0; w < weeks; w++) {
    let label = '';
    for (let d = 0; d < 7; d++) {
      const date = addDays(start, w * 7 + d);
      if (d === 0) {
        const mo = date.getMonth();
        if (mo !== lastMonth) { label = MONTHS[mo]; lastMonth = mo; }
      }
      if (date > today) { cells += '<div class="cell empty"></div>'; continue; }
      const key = ymd(date);
      const c = map.get(key) || 0;
      const lvl = c === 0 ? 0 : c < 3 ? 1 : c < 6 ? 2 : c < 10 ? 3 : 4;
      cells += `<div class="cell lvl-${lvl}" title="${key} · ${c} 条"></div>`;
    }
    months.push(label);
  }
  $('#heatmap').innerHTML = cells;
  $('#monthLabels').innerHTML = months.map(m => `<span>${m}</span>`).join('');

  const recent = state.activity.filter(d => d.count > 0).slice(-6).reverse();
  const max = Math.max(1, ...recent.map(d => d.count));
  $('#recentDays').innerHTML = recent.length
    ? recent.map(d => {
        const dt = new Date(d.date + 'T00:00:00');
        return `<li>
          <span class="rdate">${dt.getMonth() + 1}月${dt.getDate()}日</span>
          <span class="rbar" style="width:${Math.max(6, (d.count / max) * 180)}px"></span>
          <span class="rcount">${d.count} 条</span>
        </li>`;
      }).join('')
    : '<li class="msg-empty">最近没有活动</li>';
}

/* ---------- views ---------- */

function setView(v, fromHash) {
  if (!['board', 'inbox', 'calendar'].includes(v)) v = 'inbox';
  state.view = v;
  document.body.dataset.view = v;
  if (v === 'inbox') chatPinned = true;
  if (!fromHash) {
    const target = v === 'inbox' ? location.pathname : '#' + v;
    history.replaceState(null, '', target);
  }
  $$('.view').forEach(el => { el.hidden = el.id !== 'view-' + v; });
  $$('.tab, .bnav-item').forEach(b => {
    const on = b.dataset.view === v;
    b.classList.toggle('active', on);
    b.setAttribute('aria-selected', on ? 'true' : 'false');
  });
  window.scrollTo(0, 0);

  if (v === 'board') renderBoard();
  if (v === 'inbox') { renderInbox(); refreshChat().catch(() => {}); }
  if (v === 'calendar') { renderCalendar(); refreshActivity().catch(() => {}); }
}

function viewFromHash() {
  const h = location.hash.replace('#', '');
  if (h.startsWith('task=') || h === 'board') return 'board';
  return h === 'calendar' ? 'calendar' : 'inbox';
}

function taskFromHash() {
  const m = location.hash.match(/(?:^#|&)task=([^&]+)/);
  return m ? decodeURIComponent(m[1]) : null;
}

/* ---------- theme ---------- */

const SUN = '<svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="4.2"/><path d="M12 2v2.4M12 19.6V22M2 12h2.4M19.6 12H22M4.9 4.9l1.7 1.7M17.4 17.4l1.7 1.7M19.1 4.9l-1.7 1.7M6.6 17.4l-1.7 1.7"/></svg>';
const MOON = '<svg viewBox="0 0 24 24"><path d="M20 14.5A8.5 8.5 0 0 1 9.5 4a8.5 8.5 0 1 0 10.5 10.5z"/></svg>';

function setTheme(dark) {
  document.documentElement.classList.toggle('dark-theme', dark);
  try { localStorage.setItem('ab-theme', dark ? 'dark' : 'light'); } catch (_) {}
  const meta = document.querySelector('meta[name="theme-color"]');
  if (meta) meta.setAttribute('content', dark ? '#15130f' : '#f3efe6');
  const btn = $('#themeToggle');
  if (btn) btn.innerHTML = dark ? SUN : MOON;
}

function toggleTheme() {
  setTheme(!document.documentElement.classList.contains('dark-theme'));
}

/* ---------- realtime ---------- */

function scheduleRefresh() {
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(async () => {
    try {
      await refreshTasks();
      await refreshDecisions();
      if (state.view === 'calendar') await refreshActivity();
      if (state.view === 'inbox') await refreshChat();
      if (openTaskId) await refreshDetail(openTaskId, true);
    } catch (_) {}
  }, 160);
}

function connectSSE() {
  if (es) return;
  const live = $('#live');
  es = new EventSource('/api/stream?token=' + encodeURIComponent(API_TOKEN));
  es.addEventListener('hello', () => live && live.classList.remove('off'));
  es.addEventListener('change', () => {
    if (live) live.classList.remove('off');
    scheduleRefresh();
  });
  es.onerror = () => {
    if (live) live.classList.add('off');
  };
}

/* ---------- keyboard / viewport ---------- */

function installViewport() {
  const vv = window.visualViewport;
  if (!vv) return;
  const apply = () => {
    const kb = Math.max(0, window.innerHeight - vv.height - vv.offsetTop);
    document.documentElement.style.setProperty('--kb', kb + 'px');
    document.body.classList.toggle('kb-open', kb > 0);
    // 键盘改变可视高度后，仍贴底的消息流要跟着贴回去。
    if (chatPinned) requestAnimationFrame(scrollChatBottom);
  };
  vv.addEventListener('resize', apply);
  vv.addEventListener('scroll', apply);
  apply();
}

/* ---------- push ---------- */

let pushEnabled = false;

function pushSupported() {
  return 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window;
}

function updatePushButton() {
  const btn = $('#pushToggle');
  if (!btn) return;
  btn.classList.toggle('push-on', pushEnabled);
  btn.classList.toggle('push-off', !pushEnabled);
  btn.setAttribute('aria-pressed', pushEnabled ? 'true' : 'false');
  const label = pushEnabled ? '关闭通知' : '开启通知';
  btn.setAttribute('aria-label', label);
  btn.title = label;
}

async function syncPushStatus() {
  if (STATIC || !pushSupported()) return;
  try {
    const st = await api('/api/push/status');
    pushEnabled = !!st.enabled;
    updatePushButton();
  } catch (_) {}
}

function urlBase64ToUint8Array(base64) {
  const padding = '='.repeat((4 - (base64.length % 4)) % 4);
  const raw = atob((base64 + padding).replace(/-/g, '+').replace(/_/g, '/'));
  const out = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
  return out;
}

async function enablePush() {
  const perm = await Notification.requestPermission();
  if (perm !== 'granted') {
    toast('没拿到通知权限，可在浏览器站点设置里允许');
    return;
  }
  const reg = await navigator.serviceWorker.ready;
  const { key } = await api('/api/push/key');
  let sub = await reg.pushManager.getSubscription();
  if (!sub) {
    sub = await reg.pushManager.subscribe({
      userVisibleOnly: true,
      applicationServerKey: urlBase64ToUint8Array(key),
    });
  }
  await api('/api/push/subscribe', { method: 'POST', body: sub.toJSON() });
  pushEnabled = true;
  updatePushButton();
  toast('已开启通知：等你拍板、完成、阻塞时提醒');
}

async function disablePush() {
  const reg = await navigator.serviceWorker.ready;
  const sub = await reg.pushManager.getSubscription();
  if (sub) await sub.unsubscribe().catch(() => {});
  await api('/api/push/unsubscribe', { method: 'POST' });
  pushEnabled = false;
  updatePushButton();
  toast('已关闭通知');
}

async function togglePush() {
  if (!window.isSecureContext) {
    toast('通知需要 HTTPS 或 localhost 访问，当前地址浏览器不开放通知');
    return;
  }
  if (!pushSupported()) {
    toast('这个浏览器不支持通知');
    return;
  }
  try {
    if (pushEnabled) await disablePush();
    else await enablePush();
  } catch (e) {
    toast('通知设置失败：' + e.message, 'bad');
  }
}

/* ---------- boot ---------- */

async function boot({ keepUI } = {}) {
  if (!keepUI) {
    skeletonBoard();
    setTheme(document.documentElement.classList.contains('dark-theme'));
  }
  bindUI();
  installViewport();
  try {
    await Promise.all([refreshTasks(), refreshDecisions()]);
    const v = viewFromHash();
    setView(v, true);
    if (v === 'inbox') await refreshChat();
    if (v === 'calendar') await refreshActivity();
    const deep = taskFromHash();
    if (deep) await openTask(deep);
    if (!STATIC) {
      connectSSE();
      if ('serviceWorker' in navigator) {
        navigator.serviceWorker.register('/sw.js').catch(() => {});
        syncPushStatus();
      }
    }
  } catch (e) {
    if (e.message !== 'unauthorized') toast('连接失败：' + e.message, 'bad');
  }
}

let bound = false;
function bindUI() {
  if (bound) return;
  bound = true;

  $('#themeToggle').addEventListener('click', toggleTheme);
  $('#pushToggle').addEventListener('click', togglePush);
  updatePushButton();
  $('#pendingToggle').addEventListener('click', () => {
    pendingOpen = !pendingOpen;
    renderPendingBar();
  });
  $('#pendingList').addEventListener('click', e => {
    const row = e.target.closest('.pb-row');
    if (!row) return;
    jumpFromPending(row.dataset.task, Number(row.dataset.seq) || 0);
  });
  $('#logoutBtn').addEventListener('click', async () => {
    try {
      await fetch('/api/logout', {
        method: 'POST',
        credentials: 'same-origin',
        headers: authHeaders(),
      });
    } catch (_) {}
    location.replace('/login');
  });
  $('#sheetClose').addEventListener('click', closeSheet);

  $$('.tab, .bnav-item').forEach(b => {
    b.addEventListener('click', () => setView(b.dataset.view));
  });

  window.addEventListener('hashchange', () => setView(viewFromHash(), true));

  // board card click
  $('#board').addEventListener('click', e => {
    const card = e.target.closest('.card');
    if (card) openTask(card.dataset.id);
  });
  $('#board').addEventListener('keydown', e => {
    if (e.key !== 'Enter' && e.key !== ' ') return;
    const card = e.target.closest('.card');
    if (card) { e.preventDefault(); openTask(card.dataset.id); }
  });

  // dialog backdrop close
  const dlg = $('#detail');
  dlg.addEventListener('click', e => { if (e.target === dlg) closeSheet(); });
  dlg.addEventListener('close', () => { openTaskId = null; detailFromSeq = null; replyTo = null; });

  // timeline reply buttons
  $('#d-timeline').addEventListener('click', e => {
    const btn = e.target.closest('[data-reply]');
    if (!btn) return;
    replyTo = Number(btn.dataset.reply);
    updateComposeCtx();
    const input = $('#composeInput');
    if (input) { input.focus(); input.scrollIntoView({ block: 'nearest', behavior: 'smooth' }); }
  });

  // pending decision option buttons (detail)
  $('#d-actions').addEventListener('click', e => {
    const back = e.target.closest('#dBackChat');
    if (back) { backToChat(Number(back.dataset.seq)); return; }
    const btn = e.target.closest('[data-answer]');
    if (btn) answerDecision(btn.dataset.answer, btn.dataset.value);
  });
  $('#d-actions').addEventListener('submit', e => {
    e.preventDefault();
    const form = e.target.closest('[data-decision]');
    if (!form) return;
    const input = form.querySelector('input');
    if (input && input.value.trim()) answerDecision(form.dataset.decision, input.value.trim());
  });

  // composer
  $('#composeCtxClear').addEventListener('click', () => { replyTo = null; updateComposeCtx(); });
  $('#composeSend').addEventListener('click', sendReply);
  const ci = $('#composeInput');
  ci.addEventListener('input', () => autogrow(ci));
  ci.addEventListener('keydown', e => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      sendReply();
    }
  });

  // inbox delegation
  $('#inbox').addEventListener('click', e => {
    const ans = e.target.closest('[data-answer]');
    if (ans) { answerDecision(ans.dataset.answer, ans.dataset.value); return; }
    const taskBtn = e.target.closest('[data-task]');
    if (taskBtn) openTask(taskBtn.dataset.task, Number(taskBtn.dataset.msg) || null);
  });
  $('#inbox').addEventListener('submit', e => {
    const form = e.target;
    if (form.id === 'freeForm') {
      e.preventDefault();
      const input = form.querySelector('textarea');
      if (input && input.value.trim()) sendFreeMessage(input.value);
      return;
    }
    if (form.matches('[data-decision]')) {
      e.preventDefault();
      const input = form.querySelector('input');
      if (input && input.value.trim()) answerDecision(form.dataset.decision, input.value.trim());
    }
  });
  $('#inbox').addEventListener('keydown', e => {
    if (e.target.id !== 'freeInput') return;
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      const t = e.target;
      if (t.form && t.form.requestSubmit) t.form.requestSubmit();
      else if (t.value.trim()) sendFreeMessage(t.value);
    }
  });
  $('#inbox').addEventListener('input', e => {
    if (e.target.id === 'freeInput') {
      freeDraft = e.target.value;
      autogrow(e.target);
    }
  });
}

boot();
