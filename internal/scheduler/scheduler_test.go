package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"qclaw2api/internal/auth"
	"qclaw2api/internal/pool"
)

func mkAuth(uid string) *auth.Auth {
	return &auth.Auth{
		JWTToken:     "jwt-" + uid,
		ChannelToken: "ct-" + uid,
		SKAPIKey:     "sk-test-" + uid,
		UserID:       uid,
		Nickname:     "用户" + uid,
		GUID:         "qclawmp_" + uid,
		ExpiresAt:    time.Now().Add(30 * 24 * time.Hour).Unix(),
	}
}

// probeRecorder 记录每次探测的 uid 与结果，可注入错误。
type probeRecorder struct {
	calls   map[string]int
	errByID map[string]error
}

func newProbeRecorder() *probeRecorder {
	return &probeRecorder{calls: map[string]int{}, errByID: map[string]error{}}
}

func (r *probeRecorder) probe(a *auth.Auth) error {
	if a == nil {
		return errors.New("nil auth")
	}
	r.calls[a.UserID]++
	return r.errByID[a.UserID]
}

// newTestScheduler 组装 scheduler + 注入 probe recorder。
func newTestScheduler(t *testing.T, rec *probeRecorder) (*Scheduler, *pool.Pool) {
	t.Helper()
	p := pool.New("", pool.Config{RPM: 60, ErrThreshold: 5, ErrCooldown: 10 * time.Minute})
	p.Add(mkAuth("1"))
	p.Add(mkAuth("2"))
	s := New(Config{
		Pool:           p,
		KeepaliveHours: []int{4},
		Probe:          rec.probe,
	})
	return s, p
}

// TestRunKeepaliveProbeRecovers 校验探测成功 → Recover 解除冷却。
func TestRunKeepaliveProbeRecovers(t *testing.T) {
	rec := newProbeRecorder()
	s, p := newTestScheduler(t, rec)

	// uid1 积分冷却；uid2 正常（不探测）
	p.CooldownCredit("1", "积分不足")
	if got := p.PickExcluding(map[string]bool{"2": true}); got != nil {
		t.Fatal("expected uid1 cooled")
	}

	s.RunKeepaliveNow(context.Background(), "2026-08-02")

	if rec.calls["1"] != 1 {
		t.Errorf("probe uid1 calls = %d, want 1", rec.calls["1"])
	}
	if rec.calls["2"] != 0 {
		t.Errorf("probe uid2 calls = %d, want 0 (healthy accounts not probed)", rec.calls["2"])
	}
	st, _ := p.Status("1")
	if st.Cooling || st.Reason != "" {
		t.Errorf("uid1 should be recovered after probe OK: %+v", st)
	}
	if got := p.PickExcluding(map[string]bool{"2": true}); got == nil || got.UserID != "1" {
		t.Fatalf("expected uid1 pickable after recover, got %v", got)
	}
}

// TestRunKeepaliveProbeFail 校验探测失败 → 保持冷却不滚动（R3：冷却无到期）。
// 账号保留 cooling 状态（不 Recover），下次 keepalive 继续探测。
func TestRunKeepaliveProbeFail(t *testing.T) {
	rec := newProbeRecorder()
	rec.errByID["1"] = errors.New("upstream 402 credit")
	s, p := newTestScheduler(t, rec)

	p.CooldownCredit("1", "积分不足")

	s.RunKeepaliveNow(context.Background(), "2026-08-02")

	if rec.calls["1"] != 1 {
		t.Errorf("probe uid1 calls = %d, want 1", rec.calls["1"])
	}
	st, _ := p.Status("1")
	if !st.Cooling {
		t.Errorf("uid1 should stay cooling after probe fail: %+v", st)
	}
	// R3：失败只 log，不重新 CooldownCredit——until 保持零值（无到期滚动）。
	if !st.Until.IsZero() {
		t.Errorf("until = %v, want zero (R3 失败不滚动)", st.Until)
	}
	if got := p.PickExcluding(map[string]bool{"2": true}); got != nil {
		t.Fatalf("uid1 must not be pickable while cooling: %v", got)
	}
}

// TestRunKeepaliveDayDedup 校验当日 keepalive 不重复执行（同一 dayKey）。
func TestRunKeepaliveDayDedup(t *testing.T) {
	rec := newProbeRecorder()
	s, p := newTestScheduler(t, rec)

	p.CooldownCredit("1", "积分不足")

	dayKey := "2026-08-02"
	s.RunKeepaliveNow(context.Background(), dayKey)
	s.RunKeepaliveNow(context.Background(), dayKey)

	if rec.calls["1"] != 1 {
		t.Errorf("probe uid1 calls = %d, want 1 (day dedup)", rec.calls["1"])
	}
}

// TestTickKeepaliveAtHour 校验 keepalive 小时触发探测，非 keepalive 小时不触发。
func TestTickKeepaliveAtHour(t *testing.T) {
	rec := newProbeRecorder()
	s, p := newTestScheduler(t, rec)
	p.CooldownCredit("1", "积分不足")

	// 非 keepalive 小时：不触发
	s.setNow(func() time.Time { return time.Date(2026, 8, 2, 12, 0, 0, 0, time.Local) })
	s.tick(context.Background())
	if rec.calls["1"] != 0 {
		t.Errorf("calls at hour 12 = %d, want 0", rec.calls["1"])
	}

	// keepalive 小时 4：触发
	s.setNow(func() time.Time { return time.Date(2026, 8, 2, 4, 0, 0, 0, time.Local) })
	s.tick(context.Background())
	if rec.calls["1"] != 1 {
		t.Errorf("calls at hour 4 = %d, want 1", rec.calls["1"])
	}
}

// TestTickKeepaliveDayDedup 校验 tick 当日去重：第二次同一日 tick 不再探测。
func TestTickKeepaliveDayDedup(t *testing.T) {
	rec := newProbeRecorder()
	s, p := newTestScheduler(t, rec)
	p.CooldownCredit("1", "积分不足")

	s.setNow(func() time.Time { return time.Date(2026, 8, 2, 4, 0, 0, 0, time.Local) })
	s.tick(context.Background())
	if rec.calls["1"] != 1 {
		t.Fatalf("first tick calls = %d, want 1", rec.calls["1"])
	}
	// 同一天再跑一次 tick（重置冷却以便可再次探测到 uid1，模拟下一轮）
	p.Recover("1")
	p.CooldownCredit("1", "积分不足")
	s.tick(context.Background())
	if rec.calls["1"] != 1 {
		t.Errorf("second tick calls = %d, want 1 (day dedup)", rec.calls["1"])
	}
}
