package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

const version = "0.1.0"

type paths struct {
	data     string
	db       string
	token    string
	password string
}

func resolvePaths(dataFlag, dbFlag string) (paths, error) {
	data := dataFlag
	if data == "" {
		data = os.Getenv("BOARD_DATA")
	}
	if data == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return paths{}, err
		}
		data = filepath.Join(home, ".agent-board")
	}
	if err := os.MkdirAll(data, 0o700); err != nil {
		return paths{}, err
	}
	db := dbFlag
	if db == "" {
		db = filepath.Join(data, "board.db")
	}
	return paths{
		data:     data,
		db:       db,
		token:    filepath.Join(data, "token"),
		password: filepath.Join(data, "password.txt"),
	}, nil
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", envOr("BOARD_ADDR", "127.0.0.1:8765"), "listen address")
	dataFlag := fs.String("data", "", "data directory (default ~/.agent-board)")
	dbFlag := fs.String("db", "", "sqlite path (default <data>/board.db)")
	tokenFlag := fs.String("token", "", "bearer token (default: env BOARD_TOKEN or <data>/token)")
	passFlag := fs.String("password", "", "web password (default: env BOARD_PASSWORD, or generated once)")
	secure := fs.Bool("secure-cookie", false, "set Secure on session cookie (behind HTTPS)")
	fs.Parse(args)

	p, err := resolvePaths(*dataFlag, *dbFlag)
	if err != nil {
		log.Fatal(err)
	}
	store, err := OpenStore(p.db)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	token := *tokenFlag
	if token == "" {
		token = os.Getenv("BOARD_TOKEN")
	}
	if token == "" {
		if b, err := os.ReadFile(p.token); err == nil {
			token = strings.TrimSpace(string(b))
		}
	}
	if token == "" {
		token, err = randomHex(32)
		if err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(p.token, []byte(token+"\n"), 0o600); err != nil {
			log.Fatal(err)
		}
	}

	keyHex, err := store.metaGet("session_key")
	if err != nil {
		keyHex, err = randomHex(32)
		if err != nil {
			log.Fatal(err)
		}
		if err := store.metaSet("session_key", keyHex); err != nil {
			log.Fatal(err)
		}
	}
	sessionKey, err := hex.DecodeString(keyHex)
	if err != nil {
		log.Fatal(err)
	}

	passHash, hashErr := store.metaGet("password_hash")
	pass := *passFlag
	if pass == "" {
		pass = os.Getenv("BOARD_PASSWORD")
	}
	generated := ""
	if pass != "" {
		passHash = hashPassword(pass)
		if err := store.metaSet("password_hash", passHash); err != nil {
			log.Fatal(err)
		}
		os.Remove(p.password)
	} else if hashErr == ErrNotFound {
		generated, err = randomHex(8)
		if err != nil {
			log.Fatal(err)
		}
		passHash = hashPassword(generated)
		if err := store.metaSet("password_hash", passHash); err != nil {
			log.Fatal(err)
		}
		_ = os.WriteFile(p.password, []byte(generated+"\n"), 0o600)
	}

	fmt.Printf("agent-board %s\n", version)
	fmt.Printf("  url:      http://%s\n", *addr)
	fmt.Printf("  data:     %s\n", p.data)
	fmt.Printf("  db:       %s\n", p.db)
	fmt.Printf("  token:    %s\n", p.token)
	if generated != "" {
		fmt.Printf("  password: %s  (generated, saved to %s)\n", generated, p.password)
	} else {
		fmt.Printf("  password: %s\n", map[bool]string{true: "set via flag/env", false: "already set"}[pass != ""])
	}

	srv := NewServer(store, Config{Addr: *addr, Version: version, SecureCookie: *secure},
		token, passHash, sessionKey)
	if err := srv.Run(); err != nil {
		log.Fatal(err)
	}
}

