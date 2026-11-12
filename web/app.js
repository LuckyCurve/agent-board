/* Agent Board front-end — vanilla JS, no framework. */
/* global ABLinkify */

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

const COLUMNS = [
  { key: 'todo', label: '待办' },
  { key: 'doing', label: '进行中' },
  { key: 'blocked', label: '阻塞' },
  { key: 'done', label: '已完成' },
  { key: 'archived', label: '归档' },
];

const state = {
  tasks: [],
  archived: [],
  decisions: [],
  chat: [],
  activity: [],
  agents: [],
  view: 'inbox',
  loaded: false,
  filterAgent: '',      // 空 = 全部 Agent
  filterTags: [],       // 选中的标签（多选，任一命中即显示）
  calAgent: '',         // 日历是全局的，这里单独挑一个 Agent 看
};

// ?static=1 关闭 SSE 与 service worker，供无头截图与弱网排查使用。
const STATIC = new URLSearchParams(location.search).has('static');

function loadFilters() {
  try { state.filterAgent = localStorage.getItem('ab-agent') || ''; } catch (_) {}
  try {
    const raw = JSON.parse(localStorage.getItem('ab-tags') || '[]');
    if (Array.isArray(raw)) state.filterTags = raw.filter(x => typeof x === 'string');
  } catch (_) {}
}

function saveFilters() {
  try {
    localStorage.setItem('ab-agent', state.filterAgent);
    localStorage.setItem('ab-tags', JSON.stringify(state.filterTags));
  } catch (_) {}
}

let openTaskId = null;
let detailFromSeq = null;  // 从对话点进来的消息 seq，用于返回上下文
let detailReturnFocus = null; // 关掉详情后把焦点还给原来那张卡
let freeDraft = '';
let refreshTimer = null;
let es = null;
let lastEventSeq = 0;      // 本页见过的最大 seq，唤醒/重连时按它补拉
let chatPinned = true;     // 消息流是否贴着底部；用户往上翻时关闭
let pendingOpen = false;   // 「等你决定」列表是否展开
let flashSeq = 0;          // 刚从待拍板跳进来的消息，渲染后闪一下
let pendingImages = [];    // 已上传、等随消息发出的图片 id
let uploadingImages = 0;   // 正在上传的图片数：还有图没传完时不许发
let activeTaskID = '';    // 对话里引用的任务：之后发的消息都挂到这张卡
let plusOpen = false;
let systemNote = '';       // /状态 等本地提示，不落库
try { activeTaskID = localStorage.getItem('ab-active-task') || ''; } catch (_) {}

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

const richText = ABLinkify.richText;
const richPreview = ABLinkify.richPreview;

// #task=T-N 渲染成胶囊时带上卡片标题，找不到就只显示编号。
const linkOpts = {
  taskTitle(id) {
    const t = (state.tasks || []).find(x => x.id === id) ||
      (state.archived || []).find(x => x.id === id);
    return t ? t.title : '';
  },
};

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

// 网页一律走同源会话 cookie，页面里不再内嵌主令牌。
// 过渡期：旧页面（部署前打开、还没刷新）仍带着 meta token，服务端照收；
// 这里也照样发出去，免得旧页面在刷新前就断了。
const LEGACY_TOKEN = (document.querySelector('meta[name="api-token"]') || {}).content || '';

function authHeaders(extra) {
  return Object.assign(LEGACY_TOKEN ? { Authorization: 'Bearer ' + LEGACY_TOKEN } : {}, extra || {});
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
  renderFilterBar();
  if (state.view === 'board') renderBoard();
}

async function refreshArchived() {
  const data = await api('/api/tasks?view=archived');
  state.archived = data.tasks || [];
  renderFilterBar();
  if (state.view === 'archive') renderArchive();
}

async function moveTask(id, column, note) {
  try {
    await api('/api/tasks/' + encodeURIComponent(id), { method: 'PATCH', body: { column } });
    toast(note || '已移动');
    await Promise.all([refreshTasks(), refreshArchived(), refreshDecisions()]);
    if (state.view === 'inbox') await refreshChat();
  } catch (e) {
    toast('操作失败：' + e.message, 'bad');
  }
}

async function deleteTask(id) {
  if (!window.confirm('永久删除这张卡？事件会留作审计，但卡片不可恢复。')) return;
  try {
    await api('/api/tasks/' + encodeURIComponent(id) + '?confirm=yes', { method: 'DELETE' });
    toast('已删除');
    await Promise.all([refreshTasks(), refreshArchived(), refreshDecisions()]);
    if (state.view === 'inbox') await refreshChat();
  } catch (e) {
    toast('删除失败：' + e.message, 'bad');
  }
}

