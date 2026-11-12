# 教训

| 现象 | 根因 | 现在靠什么守住 |
|---|---|---|
| 深色主题不生效 | Radix 亮色文件里没有 dark 变量，得单独取 `*-dark.css` | `web/radix.css` 里深浅两套都并入；截图核对 `body` 背景色 |
| 关闭的详情面板还浮在页面上 | `dialog.sheet { display:flex }` 压过了浏览器默认的 `dialog:not([open]){display:none}` | `dialog.sheet:not([open]) { display:none; }` |
| 无头 Chrome 截不出 375px | Chrome 窗口有约 500px 下限，`--window-size` 无效 | CDP `Emulation.setDeviceMetricsOverride` 精确设宽；脚本同时断言 `scrollWidth == innerWidth` |
| 截图命令一直不结束 | 页面 SSE 长连接让无头 Chrome 认为还在加载 | 截图页加 `?static=1`，跳过 SSE 和 Service Worker |
| 改了 CSS/JS 看不到效果 | 旧 Chrome profile + Service Worker 缓存 | 静态资源 `Cache-Control: no-cache`；截图脚本禁缓存、每次清 profile |
| 不能编译 SQLite 驱动 | 本机没有 gcc | 用纯 Go 驱动 `modernc.org/sqlite` |
| 前端漏一个变量声明就上线，用户直接发不出消息 | 整段重写 web/app.js 时把 `let uploadingImages` 一起带没了 | CI 跑 `node --check` + eslint `no-undef`/`no-unused-vars`（web/ 有 eslint.config.mjs），漏声明直接红 |
| 推送在 Chrome/iOS 上收不到 | webpush-go v1.4.0 用的是旧版加密格式（盐塞在正文里），现代推送服务只认 RFC 8291 | 服务端自己按 RFC 8291 加密：盐进 `Encryption` 头、服务端公钥进 `Crypto-Key` 头、正文只留密文；`push_chain_test.go` 按浏览器的算法解密验原文 |
| 推送在 iPhone 上收不到，Chrome 上却正常 | VAPID 的 `sub` 写死成 `mailto:...@localhost`，Apple 直接拒整个 JWT（403 BadJwtToken）；Chrome 不挑这个 | `sub` 改成 `VAPID_SUB` / `--vapid-sub` 可配，默认按站点域名推一个；`push_vapid_test.go` 锁住「只用 mailto:/https:、不含 localhost」 |
| 推送失败时日志里一行都没有 | 非 404/410 的失败只做了计数，没写日志 | 失败一律 `log.Printf` 状态码 + 响应正文（长串打码、endpoint 只留主机名） |
| 同时带令牌和网页会话时，绑定令牌能越权看全量 | 判断「是不是主会话」时先看 cookie 再看令牌，绑定令牌随手带上浏览器登录态就升级了 | `isMainCaller` 只要请求带了 Bearer 就按令牌判权限，cookie 只在没带令牌时才算；`security_test.go` 覆盖 |
| 改成 cookie 鉴权后，一批「匿名 401」测试全变 200 | 那些测试的 client 自带登录 cookie，已经不算匿名 | 匿名断言一律用 `noRedirectClient()`，别再拿 `e.client` 当匿名 |
