package main

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
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
	// ImagesDir 存消息/事件里的图片；留空时退回临时目录（测试用）。
	ImagesDir string
	// ImageMaxBytes 单张上限；0 用默认 5MB。
	ImageMaxBytes int64
	// DeployTrigger 是「立即更新」的触发文件路径；留空表示本机不提供这个功能。
	DeployTrigger string
	// VapidSub 是推送服务里的联系人地址（mailto: 或 https: 开头）；留空用默认。
	VapidSub string
}

type Server struct {
	store     *Store
	cfg       Config
	loginRate *loginLimiter
	imagesDir string
}

func NewServer(store *Store, cfg Config) *Server {
	dir := cfg.ImagesDir
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "agent-board-images")
	}
	return &Server{
		store:     store,
		cfg:       cfg,
		loginRate: newLoginLimiter(cfg.LoginLimit, cfg.LoginWindow),
		imagesDir: dir,
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
	mux.HandleFunc("DELETE /api/tasks/{id}", s.authed(s.handleTaskDelete))
	mux.HandleFunc("POST /api/tasks/{id}/events", s.authed(s.handleEventCreate))
	mux.HandleFunc("GET /api/tasks/{id}/events", s.authed(s.handleTaskEvents))
	mux.HandleFunc("POST /api/tasks/{id}/tags", s.authed(s.handleTaskTags))
	mux.HandleFunc("GET /api/agents", s.authed(s.handleAgents))

	mux.HandleFunc("GET /api/decisions", s.authed(s.handleDecisions))
	mux.HandleFunc("POST /api/decisions", s.authed(s.handleDecisionCreate))
	mux.HandleFunc("POST /api/decisions/{id}/answer", s.authed(s.handleDecisionAnswer))

	mux.HandleFunc("GET /api/replies", s.authed(s.handleReplies))
	mux.HandleFunc("PATCH /api/replies/{seq}", s.authed(s.handleReplyUpdate))
	mux.HandleFunc("GET /api/chat", s.authed(s.handleChat))
	mux.HandleFunc("POST /api/message", s.authed(s.handleMessage))
	mux.HandleFunc("POST /api/images", s.authed(s.handleImageUpload))
	mux.HandleFunc("GET /images/{name}", s.authedMedia(s.handleImageGet))
	mux.HandleFunc("GET /api/push/key", s.authed(s.handlePushKey))
	mux.HandleFunc("GET /api/push/status", s.authed(s.handlePushStatus))
	mux.HandleFunc("POST /api/push/test", s.authed(s.handlePushTest))
	mux.HandleFunc("POST /api/push/subscribe", s.authed(s.handlePushSubscribe))
	mux.HandleFunc("POST /api/push/unsubscribe", s.authed(s.handlePushUnsubscribe))
	mux.HandleFunc("POST /api/deploy", s.authed(s.handleDeploy))

	mux.HandleFunc("GET /api/events", s.authed(s.handleEventsLongPoll))
	mux.HandleFunc("POST /api/claim", s.authed(s.handleClaim))
	mux.HandleFunc("POST /api/ack", s.authed(s.handleAck))
	mux.HandleFunc("GET /api/consumer", s.authed(s.handleConsumer))
	mux.HandleFunc("GET /api/stream", s.authedStream(s.handleStream))
	mux.HandleFunc("GET /api/activity", s.authed(s.handleActivity))

	// 未登记的 API 也不给匿名探测的机会。
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if s.callerAuth(r) == authNone {
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
				"script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		// 只在确实是 HTTPS 时下发，免得本机 http 开发被浏览器记死。
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			h.Set("Strict-Transport-Security", "max-age=15552000; includeSubDomains")
		}
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

// apiTokenValues 每次请求都读文件，token 轮换后立即生效。
// 文件第一行是当前令牌，后面几行是轮换前的旧令牌（宽限期内仍可用），
// 这样轮换是一次原子写，CLI 不会在换的瞬间全 401。
func (s *Server) apiTokenValues() []string {
	if s.cfg.Token != "" {
		return []string{s.cfg.Token}
	}
	if s.cfg.TokenFile == "" {
		return nil
	}
	b, err := os.ReadFile(s.cfg.TokenFile)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if t := strings.TrimSpace(line); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
}

func (s *Server) validToken(token string) bool {
	if token == "" {
		return false
	}
	ok := false
	for _, want := range s.apiTokenValues() {
		// 不提前 return，别让响应时间暴露匹配到第几行。
		if subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1 {
			ok = true
		}
	}
	return ok
}

// hasToken 只看 Agent token，不认网页登录态。
// 主令牌和绑定了 Agent 的令牌都算数，后者的权限另算。
func (s *Server) hasToken(r *http.Request) bool {
	token := bearerToken(r)
	if token == "" {
		return false
	}
	if s.validToken(token) {
		return true
	}
	_, ok, err := s.store.AgentForToken(sessionHash(token))
	return err == nil && ok
}

// callerAuth 记下这次请求是靠什么进来的。
// 网页用会话 cookie，CLI / Agent 用 Bearer token，两种都合法；
// 区别只在写操作：cookie 会被浏览器自动带上，所以要多一道 Origin 校验。
type callerAuth int

const (
	authNone callerAuth = iota
	authToken
	authCookie
)

func (s *Server) callerAuth(r *http.Request) callerAuth {
	if s.hasToken(r) {
		return authToken
	}
	if s.hasSession(r) {
		return authCookie
	}
	return authNone
}

func isWriteMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

// sameOriginRequest 是 CSRF 的第二道闸。浏览器的跨站请求一定会带 Origin，
// 所以只要带了就必须和本站同源；不带 Origin 的（同源表单、CLI）交给
// SameSite=Lax 兜底——跨站带 cookie 的 POST 到不了这里。
func sameOriginRequest(r *http.Request) bool {
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
	}
	if src == "" {
		return true
	}
	u, err := url.Parse(src)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// identityHeader 是请求里的 Agent 身份，和令牌解耦：
// 同一把令牌可以代表不同 Agent，服务端只按这个身份收窄数据。
// 留空或写 main 表示主会话，看全量。
const identityHeader = "X-Agent-Identity"

// mainIdentity 是主会话身份；它不是某个 Agent 的名字，而是「不设限」。
const mainIdentity = "main"

// agentScope 决定这个请求能碰哪些 Agent 的卡。
// 身份头只对主会话本人有效（网页登录态或主令牌）——绑定了 Agent 的令牌
// 换个头就能看别人的卡，那等于没有隔离。
func (s *Server) agentScope(r *http.Request) string {
	if s.isMainCaller(r) {
		if id := strings.TrimSpace(r.Header.Get(identityHeader)); id != "" {
			if strings.EqualFold(id, mainIdentity) {
				return ""
			}
			return id
		}
		return ""
	}
	token := bearerToken(r)
	if token == "" {
		return ""
	}
	agent, ok, err := s.store.AgentForToken(sessionHash(token))
	if err != nil || !ok {
		return ""
	}
	return agent
}

// isMainCaller：网页登录态，或持有主令牌。绑定令牌不算——它换个身份头
// 就能看别人的卡，那等于没有隔离。
// 同时带了令牌和 cookie 时以令牌为准：否则绑定令牌只要顺手带上网页会话
// 就能升级成主会话，隔离形同虚设。
func (s *Server) isMainCaller(r *http.Request) bool {
	if tok := bearerToken(r); tok != "" {
		return s.validToken(tok)
	}
	return s.hasSession(r)
}

// ownsTask 判断请求能不能动这张卡。
func (s *Server) ownsTask(w http.ResponseWriter, r *http.Request, taskID string) bool {
	agent := s.agentScope(r)
	if agent == "" {
		return true
	}
	owner, err := s.store.TaskOwner(taskID)
	if err != nil || owner != agent {
		writeErr(w, http.StatusForbidden, "这张卡不归你管")
		return false
	}
	return true
}

// authed 是 /api/* 的统一闸门：网页会话 cookie 或 Agent token，二选一。
// 写操作若靠 cookie 进来，还要过同源校验——cookie 是浏览器自动带的，
// 光靠它挡不住跨站提交。
func (s *Server) authed(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := s.callerAuth(r)
		if auth == authNone {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if auth == authCookie && isWriteMethod(r.Method) && !sameOriginRequest(r) {
			writeErr(w, http.StatusForbidden, "跨站请求已拒绝")
			return
		}
		fn(w, r)
	}
}

// EventSource 不能带自定义头，只对 SSE 放宽到查询参数。
func (s *Server) authedStream(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.hasSession(r) {
			fn(w, r)
			return
		}
		token := bearerToken(r)
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		if !s.validToken(token) {
			if agent, ok, err := s.store.AgentForToken(sessionHash(token)); err != nil || !ok || agent == "" {
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
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

// clientIP 取真实来源地址。服务只该被反向代理访问，代理后面 RemoteAddr
// 永远是 127.0.0.1，所以只信任来自本机的 X-Forwarded-For。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if host != "127.0.0.1" && host != "::1" {
		return host
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// 取最右一段：它是最近一跳代理看到的地址，客户端无法伪造。
		parts := strings.Split(xff, ",")
		if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
			return ip
		}
	}
	if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
		return ip
	}
	return host
}

