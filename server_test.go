package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"
)

var testPassHash = hashPassword("test-pw")

type testEnv struct {
	store  *Store
	srv    *Server
	ts     *httptest.Server
	client *http.Client
}

func newTestServer(t *testing.T, cfg Config, passHash string) *testEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := OpenStore(filepath.Join(dir, "board.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:0"
	}
	s := NewServer(st, cfg, passHash)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &testEnv{store: st, srv: s, ts: ts}
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newTestServer(t, Config{Version: "test"}, testPassHash)
	e.client = loginWithJar(t, e.ts.URL, "test-pw")
	return e
}

func (e *testEnv) req(t *testing.T, method, path string, body any) (int, map[string]any, *http.Response) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out, resp
}

func (e *testEnv) createTask(t *testing.T, title, column string) string {
	t.Helper()
	code, body, _ := e.req(t, "POST", "/api/tasks", map[string]any{
		"title": title, "project": "p", "agent": "a1", "column": column})
	if code != http.StatusCreated {
		t.Fatalf("create task: %d %v", code, body)
	}
	return body["task"].(map[string]any)["id"].(string)
}

func (e *testEnv) addEvent(t *testing.T, taskID, kind, text, key string) map[string]any {
	t.Helper()
	code, body, _ := e.req(t, "POST", "/api/tasks/"+taskID+"/events", map[string]any{
		"kind": kind, "text": text, "key": key})
	if code != http.StatusCreated {
		t.Fatalf("add event: %d %v", code, body)
	}
	return body["event"].(map[string]any)
}

func TestTaskEventAndIdempotency(t *testing.T) {
	e := newTestEnv(t)
	id := e.createTask(t, "把导出层改成流式写入", "doing")
	if id != "T-1" {
		t.Fatalf("first task id = %q", id)
	}
	ev := e.addEvent(t, id, "progress", "单测全过", "k-1")
	again := e.addEvent(t, id, "progress", "单测全过", "k-1")
	if ev["seq"] != again["seq"] {
		t.Fatalf("idempotent seq mismatch: %v vs %v", ev["seq"], again["seq"])
	}
	e.addEvent(t, id, "blocked", "等权限", "")

	code, body, _ := e.req(t, "GET", "/api/tasks/"+id, nil)
	if code != http.StatusOK {
		t.Fatalf("get task: %d", code)
	}
	events := body["events"].([]any)
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3 (create, progress, blocked)", len(events))
	}
	task := body["task"].(map[string]any)
	if task["column"] != "doing" || task["agent"] != "a1" {
		t.Fatalf("task fields: %v", task)
	}
	last := task["last_event"].(map[string]any)
	if last["body"] != "等权限" {
		t.Fatalf("last event = %v", last["body"])
	}

	code, _, _ = e.req(t, "POST", "/api/tasks/T-404/events", map[string]any{"text": "x"})
	if code != http.StatusNotFound {
		t.Fatalf("missing task: got %d want 404", code)
	}
}

func TestReplyLifecycleAndAck(t *testing.T) {
	e := newTestEnv(t)
	id := e.createTask(t, "回测缺口报告", "doing")
	orig := e.addEvent(t, id, "progress", "缺口 37 天", "")

	code, body, _ := e.req(t, "POST", "/api/tasks/"+id+"/events", map[string]any{
		"kind": "user_reply", "text": "先跑一版", "data": fmt.Sprintf(`{"reply_to_seq":%v}`, orig["seq"])})
	if code != http.StatusCreated {
		t.Fatalf("reply: %d %v", code, body)
	}
	seq := int64(body["event"].(map[string]any)["seq"].(float64))

	code, body, _ = e.req(t, "GET", "/api/replies?recent=10", nil)
	if code != http.StatusOK {
		t.Fatalf("replies: %d", code)
	}
	rs := body["replies"].([]any)
	if len(rs) != 1 || rs[0].(map[string]any)["status"] != "received" {
		t.Fatalf("replies = %v", rs)
	}

	code, body, _ = e.req(t, "PATCH", fmt.Sprintf("/api/replies/%d", seq), map[string]any{
		"status": "processing", "agent": "a1"})
	if code != http.StatusOK || body["reply"].(map[string]any)["status"] != "processing" {
		t.Fatalf("take: %d %v", code, body)
	}

	code, _, _ = e.req(t, "POST", "/api/ack", map[string]any{"consumer": "main", "seq": seq})
	if code != http.StatusOK {
		t.Fatalf("ack: %d", code)
	}
	rp, err := e.store.ReplyBySeq(seq)
	if err != nil || rp.Status != "done" {
		t.Fatalf("reply after ack = %+v, %v", rp, err)
	}

	code, _, _ = e.req(t, "PATCH", fmt.Sprintf("/api/replies/%d", seq), map[string]any{"status": "bogus"})
	if code != http.StatusBadRequest {
		t.Fatalf("bad status: got %d want 400", code)
	}
}

