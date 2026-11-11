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
