package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const version = "0.1.0"

type paths struct {
	data           string
	db             string
	token          string
	legacyPassword string
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
		data:           data,
		db:             db,
		token:          filepath.Join(data, "token"),
		legacyPassword: filepath.Join(data, "password.txt"),
	}, nil
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", envOr("BOARD_ADDR", "127.0.0.1:8765"), "listen address")
	dataFlag := fs.String("data", "", "data directory (default ~/.agent-board)")
	dbFlag := fs.String("db", "", "sqlite path (default <data>/board.db)")
	passFlag := fs.String("password", "", "web password (default: env BOARD_PASSWORD; generated on first start)")
	tokenFlag := fs.String("token", "", "agent token (default: env BOARD_TOKEN; generated once into <data>/token)")
	secure := fs.Bool("secure-cookie", false, "set Secure on session cookie (behind HTTPS)")
	deployTrigger := fs.String("deploy-trigger", envOr("BOARD_DEPLOY_TRIGGER", ""), "trigger file for 'update now' (default: env BOARD_DEPLOY_TRIGGER)")
	vapidSub := fs.String("vapid-sub", envOr("VAPID_SUB", ""), "contact for web push (mailto: or https:)")
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

	pass := *passFlag
	if pass == "" {
		pass = os.Getenv("BOARD_PASSWORD")
	}
	generated, err := ensurePassword(store, pass)
	if err != nil {
		log.Fatal(err)
	}
	// 旧版本把口令明文写进文件；启动时顺手清掉。
	os.Remove(p.legacyPassword)

	tokenProvided := *tokenFlag
	if tokenProvided == "" {
		tokenProvided = os.Getenv("BOARD_TOKEN")
	}
	generatedToken, err := ensureToken(p.token, tokenProvided)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("agent-board %s\n", version)
	fmt.Printf("  url:      http://%s\n", *addr)
	fmt.Printf("  data:     %s\n", p.data)
	fmt.Printf("  db:       %s\n", p.db)
	fmt.Printf("  token:    %s\n", p.token)
	switch {
	case generated != "":
		fmt.Printf("  password: 首次启动口令 %s（只显示这一次，请记下）\n", generated)
	case pass != "":
		fmt.Printf("  password: 已按启动参数更新\n")
	default:
		fmt.Printf("  password: 已设置\n")
	}
	switch {
	case generatedToken != "":
		fmt.Printf("  agent token: %s（只显示这一次，同时写入上面的文件）\n", generatedToken)
	case tokenProvided != "":
		fmt.Printf("  agent token: 已按启动参数写入文件\n")
	default:
		fmt.Printf("  agent token: 已从文件加载\n")
	}

	srv := NewServer(store, Config{Addr: *addr, Version: version, SecureCookie: *secure,
		LoginDelay: 250 * time.Millisecond, TokenFile: p.token,
		DeployTrigger: *deployTrigger,
		VapidSub:      *vapidSub,
		ImagesDir:     filepath.Join(p.data, "images")})
	if n, err := srv.cleanupImages(30 * 24 * time.Hour); err == nil && n > 0 {
		fmt.Printf("  images:   清理了 %d 张 30 天前的旧图\n", n)
	}
	if err := srv.Run(); err != nil {
		log.Fatal(err)
	}
}

// ensurePassword 保证库里有一份盐哈希口令。首次启动生成随机口令并返回它，
// 调用方只该把它打到控制台一次；之后每次启动都复用库里的哈希。
func ensurePassword(store *Store, provided string) (generated string, err error) {
	existing, getErr := store.metaGet("password_hash")
	if getErr != nil && getErr != ErrNotFound {
		return "", getErr
	}
	if provided != "" {
		if strings.TrimSpace(provided) == "" {
			return "", fmt.Errorf("password cannot be blank")
		}
		if getErr == nil && verifyPassword(existing, provided) {
			return "", nil
		}
		if err := store.metaSet("password_hash", hashPassword(provided)); err != nil {
			return "", err
		}
		return "", store.DeleteAllSessions()
	}
	if getErr == nil {
		return "", nil
	}
	generated, err = randomHex(10)
	if err != nil {
		return "", err
	}
	if err := store.metaSet("password_hash", hashPassword(generated)); err != nil {
		return "", err
	}
	return generated, nil
}

