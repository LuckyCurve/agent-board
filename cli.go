package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// client 每次请求都带 Agent token；token 默认从 <data>/token 读，BOARD_TOKEN 可覆盖。
type client struct {
	base  string
	token string
}

func newClient() (*client, error) {
	base := strings.TrimRight(envOr("BOARD_URL", "http://127.0.0.1:8765"), "/")
	token := strings.TrimSpace(os.Getenv("BOARD_TOKEN"))
	tokenPath := ""
	if p, err := resolvePaths("", ""); err == nil {
		tokenPath = p.token
		if token == "" {
			if b, err := os.ReadFile(p.token); err == nil {
				token = strings.TrimSpace(string(b))
			}
		}
	}
	if token == "" {
		return nil, fmt.Errorf("no agent token: 启动一次 `agent-board serve` 生成 %s，或设置 BOARD_TOKEN", tokenPath)
	}
	return &client{base: base, token: token}, nil
}

func (c *client) rawOnce(method, path string, body any, timeout time.Duration) (int, []byte, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, r)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	cl := &http.Client{Timeout: timeout}
	resp, err := cl.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

func (c *client) raw(method, path string, body any, timeout time.Duration) (int, []byte, error) {
	return c.rawOnce(method, path, body, timeout)
}

func apiError(b []byte, code int) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return e.Error
	}
	return fmt.Sprintf("http %d: %s", code, strings.TrimSpace(string(b)))
}

