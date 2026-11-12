package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// reqWithIdentity 用主令牌 + 身份头发请求，模拟「同一把令牌代表不同 Agent」。
func reqWithIdentity(t *testing.T, e *testEnv, method, path string, body any, token, identity string) (int, map[string]any, *http.Response) {
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
	req.Header.Set(identityHeader, identity)
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

// 中文 Windows 控制台会把中文参数写成 GBK 字节；进来要扶正成 UTF-8，
// 不然网页上全是乱码（命令行看着正常，因为它按 GBK 解）。
func TestGarbledConsoleInputIsRepaired(t *testing.T) {
	e := newTestEnv(t)
	// "筛选" 的 GBK 字节 C9 B8 D1 A1，被当成 Latin-1 读进来就是 ɸѡ。
	code, body, _ := e.req(t, "POST", "/api/tasks", map[string]any{
		"title": "标签筛选乱码", "project": "看板", "agent": "开发", "column": "todo"})
	if code != http.StatusCreated {
		t.Fatalf("建卡: %d %v", code, body)
	}
	id := body["task"].(map[string]any)["id"].(string)

	code, body, _ = e.req(t, "POST", "/api/tasks/"+id+"/tags", map[string]any{
		"add": []string{"ɸѡ"},
	})
	if code != http.StatusOK {
		t.Fatalf("加标签: %d %v", code, body)
	}
	tags := body["task"].(map[string]any)["tags"].([]any)
	if len(tags) != 1 || tags[0] != "筛选" {
		t.Fatalf("GBK 字节的标签该被扶正成「筛选」，实际 %v", tags)
	}

	// 正常中文不能被反过来打坏。
	code, body, _ = e.req(t, "POST", "/api/tasks/"+id+"/tags", map[string]any{
		"add": []string{"界面"},
	})
	if code != http.StatusOK {
		t.Fatalf("加标签: %d %v", code, body)
	}
	got := map[string]bool{}
	for _, x := range body["task"].(map[string]any)["tags"].([]any) {
		got[x.(string)] = true
	}
	if !got["界面"] {
		t.Fatalf("正常中文标签被改坏了: %v", body["task"])
	}
}

// 老库里已经写坏的标签，启动时扶正一次，正常标签不动。
func TestRepairMojibakeTagsOnOpen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "board.db")
	st, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.CreateTask("带坏标签的卡", "", "dev", "doing", "")
	if err != nil {
		t.Fatal(err)
	}
	// 绕过写入路径，模拟老版本已经存进去的乱码。
	if _, err := st.db.Exec(`INSERT INTO task_tags(task_id,tag) VALUES(?,?)`, id.ID, "ɸѡ"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO task_tags(task_id,tag) VALUES(?,?)`, id.ID, "界面"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	st2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	tags, err := st2.TaskTags(id.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, x := range tags {
		got[x] = true
	}
	if !got["筛选"] {
		t.Fatalf("乱码标签该被扶正成「筛选」，实际 %v", tags)
	}
	if got["ɸѡ"] {
		t.Fatalf("老的乱码标签该被删掉，实际 %v", tags)
	}
	if !got["界面"] {
		t.Fatalf("正常标签被动了: %v", tags)
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

// main 是「不设限」的身份，不是 Agent，不该出现在切换器里。
func TestAgentsListExcludesMainIdentity(t *testing.T) {
	e := newTestEnv(t)
	e.createTask(t, "看板开发", "doing")
	code, body, _ := e.req(t, "POST", "/api/tasks", map[string]any{
		"title": "主会话建的卡", "agent": "main", "column": "todo"})
	if code != http.StatusCreated {
		t.Fatalf("建卡: %d %v", code, body)
	}
	code, body, _ = e.req(t, "GET", "/api/agents", nil)
	if code != http.StatusOK {
		t.Fatalf("取 Agent 列表: %d %v", code, body)
	}
	for _, a := range body["agents"].([]any) {
		if strings.EqualFold(a.(string), "main") {
			t.Fatalf("main 不是 Agent，不该出现在列表里: %v", body["agents"])
		}
	}
}

// 每台机器的主 Agent 登记一次，切换器据此显示机器名、用户名和 IP。
// IP 只认请求来源：客户端写什么都会被覆盖。
func TestHostRegisterRecordsMachineAndSourceIP(t *testing.T) {
	e := newTestEnv(t)
	code, body, _ := e.req(t, "POST", "/api/hosts", map[string]any{
		"agent": "看板开发", "hostname": "DESKTOP-1", "username": "lucky", "ip": "1.2.3.4"})
	if code != http.StatusOK {
		t.Fatalf("登记主机: %d %v", code, body)
	}
	host := body["host"].(map[string]any)
	if host["ip"] != "127.0.0.1" {
		t.Fatalf("IP 该按请求来源记，实际 %v", host["ip"])
	}
	if host["hostname"] != "DESKTOP-1" || host["username"] != "lucky" {
		t.Fatalf("机器名/用户名没记上: %v", host)
	}

	code, body, _ = e.req(t, "GET", "/api/agents", nil)
	if code != http.StatusOK {
		t.Fatalf("取 Agent 列表: %d %v", code, body)
	}
	hosts := body["hosts"].([]any)
	if len(hosts) != 1 {
		t.Fatalf("应该有一台主机: %v", hosts)
	}
	if got := hosts[0].(map[string]any)["agent"]; got != "看板开发" {
		t.Fatalf("主机归谁: %v", got)
	}
}

// 一台机器只留一个主 Agent：同机器同用户再登记别的名字，旧名字让位。
func TestHostKeepsOneMainAgentPerMachine(t *testing.T) {
	e := newTestEnv(t)
	for _, a := range []string{"旧主 Agent", "新主 Agent"} {
		code, body, _ := e.req(t, "POST", "/api/hosts", map[string]any{
			"agent": a, "hostname": "DESKTOP-1", "username": "lucky"})
		if code != http.StatusOK {
			t.Fatalf("登记 %s: %d %v", a, code, body)
		}
	}
	code, body, _ := e.req(t, "GET", "/api/agents", nil)
	if code != http.StatusOK {
		t.Fatalf("取 Agent 列表: %d %v", code, body)
	}
	hosts := body["hosts"].([]any)
	if len(hosts) != 1 {
		t.Fatalf("同一台机器只该留一个主 Agent: %v", hosts)
	}
	if got := hosts[0].(map[string]any)["agent"]; got != "新主 Agent" {
		t.Fatalf("后登记的主 Agent 该顶掉旧的，实际 %v", got)
	}
}

// 绑定令牌只能登记自己，不能替别的 Agent 报名——否则切换器能被塞假主机。
func TestHostRegisterCannotSpoofOtherAgent(t *testing.T) {
	e := newTestEnv(t)
	tok := "scoped-a2"
	if err := e.store.SaveAgentToken(sessionHash(tok), "a2"); err != nil {
		t.Fatal(err)
	}
	code, body, _ := reqWithIdentity(t, e, "POST", "/api/hosts", map[string]any{
		"agent": "a1", "hostname": "H", "username": "u"}, tok, "a1")
	if code != http.StatusOK {
		t.Fatalf("登记主机: %d %v", code, body)
	}
	if got := body["host"].(map[string]any)["agent"]; got != "a2" {
		t.Fatalf("绑定令牌只能登记自己，实际记成了 %v", got)
	}
}

// 主令牌可以替别的 Agent 登记（主会话替各机器报名）。
func TestHostRegisterMainTokenCanNameAgent(t *testing.T) {
	e := newTestEnv(t)
	code, body, _ := reqWithIdentity(t, e, "POST", "/api/hosts", map[string]any{
		"agent": "wealth-dev", "hostname": "LAPTOP", "username": "lucky"}, e.token, "")
	if code != http.StatusOK {
		t.Fatalf("登记主机: %d %v", code, body)
	}
	if got := body["host"].(map[string]any)["agent"]; got != "wealth-dev" {
		t.Fatalf("主令牌登记的主 Agent: %v", got)
	}
	// main 不是 Agent，拒绝。
	code, body, _ = e.req(t, "POST", "/api/hosts", map[string]any{"agent": "main"})
	if code != http.StatusBadRequest {
		t.Fatalf("main 不该能登记成主机: %d %v", code, body)
	}
}

// 身份隔离：同一把令牌可以代表不同 Agent，服务端按身份收窄，不靠令牌。
func TestIdentityIsolationByHeader(t *testing.T) {
	e := newTestEnv(t)

	// 主会话建两张卡，分别归 a1 / a2。
	code, body, _ := e.req(t, "POST", "/api/tasks", map[string]any{
		"title": "a1 的卡", "agent": "a1", "column": "doing"})
	if code != http.StatusCreated {
		t.Fatalf("建 a1 的卡: %d %v", code, body)
	}
	mine := body["task"].(map[string]any)["id"].(string)
	code, body, _ = e.req(t, "POST", "/api/tasks", map[string]any{
		"title": "a2 的卡", "agent": "a2", "column": "doing"})
	if code != http.StatusCreated {
		t.Fatalf("建 a2 的卡: %d %v", code, body)
	}
	theirs := body["task"].(map[string]any)["id"].(string)

	// 给 a2 的卡挂一条待拍板和一条对话，确认 a1 看不到。
	code, body, _ = e.req(t, "POST", "/api/decisions", map[string]any{
		"task_id": theirs, "question": "a2 要拍板的事", "options": []string{"行", "不行"}})
	if code != http.StatusCreated {
		t.Fatalf("建待拍板: %d %v", code, body)
	}
	decisionID := body["decision"].(map[string]any)["id"].(string)
	code, body, _ = e.req(t, "POST", "/api/message", map[string]any{
		"text": "a2 卡上的话", "from": "a2", "task_id": theirs})
	if code != http.StatusCreated {
		t.Fatalf("发消息: %d %v", code, body)
	}
	code, body, _ = e.req(t, "POST", "/api/tasks/"+theirs+"/events", map[string]any{
		"kind": "progress", "text": "a2 的进展", "agent": "a2"})
	if code != http.StatusCreated {
		t.Fatalf("写事件: %d %v", code, body)
	}

	// 同一把主令牌，只是带上 a1 的身份。
	asA1 := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		code, out, _ := reqWithIdentity(t, e, method, path, body, e.token, "a1")
		return code, out
	}

	// 列表里只有自己的卡。
	code, out := asA1("GET", "/api/tasks", nil)
	if code != http.StatusOK {
		t.Fatalf("列卡: %d %v", code, out)
	}
	tasks := out["tasks"].([]any)
	if len(tasks) != 1 || tasks[0].(map[string]any)["id"] != mine {
		t.Fatalf("a1 只该看见自己的卡: %v", tasks)
	}

	// 别人的卡：读、写都 403，不是静默空。
	if code, _ = asA1("GET", "/api/tasks/"+theirs, nil); code != http.StatusForbidden {
		t.Fatalf("读别人的卡: %d, want 403", code)
	}
	if code, _ = asA1("POST", "/api/tasks/"+theirs+"/events", map[string]any{
		"kind": "note", "text": "插一脚"}); code != http.StatusForbidden {
		t.Fatalf("往别人的卡写事件: %d, want 403", code)
	}
	if code, _ = asA1("PATCH", "/api/tasks/"+theirs, map[string]any{"column": "done"}); code != http.StatusForbidden {
		t.Fatalf("动别人的卡: %d, want 403", code)
	}
	if code, _ = asA1("GET", "/api/tasks/"+theirs+"/events", nil); code != http.StatusForbidden {
		t.Fatalf("看别人的事件: %d, want 403", code)
	}
	if code, _ = asA1("POST", "/api/message", map[string]any{
		"text": "挂到别人卡上", "task_id": theirs}); code != http.StatusForbidden {
		t.Fatalf("往别人的卡发消息: %d, want 403", code)
	}
	if code, _ = asA1("POST", "/api/decisions/"+decisionID+"/answer",
		map[string]any{"answer": "行"}); code != http.StatusForbidden {
		t.Fatalf("答别人的待拍板: %d, want 403", code)
	}

	// 别人的待拍板、回执、对话都不返回。
	code, out = asA1("GET", "/api/decisions?status=pending", nil)
	if code != http.StatusOK {
		t.Fatalf("看待拍板: %d %v", code, out)
	}
	if n := len(out["decisions"].([]any)); n != 0 {
		t.Fatalf("a1 不该看见 a2 的待拍板: %v", out["decisions"])
	}
	code, out = asA1("GET", "/api/chat?recent=50", nil)
	if code != http.StatusOK {
		t.Fatalf("看对话: %d %v", code, out)
	}
	for _, m := range out["messages"].([]any) {
		if m.(map[string]any)["body"] == "a2 卡上的话" {
			t.Fatal("a1 不该看见 a2 卡上的消息")
		}
	}
	code, out = asA1("GET", "/api/replies?recent=50", nil)
	if code != http.StatusOK {
		t.Fatalf("看回执: %d %v", code, out)
	}
	for _, rp := range out["replies"].([]any) {
		m := rp.(map[string]any)
		if m["task_id"] == theirs {
			t.Fatalf("a1 不该看见 a2 卡上的回执: %v", rp)
		}
		// 自由对话的回执本身不挂卡，但它对应的消息挂着 a2 的卡，同样不能给。
		if m["text"] == "a2 卡上的话" {
			t.Fatalf("a1 不该看见 a2 卡上消息的回执: %v", rp)
		}
	}

	// 自己名下照常能改。
	if code, _ = asA1("PATCH", "/api/tasks/"+mine, map[string]any{"column": "done"}); code != http.StatusOK {
		t.Fatalf("动自己的卡: %d, want 200", code)
	}

	// 主会话（不带身份）全局可见。
	code, out, _ = e.req(t, "GET", "/api/tasks", nil)
	if code != http.StatusOK || len(out["tasks"].([]any)) != 2 {
		t.Fatalf("主会话该看见全部: %d %v", code, out)
	}
}

// 身份头也可以只带 main，等价于主会话。
func TestIdentityMainSeesEverything(t *testing.T) {
	e := newTestEnv(t)
	e.createTask(t, "a1 的卡", "doing")
	code, out, _ := reqWithIdentity(t, e, "GET", "/api/tasks", nil, e.token, "main")
	if code != http.StatusOK {
		t.Fatalf("列卡: %d %v", code, out)
	}
	if n := len(out["tasks"].([]any)); n != 1 {
		t.Fatalf("main 该看见全部: %v", out["tasks"])
	}
}

// 回执：身份不符时连状态都不能改，且不能先改后拒。
func TestReplyUpdateRespectsIdentity(t *testing.T) {
	e := newTestEnv(t)
	code, body, _ := e.req(t, "POST", "/api/tasks", map[string]any{
		"title": "a2 的卡", "agent": "a2", "column": "doing"})
	if code != http.StatusCreated {
		t.Fatalf("建卡: %d %v", code, body)
	}
	id := body["task"].(map[string]any)["id"].(string)
	code, body, _ = e.req(t, "POST", "/api/message", map[string]any{
		"text": "要处理的一句话", "from": "user", "task_id": id})
	if code != http.StatusCreated {
		t.Fatalf("发消息: %d %v", code, body)
	}
	seq := body["event"].(map[string]any)["seq"]

	code, out, _ := reqWithIdentity(t, e, "PATCH", "/api/replies/"+fmt.Sprint(seq),
		map[string]any{"status": "done"}, e.token, "a1")
	if code != http.StatusForbidden {
		t.Fatalf("改别人卡上的回执: %d %v", code, out)
	}
	// 被拒之后状态没变。
	code, out, _ = e.req(t, "GET", "/api/replies?recent=10", nil)
	if code != http.StatusOK {
		t.Fatalf("看回执: %d", code)
	}
	for _, rp := range out["replies"].([]any) {
		m := rp.(map[string]any)
		if m["seq"] == seq && m["status"] == "done" {
			t.Fatalf("403 之前状态已经被改了: %v", m)
		}
	}
}

// 看板计数也按身份收窄。
func TestBoardCountsScoped(t *testing.T) {
	e := newTestEnv(t)
	e.createTask(t, "a1 的卡", "doing")
	code, body, _ := e.req(t, "POST", "/api/tasks", map[string]any{
		"title": "a2 的卡", "agent": "a2", "column": "doing"})
	if code != http.StatusCreated {
		t.Fatalf("建卡: %d %v", code, body)
	}
	code, out, _ := reqWithIdentity(t, e, "GET", "/api/board", nil, e.token, "a1")
	if code != http.StatusOK {
		t.Fatalf("看计数: %d %v", code, out)
	}
	if n := out["counts"].(map[string]any)["tasks"]; n != float64(1) {
		t.Fatalf("a1 的卡数应该是 1: %v", n)
	}
	code, out, _ = e.req(t, "GET", "/api/board", nil)
	if code != http.StatusOK || out["counts"].(map[string]any)["tasks"] != float64(2) {
		t.Fatalf("主会话该看见 2 张: %d %v", code, out)
	}
}

// 老的 --agent 令牌绑定仍然生效，没有因为身份头机制被破坏。
func TestLegacyAgentTokenStillScoped(t *testing.T) {
	e := newTestEnv(t)
	agentToken := "legacy-a2-token"
	if err := e.store.SaveAgentToken(sessionHash(agentToken), "a2"); err != nil {
		t.Fatal(err)
	}
	e.createTask(t, "a1 的卡", "doing")
	code, out, _ := reqWithToken(t, e, "GET", "/api/tasks", nil, agentToken)
	if code != http.StatusOK {
		t.Fatalf("列卡: %d %v", code, out)
	}
	if n := len(out["tasks"].([]any)); n != 0 {
		t.Fatalf("a2 的令牌不该看见 a1 的卡: %v", out["tasks"])
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
