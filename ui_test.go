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
	rule := css[start:strings.Index(css[start:], "}")+start]
	for _, want := range []string{"display: flex", "flex-direction: column", "height: calc(100dvh"} {
		if !strings.Contains(rule, want) {
			t.Fatalf("对话页应由 body 纵向 flex 撑满：缺 %q\n%s", want, rule)
		}
	}
	if strings.Contains(css, "height: calc(100dvh - var(--topbar-h)") {
		t.Fatal("对话页高度别再手减顶栏，筛选项会把输入框推到导航底下")
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

func TestDetailTitleIsFocusable(t *testing.T) {
	e := newTestEnv(t)
	body := getPageBody(t, e, "/")
	if !strings.Contains(body, `id="d-title" tabindex="-1"`) {
		t.Fatal("detail title should be programmatically focusable")
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
