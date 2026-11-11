package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound = errors.New("not found")
	columns     = map[string]bool{"todo": true, "doing": true, "blocked": true, "done": true}
)

type Event struct {
	Seq       int64  `json:"seq"`
	Key       string `json:"key,omitempty"`
	Type      string `json:"type"`
	TaskID    string `json:"task_id,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Author    string `json:"author,omitempty"`
	Chat      bool   `json:"chat,omitempty"`
	LinkTask  string `json:"link_task,omitempty"`
	Session   string `json:"session,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Body      string `json:"body,omitempty"`
	Data      string `json:"data,omitempty"`
	CreatedAt string `json:"created_at"`
}

type Task struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Project   string `json:"project,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Column    string `json:"column"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	LastSeq   int64  `json:"last_seq"`
	LastEvent *Event `json:"last_event,omitempty"`
	Pending   int    `json:"pending_decisions"`
	Reply     *Reply `json:"reply,omitempty"`
}

// Reply 是用户某次输入的处理状态：received → processing → done / failed / timeout。
// 用户发完立刻看到「已收到」，不会石沉大海。
type Reply struct {
	Seq       int64  `json:"seq"`
	TaskID    string `json:"task_id,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Author    string `json:"author,omitempty"`
	Text      string `json:"text"`
	Status    string `json:"status"`
	Note      string `json:"note,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type Decision struct {
	ID         string `json:"id"`
	TaskID     string `json:"task_id"`
	Question   string `json:"question"`
	Options    string `json:"options"`
	Status     string `json:"status"`
	Answer     string `json:"answer,omitempty"`
	CreatedAt  string `json:"created_at"`
	AnsweredAt string `json:"answered_at,omitempty"`
}

