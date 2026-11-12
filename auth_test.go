package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func get(t *testing.T, cl *http.Client, rawURL string, header map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func postLogin(t *testing.T, e *testEnv, pw string) *http.Response {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"password": pw})
	resp, err := http.Post(e.ts.URL+"/api/login", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func bodyString(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// 页面靠网页登录态；接口收 Bearer token 或会话 cookie，两种都合法，匿名一律 401。
func TestAnonymousRequestsAreBlocked(t *testing.T) {
	e := newTestServer(t, Config{Version: "test"}, testPassHash)
	anon := noRedirectClient()

	for _, p := range []string{"/api/tasks", "/api/health", "/api/me", "/api/board", "/api/nope"} {
		resp := get(t, anon, e.ts.URL+p, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s anonymous: got %d want 401", p, resp.StatusCode)
		}
	}

	// 正确的 token 才能进；错的照样 401。
	if got := get(t, anon, e.ts.URL+"/api/tasks", map[string]string{
		"Authorization": "Bearer wrong"}).StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d want 401", got)
	}
	if got := get(t, anon, e.ts.URL+"/api/tasks", map[string]string{
		"Authorization": "Bearer test-token"}).StatusCode; got != http.StatusOK {
		t.Fatalf("correct token: got %d want 200", got)
	}

	// 网页登录态也能调接口（D-2 拍板改成 cookie）。
	jar := loginWithJar(t, e.ts.URL, "test-pw")
	if got := get(t, jar, e.ts.URL+"/api/tasks", nil).StatusCode; got != http.StatusOK {
		t.Fatalf("cookie api: got %d want 200", got)
	}
	if got := get(t, jar, e.ts.URL+"/", nil).StatusCode; got != http.StatusOK {
		t.Fatalf("cookie page: got %d want 200", got)
	}

	for _, p := range []string{"/", "/index.html", "/nope"} {
		resp := get(t, anon, e.ts.URL+p, nil)
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s anonymous page: got %d want 303", p, resp.StatusCode)
		}
		loc, err := url.Parse(resp.Header.Get("Location"))
		if err != nil || loc.Path != "/login" {
			t.Fatalf("%s redirect location = %q", p, resp.Header.Get("Location"))
		}
		if got := loc.Query().Get("next"); got != p {
			t.Fatalf("%s next = %q want %q", p, got, p)
		}
	}

	for _, p := range []string{"/login", "/login.html", "/app.css", "/app.js", "/login.js",
		"/icon.svg", "/manifest.webmanifest", "/sw.js"} {
		if got := get(t, anon, e.ts.URL+p, nil).StatusCode; got != http.StatusOK {
			t.Fatalf("%s public: got %d want 200", p, got)
		}
	}
	if got := get(t, anon, e.ts.URL+"/missing.css", nil).StatusCode; got != http.StatusNotFound {
		t.Fatalf("missing asset: got %d want 404", got)
	}
}

