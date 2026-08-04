package pool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"qclaw2api/internal/auth"
)

func mkAuth(uid string) *auth.Auth {
	return &auth.Auth{
		JWTToken:  "jwt-" + uid,
		SKAPIKey:  "sk-test-" + uid,
		UserID:    uid,
		Nickname:  "用户" + uid,
		GUID:      "qclawmp_" + uid,
		ExpiresAt: 1783000000,
	}
}

func testPool(t *testing.T) *Pool {
	t.Helper()
	return New(filepath.Join(t.TempDir(), "state.json"), Config{RPM: 60, ErrThreshold: 5, ErrCooldown: 10 * time.Minute})
}

// addAcct 加账号（Add 只负责凭证；无积分设置）。
func addAcct(p *Pool, uid string) {
	p.Add(mkAuth(uid))
}

// TestPickSticky 校验黏性选号：全部 healthy 时持续复用同一账号（不轮换）。
func TestPickSticky(t *testing.T) {
	p := testPool(t)
	addAcct(p, "1")
	addAcct(p, "2")
	addAcct(p, "3")
	first := p.Pick()
	if first == nil {
		t.Fatal("nil first pick")
	}
	// 后续 8 次应始终返回同一账号（黏性，不轮换）
	for i := 0; i < 8; i++ {
		a := p.Pick()
		if a == nil {
			t.Fatalf("nil pick on iteration %d", i)
		}
		if a.UserID != first.UserID {
			t.Fatalf("sticky violated: picked %s want %s (iteration %d)", a.UserID, first.UserID, i)
		}
	}
}

// TestPickSwitchesAfterCooldown 校验：当前在用账号冷却后，切换到其他 healthy 号；不选冷却号。
func TestPickSwitchesAfterCooldown(t *testing.T) {
	p := testPool(t)
	addAcct(p, "1")
	addAcct(p, "2")
	// 第一次选到某号（黏性锁定）
	first := p.Pick()
	if first == nil {
		t.Fatal("nil first pick")
	}
	other := "1"
	if first.UserID == "1" {
		other = "2"
	}
	// 冷却当前在用账号 → 应切换到另一个 healthy 号
	p.CooldownCredit(first.UserID, "积分不足")
	next := p.Pick()
	if next == nil {
		t.Fatal("expected switch to healthy account")
	}
	if next.UserID == first.UserID {
		t.Fatalf("picked cooled account %s", next.UserID)
	}
	if next.UserID != other {
		t.Fatalf("picked %s want %s", next.UserID, other)
	}
	// 两个都冷却 → nil
	p.CooldownCredit(other, "积分不足")
	if got := p.Pick(); got != nil {
		t.Fatalf("expected nil when all cooled, got %v", got.UserID)
	}
}

// TestPickSkipsCooled 校验已冷却账号永不返回（即使曾 active）。
func TestPickSkipsCooled(t *testing.T) {
	p := testPool(t)
	addAcct(p, "1")
	addAcct(p, "2")
	// 冷却 2，激活 1
	p.Cooldown("2", CoolRate, time.Hour, ReasonSoftRate)
	got := p.Pick()
	if got == nil || got.UserID != "1" {
		t.Fatalf("Pick = %v, want uid 1 (skip cooled 2)", got)
	}
}

// TestPickExcluding 校验跳过 tried。
func TestPickExcluding(t *testing.T) {
	p := testPool(t)
	addAcct(p, "1")
	addAcct(p, "2")
	got := p.PickExcluding(map[string]bool{"2": true})
	if got == nil || got.UserID != "1" {
		t.Fatalf("PickExcluding = %v, want uid 1", got)
	}
	// 全部 tried → nil
	got = p.PickExcluding(map[string]bool{"1": true, "2": true})
	if got != nil {
		t.Fatalf("expected nil when all tried, got %v", got.UserID)
	}
}

// TestCooldown 校验冷却后不可选、到期后恢复。
func TestCooldown(t *testing.T) {
	p := testPool(t)
	addAcct(p, "1")
	p.Cooldown("1", CoolRate, time.Hour, ReasonSoftRate)
	if got := p.Pick(); got != nil {
		t.Fatalf("expected nil during cooldown, got %v", got.UserID)
	}
	// 到期后恢复（手动把 until 设到过去）
	p.mu.Lock()
	p.byUID["1"].until = time.Now().Add(-time.Second)
	p.mu.Unlock()
	if got := p.Pick(); got == nil || got.UserID != "1" {
		t.Fatalf("expected pick after cooldown expiry")
	}
}

