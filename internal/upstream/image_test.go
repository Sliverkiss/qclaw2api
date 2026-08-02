package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"qclaw2api/internal/auth"
	"qclaw2api/internal/jprx"
)

func imgAuth() *auth.Auth {
	return &auth.Auth{
		JWTToken:     "jwt-img",
		ChannelToken: "ct-img",
		SKAPIKey:     "sk-test-img",
		UserID:       "999",
		GUID:         "qclawmp_img",
		ExpiresAt:    time.Now().Add(30 * 24 * time.Hour).Unix(),
	}
}

// fake4299 假 4299 网关：按状态序列返回。
type fake4299 struct {
	mu       atomic.Int32
	statuses []string // 每次请求消费一个状态
	images   []string
}

func (f *fake4299) handler(w http.ResponseWriter, r *http.Request) {
	idx := int(f.mu.Add(1)) - 1
	w.Header().Set("Content-Type", "application/json")
	if idx >= len(f.statuses) {
		idx = len(f.statuses) - 1
	}
	status := f.statuses[idx]
	img := ""
	if status == "success" && len(f.images) > idx {
		img = f.images[idx]
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ret": 0, "data": map[string]any{
			"resp": map[string]any{"common": map[string]any{"code": 0}, "data": map[string]any{
				"status":     status,
				"image_url":  img,
				"request_id": "req-1",
			}},
		},
	})
}

// newImageClient 组装带假 4299 网关的 upstream.Client。
func newImageClient(t *testing.T, f *fake4299) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	jc := jprx.New()
	jc.SetBase(srv.URL)
	c := New()
	c.SetJPRX(jc)
	c.SetImageConfig(ImageConfig{PollInterval: 10 * time.Millisecond, PollTimeout: time.Second})
	return c
}

// TestGenerateImageImmediateSuccess 首次就 success。
func TestGenerateImageImmediateSuccess(t *testing.T) {
	f := &fake4299{statuses: []string{"success"}, images: []string{"http://img/1.png"}}
	c := newImageClient(t, f)

	url, err := c.GenerateImage(context.Background(), imgAuth(), "a cat")
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if url != "http://img/1.png" {
		t.Errorf("url = %q", url)
	}
}

// TestGenerateImagePendingThenSuccess 提交 pending → 轮询 success。
func TestGenerateImagePendingThenSuccess(t *testing.T) {
	f := &fake4299{statuses: []string{"pending", "success"}, images: []string{"", "http://img/2.png"}}
	c := newImageClient(t, f)

	url, err := c.GenerateImage(context.Background(), imgAuth(), "a dog")
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if url != "http://img/2.png" {
		t.Errorf("url = %q", url)
	}
}

// TestGenerateImageFailed 轮询 failed → 错误。
func TestGenerateImageFailed(t *testing.T) {
	f := &fake4299{statuses: []string{"pending", "failed"}}
	c := newImageClient(t, f)

	if _, err := c.GenerateImage(context.Background(), imgAuth(), "a bird"); err == nil {
		t.Fatal("expected error on failed")
	}
}

// TestGenerateImageTimeout 一直 pending → 超时返回 ErrImageTimeout。
func TestGenerateImageTimeout(t *testing.T) {
	f := &fake4299{statuses: []string{"pending", "pending", "pending", "pending", "pending"}}
	c := newImageClient(t, f)
	c.SetImageConfig(ImageConfig{PollInterval: 5 * time.Millisecond, PollTimeout: 20 * time.Millisecond})

	_, err := c.GenerateImage(context.Background(), imgAuth(), "a fish")
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

// TestGenerateImageIndependentClient 校验生图不被 c.HTTP 总超时误伤（F8/P1-7）。
// c.HTTP.Timeout 极短（模拟 main 里非对话路径的总超时），
// 若 GenerateImage 仍共享 c.HTTP，假 4299 延迟 150ms 会报传输错误（context deadline exceeded）。
// 修复后 jprx 用独立 client，单请求不受 c.HTTP.Timeout 约束 → 正常出图。
func TestGenerateImageIndependentClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond) // 延迟 > c.HTTP.Timeout(50ms)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ret": 0, "data": map[string]any{
				"resp": map[string]any{"common": map[string]any{"code": 0}, "data": map[string]any{
					"status": "success", "image_url": "http://img/independent.png", "request_id": "req-1",
				}},
			},
		})
	}))
	t.Cleanup(srv.Close)

	jc := jprx.New()
	jc.SetBase(srv.URL)
	c := New()
	c.SetJPRX(jc)
	c.SetImageConfig(ImageConfig{PollInterval: 5 * time.Millisecond, PollTimeout: 2 * time.Second})
	c.HTTP.Timeout = 50 * time.Millisecond // 非对话路径总超时（修复前会注入并误伤生图）

	url, err := c.GenerateImage(context.Background(), imgAuth(), "a horse")
	if err != nil {
		t.Fatalf("GenerateImage: %v (want success, must not be killed by c.HTTP.Timeout)", err)
	}
	if url != "http://img/independent.png" {
		t.Errorf("url = %q", url)
	}
}