func (c *client) do(method, path string, body, out any) error {
	code, b, err := c.raw(method, path, body, 15*time.Second)
	if err != nil {
		return err
	}
	if code == http.StatusUnauthorized {
		return errors.New("token 无效：检查 BOARD_TOKEN 或 <data>/token")
	}
	if code >= 400 {
		return errors.New(apiError(b, code))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// stringList 让 --image 可以写多次。
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// uploadImage 原样传图片字节，类型以服务端魔数为准。
func (c *client) uploadImage(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest("POST", c.base+"/api/images", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", http.DetectContentType(b))
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", errors.New(apiError(out, resp.StatusCode))
	}
	var got struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		return "", err
	}
	return got.ID, nil
}

func uploadImages(c *client, paths []string) ([]string, error) {
	ids := make([]string, 0, len(paths))
	for _, p := range paths {
		id, err := c.uploadImage(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func mustClient() *client {
	c, err := newClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return c
}

func cmdTask(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: agent-board task new|list|show ...")
		os.Exit(2)
	}
	switch args[0] {
	case "new":
		fs := flag.NewFlagSet("task new", flag.ExitOnError)
		title := fs.String("title", "", "task title")
		project := fs.String("project", "", "project name")
		agent := fs.String("agent", "", "owning agent")
		column := fs.String("column", "todo", "todo|doing|blocked|done")
		key := fs.String("key", "", "idempotency key")
		fs.Parse(args[1:])
		if *title == "" {
			fmt.Fprintln(os.Stderr, "--title required")
			os.Exit(2)
		}
		var resp struct {
			Task Task `json:"task"`
		}
		err := mustClient().do("POST", "/api/tasks", map[string]string{
			"title": *title, "project": *project, "agent": *agent,
			"column": *column, "key": *key,
		}, &resp)
		if err != nil {
			fail(err)
		}
		fmt.Println(resp.Task.ID)

	case "list":
		fs := flag.NewFlagSet("task list", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "print raw json")
		fs.Parse(args[1:])
		code, b, err := mustClient().raw("GET", "/api/tasks", nil, 15*time.Second)
		if err != nil {
			fail(err)
		}
		if code >= 400 {
			fail(fmt.Errorf("http %d: %s", code, strings.TrimSpace(string(b))))
		}
		if *asJSON {
			fmt.Println(string(b))
			return
		}
		var resp struct {
			Tasks []Task `json:"tasks"`
		}
		if err := json.Unmarshal(b, &resp); err != nil {
			fail(err)
		}
		if len(resp.Tasks) == 0 {
			fmt.Println("(no tasks)")
			return
		}
		for _, t := range resp.Tasks {
			extra := ""
			if t.Project != "" {
				extra += " · " + t.Project
			}
			if t.Agent != "" {
				extra += " · " + t.Agent
			}
			if t.Pending > 0 {
				extra += fmt.Sprintf(" · 待拍板 %d", t.Pending)
			}
			last := ""
			if t.LastEvent != nil && t.LastEvent.Body != "" {
				last = "  " + oneLine(t.LastEvent.Body, 60)
			}
			fmt.Printf("%-5s %-8s %s%s%s\n", t.ID, t.Column, t.Title, extra, last)
		}

	case "show":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: agent-board task show T-1")
			os.Exit(2)
		}
		var resp struct {
			Task   Task    `json:"task"`
			Events []Event `json:"events"`
		}
		if err := mustClient().do("GET", "/api/tasks/"+url.PathEscape(args[1]), nil, &resp); err != nil {
			fail(err)
		}
		t := resp.Task
		fmt.Printf("%s  [%s]  %s\n", t.ID, t.Column, t.Title)
		if t.Project != "" || t.Agent != "" {
			fmt.Printf("project=%s  agent=%s\n", t.Project, t.Agent)
		}
		fmt.Println()
		for _, e := range resp.Events {
			ts := shortTime(e.CreatedAt)
			label := e.Kind
			if label == "" {
				label = e.Type
			}
			fmt.Printf("%s  %-10s %s\n", ts, label, oneLine(e.Body, 120))
		}

	default:
		fmt.Fprintf(os.Stderr, "unknown task command %q\n", args[0])
		os.Exit(2)
	}
}

func cmdMove(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: agent-board move T-1 doing|blocked|done")
		os.Exit(2)
	}
	var resp map[string]any
	if err := mustClient().do("PATCH", "/api/tasks/"+url.PathEscape(args[0]),
		map[string]string{"column": args[1]}, &resp); err != nil {
		fail(err)
	}
	fmt.Printf("ok %s -> %s\n", args[0], args[1])
}

func cmdEvent(args []string, defaultKind string) {
	fs := flag.NewFlagSet("event", flag.ExitOnError)
	task := fs.String("task", "", "task id")
	kind := fs.String("kind", defaultKind, "progress|note|tool|artifact|conclusion|blocked|dispatch")
	text := fs.String("text", "", "event text")
	agent := fs.String("agent", "", "agent name")
	session := fs.String("session", "", "session id")
	data := fs.String("data", "", "extra json")
	key := fs.String("key", "", "idempotency key")
	fs.Parse(args)
	if *task == "" || (*text == "" && *data == "") {
		fmt.Fprintln(os.Stderr, "usage: agent-board event --task T-1 --kind progress --text \"...\"")
		os.Exit(2)
	}
	var resp struct {
		Event Event `json:"event"`
	}
	err := mustClient().do("POST", "/api/tasks/"+url.PathEscape(*task)+"/events", map[string]string{
		"kind": *kind, "text": *text, "agent": *agent, "session": *session,
		"data": *data, "key": *key,
	}, &resp)
	if err != nil {
		fail(err)
	}
	fmt.Printf("ok seq=%d\n", resp.Event.Seq)
}

func cmdQuickEvent(args []string, kind string) {
	if len(args) < 2 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintf(os.Stderr, "usage: agent-board %s T-1 \"一行内容\" [--agent X]\n", kind)
		os.Exit(2)
	}
	task, text := args[0], args[1]
	fs := flag.NewFlagSet(kind, flag.ExitOnError)
	agent := fs.String("agent", "", "agent name")
	session := fs.String("session", "", "session id")
	key := fs.String("key", "", "idempotency key")
	var images stringList
	fs.Var(&images, "image", "attach an image file (repeatable)")
	fs.Parse(args[2:])
	c := mustClient()
	ids, uploadErr := uploadImages(c, images)
	if uploadErr != nil {
		fail(uploadErr)
	}
	var resp struct {
		Event Event `json:"event"`
	}
	err := c.do("POST", "/api/tasks/"+url.PathEscape(task)+"/events", map[string]any{
		"kind": kind, "text": text, "agent": *agent, "session": *session, "key": *key, "images": ids,
	}, &resp)
	if err != nil {
		fail(err)
	}
	fmt.Printf("ok seq=%d\n", resp.Event.Seq)
}