// TestDisable 校验永久禁用不可选。
func TestDisable(t *testing.T) {
	p := testPool(t)
	addAcct(p, "1")
	p.Disable("1", ReasonDisabled)
	if got := p.Pick(); got != nil {
		t.Fatalf("expected nil when disabled")
	}
}

// TestCooldownCredit 校验积分冷却 until = 次日 0 点（R3），且状态类型为 CoolCredit。
func TestCooldownCredit(t *testing.T) {
	p := testPool(t)
	addAcct(p, "1")
	p.CooldownCredit("1", "积分不足")
	st, ok := p.Status("1")
	if !ok || !st.Cooling {
		t.Fatalf("expected cooling after CooldownCredit, got %+v", st)
	}
	if st.Reason != "积分不足" {
		t.Errorf("reason = %q", st.Reason)
	}
	want := NextMidnight(time.Now())
	if !st.Until.Equal(want) {
		t.Errorf("until = %v, want NextMidnight=%v", st.Until, want)
	}
	// 冷却中不可选
	if got := p.Pick(); got != nil {
		t.Fatalf("credit-cooled account must not be pickable: %v", got)
	}
	p.mu.RLock()
	kind := p.byUID["1"].coolKind
	p.mu.RUnlock()
	if kind != CoolCredit {
		t.Errorf("coolKind = %v, want CoolCredit(%d)", kind, CoolCredit)
	}
}

