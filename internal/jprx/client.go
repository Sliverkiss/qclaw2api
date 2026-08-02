// client.go jprx HTTP 客户端：签名头构造、多级信封容错解析、
// X-New-Token 捕获与各 cmd 封装。
package jprx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"qclaw2api/internal/auth"
)

// Client 是 jprx 网关客户端。
type Client struct {
	HTTP         *http.Client
	base         string
	captureToken bool // X-New-Token 捕获开关：仅登录链路开启
}

// New 创建默认 jprx 客户端。
func New() *Client {
	return &Client{
		HTTP: &http.Client{Timeout: 120 * time.Second},
		base: BaseURL,
	}
}

// SetBase 覆盖网关基址（测试注入 httptest server 用）。
func (c *Client) SetBase(base string) { c.base = base }

// SetCaptureNewToken 开启/关闭 X-New-Token 捕获（仅登录链路开启，运行期不捕获）。
func (c *Client) SetCaptureNewToken(on bool) { c.captureToken = on }

// JPRXError 是信封 code 非 0 时的业务错误。
type JPRXError struct {
	Code    int
	Message string
	Cmd     string
}

func (e *JPRXError) Error() string {
	return fmt.Sprintf("jprx %s error code=%d msg=%s", e.Cmd, e.Code, e.Message)
}

// IsLoginExpired 报告错误码是否为 21004（登录已过期）。
func (e *JPRXError) IsLoginExpired() bool { return e.Code == 21004 }

