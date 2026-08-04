// Package scheduler 定时任务：每日冷却恢复探测（keepalive）。
// 对冷却中账号发 max_tokens=1 最小对话测试，通过则恢复（Recover），失败保持冷却。
package scheduler

import (
	"context"
	"log"
	"sync"
	"time"

	"qclaw2api/internal/auth"
	"qclaw2api/internal/pool"
)

// Config scheduler 构造参数。
type Config struct {
	Pool           *pool.Pool
	KeepaliveHours []int // 每日探测整点，默认 [0]（中国时区 0 点）
	Probe          func(*auth.Auth) error
}

// Scheduler 定时任务调度器。
type Scheduler struct {
	cfg          Config
	mu           sync.Mutex
	keepaliveDay string // 当日 keepalive 已执行标记
	now          func() time.Time
}

// New 创建调度器。
func New(cfg Config) *Scheduler {
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{0} // R3：默认中国时区 0 点
	}
	return &Scheduler{cfg: cfg, now: time.Now}
}

// setNow 覆盖时间函数（测试注入）。
func (s *Scheduler) setNow(f func() time.Time) { s.now = f }

// Run 启动调度循环：等待整点触发 keepalive。
func (s *Scheduler) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(1 * time.Minute):
			s.tick(ctx)
		}
	}
}

// tick 每分钟检查一次：当前小时在 keepalive 列表且当日未跑过 → 探测冷却中账号。
func (s *Scheduler) tick(ctx context.Context) {
	now := s.now()
	hour := now.Hour()
	dayKey := now.Format("2006-01-02")
	if contains(s.cfg.KeepaliveHours, hour) && !s.ranKeepaliveToday(dayKey) {
		s.RunKeepaliveNow(ctx, dayKey)
	}
}

// ranKeepaliveToday 报告当日 keepalive 是否已执行。
func (s *Scheduler) ranKeepaliveToday(dayKey string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keepaliveDay == dayKey
}

func contains(hours []int, h int) bool {
	for _, x := range hours {
		if x == h {
			return true
		}
	}
	return false
}

// RunKeepaliveNow 立即对冷却中账号做对话探测：成功 Recover，失败保持冷却。
// R3：冷却无到期——探测失败不滚动 CooldownCredit，账号保持冷却直到下次探测通过。
// 禁用账号跳过。当日去重由调用方（tick/Run）负责。
func (s *Scheduler) RunKeepaliveNow(ctx context.Context, dayKey string) {
	s.mu.Lock()
	s.keepaliveDay = dayKey
	s.mu.Unlock()

	for _, uid := range s.cfg.Pool.CoolingUIDs() {
		acct := s.cfg.Pool.AuthByUID(uid)
		if acct == nil {
			continue
		}
		// Probe 内部自带 15s 超时，防止上游挂起阻塞 keepalive。
		if err := s.cfg.Probe(acct); err == nil {
			s.cfg.Pool.Recover(uid)
			log.Printf("keepalive: uid=%s probe OK, recovered", uid)
		} else {
			// R3：探测失败 → 保持冷却，只 log（不调 CooldownCredit 滚动）。
			// 冷却无到期，恢复完全由后续 keepalive 探测决定。
			log.Printf("keepalive: uid=%s probe fail, stay cooling: %v", uid, err)
		}
	}
}