// TestNextMidnight 校验次日 0 点（跨月/跨年边界，R3）。
func TestNextMidnight(t *testing.T) {
	cases := []struct {
		in   time.Time
		want time.Time
	}{
		{time.Date(2026, 8, 4, 23, 59, 59, 0, time.Local), time.Date(2026, 8, 5, 0, 0, 0, 0, time.Local)},
		{time.Date(2026, 8, 31, 15, 30, 0, 0, time.Local), time.Date(2026, 9, 1, 0, 0, 0, 0, time.Local)},
		{time.Date(2026, 12, 31, 23, 0, 0, 0, time.Local), time.Date(2027, 1, 1, 0, 0, 0, 0, time.Local)},
		{time.Date(2026, 2, 28, 10, 0, 0, 0, time.Local), time.Date(2026, 3, 1, 0, 0, 0, 0, time.Local)},
	}
	for _, c := range cases {
		if got := NextMidnight(c.in); !got.Equal(c.want) {
			t.Errorf("NextMidnight(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestNextMonthStart 校验跨年/跨月。
func TestNextMonthStart(t *testing.T) {
	cases := []struct {
		in   time.Time
		want time.Time
	}{
		{time.Date(2026, 8, 2, 15, 30, 0, 0, time.Local), time.Date(2026, 9, 1, 0, 0, 0, 0, time.Local)},
		{time.Date(2026, 12, 31, 23, 59, 59, 0, time.Local), time.Date(2027, 1, 1, 0, 0, 0, 0, time.Local)},
		{time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local), time.Date(2026, 2, 1, 0, 0, 0, 0, time.Local)},
	}
	for _, c := range cases {
		if got := NextMonthStart(c.in); !got.Equal(c.want) {
			t.Errorf("NextMonthStart(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestRecover 校验 Recover 清除冷却标记（keepalive 探测通过后恢复）。
func TestRecover(t *testing.T) {
	p := testPool(t)
	addAcct(p, "1")
	p.CooldownCredit("1", "积分不足")
	if got := p.Pick(); got != nil {
		t.Fatal("expected cooled")
	}
	p.Recover("1")
	st, _ := p.Status("1")
	if st.Cooling || st.Reason != "" {
		t.Errorf("after Recover status = %+v, want clean", st)
	}
	if got := p.Pick(); got == nil || got.UserID != "1" {
		t.Fatalf("expected pick after Recover, got %v", got)
	}
}

// TestRecoverRestoresActive 校验 Recover 恢复 active=true（P0-2）：
// keepalive 探测通过后恢复的账号应重新成为在用（黏性），连续 Pick 返回该账号；
// 若 Recover 未恢复 active，恢复的账号会跌入 fallback 被其他 healthy 号抢占（横跳）。
func TestRecoverRestoresActive(t *testing.T) {
	p := testPool(t)
	addAcct(p, "1")
	addAcct(p, "2")
	// 锁定 uid1 为在用账号（排除 uid2，冷却期间不选号 → uid2 不会成为 active）
	if got := p.PickExcluding(map[string]bool{"2": true}); got == nil || got.UserID != "1" {
		t.Fatalf("PickExcluding = %v, want uid1", got)
	}
	p.CooldownCredit("1", "积分不足")
	p.Recover("1") // keepalive 探测通过，立即恢复
	// 恢复后 active 恢复 → 连续 Pick 持续返回 uid1（黏性不横跳）
	for i := 0; i < 5; i++ {
		a := p.Pick()
		if a == nil {
			t.Fatalf("nil pick on iteration %d", i)
		}
		if a.UserID != "1" {
			t.Fatalf("recovered account not sticky (P0-2): picked %s want 1 (iteration %d)",
				a.UserID, i)
		}
	}
}

// TestCoolingUIDs 校验只返回积分冷却（CoolCredit）未禁用账号。
// 短冷却（CoolRate/CoolErr）时间自愈，keepalive 不探测（P0-3）。
func TestCoolingUIDs(t *testing.T) {
	p := testPool(t)
	addAcct(p, "1")
	addAcct(p, "2")
	addAcct(p, "3")
	p.CooldownCredit("1", "积分不足")
	p.Cooldown("2", CoolRate, time.Minute, ReasonSoftRate)
	p.Disable("3", ReasonDisabled)
	got := p.CoolingUIDs()
	if len(got) != 1 || got[0] != "1" {
		t.Fatalf("CoolingUIDs = %v, want [1] (only credit cooldown, disabled excluded)", got)
	}
}

// TestNoteErrorThreshold 校验连续错误达阈值自动冷却。
func TestNoteErrorThreshold(t *testing.T) {
	p := testPool(t)
	addAcct(p, "1")
	for i := 0; i < 5; i++ {
		p.NoteError("1")
	}
	st, _ := p.Status("1")
	if !st.Cooling {
		t.Errorf("expected cooling after 5 errors, got status %+v", st)
	}
	// 成功重置
	p.NoteSuccess("1")
	st, _ = p.Status("1")
	if st.ErrCount != 0 {
		t.Errorf("ErrCount = %d, want 0", st.ErrCount)
	}
}

// TestRPMBucket 校验令牌桶限流。
func TestRPMBucket(t *testing.T) {
	p := testPool(t)
	addAcct(p, "1")
	// 初始满桶（rpm=60），前 60 次放行
	for i := 0; i < 60; i++ {
		if !p.ReserveToken("1") {
			t.Fatalf("iteration %d should be allowed", i)
		}
	}
	// 第 61 次立即拒绝
	if p.ReserveToken("1") {
		t.Fatalf("expected rate limit exceeded")
	}
	// 未知账号不阻塞
	if !p.ReserveToken("ghost") {
		t.Fatalf("unknown uid should not be blocked")
	}
}

// TestStatePersistence 校验 state.json 持久化与加载。
func TestStatePersistence(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp, Config{RPM: 60, ErrThreshold: 5, ErrCooldown: 10 * time.Minute})
	addAcct(p, "1")
	addAcct(p, "2")
	p.Disable("1", ReasonDisabled)
	p.Cooldown("2", CoolRate, time.Minute, ReasonSoftRate)

	// 新池加载同一 state 文件
	p2 := New(fp, Config{RPM: 60, ErrThreshold: 5, ErrCooldown: 10 * time.Minute})
	p2.Add(mkAuth("1"))
	p2.Add(mkAuth("2"))
	st1, ok := p2.Status("1")
	if !ok || !st1.Disabled {
		t.Errorf("uid1 disabled not persisted: %+v", st1)
	}
	st2, ok := p2.Status("2")
	if !ok || !st2.Cooling || st2.Reason != ReasonSoftRate {
		t.Errorf("uid2 cooling not persisted: %+v", st2)
	}
}

// TestStateFileNoTempResidue 校验持久化后无 .tmp 残留。
func TestStateFileNoTempResidue(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp, Config{RPM: 60, ErrThreshold: 5, ErrCooldown: 10 * time.Minute})
	addAcct(p, "1")
	p.CooldownCredit("1", "积分不足")
	if _, err := os.Stat(fp + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp should not remain")
	}
}

// TestOldStateCompat 校验旧 state.json（含 credits 字段）可加载且 credits 被忽略、
// 旧 cool_kind=0（CoolHard）映射为 CoolCredit 语义兼容。
func TestOldStateCompat(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	old := `{"accounts":{"1":{"credits":100,"disabled":false,"reason":"hard_credit: no balance","cool_kind":0,"until":"2026-09-01T00:00:00+08:00","last_pick":"0001-01-01T00:00:00Z"}}}`
	if err := os.WriteFile(fp, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp, Config{RPM: 60, ErrThreshold: 5, ErrCooldown: 10 * time.Minute})
	p.Add(mkAuth("1"))
	st, ok := p.Status("1")
	if !ok {
		t.Fatal("account 1 not loaded")
	}
	if !st.Cooling {
		t.Errorf("old cool_kind=0 (CoolHard) should map to cooling: %+v", st)
	}
	p.mu.RLock()
	kind := p.byUID["1"].coolKind
	p.mu.RUnlock()
	if kind != CoolCredit {
		t.Errorf("old cool_kind=0 mapped to %v, want CoolCredit(0)", kind)
	}
}

// TestSyncToDir 校验目录扫描对齐：新增/剔除账号，保留既有状态。
func TestSyncToDir(t *testing.T) {
	p := testPool(t)
	addAcct(p, "1")
	addAcct(p, "2")
	p.Cooldown("1", CoolErr, 10*time.Minute, ReasonErr)
	// 新扫描只剩 1 和 3 → 剔除 2、加入 3
	p.SyncToDir([]*auth.Auth{mkAuth("1"), mkAuth("3")})
	if _, ok := p.Status("2"); ok {
		t.Errorf("uid2 should be removed by SyncToDir")
	}
	// 已存在账号状态保留
	st1, _ := p.Status("1")
	if !st1.Cooling || st1.Reason != ReasonErr {
		t.Errorf("uid1 cooling not preserved after SyncToDir: %+v", st1)
	}
	// 新账号可被选中
	if got := p.Pick(); got == nil || got.UserID != "3" {
		t.Fatalf("Pick = %v, want 3", got)
	}
}

// TestListSorted 校验 List 按 UID 稳定排序。
func TestListSorted(t *testing.T) {
	p := testPool(t)
	addAcct(p, "3")
	addAcct(p, "1")
	addAcct(p, "2")
	got := p.List()
	for i := 1; i < len(got); i++ {
		if got[i-1].UID >= got[i].UID {
			t.Errorf("List not sorted at %d: %v", i, got)
		}
	}
}

// TestStatus 校验脱敏状态字段（无 Credits）。
func TestStatus(t *testing.T) {
	p := testPool(t)
	addAcct(p, "42")
	p.Cooldown("42", CoolRate, time.Minute, ReasonSoftRate)
	st, ok := p.Status("42")
	if !ok {
		t.Fatal("status not found")
	}
	if !st.Cooling || st.Reason != ReasonSoftRate {
		t.Errorf("status = %+v", st)
	}
	// 状态 JSON 可序列化（/status 用）
	if _, err := json.Marshal(st); err != nil {
		t.Errorf("marshal status: %v", err)
	}
}

// TestAuthByUID 校验凭证查询。
func TestAuthByUID(t *testing.T) {
	p := testPool(t)
	addAcct(p, "1")
	a := p.AuthByUID("1")
	if a == nil || a.SKAPIKey != "sk-test-1" {
		t.Fatalf("AuthByUID = %v", a)
	}
	if p.AuthByUID("ghost") != nil {
		t.Fatalf("expected nil for unknown uid")
	}
}

// TestPickConcurrent 校验并发 Pick 无数据竞争（F3：PickExcluding 持写锁）。
// N=8 goroutine 各 Pick 100 次，-race 下不得报告 lastPick 竞争。
func TestPickConcurrent(t *testing.T) {
	p := testPool(t)
	for i := 0; i < 8; i++ {
		addAcct(p, fmt.Sprintf("u%d", i))
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				// 排除一个 uid，避免每次必然同号；nil 在无 healthy 时合法
				_ = p.PickExcluding(map[string]bool{"u0": true})
			}
		}()
	}
	wg.Wait()
}
