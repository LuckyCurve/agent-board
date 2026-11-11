package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// reqWithToken 用指定令牌发请求。
func reqWithToken(t *testing.T, e *testEnv, method, path string, body any, token string) (int, map[string]any, *http.Response) {
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
	req.Header.Set("Authorization", "Bearer "+token)
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

// 标签：加、删、列出、筛选用的标签集合。
func TestTaskTags(t *testing.T) {
	e := newTestEnv(t)
	id := e.createTask(t, "带标签的卡", "doing")

	code, body, _ := e.req(t, "POST", "/api/tasks/"+id+"/tags", map[string]any{
		"add": []string{"界面", "推送", "界面"},
	})
	if code != http.StatusOK {
		t.Fatalf("加标签: %d %v", code, body)
	}
	tags := body["task"].(map[string]any)["tags"].([]any)
	if len(tags) != 2 {
		t.Fatalf("标签去重后应该是 2 个: %v", tags)
	}

	code, body, _ = e.req(t, "POST", "/api/tasks/"+id+"/tags", map[string]any{
		"remove": []string{"推送"},
	})
	if code != http.StatusOK {
		t.Fatalf("删标签: %d %v", code, body)
	}
	tags = body["task"].(map[string]any)["tags"].([]any)
	if len(tags) != 1 || tags[0] != "界面" {
		t.Fatalf("删完应该是只剩「界面」: %v", tags)
	}

	// 列卡时也要带上标签。
	code, body, _ = e.req(t, "GET", "/api/tasks", nil)
	if code != http.StatusOK {
		t.Fatalf("列卡: %d", code)
	}
	tasks := body["tasks"].([]any)
	t0 := tasks[0].(map[string]any)
	if t0["tags"] == nil {
		t.Fatal("列卡时没带标签")
	}
}

// 转交：卡片换主人，并且留下一条事件说明来龙去脉。
func TestTaskTransferKeepsTrail(t *testing.T) {
	e := newTestEnv(t)
	id := e.createTask(t, "要转交的卡", "doing")

	code, body, _ := e.req(t, "PATCH", "/api/tasks/"+id, map[string]any{"agent": "a2"})
	if code != http.StatusOK {
		t.Fatalf("转交: %d %v", code, body)
	}
	if got := body["task"].(map[string]any)["agent"]; got != "a2" {
		t.Fatalf("agent = %v, want a2", got)
	}

	code, body, _ = e.req(t, "GET", "/api/tasks/"+id+"/events", nil)
	if code != http.StatusOK {
		t.Fatalf("看事件: %d", code)
	}
	found := false
	for _, ev := range body["events"].([]any) {
		e0 := ev.(map[string]any)
		if e0["type"] == "task.assigned" {
			found = true
			if !strings.Contains(e0["body"].(string), "a2") {
				t.Fatalf("事件里没说清楚: %v", e0["body"])
			}
		}
	}
	if !found {
		t.Fatal("没有留下转交事件")
	}
}

// 绑定了 Agent 的令牌只能碰自己名下的卡。
func TestAgentTokenIsolation(t *testing.T) {
	e := newTestEnv(t)

	// 一把只属于 a2 的令牌。
	agentToken := "agent-token-for-a2"
	if err := e.store.SaveAgentToken(sessionHash(agentToken), "a2"); err != nil {
		t.Fatal(err)
	}

	mine := e.createTask(t, "a1 的卡", "doing")
	code, body, _ := e.req(t, "POST", "/api/tasks", map[string]any{
		"title": "a2 的卡", "agent": "a2", "column": "doing"})
	if code != http.StatusCreated {
		t.Fatalf("建卡: %d %v", code, body)
	}
	theirs := body["task"].(map[string]any)["id"].(string)

	// 用 a2 的令牌请求。
	code, body, _ = reqWithToken(t, e, "GET", "/api/tasks", nil, agentToken)
	if code != http.StatusOK {
		t.Fatalf("列卡: %d %v", code, body)
	}
	tasks := body["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("a2 只该看见自己的 1 张卡，实际 %d 张", len(tasks))
	}
	if got := tasks[0].(map[string]any)["id"]; got != theirs {
		t.Fatalf("看见的是 %v，应该是 %s", got, theirs)
	}

	// 别人的卡：读、改都不行。
	code, _, _ = reqWithToken(t, e, "GET", "/api/tasks/"+mine, nil, agentToken)
	if code != http.StatusForbidden {
		t.Fatalf("读别人的卡: %d, want 403", code)
	}
	code, _, _ = reqWithToken(t, e, "POST", "/api/tasks/"+mine+"/events", map[string]any{
		"kind": "note", "text": "插一脚"}, agentToken)
	if code != http.StatusForbidden {
		t.Fatalf("往别人的卡写事件: %d, want 403", code)
	}
	code, _, _ = reqWithToken(t, e, "PATCH", "/api/tasks/"+mine, map[string]any{
		"column": "done"}, agentToken)
	if code != http.StatusForbidden {
		t.Fatalf("动别人的卡: %d, want 403", code)
	}

	// 自己名下随便动。
	code, _, _ = reqWithToken(t, e, "PATCH", "/api/tasks/"+theirs, map[string]any{
		"column": "doing"}, agentToken)
	if code != http.StatusOK {
		t.Fatalf("动自己的卡: %d, want 200", code)
	}

	// 建的卡自动归自己。
	code, body, _ = reqWithToken(t, e, "POST", "/api/tasks", map[string]any{
		"title": "新卡", "agent": "a1", "column": "todo"}, agentToken)
	if code != http.StatusCreated {
		t.Fatalf("建卡: %d %v", code, body)
	}
	if got := body["task"].(map[string]any)["agent"]; got != "a2" {
		t.Fatalf("建的卡应该归 a2，实际 %v", got)
	}
}

// 页面右上角的切换器要知道有哪些 Agent。
func TestAgentsList(t *testing.T) {
	e := newTestEnv(t)
	e.createTask(t, "看板开发", "doing")
	code, body, _ := e.req(t, "POST", "/api/tasks", map[string]any{
		"title": "云端部署", "agent": "云端部署", "column": "todo"})
	if code != http.StatusCreated {
		t.Fatalf("建卡: %d %v", code, body)
	}
	code, body, _ = e.req(t, "GET", "/api/agents", nil)
	if code != http.StatusOK {
		t.Fatalf("取 Agent 列表: %d %v", code, body)
	}
	agents := body["agents"].([]any)
	if len(agents) != 2 {
		t.Fatalf("应该有 2 个 Agent: %v", agents)
	}
}

// 对话流里也按 Agent 隔离：别人的卡上挂着的话不该出现在这个 Agent 眼前。
func TestChatIsScopedToAgent(t *testing.T) {
	e := newTestEnv(t)
	agentToken := "chat-agent-token"
	if err := e.store.SaveAgentToken(sessionHash(agentToken), "a2"); err != nil {
		t.Fatal(err)
	}
	other := e.createTask(t, "a1 的卡", "doing")
	code, body, _ := e.req(t, "POST", "/api/tasks", map[string]any{
		"title": "a2 的卡", "agent": "a2", "column": "doing"})
	if code != http.StatusCreated {
		t.Fatalf("建卡: %d %v", code, body)
	}
	mine := body["task"].(map[string]any)["id"].(string)

	for _, m := range []map[string]any{
		{"text": "别人的卡上的话", "from": "user", "task_id": other},
		{"text": "自己的卡上的话", "from": "user", "task_id": mine},
		{"text": "没有卡的话", "from": "user"},
	} {
		code, body, _ = e.req(t, "POST", "/api/message", m)
		if code != http.StatusCreated {
			t.Fatalf("发消息: %d %v", code, body)
		}
	}

	code, body, _ = reqWithToken(t, e, "GET", "/api/chat?recent=50", nil, agentToken)
	if code != http.StatusOK {
		t.Fatalf("看对话: %d %v", code, body)
	}
	var texts []string
	for _, m := range body["messages"].([]any) {
		texts = append(texts, m.(map[string]any)["body"].(string))
	}
	joined := strings.Join(texts, "|")
	if strings.Contains(joined, "别人的卡上的话") {
		t.Fatalf("a2 不该看见 a1 的卡上说的话: %v", texts)
	}
	if !strings.Contains(joined, "自己的卡上的话") {
		t.Fatalf("a2 该看见自己卡上的话: %v", texts)
	}
	if !strings.Contains(joined, "没有卡的话") {
		t.Fatalf("没挂卡的话人人都该看得见: %v", texts)
	}
}