func cmdDone(args []string) {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "usage: agent-board done T-1 --text \"结论\"")
		os.Exit(2)
	}
	task := args[0]
	fs := flag.NewFlagSet("done", flag.ExitOnError)
	text := fs.String("text", "", "conclusion")
	agent := fs.String("agent", "", "agent name")
	session := fs.String("session", "", "session id")
	key := fs.String("key", "", "idempotency key")
	fs.Parse(args[1:])
	if *text == "" {
		fmt.Fprintln(os.Stderr, "--text required")
		os.Exit(2)
	}
	c := mustClient()
	// 先移列再写结论：卡片上的「最新进展」才会是结论本身。
	if err := c.do("PATCH", "/api/tasks/"+url.PathEscape(task),
		map[string]string{"column": "done"}, nil); err != nil {
		fail(err)
	}
	var resp struct {
		Event Event `json:"event"`
	}
	if err := c.do("POST", "/api/tasks/"+url.PathEscape(task)+"/events", map[string]string{
		"kind": "conclusion", "text": *text, "agent": *agent, "session": *session, "key": *key,
	}, &resp); err != nil {
		fail(err)
	}
	fmt.Printf("ok seq=%d -> done\n", resp.Event.Seq)
}

func cmdAsk(args []string) {
	fs := flag.NewFlagSet("ask", flag.ExitOnError)
	task := fs.String("task", "", "task id")
	question := fs.String("question", "", "decision question")
	options := fs.String("options", "同意,不同意", "comma separated options")
	key := fs.String("key", "", "idempotency key")
	fs.Parse(args)
	if *task == "" || *question == "" {
		fmt.Fprintln(os.Stderr, "usage: agent-board ask --task T-1 --question \"...\" --options \"同意,重做\"")
		os.Exit(2)
	}
	raw := strings.ReplaceAll(*options, "，", ",")
	var opts []string
	for _, o := range strings.Split(raw, ",") {
		if o = strings.TrimSpace(o); o != "" {
			opts = append(opts, o)
		}
	}
	var resp struct {
		Decision Decision `json:"decision"`
	}
	err := mustClient().do("POST", "/api/decisions", map[string]any{
		"task_id": *task, "question": *question, "options": opts, "key": *key,
	}, &resp)
	if err != nil {
		fail(err)
	}
	fmt.Println(resp.Decision.ID)
}

func cmdDecisions(args []string) {
	fs := flag.NewFlagSet("decisions", flag.ExitOnError)
	status := fs.String("status", "pending", "pending|answered|all")
	fs.Parse(args)
	q := ""
	if *status != "all" && *status != "" {
		q = "?status=" + url.QueryEscape(*status)
	}
	var resp struct {
		Decisions []Decision `json:"decisions"`
	}
	if err := mustClient().do("GET", "/api/decisions"+q, nil, &resp); err != nil {
		fail(err)
	}
	if len(resp.Decisions) == 0 {
		fmt.Println("(none)")
		return
	}
	for _, d := range resp.Decisions {
		fmt.Printf("%-4s %-5s %-6s %s  [%s]\n", d.ID, d.Status, d.TaskID, d.Question, d.Options)
	}
}

