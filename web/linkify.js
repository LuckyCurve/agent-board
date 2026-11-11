// 正文链接识别：只认 http(s) 和 #task=T-N，其余一律当纯文本。
// 单独成文件是为了能在 Node 里直接测（app.js 一加载就要 DOM）。
/* global module */
(function (root) {
  'use strict';

  // 中文标点不可能出现在 URL 里，直接当边界；不然「见 https://x.com。然后」会把整句吞进链接。
  const LINK_RE = /https?:\/\/[^\s<>"'`，。；：！？、）】》「」【】（）]+|#task=T-\d+/gi;
  const TRAIL_PUNCT = /[.,;:!?，。；：！？、）】》]$/;

  function escHTML(v) {
    return String(v == null ? '' : v).replace(/[&<>"']/g, c => ({
      '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
    }[c]));
  }

  // 把一段文本切成 {text} 和 {text, url} 两种片段；标点留在文本里。
  function textSegments(v) {
    const s = String(v == null ? '' : v);
    const segs = [];
    let last = 0;
    LINK_RE.lastIndex = 0;
    let m;
    while ((m = LINK_RE.exec(s)) !== null) {
      if (m.index > last) segs.push({ text: s.slice(last, m.index) });
      let hit = m[0];
      let tail = '';
      while (hit.length > 8 && TRAIL_PUNCT.test(hit)) {
        tail = hit.slice(-1) + tail;
        hit = hit.slice(0, -1);
      }
      // 右括号只在没有配对的左括号时才算句末标点。
      while (hit.endsWith(')') && hit.split('(').length < hit.split(')').length) {
        tail = ')' + tail;
        hit = hit.slice(0, -1);
      }
      if (hit.length <= 8) {  // 只剩 "https://" 这种残片，不当链接
        segs.push({ text: m[0] });
        last = m.index + m[0].length;
        continue;
      }
      segs.push({ text: hit, url: hit });
      if (tail) segs.push({ text: tail });
      last = m.index + m[0].length;
    }
    if (last < s.length) segs.push({ text: s.slice(last) });
    return segs;
  }

  function anchor(url, text) {
    return '<a class="lnk" href="' + escHTML(url) + '" target="_blank" rel="noopener noreferrer">' +
      escHTML(text) + '</a>';
  }

  // 纯文本 → 带锚点的 HTML。每一段都过 esc，链接地址也只可能来自那两种形态。
  function richText(v) {
    return textSegments(v).map(seg => seg.url ? anchor(seg.url, seg.text) : escHTML(seg.text)).join('');
  }

  // 卡片摘要：按段截断，不把链接切成半截。
  function richPreview(v, n) {
    let out = '';
    let used = 0;
    let cut = false;
    for (const seg of textSegments(v)) {
      if (used + seg.text.length > n) {
        if (!seg.url) out += escHTML(seg.text.slice(0, Math.max(0, n - used)));
        cut = true;
        break;
      }
      out += seg.url ? anchor(seg.url, seg.text) : escHTML(seg.text);
      used += seg.text.length;
    }
    return cut ? out + '…' : out;
  }

  const api = { escHTML, textSegments, richText, richPreview };
  root.ABLinkify = api;
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
})(typeof globalThis !== 'undefined' ? globalThis : this);
