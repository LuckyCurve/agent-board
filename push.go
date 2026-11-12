package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/hkdf"
)

// pushMessage 是推到通知栏的内容；task_id 让点击通知能回到对应卡片。
type pushMessage struct {
	Title  string `json:"title"`
	Body   string `json:"body"`
	TaskID string `json:"task_id,omitempty"`
}

// pushMessageFor 决定一件事值不值得打扰：只有要人动手或要人知道的才推。
// 进展也推——用户要的是「它在动」，不是等到最后才知道卡住了。
func pushMessageFor(kind, taskID, text string) (pushMessage, bool) {
	var label string
	switch kind {
	case "decision":
		label = "等你拍板"
	case "done":
		label = "完成"
	case "blocked":
		label = "阻塞"
	case "failed":
		label = "失败"
	case "progress":
		label = "进展"
	default:
		return pushMessage{}, false
	}
	body := truncateRunes(strings.TrimSpace(text), 120)
	if body == "" {
		body = label
	}
	title := label
	if taskID != "" {
		title = taskID + " " + label
	}
	return pushMessage{Title: title, Body: body, TaskID: taskID}, true
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

var (
	vapidMu   sync.Mutex
	vapidPriv string
	vapidPub  string
)

// ensureVAPID 首次用时生成密钥并落库，重启后沿用；订阅者不会因此失效。
// 私钥存 base64url 的标量，公钥存 base64url 的非压缩点，和浏览器端一致。
func (s *Server) ensureVAPID() (priv, pub string, err error) {
	vapidMu.Lock()
	defer vapidMu.Unlock()
	if vapidPriv != "" && vapidPub != "" {
		return vapidPriv, vapidPub, nil
	}
	priv, err = s.store.metaGet("vapid_private")
	if err == ErrNotFound {
		priv, err = "", nil
	}
	if err != nil {
		return "", "", err
	}
	pub, err = s.store.metaGet("vapid_public")
	if err == ErrNotFound {
		pub, err = "", nil
	}
	if err != nil {
		return "", "", err
	}
	if priv == "" || pub == "" {
		priv, pub, err = generateVAPID()
		if err != nil {
			return "", "", err
		}
		if err = s.store.metaSet("vapid_private", priv); err != nil {
			return "", "", err
		}
		if err = s.store.metaSet("vapid_public", pub); err != nil {
			return "", "", err
		}
	}
	vapidPriv, vapidPub = priv, pub
	return priv, pub, nil
}

func generateVAPID() (priv, pub string, err error) {
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.RawURLEncoding.EncodeToString(k.Bytes()),
		base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), nil
}

// notify 异步把通知发给所有订阅端；失效订阅（404/410）顺手删掉。
func (s *Server) notify(kind, taskID, text string) {
	msg, ok := pushMessageFor(kind, taskID, text)
	if !ok {
		return
	}
	// 进展本来就会连着来，同一张卡隔一会儿再说一次就够了。
	if kind == "progress" && !s.allowProgressPush(taskID) {
		return
	}
	go s.sendPush(msg)
}

// 同一张卡的进展，5 分钟内只推第一条。结论、阻塞、失败照旧条条推。
const progressPushEvery = 5 * time.Minute

func (s *Server) allowProgressPush(taskID string) bool {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	now := time.Now()
	if last, ok := s.lastProgress[taskID]; ok && now.Sub(last) < progressPushEvery {
		return false
	}
	// 顺手清掉过期的，别让这个表跟着卡片数一直长。
	for k, t := range s.lastProgress {
		if now.Sub(t) > progressPushEvery {
			delete(s.lastProgress, k)
		}
	}
	s.lastProgress[taskID] = now
	return true
}

// sendPush 发给所有订阅端，返回成功台数、失败台数和第一条失败原因。
func (s *Server) sendPush(msg pushMessage) (sent, failed int, reason string) {
	priv, pub, err := s.ensureVAPID()
	if err != nil {
		log.Printf("push: vapid: %v", err)
		return 0, 1, "服务器缺少推送密钥"
	}
	subs, err := s.store.PushSubs()
	if err != nil {
		log.Printf("push: subs: %v", err)
		return 0, 0, ""
	}
	if len(subs) == 0 {
		return 0, 0, ""
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return 0, 0, ""
	}
	client := &http.Client{Timeout: 10 * time.Second}
	subject := s.vapidSubject()
	for _, sub := range subs {
		resp, err := sendWebPush(client, payload, sub.Endpoint, sub.P256dh, sub.Auth, pub, priv, subject)
		if err != nil {
			log.Printf("push: %s 发送失败: %v", pushHost(sub.Endpoint), err)
			failed++
			if reason == "" {
				reason = "连不上推送服务"
			}
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			// 推送服务会说明原因（密钥不符、订阅过期、格式不对），记进日志才查得动。
			log.Printf("push: %s 拒绝了这条通知: status=%d body=%s",
				pushHost(sub.Endpoint), resp.StatusCode, redactPushText(string(body)))
			failed++
			if reason == "" {
				reason = pushRejectReason(resp.StatusCode, string(body))
			}
			if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
				_ = s.store.DeletePushSub(sub.Endpoint)
			}
			continue
		}
		sent++
	}
	return sent, failed, reason
}