function renderArchive() {
  const el = $('#archiveList');
  if (!el) return;
  const all = state.archived;
  const list = all.filter(matchTask);
  const n = list.length;
  const count = $('#archiveCount');
  if (count) count.textContent = String(n);
  el.innerHTML = n ? list.map(t => `<article class="archive-row" data-id="${esc(t.id)}">
    <div class="ar-main">
      <span class="task-id">${esc(t.id)}</span>
      <b>${esc(t.title)}</b>
      <span class="ar-meta">${esc(t.agent || '')} · ${rel(t.updated_at)}</span>
    </div>
    <div class="ar-actions">
      <button class="btn ghost" data-restore="${esc(t.id)}">恢复</button>
      <button class="btn ghost danger" data-delete="${esc(t.id)}">删除</button>
    </div>
  </article>`).join('')
    : (all.length
      ? '<div class="empty"><div class="e-title">没有匹配的归档</div><div class="e-sub">换个筛选条件试试</div></div>'
      : '<div class="empty"><div class="e-title">还没有归档</div><div class="e-sub">把看板最右「归档」列里的卡片拖过去，或在卡片详情里一键归档</div></div>');
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
  const q = state.calAgent ? '?days=84&agent=' + encodeURIComponent(state.calAgent) : '?days=84';
  const data = await api('/api/activity' + q);
  state.activity = data.days || [];
  renderCalAgents();
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

/* ---------- 筛选：Agent + 标签 ---------- */

// 一张卡能按什么筛：项目算一个，自定义标签各自算一个。
function taskTags(t) {
  const out = [];
  if (t.project) out.push('项目:' + t.project);
  (t.tags || []).forEach(x => out.push(x));
  return out;
}

function matchTask(t) {
  if (state.filterAgent && t.agent !== state.filterAgent) return false;
  if (state.filterTags.length) {
    const tags = taskTags(t);
    if (!state.filterTags.some(f => tags.indexOf(f) >= 0)) return false;
  }
  return true;
}

function filterActive() {
  return !!(state.filterAgent || state.filterTags.length);
}

// 筛选行上的标签：项目 + 自定义标签，去重排序。
function allFilterTags() {
  const set = new Set();
  state.tasks.concat(state.archived).forEach(t => taskTags(t).forEach(x => set.add(x)));
  return Array.from(set).sort((a, b) => a.localeCompare(b, 'zh'));
}

function tagChipHTML(tag) {
  const on = state.filterTags.indexOf(tag) >= 0;
  const i = tag.indexOf(':');
  const kind = i > 0 ? tag.slice(0, i) : '';
  const name = i > 0 ? tag.slice(i + 1) : tag;
  const label = kind
    ? `<i class="tc-kind">${esc(kind)}</i>${esc(name)}`
    : esc(name);
  return `<button class="tagchip${on ? ' on' : ''}" data-tag="${esc(tag)}" aria-pressed="${on ? 'true' : 'false'}">${label}</button>`;
}

function renderFilterBar() {
  const row = $('#tagRow');
  if (row) {
    const tags = allFilterTags();
    row.innerHTML = tags.length
      ? tags.map(tagChipHTML).join('')
      : '<span class="fb-hint">还没有标签，卡片详情里可以加</span>';
  }
  const clear = $('#clearFilter');
  if (clear) clear.hidden = !filterActive();
  const sel = $('#agentSwitch');
  if (sel && sel.value !== state.filterAgent) sel.value = state.filterAgent;
  // 手机上筛选收起时，用按钮上的角标告诉用户「筛着呢」。
  const badge = $('#filterBadge');
  if (badge) {
    const n = state.filterTags.length + (state.filterAgent ? 1 : 0);
    badge.hidden = n === 0;
    badge.textContent = String(n);
  }
  const toggle = $('#filterToggle');
  if (toggle) toggle.classList.toggle('on', filterActive());
}

function toggleTag(tag) {
  const i = state.filterTags.indexOf(tag);
  if (i >= 0) state.filterTags.splice(i, 1);
  else state.filterTags.push(tag);
  saveFilters();
  renderFilterBar();
  renderBoard();
  renderArchive();
  if (state.view === 'inbox') renderInbox();
}

function clearFilters() {
  state.filterAgent = '';
  state.filterTags = [];
  saveFilters();
  renderFilterBar();
  renderBoard();
  renderArchive();
  if (state.view === 'inbox') renderInbox();
  toast('已清除筛选');
}

async function refreshAgents() {
  try {
    const data = await api('/api/agents');
    state.agents = data.agents || [];
  } catch (_) {
    state.agents = [];
  }
  const sel = $('#agentSwitch');
  if (sel) {
    const cur = state.filterAgent;
    sel.innerHTML = '<option value="">全部 Agent</option>' +
      state.agents.map(a => `<option value="${esc(a)}">${esc(a)}</option>`).join('');
    sel.value = state.agents.indexOf(cur) >= 0 ? cur : '';
    if (sel.value !== state.filterAgent) {
      state.filterAgent = sel.value;
      saveFilters();
    }
  }
  renderCalAgents();
}

// Agent 切换器就在筛选行，跟标签筛选放一起；窄屏也不用挪。

/* ---------- board ---------- */

function lastTextOf(e) {
  if (!e) return '';
  if (e.type === 'task.moved') return '移到' + colLabel(e.body);
  return e.body || '';
}

function cardHTML(t) {
  const last = t.last_event;
  const lastHTML = richPreview(lastTextOf(last), 96, linkOpts);
  const r = t.reply;
  const alert = taskAlert(t);
  const strip = r && (r.status === 'received' || r.status === 'processing')
    ? `<div class="reply-strip ${esc(r.status)}">${statusBadge(r)}</div>` : '';
  const alertHTML = alert && alert.kind === 'failed'
    ? `<div class="card-alert"><b>${esc(alert.label)}</b><span>${esc(trunc(alert.reason, 64))}</span></div>` : '';
  return `<article class="card" data-id="${esc(t.id)}" draggable="true" tabindex="0" role="button" aria-label="${esc(t.title)}">
    <div class="card-top">
      <span class="status-dot ${esc(t.column)}"></span>
      <span class="task-id">${esc(t.id)}</span>
      <time class="card-time">${rel(t.updated_at)}</time>
    </div>
    <h3 class="card-title">${esc(t.title)}</h3>
    ${lastHTML ? `<p class="card-last">${lastHTML}</p>` : ''}
    ${alertHTML}
    ${strip}
    <div class="card-meta">
      ${t.project ? `<span class="chip">${esc(t.project)}</span>` : ''}
      ${t.agent ? `<span class="chip ghost">${esc(t.agent)}</span>` : ''}
      ${(t.tags || []).map(x => `<span class="chip tag">${esc(x)}</span>`).join('')}
      ${t.pending_decisions > 0 ? `<span class="badge warn">待拍板 ${t.pending_decisions}</span>` : ''}
      <button type="button" class="card-quote" data-quote="${esc(t.id)}">引用</button>
    </div>
  </article>`;
}

function renderBoard() {
  const board = $('#board');
  if (!board) return;
  const filtered = filterActive();
  board.innerHTML = COLUMNS.map(col => {
    const items = col.key === 'archived' ? [] : state.tasks.filter(t => t.column === col.key && matchTask(t));
    const empty = filtered ? '没有匹配的卡片' : '暂无任务';
    return `<section class="column" data-col="${col.key}" data-drop="1">
      <header class="col-head">
        <span class="col-dot ${col.key}"></span>
        <h2>${col.label}</h2>
        <span class="col-count">${items.length}</span>
      </header>
      <div class="col-body">
        ${col.key === 'archived' ? '<div class="col-empty">把卡片拖到这里归档</div>' :
          (items.length ? items.map(cardHTML).join('') : `<div class="col-empty">${empty}</div>`)}
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
        <p class="tl-body">${richText(e.body, linkOpts)}</p>
        ${imagesHTML(e.data, 'tl-images')}
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
  const dlg = $('#detail');
  if (!dlg.open) {
    const ae = document.activeElement;
    detailReturnFocus = ae && ae.closest && ae.closest('.card') ? ae : null;
    dlg.showModal();
  }
  await refreshDetail(id, false);
  // 打开时焦点落在标题上，键盘用户 Tab 才进返回/操作按钮。
  const title = $('#d-title');
  if (title) title.focus({ preventScroll: true });
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
  if (task.column !== 'archived') {
    actions += `<button class="btn ghost" id="dArchive">收进归档</button>`;
  }
  $('#d-actions').innerHTML = actions + pendingHTML(decisions || []);
  $('#d-timeline').innerHTML = timelineHTML(events || [], replies || []);
  bindDetailExtras(task);

  if (!keepScroll || atBottom) body.scrollTop = body.scrollHeight;
}

// 详情里的标签和转交。
function detailTagsHTML(task) {
  const tags = task.tags || [];
  const chips = tags.map(t => `<span class="dtag">${esc(t)}<button data-rmtag="${esc(t)}" aria-label="去掉标签 ${esc(t)}">×</button></span>`).join('');
  return `<div class="d-tags" id="dTags">
    <span class="dt-label">标签</span>
    ${chips}
    <input class="dtag-input" id="dTagInput" placeholder="加个标签" maxlength="24">
    <button class="dtag-add" id="dTagAdd">添加</button>
  </div>`;
}

function detailAssignHTML(task) {
  const list = state.agents.slice();
  if (task.agent && list.indexOf(task.agent) < 0) list.unshift(task.agent);
  const opts = ['<option value="">（不指派）</option>']
    .concat(list.map(a => `<option value="${esc(a)}"${a === task.agent ? ' selected' : ''}>${esc(a)}</option>`)).join('');
  return `<div class="d-assign">
    <label for="dAssign">负责 Agent</label>
    <select id="dAssign">${opts}</select>
    <button class="btn ghost tiny" id="dAssignBtn">转交</button>
  </div>`;
}

function bindDetailExtras(task) {
  const body = $('#d-actions');
  if (!body) return;
  body.insertAdjacentHTML('beforeend', detailTagsHTML(task) + detailAssignHTML(task));

  const add = $('#dTagAdd');
  const input = $('#dTagInput');
  const submit = () => {
    const v = (input.value || '').trim();
    if (!v) return;
    input.value = '';
    addTaskTag(task.id, v);
  };
  if (add) add.addEventListener('click', submit);
  if (input) input.addEventListener('keydown', ev => {
    if (ev.key === 'Enter') { ev.preventDefault(); submit(); }
  });

  body.querySelectorAll('[data-rmtag]').forEach(b => b.addEventListener('click', () => {
    removeTaskTag(task.id, b.dataset.rmtag);
  }));

  const assign = $('#dAssignBtn');
  if (assign) assign.addEventListener('click', async () => {
    const sel = $('#dAssign');
    if (!sel || !sel.value) { toast('先选一个 Agent', 'bad'); return; }
    try {
      await api('/api/tasks/' + encodeURIComponent(task.id), {
        method: 'PATCH', body: { agent: sel.value },
      });
      toast('已转交给 ' + sel.value);
      await Promise.all([refreshTasks(), refreshAgents()]);
      if (openTaskId) await refreshDetail(openTaskId, true);
    } catch (e) {
      toast('转交失败：' + e.message, 'bad');
    }
  });
}

async function addTaskTag(taskID, tag) {
  try {
    await api('/api/tasks/' + encodeURIComponent(taskID) + '/tags', {
      method: 'POST', body: { add: [tag] },
    });
    await refreshTasks();
    renderFilterBar();
    if (openTaskId) await refreshDetail(openTaskId, true);
    toast('已加标签');
  } catch (e) {
    toast('加标签失败：' + e.message, 'bad');
  }
}

async function removeTaskTag(taskID, tag) {
  try {
    await api('/api/tasks/' + encodeURIComponent(taskID) + '/tags', {
      method: 'POST', body: { remove: [tag] },
    });
    await refreshTasks();
    renderFilterBar();
    if (openTaskId) await refreshDetail(openTaskId, true);
  } catch (e) {
    toast('去掉标签失败：' + e.message, 'bad');
  }
}

function closeSheet() {
  const dlg = $('#detail');
  if (dlg.open) dlg.close();
  openTaskId = null;
  detailFromSeq = null;
  if (location.hash.includes('task=')) history.replaceState(null, '', location.pathname);
}

/* ---------- composing ---------- */

function autogrow(el) {
  el.style.height = 'auto';
  el.style.height = Math.min(el.scrollHeight, 140) + 'px';
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

function setActiveTask(id) {
  activeTaskID = id || '';
  try {
    if (activeTaskID) localStorage.setItem('ab-active-task', activeTaskID);
    else localStorage.removeItem('ab-active-task');
  } catch (_) {}
  renderComposeContext();
}

function renderComposeContext() {
  const box = $('#activeTaskBar');
  if (!box) return;
  const t = activeTaskID ? taskById(activeTaskID) : null;
  box.hidden = !t;
  if (!t) return;
  box.innerHTML = `<button type="button" class="at-open" data-task="${esc(t.id)}" title="打开卡片">
      <span class="at-id">${esc(t.id)}</span><span class="at-title">${esc(trunc(t.title, 40))}</span>
    </button>
    <button type="button" class="at-x" id="activeTaskClear" aria-label="取消引用">×</button>`;
}

function renderPlusMenu() {
  const m = $('#plusMenu');
  if (!m) return;
  m.hidden = !plusOpen;
  if (!plusOpen) return;
  const tasks = state.tasks.filter(t => t.column === 'doing' || t.column === 'todo').slice(0, 8);
  m.innerHTML = `
    <button type="button" data-attach>选图片</button>
    <button type="button" data-camera>拍照</button>
    <div class="pm-label">引用任务</div>
    ${tasks.length ? tasks.map(t => `<button type="button" data-select-task="${esc(t.id)}">${esc(t.id)} · ${esc(trunc(t.title, 24))}</button>`).join('')
      : '<div class="pm-empty">还没有进行中的任务</div>'}`;
}

function closePlusMenu() {
  plusOpen = false;
  renderPlusMenu();
}

function clearComposeImages() {
  pendingImages = [];
  renderComposeImages();
}

function renderComposeImages() {
  const box = $('#freeImages');
  if (!box) return;
  box.hidden = pendingImages.length === 0;
  box.innerHTML = pendingImages.map((id, i) => `<span class="ci">
    <img src="/images/${esc(id)}" alt="待发送图片 ${i + 1}">
    <button type="button" class="ci-x" data-remove-image="${i}" aria-label="移除图片">
      <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>
    </button>
  </span>`).join('');
}

const IMAGE_TYPES = ['image/png', 'image/jpeg', 'image/webp'];
const IMAGE_MAX_BYTES = 5 << 20;

async function uploadImageFiles(files) {
  const list = Array.from(files).filter(f => IMAGE_TYPES.includes(f.type));
  if (!list.length) {
    toast('只支持 PNG / JPG / WebP 图片', 'bad');
    return;
  }
  for (const f of list) {
    if (pendingImages.length >= 6) { toast('一条消息最多 6 张图', 'bad'); break; }
    if (f.size > IMAGE_MAX_BYTES) { toast('单张图片不能超过 5MB', 'bad'); continue; }
    uploadingImages++;
    try {
      const res = await fetch('/api/images', {
        method: 'POST',
        credentials: 'same-origin',
        headers: authHeaders({ 'Content-Type': f.type }),
        body: f,
      });
      if (res.status === 401) {
        location.replace('/login?next=' + encodeURIComponent(location.pathname));
        throw new Error('unauthorized');
      }
      const data = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(data.error || ('HTTP ' + res.status));
      pendingImages.push(data.id);
      renderComposeImages();
    } catch (e) {
      if (e.message !== 'unauthorized') toast('图片上传失败：' + e.message, 'bad');
    } finally {
      uploadingImages--;
    }
  }
  const fi = $('#freeInput');
  if (fi) fi.focus();
}

function imagesHTML(dataStr, cls = '') {
  let ids = [];
  try { ids = parseData(dataStr).images || []; } catch (_) {}
  if (!Array.isArray(ids) || !ids.length) return '';
  return `<div class="img-grid ${cls}">${ids.map(id =>
    `<button type="button" class="img-thumb" data-image="${esc(id)}" aria-label="查看图片">
      <img src="/images/${esc(id)}" alt="图片" loading="lazy">
    </button>`).join('')}</div>`;
}

function openLightbox(id) {
  const dlg = $('#lightbox');
  const img = $('#lightboxImg');
  if (!dlg || !img) return;
  img.src = '/images/' + id;
  if (!dlg.open) dlg.showModal();
}

function closeLightbox() {
  const dlg = $('#lightbox');
  if (dlg && dlg.open) dlg.close();
  const img = $('#lightboxImg');
  if (img) img.removeAttribute('src');
}

async function sendFreeMessage(text) {
  text = (text || '').trim();
  const images = pendingImages.slice();
  if (!text && !images.length) return;
  if (uploadingImages > 0) {
    toast('图片还在上传，稍等一下');
    return;
  }
  if (text[0] === '/') {
    if (images.length) { toast('指令不能带图片', 'bad'); return; }
    await runCommand(text);
    return;
  }
  try {
    await api('/api/message', { method: 'POST', body: { text, images, task_id: activeTaskID || '', key: uid() } });
    // 先清掉草稿和当前框里的字，刷新后 renderInbox 才不会把旧内容填回去。
    systemNote = '';
    freeDraft = '';
    chatPinned = true;
    clearComposeImages();
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

// 输入框指令：看状态、引用/取消任务、建卡派人、挂待拍板。
async function runCommand(text) {
  const m = /^\/(状态|任务|取消|派|拍板)\s*([\s\S]*)$/.exec(text);
  if (!m) {
    toast('未知指令。可用：/状态、/任务 T-1、/取消、/派 X …、/拍板 T-1 …', 'bad');
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
    if (cmd === '取消') {
      setActiveTask('');
      clearFreeInput();
      toast('已取消引用');
      return;
    }
    if (cmd === '任务') {
      const id = rest.toUpperCase();
      const t = state.tasks.find(x => x.id === id);
      if (!t) { toast('没找到这张卡。用法：/任务 T-1', 'bad'); return; }
      setActiveTask(t.id);
      clearFreeInput();
      toast(`已引用 ${t.id}，之后的消息都挂到这张卡`);
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

function quoteTask(id) {
  setActiveTask(id);
  const dlg = $('#detail');
  if (dlg && dlg.open) closeSheet();
  setView('inbox');
  setTimeout(() => {
    const fi = $('#freeInput');
    if (fi) fi.focus();
  }, 60);
  toast(`已引用 ${id}，在对话里继续说`);
}

function messageHTML(m) {
  const agent = m.author && m.author !== 'user';
  const who = agent ? m.author : '你';
  const t = m.link_task ? taskById(m.link_task) : null;
  const sig = t ? taskAlert(t) : null;
  const quote = t
    ? `<button class="msg-quote" data-task="${esc(t.id)}" data-msg="${m.seq}" title="打开 ${esc(t.id)}">
         <span class="mq-id">${esc(t.id)}</span>
         <span class="mq-title">${esc(trunc(t.title, 40))}</span>
         ${sig ? `<span class="mq-state ${sig.kind}">${esc(sig.label)}</span>` : ''}
       </button>`
    : '';
  const alert = sig && sig.kind === 'failed' && sig.reason
    ? `<div class="msg-alert"><b>${esc(sig.label)}</b><span>${esc(trunc(sig.reason, 90))}</span></div>`
    : '';
  return `<li class="msg-item${agent ? ' agent' : ''}${sig && sig.kind === 'failed' ? ' danger' : ''}" data-seq="${m.seq}">
    <div class="msg-who">${esc(who)}</div>
    ${quote}
    ${m.body ? `<div class="msg-text">${richText(m.body, linkOpts)}</div>` : ''}
    ${imagesHTML(m.data)}
    ${alert}
    <div class="msg-foot"><time>${rel(m.created_at)}</time></div>
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

  // 挂了卡的消息跟着筛选走；没挂卡的自由对话一直看得见。
  const msgs = state.chat.slice().reverse().filter(m => {
    if (!filterActive()) return true;
    const tid = m.link_task || m.task_id;
    if (!tid) return true;
    const t = taskById(tid);
    return t ? matchTask(t) : true;
  });
  const msgList = (msgs.length ? msgs.map(messageHTML).join('') : '<li class="msg-empty">还没有消息</li>')
    + (systemNote ? `<li class="msg-system">${esc(systemNote)}</li>` : '');
  const scrollHTML = `
      <section class="inbox-section">
        <h2 class="sec-h">待拍板 <span>${pending.length}</span></h2>
        ${decisionsHTML}
      </section>
      <section class="inbox-section chat-section">
        <h2 class="sec-h">跟主会话说一句</h2>
        <ul class="msg-list" id="msgList">${msgList}</ul>
      </section>`;
  const formHTML = `
    <form class="free-compose" id="freeForm">
      <div class="compose-images" id="freeImages" hidden></div>
      <div class="active-task" id="activeTaskBar" hidden></div>
      <div class="plus-menu" id="plusMenu" hidden></div>
      <div class="free-row">
        <button type="button" class="attach-btn" id="plusBtn" aria-label="更多" title="更多" aria-expanded="false">
          <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 5v14M5 12h14"/></svg>
        </button>
        <textarea id="freeInput" rows="1" enterkeyhint="send" placeholder="发消息…">${esc(freeDraft)}</textarea>
        <button class="send-btn" type="submit" aria-label="发送">
          <svg viewBox="0 0 24 24"><path d="M12 19V5M6 11l6-6 6 6"/></svg>
        </button>
      </div>
      <input type="file" id="freeFile" accept="image/png,image/jpeg,image/webp" multiple hidden>
      <input type="file" id="freeCam" accept="image/*" capture="environment" hidden>
    </form>`;

  // 输入框不重画：消息刷新时正在打字也不能掉焦点、丢草稿。
  const liveForm = el.querySelector('#freeForm');
  if (liveForm) {
    const sc = el.querySelector('#inboxScroll');
    if (sc) sc.innerHTML = scrollHTML;
  } else {
    el.innerHTML = `<div class="inbox-scroll" id="inboxScroll">${scrollHTML}</div>${formHTML}`;
  }
  renderComposeImages();
  renderComposeContext();
  renderPlusMenu();
  const plus = $('#plusBtn');
  if (plus) plus.setAttribute('aria-expanded', plusOpen ? 'true' : 'false');
  const fi = $('#freeInput');
  if (fi) autogrow(fi);

  const sc = $('#inboxScroll');
  if (sc) {
    if (!sc.dataset.scrollBound) {
      sc.dataset.scrollBound = '1';
      sc.addEventListener('scroll', () => { chatPinned = chatNearBottom(sc); }, { passive: true });
    }
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

// 日历是全局的，这里单独挑一个 Agent 看它的动静。
function renderCalAgents() {
  const el = $('#calAgents');
  if (!el) return;
  const chips = [{ v: '', n: '全部' }].concat(state.agents.map(a => ({ v: a, n: a })));
  el.innerHTML = chips.map(c => `<button class="tagchip${state.calAgent === c.v ? ' on' : ''}" data-calagent="${esc(c.v)}">${esc(c.n)}</button>`).join('');
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
  if (!['board', 'inbox', 'calendar', 'archive'].includes(v)) v = 'inbox';
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
  if (v === 'archive') { renderArchive(); refreshArchived().catch(() => {}); }
}

function viewFromHash() {
  const h = location.hash.replace('#', '');
  if (h.startsWith('task=') || h === 'board') return 'board';
  if (h === 'archive') return 'archive';
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
  const icon = $('#themeIcon');
  if (icon) icon.innerHTML = dark ? SUN : MOON;
  const state = $('#themeState');
  if (state) state.textContent = dark ? '深色' : '浅色';
}

function toggleTheme() {
  setTheme(!document.documentElement.classList.contains('dark-theme'));
}

/* 顶栏只留一个「设置」入口，其余都收进下拉。 */
let settingsOpen = false;

function renderSettings() {
  const menu = $('#settingsMenu');
  const btn = $('#settingsBtn');
  if (menu) menu.hidden = !settingsOpen;
  if (btn) btn.setAttribute('aria-expanded', settingsOpen ? 'true' : 'false');
}

function closeSettings() {
  if (!settingsOpen) return;
  settingsOpen = false;
  renderSettings();
}

/* ---------- realtime ---------- */

async function syncNow() {
  if (syncNow.running) return;
  syncNow.running = true;
  try {
    await refreshTasks();
    await refreshDecisions();
    if (state.view === 'calendar') await refreshActivity();
    if (state.view === 'archive') await refreshArchived();
    if (state.view === 'inbox') await refreshChat();
    if (openTaskId) await refreshDetail(openTaskId, true);
  } catch (_) {
  } finally {
    syncNow.running = false;
  }
}

function scheduleRefresh() {
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(() => syncNow().catch(() => {}), 160);
}

function connectSSE() {
  if (es) return;
  const live = $('#live');
  const since = lastEventSeq ? '&since=' + lastEventSeq : '';
  // 登录态走 cookie；旧页面留下的 token 一并带上，刷新前后都能连。
  const tok = LEGACY_TOKEN ? 'token=' + encodeURIComponent(LEGACY_TOKEN) + '&' : '';
  es = new EventSource('/api/stream?' + tok + since.replace(/^&/, ''));
  es.addEventListener('hello', e => {
    try {
      const d = JSON.parse(e.data);
      if (d.seq > lastEventSeq) lastEventSeq = d.seq;
    } catch (_) {}
    live && live.classList.remove('off');
    scheduleRefresh();
  });
  es.addEventListener('change', e => {
    if (live) live.classList.remove('off');
    try {
      const d = JSON.parse(e.data);
      if (d.seq > lastEventSeq) lastEventSeq = d.seq;
    } catch (_) {}
    scheduleRefresh();
  });
  es.onerror = () => {
    // 移动浏览器会掐断后台 SSE；EventSource 自带重连，唤醒时再强制同步一次。
    if (live) live.classList.add('off');
    if (!document.hidden) scheduleRefresh();
  };
}

function installLifecycleSync() {
  document.addEventListener('visibilitychange', () => {
    if (document.hidden) return;
    scheduleRefresh();
    if (es && es.readyState === EventSource.CLOSED) {
      es.close();
      es = null;
      connectSSE();
    }
  });
  window.addEventListener('focus', () => { if (!document.hidden) scheduleRefresh(); });
  // 前台每 20 秒兜底，SSE 被冻结也不会一直漏消息。
  setInterval(() => { if (!document.hidden) syncNow().catch(() => {}); }, 20000);
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
let pushDevices = 0;
let pushMsg = '';

function pushSupported() {
  // 浏览器只在 HTTPS / localhost 开放通知；不支持时压根别把铃铛摆出来。
  return window.isSecureContext && 'serviceWorker' in navigator &&
    'PushManager' in window && 'Notification' in window;
}

function updatePushButton() {
  const btn = $('#pushToggle');
  if (!btn) return;
  btn.hidden = !pushSupported();
  btn.classList.toggle('push-on', pushEnabled);
  btn.classList.toggle('push-off', !pushEnabled);
  btn.setAttribute('aria-pressed', pushEnabled ? 'true' : 'false');
  btn.title = pushMsg || (pushEnabled ? '关闭通知' : '开启通知');
  const state = $('#pushState');
  if (state) state.textContent = pushEnabled ? (pushDevices ? `${pushDevices} 台` : '开') : '关';
}

async function syncPushStatus() {
  if (STATIC) return;
  try {
    const st = await api('/api/push/status');
    pushEnabled = !!st.enabled;
    pushDevices = st.devices || 0;
    pushMsg = st.message || '';
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
  syncPushStatus();
  toast('已开启通知：等你拍板、完成、阻塞时提醒');
}

async function disablePush() {
  const reg = await navigator.serviceWorker.ready;
  const sub = await reg.pushManager.getSubscription();
  if (sub) await sub.unsubscribe().catch(() => {});
  await api('/api/push/unsubscribe', { method: 'POST' });
  pushEnabled = false;
  updatePushButton();
  syncPushStatus();
  toast('已关闭通知');
}

async function togglePush() {
  if (!pushSupported()) return;
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
  loadFilters();
  bindUI();
  installViewport();
  try {
    await Promise.all([refreshTasks(), refreshDecisions(), refreshAgents()]);
    renderFilterBar();
    const v = viewFromHash();
    setView(v, true);
    if (v === 'inbox') await refreshChat();
    if (v === 'calendar') await refreshActivity();
    const deep = taskFromHash();
    if (deep) await openTask(deep);
    if (!STATIC) {
      connectSSE();
      installLifecycleSync();
      if ('serviceWorker' in navigator) {
        if (pushSupported()) {
          navigator.serviceWorker.register('/sw.js').catch(() => {});
          syncPushStatus();
        }
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

  $('#themeToggle').addEventListener('click', () => { toggleTheme(); renderSettings(); });
  $('#pushToggle').addEventListener('click', async () => { await togglePush(); renderSettings(); });
  updatePushButton();

  // 设置下拉：点外面、按 Esc 都收起来。
  const settingsBtn = $('#settingsBtn');
  if (settingsBtn) {
    settingsBtn.addEventListener('click', () => {
      settingsOpen = !settingsOpen;
      renderSettings();
    });
  }
  document.addEventListener('click', e => {
    if (!settingsOpen) return;
    if (e.target.closest('.settings-wrap')) return;
    closeSettings();
  });
  document.addEventListener('keydown', e => {
    if (e.key === 'Escape' && settingsOpen) { closeSettings(); settingsBtn && settingsBtn.focus(); }
  });

  // 筛选：标签多选、Agent 切换、清除
  const tagRow = $('#tagRow');
  if (tagRow) {
    tagRow.addEventListener('click', e => {
      const chip = e.target.closest('[data-tag]');
      if (chip) toggleTag(chip.dataset.tag);
    });
  }
  const clearBtn = $('#clearFilter');
  if (clearBtn) clearBtn.addEventListener('click', clearFilters);

  // 窄屏筛选是折叠的：点按钮才摊开，选完一个标签自动收起，免得一直占着一行。
  const filterToggle = $('#filterToggle');
  const filterPanel = $('#filterPanel');
  if (filterToggle && filterPanel) {
    filterToggle.addEventListener('click', () => {
      const open = !filterPanel.classList.contains('open');
      filterPanel.classList.toggle('open', open);
      filterToggle.setAttribute('aria-expanded', open ? 'true' : 'false');
    });
  }
  const agentSel = $('#agentSwitch');
  if (agentSel) {
    agentSel.addEventListener('change', () => {
      state.filterAgent = agentSel.value;
      saveFilters();
      renderFilterBar();
      renderBoard();
      renderArchive();
      if (state.view === 'inbox') renderInbox();
    });
  }
  const calAgents = $('#calAgents');
  if (calAgents) {
    calAgents.addEventListener('click', e => {
      const chip = e.target.closest('[data-calagent]');
      if (!chip) return;
      state.calAgent = chip.dataset.calagent;
      refreshActivity();
    });
  }
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
    if (e.target.closest('.lnk')) return;  // 卡片摘要里的链接自己开新标签
    const q = e.target.closest('[data-quote]');
    if (q) { quoteTask(q.dataset.quote); return; }
    const card = e.target.closest('.card');
    if (card) openTask(card.dataset.id);
  });
  $('#archiveList').addEventListener('click', e => {
    const restore = e.target.closest('[data-restore]');
    if (restore) { moveTask(restore.dataset.restore, 'done', '已恢复到已完成'); return; }
    const del = e.target.closest('[data-delete]');
    if (del) deleteTask(del.dataset.delete);
  });
  let dragID = '';
  $('#board').addEventListener('dragstart', e => {
    const card = e.target.closest('.card');
    if (!card) return;
    dragID = card.dataset.id;
    e.dataTransfer.setData('text/plain', dragID);
    e.dataTransfer.effectAllowed = 'move';
  });
  $('#board').addEventListener('dragover', e => {
    const col = e.target.closest('[data-drop]');
    if (col) { e.preventDefault(); col.classList.add('drop-target'); }
  });
  $('#board').addEventListener('dragleave', e => {
    const col = e.target.closest('[data-drop]');
    if (col && !col.contains(e.relatedTarget)) col.classList.remove('drop-target');
  });
  $('#board').addEventListener('drop', e => {
    const col = e.target.closest('[data-drop]');
    if (!col) return;
    e.preventDefault();
    col.classList.remove('drop-target');
    const id = dragID || e.dataTransfer.getData('text/plain');
    if (id) moveTask(id, col.dataset.col, col.dataset.col === 'archived' ? '已收进归档' : '已移动');
    dragID = '';
  });
  $('#board').addEventListener('dragend', () => {
    $$('.drop-target').forEach(el => el.classList.remove('drop-target'));
    dragID = '';
  });
  $('#board').addEventListener('keydown', e => {
    if (e.key !== 'Enter' && e.key !== ' ') return;
    const card = e.target.closest('.card');
    if (card) { e.preventDefault(); openTask(card.dataset.id); }
  });

  // dialog backdrop close
  const dlg = $('#detail');
  dlg.addEventListener('click', e => { if (e.target === dlg) closeSheet(); });
  dlg.addEventListener('close', () => {
    openTaskId = null;
    detailFromSeq = null;
    const back = detailReturnFocus;
    detailReturnFocus = null;
    if (back && document.contains(back)) back.focus();
  });

  // 卡片详情只读：时间线里的「回复」跳回对话并引用这张卡。
  $('#d-timeline').addEventListener('click', e => {
    if (e.target.closest('.lnk')) return;  // 正文里的外链自己开新标签
    const taskChip = e.target.closest('.lnk-task');
    if (taskChip) { openTask(taskChip.dataset.task); return; }
    const btn = e.target.closest('[data-reply]');
    if (!btn || !openTaskId) return;
    quoteTask(openTaskId);
  });
  $('#dQuoteChat').addEventListener('click', () => {
    if (!openTaskId) return;
    quoteTask(openTaskId);
  });

  // pending decision option buttons (detail)
  $('#d-actions').addEventListener('click', e => {
    const back = e.target.closest('#dBackChat');
    if (back) { backToChat(Number(back.dataset.seq)); return; }
    if (e.target.closest('#dArchive')) { moveTask(openTaskId, 'archived', '已收进归档'); closeSheet(); return; }
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

  // inbox delegation
  $('#inbox').addEventListener('click', e => {
    if (e.target.closest('.lnk')) return;  // 正文外链交给浏览器
    if (e.target.closest('#plusBtn')) {
      plusOpen = !plusOpen;
      renderPlusMenu();
      const b = $('#plusBtn');
      if (b) b.setAttribute('aria-expanded', plusOpen ? 'true' : 'false');
      return;
    }
    const select = e.target.closest('[data-select-task]');
    if (select) {
      setActiveTask(select.dataset.selectTask);
      closePlusMenu();
      toast(`已引用 ${select.dataset.selectTask}`);
      return;
    }
    if (e.target.closest('#activeTaskClear')) { setActiveTask(''); toast('已取消引用'); return; }
    const rm = e.target.closest('[data-remove-image]');
    if (rm) {
      pendingImages.splice(Number(rm.dataset.removeImage), 1);
      renderComposeImages();
      return;
    }
    const pick = e.target.closest('[data-attach]');
    if (pick) { closePlusMenu(); const f = $('#freeFile'); if (f) f.click(); return; }
    const snap = e.target.closest('[data-camera]');
    if (snap) { closePlusMenu(); const f = $('#freeCam'); if (f) f.click(); return; }
    const ans = e.target.closest('[data-answer]');
    if (ans) { answerDecision(ans.dataset.answer, ans.dataset.value); return; }
    const taskBtn = e.target.closest('[data-task]');
    if (taskBtn) openTask(taskBtn.dataset.task, Number(taskBtn.dataset.msg) || null);
  });
  $('#inbox').addEventListener('change', e => {
    const t = e.target;
    if (t.id !== 'freeFile' && t.id !== 'freeCam') return;
    if (t.files && t.files.length) uploadImageFiles(t.files);
    t.value = '';
  });
  $('#inbox').addEventListener('paste', e => {
    if (e.target.id !== 'freeInput' || !e.clipboardData) return;
    const files = Array.from(e.clipboardData.items)
      .filter(i => i.kind === 'file' && i.type.startsWith('image/'))
      .map(i => i.getAsFile())
      .filter(Boolean);
    if (files.length) { e.preventDefault(); uploadImageFiles(files); }
  });
  // 图片放大：消息、卡片时间线、待发预览都走同一个灯箱。
  document.addEventListener('click', e => {
    const thumb = e.target.closest('[data-image]');
    if (thumb) { openLightbox(thumb.dataset.image); return; }
    if (e.target.closest('#lightboxClose')) { closeLightbox(); return; }
    if (e.target.id === 'lightbox') closeLightbox();
  });
  $('#inbox').addEventListener('submit', e => {
    const form = e.target;
    if (form.id === 'freeForm') {
      e.preventDefault();
      const input = form.querySelector('textarea');
      if (input && (input.value.trim() || pendingImages.length)) sendFreeMessage(input.value);
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
