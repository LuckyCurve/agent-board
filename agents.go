package main

import (
	"database/sql"
	"errors"
	"sort"
	"strings"
)

// 卡片标签：项目、Agent、关键词都当标签用，过滤时一套机制走完。
// 标签只存关键词本身，「项目:财富」这种前缀形式由前端拼。

func normalizeTag(tag string) string {
	return strings.TrimSpace(tag)
}

// SetTaskTags 整份替换一张卡的标签。
func (s *Store) SetTaskTags(taskID string, tags []string) error {
	if taskID == "" {
		return errors.New("task id required")
	}
	seen := map[string]bool{}
	clean := make([]string, 0, len(tags))
	for _, t := range tags {
		t = normalizeTag(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		clean = append(clean, t)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM task_tags WHERE task_id=?`, taskID); err != nil {
		return err
	}
	for _, t := range clean {
		if _, err := tx.Exec(`INSERT INTO task_tags(task_id,tag) VALUES(?,?)`, taskID, t); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AddTaskTags 增删标签，返回改完之后的完整列表。
func (s *Store) AddTaskTags(taskID string, add, remove []string) ([]string, error) {
	if taskID == "" {
		return nil, errors.New("task id required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, t := range remove {
		t = normalizeTag(t)
		if t == "" {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM task_tags WHERE task_id=? AND tag=?`, taskID, t); err != nil {
			return nil, err
		}
	}
	for _, t := range add {
		t = normalizeTag(t)
		if t == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO task_tags(task_id,tag) VALUES(?,?)`, taskID, t); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.TaskTags(taskID)
}

func (s *Store) TaskTags(taskID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT tag FROM task_tags WHERE task_id=? ORDER BY tag`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AllTags 是全库出现过的标签，供前端做筛选条。
func (s *Store) AllTags() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT tag FROM task_tags ORDER BY tag`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// attachTags 一次把一批卡的标签填上。
func (s *Store) attachTags(tasks []Task) error {
	if len(tasks) == 0 {
		return nil
	}
	byID := map[string][]string{}
	for _, t := range tasks {
		t.Tags = []string{}
	}
	placeholders := make([]string, 0, len(tasks))
	args := make([]any, 0, len(tasks))
	for _, t := range tasks {
		placeholders = append(placeholders, "?")
		args = append(args, t.ID)
	}
	rows, err := s.db.Query(`SELECT task_id,tag FROM task_tags WHERE task_id IN (`+
		strings.Join(placeholders, ",")+`) ORDER BY tag`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, tag string
		if err := rows.Scan(&id, &tag); err != nil {
			return err
		}
		byID[id] = append(byID[id], tag)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range tasks {
		if tags, ok := byID[tasks[i].ID]; ok {
			tasks[i].Tags = tags
		}
	}
	return nil
}

// AssignTask 把卡转交给另一个 Agent，留一条事件说明。
func (s *Store) AssignTask(taskID, agent, key string) (Task, error) {
	agent = strings.TrimSpace(agent)
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
	from := t.Agent
	t.Agent = agent
	t.UpdatedAt = nowISO()
	if _, err := tx.Exec(`UPDATE tasks SET agent=?, updated_at=? WHERE id=?`, nullify(agent), t.UpdatedAt, taskID); err != nil {
		return Task{}, err
	}
	body := "接手"
	if from != "" {
		body = "从「" + from + "」转交给「" + agent + "」"
	} else {
		body = "指派给「" + agent + "」"
	}
	seq, _, err := insertEvent(tx, &Event{Key: key, Type: "task.assigned", TaskID: taskID,
		Agent: agent, Kind: "assigned", Body: body})
	if err != nil {
		return Task{}, err
	}
	t.LastSeq = seq
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	return t, nil
}

// Agent 令牌：每个 Agent 一把，只能碰自己的卡。

func (s *Store) SaveAgentToken(tokenHash, agent string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO agent_tokens(token_hash,agent,created_at) VALUES(?,?,?)`,
		tokenHash, agent, nowISO())
	return err
}

// AgentForToken 返回这把令牌绑定的 Agent；没有绑定就是主令牌。
func (s *Store) AgentForToken(tokenHash string) (string, bool, error) {
	var agent string
	err := s.db.QueryRow(`SELECT agent FROM agent_tokens WHERE token_hash=?`, tokenHash).Scan(&agent)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return agent, true, nil
}

// ListAgents 是所有出现过的 Agent：卡片归属 + 令牌绑定。
func (s *Store) ListAgents() ([]string, error) {
	rows, err := s.db.Query(`
		SELECT agent FROM tasks WHERE COALESCE(agent,'')<>''
		UNION
		SELECT agent FROM agent_tokens WHERE COALESCE(agent,'')<>''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// TaskOwner 查一张卡的归属 Agent。
func (s *Store) TaskOwner(taskID string) (string, error) {
	var agent string
	err := s.db.QueryRow(`SELECT COALESCE(agent,'') FROM tasks WHERE id=?`, taskID).Scan(&agent)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return agent, nil
}

// ErrNotOwner：这张卡不归这个 Agent 管。
var ErrNotOwner = errors.New("这张卡不归你管")