func cmdPasswd(args []string) {
	fs := flag.NewFlagSet("passwd", flag.ExitOnError)
	dataFlag := fs.String("data", "", "data directory")
	dbFlag := fs.String("db", "", "sqlite path")
	pass := fs.String("password", "", "new password")
	fs.Parse(args)
	if *pass == "" {
		fmt.Fprintln(os.Stderr, "usage: agent-board passwd --password NEW")
		os.Exit(2)
	}
	p, err := resolvePaths(*dataFlag, *dbFlag)
	if err != nil {
		log.Fatal(err)
	}
	store, err := OpenStore(p.db)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	if err := store.metaSet("password_hash", hashPassword(*pass)); err != nil {
		log.Fatal(err)
	}
	os.Remove(p.password)
	fmt.Println("password updated")
}

func cmdToken(args []string) {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	dataFlag := fs.String("data", "", "data directory")
	tokenFlag := fs.String("token", "", "token to save")
	fs.Parse(args)
	if *tokenFlag != "" {
		p, err := resolvePaths(*dataFlag, "")
		if err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(p.token, []byte(*tokenFlag+"\n"), 0o600); err != nil {
			log.Fatal(err)
		}
		fmt.Println(p.token)
		return
	}
	p, err := resolvePaths(*dataFlag, "")
	if err != nil {
		log.Fatal(err)
	}
	if b, err := os.ReadFile(p.token); err == nil {
		fmt.Print(string(b))
		return
	}
	fmt.Fprintln(os.Stderr, "no token file; start the server once or pass --token")
	os.Exit(1)
}

func usage() {
	fmt.Print(`agent-board — 本地 Agent 任务看板

  agent-board serve [--addr 127.0.0.1:8765] [--password ...] [--token ...]
  agent-board task new --title "..." [--project ...] [--agent ...] [--column todo]
  agent-board task list [--json]
  agent-board task show T-1
  agent-board move T-1 doing|blocked|done|todo
  agent-board progress T-1 "一行进展" [--agent ...]
  agent-board note T-1 "备注" [--agent ...] [--session ...]
  agent-board done T-1 --text "结论"
  agent-board ask --task T-1 --question "..." [--options "同意,重做,稍后"]
  agent-board decisions [--status pending]
  agent-board watch [--consumer main] [--since N]
  agent-board dispatch --seq N --to "Agent" [--note "..."]
  agent-board ack --seq N [--consumer main]
  agent-board take --seq N [--agent X]
  agent-board fail --seq N --reason "..."
  agent-board message --text "..."
  agent-board replies [--recent 20] [--free]
  agent-board reset --yes          # 清空一切，编号重来
  agent-board archive --yes        # 全部收进已完成，保留历史
  agent-board board                # 当前数量
  agent-board passwd --password NEW
  agent-board token
  agent-board health

环境变量：BOARD_URL（默认 http://127.0.0.1:8765）、BOARD_TOKEN、BOARD_DATA
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "task":
		cmdTask(os.Args[2:])
	case "move":
		cmdMove(os.Args[2:])
	case "event":
		cmdEvent(os.Args[2:], "note")
	case "progress":
		cmdQuickEvent(os.Args[2:], "progress")
	case "note":
		cmdQuickEvent(os.Args[2:], "note")
	case "done":
		cmdDone(os.Args[2:])
	case "ask":
		cmdAsk(os.Args[2:])
	case "decisions":
		cmdDecisions(os.Args[2:])
	case "watch":
		cmdWatch(os.Args[2:])
	case "dispatch":
		cmdDispatch(os.Args[2:])
	case "ack":
		cmdAck(os.Args[2:])
	case "take":
		cmdTake(os.Args[2:])
	case "fail":
		cmdFail(os.Args[2:])
	case "message":
		cmdMessage(os.Args[2:])
	case "replies":
		cmdReplies(os.Args[2:])
	case "reset":
		cmdBoardLifecycle(os.Args[2:], "reset")
	case "archive":
		cmdBoardLifecycle(os.Args[2:], "archive")
	case "board":
		cmdBoard(os.Args[2:])
	case "passwd":
		cmdPasswd(os.Args[2:])
	case "token":
		cmdToken(os.Args[2:])
	case "health":
		cmdHealth(os.Args[2:])
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}
