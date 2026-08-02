package upstream

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"qclaw2api/internal/auth"
)

func chatAuth() *auth.Auth {
	return &auth.Auth{
		JWTToken:  "jwt-chat",
		SKAPIKey:  "sk-test-chat",
		UserID:    "chat-1",
		ExpiresAt: time.Now().Add(30 * 24 * time.Hour).Unix(),
	}
}

// TestChatStreamSlowStream 复现 P0-2：http.Client.Timeout 会掐断流式 body 读取。
// chatHTTP 独立且无总超时 → 慢流式完整接收 + [DONE]。
func TestChatStreamSlowStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for i := 0; i < 5; i++ {
			fmt.Fprintf(w, `data: {"id":"x","choices":[{"delta":{"content":"c%d"}}]}`+"\n\n", i)
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(150 * time.Millisecond)
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()

	c := New()
	c.SetChatURL(srv.URL)
	// 模拟 main.go：非对话 HTTP client 带总超时。若 ChatStream 误用 c.HTTP，
	// 短总超时会在 ~250ms 掐断流式（总时长 ~750ms），chunk 丢失。
	c.HTTP.Timeout = 250 * time.Millisecond

	rc, status, _, err := c.ChatStream(context.Background(), chatAuth(),
		[]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer rc.Close()
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	raw, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	body := string(raw)
	for _, want := range []string{`"content":"c0"`, `"content":"c4"`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}