func cmdWatch(args []string) {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	consumer := fs.String("consumer", "main", "consumer name")
	sinceFlag := fs.Int64("since", -1, "start seq (default: server acked_seq)")
	fs.Parse(args)
	c := mustClient()

	since := *sinceFlag
	if since < 0 {
		var cur struct {
			AckedSeq int64 `json:"acked_seq"`
		}
		if err := c.do("GET", "/api/consumer?name="+url.QueryEscape(*consumer), nil, &cur); err != nil {
			fail(err)
		}
		since = cur.AckedSeq
	}
	fmt.Fprintf(os.Stderr, "watching from seq=%d as %s\n", since, *consumer)

	for {
		var resp struct {
			Events []Event `json:"events"`
			Next   int64   `json:"next"`
		}
		path := fmt.Sprintf("/api/events?since=%d&wait=50&consumer=%s", since, url.QueryEscape(*consumer))
		code, b, err := c.raw("GET", path, nil, 70*time.Second)
		if err != nil {
			fmt.Fprintf(os.Stderr, "watch: %v\n", err)
			time.Sleep(3 * time.Second)
			continue
		}
		if code == http.StatusUnauthorized {
			fail(errors.New("token 无效：检查 BOARD_TOKEN 或 <data>/token"))
		}
		if code >= 400 {
			fmt.Fprintf(os.Stderr, "watch: http %d %s\n", code, strings.TrimSpace(string(b)))
			time.Sleep(3 * time.Second)
			continue
		}
		if err := json.Unmarshal(b, &resp); err != nil {
			fmt.Fprintf(os.Stderr, "watch: %v\n", err)
			time.Sleep(3 * time.Second)
			continue
		}
		retry := false
		for _, ev := range resp.Events {
			ccode, cb, cerr := c.raw("POST", "/api/claim", map[string]any{
				"consumer": *consumer, "seq": ev.Seq}, 10*time.Second)
			if cerr != nil {
				fmt.Fprintf(os.Stderr, "watch: claim %d: %v\n", ev.Seq, cerr)
				retry = true
				break
			}
			if ccode == http.StatusConflict {
				_ = cb
				since = ev.Seq
				continue
			}
			// 用户发来的输入才亮「执行中」；Agent 自己的回话不处理。
			if eventAuthor(ev) == "user" &&
				(ev.Kind == "user_reply" || ev.Kind == "user_reaction" || ev.Type == "decision.answered") {
				_, _, _ = c.raw("PATCH", fmt.Sprintf("/api/replies/%d", ev.Seq),
					map[string]string{"status": "processing", "agent": ev.Agent}, 10*time.Second)
			}
			printEventLine(ev)
			since = ev.Seq
		}
		if retry {
			time.Sleep(3 * time.Second)
			continue
		}
		if resp.Next > since && len(resp.Events) == 0 {
			since = resp.Next
		}
		if len(resp.Events) == 0 {
			continue
		}
	}
}

func eventAuthor(ev Event) string {
	if ev.Author == "" {
		return "user"
	}
	return ev.Author
}

func printEventLine(ev Event) {
	text := oneLine(ev.Body, 800)
	author := eventAuthor(ev)
	switch {
	case ev.Type == "decision.answered":
		fmt.Printf("BOARD_DECISION_ANSWERED | seq=%d | task=%s | agent=%s | author=%s | text=%s | data=%s\n",
			ev.Seq, ev.TaskID, ev.Agent, author, text, ev.Data)
	case ev.Kind == "user_reply" && ev.TaskID == "" && author == "user":
		fmt.Printf("BOARD_USER_MESSAGE | seq=%d | author=user | text=%s | data=%s\n", ev.Seq, text, ev.Data)
	case ev.Kind == "user_reply" && ev.TaskID == "":
		fmt.Printf("BOARD_AGENT_MESSAGE | seq=%d | author=%s | text=%s | data=%s\n", ev.Seq, author, text, ev.Data)
	case ev.Kind == "user_reply" || ev.Kind == "user_reaction":
		fmt.Printf("BOARD_USER_REPLY | seq=%d | task=%s | agent=%s | author=%s | text=%s | data=%s\n",
			ev.Seq, ev.TaskID, ev.Agent, author, text, ev.Data)
	case ev.Type == "decision.created":
		fmt.Printf("BOARD_DECISION | seq=%d | task=%s | agent=%s | text=%s | data=%s\n",
			ev.Seq, ev.TaskID, ev.Agent, text, ev.Data)
	default:
		fmt.Printf("BOARD_EVENT | seq=%d | type=%s | kind=%s | task=%s | agent=%s | text=%s\n",
			ev.Seq, ev.Type, ev.Kind, ev.TaskID, ev.Agent, text)
	}
}

