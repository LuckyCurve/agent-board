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

test('#task=T-N 也渲染成锚点', () => {
  const out = richText('详见 #task=T-12');
  assert.match(out, /href="#task=T-12"/);
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