// 写接口、长轮询、SSE 全都认 token；浏览器 cookie 也认（D-2），但写操作要同源。
func TestAgentTokenCoversWriteLongPollAndSSE(t *testing.T) {
	e := newTestEnv(t)
	login := loginWithJar(t, e.ts.URL, "test-pw") // 有网页登录态，但没有 token
	anon := noRedirectClient()                    // 没 cookie 也没 token

	type call struct {
		method, path string
		body         any
	}
	calls := []call{
		{"GET", "/api/events?since=0&wait=0", nil},
		{"GET", "/api/me", nil},
	}
	for _, c := range calls {
		for _, tok := range []string{"", "wrong"} {
			if got := doWithToken(t, anon, c.method, e.ts.URL+c.path, c.body, tok); got != http.StatusUnauthorized {
				t.Fatalf("%s %s token=%q: got %d want 401", c.method, c.path, tok, got)
			}
		}
		if got := doWithToken(t, anon, c.method, e.ts.URL+c.path, c.body, e.token); got >= 400 {
			t.Fatalf("%s %s correct token: got %d", c.method, c.path, got)
		}
	}

	// 只带 cookie 也能读；写要过同源校验（这里没有 Origin，交给 SameSite 兜底）。
	if got := doWithToken(t, login, "GET", e.ts.URL+"/api/me", nil, ""); got != http.StatusOK {
		t.Fatalf("cookie read: got %d want 200", got)
	}
	if got := doWithToken(t, login, "POST", e.ts.URL+"/api/tasks", map[string]any{"title": "t"}, ""); got >= 400 {
		t.Fatalf("cookie write: got %d", got)
	}

	for _, tok := range []string{"", "wrong"} {
		if got := doWithToken(t, anon, "GET", e.ts.URL+"/api/stream", nil, tok); got != http.StatusUnauthorized {
			t.Fatalf("sse token=%q: got %d want 401", tok, got)
		}
	}
	// EventSource 不能带头，SSE 允许查询参数。
	if got := doWithToken(t, anon, "GET", e.ts.URL+"/api/stream?token="+e.token, nil, ""); got != http.StatusOK {
		t.Fatalf("sse query token: got %d want 200", got)
	}
	// 浏览器里 EventSource 自动带 cookie，不需要查询参数。
	if got := doWithToken(t, login, "GET", e.ts.URL+"/api/stream", nil, ""); got != http.StatusOK {
		t.Fatalf("sse cookie: got %d want 200", got)
	}
}

func doWithToken(t *testing.T, cl *http.Client, method, rawURL string, body any, token string) int {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, rawURL, r)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// SSE 会一直挂着；拿到响应头就撤。
	resp.Body.Close()
	return resp.StatusCode
}

// 页面里不再内嵌主令牌：网页走 cookie 就够了，令牌留在服务端和 CLI 手里。
func TestPageCarriesNoAgentToken(t *testing.T) {
	e := newTestEnv(t)
	page := bodyString(t, get(t, e.client, e.ts.URL+"/", nil))
	if strings.Contains(page, e.token) || strings.Contains(page, "api-token") {
		t.Fatal("页面不该再内嵌 Agent token，网页鉴权走会话 cookie")
	}
	login := bodyString(t, get(t, noRedirectClient(), e.ts.URL+"/login", nil))
	if strings.Contains(login, e.token) || strings.Contains(login, "api-token") {
		t.Fatal("login page must not carry the agent token")
	}
}

func TestTokenFileBootstrapAndRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	gen, err := ensureToken(path, "")
	if err != nil || gen == "" {
		t.Fatalf("bootstrap token: %q %v", gen, err)
	}
	b, _ := os.ReadFile(path)
	if strings.TrimSpace(string(b)) != gen {
		t.Fatal("token file does not match generated token")
	}
	if again, err := ensureToken(path, ""); err != nil || again != "" {
		t.Fatalf("second start should reuse token: %q %v", again, err)
	}

	provided := filepath.Join(dir, "token2")
	if gen, err := ensureToken(provided, "fixed-token"); err != nil || gen != "" {
		t.Fatalf("provided token: %q %v", gen, err)
	}
	b, _ = os.ReadFile(provided)
	if strings.TrimSpace(string(b)) != "fixed-token" {
		t.Fatal("provided token not written")
	}

	// 服务每次请求都读文件，轮换后立即生效。
	e := newTestServer(t, Config{Version: "test", TokenFile: path}, testPassHash)
	if got := doWithToken(t, noRedirectClient(), "GET", e.ts.URL+"/api/tasks", nil, gen); got != http.StatusOK {
		t.Fatalf("token from file: got %d", got)
	}
	if err := os.WriteFile(path, []byte("rotated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := doWithToken(t, noRedirectClient(), "GET", e.ts.URL+"/api/tasks", nil, gen); got != http.StatusUnauthorized {
		t.Fatalf("old token after rotation: got %d want 401", got)
	}
	if got := doWithToken(t, noRedirectClient(), "GET", e.ts.URL+"/api/tasks", nil, "rotated"); got != http.StatusOK {
		t.Fatalf("rotated token: got %d want 200", got)
	}
}