func cmdDispatch(args []string) {
	fs := flag.NewFlagSet("dispatch", flag.ExitOnError)
	seq := fs.Int64("seq", -1, "reply event seq")
	to := fs.String("to", "", "agent to dispatch to")
	note := fs.String("note", "", "optional dispatch note")
	fs.Parse(args)
	if *seq < 0 || *to == "" {
		fmt.Fprintln(os.Stderr, "usage: agent-board dispatch --seq N --to \"Agent\" [--note \"...\"]")
		os.Exit(2)
	}
	c := mustClient()
	var pr struct {
		Reply Reply `json:"reply"`
	}
	if err := c.do("PATCH", fmt.Sprintf("/api/replies/%d", *seq),
		map[string]string{"status": "processing", "agent": *to}, &pr); err != nil {
		fail(err)
	}
	text := "已派给 " + *to
	if *note != "" {
		text += "：" + *note
	}
	if pr.Reply.TaskID != "" {
		data, _ := json.Marshal(map[string]int64{"reply_to_seq": *seq})
		if err := c.do("POST", "/api/tasks/"+url.PathEscape(pr.Reply.TaskID)+"/events", map[string]string{
			"kind": "dispatch", "text": text, "agent": *to, "data": string(data),
		}, nil); err != nil {
			fail(err)
		}
	}
	fmt.Printf("ok reply=%d -> %s\n", *seq, *to)
}

func cmdBoardLifecycle(args []string, mode string) {
	fs := flag.NewFlagSet(mode, flag.ExitOnError)
	yes := fs.Bool("yes", false, "confirm the destructive action")
	fs.Parse(args)
	if !*yes {
		if mode == "reset" {
			fmt.Fprintln(os.Stderr, "reset 会清空所有任务、事件、待拍板和回执，编号重来。确认请加 --yes")
		} else {
			fmt.Fprintln(os.Stderr, "archive 会把所有卡收进已完成、未决项收口。确认请加 --yes")
		}
		os.Exit(2)
	}
	var resp map[string]any
	if err := mustClient().do("POST", "/api/board/"+mode,
		map[string]string{"confirm": mode}, &resp); err != nil {
		fail(err)
	}
	fmt.Printf("ok %s %v\n", mode, resp[mode])
}

func cmdBoard(args []string) {
	fs := flag.NewFlagSet("board", flag.ExitOnError)
	fs.Parse(args)
	var resp struct {
		Counts map[string]int `json:"counts"`
	}
	if err := mustClient().do("GET", "/api/board", nil, &resp); err != nil {
		fail(err)
	}
	fmt.Printf("tasks=%d events=%d decisions=%d 待拍板=%d 未结回执=%d\n",
		resp.Counts["tasks"], resp.Counts["events"], resp.Counts["decisions"],
		resp.Counts["pending_decisions"], resp.Counts["open_replies"])
}

func cmdTake(args []string) {
	fs := flag.NewFlagSet("take", flag.ExitOnError)
	seq := fs.Int64("seq", -1, "reply event seq")
	agent := fs.String("agent", "", "agent now handling it")
	fs.Parse(args)
	if *seq < 0 {
		fmt.Fprintln(os.Stderr, "usage: agent-board take --seq N [--agent X]")
		os.Exit(2)
	}
	var resp struct {
		Reply Reply `json:"reply"`
	}
	if err := mustClient().do("PATCH", fmt.Sprintf("/api/replies/%d", *seq),
		map[string]string{"status": "processing", "agent": *agent}, &resp); err != nil {
		fail(err)
	}
	fmt.Printf("ok reply=%d status=%s\n", resp.Reply.Seq, resp.Reply.Status)
}

