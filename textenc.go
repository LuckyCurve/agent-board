package main

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// toUTF8 兜住 Windows 控制台的编码坑。
// 中文 Windows 的 Git Bash / cmd 里敲中文，参数到进程手里常常已经是 GBK 字节。
// 存进库后网页按 UTF-8 显示就是「ɸѡ」这种乱码，命令行却看着正常（它按 GBK 解）。
//
// 两种形态都要认：
//   - 非法 UTF-8：直接按 GBK 解一次；
//   - 合法 UTF-8，但整串落在 U+0100–U+2E7F（西欧重音、IPA、西里尔…），一个汉字都没有：
//     GBK 的双字节碰巧拼成了合法 UTF-8。这种才换，否则「界面」这类真中文会被反过来打坏。
func toUTF8(s string) string {
	if s == "" {
		return s
	}
	if !utf8.ValidString(s) {
		if out, err := simplifiedchinese.GBK.NewDecoder().String(s); err == nil && hasCJK(out) {
			return out
		}
		return s
	}
	if !looksLikeGBKMojibake(s) {
		return s
	}
	out, err := simplifiedchinese.GBK.NewDecoder().String(s)
	if err != nil || !hasCJK(out) {
		return s
	}
	// 往返校验：换回来的字按 GBK 编回去必须还是原来那串字节。
	if back, err := simplifiedchinese.GBK.NewEncoder().String(out); err != nil || back != s {
		return s
	}
	return out
}

// looksLikeGBKMojibake：每个字符都在 U+0100–U+2E7F 之间。
// 真中文至少有一个字落在 U+4E00 以上，直接排除；纯 ASCII 也排除。
func looksLikeGBKMojibake(s string) bool {
	for _, r := range s {
		if r < 0x100 || r > 0x2E7F {
			return false
		}
	}
	return true
}

func hasCJK(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

// cleanText 给用户可见的短文本（标签、标题、项目名、Agent 名）用。
func cleanText(s string) string {
	return strings.TrimSpace(toUTF8(s))
}

// repairMojibake 把老库里已经写坏的标签扶正一次。
// 只动「看着就是 GBK 乱码」的那几条，正常标签原样留着；冲突的（扶正后撞上已有标签）直接删。
func (s *Store) repairMojibake() error {
	rows, err := s.db.Query(`SELECT DISTINCT tag FROM task_tags`)
	if err != nil {
		return err
	}
	var bad []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return err
		}
		if fixed := toUTF8(t); fixed != t {
			bad = append(bad, t)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(bad) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, old := range bad {
		fixed := toUTF8(old)
		if _, err := tx.Exec(`INSERT OR IGNORE INTO task_tags(task_id,tag)
			SELECT task_id, ? FROM task_tags WHERE tag=?`, fixed, old); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM task_tags WHERE tag=?`, old); err != nil {
			return err
		}
	}
	return tx.Commit()
}