func TestDecisionFlow(t *testing.T) {
	e := newTestEnv(t)
	id := e.createTask(t, "skills 索引重构", "blocked")
	code, body, _ := e.req(t, "POST", "/api/decisions", map[string]any{
		"task_id": id, "question": "按项目拆还是合并？", "options": []string{"按项目拆", "合并"}})
	if code != http.StatusCreated {
		t.Fatalf("decision: %d %v", code, body)
	}
	did := body["decision"].(map[string]any)["id"].(string)

	code, body, _ = e.req(t, "GET", "/api/decisions?status=pending", nil)
	if code != http.StatusOK || len(body["decisions"].([]any)) != 1 {
		t.Fatalf("pending: %d %v", code, body)
	}
	code, _, _ = e.req(t, "GET", "/api/tasks", nil)
	if code != http.StatusOK {
		t.Fatal(code)
	}

	code, body, _ = e.req(t, "POST", "/api/decisions/"+did+"/answer", map[string]any{"answer": "按项目拆"})
	if code != http.StatusOK || body["decision"].(map[string]any)["status"] != "answered" {
		t.Fatalf("answer: %d %v", code, body)
	}
	code, _, _ = e.req(t, "POST", "/api/decisions/"+did+"/answer", map[string]any{"answer": "再答一次"})
	if code != http.StatusBadRequest {
		t.Fatalf("double answer: got %d want 400", code)
	}

	// 拍板也生成一条回执，主会话能看到并派活
	code, body, _ = e.req(t, "GET", "/api/replies?recent=10", nil)
	if code != http.StatusOK {
		t.Fatal(code)
	}
	rs := body["replies"].([]any)
	if len(rs) != 1 || rs[0].(map[string]any)["text"] != "按项目拆" {
		t.Fatalf("decision reply = %v", rs)
	}
}

