package main

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"html"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

//go:embed web
var webRoot embed.FS

const sessionCookie = "ab_session"
const sessionTTL = 30 * 24 * time.Hour

type Config struct {
	Addr         string
	Version      string
	SecureCookie bool
	// LoginLimit / LoginWindow 控制同一来源连续输错的放行上限；0 用默认 5 次/分钟。
	LoginLimit  int
	LoginWindow time.Duration
	// LoginDelay 是每次失败后的固定延迟，用来抹平「出错快、成功慢」的时间差。
	LoginDelay time.Duration
	// Token 是 Agent / CLI 用的接口密钥；生产环境用 TokenFile 每次请求实时读取，
	// 轮换后无需重启。两者都为空时所有接口都拒绝。
	Token     string
	TokenFile string
}

type Server struct {
	store     *Store
	cfg       Config
	loginRate *loginLimiter
}

func NewServer(store *Store, cfg Config) *Server {
	return &Server{
		store:     store,
		cfg:       cfg,
		loginRate: newLoginLimiter(cfg.LoginLimit, cfg.LoginWindow),
	}
}

func (s *Server) Run() error {
	srv := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("agent-board %s listening on http://%s", s.cfg.Version, s.cfg.Addr)
	return srv.ListenAndServe()
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("GET /login", s.handleLoginPage)

	mux.HandleFunc("GET /api/health", s.authed(s.handleHealth))
	mux.HandleFunc("GET /api/me", s.authed(s.handleMe))
	mux.HandleFunc("POST /api/logout", s.authed(s.handleLogout))
	mux.HandleFunc("POST /api/board/reset", s.authed(s.handleBoardReset))
	mux.HandleFunc("POST /api/board/archive", s.authed(s.handleBoardArchive))
	mux.HandleFunc("GET /api/board", s.authed(s.handleBoard))

	mux.HandleFunc("GET /api/tasks", s.authed(s.handleTasks))
	mux.HandleFunc("POST /api/tasks", s.authed(s.handleTaskCreate))
	mux.HandleFunc("GET /api/tasks/{id}", s.authed(s.handleTaskGet))
	mux.HandleFunc("PATCH /api/tasks/{id}", s.authed(s.handleTaskMove))
	mux.HandleFunc("POST /api/tasks/{id}/events", s.authed(s.handleEventCreate))
	mux.HandleFunc("GET /api/tasks/{id}/events", s.authed(s.handleTaskEvents))

	mux.HandleFunc("GET /api/decisions", s.authed(s.handleDecisions))
	mux.HandleFunc("POST /api/decisions", s.authed(s.handleDecisionCreate))
	mux.HandleFunc("POST /api/decisions/{id}/answer", s.authed(s.handleDecisionAnswer))

	mux.HandleFunc("GET /api/replies", s.authed(s.handleReplies))
	mux.HandleFunc("PATCH /api/replies/{seq}", s.authed(s.handleReplyUpdate))
	mux.HandleFunc("GET /api/chat", s.authed(s.handleChat))
	mux.HandleFunc("POST /api/message", s.authed(s.handleMessage))
	mux.HandleFunc("GET /api/push/key", s.authed(s.handlePushKey))
	mux.HandleFunc("GET /api/push/status", s.authed(s.handlePushStatus))
	mux.HandleFunc("POST /api/push/subscribe", s.authed(s.handlePushSubscribe))
	mux.HandleFunc("POST /api/push/unsubscribe", s.authed(s.handlePushUnsubscribe))

	mux.HandleFunc("GET /api/events", s.authed(s.handleEventsLongPoll))
	mux.HandleFunc("POST /api/claim", s.authed(s.handleClaim))
	mux.HandleFunc("POST /api/ack", s.authed(s.handleAck))
	mux.HandleFunc("GET /api/consumer", s.authed(s.handleConsumer))
	mux.HandleFunc("GET /api/stream", s.authedStream(s.handleStream))
	mux.HandleFunc("GET /api/activity", s.authed(s.handleActivity))

	// 未登记的 API 也不给匿名探测的机会。
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if !s.hasToken(r) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeErr(w, http.StatusNotFound, "not found")
	})

	mux.HandleFunc("/", s.handleStatic)

	return s.withHeaders(mux)
}

func (s *Server) withHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
				"script-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// ---- auth ----

const (
	loginError   = "口令不对"
	loginLimited = "尝试过于频繁，稍后再试"
)

func (s *Server) sessionToken(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return "", false
	}
	if !s.store.SessionValid(c.Value) {
		return "", false
	}
	return c.Value, true
}

