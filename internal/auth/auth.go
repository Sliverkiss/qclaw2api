// Package auth 解析 QClaw auth 文件（嵌套形/扁平形双形态），
// 提供原子写回与到期判断。
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Auth 是归一化后的 QClaw 账号凭证。
// 文件形态见 SPEC §3.1：
//
//	嵌套形 {"auth":{...},"account":{...}}   （cmd/login 落盘）
//	扁平形 {"jwt_token":...,"user_id":...}  （手建兼容）
type Auth struct {
	// mu 串行化 refresh 写与 SaveAtomic 读，防止并发写回半更新。
	mu sync.Mutex

	JWTToken     string // jprx 登录态（X-OpenClaw-Token），4026 返回 token
	ChannelToken string // openclaw_channel_token，4026 返回 / 4058 刷新
	SKAPIKey     string // sk_api_key，4055 下发，aizone 对话 Bearer
	UserID       string // user_id（X-Account-Id / X-Account）
	Nickname     string // 昵称
	GUID         string // qclawmp_<uuid>，签名/风控兜底
	ExpiresAt    int64  // Unix 秒，4026 expires_in=2592000 → now+30天
	FilePath     string // 来源文件；refresh 后原子写回此处
}

// Lock 供同进程内其他包（jprx X-New-Token 捕获）在改写字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// SnapshotJWT 持锁返回 JWTToken 副本，供无锁读侧（signHeaders 等）安全读取。
// JWTToken 会被 captureNewToken 并发改写，直接读会数据竞争。
func (a *Auth) SnapshotJWT() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.JWTToken
}

// Parse 兼容两种磁盘形态：嵌套形 {auth,account} 与扁平形（字段平铺）。
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var a Auth
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				JWTToken     string `json:"jwt_token"`
				ChannelToken string `json:"openclaw_channel_token"`
				SKAPIKey     string `json:"sk_api_key"`
				ExpiresAt    int64  `json:"expires_at"`
				GUID         string `json:"guid"`
			} `json:"auth"`
			Account struct {
				UserID   string `json:"user_id"`
				Nickname string `json:"nickname"`
			} `json:"account"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			JWTToken:     n.Auth.JWTToken,
			ChannelToken: n.Auth.ChannelToken,
			SKAPIKey:     n.Auth.SKAPIKey,
			ExpiresAt:    n.Auth.ExpiresAt,
			GUID:         n.Auth.GUID,
			UserID:       n.Account.UserID,
			Nickname:     n.Account.Nickname,
		}
	} else {
		var f struct {
			JWTToken     string `json:"jwt_token"`
			ChannelToken string `json:"openclaw_channel_token"`
			SKAPIKey     string `json:"sk_api_key"`
			ExpiresAt    int64  `json:"expires_at"`
			GUID         string `json:"guid"`
			UserID       string `json:"user_id"`
			Nickname     string `json:"nickname"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			JWTToken:     f.JWTToken,
			ChannelToken: f.ChannelToken,
			SKAPIKey:     f.SKAPIKey,
			ExpiresAt:    f.ExpiresAt,
			GUID:         f.GUID,
			UserID:       f.UserID,
			Nickname:     f.Nickname,
		}
	}
	if strings.TrimSpace(a.JWTToken) == "" {
		return nil, fmt.Errorf("parse_error: missing jwt_token")
	}
	return &a, nil
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename 0600）。
// 加锁外壳：防止与 refresh 并发读写字段导致写回半更新。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveAtomicLocked()
}

// SaveAtomicLocked 是 SaveAtomic 的持锁内部版本；调用方必须已持有 a.mu
// （配合 a.Lock()/a.Unlock() 使用，避免重复加锁）。
func (a *Auth) SaveAtomicLocked() error {
	return a.saveAtomicLocked()
}

// saveAtomicLocked 是 SaveAtomic 的持锁内部版本；调用方必须已持有 a.mu。
func (a *Auth) saveAtomicLocked() error {
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"jwt_token":              a.JWTToken,
			"openclaw_channel_token": a.ChannelToken,
			"sk_api_key":             a.SKAPIKey,
			"expires_at":             a.ExpiresAt,
			"guid":                   a.GUID,
		},
		"account": map[string]any{
			"user_id":  a.UserID,
			"nickname": a.Nickname,
		},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.FilePath)
}

// LoadDir 扫描 dir 下 qclaw-*.json，解析成功者纳入。
// 解析失败的文件静默跳过（启动日志由调用方统计）。
func LoadDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "qclaw-*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		out = append(out, a)
	}
	return out, nil
}
