package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func getPageBody(t *testing.T, e *testEnv, path string) string {
	t.Helper()
	resp, err := e.client.Get(e.ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d", path, resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestChatIsTheFirstTab(t *testing.T) {
	e := newTestEnv(t)
	body := getPageBody(t, e, "/")
	chat := strings.Index(body, `data-view="inbox">对话`)
	board := strings.Index(body, `data-view="board">看板`)
	if chat < 0 || board < 0 {
		t.Fatalf("tabs missing: chat=%d board=%d", chat, board)
	}
	if chat > board {
		t.Fatal("对话 should be the first tab, before 看板")
	}
	if !strings.Contains(body, "<h1>对话</h1>") {
		t.Fatal("chat page should be titled 对话")
	}
}

func TestInboxHasPinnedPendingBar(t *testing.T) {
	e := newTestEnv(t)
	body := getPageBody(t, e, "/")
	bar := strings.Index(body, `id="pendingBar"`)
	inbox := strings.Index(body, `class="inbox" id="inbox"`)
	if bar < 0 {
		t.Fatal("inbox should have a pending bar")
	}
	if inbox < 0 || bar > inbox {
		t.Fatal("pending bar should sit above the scrollable inbox, so it stays visible")
	}
	if !strings.Contains(body, "等你决定") {
		t.Fatal("pending bar should say 等你决定")
	}
	if !strings.Contains(body, `class="pending-bar" id="pendingBar" hidden`) {
		t.Fatal("pending bar should start hidden until there is something to decide")
	}
}

func TestPushButtonHiddenUntilSupported(t *testing.T) {
	e := newTestEnv(t)
	body := getPageBody(t, e, "/")
	if !strings.Contains(body, `id="pushToggle"`) {
		t.Fatal("push toggle missing")
	}
	// JS 只在安全上下文里把它露出来（通知需要 HTTPS / localhost）。
	js := getPageBody(t, e, "/app.js")
	if !strings.Contains(js, "btn.hidden = !pushSupported()") {
		t.Fatal("push toggle should start hidden; JS reveals it only in a secure context")
	}
}

// 对话页高度靠 flex 分配，不能手算「视口减顶栏」——筛选项出现后输入框会被底栏盖住。
func TestInboxHeightIsFlexNotCalc(t *testing.T) {
	e := newTestEnv(t)
	css := getPageBody(t, e, "/app.css")
	start := strings.Index(css, `body[data-view="inbox"] {`)
	if start < 0 {
		t.Fatal("app.css 缺少对话页布局规则")
	}
	rule := css[start : strings.Index(css[start:], "}")+start]
	for _, want := range []string{"display: flex", "flex-direction: column", "height: calc(100dvh"} {
		if !strings.Contains(rule, want) {
			t.Fatalf("对话页应由 body 纵向 flex 撑满：缺 %q\n%s", want, rule)
		}
	}
	if strings.Contains(css, "height: calc(100dvh - var(--topbar-h)") {
		t.Fatal("对话页高度别再手减顶栏，筛选项会把输入框推到导航底下")
	}
}

// 对话页的 .main 在纵向 flex 里必须显式给满宽。
// 它有 margin: 0 auto，flex 里会被当成「不拉伸」，宽度退化成内容的 min-content：
// 长消息里的 .mq-title（white-space: nowrap）把整页撑到 496，右对齐气泡被挤出屏幕。
func TestInboxMainFillsWidth(t *testing.T) {
	e := newTestEnv(t)
	css := getPageBody(t, e, "/app.css")
	start := strings.Index(css, `body[data-view="inbox"] .main {`)
	if start < 0 {
		t.Fatal("app.css 缺少对话页 .main 规则")
	}
	rule := css[start : strings.Index(css[start:], "}")+start]
	if !strings.Contains(rule, "width: 100%") {
		t.Fatalf("对话页 .main 必须 width: 100%%，否则 margin:auto 让它按 min-content 收缩\n%s", rule)
	}
	if !strings.Contains(rule, "overflow: hidden") {
		t.Fatalf("对话页 .main 需要裁掉残余横向溢出\n%s", rule)
	}
}

// 桌面两侧留白也得能滚：滚动容器必须占满整宽，收窄交给里面的内容。
func TestInboxScrollAreaSpansFullWidth(t *testing.T) {
	e := newTestEnv(t)
	css := getPageBody(t, e, "/app.css")
	start := strings.Index(css, `body[data-view="inbox"] .inbox {`)
	if start < 0 {
		t.Fatal("app.css 缺少对话页 .inbox 规则")
	}
	rule := css[start : strings.Index(css[start:], "}")+start]
	if strings.Contains(rule, "max-width: 780px") {
		t.Fatalf("对话页 .inbox 不能自己收窄，否则两侧留白不属于滚动区，滚轮在上面没反应\n%s", rule)
	}
	if !strings.Contains(rule, "max-width: none") {
		t.Fatalf("对话页 .inbox 必须占满整宽\n%s", rule)
	}
	// 内容本身仍收窄居中。
	if !strings.Contains(css, "body[data-view=\"inbox\"] .inbox-section") ||
		!strings.Contains(css, "max-width: 780px") {
		t.Fatal("对话页内容仍应收窄到 780 并居中")
	}
}

// 顶栏只留一个「设置」入口；测试消息、立即更新这类低频动作不再占位置。
func TestTopbarHasSingleSettingsEntry(t *testing.T) {
	e := newTestEnv(t)
	body := getPageBody(t, e, "/")
	if !strings.Contains(body, `id="settingsBtn"`) || !strings.Contains(body, `id="settingsMenu"`) {
		t.Fatal("顶栏应该只有一个「设置」入口，点开是下拉菜单")
	}
	for _, gone := range []string{`id="pushTest"`, `id="deployBtn"`, "发一条测试通知", "立即更新"} {
		if strings.Contains(body, gone) {
			t.Fatalf("顶栏不该再有 %s", gone)
		}
	}
	for _, want := range []string{`id="pushToggle"`, `id="themeToggle"`, `id="logoutBtn"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("设置下拉里应该有 %s", want)
		}
	}
	// 下拉默认收起，点开才展开。
	if !strings.Contains(body, `id="settingsMenu" hidden`) {
		t.Fatal("设置下拉默认应该是收起的")
	}
}

// Agent 切换器常驻筛选行，不再在顶栏和筛选行之间来回搬家。
func TestAgentSwitchLivesInFilterBar(t *testing.T) {
	e := newTestEnv(t)
	body := getPageBody(t, e, "/")
	bar := strings.Index(body, `id="filterBar"`)
	sel := strings.Index(body, `id="agentSwitch"`)
	if bar < 0 || sel < bar {
		t.Fatal("Agent 切换器应该固定在筛选行里")
	}
	js := getPageBody(t, e, "/app.js")
	if strings.Contains(js, "placeAgentSwitch") {
		t.Fatal("切换器不该再按屏宽搬家")
	}
}

// 切换器只列每台机器的主 Agent，并显示那台机器的信息。
func TestAgentSwitchShowsHostsOnly(t *testing.T) {
	e := newTestEnv(t)
	body := getPageBody(t, e, "/")
	if !strings.Contains(body, `id="agentHost"`) {
		t.Fatal("切换器旁要有主机信息的位置")
	}
	js := getPageBody(t, e, "/app.js")
	for _, want := range []string{"switchAgents()", "state.hosts[a]", "renderAgentHost()", "hostLabel(a)"} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js 缺 %s（切换器要按主机收窄并显示机器信息）", want)
		}
	}
	css := getPageBody(t, e, "/app.css")
	if !strings.Contains(css, ".agent-host") {
		t.Fatal("app.css 缺主机信息样式")
	}
}

// 消息正文、卡片事件、卡片摘要里的链接都要渲染成可点锚点。
func TestBodyLinksAreAnchors(t *testing.T) {
	e := newTestEnv(t)
	body := getPageBody(t, e, "/")
	li := strings.Index(body, `src="/linkify.js"`)
	app := strings.Index(body, `src="/app.js"`)
	if li < 0 || app < 0 || li > app {
		t.Fatal("linkify.js 要先于 app.js 加载")
	}
	js := getPageBody(t, e, "/app.js")
	for _, want := range []string{"richText(m.body, linkOpts)", "richText(e.body, linkOpts)", "richPreview(lastTextOf(last), 96, linkOpts)"} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js 里 %s 没走链接渲染", want)
		}
	}
	lib := getPageBody(t, e, "/linkify.js")
	for _, want := range []string{`target="_blank"`, "noopener noreferrer", "escHTML", "lnk-task"} {
		if !strings.Contains(lib, want) {
			t.Fatalf("linkify.js 缺 %s", want)
		}
	}
}

// #task=T-N 渲染成胶囊，并且点得开卡片。
func TestTaskRefsRenderAsCapsules(t *testing.T) {
	e := newTestEnv(t)
	lib := getPageBody(t, e, "/linkify.js")
	for _, want := range []string{"#task=(T-\\d+)", "lnk-task", "lt-id", "lt-title"} {
		if !strings.Contains(lib, want) {
			t.Fatalf("linkify.js 缺 %s", want)
		}
	}
	// Markdown 链接也要认，不能原样当文本。
	if !strings.Contains(lib, `\[([^\]\n]{1,120})\]`) {
		t.Fatal("linkify.js 没识别 [文字](url)")
	}
	js := getPageBody(t, e, "/app.js")
	if !strings.Contains(js, "closest('.lnk-task')") {
		t.Fatal("app.js 没接住胶囊点击")
	}
	css := getPageBody(t, e, "/app.css")
	for _, want := range []string{".lnk-task", ".lt-id", ".lt-title"} {
		if !strings.Contains(css, want) {
			t.Fatalf("app.css 缺胶囊样式 %s", want)
		}
	}
}

// 窄屏筛选收成一个按钮；摊开会把一行挤爆，375 就放不下。
func TestFilterCollapsesOnMobile(t *testing.T) {
	e := newTestEnv(t)
	body := getPageBody(t, e, "/")
	for _, want := range []string{`id="filterToggle"`, `id="filterPanel"`, `id="filterBadge"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("筛选行缺少 %s", want)
		}
	}
	css := getPageBody(t, e, "/app.css")
	start := strings.Index(css, ".fb-toggle { display: none; }")
	if start < 0 {
		t.Fatal("宽屏不该显示筛选按钮")
	}
	mobile := strings.Index(css, "@media (max-width: 899px)")
	if mobile < 0 || !strings.Contains(css[mobile:], ".fb-panel.open { display: flex; }") {
		t.Fatal("窄屏筛选要能点开")
	}
	// 手机上滚动条是浮层，不该再留槽位，否则 320 宽会被挤出横向滚动。
	if !strings.Contains(css, ".inbox-scroll { scrollbar-gutter: auto; }") {
		t.Fatal("窄屏不该给滚动条留槽")
	}
}