func (s *Server) hasSession(r *http.Request) bool {
	_, ok := s.sessionToken(r)
	return ok
}

// apiTokenValue 每次请求都读文件，token 轮换后立即生效。
func (s *Server) apiTokenValue() string {
	if s.cfg.Token != "" {
		return s.cfg.Token
	}
	if s.cfg.TokenFile != "" {
		if b, err := os.ReadFile(s.cfg.TokenFile); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
}

func (s *Server) validToken(token string) bool {
	want := s.apiTokenValue()
	if token == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1
}

// hasToken 只看 Agent token，不认网页登录态。
func (s *Server) hasToken(r *http.Request) bool {
	return s.validToken(bearerToken(r))
}

func (s *Server) authed(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.hasToken(r) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		fn(w, r)
	}
}

// EventSource 不能带自定义头，只对 SSE 放宽到查询参数。
func (s *Server) authedStream(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		if !s.validToken(token) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		fn(w, r)
	}
}

func (s *Server) setSession(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cfg.SecureCookie,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

func (s *Server) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cfg.SecureCookie,
		MaxAge:   -1,
	})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ip := clientIP(r)
	if blocked, wait := s.loginRate.status(ip); blocked {
		secs := int(wait.Seconds())
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		writeErr(w, http.StatusTooManyRequests, loginLimited)
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	passHash, err := s.store.metaGet("password_hash")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !verifyPassword(passHash, req.Password) {
		s.loginRate.fail(ip)
		if s.cfg.LoginDelay > 0 {
			time.Sleep(s.cfg.LoginDelay)
		}
		writeErr(w, http.StatusUnauthorized, loginError)
		return
	}
	token, err := sessionToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.store.CreateSession(token, sessionTTL); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.loginRate.reset(ip)
	s.setSession(w, token)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.hasSession(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.servePage(w, "login.html")
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token, ok := s.sessionToken(r); ok {
		_ = s.store.DeleteSession(token)
	}
	s.clearSession(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": "cookie", "version": s.cfg.Version})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.cfg.Version,
		"time": time.Now().UTC().Format(time.RFC3339)})
}

// ---- board lifecycle (AI 是唯一驱动者) ----

// POST /api/board/reset 清空一切并重置编号，相当于新会话开局。
// 必须显式 confirm："reset"，避免误触。
func (s *Server) handleBoardReset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if req.Confirm != "reset" {
		writeErr(w, http.StatusBadRequest, `confirm "reset" required`)
		return
	}
	cleared, err := s.store.ResetBoard()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cleared": cleared})
}

// POST /api/board/archive 保留历史，把所有卡移到已完成、待拍板标为已归档。
func (s *Server) handleBoardArchive(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if req.Confirm != "archive" {
		writeErr(w, http.StatusBadRequest, `confirm "archive" required`)
		return
	}
	archived, err := s.store.ArchiveBoard()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "archived": archived})
}

func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	counts, err := s.store.BoardCounts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"counts": counts})
}

// ---- tasks ----

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.store.ListTasks()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tasks == nil {
		tasks = []Task{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

func (s *Server) handleTaskCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title   string `json:"title"`
		Project string `json:"project"`
		Agent   string `json:"agent"`
		Column  string `json:"column"`
		Key     string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	t, err := s.store.CreateTask(req.Title, req.Project, req.Agent, req.Column, req.Key)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"task": t})
}

func (s *Server) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, events, err := s.store.GetTask(id)
	if err == ErrNotFound {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if events == nil {
		events = []Event{}
	}
	decisions, _ := s.store.DecisionsByTask(id)
	replies, _ := s.store.RepliesByTask(id)
	if decisions == nil {
		decisions = []Decision{}
	}
	if replies == nil {
		replies = []Reply{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"task": t, "events": events, "decisions": decisions, "replies": replies})
}