// parseEnvelope 容错解析 jprx 响应信封，返回业务码、业务 payload 与 message。
// 支持三种形态（SPEC §1.1/§1.3）：
//
//	形态 A（data 集）：{ret:0, data:{resp:{common:{code,message}, data:{...}}}}
//	形态 B（4055）：  {common:{code}, data:{...}}
//	形态 C（扁平）：  {code, data, message}
func parseEnvelope(raw []byte) (code int, payload []byte, message string, err error) {
	var top struct {
		Ret     int    `json:"ret"`
		Code    *int   `json:"code"`
		Message string `json:"message"`
		Common  *struct {
			Code    *int   `json:"code"`
			Message string `json:"message"`
		} `json:"common"`
		Data json.RawMessage `json:"data"`
	}
	if err = json.Unmarshal(raw, &top); err != nil {
		return 0, nil, "", fmt.Errorf("envelope: parse: %w", err)
	}
	code = -1 // 未显式给出时保持 -1，由调用方判断
	if top.Code != nil {
		code = *top.Code
	}
	if top.Common != nil && top.Common.Code != nil {
		code = *top.Common.Code
	}
	if top.Common != nil {
		message = top.Common.Message
	}
	if top.Message != "" {
		message = top.Message
	}

	payload = top.Data
	if len(top.Data) > 0 {
		var inner struct {
			Common *struct {
				Code    *int   `json:"code"`
				Message string `json:"message"`
			} `json:"common"`
			Resp *struct {
				Common *struct {
					Code    *int   `json:"code"`
					Message string `json:"message"`
				} `json:"common"`
				Data json.RawMessage `json:"data"`
			} `json:"resp"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(top.Data, &inner); err == nil {
			if inner.Resp != nil {
				if inner.Resp.Common != nil && inner.Resp.Common.Code != nil {
					code = *inner.Resp.Common.Code
				}
				if inner.Resp.Common != nil {
					message = inner.Resp.Common.Message
				}
				payload = inner.Resp.Data
			} else if inner.Common != nil {
				if inner.Common.Code != nil {
					code = *inner.Common.Code
				}
				if inner.Common.Message != "" {
					message = inner.Common.Message
				}
				payload = inner.Data
			} else if len(inner.Data) > 0 {
				payload = inner.Data
			}
		}
	}
	return code, payload, message, nil
}

// signHeaders 填充 jprx 签名头与登录态头。
func signHeaders(h http.Header, body []byte, acct *auth.Auth) error {
	ts := Timestamp()
	sig, _, err := Sign(body, ts)
	if err != nil {
		return err
	}
	h.Set("Content-Type", "application/json")
	h.Set("X-Sign-Timestamp", ts)
	h.Set("X-Sign-Signature", sig)
	h.Set("X-OpenClaw-ClientVersion", ClientVersion)
	h.Set("X-Session", "")
	if acct != nil {
		// JWTToken 会被 captureNewToken 并发改写，必须走快照读（F4）。
		if jwt := acct.SnapshotJWT(); jwt != "" {
			h.Set("X-OpenClaw-Token", jwt)
		}
		if acct.GUID != "" {
			h.Set("X-Guid", acct.GUID)
		}
		if acct.UserID != "" {
			h.Set("X-Account", acct.UserID)
			h.Set("X-Account-Id", acct.UserID)
		}
	}
	return nil
}

// doForward 执行一次带签名的 jprx POST，返回原始响应 body 与响应头。
// path 形如 /data/<cmd>/forward 或 /api/v1/4055。
func (c *Client) doForward(ctx context.Context, path string, body []byte, acct *auth.Auth) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("jprx: new request: %w", err)
	}
	if err := signHeaders(req.Header, body, acct); err != nil {
		return nil, nil, fmt.Errorf("jprx: sign: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("jprx: %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("jprx: read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return raw, resp.Header, fmt.Errorf("jprx %s http %d: %s", path, resp.StatusCode, truncate(raw, 300))
	}
	return raw, resp.Header, nil
}

// captureNewToken 检查响应头 X-New-Token：非空且异于当前 JWT → 覆盖并原子落盘。
func captureNewToken(h http.Header, acct *auth.Auth) error {
	if acct == nil {
		return nil
	}
	newTok := h.Get("X-New-Token")
	if newTok == "" {
		return nil
	}
	acct.Lock()
	defer acct.Unlock()
	// 全部判断移入锁内（锁外首读 acct.JWTToken 是数据竞争，F4）
	if newTok == acct.JWTToken {
		return nil
	}
	acct.JWTToken = newTok
	if acct.FilePath != "" {
		return acct.SaveAtomicLocked()
	}
	return nil
}

// exec 执行 cmd 并做信封解析与 X-New-Token 捕获，返回业务 payload。
func (c *Client) exec(ctx context.Context, cmd string, body []byte, acct *auth.Auth, useAPIV1 bool) ([]byte, error) {
	path := "/data/" + cmd + "/forward"
	if useAPIV1 {
		path = "/api/v1/" + cmd
	}
	raw, h, err := c.doForward(ctx, path, body, acct)
	if err != nil {
		// HTTP 层失败也尝试捕获 X-New-Token（部分网关错误仍带续期头）
		if c.captureToken && h != nil {
			_ = captureNewToken(h, acct)
		}
		return nil, err
	}
	if c.captureToken {
		if err := captureNewToken(h, acct); err != nil {
			return nil, fmt.Errorf("jprx %s capture token: %w", cmd, err)
		}
	}
	code, payload, msg, err := parseEnvelope(raw)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, &JPRXError{Code: code, Message: msg, Cmd: cmd}
	}
	return payload, nil
}

// ---- cmd 封装 ----

// WxLoginState 4050：拿 CSRF state。
func (c *Client) WxLoginState(ctx context.Context, guid string) (string, error) {
	body, _ := json.Marshal(map[string]string{"guid": guid})
	payload, err := c.exec(ctx, "4050", body, nil, false)
	if err != nil {
		return "", err
	}
	var r struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(payload, &r); err != nil {
		return "", fmt.Errorf("4050: parse: %w", err)
	}
	if r.State == "" {
		return "", fmt.Errorf("4050: empty state")
	}
	return r.State, nil
}

// LoginResult 4026 登录产物。
type LoginResult struct {
	JWTToken     string `json:"token"`
	ExpiresIn    int64  `json:"expires_in"`
	ChannelToken string `json:"openclaw_channel_token"`
	UserID       string `json:"user_id"`
	Nickname     string `json:"nickname"`
	IsNewUser    bool   `json:"is_new_user"`
}

// rawToIDString 兼容 user_id 的数字/字符串两种形态（实测 4026 返回数字）。
func rawToIDString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	}
	return strings.TrimSpace(string(raw))
}

// WxLogin 4026：code 换 JWT。
func (c *Client) WxLogin(ctx context.Context, guid, code, state string) (*LoginResult, error) {
	body, _ := json.Marshal(map[string]string{"guid": guid, "code": code, "state": state})
	payload, err := c.exec(ctx, "4026", body, nil, false)
	if err != nil {
		return nil, err
	}
	var r struct {
		Token      string `json:"token"`
		ExpiresIn  int64  `json:"expires_in"`
		ChannelTok string `json:"openclaw_channel_token"`
		UserInfo   struct {
			UserID   json.RawMessage `json:"user_id"`
			Nickname string          `json:"nickname"`
		} `json:"user_info"`
		IsNewUser bool `json:"is_new_user"`
	}
	if err := json.Unmarshal(payload, &r); err != nil {
		return nil, fmt.Errorf("4026: parse: %w", err)
	}
	if r.Token == "" {
		return nil, fmt.Errorf("4026: empty token")
	}
	return &LoginResult{
		JWTToken:     r.Token,
		ExpiresIn:    r.ExpiresIn,
		ChannelToken: r.ChannelTok,
		UserID:       rawToIDString(r.UserInfo.UserID),
		Nickname:     r.UserInfo.Nickname,
		IsNewUser:    r.IsNewUser,
	}, nil
}

// APIKey 4055 产物。
type APIKey struct {
	Key       string `json:"key"`
	MaskedKey string `json:"masked_key"`
	KeyHash   string `json:"key_hash"`
	CreatedAt string `json:"created_at"`
}

// GetAPIKey 4055：走 api/v1/4055，幂等返回 sk-apiKey。
func (c *Client) GetAPIKey(ctx context.Context, acct *auth.Auth) (*APIKey, error) {
	body := []byte(`{"web_version":"1.4.0","web_env":"release"}`)
	payload, err := c.exec(ctx, "4055", body, acct, true)
	if err != nil {
		return nil, err
	}
	var r struct {
		Key       string `json:"key"`
		MaskedKey string `json:"masked_key"`
		KeyHash   string `json:"key_hash"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(payload, &r); err != nil {
		return nil, fmt.Errorf("4055: parse: %w", err)
	}
	if r.Key == "" {
		return nil, fmt.Errorf("4055: empty key")
	}
	return &APIKey{Key: r.Key, MaskedKey: r.MaskedKey, KeyHash: r.KeyHash, CreatedAt: r.CreatedAt}, nil
}

// BalanceItem 4110 余额明细条目。
// 注意：Q 点数值是浮点（实测 2499.99 等小数），必须用 float64 而非 int64。
type BalanceItem struct {
	Label        string  `json:"label"`
	TotalAmount  float64 `json:"total_amount"`
	RemainAmount float64 `json:"remain_amount"`
	ExpireTime   string  `json:"expire_time"`
}

// QBalance 4110 Q 点账户。
type QBalance struct {
	Balance       float64 `json:"balance"`
	BalanceDetail struct {
		ActivityQ float64       `json:"activity_q"`
		Items     []BalanceItem `json:"items"`
	} `json:"balance_detail"`
}

// GetQBalance 4110：查询 Q 点。
func (c *Client) GetQBalance(ctx context.Context, acct *auth.Auth) (*QBalance, error) {
	payload, err := c.exec(ctx, "4110", []byte(`{}`), acct, false)
	if err != nil {
		return nil, err
	}
	var r QBalance
	if err := json.Unmarshal(payload, &r); err != nil {
		return nil, fmt.Errorf("4110: parse: %w", err)
	}
	return &r, nil
}

// TodayTokens 4075 今日额度。
type TodayTokens struct {
	DailyTokenLimit int64 `json:"daily_token_limit"`
	DailyTokenUsed  int64 `json:"daily_token_used"`
	RPMLimit        int   `json:"rpm_limit"`
}

// GetTodayTokens 4075：今日剩余 tokens。
func (c *Client) GetTodayTokens(ctx context.Context, acct *auth.Auth) (*TodayTokens, error) {
	payload, err := c.exec(ctx, "4075", []byte(`{}`), acct, false)
	if err != nil {
		return nil, err
	}
	var r TodayTokens
	if err := json.Unmarshal(payload, &r); err != nil {
		return nil, fmt.Errorf("4075: parse: %w", err)
	}
	return &r, nil
}

// GetUsage 4172：用量明细。
func (c *Client) GetUsage(ctx context.Context, acct *auth.Auth, start, end string) ([]byte, error) {
	body, _ := json.Marshal(map[string]string{"start_date": start, "end_date": end})
	return c.exec(ctx, "4172", body, acct, false)
}

// ModelStatus 4320 模型列表。
type ModelStatus struct {
	ModelStatusList []ModelInfo `json:"model_status_list"`
}

// ModelInfo 单个模型。
type ModelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// GetModelStatus 4320：模型列表。
func (c *Client) GetModelStatus(ctx context.Context, acct *auth.Auth) (*ModelStatus, error) {
	payload, err := c.exec(ctx, "4320", []byte(`{}`), acct, false)
	if err != nil {
		return nil, err
	}
	var r ModelStatus
	if err := json.Unmarshal(payload, &r); err != nil {
		return nil, fmt.Errorf("4320: parse: %w", err)
	}
	if len(r.ModelStatusList) == 0 {
		return nil, fmt.Errorf("4320: empty model_status_list")
	}
	return &r, nil
}

// truncate 截断长文本用于错误信息。
func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