func TestDetailTitleIsFocusable(t *testing.T) {
	e := newTestEnv(t)
	body := getPageBody(t, e, "/")
	if !strings.Contains(body, `id="d-title" tabindex="-1"`) {
		t.Fatal("detail title should be programmatically focusable")
	}
}

// 点胶囊进详情时标题不能自带焦点环（看着像被选中）；键盘进来仍要能聚焦。
func TestDetailFocusDependsOnInputKind(t *testing.T) {
	e := newTestEnv(t)
	js := getPageBody(t, e, "/app.js")
	for _, want := range []string{
		"lastInputKind",                // 记住这次是键盘还是指针
		"lastInputKind === 'keyboard'", // 只有键盘才把焦点放到标题
		"sel.removeAllRanges()",        // 打开时清掉双击留下的选区
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js 缺 %s", want)
		}
	}
	// 正文不能被 user-select:none 一刀切——那会连复制一起废掉。
	css := getPageBody(t, e, "/app.css")
	for _, sel := range []string{".detail-title", ".msg-text", ".tl-body"} {
		i := strings.Index(css, sel+" {")
		if i < 0 {
			continue
		}
		block := css[i : strings.Index(css[i:], "}")+i]
		if strings.Contains(block, "user-select: none") {
			t.Fatalf("%s 不能用 user-select:none，复制会一起废掉", sel)
		}
	}
}

