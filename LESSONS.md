# 教训

| 现象 | 根因 | 现在靠什么守住 |
|---|---|---|
| 深色主题不生效 | Radix 亮色文件里没有 dark 变量，得单独取 `*-dark.css` | `web/radix.css` 里深浅两套都并入；截图核对 `body` 背景色 |
| 关闭的详情面板还浮在页面上 | `dialog.sheet { display:flex }` 压过了浏览器默认的 `dialog:not([open]){display:none}` | `dialog.sheet:not([open]) { display:none; }` |
| 无头 Chrome 截不出 375px | Chrome 窗口有约 500px 下限，`--window-size` 无效 | CDP `Emulation.setDeviceMetricsOverride` 精确设宽；脚本同时断言 `scrollWidth == innerWidth` |
| 截图命令一直不结束 | 页面 SSE 长连接让无头 Chrome 认为还在加载 | 截图页加 `?static=1`，跳过 SSE 和 Service Worker |
| 改了 CSS/JS 看不到效果 | 旧 Chrome profile + Service Worker 缓存 | 静态资源 `Cache-Control: no-cache`；截图脚本禁缓存、每次清 profile |
| 不能编译 SQLite 驱动 | 本机没有 gcc | 用纯 Go 驱动 `modernc.org/sqlite` |
