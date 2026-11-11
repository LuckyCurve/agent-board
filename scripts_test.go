package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 仓库自带的东西要齐：监听脚本、给 Agent 看的接入说明、给人看的说明。
func repoFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("读不到 %s: %v", name, err)
	}
	return string(b)
}

func TestWatchScriptsExistAndCoverLifecycle(t *testing.T) {
	sh := repoFile(t, filepath.Join("scripts", "watch.sh"))
	ps := repoFile(t, filepath.Join("scripts", "watch.ps1"))
	for _, want := range []string{"start", "stop", "status", "logs"} {
		if !strings.Contains(sh, want) {
			t.Fatalf("watch.sh 缺 %s", want)
		}
		if !strings.Contains(ps, want) {
			t.Fatalf("watch.ps1 缺 %s", want)
		}
	}
	for _, want := range []string{"watch", "--consumer", "--wait"} {
		if !strings.Contains(sh, want) {
			t.Fatalf("watch.sh 没调起 watch: %s", want)
		}
		if !strings.Contains(ps, want) {
			t.Fatalf("watch.ps1 没调起 watch: %s", want)
		}
	}
}

func TestAgentsDocIsSplitInTwo(t *testing.T) {
	doc := repoFile(t, "AGENTS.md")
	use := strings.Index(doc, "使用本仓库时")
	dev := strings.Index(doc, "开发本仓库时")
	if use < 0 || dev < 0 {
		t.Fatalf("AGENTS.md 要分「使用本仓库时」和「开发本仓库时」两章: use=%d dev=%d", use, dev)
	}
	if use > dev {
		t.Fatal("「使用本仓库时」应该在「开发本仓库时」前面")
	}
	// 使用那章第一件事：中继只分发。
	head := doc[use:dev]
	if !strings.Contains(head, "只做分发") {
		t.Fatal("使用章节开头要写明「中继只分发，不亲自动手」")
	}
	// 可直接复制的启动命令。
	for _, want := range []string{"watch.sh start", "watch.ps1 start", "notify.py poll"} {
		if !strings.Contains(head, want) {
			t.Fatalf("使用章节缺可直接复制的启动命令：%s", want)
		}
	}
}

func TestReadmeIsSplitInTwo(t *testing.T) {
	doc := repoFile(t, "README.md")
	what := strings.Index(doc, "看板是什么")
	how := strings.Index(doc, "Agent 怎么接入")
	if what < 0 || how < 0 {
		t.Fatalf("README 要分「看板是什么」和「Agent 怎么接入」两章: what=%d how=%d", what, how)
	}
	if what > how {
		t.Fatal("「看板是什么」应该在「Agent 怎么接入」前面")
	}
}

// 口令、token、数据库都不许进仓库。
func TestNoSecretsInRepo(t *testing.T) {
	ignore := repoFile(t, ".gitignore")
	for _, want := range []string{"token", "*.db", "*.db-wal", "*.db-shm", "password.txt"} {
		if !strings.Contains(ignore, want) {
			t.Fatalf(".gitignore 没挡住 %s", want)
		}
	}
	docs := []string{"AGENTS.md", "README.md",
		filepath.Join("scripts", "watch.sh"), filepath.Join("scripts", "watch.ps1")}
	for _, name := range docs {
		for _, line := range strings.Split(repoFile(t, name), "\n") {
			for _, f := range strings.Fields(line) {
				f = strings.Trim(f, "\"'`(),")
				if len(f) >= 32 && isHex(f) {
					t.Fatalf("%s 里像是有密钥：%s", name, f)
				}
			}
		}
	}
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}
