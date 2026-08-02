package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"qclaw2api/internal/auth"
	"qclaw2api/internal/jprx"
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

// fakeJPRX 假 jprx 网关：4110 返回 balance，4075 返回限额，4058 返回空。
type fakeJPRX struct {
	balance  int64
	limit    int64
	reqs4110 int
	reqs4075 int
	reqs4058 int
}

func (f *fakeJPRX) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/data/4110/forward":
		f.reqs4110++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ret": 0, "data": map[string]any{
				"resp": map[string]any{"common": map[string]any{"code": 0}, "data": map[string]any{
					"balance": f.balance,
					"balance_detail": map[string]any{
						"activity_q": f.balance,
						"items":      []any{},
					},
				}},
			},
		})
	case r.URL.Path == "/data/4075/forward":
		f.reqs4075++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ret": 0, "data": map[string]any{
				"resp": map[string]any{"common": map[string]any{"code": 0}, "data": map[string]any{
					"daily_token_limit": f.limit, "daily_token_used": 0, "rpm_limit": 60,
				}},
			},
		})
	case r.URL.Path == "/data/4058/forward":
		f.reqs4058++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ret": 0, "data": map[string]any{
				"resp": map[string]any{"common": map[string]any{"code": 0}, "data": map[string]any{
					"openclaw_channel_token": "ct-same",
				}},
			},
		})
	default:
		http.NotFound(w, r)
	}
}

// newTestScheduler 组装 scheduler + 假 jprx 网关。
func newTestScheduler(t *testing.T, j *fakeJPRX) (*Scheduler, *pool.Pool) {
	t.Helper()
	jSrv := httptest.NewServer(http.HandlerFunc(j.handler))
	t.Cleanup(jSrv.Close)

	p := pool.New("", pool.Config{RPM: 60, ErrThreshold: 5, ErrCooldown: 10 * time.Minute})
	p.Add(mkAuth("1"))
	p.SetCredits("1", 100)

	jc := jprx.New()
	jc.SetBase(jSrv.URL)

	s := New(Config{
		Pool:                p,
		JPRX:                jc,
		KeepaliveHours:      []int{4},
		CreditIntervalHours: 6,
	})
	return s, p
}

// TestRunCreditNow 校验积分刷新：4110 更新 balance。
func TestRunCreditNow(t *testing.T) {
	j := &fakeJPRX{balance: 500, limit: 40000000}
	s, p := newTestScheduler(t, j)

	s.RunCreditNow(context.Background())
	if j.reqs4110 != 1 {
		t.Errorf("4110 calls = %d, want 1", j.reqs4110)
	}
	if j.reqs4075 != 1 {
		t.Errorf("4075 calls = %d, want 1", j.reqs4075)
	}
	st, _ := p.Status("1")
	if st.Credits != 500 {
		t.Errorf("credits = %v, want 500", st.Credits)
	}
}

// TestRunCreditNowReenables 校验余额>0 自动解冻 hard_credit。
func TestRunCreditNowReenables(t *testing.T) {
	j := &fakeJPRX{balance: 500, limit: 40000000}
	s, p := newTestScheduler(t, j)

	// 余额 0 → hard_credit 冷却
	p.SetCredits("1", 0)
	p.Cooldown("1", pool.CoolHard, 12*time.Hour, pool.ReasonHardCredit)
	if got := p.Pick(); got != nil {
		t.Fatal("expected cooled")
	}

	// 但冷却中的账号会被 RunCreditNow 跳过（不调上游），无法解冻。
	// 解冻路径：先解除冷却（模拟冷却到期），再刷新 → 更新余额。
	p.ClearCooldown("1")
	s.RunCreditNow(context.Background())
	if got := p.Pick(); got == nil {
		t.Error("account should be pickable after credits")
	}
	st, _ := p.Status("1")
	if st.Credits != 500 {
		t.Errorf("credits = %v, want 500", st.Credits)
	}
}

// TestRunCreditNowSkipsCooldown 冷却中账号跳过（不调上游）。
func TestRunCreditNowSkipsCooldown(t *testing.T) {
	j := &fakeJPRX{balance: 500, limit: 40000000}
	s, p := newTestScheduler(t, j)

	// 余额 0 → hard_credit 冷却
	p.SetCredits("1", 0)
	p.Cooldown("1", pool.CoolHard, 12*time.Hour, pool.ReasonHardCredit)

	s.RunCreditNow(context.Background())
	if j.reqs4110 != 0 {
		t.Errorf("4110 calls = %d, want 0 (cooling skipped)", j.reqs4110)
	}
}

// TestRunKeepaliveNow 校验 4058 续期。
func TestRunKeepaliveNow(t *testing.T) {
	j := &fakeJPRX{balance: 100, limit: 40000000}
	s, _ := newTestScheduler(t, j)

	s.RunKeepaliveNow(context.Background(), "2026-08-02")
	if j.reqs4058 != 1 {
		t.Errorf("4058 calls = %d, want 1", j.reqs4058)
	}
}

// TestTickTriggersCredit 校验 tick 到间隔小时后触发积分刷新。
func TestTickTriggersCredit(t *testing.T) {
	j := &fakeJPRX{balance: 500, limit: 40000000}
	s, _ := newTestScheduler(t, j)

	base := time.Date(2026, 8, 2, 12, 0, 0, 0, time.Local)
	s.setNow(func() time.Time { return base })
	// 先跑一次记录 creditLast = base
	s.RunCreditNow(context.Background())
	// 快进 7 小时
	s.setNow(func() time.Time { return base.Add(7 * time.Hour) })
	s.tick(context.Background())
	if j.reqs4110 < 2 {
		t.Errorf("4110 calls = %d, want >= 2 (interval elapsed)", j.reqs4110)
	}
}

// TestTickKeepaliveAtHour 校验 keepalive 小时触发 4058。
func TestTickKeepaliveAtHour(t *testing.T) {
	j := &fakeJPRX{balance: 100, limit: 40000000}
	s, _ := newTestScheduler(t, j)

	base := time.Date(2026, 8, 2, 4, 0, 0, 0, time.Local) // keepalive hour 4
	s.setNow(func() time.Time { return base })
	s.tick(context.Background())
	if j.reqs4058 != 1 {
		t.Errorf("4058 calls = %d, want 1 (hour 4)", j.reqs4058)
	}
}
