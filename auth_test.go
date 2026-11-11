package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
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

// 未登录时，除登录页和静态资源外一律不放行；Bearer token 也不再是旁路。
func TestAnonymousRequestsAreBlocked(t *testing.T) {
	e := newTestServer(t, Config{Version: "test"}, testPassHash)
	anon := noRedirectClient()

	for _, p := range []string{"/api/tasks", "/api/health", "/api/me", "/api/board", "/api/nope"} {
		resp := get(t, anon, e.ts.URL+p, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s anonymous: got %d want 401", p, resp.StatusCode)
		}
	}

	resp := get(t, anon, e.ts.URL+"/api/tasks", map[string]string{"Authorization": "Bearer test-token"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bearer token bypass: got %d want 401", resp.StatusCode)
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
	for _, pw := range []string{"", "nope", "test-pw2"} {
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

	if got := get(t, e.client, e.ts.URL+"/api/me", nil).StatusCode; got != http.StatusOK {
		t.Fatalf("me before logout: got %d", got)
	}
	cookies := e.client.Jar.Cookies(mustURL(t, e.ts.URL))
	var token string
	for _, c := range cookies {
		if c.Name == sessionCookie {
			token = c.Value
		}
	}
	if token == "" {
		t.Fatal("login did not store session cookie")
	}

	req, _ := http.NewRequest("POST", e.ts.URL+"/api/logout", nil)
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
	if got := get(t, e.client, e.ts.URL+"/api/me", nil).StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("me after logout: got %d want 401", got)
	}

	// 服务端会话已撤销：抄下来的旧 cookie 也失效。
	old := noRedirectClient()
	if got := get(t, old, e.ts.URL+"/api/me", map[string]string{
		"Cookie": sessionCookie + "=" + token}).StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("replayed cookie: got %d want 401", got)
	}
	if got := get(t, old, e.ts.URL+"/", map[string]string{
		"Cookie": sessionCookie + "=" + token}).StatusCode; got != http.StatusSeeOther {
		t.Fatalf("replayed cookie page: got %d want 303", got)
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
