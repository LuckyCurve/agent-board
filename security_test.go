package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 身份头只对主令牌有效：绑定了 Agent 的令牌换个头不能看别人的卡。
func TestScopedTokenIgnoresIdentityHeader(t *testing.T) {
	e := newTestEnv(t)
	tok := "scoped-a2"
	if err := e.store.SaveAgentToken(sessionHash(tok), "a2"); err != nil {
		t.Fatal(err)
	}
	e.createTask(t, "a1 的卡", "doing")

	code, out, _ := reqWithIdentity(t, e, "GET", "/api/tasks", nil, tok, "a1")
	if code != http.StatusOK {
		t.Fatalf("列卡: %d %v", code, out)
	}
	if n := len(out["tasks"].([]any)); n != 0 {
		t.Fatalf("a2 的令牌伪装 a1 看到了 %d 张卡: %v", n, out["tasks"])
	}

	// 主令牌带头则照常收窄。
	e2 := newTestEnv(t)
	e2.createTask(t, "a1 的卡", "doing")
	code, out, _ = reqWithIdentity(t, e2, "GET", "/api/tasks", nil, e2.token, "a2")
	if code != http.StatusOK || len(out["tasks"].([]any)) != 0 {
		t.Fatalf("主令牌按身份收窄失效: %d %v", code, out)
	}
}

