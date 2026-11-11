package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// pushMessage 是推到通知栏的内容；task_id 让点击通知能回到对应卡片。
type pushMessage struct {
	Title  string `json:"title"`
	Body   string `json:"body"`
	TaskID string `json:"task_id,omitempty"`
}

// pushMessageFor 决定一件事值不值得打扰：只有要人动手或要人知道的才推。
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
		priv, pub, err = webpush.GenerateVAPIDKeys()
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

// notify 异步把通知发给所有订阅端；失效订阅（404/410）顺手删掉。
func (s *Server) notify(kind, taskID, text string) {
	msg, ok := pushMessageFor(kind, taskID, text)
	if !ok {
		return
	}
	go s.sendPush(msg)
}

func (s *Server) sendPush(msg pushMessage) {
	priv, pub, err := s.ensureVAPID()
	if err != nil {
		log.Printf("push: vapid: %v", err)
		return
	}
	subs, err := s.store.PushSubs()
	if err != nil {
		log.Printf("push: subs: %v", err)
		return
	}
	if len(subs) == 0 {
		return
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	for _, sub := range subs {
		resp, err := webpush.SendNotification(payload, &webpush.Subscription{
			Endpoint: sub.Endpoint,
			Keys:     webpush.Keys{Auth: sub.Auth, P256dh: sub.P256dh},
		}, &webpush.Options{
			Subscriber:      "mailto:agent-board@localhost",
			VAPIDPublicKey:  pub,
			VAPIDPrivateKey: priv,
			TTL:             3600,
			Urgency:         webpush.UrgencyHigh,
			HTTPClient:      client,
		})
		if err != nil {
			log.Printf("push: send: %v", err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
			_ = s.store.DeletePushSub(sub.Endpoint)
		}
	}
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
	writeJSON(w, http.StatusOK, map[string]any{"enabled": n > 0, "devices": n})
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
