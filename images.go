package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const defaultImageMaxBytes int64 = 5 << 20
const maxImagesPerMessage = 6

var imageNameRE = regexp.MustCompile(`^[a-f0-9]{32}\.(png|jpg|webp)$`)

func (s *Server) imagesLimit() int64 {
	if s.cfg.ImageMaxBytes > 0 {
		return s.cfg.ImageMaxBytes
	}
	return defaultImageMaxBytes
}

// detectImageExt 只认魔数，不信任请求里的 Content-Type 或文件名。
func detectImageExt(b []byte) string {
	switch {
	case len(b) >= 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return ".png"
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return ".jpg"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return ".webp"
	}
	return ""
}

// POST /api/images 收原始图片字节，返回可挂到消息/事件上的 id。
func (s *Server) handleImageUpload(w http.ResponseWriter, r *http.Request) {
	limit := s.imagesLimit()
	body := http.MaxBytesReader(w, r.Body, limit)
	b, err := io.ReadAll(body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("图片最多 %d MB", limit>>20))
			return
		}
		writeErr(w, http.StatusBadRequest, "read image failed")
		return
	}
	ext := detectImageExt(b)
	if ext == "" {
		writeErr(w, http.StatusBadRequest, "只收 PNG / JPG / WebP")
		return
	}
	if err := os.MkdirAll(s.imagesDir, 0o700); err != nil {
		writeErr(w, http.StatusInternalServerError, "image store unavailable")
		return
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		writeErr(w, http.StatusInternalServerError, "image store unavailable")
		return
	}
	id := hex.EncodeToString(raw) + ext
	tmp := filepath.Join(s.imagesDir, id+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		writeErr(w, http.StatusInternalServerError, "save image failed")
		return
	}
	if err := os.Rename(tmp, filepath.Join(s.imagesDir, id)); err != nil {
		os.Remove(tmp)
		writeErr(w, http.StatusInternalServerError, "save image failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "url": "/images/" + id})
}

// authedMedia 给 <img src> 用：网页登录态、Bearer、或查询参数 token 都认。
func (s *Server) authedMedia(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.hasSession(r) || s.validToken(bearerToken(r)) || s.validToken(r.URL.Query().Get("token")) {
			fn(w, r)
			return
		}
		writeErr(w, http.StatusUnauthorized, "unauthorized")
	}
}

// GET /images/{name} 只发数据目录 images 下的白名单文件名，阻断路径穿越。
func (s *Server) handleImageGet(w http.ResponseWriter, r *http.Request) {
	name := path.Base(r.PathValue("name"))
	if !imageNameRE.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	full := filepath.Join(s.imagesDir, name)
	if _, err := os.Stat(full); err != nil {
		http.NotFound(w, r)
		return
	}
	if ct := mime.TypeByExtension(filepath.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeFile(w, r, full)
}

// validateImageIDs 拒绝路径、未知 id，以及一次挂太多张。
func (s *Server) validateImageIDs(ids []string) error {
	if len(ids) > maxImagesPerMessage {
		return fmt.Errorf("最多 %d 张图", maxImagesPerMessage)
	}
	for _, id := range ids {
		if !imageNameRE.MatchString(id) {
			return errors.New("invalid image id")
		}
		if _, err := os.Stat(filepath.Join(s.imagesDir, id)); err != nil {
			return errors.New("image not found")
		}
	}
	return nil
}

// cleanupImages 清掉超过 maxAge 的图片文件，避免长期堆积；0 表示不清理。
func (s *Server) cleanupImages(maxAge time.Duration) (int, error) {
	if maxAge <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(s.imagesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().Add(-maxAge)
	n := 0
	for _, e := range entries {
		if e.IsDir() || !imageNameRE.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(s.imagesDir, e.Name())); err == nil {
			n++
		}
	}
	return n, nil
}

func mergeImagesIntoData(data string, ids []string) (string, error) {
	if len(ids) == 0 {
		return data, nil
	}
	m := map[string]any{}
	if strings.TrimSpace(data) != "" {
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			return "", errors.New("data 必须是 JSON 对象")
		}
	}
	m["images"] = ids
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