func TestCardsHaveNoComposerAndCanBeQuoted(t *testing.T) {
	e := newTestEnv(t)
	body := getPageBody(t, e, "/")
	for _, bad := range []string{"composeInput", "composeSend", `id="composeCtx"`} {
		if strings.Contains(body, bad) {
			t.Fatalf("card detail should not host a chat composer (%s)", bad)
		}
	}
	if !strings.Contains(body, `id="dQuoteChat"`) {
		t.Fatal("card detail should offer 回到对话并引用")
	}
	js := getPageBody(t, e, "/app.js")
	if !strings.Contains(js, `data-quote=`) {
		t.Fatal("board cards should offer a 引用 button that jumps into chat")
	}
}

func TestComposerHasPlusMenuAndActiveTask(t *testing.T) {
	e := newTestEnv(t)
	js := getPageBody(t, e, "/app.js")
	for _, want := range []string{`id="plusBtn"`, `id="plusMenu"`, `id="activeTaskBar"`} {
		if !strings.Contains(js, want) {
			t.Fatalf("composer missing %s", want)
		}
	}
	if strings.Contains(js, `id="attachBtn"`) || strings.Contains(js, `id="camBtn"`) {
		t.Fatal("the composer should have one + button, not separate image buttons")
	}
}

// 顶栏有 Agent 切换器，下面有多选标签行；两者都要记住选择。
func TestFilterBarHasAgentSwitchAndTags(t *testing.T) {
	e := newTestEnv(t)
	body := getPageBody(t, e, "/")
	for _, want := range []string{`id="agentSwitch"`, `id="tagRow"`, `id="clearFilter"`, `id="filterBar"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("缺少 %s", want)
		}
	}
	js := getPageBody(t, e, "/app.js")
	for _, want := range []string{"ab-agent", "ab-tags", "toggleTag", "matchTask"} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js 缺少 %s（筛选要记住选择）", want)
		}
	}
}

// 卡片详情里能加标签、能转交。
func TestDetailHasTagsAndTransfer(t *testing.T) {
	e := newTestEnv(t)
	js := getPageBody(t, e, "/app.js")
	for _, want := range []string{"/tags", "dTagAdd", "dAssignBtn", "转交"} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js 缺少 %s", want)
		}
	}
}

// 标签筛选只在看板页生效：归档、对话不按标签收窄，筛选条也只在该页出现。
func TestTagFilterIsBoardOnly(t *testing.T) {
	e := newTestEnv(t)
	js := getPageBody(t, e, "/app.js")
	if strings.Contains(js, "all.filter(matchTask)") {
		t.Fatal("归档页不该再按标签筛选，否则归档里的卡会凭空消失")
	}
	if !strings.Contains(js, "const msgs = state.chat.slice().reverse();") {
		t.Fatal("对话页不该按标签筛消息，聊天上下文要完整")
	}
	if !strings.Contains(js, "function syncFilterBar()") ||
		!strings.Contains(js, "bar.hidden = state.view !== 'board'") {
		t.Fatal("筛选条只该在看板页出现")
	}
	if !strings.Contains(js, "syncFilterBar();") {
		t.Fatal("切页时要同步筛选条的可见性")
	}
}

// 桌面端要有能摸到的归档入口：已完成的卡上直接给按钮，看板顶部有条归档栏。
func TestDoneCardsOfferArchiveOnDesktop(t *testing.T) {
	e := newTestEnv(t)
	body := getPageBody(t, e, "/")
	if !strings.Contains(body, `id="boardArchive"`) {
		t.Fatal("看板顶部要有归档入口")
	}
	js := getPageBody(t, e, "/app.js")
	if !strings.Contains(js, `data-archive="${esc(t.id)}"`) ||
		!strings.Contains(js, "t.column === 'done'") {
		t.Fatal("已完成的卡上要有直接归档的按钮")
	}
	if !strings.Contains(js, "const COLUMNS = [") || strings.Contains(js, "key: 'archived'") {
		t.Fatal("归档不该再占看板一列——宽屏下会折到第二行，等于没有入口")
	}
	css := getPageBody(t, e, "/app.css")
	for _, want := range []string{".board-bar", ".bb-archive", ".card-archive"} {
		if !strings.Contains(css, want) {
			t.Fatalf("app.css 缺 %s", want)
		}
	}
}
func TestMessageBubblesShowDeliveryState(t *testing.T) {
	e := newTestEnv(t)
	js := getPageBody(t, e, "/app.js")
	for _, want := range []string{
		"function outboxHTML",    // 乐观气泡：先上屏再等回执
		"function msgStateHTML",  // 每条消息的状态图标
		"function deliverOutbox", // 发送 + 失败留痕
		"function retryOutbox",   // 点气泡重发
		"data-outbox=",
		"key: entry.key", // 幂等键，重发不会发成两条
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js 缺 %s", want)
		}
	}
	css := getPageBody(t, e, "/app.css")
	for _, want := range []string{
		".msg-state.sending", ".msg-state.failed",
		".msg-state.unread", ".msg-state.read", ".msg-state.done",
		".msg-item.outbox.failed",
	} {
		if !strings.Contains(css, want) {
			t.Fatalf("app.css 缺 %s", want)
		}
	}
}

func TestGrainLayerScrollsWithContent(t *testing.T) {
	e := newTestEnv(t)
	css := getPageBody(t, e, "/app.css")
	start := strings.Index(css, "\nbody {")
	if start < 0 {
		t.Fatal("app.css 找不到 body 规则")
	}
	start++ // 跳过换行，从 body 本身开始
	rule := css[start : strings.Index(css[start:], "}")+start]
	if strings.Contains(rule, "background-attachment: fixed;") {
		t.Fatalf("body 的背景层不能整层 fixed，噪点会每帧重画导致闪烁\n%s", rule)
	}
	if !strings.Contains(rule, "background-attachment: fixed, fixed, scroll;") {
		t.Fatalf("body 的三层背景应为「渐变 fixed、噪点 scroll」\n%s", rule)
	}
}