func (s *Server) handleTaskMove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Column string `json:"column"`
		Key    string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	t, err := s.store.MoveTask(r.PathValue("id"), req.Column, req.Key)
	if err == ErrNotFound {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if t.Column == "done" {
		s.notify("done", t.ID, t.Title)
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": t})
}

var allowedEventKinds = map[string]bool{
	"progress": true, "note": true, "tool": true, "artifact": true,
	"conclusion": true, "blocked": true, "dispatch": true,
	"user_reply": true, "user_reaction": true,
}

func (s *Server) handleEventCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind    string `json:"kind"`
		Text    string `json:"text"`
		Agent   string `json:"agent"`
		Author  string `json:"author"`
		Session string `json:"session"`
		Data    string `json:"data"`
		Key     string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if req.Kind == "" {
		req.Kind = "note"
	}
	if !allowedEventKinds[req.Kind] {
		writeErr(w, http.StatusBadRequest, "unknown kind: "+req.Kind)
		return
	}
	if strings.TrimSpace(req.Text) == "" && req.Data == "" {
		writeErr(w, http.StatusBadRequest, "text required")
		return
	}
	if (req.Kind == "user_reply" || req.Kind == "user_reaction") && req.Author == "" {
		req.Author = "user"
	}
	ev, err := s.store.AddEvent(r.PathValue("id"), "event", req.Kind, req.Text,
		req.Agent, req.Author, req.Session, req.Data, req.Key)
	if err == ErrNotFound {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.Kind == "blocked" {
		s.notify("blocked", ev.TaskID, req.Text)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"event": ev})
}

func (s *Server) handleTaskEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, _, err := s.store.GetTask(id); err == ErrNotFound {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	events, err := s.store.EventsByTask(id, 1000)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if events == nil {
		events = []Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// ---- decisions ----

func (s *Server) handleDecisions(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	ds, err := s.store.ListDecisions(status, 200)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ds == nil {
		ds = []Decision{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"decisions": ds})
}

func (s *Server) handleDecisionCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TaskID   string   `json:"task_id"`
		Question string   `json:"question"`
		Options  []string `json:"options"`
		Key      string   `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if len(req.Options) == 0 {
		req.Options = []string{"同意", "不同意"}
	}
	optJSON, _ := json.Marshal(req.Options)
	d, err := s.store.CreateDecision(req.TaskID, req.Question, string(optJSON), req.Key)
	if err == ErrNotFound {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.notify("decision", d.TaskID, d.Question)
	writeJSON(w, http.StatusCreated, map[string]any{"decision": d})
}

func (s *Server) handleDecisionAnswer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Answer string `json:"answer"`
		Key    string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	d, err := s.store.AnswerDecision(r.PathValue("id"), req.Answer, req.Key)
	if err == ErrNotFound {
		writeErr(w, http.StatusNotFound, "decision not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"decision": d})
}

func (s *Server) handleReplyUpdate(w http.ResponseWriter, r *http.Request) {
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad seq")
		return
	}
	var req struct {
		Status string `json:"status"`
		Agent  string `json:"agent"`
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	rp, err := s.store.UpdateReply(seq, req.Status, req.Agent, req.Note)
	if err == ErrNotFound {
		writeErr(w, http.StatusNotFound, "reply not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Status == "failed" || req.Status == "timeout" {
		reason := req.Note
		if strings.TrimSpace(reason) == "" {
			reason = rp.Text
		}
		s.notify("failed", rp.TaskID, reason)
	}
	writeJSON(w, http.StatusOK, map[string]any{"reply": rp})
}

func (s *Server) handleReplies(w http.ResponseWriter, r *http.Request) {
	s.store.SweepReplies()
	limit, _ := strconv.Atoi(r.URL.Query().Get("recent"))
	free := r.URL.Query().Get("free") == "1"
	rs, err := s.store.RecentReplies(limit, free)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"replies": rs})
}

// GET /api/chat 返回自由对话（含挂靠的卡片），按新到旧。
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("recent"))
	msgs, err := s.store.ChatMessages(limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
}

// POST /api/message 是「随手发一句」的入口：user 的消息等主会话处理，
// agent 的回话直接进对话流供用户看，不产生待办回执。
func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text   string   `json:"text"`
		From   string   `json:"from"`
		TaskID string   `json:"task_id"`
		Images []string `json:"images"`
		Key    string   `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeErr(w, http.StatusBadRequest, "text required")
		return
	}
	author := strings.TrimSpace(req.From)
	if author == "" {
		author = "user"
	}
	ev, err := s.store.AddChatMessage(req.Text, author, strings.TrimSpace(req.TaskID), req.Images, req.Key)
	if err == ErrNotFound {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	rp, _ := s.store.ReplyBySeq(ev.Seq)
	writeJSON(w, http.StatusCreated, map[string]any{"event": ev, "reply": rp})
}

// ---- event cursor / long poll / SSE ----

func (s *Server) handleEventsLongPoll(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	waitSec, _ := strconv.Atoi(q.Get("wait"))
	if waitSec < 0 {
		waitSec = 0
	}
	if waitSec > 55 {
		waitSec = 55
	}
	deadline := time.Now().Add(time.Duration(waitSec) * time.Second)
	for {
		events, err := s.store.EventsAfter(since, limit)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if len(events) > 0 {
			latest, _ := s.store.LatestSeq()
			writeJSON(w, http.StatusOK, map[string]any{
				"events": events, "next": events[len(events)-1].Seq, "latest": latest})
			return
		}
		if !time.Now().Before(deadline) {
			latest, _ := s.store.LatestSeq()
			writeJSON(w, http.StatusOK, map[string]any{
				"events": []Event{}, "next": since, "latest": latest})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(400 * time.Millisecond):
		}
	}
}

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Consumer string `json:"consumer"`
		Seq      int64  `json:"seq"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if req.Consumer == "" {
		req.Consumer = "main"
	}
	claimed, acked, err := s.store.Claim(req.Consumer, req.Seq)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !claimed {
		reason := "leased"
		if acked {
			reason = "acked"
		}
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "reason": reason})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Consumer string `json:"consumer"`
		Seq      int64  `json:"seq"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if req.Consumer == "" {
		req.Consumer = "main"
	}
	if err := s.store.Ack(req.Consumer, req.Seq); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// ack 代表这条输入已经处理完，用户侧的状态灯跟着变绿。
	if _, err := s.store.ReplyBySeq(req.Seq); err == nil {
		_, _ = s.store.UpdateReply(req.Seq, "done", "", "已处理")
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "acked_seq": req.Seq})
}

