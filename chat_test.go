package main

import (
	"net/http"
	"testing"
)

func TestChatMessageLinksTaskWithoutTouchingTimeline(t *testing.T) {
	e := newTestEnv(t)
	id := e.createTask(t, "导出改成流式", "doing")
	_, before, _ := e.req(t, "GET", "/api/tasks/"+id, nil)
	beforeSeq := int64(before["task"].(map[string]any)["last_seq"].(float64))

	code, body, _ := e.req(t, "POST", "/api/message", map[string]any{
		"text": "这个任务现在怎样了", "from": "user", "task_id": id,
	})
	if code != http.StatusCreated {
		t.Fatalf("chat message: %d %v", code, body)
	}
	ev := body["event"].(map[string]any)
	if ev["chat"] != true {
		t.Fatalf("event.chat = %v, want true", ev["chat"])
	}
	if ev["link_task"] != id {
		t.Fatalf("event.link_task = %v, want %s", ev["link_task"], id)
	}

	// 聊天不该把一句话塞成卡片的最新进展。
	_, after, _ := e.req(t, "GET", "/api/tasks/"+id, nil)
	afterSeq := int64(after["task"].(map[string]any)["last_seq"].(float64))
	if afterSeq != beforeSeq {
		t.Fatalf("chat message bumped task last_seq %d -> %d", beforeSeq, afterSeq)
	}

	code, body, _ = e.req(t, "GET", "/api/chat?recent=20", nil)
	if code != http.StatusOK {
		t.Fatalf("chat list: %d %v", code, body)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	m := msgs[0].(map[string]any)
	if m["link_task"] != id || m["body"] != "这个任务现在怎样了" || m["author"] != "user" {
		t.Fatalf("chat message = %v", m)
	}
}

func TestChatRejectsUnknownTaskAndRequiresToken(t *testing.T) {
	e := newTestEnv(t)
	code, _, _ := e.req(t, "POST", "/api/message", map[string]any{
		"text": "x", "task_id": "T-404",
	})
	if code != http.StatusNotFound {
		t.Fatalf("unknown task: %d, want 404", code)
	}
	code = doWithToken(t, e.client, "GET", e.ts.URL+"/api/chat", nil, "")
	if code != http.StatusUnauthorized {
		t.Fatalf("anonymous chat: %d, want 401", code)
	}
}

func TestChatKeepsChronologicalOrderAndLimit(t *testing.T) {
	e := newTestEnv(t)
	for _, text := range []string{"一", "二", "三"} {
		code, _, _ := e.req(t, "POST", "/api/message", map[string]any{"text": text})
		if code != http.StatusCreated {
			t.Fatalf("send %s: %d", text, code)
		}
	}
	code, body, _ := e.req(t, "GET", "/api/chat?recent=2", nil)
	if code != http.StatusOK {
		t.Fatalf("chat: %d %v", code, body)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("limit not applied: %d", len(msgs))
	}
	// 接口按新到旧返回，前端再反转成 IM 顺序。
	if msgs[0].(map[string]any)["body"] != "三" || msgs[1].(map[string]any)["body"] != "二" {
		t.Fatalf("chat order = %v", msgs)
	}
}
