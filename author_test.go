package main

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	b, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func replyBySeq(t *testing.T, replies []any, seq int64) map[string]any {
	t.Helper()
	for _, raw := range replies {
		r, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if int64(r["seq"].(float64)) == seq {
			return r
		}
	}
	t.Fatalf("reply seq %d not found in %v", seq, replies)
	return nil
}

func TestMessageAuthorSeparatesUserAndAgent(t *testing.T) {
	e := newTestEnv(t)

	code, body, _ := e.req(t, "POST", "/api/message", map[string]any{"text": "用户提问"})
	if code != http.StatusCreated {
		t.Fatalf("user message: %d %v", code, body)
	}
	userEv := body["event"].(map[string]any)
	if userEv["author"] != "user" {
		t.Fatalf("user message author = %v, want user", userEv["author"])
	}
	userSeq := int64(userEv["seq"].(float64))

	code, body, _ = e.req(t, "POST", "/api/message", map[string]any{
		"text": "主会话回话", "from": "main"})
	if code != http.StatusCreated {
		t.Fatalf("agent message: %d %v", code, body)
	}
	agentEv := body["event"].(map[string]any)
	if agentEv["author"] != "main" {
		t.Fatalf("agent message author = %v, want main", agentEv["author"])
	}
	agentSeq := int64(agentEv["seq"].(float64))

	code, body, _ = e.req(t, "GET", "/api/replies?free=1&recent=20", nil)
	if code != http.StatusOK {
		t.Fatalf("free replies: %d %v", code, body)
	}
	replies := body["replies"].([]any)
	userReply := replyBySeq(t, replies, userSeq)
	if userReply["author"] != "user" {
		t.Fatalf("user reply author = %v", userReply["author"])
	}
	if userReply["status"] != "received" {
		t.Fatalf("user reply status = %v, want received", userReply["status"])
	}
	agentReply := replyBySeq(t, replies, agentSeq)
	if agentReply["author"] != "main" {
		t.Fatalf("agent reply author = %v", agentReply["author"])
	}
	if agentReply["status"] != "done" {
		t.Fatalf("agent reply status = %v, want done (no ack needed)", agentReply["status"])
	}
}

func TestWatchLabelsUserAndAgentMessages(t *testing.T) {
	user := captureStdout(t, func() {
		printEventLine(Event{Seq: 1, Type: "event", Kind: "user_reply", Author: "user", Body: "用户说"})
	})
	if !strings.Contains(user, "BOARD_USER_MESSAGE") {
		t.Fatalf("user message line = %q", user)
	}
	if strings.Contains(user, "BOARD_AGENT_MESSAGE") {
		t.Fatalf("user message wrongly labelled agent: %q", user)
	}

	agent := captureStdout(t, func() {
		printEventLine(Event{Seq: 2, Type: "event", Kind: "user_reply", Author: "main", Body: "主会话说"})
	})
	if !strings.Contains(agent, "BOARD_AGENT_MESSAGE") || !strings.Contains(agent, "author=main") {
		t.Fatalf("agent message line = %q", agent)
	}
	if strings.Contains(agent, "BOARD_USER_MESSAGE") {
		t.Fatalf("agent message wrongly labelled user: %q", agent)
	}

	// 旧数据没有 author，按用户输入处理，保证 watch 仍会派活。
	if got := eventAuthor(Event{Kind: "user_reply"}); got != "user" {
		t.Fatalf("legacy event author = %q, want user", got)
	}
}