func TestLoginCookieAttributes(t *testing.T) {
	e := newTestServer(t, Config{Version: "test"}, testPassHash)
	resp := postLogin(t, e, "test-pw")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: got %d", resp.StatusCode)
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookie {
		t.Fatalf("cookies = %v", cookies)
	}
	c := cookies[0]
	if !c.HttpOnly {
		t.Fatal("session cookie must be HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("SameSite = %v want Lax", c.SameSite)
	}
	if c.Path != "/" || c.MaxAge <= 0 {
		t.Fatalf("cookie path/maxage = %q/%d", c.Path, c.MaxAge)
	}
	if c.Secure {
		t.Fatal("plain http should not mark cookie Secure")
	}
	if c.Value == "" || strings.Contains(c.Value, "test-pw") {
		t.Fatal("cookie value must be an opaque session id")
	}

	secure := newTestServer(t, Config{Version: "test", SecureCookie: true}, testPassHash)
	sc := postLogin(t, secure, "test-pw").Cookies()
	if len(sc) != 1 || !sc[0].Secure {
		t.Fatalf("secure cookie config ignored: %v", sc)
	}
}

func TestWrongPasswordIsGeneric(t *testing.T) {
	e := newTestServer(t, Config{Version: "test"}, testPassHash)
	var first string
	for _, pw := range []string{"", "nope", "definitely-not-the-password"} {
		resp := postLogin(t, e, pw)
		body := bodyString(t, resp)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong password %q: got %d want 401", pw, resp.StatusCode)
		}
		if first == "" {
			first = body
		} else if body != first {
			t.Fatalf("failure leaks detail: %q vs %q", first, body)
		}
		if strings.Contains(strings.ToLower(body), "password") || strings.Contains(body, "test") {
			t.Fatalf("failure body leaks detail: %q", body)
		}
	}
}

func TestLoginRateLimited(t *testing.T) {
	e := newTestServer(t, Config{Version: "test", LoginLimit: 3, LoginWindow: time.Minute}, testPassHash)
	for i := 0; i < 3; i++ {
		if got := postLogin(t, e, "wrong").StatusCode; got != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d want 401", i+1, got)
		}
	}
	resp := postLogin(t, e, "test-pw")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("after failures, correct password: got %d want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("rate limited response should carry Retry-After")
	}
}