func cmdFail(args []string) {
	fs := flag.NewFlagSet("fail", flag.ExitOnError)
	seq := fs.Int64("seq", -1, "reply event seq")
	reason := fs.String("reason", "", "why it failed")
	fs.Parse(args)
	if *seq < 0 {
		fmt.Fprintln(os.Stderr, "usage: agent-board fail --seq N --reason \"...\"")
		os.Exit(2)
	}
	var resp struct {
		Reply Reply `json:"reply"`
	}
	if err := mustClient().do("PATCH", fmt.Sprintf("/api/replies/%d", *seq),
		map[string]string{"status": "failed", "note": *reason}, &resp); err != nil {
		fail(err)
	}
	fmt.Printf("ok reply=%d status=failed\n", resp.Reply.Seq)
}

func cmdMessage(args []string) {
	fs := flag.NewFlagSet("message", flag.ExitOnError)
	text := fs.String("text", "", "free message text")
	from := fs.String("from", "user", "author: user or agent name")
	task := fs.String("task", "", "link the message to a task")
	key := fs.String("key", "", "idempotency key")
	var images stringList
	fs.Var(&images, "image", "attach an image file (repeatable)")
	fs.Parse(args)
	if *text == "" && len(images) == 0 {
		fmt.Fprintln(os.Stderr, "usage: agent-board message --text \"...\" [--from main] [--task T-1] [--image a.png --image b.jpg]")
		os.Exit(2)
	}
	c := mustClient()
	ids, err := uploadImages(c, images)
	if err != nil {
		fail(err)
	}
	var resp struct {
		Event Event `json:"event"`
	}
	if err := c.do("POST", "/api/message",
		map[string]any{"text": *text, "from": *from, "task_id": *task, "key": *key, "images": ids}, &resp); err != nil {
		fail(err)
	}
	fmt.Printf("ok seq=%d\n", resp.Event.Seq)
}

func cmdReplies(args []string) {
	fs := flag.NewFlagSet("replies", flag.ExitOnError)
	recent := fs.Int("recent", 20, "how many")
	free := fs.Bool("free", false, "only free messages")
	fs.Parse(args)
	q := fmt.Sprintf("/api/replies?recent=%d", *recent)
	if *free {
		q += "&free=1"
	}
	var resp struct {
		Replies []Reply `json:"replies"`
	}
	if err := mustClient().do("GET", q, nil, &resp); err != nil {
		fail(err)
	}
	for _, r := range resp.Replies {
		fmt.Printf("%d\t%-10s\t%-6s\t%s\n", r.Seq, r.Status, r.TaskID, oneLine(r.Text, 80))
	}
}

func cmdAck(args []string) {
	fs := flag.NewFlagSet("ack", flag.ExitOnError)
	seq := fs.Int64("seq", -1, "event seq")
	consumer := fs.String("consumer", "main", "consumer name")
	fs.Parse(args)
	if *seq < 0 {
		fmt.Fprintln(os.Stderr, "usage: agent-board ack --seq N")
		os.Exit(2)
	}
	if err := mustClient().do("POST", "/api/ack", map[string]any{
		"consumer": *consumer, "seq": *seq}, nil); err != nil {
		fail(err)
	}
	fmt.Printf("ok acked seq=%d\n", *seq)
}

func cmdHealth(args []string) {
	fs := flag.NewFlagSet("health", flag.ExitOnError)
	fs.Parse(args)
	code, b, err := mustClient().raw("GET", "/api/health", nil, 5*time.Second)
	if err != nil {
		fail(err)
	}
	if code >= 400 {
		fail(errors.New(apiError(b, code)))
	}
	fmt.Println(strings.TrimSpace(string(b)))
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if max > 0 && len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

func shortTime(iso string) string {
	t, err := time.Parse(time.RFC3339Nano, iso)
	if err != nil {
		return iso
	}
	return t.Local().Format("01-02 15:04")
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
