package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/hkdf"
)

// 推的链路要完整：订阅存下来 → 服务端按 RFC 8291 加密并用 VAPID 签名 →
// 推送端点收到的东西能按浏览器那套算法解回原文。
func TestPushChainDeliversSignedEncryptedNotification(t *testing.T) {
	e := newTestEnv(t)

	// 假装是浏览器：自己生成一对密钥，p256dh 给服务端，私钥留着解密。
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p256dh := base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes())
	authRaw := make([]byte, 16)
	if _, err := rand.Read(authRaw); err != nil {
		t.Fatal(err)
	}
	auth := base64.RawURLEncoding.EncodeToString(authRaw)

	type got struct {
		header http.Header
		body   []byte
	}
	gotCh := make(chan got, 4)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotCh <- got{header: r.Header.Clone(), body: b}
		w.WriteHeader(http.StatusCreated)
	}))
	defer fake.Close()

	if err := e.store.SavePushSub(fake.URL, p256dh, auth); err != nil {
		t.Fatalf("存订阅: %v", err)
	}
	if n, err := e.store.PushSubCount(); err != nil || n != 1 {
		t.Fatalf("订阅数 = %d (%v)", n, err)
	}

	// 触发一次「等你拍板」推送。
	id := e.createTask(t, "推送链路", "todo")
	code, body, _ := e.req(t, "POST", "/api/decisions", map[string]any{
		"task_id": id, "question": "这条能收到吗？", "options": []string{"能", "不能"},
	})
	if code != http.StatusCreated {
		t.Fatalf("建待拍板: %d %v", code, body)
	}

	var g got
	select {
	case g = <-gotCh:
	case <-time.After(5 * time.Second):
		t.Fatal("推送端点没收到请求")
	}

	authz := g.header.Get("Authorization")
	if !strings.HasPrefix(authz, "vapid ") || !strings.Contains(authz, "t=") ||
		!strings.Contains(authz, "k=") {
		t.Fatalf("Authorization 不是 VAPID 签名：%q", authz)
	}
	if g.header.Get("TTL") == "" {
		t.Fatal("缺少 TTL 头")
	}
	if g.header.Get("Content-Encoding") != "aes128gcm" {
		t.Fatalf("Content-Encoding = %q", g.header.Get("Content-Encoding"))
	}
	// Chrome/iOS 只认这个格式：盐在 Encryption 头，服务端公钥在 Crypto-Key 头，正文只有密文。
	if !strings.HasPrefix(g.header.Get("Encryption"), "salt=") {
		t.Fatalf("Encryption 头不对：%q", g.header.Get("Encryption"))
	}
	if !strings.HasPrefix(g.header.Get("Crypto-Key"), "dh=") {
		t.Fatalf("Crypto-Key 头不对：%q", g.header.Get("Crypto-Key"))
	}

	plain, err := decryptPush(priv, authRaw, g.header, g.body)
	if err != nil {
		t.Fatalf("按浏览器的算法解密失败: %v", err)
	}
	var msg pushMessage
	if err := json.Unmarshal(plain, &msg); err != nil {
		t.Fatalf("推送正文不是 JSON: %v (%s)", err, plain)
	}
	if !strings.Contains(msg.Title, "等你拍板") {
		t.Fatalf("标题 = %q", msg.Title)
	}
	if msg.Body != "这条能收到吗？" {
		t.Fatalf("正文 = %q", msg.Body)
	}
	if msg.TaskID != id {
		t.Fatalf("task_id = %q, want %s", msg.TaskID, id)
	}
}

// decryptPush 站在浏览器/service worker 那头解密：盐在头里，公钥也在头里。
func decryptPush(priv *ecdh.PrivateKey, authSecret []byte, h http.Header, body []byte) ([]byte, error) {
	saltB64 := strings.TrimPrefix(h.Get("Encryption"), "salt=")
	salt, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(saltB64, "="))
	if err != nil {
		return nil, err
	}
	asPubB64 := strings.TrimPrefix(h.Get("Crypto-Key"), "dh=")
	asPub, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(asPubB64, "="))
	if err != nil {
		return nil, err
	}
	asRemote, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		return nil, err
	}
	shared, err := priv.ECDH(asRemote)
	if err != nil {
		return nil, err
	}
	uaPub := priv.PublicKey().Bytes()
	info := make([]byte, 0, len("WebPush: info\x00")+len(uaPub)+len(asPub))
	info = append(info, "WebPush: info\x00"...)
	info = append(info, uaPub...)
	info = append(info, asPub...)
	ikm := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, shared, authSecret, info), ikm); err != nil {
		return nil, err
	}
	cek := make([]byte, 16)
	if _, err := io.ReadFull(hkdf.New(sha256.New, ikm, salt, []byte("Content-Encoding: aes128gcm\x00")), cek); err != nil {
		return nil, err
	}
	nonce := make([]byte, 12)
	if _, err := io.ReadFull(hkdf.New(sha256.New, ikm, salt, []byte("Content-Encoding: nonce\x00")), nonce); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, body, []byte("aes128gcm"))
	if err != nil {
		return nil, err
	}
	// 去掉填充分隔符 0x02 及其后的填充。
	for i := len(plain) - 1; i >= 0; i-- {
		if plain[i] == 0x02 {
			return plain[:i], nil
		}
	}
	return plain, nil
}
