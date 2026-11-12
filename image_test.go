package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// 1x1 透明 PNG，足够过魔数检查。
const tinyPNG64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(tinyPNG64)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func uploadImage(t *testing.T, e *testEnv, data []byte, contentType, token string) (int, map[string]any) {
	return uploadImageAs(t, e.client, e, data, contentType, token)
}

func uploadImageAs(t *testing.T, cl *http.Client, e *testEnv, data []byte, contentType, token string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest("POST", e.ts.URL+"/api/images", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func TestImageUploadRequiresTokenAndWhitelistsTypes(t *testing.T) {
	e := newTestEnv(t)

	// 匿名（无 cookie 无 token）不给上传。
	code, _ := uploadImageAs(t, noRedirectClient(), e, tinyPNG(t), "image/png", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("anonymous upload: %d, want 401", code)
	}

	code, body := uploadImage(t, e, []byte("GIF89a not allowed"), "image/gif", e.token)
	if code != http.StatusBadRequest {
		t.Fatalf("gif upload: %d %v, want 400", code, body)
	}

	code, body = uploadImage(t, e, []byte("not really a png"), "image/png", e.token)
	if code != http.StatusBadRequest {
		t.Fatalf("fake png: %d %v, want 400", code, body)
	}
}

func TestImageUploadAndAuthenticatedFetch(t *testing.T) {
	e := newTestEnv(t)
	png := tinyPNG(t)
	code, body := uploadImage(t, e, png, "image/png", e.token)
	if code != http.StatusCreated {
		t.Fatalf("upload: %d %v", code, body)
	}
	id, _ := body["id"].(string)
	if !regexp.MustCompile(`^[a-f0-9]{32}\.png$`).MatchString(id) {
		t.Fatalf("bad image id %q", id)
	}
	if body["url"] != "/images/"+id {
		t.Fatalf("bad url %v", body["url"])
	}

	// 网页带着登录 cookie 能看
	resp, err := e.client.Get(e.ts.URL + "/images/" + id)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, png) {
		t.Fatalf("session fetch: %d, bytes equal=%v", resp.StatusCode, bytes.Equal(got, png))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content type %q", ct)
	}

	// 匿名不给看
	anon := &http.Client{}
	resp, err = anon.Get(e.ts.URL + "/images/" + id)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous image fetch: %d, want 401", resp.StatusCode)
	}
}

func TestImageUploadSizeLimit(t *testing.T) {
	e := newTestServer(t, Config{Version: "test", ImageMaxBytes: 64}, testPassHash)
	e.client = loginWithJar(t, e.ts.URL, "test-pw")
	code, body := uploadImage(t, e, append(tinyPNG(t), make([]byte, 200)...), "image/png", e.token)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize upload: %d %v, want 413", code, body)
	}
}

func TestMessageWithImages(t *testing.T) {
	e := newTestEnv(t)
	code, body := uploadImage(t, e, tinyPNG(t), "image/png", e.token)
	if code != http.StatusCreated {
		t.Fatalf("upload: %d %v", code, body)
	}
	id := body["id"].(string)

	code, body, _ = e.req(t, "POST", "/api/message", map[string]any{
		"text": "", "from": "agent", "images": []string{id}})
	if code != http.StatusCreated {
		t.Fatalf("image-only message: %d %v", code, body)
	}

	code, body, _ = e.req(t, "GET", "/api/chat", nil)
	if code != http.StatusOK {
		t.Fatalf("chat: %d", code)
	}
	msgs := body["messages"].([]any)
	var found bool
	for _, m := range msgs {
		ev := m.(map[string]any)
		var data map[string]any
		_ = json.Unmarshal([]byte(ev["data"].(string)), &data)
		imgs, _ := data["images"].([]any)
		for _, im := range imgs {
			if im == id {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("chat message missing image %s", id)
	}

	code, _, _ = e.req(t, "POST", "/api/message", map[string]any{
		"text": "x", "images": []string{"../board.db"}})
	if code != http.StatusBadRequest {
		t.Fatalf("unsafe image id: %d, want 400", code)
	}
}

func TestImageCleanupRemovesOldFiles(t *testing.T) {
	e := newTestEnv(t)
	dir := e.srv.imagesDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, strings.Repeat("a", 32)+".png")
	keep := filepath.Join(dir, strings.Repeat("b", 32)+".png")
	for _, p := range []string{old, keep} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	n, err := e.srv.cleanupImages(24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old image should be removed")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("recent image should stay")
	}
}