// 反代后面 RemoteAddr 都是 127.0.0.1，必须按 X-Forwarded-For 里的真实 IP 限流。
func TestLoginLimitUsesForwardedIP(t *testing.T) {
	e := newTestServer(t, Config{Version: "test", LoginLimit: 2, LoginWindow: time.Minute}, testPassHash)

	try := func(ip, pw string) int {
		body := strings.NewReader(`{"password":"` + pw + `"}`)
		req, _ := http.NewRequest("POST", e.ts.URL+"/api/login", body)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", ip)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// 攻击者打满自己的桶。
	for i := 0; i < 2; i++ {
		if got := try("203.0.113.9", "wrong"); got != http.StatusUnauthorized {
			t.Fatalf("攻击者第 %d 次: %d", i+1, got)
		}
	}
	if got := try("203.0.113.9", "test-pw"); got != http.StatusTooManyRequests {
		t.Fatalf("攻击者该被限流: %d", got)
	}
	// 别人不受影响。
	if got := try("198.51.100.7", "test-pw"); got != http.StatusOK {
		t.Fatalf("另一个 IP 不该被连坐: %d", got)
	}
}

// 失败记录落库：重启（新建 Server）后仍然拦得住。
func TestLoginFailuresSurviveRestart(t *testing.T) {
	e := newTestServer(t, Config{Version: "test", LoginLimit: 2, LoginWindow: time.Minute}, testPassHash)
	for i := 0; i < 2; i++ {
		postLogin(t, e, "wrong")
	}
	// 换一个全新的 Server（内存桶是空的），复用同一个库。
	fresh := NewServer(e.store, Config{Version: "test", LoginLimit: 2, LoginWindow: time.Minute})
	ts := httptest.NewServer(fresh.Handler())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/api/login", "application/json",
		strings.NewReader(`{"password":"test-pw"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("重启后仍该被拦: %d", resp.StatusCode)
	}
}

// 全局桶阈值要宽：单 IP 打满不会把主人一起锁死。
func TestGlobalBucketIsLoose(t *testing.T) {
	l := newLoginLimiter(3, time.Minute)
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		l.fail("attacker")
	}
	if blocked, _ := l.status("attacker"); !blocked {
		t.Fatal("攻击者自己该被限流")
	}
	if blocked, _ := l.status("owner"); blocked {
		t.Fatal("别人打满自己的桶，不该把主人也锁住")
	}
	if l.globalMax < 1000 {
		t.Fatalf("全局桶阈值太低，容易被拿来锁人: %d", l.globalMax)
	}
}

// HTTPS 下补 HSTS；本机 http 不下发，免得开发环境被浏览器记死。
func TestHSTSOnlyOverHTTPS(t *testing.T) {
	e := newTestEnv(t)
	plain := get(t, e.client, e.ts.URL+"/login", nil)
	if plain.Header.Get("Strict-Transport-Security") != "" {
		t.Fatal("http 不该下发 HSTS")
	}
	req, _ := http.NewRequest("GET", e.ts.URL+"/login", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Strict-Transport-Security") == "" {
		t.Fatal("HTTPS 下该下发 HSTS")
	}
}

// CSP 收紧：不再允许内联脚本。
func TestCSPDisallowsInlineScript(t *testing.T) {
	e := newTestEnv(t)
	resp := get(t, e.client, e.ts.URL+"/login", nil)
	csp := resp.Header.Get("Content-Security-Policy")
	if strings.Contains(csp, "'unsafe-inline'") && strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Fatalf("script-src 不该再放内联: %s", csp)
	}
	if !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("script-src 应该是 'self': %s", csp)
	}
	for _, page := range []string{"/login"} {
		body := bodyString(t, get(t, e.client, e.ts.URL+page, nil))
		if strings.Contains(body, "<script>") {
			t.Fatalf("%s 还有内联脚本，CSP 会拦掉", page)
		}
	}
}

// 令牌轮换：旧令牌在宽限期内仍然可用，prune 之后才失效。
func TestTokenRotationKeepsGrace(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/token"
	if _, err := ensureToken(path, "old-token"); err != nil {
		t.Fatal(err)
	}
	st, err := OpenStore(dir + "/board.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := NewServer(st, Config{TokenFile: path})

	if !s.validToken("old-token") {
		t.Fatal("初始令牌该有效")
	}
	// 模拟 token --new 的写法：新令牌在前，旧的留一行。
	if err := writeTokenFile(path, "new-token", "old-token"); err != nil {
		t.Fatal(err)
	}
	if !s.validToken("new-token") || !s.validToken("old-token") {
		t.Fatal("宽限期内新旧令牌都该有效")
	}
	if err := writeTokenFile(path, "new-token"); err != nil {
		t.Fatal(err)
	}
	if s.validToken("old-token") {
		t.Fatal("prune 之后旧令牌该失效")
	}
	if !s.validToken("new-token") {
		t.Fatal("当前令牌不该失效")
	}
}

// D-2：网页走 cookie。cookie 会被浏览器自动带上，所以写操作必须过同源校验；
// 跨站页面借用户的登录态提交，一律 403。
func TestCookieWriteRequiresSameOrigin(t *testing.T) {
	e := newTestEnv(t)
	body := func() io.Reader { return strings.NewReader(`{"title":"csrf 测试"}`) }

	do := func(origin string) int {
		req, _ := http.NewRequest("POST", e.ts.URL+"/api/tasks", body())
		req.Header.Set("Content-Type", "application/json")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := e.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if got := do("https://evil.example"); got != http.StatusForbidden {
		t.Fatalf("跨站写操作: got %d want 403", got)
	}
	if got := do(e.ts.URL); got >= 400 {
		t.Fatalf("同源写操作: got %d", got)
	}
	// 不带 Origin 的同源表单/旧客户端照旧放行，跨站带 cookie 的 POST 由 SameSite=Lax 挡住。
	if got := do(""); got >= 400 {
		t.Fatalf("无 Origin 写操作: got %d", got)
	}

	// 读操作不受 Origin 限制：跨站读不到 cookie，同源页面读本来就该放行。
	req, _ := http.NewRequest("GET", e.ts.URL+"/api/tasks", nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("读操作不该被 Origin 拦: %d", resp.StatusCode)
	}
}

// 过渡期：部署前已经打开、还没刷新的旧页面仍带着 meta token。
// 令牌继续有效，但页面本身不再注入它——新开的页面只靠 cookie。
func TestLegacyTokenStillWorksForApi(t *testing.T) {
	e := newTestServer(t, Config{Version: "test"}, testPassHash)
	anon := noRedirectClient()
	if got := get(t, anon, e.ts.URL+"/api/tasks", map[string]string{
		"Authorization": "Bearer " + e.token}).StatusCode; got != http.StatusOK {
		t.Fatalf("旧页面的令牌该继续可用: got %d", got)
	}
}

// cookie 鉴权下，网页会话自己就是主会话，不该被当成某个 Agent 收窄。
func TestCookieSessionSeesWholeBoard(t *testing.T) {
	e := newTestEnv(t)
	if code, out, _ := e.req(t, "POST", "/api/tasks", map[string]any{"title": "别人的卡", "agent": "a9"}); code >= 400 {
		t.Fatalf("建卡: %d %v", code, out)
	}
	code, out, _ := e.req(t, "GET", "/api/tasks", nil)
	if code != http.StatusOK {
		t.Fatalf("列卡: %d", code)
	}
	if n := len(out["tasks"].([]any)); n == 0 {
		t.Fatal("网页会话该看到全部卡片，而不是被收窄成空")
	}
}