// ensureToken 让 token 文件成为唯一事实源。首次启动生成并返回它一次；
// 之后只读文件，值本身不打印、不进日志。
func ensureToken(path, provided string) (generated string, err error) {
	if provided != "" {
		return "", os.WriteFile(path, []byte(provided+"\n"), 0o600)
	}
	if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) != "" {
		return "", nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	generated, err = randomHex(32)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(generated+"\n"), 0o600); err != nil {
		return "", err
	}
	return generated, nil
}

func cmdToken(args []string) {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	dataFlag := fs.String("data", "", "data directory")
	rotate := fs.Bool("new", false, "generate a new token")
	agent := fs.String("agent", "", "bind a new token to this agent (it only sees that agent's cards)")
	fs.Parse(args)
	p, err := resolvePaths(*dataFlag, "")
	if err != nil {
		log.Fatal(err)
	}
	if *agent != "" {
		// 给某个 Agent 单独发一把令牌：它只能碰自己名下的卡。
		store, err := OpenStore(p.db)
		if err != nil {
			log.Fatal(err)
		}
		defer store.Close()
		tok, err := randomHex(32)
		if err != nil {
			log.Fatal(err)
		}
		if err := store.SaveAgentToken(sessionHash(tok), *agent); err != nil {
			log.Fatal(err)
		}
		fmt.Fprintf(os.Stderr, "给「%s」发了一把令牌（只显示这一次）\n", *agent)
		fmt.Println(tok)
		return
	}
	if *rotate {
		tok, err := randomHex(32)
		if err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(p.token, []byte(tok+"\n"), 0o600); err != nil {
			log.Fatal(err)
		}
		fmt.Fprintln(os.Stderr, "new token written to "+p.token)
		fmt.Println(tok)
		return
	}
	b, err := os.ReadFile(p.token)
	if err != nil {
		fmt.Fprintln(os.Stderr, "no token file; start `agent-board serve` once or run `agent-board token --new`")
		os.Exit(1)
	}
	fmt.Print(string(b))
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
	if err := store.DeleteAllSessions(); err != nil {
		log.Fatal(err)
	}
	os.Remove(p.legacyPassword)
	fmt.Println("password updated; existing sessions revoked")
}

func usage() {
	fmt.Print(`agent-board — 本地 Agent 任务看板

  agent-board serve [--addr 127.0.0.1:8765] [--password ...] [--token ...]
  agent-board token [--new]              # 查看 / 重发 Agent token
  agent-board task new --title "..." [--project ...] [--agent ...] [--column todo]
  agent-board task list [--json]
  agent-board task show T-1
  agent-board move T-1 todo|doing|blocked|done|archived
  agent-board task-archive T-1          # 收进归档，编号不变
  agent-board task-delete T-1 --yes     # 永久删除，未结项会拒绝
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
  agent-board message --text "..." [--from main] [--task T-1]
  agent-board replies [--recent 20] [--free]
  agent-board reset --yes          # 清空一切，编号重来
  agent-board archive --yes        # 全部收进已完成，保留历史
  agent-board board                # 当前数量
  agent-board passwd --password NEW
  agent-board health

环境变量：BOARD_URL（默认 http://127.0.0.1:8765）、BOARD_TOKEN（默认读 <data>/token）、BOARD_PASSWORD、BOARD_DATA、BOARD_AGENT
每个子命令都可加 --as "Agent"（或设 BOARD_AGENT）：只看见该 Agent 名下的数据，越权返回 403；不加则是主会话，全局可见。
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	// 「我是谁」从整条命令行里认，子命令不用各自处理。
	cliIdentity = scanIdentity(os.Args[2:])
	os.Args = append(os.Args[:2:2], stripIdentityArgs(os.Args[2:])...)
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
	case "task-archive":
		cmdTaskArchive(os.Args[2:])
	case "task-delete":
		cmdTaskDelete(os.Args[2:])
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
