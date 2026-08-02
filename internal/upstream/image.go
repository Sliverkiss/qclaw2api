// image.go QClaw 生图上游（jprx 4299）：提交 {prompt} → 轮询重放同 prompt → image_url。
package upstream

import (
	"context"
	"errors"
	"time"

	"qclaw2api/internal/auth"
	"qclaw2api/internal/jprx"
)

// ImageConfig 生图轮询参数。
type ImageConfig struct {
	PollInterval time.Duration
	PollTimeout  time.Duration
}

// ErrImageTimeout 生图轮询超时哨兵错误（调用方据此不冷却账号）。
var ErrImageTimeout = errors.New("image generation timed out")

var (
	imagePollInterval = 3 * time.Second
	imagePollTimeout  = 120 * time.Second
)

// SetImageConfig 设置生图轮询参数（main 组装时调用）。
func (c *Client) SetImageConfig(cfg ImageConfig) {
	if cfg.PollInterval > 0 {
		imagePollInterval = cfg.PollInterval
	}
	if cfg.PollTimeout > 0 {
		imagePollTimeout = cfg.PollTimeout
	}
}

// SetJPRX 注入 jprx 客户端（测试指向 httptest 用）。
func (c *Client) SetJPRX(j *jprx.Client) {
	if j != nil {
		c.jprx = j
	}
}

// GenerateImage 4299 提交生图并轮询（重放同一 prompt），返回 image_url。
// 轮询超时返回 errImageTimeout（调用方不冷却账号）。
func (c *Client) GenerateImage(ctx context.Context, acct *auth.Auth, prompt string) (string, error) {
	j := c.jprx
	if j == nil {
		j = jprx.New()
	}
	// 不共享 c.HTTP（F8/P1-7）：c.HTTP 可能带总超时（main 里 120s），会掐掉生图提交/轮询；
	// 用 jprx 自己的 client，单请求超时各自控制，轮询总时长由 imagePollTimeout 约束。

	// 提交
	res, err := j.GenerateImage(ctx, acct, prompt)
	if err != nil {
		return "", err
	}
	// 若一次就成功（非 pending）直接返回
	if res.Status == "success" && res.ImageURL != "" {
		return res.ImageURL, nil
	}

	// 轮询：重放同一 prompt（SPEC §3.6 实测机制）
	deadline := time.Now().Add(imagePollTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(imagePollInterval):
		}
		res, err = j.GenerateImage(ctx, acct, prompt)
		if err != nil {
			return "", err
		}
		switch res.Status {
		case "success":
			if res.ImageURL == "" {
				return "", errors.New("image success without url")
			}
			return res.ImageURL, nil
		case "failed":
			return "", errors.New("image generation failed upstream")
		}
		// pending / 其他 → 继续轮询
	}
	return "", ErrImageTimeout
}
