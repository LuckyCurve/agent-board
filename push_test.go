package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPushSubscriptionRequiresTokenAndToggles(t *testing.T) {
	e := newTestEnv(t)

	code := doWithToken(t, noRedirectClient(), "GET", e.ts.URL+"/api/push/status", nil, "")
	if code != http.StatusUnauthorized {
		t.Fatalf("anonymous push status: %d, want 401", code)
	}

	code, body, _ := e.req(t, "GET", "/api/push/status", nil)
	if code != http.StatusOK || body["enabled"] != false {
		t.Fatalf("initial status: %d %v", code, body)
	}

	code, body, _ = e.req(t, "POST", "/api/push/subscribe", map[string]any{
		"endpoint": "https://push.example.test/sub/1",
		"keys":     map[string]string{"p256dh": "pkey", "auth": "asecret"},
	})
	if code != http.StatusOK {
		t.Fatalf("subscribe: %d %v", code, body)
	}
	code, body, _ = e.req(t, "GET", "/api/push/status", nil)
	if code != http.StatusOK || body["enabled"] != true {
		t.Fatalf("status after subscribe: %d %v", code, body)
	}

	code, _, _ = e.req(t, "POST", "/api/push/subscribe", map[string]any{
		"endpoint": "https://push.example.test/sub/1",
		"keys":     map[string]string{"p256dh": "", "auth": ""},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("empty keys: %d, want 400", code)
	}

	code, body, _ = e.req(t, "POST", "/api/push/unsubscribe", nil)
	if code != http.StatusOK {
		t.Fatalf("unsubscribe: %d %v", code, body)
	}
	code, body, _ = e.req(t, "GET", "/api/push/status", nil)
	if code != http.StatusOK || body["enabled"] != false {
		t.Fatalf("status after unsubscribe: %d %v", code, body)
	}
}

func TestPushKeyIsStable(t *testing.T) {
	e := newTestEnv(t)
	code, body, _ := e.req(t, "GET", "/api/push/key", nil)
	if code != http.StatusOK {
		t.Fatalf("push key: %d %v", code, body)
	}
	key := body["key"].(string)
	if key == "" {
		t.Fatal("empty VAPID public key")
	}
	_, again, _ := e.req(t, "GET", "/api/push/key", nil)
	if again["key"] != key {
		t.Fatal("VAPID key changed between requests")
	}
}

func TestPushOnlyForAttentionEvents(t *testing.T) {
	cases := []struct {
		kind string
		text string
		ok   bool
		want string
	}{
		{"decision", "选本地盘还是对象存储", true, "等你拍板"},
		{"done", "导出已经交付", true, "完成"},
		{"blocked", "权限没开", true, "阻塞"},
		{"failed", "截图脚本崩了", true, "失败"},
		{"progress", "跑了一半", true, "进展"},
		{"note", "随手记", false, ""},
		{"tool", "grep 一下", false, ""},
	}
	for _, c := range cases {
		msg, ok := pushMessageFor(c.kind, "T-1", c.text)
		if ok != c.ok {
			t.Fatalf("%s: ok=%v, want %v", c.kind, ok, c.ok)
		}
		if !c.ok {
			continue
		}
		if !strings.Contains(msg.Title, c.want) && !strings.Contains(msg.Body, c.want) {
			t.Fatalf("%s: message %+v missing %q", c.kind, msg, c.want)
		}
		if !strings.Contains(msg.Body, c.text) {
			t.Fatalf("%s: message body %q missing reason %q", c.kind, msg.Body, c.text)
		}
	}
}

// 进展也要推：用户要的是「它在动」。同一张卡连着报进展只打扰一次。
func TestProgressPushHitsOncePerCard(t *testing.T) {
	e := newTestEnv(t)

	var hits atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		hits.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer fake.Close()

	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authRaw := make([]byte, 16)
	if _, err := rand.Read(authRaw); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SavePushSub(fake.URL,
		base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()),
		base64.RawURLEncoding.EncodeToString(authRaw)); err != nil {
		t.Fatal(err)
	}

	id := e.createTask(t, "边做边报", "doing")
	for _, text := range []string{"第一步", "第二步", "第三步"} {
		code, body, _ := e.req(t, "POST", "/api/tasks/"+id+"/events", map[string]any{
			"kind": "progress", "text": text})
		if code != http.StatusCreated {
			t.Fatalf("progress: %d %v", code, body)
		}
	}
	// 推送是异步的，给它一点时间把能发的都发完。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && hits.Load() == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if n := hits.Load(); n != 1 {
		t.Fatalf("同一张卡连报三条进展，推送了 %d 次，want 1", n)
	}

	// 换一张卡不受影响。
	id2 := e.createTask(t, "另一张卡", "doing")
	code, _, _ := e.req(t, "POST", "/api/tasks/"+id2+"/events", map[string]any{
		"kind": "progress", "text": "开工"})
	if code != http.StatusCreated {
		t.Fatalf("second card progress: %d", code)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && hits.Load() < 2 {
		time.Sleep(50 * time.Millisecond)
	}
	if n := hits.Load(); n != 2 {
		t.Fatalf("另一张卡的进展没推：共 %d 次，want 2", n)
	}

	// 完成不受限流：照旧推。
	code, _, _ = e.req(t, "PATCH", "/api/tasks/"+id, map[string]any{"column": "done"})
	if code != http.StatusOK {
		t.Fatalf("done: %d", code)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && hits.Load() < 3 {
		time.Sleep(50 * time.Millisecond)
	}
	if n := hits.Load(); n != 3 {
		t.Fatalf("完成该照旧推：共 %d 次，want 3", n)
	}
}
