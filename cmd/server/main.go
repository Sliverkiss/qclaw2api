// main.go qclaw2api 入口：加载配置，组装 pool/jprx/upstream/server/scheduler，起 HTTP 服务。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"qclaw2api/internal/auth"
	"qclaw2api/internal/jprx"
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

	// jprx 客户端
	jc := jprx.New()

	// aizone 上游（对话走 ResponseHeaderTimeout，其余走 Timeout）
	up := upstream.New()
	up.SetResponseHeaderTimeout(time.Duration(cfg.Upstream.ResponseHeaderTimeoutSeconds) * time.Second)
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	up.SetImageConfig(upstream.ImageConfig{
		PollInterval: time.Duration(cfg.Image.PollIntervalSeconds) * time.Second,
		PollTimeout:  time.Duration(cfg.Image.PollTimeoutSeconds) * time.Second,
	})

	// scheduler：每日冷却恢复探测（对冷却中账号发 max_tokens=1 最小对话测试）
	sch := scheduler.New(scheduler.Config{
		Pool:           p,
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
		Probe: func(acct *auth.Auth) error {
			probeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
			return nil
		},
	})

	// HTTP handler
	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		JPRX:         jc,
		APIKey:       cfg.APIKey,
		MaxRotate:    3,
		HardCooldown: cfg.HardCreditDur,
		SoftCooldown: cfg.SoftRateDur,
		ErrThreshold: cfg.Cooldown.ErrThresh,
		ErrCooldown:  cfg.ErrCooldownDur,
		RefreshSkew:  cfg.RefreshSkew,
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