// loginBucket 把内存桶和落库记录合起来判断：内存桶负责快速拒绝，
// 落库记录保证重启之后别人仍然打不进来。
func (s *Server) loginBucket(ip string) (bool, time.Duration) {
	if blocked, wait := s.loginRate.status(ip); blocked {
		return true, wait
	}
	n, err := s.store.LoginFailuresSince(ip, time.Now().Add(-s.loginRate.window))
	if err == nil && n >= s.loginRate.limit {
		return true, s.loginRate.window
	}
	return false, 0
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ip := clientIP(r)
	if blocked, wait := s.loginBucket(ip); blocked {
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
		_ = s.store.AddLoginFailure(ip, time.Now())
		log.Printf("login: 失败 ip=%s", ip)
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
	_ = s.store.ClearLoginFailures(ip)
	log.Printf("login: 成功 ip=%s", ip)
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
	// 看板计数也按身份收窄，别让 Agent 从数量上看出别人有多少活。
	agent := s.agentScope(r)
	if agent != "" {
		counts, err := s.store.BoardCountsFor(agent)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"counts": counts})
		return
	}
	counts, err := s.store.BoardCounts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"counts": counts})
}

// ---- tasks ----

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.store.ListTasks(r.URL.Query().Get("view") == "archived", s.agentScope(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tasks == nil {
		tasks = []Task{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

// handleAgents 列出所有 Agent，页面右上角的切换器用。
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := s.store.ListAgents()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if agents == nil {
		agents = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
}

// handleTaskTags 给卡片加/删标签。
func (s *Server) handleTaskTags(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.ownsTask(w, r, id) {
		return
	}
	var req struct {
		Add    []string `json:"add"`
		Remove []string `json:"remove"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if _, err := s.store.AddTaskTags(id, req.Add, req.Remove); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	t, _, err := s.store.GetTask(id)
	if err == ErrNotFound {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": t})
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
	// 绑定到某个 Agent 的令牌，建出来的卡就归它。
	if agent := s.agentScope(r); agent != "" {
		req.Agent = agent
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
	if !s.ownsTask(w, r, id) {
		return
	}
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
	id := r.PathValue("id")
	if !s.ownsTask(w, r, id) {
		return
	}
	var req struct {
		Column string `json:"column"`
		Agent  string `json:"agent"`
		Key    string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	// 转交给别的 Agent：留下一条事件说明来龙去脉。
	if req.Agent != "" {
		if agent := s.agentScope(r); agent != "" && agent != req.Agent {
			writeErr(w, http.StatusForbidden, "不能把卡转交给别的 Agent")
			return
		}
		t, err := s.store.AssignTask(id, req.Agent, req.Key)
		if err == ErrNotFound {
			writeErr(w, http.StatusNotFound, "task not found")
			return
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"task": t})
		return
	}
	t, err := s.store.MoveTask(id, req.Column, req.Key)
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

func (s *Server) handleTaskDelete(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("confirm") != "yes" {
		writeErr(w, http.StatusBadRequest, "删除要二次确认：confirm=yes")
		return
	}
	id := r.PathValue("id")
	if !s.ownsTask(w, r, id) {
		return
	}
	if _, _, err := s.store.GetTask(id); err == ErrNotFound {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	err := s.store.DeleteTask(id)
	if err == ErrNotFound {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	var open *OpenItemError
	if errors.As(err, &open) {
		writeErr(w, http.StatusConflict, "不能删除："+open.Reason)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

var allowedEventKinds = map[string]bool{
	"progress": true, "note": true, "tool": true, "artifact": true,
	"conclusion": true, "blocked": true, "dispatch": true,
	"user_reply": true, "user_reaction": true,
}

func (s *Server) handleEventCreate(w http.ResponseWriter, r *http.Request) {
	if !s.ownsTask(w, r, r.PathValue("id")) {
		return
	}
	var req struct {
		Kind    string   `json:"kind"`
		Text    string   `json:"text"`
		Agent   string   `json:"agent"`
		Author  string   `json:"author"`
		Session string   `json:"session"`
		Data    string   `json:"data"`
		Images  []string `json:"images"`
		Key     string   `json:"key"`
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
	if strings.TrimSpace(req.Text) == "" && req.Data == "" && len(req.Images) == 0 {
		writeErr(w, http.StatusBadRequest, "text or image required")
		return
	}
	if err := s.validateImageIDs(req.Images); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	data, err := mergeImagesIntoData(req.Data, req.Images)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if (req.Kind == "user_reply" || req.Kind == "user_reaction") && req.Author == "" {
		req.Author = "user"
	}
	ev, err := s.store.AddEvent(r.PathValue("id"), "event", req.Kind, req.Text,
		req.Agent, req.Author, req.Session, data, req.Key)
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
	if !s.ownsTask(w, r, id) {
		return
	}
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
	ds, err := s.store.ListDecisions(status, 200, s.agentScope(r))
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
	// 先查这张待拍板挂在哪张卡上，再按身份判断能不能碰；别人的卡直接 403。
	if agent := s.agentScope(r); agent != "" {
		taskID, err := s.store.DecisionTask(r.PathValue("id"))
		if err == ErrNotFound {
			writeErr(w, http.StatusNotFound, "decision not found")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if owner, err := s.store.TaskOwner(taskID); err != nil || owner != agent {
			writeErr(w, http.StatusForbidden, "这张卡不归你管")
			return
		}
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
	// 先确认这条回执归不归你，再动它；不然 403 之前状态已经改了。
	if agent := s.agentScope(r); agent != "" {
		cur, err := s.store.ReplyBySeq(seq)
		if err == ErrNotFound {
			writeErr(w, http.StatusNotFound, "reply not found")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if cur.TaskID != "" {
			if owner, err := s.store.TaskOwner(cur.TaskID); err != nil || owner != agent {
				writeErr(w, http.StatusForbidden, "这条回执不归你管")
				return
			}
		} else if link, err := s.store.EventLink(seq); err == nil && link != "" {
			// 自由对话的回执本身没挂卡，但它对应的消息可能挂着。
			if owner, err := s.store.TaskOwner(link); err != nil || owner != agent {
				writeErr(w, http.StatusForbidden, "这条回执不归你管")
				return
			}
		}
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
	rs, err := s.store.RecentReplies(limit, free, s.agentScope(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rs == nil {
		rs = []Reply{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"replies": rs})
}

// GET /api/chat 返回自由对话（含挂靠的卡片），按新到旧。
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("recent"))
	msgs, err := s.store.ChatMessagesWithLinked(limit, s.agentScope(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if msgs == nil {
		msgs = []Event{}
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
	if strings.TrimSpace(req.Text) == "" && len(req.Images) == 0 {
		writeErr(w, http.StatusBadRequest, "text or image required")
		return
	}
	if err := s.validateImageIDs(req.Images); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	author := strings.TrimSpace(req.From)
	if author == "" {
		author = "user"
	}
	// 身份可以往自己的卡上挂消息；别人的卡直接拒，别让消息落进去。
	if agent := s.agentScope(r); agent != "" {
		if author == "user" || author == mainIdentity {
			author = agent
		}
		if taskID := strings.TrimSpace(req.TaskID); taskID != "" {
			if owner, err := s.store.TaskOwner(taskID); err != nil || owner != agent {
				writeErr(w, http.StatusForbidden, "这张卡不归你管")
				return
			}
		}
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
	agent := s.agentScope(r)
	for {
		events, err := s.store.EventsAfter(since, limit, agent)
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
	// 重连时浏览器给 Last-Event-ID，或前端显式传 since；从断点补发漏掉的事件。
	if since, err := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64); err == nil && since > 0 && since < last {
		last = since
	}
	if since, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64); err == nil && since > 0 && since < last {
		last = since
	}
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
				evs, err := s.store.EventsAfter(last, 100, s.agentScope(r))
				if err != nil {
					return
				}
				for _, ev := range evs {
					payload, _ := json.Marshal(map[string]any{"seq": ev.Seq, "type": ev.Type,
						"task_id": ev.TaskID, "body": ev.Body, "kind": ev.Kind})
					fmt.Fprintf(w, "id: %d\nevent: change\ndata: %s\n\n", ev.Seq, payload)
				}
				if len(evs) == 100 && evs[len(evs)-1].Seq > last {
					last = evs[len(evs)-1].Seq
				} else {
					last = cur
				}
				fl.Flush()
			}
			if r.URL.Query().Get("once") == "1" {
				return
			}
		}
	}
}

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 366 {
		days = 84
	}
	// 日历默认是全局的；挑一个 Agent 时只数它名下卡的事件。
	agent := r.URL.Query().Get("agent")
	q := `
		SELECT date(created_at,'localtime') AS d, COUNT(*)
		FROM events e
		WHERE date(created_at,'localtime') >= date('now','localtime',?)
		AND (e.task_id IS NULL OR e.task_id='' OR e.task_id NOT IN (SELECT id FROM tasks WHERE column_name='archived'))`
	args := []any{fmt.Sprintf("-%d days", days-1)}
	if agent != "" {
		q += ` AND (COALESCE(NULLIF(e.link_task,''),e.task_id,'')='' OR COALESCE(NULLIF(e.link_task,''),e.task_id,'') IN
			(SELECT id FROM tasks WHERE agent=?))`
		args = append(args, agent)
	}
	q += ` GROUP BY d ORDER BY d`
	rows, err := s.store.db.Query(q, args...)
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

// ---- deploy ----

// handleDeploy 触发本机自动更新。
// 这里只写一个触发文件，真正的拉取/构建/重启由服务器上的 systemd 盯着做。
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	if s.cfg.DeployTrigger == "" {
		writeErr(w, http.StatusNotImplemented, "本机未启用自动更新")
		return
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	if err := os.WriteFile(s.cfg.DeployTrigger, []byte(stamp+"\n"), 0o644); err != nil {
		log.Printf("deploy trigger failed: %v", err)
		writeErr(w, http.StatusInternalServerError, "触发更新失败")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "msg": "已开始更新，几秒后刷新"})
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
