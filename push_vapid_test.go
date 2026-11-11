package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Apple 会拒掉 host 是不合法域名的 VAPID（localhost 就不行），sub 得是像样的地址。
func TestVapidSubjectIsUsable(t *testing.T) {
	cases := []struct {
		name       string
		configured string
		baseURL    string
		want       string
	}{
		{"默认不留 localhost", "", "", "mailto:admin@example.com"},
		{"配置优先", "mailto:me@example.org", "https://x.example.com", "mailto:me@example.org"},
		{"https 也认", "https://example.com/contact", "", "https://example.com/contact"},
		{"按站点地址推一个", "", "https://43-165-128-180.sslip.io", "mailto:admin@43-165-128-180.sslip.io"},
		{"localhost 站点不推", "", "http://localhost:8765", "mailto:admin@example.com"},
		{"乱写的配置丢掉", "not-a-uri", "", "mailto:admin@example.com"},
	}
	for _, c := range cases {
		got := vapidSubject(c.configured, c.baseURL)
		if got != c.want {
			t.Errorf("%s: sub = %q, want %q", c.name, got, c.want)
		}
		if !isVapidSubject(got) {
			t.Errorf("%s: sub %q 不是 mailto:/https:", c.name, got)
		}
		if strings.Contains(got, "localhost") {
			t.Errorf("%s: sub %q 里不该有 localhost", c.name, got)
		}
	}
}

// 生成的 JWT 里，sub 就是上面那套；签名对不对另说。
func TestVapidHeaderCarriesSubject(t *testing.T) {
	priv, pub, err := generateVAPID()
	if err != nil {
		t.Fatal(err)
	}
	sub := vapidSubject("mailto:ops@example.com", "")
	header, err := vapidHeader("https://web.push.apple.com/abc", pub, priv, sub, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(header, "vapid t=") || !strings.Contains(header, ", k=") {
		t.Fatalf("Authorization 格式不对: %q", header)
	}
	token := strings.TrimPrefix(strings.SplitN(header, ", k=", 2)[0], "vapid t=")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT 段数不对: %d", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	if claims["sub"] != "mailto:ops@example.com" {
		t.Fatalf("sub = %v", claims["sub"])
	}
	if claims["aud"] != "https://web.push.apple.com" {
		t.Fatalf("aud = %v", claims["aud"])
	}
}

// 403 之类的服务端问题，别把用户往「重新订阅」上引。
func TestPushRejectReasonTellsRightParty(t *testing.T) {
	if r := pushRejectReason(404, ""); !strings.Contains(r, "重新订阅") {
		t.Fatalf("404 的说法该让用户重新订阅: %q", r)
	}
	if r := pushRejectReason(403, ""); !strings.Contains(r, "不是你手机") {
		t.Fatalf("403 的说法该说明不是手机的问题: %q", r)
	}
	if r := pushRejectReason(500, ""); !strings.Contains(r, "不是你手机") {
		t.Fatalf("5xx 的说法该说明不是手机的问题: %q", r)
	}
}