// pushHost 只留推送服务的主机名，完整 endpoint 不进日志。
func pushHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "push"
	}
	return u.Host
}

// pushRejectReason 把推送服务的状态码翻成人话。
// 订阅失效才让用户重新订阅；服务端自己的问题别往用户手机上推。
func pushRejectReason(code int, body string) string {
	switch {
	case code == http.StatusNotFound || code == http.StatusGone:
		return "这个订阅已经失效：在手机上把铃铛关掉再打开，重新订阅一次"
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return "服务端的通知密钥有问题，不是你手机的问题，不用重新订阅"
	case code == http.StatusBadRequest:
		return "推送服务说这条通知格式不对，是服务端的问题，不用重新订阅"
	case code >= 500:
		return "推送服务暂时不可用，不是你手机的问题，过会儿再试"
	default:
		return fmt.Sprintf("推送服务回了 %d，先不用动手机，稍后再试一次", code)
	}
}

// redactPushText 把响应正文里的长串（令牌之类）打码，日志里只留人话。
func redactPushText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(空)"
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return longToken.ReplaceAllString(s, "…")
}

// longToken 匹配响应正文里像令牌的长串。
var longToken = regexp.MustCompile(`[A-Za-z0-9._/-]{24,}`)

// sendWebPush 按 RFC 8291 加密正文、按 VAPID 签名，POST 给推送服务。
// 盐放 Encryption 头、服务端公钥放 Crypto-Key 头，正文只有密文——
// Chrome（FCM）和 iOS 只认这个格式。
func sendWebPush(client *http.Client, payload []byte, endpoint, p256dh, auth, vapidPub, vapidPriv, sub string) (*http.Response, error) {
	body, salt, asPub, err := encryptPush(payload, p256dh, auth)
	if err != nil {
		return nil, err
	}
	header, err := vapidHeader(endpoint, vapidPub, vapidPriv, sub, time.Now().Add(12*time.Hour))
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", "3600")
	req.Header.Set("Urgency", "high")
	req.Header.Set("Encryption", "salt="+salt)
	req.Header.Set("Crypto-Key", "dh="+asPub)
	req.Header.Set("Authorization", header)
	return client.Do(req)
}

// encryptPush 是浏览器那头的解密过程的反面：RFC 8291 + RFC 8188。
func encryptPush(payload []byte, p256dh, auth string) (body []byte, saltB64, asPubB64 string, err error) {
	uaPub, err := b64Decode(p256dh)
	if err != nil {
		return nil, "", "", fmt.Errorf("p256dh: %w", err)
	}
	authSecret, err := b64Decode(auth)
	if err != nil {
		return nil, "", "", fmt.Errorf("auth: %w", err)
	}
	remote, err := ecdh.P256().NewPublicKey(uaPub)
	if err != nil {
		return nil, "", "", fmt.Errorf("p256dh: %w", err)
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, "", "", err
	}
	eph, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", "", err
	}
	shared, err := eph.ECDH(remote)
	if err != nil {
		return nil, "", "", err
	}
	asPub := eph.PublicKey().Bytes()

	info := make([]byte, 0, len("WebPush: info\x00")+len(uaPub)+len(asPub))
	info = append(info, "WebPush: info\x00"...)
	info = append(info, uaPub...)
	info = append(info, asPub...)
	ikm := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, shared, authSecret, info), ikm); err != nil {
		return nil, "", "", err
	}
	cek := make([]byte, 16)
	if _, err := io.ReadFull(hkdf.New(sha256.New, ikm, salt, []byte("Content-Encoding: aes128gcm\x00")), cek); err != nil {
		return nil, "", "", err
	}
	nonce := make([]byte, 12)
	if _, err := io.ReadFull(hkdf.New(sha256.New, ikm, salt, []byte("Content-Encoding: nonce\x00")), nonce); err != nil {
		return nil, "", "", err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, "", "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, "", "", err
	}
	// 末尾放填充分隔符 0x02，收端据此去掉尾部填充。
	plain := make([]byte, 0, len(payload)+1)
	plain = append(plain, payload...)
	plain = append(plain, 0x02)
	body = gcm.Seal(nil, nonce, plain, []byte("aes128gcm"))
	return body, base64.RawURLEncoding.EncodeToString(salt), base64.RawURLEncoding.EncodeToString(asPub), nil
}

