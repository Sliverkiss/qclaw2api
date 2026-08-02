// Package scheduler 定时任务：积分刷新（4110+4075）+ token 续期（4058）。
// QClaw 无签到机制（SPEC §1.6），故不含 checkin。
package scheduler

import (
	"context"
	"log"
	"sync"
	"time"

	"qclaw2api/internal/jprx"
	"qclaw2api/internal/pool"
)

// Config scheduler 构造参数。
type Config struct {
	Pool                *pool.Pool
	JPRX                *jprx.Client
	KeepaliveHours      []int // 每日 4058 续期整点，默认 [4]
	CreditIntervalHours int   // 积分刷新间隔小时，默认 6
}

// Scheduler 定时任务调度器。
type Scheduler struct {
	cfg          Config
	mu           sync.Mutex
	creditLast   time.Time // 上次积分刷新时间
	keepaliveDay string    // 当日 keepalive 已执行标记
	now          func() time.Time
}

// New 创建调度器。
func New(cfg Config) *Scheduler {
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{4}
	}
	if cfg.CreditIntervalHours <= 0 {
		cfg.CreditIntervalHours = 6
	}
	return &Scheduler{cfg: cfg, now: time.Now}
}

// setNow 覆盖时间函数（测试注入）。
func (s *Scheduler) setNow(f func() time.Time) { s.now = f }

// Run 启动调度循环：启动时立即积分刷新一次，之后按间隔循环。
func (s *Scheduler) Run(ctx context.Context) {
	s.RunCreditNow(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(1 * time.Minute):
			s.tick(ctx)
		}
	}
}

// tick 每分钟检查一次：到达间隔小时 → 积分刷新；当前小时在 keepalive 列表 → 续期。
func (s *Scheduler) tick(ctx context.Context) {
	now := s.now()
	hour := now.Hour()
	interval := s.cfg.CreditIntervalHours

	// 积分刷新：距上次超过 interval 小时
	if s.creditLast.IsZero() || now.Sub(s.creditLast) >= time.Duration(interval)*time.Hour {
		s.RunCreditNow(ctx)
	}

	// keepalive：当前小时在 keepalive_hours 且当日未跑过
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

// RunCreditNow 立即对全账号刷新积分（4110 Q 点 + 4075 今日额度），
// 余额>0 自动解冻 hard_credit。冷却/禁用账号跳过；账号间隔 200ms。
func (s *Scheduler) RunCreditNow(ctx context.Context) {
	s.mu.Lock()
	s.creditLast = s.now()
	s.mu.Unlock()

	for _, uid := range s.cfg.Pool.UIDs() {
		acct := s.cfg.Pool.AuthByUID(uid)
		if acct == nil {
			continue
		}
		st, ok := s.cfg.Pool.Status(uid)
		if ok && (st.Disabled || st.Cooling) {
			continue
		}
		// 4110 Q 点
		if qb, err := s.cfg.JPRX.GetQBalance(ctx, acct); err == nil {
			s.cfg.Pool.ReenableIfCredits(uid, qb.Balance)
		} else {
			log.Printf("credit uid=%s: 4110: %v", uid, err)
		}
		// 4075 今日额度（展示用，失败不阻塞）
		if _, err := s.cfg.JPRX.GetTodayTokens(ctx, acct); err != nil {
			log.Printf("credit uid=%s: 4075: %v", uid, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// RunKeepaliveNow 立即对全账号调 4058（X-New-Token 由 jprx client 捕获并落盘）。
// 禁用账号跳过。
func (s *Scheduler) RunKeepaliveNow(ctx context.Context, dayKey string) {
	s.mu.Lock()
	s.keepaliveDay = dayKey
	s.mu.Unlock()

	for _, uid := range s.cfg.Pool.UIDs() {
		acct := s.cfg.Pool.AuthByUID(uid)
		if acct == nil {
			continue
		}
		st, ok := s.cfg.Pool.Status(uid)
		if ok && st.Disabled {
			continue
		}
		if err := s.cfg.JPRX.RefreshChannelToken(ctx, acct); err != nil {
			log.Printf("keepalive uid=%s: 4058: %v", uid, err)
		}
	}
}