func (s *Server) handleConsumer(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "main"
	}
	acked, leaseSeq, leaseUntil, err := s.store.consumer(name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "acked_seq": acked,
		"lease_seq": leaseSeq, "lease_until": leaseUntil})
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")

	last, _ := s.store.LatestSeq()
	fmt.Fprintf(w, "event: hello\ndata: {\"seq\":%d}\n\n", last)
	fl.Flush()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case <-ticker.C:
			cur, err := s.store.LatestSeq()
			if err != nil {
				return
			}
			if cur > last {
				fmt.Fprintf(w, "event: change\ndata: {\"seq\":%d}\n\n", cur)
				fl.Flush()
				last = cur
			}
		}
	}
}

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 366 {
		days = 84
	}
	rows, err := s.store.db.Query(`
		SELECT date(created_at,'localtime') AS d, COUNT(*)
		FROM events
		WHERE date(created_at,'localtime') >= date('now','localtime',?)
		GROUP BY d ORDER BY d`, fmt.Sprintf("-%d days", days-1))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	type Day struct {
		Date  string `json:"date"`
		Count int    `json:"count"`
	}
	out := []Day{}
	for rows.Next() {
		var d Day
		if err := rows.Scan(&d.Date, &d.Count); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": out})
}

// ---- static ----

func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	next := r.URL.RequestURI()
	http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusSeeOther)
}

func isStaticAsset(p string) bool {
	switch path.Ext(p) {
	case ".css", ".js", ".png", ".svg", ".ico", ".webmanifest":
		return true
	}
	return p == "favicon.ico"
}

func webFile(name string) ([]byte, error) {
	sub, err := fs.Sub(webRoot, "web")
	if err != nil {
		return nil, err
	}
	return fs.ReadFile(sub, name)
}

func (s *Server) servePage(w http.ResponseWriter, name string) {
	b, err := webFile(name)
	if err != nil {
		http.Error(w, "404 page not found", http.StatusNotFound)
		return
	}
	if name == "index.html" {
		tag := ""
		if tok := s.apiTokenValue(); tok != "" {
			tag = `<meta name="api-token" content="` + html.EscapeString(tok) + `">`
		}
		b = []byte(strings.Replace(string(b), "<!-- api-token -->", tag, 1))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		p = "index.html"
	}
	if strings.Contains(p, "..") {
		http.NotFound(w, r)
		return
	}
	if p == "index.html" || p == "login.html" {
		if p == "index.html" && !s.hasSession(r) {
			s.redirectToLogin(w, r)
			return
		}
		s.servePage(w, p)
		return
	}
	if !isStaticAsset(p) {
		if !s.hasSession(r) {
			s.redirectToLogin(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}

	b, err := webFile(p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := mime.TypeByExtension(path.Ext(p))
	switch path.Ext(p) {
	case ".webmanifest":
		ct = "application/manifest+json"
	case ".js":
		ct = "text/javascript; charset=utf-8"
	case ".css":
		ct = "text/css; charset=utf-8"
	case ".svg":
		ct = "image/svg+xml"
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	// 前端是内嵌的；本地单用户工具不必省这几次请求，避免改完看不到效果。
	switch path.Ext(p) {
	case ".png", ".svg", ".ico":
		w.Header().Set("Cache-Control", "public, max-age=3600")
	default:
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Write(b)
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