type Store struct{ db *sql.DB }

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // 单写者，彻底避开 SQLITE_BUSY
	for _, p := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`PRAGMA foreign_keys=ON`,
	} {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, err
		}
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS meta (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  idem_key TEXT UNIQUE,
  type TEXT NOT NULL,
  task_id TEXT,
  agent TEXT,
  author TEXT,
  chat INTEGER NOT NULL DEFAULT 0,
  link_task TEXT,
  session TEXT,
  kind TEXT,
  body TEXT,
  data TEXT,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_task ON events(task_id, seq);
CREATE TABLE IF NOT EXISTS tasks (
  id TEXT PRIMARY KEY,
  title TEXT NOT NULL,
  project TEXT,
  agent TEXT,
  column_name TEXT NOT NULL DEFAULT 'todo',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  last_seq INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS decisions (
  id TEXT PRIMARY KEY,
  task_id TEXT,
  question TEXT NOT NULL,
  options TEXT,
  status TEXT NOT NULL DEFAULT 'pending',
  answer TEXT,
  created_at TEXT NOT NULL,
  answered_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_decisions_status ON decisions(status, created_at);
CREATE TABLE IF NOT EXISTS consumers (
  name TEXT PRIMARY KEY,
  acked_seq INTEGER NOT NULL DEFAULT 0,
  lease_seq INTEGER NOT NULL DEFAULT 0,
  lease_until INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS replies (
  seq INTEGER PRIMARY KEY,
  task_id TEXT,
  agent TEXT,
  author TEXT,
  text TEXT NOT NULL,
  status TEXT NOT NULL,
  note TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_replies_task ON replies(task_id, seq);
CREATE INDEX IF NOT EXISTS idx_replies_status ON replies(status, updated_at);
CREATE TABLE IF NOT EXISTS sessions (
  id_hash TEXT PRIMARY KEY,
  created_at TEXT NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);
`)
	if err != nil {
		return err
	}
	// 老库补列：author 用来区分用户和 Agent 的回话。
	for _, c := range []struct{ table, column, ddl string }{
		{"events", "author", "TEXT"},
		{"replies", "author", "TEXT"},
		{"events", "chat", "INTEGER NOT NULL DEFAULT 0"},
		{"events", "link_task", "TEXT"},
	} {
		if err := s.ensureColumn(c.table, c.column, c.ddl); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ensureColumn(table, column, ddl string) error {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	exists := false
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, typ        string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			exists = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil || exists {
		return err
	}
	_, err = s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + ddl)
	return err
}

func nowISO() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func nullify(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Store) metaGet(k string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM meta WHERE k=?`, k).Scan(&v)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	return v, err
}

func (s *Store) metaSet(k, v string) error {
	_, err := s.db.Exec(`INSERT INTO meta(k,v) VALUES(?,?)
		ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

// ---- sessions ----

func (s *Store) CreateSession(token string, ttl time.Duration) error {
	if token == "" {
		return errors.New("empty session token")
	}
	now := time.Now()
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, now.Unix()); err != nil {
		return err
	}
	_, err := s.db.Exec(`INSERT INTO sessions(id_hash, created_at, expires_at) VALUES(?,?,?)
		ON CONFLICT(id_hash) DO UPDATE SET created_at=excluded.created_at, expires_at=excluded.expires_at`,
		sessionHash(token), now.UTC().Format(time.RFC3339Nano), now.Add(ttl).Unix())
	return err
}

func (s *Store) SessionValid(token string) bool {
	if token == "" {
		return false
	}
	var exp int64
	err := s.db.QueryRow(`SELECT expires_at FROM sessions WHERE id_hash=?`, sessionHash(token)).Scan(&exp)
	return err == nil && time.Now().Unix() < exp
}

func (s *Store) DeleteSession(token string) error {
	if token == "" {
		return nil
	}
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id_hash=?`, sessionHash(token))
	return err
}

func (s *Store) DeleteAllSessions() error {
	_, err := s.db.Exec(`DELETE FROM sessions`)
	return err
}

func nextID(tx *sql.Tx, key, prefix string) (string, error) {
	var v int64
	err := tx.QueryRow(`SELECT CAST(v AS INTEGER) FROM meta WHERE k=?`, key).Scan(&v)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	v++
	if _, err := tx.Exec(`INSERT INTO meta(k,v) VALUES(?,?)
		ON CONFLICT(k) DO UPDATE SET v=excluded.v`, key, strconv.FormatInt(v, 10)); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%d", prefix, v), nil
}

func insertEvent(tx *sql.Tx, e *Event) (int64, bool, error) {
	if e.CreatedAt == "" {
		e.CreatedAt = nowISO()
	}
	var (
		res sql.Result
		err error
	)
	if e.Key != "" {
		res, err = tx.Exec(`INSERT INTO events(idem_key,type,task_id,agent,author,chat,link_task,session,kind,body,data,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(idem_key) DO NOTHING`,
			e.Key, e.Type, nullify(e.TaskID), nullify(e.Agent), nullify(e.Author), e.Chat,
			nullify(e.LinkTask), nullify(e.Session), nullify(e.Kind), nullify(e.Body), nullify(e.Data), e.CreatedAt)
	} else {
		res, err = tx.Exec(`INSERT INTO events(type,task_id,agent,author,chat,link_task,session,kind,body,data,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			e.Type, nullify(e.TaskID), nullify(e.Agent), nullify(e.Author), e.Chat,
			nullify(e.LinkTask), nullify(e.Session), nullify(e.Kind), nullify(e.Body), nullify(e.Data), e.CreatedAt)
	}
	if err != nil {
		return 0, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 && e.Key != "" {
		var seq int64
		if err := tx.QueryRow(`SELECT seq FROM events WHERE idem_key=?`, e.Key).Scan(&seq); err != nil {
			return 0, false, err
		}
		return seq, true, nil
	}
	seq, err := res.LastInsertId()
	return seq, false, err
}

func touchTask(tx *sql.Tx, taskID string, seq int64) error {
	if taskID == "" {
		return nil
	}
	_, err := tx.Exec(`UPDATE tasks SET last_seq=?, updated_at=? WHERE id=?`, seq, nowISO(), taskID)
	return err
}

func (s *Store) CreateTask(title, project, agent, column, key string) (Task, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Task{}, errors.New("title required")
	}
	if column == "" {
		column = "todo"
	}
	if !columns[column] {
		return Task{}, fmt.Errorf("invalid column %q (todo/doing/blocked/done)", column)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback()
	id, err := nextID(tx, "task_seq", "T")
	if err != nil {
		return Task{}, err
	}
	ts := nowISO()
	if _, err := tx.Exec(`INSERT INTO tasks(id,title,project,agent,column_name,created_at,updated_at,last_seq)
		VALUES(?,?,?,?,?,?,?,0)`, id, title, nullify(project), nullify(agent), column, ts, ts); err != nil {
		return Task{}, err
	}
	seq, _, err := insertEvent(tx, &Event{Key: key, Type: "task.created", TaskID: id,
		Agent: agent, Kind: "created", Body: title})
	if err != nil {
		return Task{}, err
	}
	if err := touchTask(tx, id, seq); err != nil {
		return Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	return Task{ID: id, Title: title, Project: project, Agent: agent, Column: column,
		CreatedAt: ts, UpdatedAt: ts, LastSeq: seq}, nil
}

func (s *Store) AddEvent(taskID, typ, kind, body, agent, author, session, data, key string) (Event, error) {
	if typ == "" {
		typ = "event"
	}
	if (kind == "user_reply" || kind == "user_reaction") && author == "" {
		author = "user"
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Event{}, err
	}
	defer tx.Rollback()
	if taskID != "" {
		var taskAgent string
		if err := tx.QueryRow(`SELECT COALESCE(agent,'') FROM tasks WHERE id=?`, taskID).Scan(&taskAgent); err != nil {
			if err == sql.ErrNoRows {
				return Event{}, ErrNotFound
			}
			return Event{}, err
		}
		// 用户回复默认落到卡片归属的 Agent，主会话据此派活。
		if agent == "" {
			agent = taskAgent
		}
	}
	ev := &Event{Key: key, Type: typ, TaskID: taskID, Agent: agent, Author: author,
		Session: session, Kind: kind, Body: body, Data: data}
	seq, dup, err := insertEvent(tx, ev)
	if err != nil {
		return Event{}, err
	}
	if !dup {
		if err := touchTask(tx, taskID, seq); err != nil {
			return Event{}, err
		}
		if kind == "user_reply" || kind == "user_reaction" {
			if err := addReplyTx(tx, seq, taskID, agent, author, body); err != nil {
				return Event{}, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return Event{}, err
	}
	ev.Seq = seq
	return *ev, nil
}

// AddChatMessage 记一条自由对话。它可以挂一张卡（LinkTask），但仍留在对话流里，
// 不写进卡片时间线，也不改卡片的「最新进展」。
func (s *Store) AddChatMessage(text, author, linkTask string, images []string, key string) (Event, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Event{}, errors.New("text required")
	}
	if author == "" {
		author = "user"
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Event{}, err
	}
	defer tx.Rollback()
	if linkTask != "" {
		var one int
		if err := tx.QueryRow(`SELECT 1 FROM tasks WHERE id=?`, linkTask).Scan(&one); err != nil {
			if err == sql.ErrNoRows {
				return Event{}, ErrNotFound
			}
			return Event{}, err
		}
	}
	payload := map[string]any{"free": true, "author": author}
	if len(images) > 0 {
		payload["images"] = images
	}
	data, _ := json.Marshal(payload)
	ev := &Event{Key: key, Type: "event", Kind: "user_reply", Author: author,
		Chat: true, LinkTask: linkTask, Body: text, Data: string(data)}
	seq, dup, err := insertEvent(tx, ev)
	if err != nil {
		return Event{}, err
	}
	if !dup {
		if err := addReplyTx(tx, seq, "", "", author, text); err != nil {
			return Event{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Event{}, err
	}
	ev.Seq = seq
	return *ev, nil
}

// ChatMessages 按新到旧返回自由对话，调用方自行反转成 IM 顺序。
func (s *Store) ChatMessages(limit int) ([]Event, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT seq,COALESCE(idem_key,''),type,COALESCE(task_id,''),COALESCE(agent,''),COALESCE(author,''),
		COALESCE(chat,0),COALESCE(link_task,''),COALESCE(session,''),COALESCE(kind,''),COALESCE(body,''),COALESCE(data,''),created_at
		FROM events WHERE chat=1 ORDER BY seq DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

func (s *Store) MoveTask(taskID, column, key string) (Task, error) {
	if !columns[column] {
		return Task{}, fmt.Errorf("invalid column %q (todo/doing/blocked/done)", column)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback()
	var t Task
	err = tx.QueryRow(`SELECT id,title,COALESCE(project,''),COALESCE(agent,''),column_name,created_at,updated_at,last_seq
		FROM tasks WHERE id=?`, taskID).Scan(&t.ID, &t.Title, &t.Project, &t.Agent, &t.Column, &t.CreatedAt, &t.UpdatedAt, &t.LastSeq)
	if err == sql.ErrNoRows {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, err
	}
	t.Column = column
	t.UpdatedAt = nowISO()
	if _, err := tx.Exec(`UPDATE tasks SET column_name=?, updated_at=? WHERE id=?`, column, t.UpdatedAt, taskID); err != nil {
		return Task{}, err
	}
	seq, _, err := insertEvent(tx, &Event{Key: key, Type: "task.moved", TaskID: taskID,
		Kind: "moved", Body: column})
	if err != nil {
		return Task{}, err
	}
	t.LastSeq = seq
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	return t, nil
}

func (s *Store) ListTasks() ([]Task, error) {
	s.SweepReplies()
	rows, err := s.db.Query(`
		SELECT t.id,t.title,COALESCE(t.project,''),COALESCE(t.agent,''),t.column_name,
		       t.created_at,t.updated_at,t.last_seq,
		       (SELECT COUNT(*) FROM decisions d WHERE d.task_id=t.id AND d.status='pending')
		FROM tasks t
		ORDER BY CASE t.column_name WHEN 'todo' THEN 0 WHEN 'doing' THEN 1 WHEN 'blocked' THEN 2 ELSE 3 END,
		         t.updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.Title, &t.Project, &t.Agent, &t.Column,
			&t.CreatedAt, &t.UpdatedAt, &t.LastSeq, &t.Pending); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if ev, err := s.lastEvent(out[i].ID); err == nil {
			out[i].LastEvent = ev
		}
		out[i].Reply = s.openReplyForTask(out[i].ID)
	}
	return out, nil
}

func (s *Store) lastEvent(taskID string) (*Event, error) {
	var e Event
	err := s.db.QueryRow(`SELECT seq,COALESCE(idem_key,''),type,COALESCE(task_id,''),COALESCE(agent,''),COALESCE(author,''),
		COALESCE(chat,0),COALESCE(link_task,''),COALESCE(session,''),COALESCE(kind,''),COALESCE(body,''),COALESCE(data,''),created_at
		FROM events WHERE task_id=? ORDER BY seq DESC LIMIT 1`, taskID).
		Scan(&e.Seq, &e.Key, &e.Type, &e.TaskID, &e.Agent, &e.Author, &e.Chat, &e.LinkTask,
			&e.Session, &e.Kind, &e.Body, &e.Data, &e.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *Store) GetTask(taskID string) (Task, []Event, error) {
	s.SweepReplies()
	var t Task
	err := s.db.QueryRow(`SELECT t.id,t.title,COALESCE(t.project,''),COALESCE(t.agent,''),t.column_name,
		t.created_at,t.updated_at,t.last_seq,
		(SELECT COUNT(*) FROM decisions d WHERE d.task_id=t.id AND d.status='pending')
		FROM tasks t WHERE t.id=?`, taskID).
		Scan(&t.ID, &t.Title, &t.Project, &t.Agent, &t.Column, &t.CreatedAt, &t.UpdatedAt, &t.LastSeq, &t.Pending)
	if err == sql.ErrNoRows {
		return t, nil, ErrNotFound
	}
	if err != nil {
		return t, nil, err
	}
	evs, err := s.EventsByTask(taskID, 1000)
	t.LastEvent, _ = s.lastEvent(taskID)
	t.Reply = s.openReplyForTask(taskID)
	return t, evs, err
}

func (s *Store) EventsByTask(taskID string, limit int) ([]Event, error) {
	rows, err := s.db.Query(`SELECT seq,COALESCE(idem_key,''),type,COALESCE(task_id,''),COALESCE(agent,''),COALESCE(author,''),
		COALESCE(chat,0),COALESCE(link_task,''),COALESCE(session,''),COALESCE(kind,''),COALESCE(body,''),COALESCE(data,''),created_at
		FROM events WHERE task_id=? ORDER BY seq ASC LIMIT ?`, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

func (s *Store) EventsAfter(seq int64, limit int) ([]Event, error) {
	rows, err := s.db.Query(`SELECT seq,COALESCE(idem_key,''),type,COALESCE(task_id,''),COALESCE(agent,''),COALESCE(author,''),
		COALESCE(chat,0),COALESCE(link_task,''),COALESCE(session,''),COALESCE(kind,''),COALESCE(body,''),COALESCE(data,''),created_at
		FROM events WHERE seq>? ORDER BY seq ASC LIMIT ?`, seq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

func scanEvents(rows *sql.Rows) ([]Event, error) {
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Seq, &e.Key, &e.Type, &e.TaskID, &e.Agent, &e.Author,
			&e.Chat, &e.LinkTask, &e.Session, &e.Kind, &e.Body, &e.Data, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) LatestSeq() (int64, error) {
	var seq int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM events`).Scan(&seq)
	return seq, err
}

func (s *Store) CreateDecision(taskID, question, optionsJSON, key string) (Decision, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return Decision{}, errors.New("question required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Decision{}, err
	}
	defer tx.Rollback()
	var taskAgent string
	if err := tx.QueryRow(`SELECT COALESCE(agent,'') FROM tasks WHERE id=?`, taskID).Scan(&taskAgent); err != nil {
		if err == sql.ErrNoRows {
			return Decision{}, ErrNotFound
		}
		return Decision{}, err
	}
	id, err := nextID(tx, "decision_seq", "D")
	if err != nil {
		return Decision{}, err
	}
	ts := nowISO()
	if _, err := tx.Exec(`INSERT INTO decisions(id,task_id,question,options,status,created_at)
		VALUES(?,?,?,?,'pending',?)`, id, taskID, question, optionsJSON, ts); err != nil {
		return Decision{}, err
	}
	data, _ := json.Marshal(map[string]string{"decision_id": id, "options": optionsJSON})
	seq, _, err := insertEvent(tx, &Event{Key: key, Type: "decision.created", TaskID: taskID,
		Agent: taskAgent, Kind: "decision", Body: question, Data: string(data)})
	if err != nil {
		return Decision{}, err
	}
	if err := touchTask(tx, taskID, seq); err != nil {
		return Decision{}, err
	}
	if err := tx.Commit(); err != nil {
		return Decision{}, err
	}
	return Decision{ID: id, TaskID: taskID, Question: question, Options: optionsJSON,
		Status: "pending", CreatedAt: ts}, nil
}

func (s *Store) AnswerDecision(id, answer, key string) (Decision, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Decision{}, err
	}
	defer tx.Rollback()
	var d Decision
	err = tx.QueryRow(`SELECT id,task_id,question,COALESCE(options,''),status,COALESCE(answer,''),
		created_at,COALESCE(answered_at,'') FROM decisions WHERE id=?`, id).
		Scan(&d.ID, &d.TaskID, &d.Question, &d.Options, &d.Status, &d.Answer, &d.CreatedAt, &d.AnsweredAt)
	if err == sql.ErrNoRows {
		return Decision{}, ErrNotFound
	}
	if err != nil {
		return Decision{}, err
	}
	if d.Status != "pending" {
		return Decision{}, fmt.Errorf("decision %s already %s", id, d.Status)
	}
	if strings.TrimSpace(answer) == "" {
		return Decision{}, errors.New("answer required")
	}
	d.Status = "answered"
	d.Answer = answer
	d.AnsweredAt = nowISO()
	if _, err := tx.Exec(`UPDATE decisions SET status='answered', answer=?, answered_at=? WHERE id=?`,
		answer, d.AnsweredAt, id); err != nil {
		return Decision{}, err
	}
	data, _ := json.Marshal(map[string]string{"decision_id": id, "answer": answer})
	var taskAgent string
	_ = tx.QueryRow(`SELECT COALESCE(agent,'') FROM tasks WHERE id=?`, d.TaskID).Scan(&taskAgent)
	seq, _, err := insertEvent(tx, &Event{Key: key, Type: "decision.answered", TaskID: d.TaskID,
		Agent: taskAgent, Kind: "decision", Body: answer, Data: string(data)})
	if err != nil {
		return Decision{}, err
	}
	if err := addReplyTx(tx, seq, d.TaskID, taskAgent, "user", answer); err != nil {
		return Decision{}, err
	}
	if err := touchTask(tx, d.TaskID, seq); err != nil {
		return Decision{}, err
	}
	if err := tx.Commit(); err != nil {
		return Decision{}, err
	}
	return d, nil
}

func (s *Store) ListDecisions(status string, limit int) ([]Decision, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT id,task_id,question,COALESCE(options,''),status,COALESCE(answer,''),
		created_at,COALESCE(answered_at,'') FROM decisions
		WHERE (?='' OR status=?)
		ORDER BY CASE status WHEN 'pending' THEN 0 ELSE 1 END, created_at DESC LIMIT ?`,
		status, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Decision
	for rows.Next() {
		var d Decision
		if err := rows.Scan(&d.ID, &d.TaskID, &d.Question, &d.Options, &d.Status, &d.Answer,
			&d.CreatedAt, &d.AnsweredAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) DecisionsByTask(taskID string) ([]Decision, error) {
	rows, err := s.db.Query(`SELECT id,task_id,question,COALESCE(options,''),status,COALESCE(answer,''),
		created_at,COALESCE(answered_at,'') FROM decisions
		WHERE task_id=? ORDER BY CASE status WHEN 'pending' THEN 0 ELSE 1 END, created_at DESC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Decision
	for rows.Next() {
		var d Decision
		if err := rows.Scan(&d.ID, &d.TaskID, &d.Question, &d.Options, &d.Status, &d.Answer,
			&d.CreatedAt, &d.AnsweredAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---- user replies / reactions ----

var replyStatuses = map[string]bool{
	"received": true, "processing": true, "done": true, "failed": true, "timeout": true,
}

const replyCols = `seq,COALESCE(task_id,''),COALESCE(agent,''),COALESCE(author,''),text,status,COALESCE(note,''),created_at,updated_at`

func addReplyTx(tx *sql.Tx, seq int64, taskID, agent, author, text string) error {
	ts := nowISO()
	status := "received"
	if author != "" && author != "user" {
		// Agent 自己发的消息已经送达，不需要用户侧再处理。
		status = "done"
	}
	_, err := tx.Exec(`INSERT OR IGNORE INTO replies(seq,task_id,agent,author,text,status,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?)`, seq, nullify(taskID), nullify(agent), nullify(author),
		text, status, ts, ts)
	return err
}

// SweepReplies 把久未接手的输入标成 timeout，界面就不会一直停在「已收到」。
func (s *Store) SweepReplies() {
	_, _ = s.db.Exec(`UPDATE replies
		SET status='timeout',
		    note=CASE WHEN COALESCE(note,'')='' THEN '超时未处理' ELSE note END,
		    updated_at=?
		WHERE (status='received'  AND julianday(created_at) < julianday('now','-10 minutes'))
		   OR (status='processing' AND julianday(updated_at) < julianday('now','-120 minutes'))`, nowISO())
}

func scanReplyRow(sc interface{ Scan(...any) error }) (Reply, error) {
	var r Reply
	err := sc.Scan(&r.Seq, &r.TaskID, &r.Agent, &r.Author, &r.Text, &r.Status, &r.Note, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

func (s *Store) ReplyBySeq(seq int64) (Reply, error) {
	r, err := scanReplyRow(s.db.QueryRow(`SELECT `+replyCols+` FROM replies WHERE seq=?`, seq))
	if err == sql.ErrNoRows {
		return r, ErrNotFound
	}
	return r, err
}

func (s *Store) UpdateReply(seq int64, status, agent, note string) (Reply, error) {
	if !replyStatuses[status] {
		return Reply{}, fmt.Errorf("invalid status %q", status)
	}
	res, err := s.db.Exec(`UPDATE replies
		SET status=?, agent=COALESCE(NULLIF(?,''),agent),
		    note=COALESCE(NULLIF(?,''),note), updated_at=?
		WHERE seq=?`, status, agent, note, nowISO(), seq)
	if err != nil {
		return Reply{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Reply{}, ErrNotFound
	}
	return s.ReplyBySeq(seq)
}

func (s *Store) RepliesByTask(taskID string) ([]Reply, error) {
	rows, err := s.db.Query(`SELECT `+replyCols+` FROM replies WHERE task_id=? ORDER BY seq ASC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Reply{}
	for rows.Next() {
		r, err := scanReplyRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) openReplyForTask(taskID string) *Reply {
	r, err := scanReplyRow(s.db.QueryRow(`SELECT `+replyCols+` FROM replies
		WHERE task_id=? AND status IN ('received','processing','failed','timeout')
		ORDER BY seq DESC LIMIT 1`, taskID))
	if err != nil {
		return nil
	}
	return &r
}

func (s *Store) RecentReplies(limit int, freeOnly bool) ([]Reply, error) {
	if limit <= 0 || limit > 200 {
		limit = 30
	}
	q := `SELECT ` + replyCols + ` FROM replies`
	if freeOnly {
		q += ` WHERE COALESCE(task_id,'')=''`
	}
	q += ` ORDER BY seq DESC LIMIT ?`
	rows, err := s.db.Query(q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Reply{}
	for rows.Next() {
		r, err := scanReplyRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- consumer cursor / lease ----

func (s *Store) consumer(name string) (acked, leaseSeq, leaseUntil int64, err error) {
	err = s.db.QueryRow(`SELECT acked_seq,lease_seq,lease_until FROM consumers WHERE name=?`, name).
		Scan(&acked, &leaseSeq, &leaseUntil)
	if err == sql.ErrNoRows {
		_, e := s.db.Exec(`INSERT OR IGNORE INTO consumers(name) VALUES(?)`, name)
		return 0, 0, 0, e
	}
	return
}

func (s *Store) Claim(name string, seq int64) (claimed, alreadyAcked bool, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback()
	var acked, leaseSeq, leaseUntil int64
	err = tx.QueryRow(`SELECT acked_seq,lease_seq,lease_until FROM consumers WHERE name=?`, name).
		Scan(&acked, &leaseSeq, &leaseUntil)
	if err == sql.ErrNoRows {
		if _, err := tx.Exec(`INSERT INTO consumers(name) VALUES(?)`, name); err != nil {
			return false, false, err
		}
	} else if err != nil {
		return false, false, err
	}
	now := time.Now().Unix()
	if seq <= acked {
		if err := tx.Commit(); err != nil {
			return false, false, err
		}
		return false, true, nil
	}
	if leaseSeq == seq && leaseUntil > now {
		if err := tx.Commit(); err != nil {
			return false, false, err
		}
		return false, false, nil
	}
	if _, err := tx.Exec(`UPDATE consumers SET lease_seq=?, lease_until=? WHERE name=?`,
		seq, now+120, name); err != nil {
		return false, false, err
	}
	return true, false, tx.Commit()
}

func (s *Store) Ack(name string, seq int64) error {
	_, err := s.db.Exec(`UPDATE consumers SET acked_seq=MAX(acked_seq,?), lease_seq=0, lease_until=0
		WHERE name=?`, seq, name)
	return err
}

// ---- board lifecycle ----
//
// 看板不是人的资产：会话退出、状态无法复原时，主 Agent 可以直接重开一局。
// 所有入口都需要显式 confirm，避免手滑。

func (s *Store) BoardCounts() (map[string]int, error) {
	var tasks, events, decisions, pendingDecisions, openReplies int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&tasks); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&events); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM decisions`).Scan(&decisions); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM decisions WHERE status='pending'`).Scan(&pendingDecisions); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM replies WHERE status IN ('received','processing')`).Scan(&openReplies); err != nil {
		return nil, err
	}
	return map[string]int{
		"tasks": tasks, "events": events, "decisions": decisions,
		"pending_decisions": pendingDecisions, "open_replies": openReplies,
	}, nil
}

// ResetBoard 清空任务、事件、待拍板、回执和消费游标，编号从 T-1 / D-1 重新开始。
func (s *Store) ResetBoard() (map[string]int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	cleared := map[string]int{}
	for name, table := range map[string]string{
		"tasks": "tasks", "events": "events", "decisions": "decisions",
		"replies": "replies", "consumers": "consumers",
	} {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			return nil, err
		}
		cleared[name] = n
		if _, err := tx.Exec(`DELETE FROM ` + table); err != nil {
			return nil, err
		}
	}
	// 事件表是 AUTOINCREMENT，删除 sqlite_sequence 才能让 seq 回到 1。
	if _, err := tx.Exec(`DELETE FROM sqlite_sequence WHERE name='events'`); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM meta WHERE k IN ('task_seq','decision_seq')`); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return cleared, nil
}

// ArchiveBoard 保留事件历史，把未完成的卡收进已完成，待拍板与未结回执一起收口。
func (s *Store) ArchiveBoard() (map[string]int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	ts := nowISO()

	rows, err := tx.Query(`SELECT id FROM tasks WHERE column_name<>'done'`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE tasks SET column_name='done', updated_at=? WHERE id=?`, ts, id); err != nil {
			return nil, err
		}
		seq, _, err := insertEvent(tx, &Event{Type: "task.archived", TaskID: id,
			Kind: "moved", Body: "已归档"})
		if err != nil {
			return nil, err
		}
		if err := touchTask(tx, id, seq); err != nil {
			return nil, err
		}
	}

	resD, err := tx.Exec(`UPDATE decisions SET status='archived', answered_at=?
		WHERE status='pending'`, ts)
	if err != nil {
		return nil, err
	}
	nD, _ := resD.RowsAffected()

	resR, err := tx.Exec(`UPDATE replies SET status='done', note='已归档', updated_at=?
		WHERE status IN ('received','processing','failed','timeout')`, ts)
	if err != nil {
		return nil, err
	}
	nR, _ := resR.RowsAffected()

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]int{"tasks": len(ids), "decisions": int(nD), "replies": int(nR)}, nil
}
