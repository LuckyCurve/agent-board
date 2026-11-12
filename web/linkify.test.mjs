import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const { richText, richPreview, textSegments } = require('../web/linkify.js');

test('http 链接渲染成新标签打开的锚点', () => {
  const out = richText('看这里 https://example.com/a?b=1 就行');
  assert.match(out, /<a class="lnk" href="https:\/\/example\.com\/a\?b=1" target="_blank" rel="noopener noreferrer">/);
  assert.match(out, /看这里 /);
});

test('#task=T-N 渲染成可点的小胶囊，带编号和标题', () => {
  const out = richText('详见 #task=T-12', { taskTitle: id => (id === 'T-12' ? '标签筛选乱码' : '') });
  assert.match(out, /class="lnk-task" data-task="T-12"/);
  assert.match(out, /class="lt-id">T-12</);
  assert.match(out, /class="lt-title">标签筛选乱码</);
  assert.match(out, /lt-ico/);
  // 不是裸链接，也不是原样文本。
  assert.ok(!out.includes('href="#task='), out);
  assert.ok(!out.includes('#task=T-12<'), out);
});

test('找不到标题时胶囊只显示编号', () => {
  const out = richText('#task=T-99', { taskTitle: () => '' });
  assert.match(out, /class="lnk-task" data-task="T-99"/);
  assert.ok(!out.includes('lt-title'), out);
});

test('Markdown 链接 [文字](url) 渲染成文字本身', () => {
  const out = richText('看 [看板](https://example.com/b) 一眼');
  assert.match(out, /<a class="lnk" href="https:\/\/example\.com\/b"/);
  assert.match(out, />看板<\/a>/);
  assert.ok(!out.includes('[看板]'), out);
  assert.ok(!out.includes('](https://'), out);
});

test('Markdown 链接里的文字也要转义', () => {
  const out = richText('[<b>x</b>](https://example.com)');
  assert.ok(!out.includes('<b>'), out);
  assert.match(out, /&lt;b&gt;x&lt;\/b&gt;/);
});

test('Markdown 链接只认 http(s)，javascript: 不认', () => {
  const out = richText('[点我](javascript:alert(1))');
  assert.ok(!out.includes('href="javascript'), out);
});

// 用户不该因为写法不同看到不同的东西：指向某张卡的三种写法都收成同一个胶囊。
test('指向 #task=T-N 的三种写法都渲染成胶囊', () => {
  const opts = { taskTitle: id => (id === 'T-6' ? '输入框重合' : '') };
  const forms = [
    '见 #task=T-6',
    '见 https://43-165-128-180.sslip.io/#task=T-6',
    '见 [T-6](https://43-165-128-180.sslip.io/#task=T-6)',
    '见 [随便写的字](#task=T-6)',
  ];
  for (const f of forms) {
    const out = richText(f, opts);
    assert.match(out, /class="lnk-task" data-task="T-6"/, f + ' => ' + out);
    assert.match(out, /class="lt-title">输入框重合</, f);
    assert.ok(!out.includes(']('), '不该漏出 Markdown 原文: ' + f);
    assert.ok(!out.includes('href="https://43-165-128-180'), '看板卡链接不该当普通外链: ' + f);
  }
});

test('指向别处的链接照旧是普通锚点，不被收成胶囊', () => {
  const out = richText('看 [文档](https://example.com/doc) 和 https://example.com/#other');
  assert.ok(!out.includes('lnk-task'), out);
  assert.match(out, /href="https:\/\/example\.com\/doc"/);
  assert.match(out, /href="https:\/\/example\.com\/#other"/);
});

test('句末标点不并进链接', () => {
  const out = richText('去 https://example.com/x。然后呢');
  assert.match(out, /href="https:\/\/example\.com\/x"/);
  assert.match(out, /<\/a>。然后呢/);
});

test('链接里的成对括号保留，句末括号去掉', () => {
  assert.match(richText('https://en.wikipedia.org/wiki/A_(b)'), /href="https:\/\/en\.wikipedia\.org\/wiki\/A_\(b\)"/);
  assert.match(richText('（见 https://example.com/x）'), /<\/a>）/);
});

test('HTML 一律转义，不给注入留口子', () => {
  const out = richText('<img src=x onerror=alert(1)> & "引号"');
  assert.ok(!out.includes('<img'));
  assert.match(out, /&lt;img src=x onerror=alert\(1\)&gt; &amp; &quot;引号&quot;/);
});

test('javascript: 之类的伪协议不会被当成链接', () => {
  const out = richText('javascript:alert(1) 和 data:text/html,<b>x</b>');
  assert.ok(!out.includes('<a '), out);
});

test('只有那两种形态能进 href，引号进不了属性', () => {
  // 链接后面紧跟引号时，引号属于正文。
  const out = richText('https://example.com/x" onclick="alert(1)');
  assert.ok(!out.includes('onclick="'), out);
  assert.match(out, /&quot; onclick=&quot;alert\(1\)/);
});

test('卡片摘要按段截断，不把链接切成半截', () => {
  const long = '前言 '.repeat(10) + 'https://example.com/very/long/path';
  const out = richPreview(long, 24);
  assert.ok(!out.includes('href="https://example.com/very'), out);
  assert.ok(out.endsWith('…'), out);
  const short = richPreview('见 https://example.com/x', 40);
  assert.match(short, /href="https:\/\/example\.com\/x"/);
  assert.ok(!short.endsWith('…'), short);
});

test('纯文本没有链接时原样转义输出', () => {
  const out = richText('没有任何链接 & <标签>');
  assert.equal(out, '没有任何链接 &amp; &lt;标签&gt;');
  assert.deepEqual(textSegments('abc'), [{ text: 'abc' }]);
});
