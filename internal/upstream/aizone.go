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
)

// ErrKind 错误分类，pool 据此决定冷却时长（SPEC §5）。
type ErrKind int

const (
	ErrNone        ErrKind = iota // 成功
	ErrHardCredit                 // 余额/积分不足 → 下月1号冷却
	ErrInactive                   // api_key_inactive 账号未激活 → 短冷却
	ErrSoftRate                   // 429 → 短冷却
	ErrSessionDead                // aizone 401 invalid_api_key / jprx 21004 → 禁用
	ErrServer                     // 5xx → 换号重试
	ErrClient                     // 其他 4xx / 业务错误
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrInactive:
		return "inactive"
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

// balanceMarkers 积分不足类关键词 → ErrHardCredit（下月1号恢复，keepalive 探测解冻）。
var balanceMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "payment required", "credit not enough",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽",
	// 中文常见文案（P1-3）；「余额为 0/0」两种空格形态都覆盖，匹配保持大小写不敏感。
	"欠费", "余额为0", "余额为 0", "余额为零", "已停用",
}

// inactiveMarkers 账号未激活类关键词 → ErrInactive（固定短冷却，非余额问题）。
var inactiveMarkers = []string{"api_key_inactive"}

var sessionDeadMarkers = []string{"invalid_api_key", "21004", "登录已过期"}

// Classify 按 HTTP 状态码 + body 判定错误类别（SPEC §2.3）。
// 顺序：inactiveMarkers → balanceMarkers → sessionDeadMarkers → 429 → 5xx → 其余 4xx。
// inactiveMarkers 前置（P1-8）：api_key_inactive 错误体可能同时含余额关键词，先判未激活。
func Classify(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	for _, m := range inactiveMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrInactive
		}
	}
	for _, m := range balanceMarkers {
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
	// HTTP 通用路径，main 可带总超时。
	HTTP *http.Client
	// chatHTTP 对话流式专用：仅 ResponseHeaderTimeout，无总超时（防 Client.Timeout 掐流式）。
	chatHTTP *http.Client
	chatURL  string
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

// hasSystemRole 检查 messages 是否已含**非空** system role。
// 实测（2026-08-02）：system 缺失 → 400 invalid request；system content 为空字符串 → 同样 400。
// 因此空 content 的 system 视为「无有效 system」，需注入默认值。
func hasSystemRole(messages []any) bool {
	for _, mi := range messages {
		m, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		if r, _ := m["role"].(string); r == "system" {
			if c, _ := m["content"].(string); c != "" {
				return true
			}
		}
	}
	return false
}

// ensureSystem 在 messages 缺有效 system 时前插默认 system。
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
	// 强制 identity：禁用 gzip/deflate，确保上游 SSE 分帧原样传输（P1-6）。
	// 压缩层会把 SSE 事件缓冲成整块，破坏流式逐帧 flush。
	req.Header.Set("Accept-Encoding", "identity")
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