func TestLongPollWaitsForEvent(t *testing.T) {
	e := newTestEnv(t)
	id := e.createTask(t, "长轮询", "todo")

	start := time.Now()
	done := make(chan map[string]any, 1)
	go func() {
		_, body, _ := e.req(t, "GET", "/api/events?since=1&wait=5", nil)
		done <- body
	}()
	time.Sleep(120 * time.Millisecond)
	e.addEvent(t, id, "progress", "来了", "")

	select {
	case body := <-done:
		if d := time.Since(start); d > 4*time.Second {
			t.Fatalf("long poll returned too late: %v", d)
		}
		evs := body["events"].([]any)
		if len(evs) == 0 || evs[0].(map[string]any)["body"] != "来了" {
			t.Fatalf("long poll events = %v", evs)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("long poll did not return")
	}
}

func TestClaimLeaseAndAck(t *testing.T) {
	e := newTestEnv(t)
	id := e.createTask(t, "领取", "todo")
	ev := e.addEvent(t, id, "user_reply", "处理一下", "")
	seq := int64(ev["seq"].(float64))

	code, body, _ := e.req(t, "POST", "/api/claim", map[string]any{"consumer": "main", "seq": seq})
	if code != http.StatusOK {
		t.Fatalf("first claim: %d %v", code, body)
	}
	code, body, _ = e.req(t, "POST", "/api/claim", map[string]any{"consumer": "main", "seq": seq})
	if code != http.StatusConflict || body["reason"] != "leased" {
		t.Fatalf("second claim: %d %v", code, body)
	}
	if code, _, _ := e.req(t, "POST", "/api/ack", map[string]any{"consumer": "main", "seq": seq}); code != http.StatusOK {
		t.Fatalf("ack: %d", code)
	}
	code, body, _ = e.req(t, "POST", "/api/claim", map[string]any{"consumer": "main", "seq": seq})
	if code != http.StatusConflict || body["reason"] != "acked" {
		t.Fatalf("claim after ack: %d %v", code, body)
	}
}

// ---- AI 是唯一驱动者：看板可随时清空/归档 ----

func TestBoardResetRequiresConfirmAndWipesEverything(t *testing.T) {
	e := newTestEnv(t)
	id := e.createTask(t, "旧任务 A", "doing")
	e.createTask(t, "旧任务 B", "todo")
	e.addEvent(t, id, "user_reply", "一句回复", "")
	e.req(t, "POST", "/api/decisions", map[string]any{"task_id": id, "question": "选哪个", "options": []string{"a", "b"}})

	code, body, _ := e.req(t, "POST", "/api/board/reset", map[string]any{})
	if code != http.StatusBadRequest {
		t.Fatalf("reset without confirm: got %d %v", code, body)
	}
	code, body, _ = e.req(t, "POST", "/api/board/reset", map[string]any{"confirm": "nope"})
	if code != http.StatusBadRequest {
		t.Fatalf("reset with wrong confirm: got %d %v", code, body)
	}

	code, body, _ = e.req(t, "POST", "/api/board/reset", map[string]any{"confirm": "reset"})
	if code != http.StatusOK {
		t.Fatalf("reset: %d %v", code, body)
	}
	if got := body["cleared"].(map[string]any)["tasks"].(float64); got != 2 {
		t.Fatalf("cleared tasks = %v want 2", got)
	}

	code, body, _ = e.req(t, "GET", "/api/tasks", nil)
	if code != http.StatusOK || len(body["tasks"].([]any)) != 0 {
		t.Fatalf("tasks after reset = %v", body)
	}
	code, body, _ = e.req(t, "GET", "/api/decisions", nil)
	if code != http.StatusOK || len(body["decisions"].([]any)) != 0 {
		t.Fatalf("decisions after reset = %v", body)
	}
	code, body, _ = e.req(t, "GET", "/api/replies?recent=10", nil)
	if code != http.StatusOK || len(body["replies"].([]any)) != 0 {
		t.Fatalf("replies after reset = %v", body)
	}
	code, body, _ = e.req(t, "GET", "/api/activity?days=84", nil)
	if code != http.StatusOK || len(body["days"].([]any)) != 0 {
		t.Fatalf("activity after reset = %v", body)
	}
	// 清空后从头开始，编号回到 T-1，长轮询游标也归零
	if newID := e.createTask(t, "新会话第一件事", "todo"); newID != "T-1" {
		t.Fatalf("id after reset = %q want T-1", newID)
	}
	acked, _, _, err := e.store.consumer("main")
	if err != nil || acked != 0 {
		t.Fatalf("consumer after reset = %d, %v", acked, err)
	}
}

func TestBoardArchiveMovesToDoneAndDismissesDecisions(t *testing.T) {
	e := newTestEnv(t)
	a := e.createTask(t, "还没做完", "doing")
	b := e.createTask(t, "已经完成", "done")
	e.addEvent(t, b, "conclusion", "做完", "")
	e.req(t, "POST", "/api/decisions", map[string]any{"task_id": a, "question": "要继续吗", "options": []string{"要", "不要"}})

	code, body, _ := e.req(t, "POST", "/api/board/archive", map[string]any{"confirm": "archive"})
	if code != http.StatusOK {
		t.Fatalf("archive: %d %v", code, body)
	}

	code, body, _ = e.req(t, "GET", "/api/tasks", nil)
	if code != http.StatusOK {
		t.Fatal(code)
	}
	for _, raw := range body["tasks"].([]any) {
		tk := raw.(map[string]any)
		if tk["column"] != "done" {
			t.Fatalf("task %v not archived: %v", tk["id"], tk["column"])
		}
	}
	code, body, _ = e.req(t, "GET", "/api/decisions?status=pending", nil)
	if code != http.StatusOK || len(body["decisions"].([]any)) != 0 {
		t.Fatalf("pending after archive = %v", body)
	}
	// 历史事件仍然在
	code, body, _ = e.req(t, "GET", "/api/tasks/"+b, nil)
	if code != http.StatusOK {
		t.Fatal(code)
	}
	if len(body["events"].([]any)) < 2 {
		t.Fatalf("archive lost events: %v", body["events"])
	}
	// 归档不重置编号
	if id := e.createTask(t, "新任务", "todo"); id != "T-3" {
		t.Fatalf("id after archive = %q want T-3", id)
	}
}

func TestStaticAssets(t *testing.T) {
	e := newTestEnv(t)
	anon := noRedirectClient()
	for _, p := range []string{"/app.css", "/app.js", "/manifest.webmanifest", "/sw.js"} {
		resp := get(t, anon, e.ts.URL+p, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
			t.Fatalf("%s cache-control = %q", p, cc)
		}
	}
	resp := get(t, e.client, e.ts.URL+"/", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/: %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("/ cache-control = %q", cc)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
