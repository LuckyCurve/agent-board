package main

import (
	"net/http"
	"strings"
	"testing"
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
		{"progress", "跑了一半", false, ""},
		{"note", "随手记", false, ""},
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
