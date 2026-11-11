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
	if !strings.Contains(body, `aria-pressed="false" hidden`) {
		t.Fatal("push toggle should start hidden; JS reveals it only in a secure context")
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
