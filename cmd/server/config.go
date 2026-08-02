// config.go 加载 JSON 配置 + QC2A_* 环境变量覆盖。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 顶层配置。
type Config struct {
	Listen    string `json:"listen"`     // ":7865"
	APIKey    string `json:"api_key"`    // 空 = 不鉴权（本地调试）
	AuthDir   string `json:"auth_dir"`   // ./auths
	StateFile string `json:"state_file"` // ./data/state.json

	Cooldown struct {
		HardCredit  string `json:"hard_credit"`   // "12h"
		SoftRate    string `json:"soft_rate"`     // "60s"
		ErrThresh   int    `json:"err_threshold"` // 连续错误次数阈值，默认 5
		ErrCooldown string `json:"err_cooldown"`  // "10m"
	} `json:"cooldown"`

	Schedule struct {
		KeepaliveHours      []int `json:"keepalive_hours"`       // 每日 4058 续期整点，默认 [4]
		CreditIntervalHours int   `json:"credit_interval_hours"` // 积分刷新间隔小时，默认 6
	} `json:"schedule"`

	Upstream struct {
		ResponseHeaderTimeoutSeconds int `json:"response_header_timeout_seconds"` // 对话上游响应头超时，默认 30
		TimeoutSeconds               int `json:"timeout_seconds"`                 // 其余请求超时，默认 120
	} `json:"upstream"`

	Image struct {
		PollIntervalSeconds int `json:"poll_interval_seconds"` // 生图轮询间隔，默认 3
		PollTimeoutSeconds  int `json:"poll_timeout_seconds"`  // 生图轮询总超时，默认 120
	} `json:"image"`

	Refresh struct {
		SkewDays int `json:"skew_days"` // 请求前惰性 refresh 的提前量，默认 7
	} `json:"refresh"`

	// 解析后的 duration 字段（不来自 JSON）
	HardCreditDur  time.Duration `json:"-"`
	SoftRateDur    time.Duration `json:"-"`
	ErrCooldownDur time.Duration `json:"-"`
	RefreshSkew    time.Duration `json:"-"`
}

// Default 返回默认配置。
func Default() *Config {
	c := &Config{
		Listen:    ":7865",
		APIKey:    "",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
	}
	c.Cooldown.HardCredit = "12h"
	c.Cooldown.SoftRate = "60s"
	c.Cooldown.ErrThresh = 5
	c.Cooldown.ErrCooldown = "10m"
	c.Schedule.KeepaliveHours = []int{4}
	c.Schedule.CreditIntervalHours = 6
	c.Upstream.ResponseHeaderTimeoutSeconds = 30
	c.Upstream.TimeoutSeconds = 120
	c.Image.PollIntervalSeconds = 3
	c.Image.PollTimeoutSeconds = 120
	c.Refresh.SkewDays = 7
	return c
}

// Load 从文件读取配置，再用 QC2A_* env 覆盖，最后解析 duration 字段。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("QC2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("QC2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("QC2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("QC2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("QC2A_HARD_CREDIT"); v != "" {
		c.Cooldown.HardCredit = v
	}
	if v := os.Getenv("QC2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("QC2A_ERR_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Cooldown.ErrThresh = n
		}
	}
	if v := os.Getenv("QC2A_ERR_COOLDOWN"); v != "" {
		c.Cooldown.ErrCooldown = v
	}
	if v := os.Getenv("QC2A_KEEPALIVE_HOURS"); v != "" {
		c.Schedule.KeepaliveHours = parseHourList(v)
	}
	if v := os.Getenv("QC2A_CREDIT_INTERVAL_HOURS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Schedule.CreditIntervalHours = n
		}
	}
	if v := os.Getenv("QC2A_RESPONSE_HEADER_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Upstream.ResponseHeaderTimeoutSeconds = n
		}
	}
	if v := os.Getenv("QC2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("QC2A_IMAGE_POLL_INTERVAL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Image.PollIntervalSeconds = n
		}
	}
	if v := os.Getenv("QC2A_IMAGE_POLL_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Image.PollTimeoutSeconds = n
		}
	}
	if v := os.Getenv("QC2A_REFRESH_SKEW_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Refresh.SkewDays = n
		}
	}
}

// parseHourList 解析 "4,16,22" 形式的整点小时列表。
func parseHourList(v string) []int {
	var out []int
	for _, part := range strings.Split(v, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 0 || n > 23 {
			continue
		}
		out = append(out, n)
	}
	return out
}

func (c *Config) normalize() error {
	var err error
	if c.HardCreditDur, err = time.ParseDuration(c.Cooldown.HardCredit); err != nil {
		return fmt.Errorf("cooldown.hard_credit: %w", err)
	}
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	if c.ErrCooldownDur, err = time.ParseDuration(c.Cooldown.ErrCooldown); err != nil {
		return fmt.Errorf("cooldown.err_cooldown: %w", err)
	}
	if c.Cooldown.ErrThresh <= 0 {
		c.Cooldown.ErrThresh = 5
	}
	if len(c.Schedule.KeepaliveHours) == 0 {
		c.Schedule.KeepaliveHours = []int{4}
	}
	if c.Schedule.CreditIntervalHours <= 0 {
		c.Schedule.CreditIntervalHours = 6
	}
	if c.Upstream.ResponseHeaderTimeoutSeconds <= 0 {
		c.Upstream.ResponseHeaderTimeoutSeconds = 30
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	if c.Image.PollIntervalSeconds <= 0 {
		c.Image.PollIntervalSeconds = 3
	}
	if c.Image.PollTimeoutSeconds <= 0 {
		c.Image.PollTimeoutSeconds = 120
	}
	if c.Refresh.SkewDays <= 0 {
		c.Refresh.SkewDays = 7
	}
	c.RefreshSkew = time.Duration(c.Refresh.SkewDays) * 24 * time.Hour
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	return nil
}
