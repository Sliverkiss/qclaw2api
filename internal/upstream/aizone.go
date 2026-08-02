// aizone.go QClaw 对话上游（aizone）调用：Bearer sk-apiKey + UA + system 注入。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"qclaw2api/internal/auth"
	"qclaw2api/internal/jprx"
)

// ErrKind 错误分类，pool 据此决定冷却时长（SPEC §5）。
type ErrKind int

const (
	ErrNone        ErrKind = iota // 成功
	ErrHardCredit                 // 403 api_key_inactive / 余额类 → 长冷却
	ErrSoftRate                   // 429 → 短冷却
	ErrSessionDead                // aizone 401 invalid_api_key / jprx 21004 → 禁用
	ErrServer                     // 5xx → 换号重试
	ErrClient                     // 其他 4xx / 业务错误
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// hardMarkers 余额/激活类错误关键词（小写比较）。
var hardMarkers = []string{
	"api_key_inactive",
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "payment required", "credit not enough",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽",
}

var sessionDeadMarkers = []string{"invalid_api_key", "21004", "登录已过期"}

// Classify 按 HTTP 状态码 + body 判定错误类别（SPEC §5）。
func Classify(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrHardCredit
		}
	}
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) || strings.Contains(lower, m) {
			return ErrSessionDead
		}
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if status >= 500 {
		return ErrServer
	}
	if status >= 400 {
		return ErrClient
	}
	return ErrNone
}

// Client 对话上游 HTTP 客户端。
type Client struct {
	// HTTP 非对话路径（生图/通用），main 可带总超时。
	HTTP *http.Client
	// chatHTTP 对话流式专用：仅 ResponseHeaderTimeout，无总超时（防 Client.Timeout 掐流式）。
	chatHTTP *http.Client
	chatURL  string
	jprx     *jprx.Client // 生图 4299 用（可注入测试）
}

// New 创建 aizone 上游客户端。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{
		HTTP:     &http.Client{Transport: tr},
		chatHTTP: &http.Client{Transport: tr},
		chatURL:  "https://mmgrcalltoken.3g.qq.com/aizone/v1/chat/completions",
		jprx:     jprx.New(),
	}
}

// SetResponseHeaderTimeout 设置对话请求的响应头超时（防 Client.Timeout 掐流式）。
// 对话 client（chatHTTP）无 http.Client.Timeout（会掐流式），只用 ResponseHeaderTimeout。
func (c *Client) SetResponseHeaderTimeout(d time.Duration) {
	if tr, ok := c.chatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = d
	}
}

// SetChatURL 覆盖 aizone 对话端点（测试注入 httptest server 用）。
func (c *Client) SetChatURL(u string) { c.chatURL = u }

// defaultSystem 是缺失 system role 时前插的默认 system 文案（覆盖网关 CodeBuddy 注入）。
const defaultSystem = "You are a helpful assistant."

// hasSystemRole 检查 messages 是否已含 system role。
func hasSystemRole(messages []any) bool {
	for _, mi := range messages {
		m, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		if r, _ := m["role"].(string); r == "system" {
			return true
		}
	}
	return false
}

// ensureSystem 在 messages 缺 system 时前插默认 system。
func ensureSystem(messages []any) []any {
	if hasSystemRole(messages) {
		return messages
	}
	head := make([]any, 0, len(messages)+1)
	head = append(head, map[string]any{"role": "system", "content": defaultSystem})
	head = append(head, messages...)
	return head
}

// PrepareBody 清洗 + 注入 system；返回最终发给 aizone 的 body。
func PrepareBody(raw []byte) ([]byte, error) {
	cleaned, err := CleanBody(raw)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(cleaned, &m); err != nil {
		return nil, err
	}
	if msgs, ok := m["messages"].([]any); ok {
		m["messages"] = ensureSystem(msgs)
	}
	return json.Marshal(m)
}

// aizoneHeaders 设置对话请求头：只留三个，绝不多带 x-qclaw-*（SPEC §1.4）。
func aizoneHeaders(h http.Header, a *auth.Auth) {
	h.Set("Authorization", "Bearer "+a.SKAPIKey)
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "OpenAI/JS 6.39.1")
}

// ChatStream 发 chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc 为 nil、body 为上游响应体、err 为 nil；传输层失败才返回 err。
func (c *Client) ChatStream(ctx context.Context, a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	prepared, err := PrepareBody(body)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("prepare body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatURL, bytes.NewReader(prepared))
	if err != nil {
		return nil, 0, nil, err
	}
	aizoneHeaders(req.Header, a)
	resp, err := c.chatHTTP.Do(req)
	if err != nil {
		log.Printf("chat_stream uid=%s: transport error: %v", a.UserID, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("chat_stream uid=%s: upstream %d %s body=%s",
			a.UserID, resp.StatusCode, kind, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// truncate 截断长文本用于错误信息。
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
