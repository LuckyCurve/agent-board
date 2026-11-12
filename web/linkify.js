// 正文链接识别。认三种写法：
//   1. [文字](https://…)  Markdown 链接，渲染成文字本身
//   2. https://…          裸链接
//   3. #task=T-N          任务引用，渲染成可点的小胶囊
// 其余一律当纯文本。单独成文件是为了能在 Node 里直接测（app.js 一加载就要 DOM）。
/* global module */
(function (root) {
  'use strict';

  // 中文标点不可能出现在 URL 里，直接当边界；不然「见 https://x.com。然后」会把整句吞进链接。
  const URL_BODY = String.raw`https?:\/\/[^\s<>"'\x60，。；：！？、）】》「」【】（）]+`;
  const TOKEN_RE = new RegExp(
    String.raw`\[([^\]\n]{1,120})\]\((https?:\/\/[^\s()<>"']+)\)` +
    String.raw`|(${URL_BODY})` +
    String.raw`|#task=(T-\d+)`, 'gi');
  const TRAIL_PUNCT = /[.,;:!?，。；：！？、）】》]$/;

  function escHTML(v) {
    return String(v == null ? '' : v).replace(/[&<>"']/g, c => ({
      '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
    }[c]));
  }

  // 把一段文本切成片段：{text} 纯文本，{url} 链接，{task} 任务引用。
  // opts.taskTitle 是个函数，用来给任务胶囊补标题。
  function textSegments(v, opts) {
    const s = String(v == null ? '' : v);
    const titleOf = (opts && opts.taskTitle) || null;
    const segs = [];
    let last = 0;
    TOKEN_RE.lastIndex = 0;
    let m;
    while ((m = TOKEN_RE.exec(s)) !== null) {
      if (m.index > last) segs.push({ text: s.slice(last, m.index) });
      last = m.index + m[0].length;

      if (m[1] !== undefined) {
        // Markdown 链接：显示方括号里的字，指向圆括号里的地址。
        segs.push({ text: m[1], url: m[2] });
        continue;
      }
      if (m[4] !== undefined) {
        const id = m[4].toUpperCase();
        let title = '';
        if (titleOf) {
          try { title = String(titleOf(id) || ''); } catch (_) { title = ''; }
        }
        segs.push({ text: m[0], task: id, title });
        continue;
      }

      // 裸链接：剥掉句末标点，剥掉没配对的右括号。
      let hit = m[3];
      let tail = '';
      while (hit.length > 8 && TRAIL_PUNCT.test(hit)) {
        tail = hit.slice(-1) + tail;
        hit = hit.slice(0, -1);
      }
      while (hit.endsWith(')') && hit.split('(').length < hit.split(')').length) {
        tail = ')' + tail;
        hit = hit.slice(0, -1);
      }
      if (hit.length <= 8) {  // 只剩 "https://" 这种残片，不当链接
        segs.push({ text: m[0] });
        continue;
      }
      segs.push({ text: hit, url: hit });
      if (tail) segs.push({ text: tail });
    }
    if (last < s.length) segs.push({ text: s.slice(last) });
    return segs;
  }

  function anchor(url, text) {
    return '<a class="lnk" href="' + escHTML(url) + '" target="_blank" rel="noopener noreferrer">' +
      escHTML(text) + '</a>';
  }

  const LINK_ICON = '<svg class="lt-ico" viewBox="0 0 16 16" aria-hidden="true">' +
    '<path d="M6.5 9.5a3 3 0 0 0 4.2 0l2-2a3 3 0 1 0-4.2-4.2l-.7.7"/>' +
    '<path d="M9.5 6.5a3 3 0 0 0-4.2 0l-2 2a3 3 0 1 0 4.2 4.2l.7-.7"/></svg>';

  // 任务胶囊：图标 + 编号 + 标题。整块可点，data-task 交给 app.js 打开详情。
  function taskCapsule(id, title) {
    const label = title
      ? '<span class="lt-title">' + escHTML(title) + '</span>'
      : '';
    return '<button type="button" class="lnk-task" data-task="' + escHTML(id) +
      '" title="打开 ' + escHTML(id + (title ? ' ' + title : '')) + '">' +
      LINK_ICON + '<span class="lt-id">' + escHTML(id) + '</span>' + label + '</button>';
  }

  function renderSeg(seg) {
    if (seg.url) return anchor(seg.url, seg.text);
    if (seg.task) return taskCapsule(seg.task, seg.title);
    return escHTML(seg.text);
  }

  // 纯文本 → 带锚点/胶囊的 HTML。每一段都过 esc，地址也只可能来自那三种形态。
  function richText(v, opts) {
    return textSegments(v, opts).map(renderSeg).join('');
  }

  // 卡片摘要：按段截断，不把链接或胶囊切成半截。
  function richPreview(v, n, opts) {
    let out = '';
    let used = 0;
    let cut = false;
    for (const seg of textSegments(v, opts)) {
      const len = seg.task ? seg.task.length + (seg.title ? seg.title.length + 1 : 0) : seg.text.length;
      if (used + len > n) {
        if (!seg.url && !seg.task) out += escHTML(seg.text.slice(0, Math.max(0, n - used)));
        cut = true;
        break;
      }
      out += renderSeg(seg);
      used += len;
    }
    return cut ? out + '…' : out;
  }

  const api = { escHTML, textSegments, richText, richPreview };
  root.ABLinkify = api;
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
})(typeof globalThis !== 'undefined' ? globalThis : this);
