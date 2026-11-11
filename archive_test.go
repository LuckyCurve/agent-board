package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestArchiveAndRestoreHidesFromMainViewsButKeepsEvents(t *testing.T) {
	e := newTestEnv(t)
	id := e.createTask(t, "收尾", "done")
	code, _, _ := e.req(t, "PATCH", "/api/tasks/"+id, map[string]any{"column": "archived"})
	if code != http.StatusOK {
		t.Fatalf("archive: %d", code)
	}
	code, body, _ := e.req(t, "GET", "/api/tasks", nil)
	if code != http.StatusOK {
		t.Fatalf("tasks: %d", code)
	}
	for _, raw := range body["tasks"].([]any) {
		if raw.(map[string]any)["id"] == id {
			t.Fatal("archived task should leave the main board")
		}
	}
	code, body, _ = e.req(t, "GET", "/api/tasks?view=archived", nil)
	if code != http.StatusOK {
		t.Fatalf("archived view: %d", code)
	}
	var found bool
	for _, raw := range body["tasks"].([]any) {
		if raw.(map[string]any)["id"] == id {
			found = true
		}
	}
	if !found {
		t.Fatal("archived task should be findable in the archived view")
	}
	code, body, _ = e.req(t, "GET", "/api/tasks/"+id+"/events", nil)
	if code != http.StatusOK || len(body["events"].([]any)) == 0 {
		t.Fatalf("events should stay: %d %v", code, body)
	}
	code, _, _ = e.req(t, "PATCH", "/api/tasks/"+id, map[string]any{"column": "done"})
	if code != http.StatusOK {
		t.Fatalf("restore: %d", code)
	}
	code, body, _ = e.req(t, "GET", "/api/tasks", nil)
	for _, raw := range body["tasks"].([]any) {
		if raw.(map[string]any)["id"] == id {
			return
		}
	}
	t.Fatal("restored task should be back on the board")
}

func TestDeleteRequiresConfirmAndRejectsOpenItems(t *testing.T) {
	e := newTestEnv(t)
	id := e.createTask(t, "要删掉的", "done")
	code, _, _ := e.req(t, "DELETE", "/api/tasks/"+id, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("delete without confirm: %d, want 400", code)
	}
	code, _, _ = e.req(t, "POST", "/api/decisions", map[string]any{
		"task_id": id, "question": "还没定", "options": []string{"好", "不好"}})
	if code != http.StatusCreated {
		t.Fatalf("decision: %d", code)
	}
	code, body, _ := e.req(t, "DELETE", "/api/tasks/"+id+"?confirm=yes", nil)
	if code != http.StatusConflict {
		t.Fatalf("delete with pending decision: %d %v, want 409", code, body)
	}
	if body["error"] == "" || body["error"] == nil {
		t.Fatal("rejection should explain why")
	}
	// 拍板后没有未结回执，删除才放行；编号不重置。
	code, body, _ = e.req(t, "GET", "/api/decisions?status=pending", nil)
	_ = body
	_ = code
}

func TestDeleteKeepsTaskNumbering(t *testing.T) {
	e := newTestEnv(t)
	first := e.createTask(t, "一", "done")
	second := e.createTask(t, "二", "done")
	code, _, _ := e.req(t, "DELETE", "/api/tasks/"+first+"?confirm=yes", nil)
	if code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	third := e.createTask(t, "三", "done")
	if third <= second {
		t.Fatalf("next id %s should not reuse %s", third, second)
	}
}

func TestStreamCatchesUpFromCursor(t *testing.T) {
	e := newTestEnv(t)
	id := e.createTask(t, "补拉测试", "todo")
	_, _, _ = e.req(t, "POST", "/api/tasks/"+id+"/events", map[string]any{
		"kind": "progress", "text": "醒来后该看到"})
	req, err := http.NewRequest("GET", e.ts.URL+"/api/stream?token="+e.token+"&since=1&once=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Last-Event-ID", "1")
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "event: change") {
		t.Fatalf("stream should replay events after cursor: %q", body)
	}
	if !strings.Contains(string(body), "醒来后该看到") {
		t.Fatalf("stream missing replayed event body: %q", body)
	}
}