// vapidSubject 决定 VAPID 里的联系人地址：优先用配置（VAPID_SUB），
// 其次按站点地址推一个，最后用一个一定能用的默认值。
func (s *Server) vapidSubject() string {
	configured := s.cfg.VapidSub
	if configured == "" {
		configured = envOr("VAPID_SUB", "")
	}
	return vapidSubject(configured, envOr("BOARD_URL", ""))
}

// vapidSubject 是推送服务里留的联系人地址。Apple 会拒掉不合法的主机名
// （localhost 就不行），所以这里只给一个一定能用的默认值，站点再按需覆盖。
func vapidSubject(configured, baseURL string) string {
	s := strings.TrimSpace(configured)
	if isVapidSubject(s) {
		return s
	}
	if u, err := url.Parse(baseURL); err == nil && u.Hostname() != "" && u.Hostname() != "localhost" {
		return "mailto:admin@" + u.Hostname()
	}
	return "mailto:admin@example.com"
}

// isVapidSubject 只认 mailto: 和 https: 两种形式。
func isVapidSubject(s string) bool {
	return strings.HasPrefix(s, "mailto:") || strings.HasPrefix(s, "https:")
}

// vapidHeader 生成 "vapid t=<jwt>, k=<公钥>"，JWT 用 ES256 签，aud 是推送服务的来源。
func vapidHeader(endpoint, vapidPub, vapidPriv, sub string, exp time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	privBytes, err := b64Decode(vapidPriv)
	if err != nil {
		return "", fmt.Errorf("vapid private: %w", err)
	}
	d := new(big.Int).SetBytes(privBytes)
	x, y := elliptic.P256().ScalarBaseMult(privBytes)
	key := &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y},
		D:         d,
	}
	head := b64JSON(map[string]any{"typ": "JWT", "alg": "ES256"})
	claims := b64JSON(map[string]any{
		"aud": u.Scheme + "://" + u.Host,
		"exp": exp.Unix(),
		"sub": sub,
	})
	signing := head + "." + claims
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return "vapid t=" + signing + "." + base64.RawURLEncoding.EncodeToString(sig) +
		", k=" + vapidPub, nil
}

func b64JSON(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

// b64Decode 浏览器给的 base64url 可能带填充也可能不带，两种都收。
func b64Decode(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "=")); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

// GET /api/push/key 把公钥给前端，用来 pushManager.subscribe。
func (s *Server) handlePushKey(w http.ResponseWriter, r *http.Request) {
	_, pub, err := s.ensureVAPID()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "push unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": pub})
}

// GET /api/push/status 告诉前端当前是否开了推送、有几台设备。
func (s *Server) handlePushStatus(w http.ResponseWriter, r *http.Request) {
	n, err := s.store.PushSubCount()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	msg := "还没有设备订阅通知：在手机上点铃铛开启"
	if n > 0 {
		msg = fmt.Sprintf("%d 台设备已订阅通知", n)
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": n > 0, "devices": n, "message": msg})
}

// POST /api/push/test 给所有订阅设备发一条固定文案，让用户自己确认手机收不收得到。
func (s *Server) handlePushTest(w http.ResponseWriter, r *http.Request) {
	n, err := s.store.PushSubCount()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"devices": 0, "sent": 0, "failed": 0,
			"message": "还没有设备订阅通知：先在手机上点铃铛开启",
		})
		return
	}
	sent, failed, reason := s.sendPush(pushMessage{
		Title: "Agent Board",
		Body:  "这是一条测试通知，收到即代表手机推送可用",
	})
	msg := "测试通知已发出"
	switch {
	case sent == 0:
		msg = "测试通知没发出去"
		if reason != "" {
			msg += "：" + reason
		}
	case failed > 0:
		msg = fmt.Sprintf("测试通知发出 %d 台，失败 %d 台", sent, failed)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"devices": n, "sent": sent, "failed": failed, "message": msg,
	})
}

func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if strings.TrimSpace(req.Endpoint) == "" || req.Keys.P256dh == "" || req.Keys.Auth == "" {
		writeErr(w, http.StatusBadRequest, "endpoint and keys required")
		return
	}
	if !strings.HasPrefix(req.Endpoint, "https://") {
		writeErr(w, http.StatusBadRequest, "endpoint must be https")
		return
	}
	if err := s.store.SavePushSub(req.Endpoint, req.Keys.P256dh, req.Keys.Auth); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true})
}

// POST /api/push/unsubscribe 一键关掉：清掉所有设备，不保留偏好。
func (s *Server) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if err := s.store.ClearPushSubs(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
}