func TestLoginLimiterWindowExpires(t *testing.T) {
	l := newLoginLimiter(2, time.Minute)
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	l.fail("ip")
	l.fail("ip")
	if blocked, _ := l.status("ip"); !blocked {
		t.Fatal("limiter should block after limit")
	}
	now = now.Add(time.Minute + time.Second)
	if blocked, _ := l.status("ip"); blocked {
		t.Fatal("limiter should release after window")
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	e := newTestEnv(t)

	if got := get(t, e.client, e.ts.URL+"/", nil).StatusCode; got != http.StatusOK {
		t.Fatalf("page before logout: got %d", got)
	}
	if got := get(t, e.client, e.ts.URL+"/api/me", map[string]string{
		"Authorization": "Bearer " + e.token}).StatusCode; got != http.StatusOK {
		t.Fatalf("me before logout: got %d", got)
	}
	cookies := e.client.Jar.Cookies(mustURL(t, e.ts.URL))
	var session string
	for _, c := range cookies {
		if c.Name == sessionCookie {
			session = c.Value
		}
	}
	if session == "" {
		t.Fatal("login did not store session cookie")
	}

	req, _ := http.NewRequest("POST", e.ts.URL+"/api/logout", nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout: got %d", resp.StatusCode)
	}
	if cleared := resp.Cookies(); len(cleared) == 0 || cleared[0].MaxAge >= 0 {
		t.Fatalf("logout did not clear cookie: %v", cleared)
	}
	if e.store.SessionValid(session) {
		t.Fatal("logout must revoke the server-side session")
	}
	noFollow := &http.Client{Jar: e.client.Jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	if got := get(t, noFollow, e.ts.URL+"/", nil).StatusCode; got != http.StatusSeeOther {
		t.Fatalf("page after logout: got %d want 303", got)
	}

	// 抄下来的旧 cookie 也失效。
	old := noRedirectClient()
	if got := get(t, old, e.ts.URL+"/", map[string]string{
		"Cookie": sessionCookie + "=" + session}).StatusCode; got != http.StatusSeeOther {
		t.Fatalf("replayed cookie page: got %d want 303", got)
	}
	if got := get(t, old, e.ts.URL+"/api/me", map[string]string{
		"Cookie": sessionCookie + "=" + session}).StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("replayed cookie api: got %d want 401", got)
	}
}

func TestPasswordHashingAndBootstrap(t *testing.T) {
	hash := hashPassword("s3cret")
	if strings.Contains(hash, "s3cret") {
		t.Fatal("hash contains plaintext")
	}
	if hashPassword("s3cret") == hash {
		t.Fatal("salt is not random")
	}
	if !verifyPassword(hash, "s3cret") || verifyPassword(hash, "wrong") {
		t.Fatal("verifyPassword mismatch")
	}

	st := testStore(t)
	generated, err := ensurePassword(st, "")
	if err != nil || generated == "" {
		t.Fatalf("first bootstrap: %q %v", generated, err)
	}
	stored, err := st.metaGet("password_hash")
	if err != nil {
		t.Fatal(err)
	}
	if stored == generated || strings.Contains(stored, generated) {
		t.Fatal("password stored in plaintext")
	}
	if !verifyPassword(stored, generated) {
		t.Fatal("stored hash does not verify generated password")
	}
	again, err := ensurePassword(st, "")
	if err != nil || again != "" {
		t.Fatalf("second start should reuse stored hash: %q %v", again, err)
	}

	// 改口令要撤销已有会话。
	if err := st.CreateSession("old-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	if !st.SessionValid("old-token") {
		t.Fatal("session should be valid")
	}
	if gen, err := ensurePassword(st, "brand-new-pw"); err != nil || gen != "" {
		t.Fatalf("set password: %q %v", gen, err)
	}
	if st.SessionValid("old-token") {
		t.Fatal("password change must revoke sessions")
	}
	stored, _ = st.metaGet("password_hash")
	if !verifyPassword(stored, "brand-new-pw") {
		t.Fatal("new password not stored")
	}
}

func TestPasswordChangeTakesEffectWithoutRestart(t *testing.T) {
	e := newTestServer(t, Config{Version: "test"}, testPassHash)
	if err := e.store.metaSet("password_hash", hashPassword("new-pw")); err != nil {
		t.Fatal(err)
	}
	if got := postLogin(t, e, "test-pw").StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("old password: got %d want 401", got)
	}
	if got := postLogin(t, e, "new-pw").StatusCode; got != http.StatusOK {
		t.Fatalf("new password: got %d want 200", got)
	}
}

func TestSessionStoreExpiry(t *testing.T) {
	st := testStore(t)
	if err := st.CreateSession("tok", -time.Minute); err != nil {
		t.Fatal(err)
	}
	if st.SessionValid("tok") {
		t.Fatal("expired session should be invalid")
	}
	if st.SessionValid("nope") {
		t.Fatal("unknown session should be invalid")
	}
	if err := st.CreateSession("tok2", time.Hour); err != nil {
		t.Fatal(err)
	}
	if !st.SessionValid("tok2") {
		t.Fatal("fresh session should be valid")
	}
	if err := st.DeleteSession("tok2"); err != nil {
		t.Fatal(err)
	}
	if st.SessionValid("tok2") {
		t.Fatal("deleted session should be invalid")
	}
}

func testStore(t *testing.T) *Store {
	t.Helper()
	st, err := OpenStore(t.TempDir() + "/board.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func loginWithJar(t *testing.T, base, pw string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	cl := &http.Client{Jar: jar}
	b, _ := json.Marshal(map[string]string{"password": pw})
	resp, err := cl.Post(base+"/api/login", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: got %d", resp.StatusCode)
	}
	return cl
}
