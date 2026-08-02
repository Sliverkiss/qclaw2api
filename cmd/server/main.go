// main.go qclaw2api 入口：加载配置，组装 pool/jprx/upstream/server/scheduler，起 HTTP 服务。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"qclaw2api/internal/auth"
	"qclaw2api/internal/pool"
	"qclaw2api/internal/scheduler"
	"qclaw2api/internal/server"
	"qclaw2api/internal/upstream"
)

// truncate 截断长文本用于错误信息。
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// probeContent 从非流式探测响应体解析 choices[0].message.content 并返回去空白后的内容。
// 空 choices / 解析失败返回错误——探测据此判断账号是否真正可用（P1-2）。
func probeContent(body []byte) (string, error) {
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("parse probe response: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("probe response has no choices")
	}
	return strings.TrimSpace(resp.Choices[0].Message.Content), nil
}

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// 配置文件不存在时给一次机会用纯默认 + env
		if os.IsNotExist(err) {
			log.Printf("config %s not found, using defaults+env", *cfgPath)
			cfg, err = Load("")
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}
	log.Printf("qclaw2api loaded config: listen=%s api_key=%v auth_dir=%s state_file=%s",
		cfg.Listen, cfg.APIKey != "", cfg.AuthDir, cfg.StateFile)

	// 加载 auth 账号
	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// 组装 pool
	p := pool.New(cfg.StateFile, pool.Config{
		RPM:          60,
		ErrThreshold: cfg.Cooldown.ErrThresh,
		ErrCooldown:  cfg.ErrCooldownDur,
	})
	for _, a := range auths {
		p.Add(a)
	}

	// aizone 上游（对话走 ResponseHeaderTimeout，其余走 Timeout）
	up := upstream.New()
	up.SetResponseHeaderTimeout(time.Duration(cfg.Upstream.ResponseHeaderTimeoutSeconds) * time.Second)
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second

	// scheduler：每日冷却恢复探测（对冷却中账号发 max_tokens=1 最小对话测试）
	sch := scheduler.New(scheduler.Config{
		Pool:           p,
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
		Probe: func(acct *auth.Auth) error {
			// HTTP 2xx 即视为通（P0-1）：max_tokens=1 小请求只需等响应头，不必等全量聚合。
			// 5s 超时覆盖慢上游（ResponseHeaderTimeout 由 up 配置）。
			probeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			body := []byte(`{"model":"default","messages":[{"role":"user","content":"hi"}],"max_tokens":1,"stream":false}`)
			rc, status, respBody, err := up.ChatStream(probeCtx, acct, body)
			if err != nil {
				return err
			}
			if rc != nil {
				defer rc.Close()
			}
			if status >= 400 {
				return fmt.Errorf("probe upstream %d: %s", status, truncate(string(respBody), 200))
			}
			// 2xx 还需校验 content 非空（P1-2）：聚合成功但空 content 视为账号不可用，避免误恢复。
			raw, err := io.ReadAll(io.LimitReader(rc, 1<<20))
			if err != nil {
				return fmt.Errorf("probe read body: %w", err)
			}
			content, err := probeContent(raw)
			if err != nil {
				return err
			}
			if content == "" {
				return fmt.Errorf("probe response has empty content")
			}
			return nil
		},
	})

	// HTTP handler
	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		ModelsFile:   cfg.ModelsFile,
		APIKey:       cfg.APIKey,
		MaxRotate:    3,
		SoftCooldown: cfg.SoftRateDur,
		ErrThreshold: cfg.Cooldown.ErrThresh,
		ErrCooldown:  cfg.ErrCooldownDur,
		// 非流式聚合总超时与其余请求一致（P1-5）
		AggregateTimeout: time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("qclaw2api listening on %s", cfg.Listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}
