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

func mkAuth(uid string, credits float64) *auth.Auth {
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

// addAcct 加账号并设置余额（Add 只负责凭证，余额由 SetCredits 设置）。
func addAcct(p *Pool, uid string, credits float64) {
	p.Add(mkAuth(uid, credits))
	p.SetCredits(uid, credits)
}

// TestPickHighestCredit 校验 Q 点最高者优先。
func TestPickHighestCredit(t *testing.T) {
	p := testPool(t)
	addAcct(p, "1", 100)
	addAcct(p, "2", 300)
	addAcct(p, "3", 200)
	got := p.Pick()
	if got == nil || got.UserID != "2" {
		t.Fatalf("Pick = %v, want uid 2", got)
	}
}

// TestPickRotateAmongClose 校验余额相近(<5%)按 lastPick 轮转。
func TestPickRotateAmongClose(t *testing.T) {
	p := testPool(t)
	addAcct(p, "1", 1000)
	addAcct(p, "2", 990) // 差 1% < 5%
	first := p.Pick()
	if first == nil {
		t.Fatal("nil pick")
	}
	// 多次挑选都在 {1,2} 之间轮转，且绝不选到其他账号
	seen := map[string]int{}
	for i := 0; i < 10; i++ {
		a := p.Pick()
		if a == nil {
			t.Fatalf("nil pick on iteration %d", i)
		}
		seen[a.UserID]++
		if a.UserID != "1" && a.UserID != "2" {
			t.Fatalf("picked unexpected uid %q", a.UserID)
		}
	}
	// 两个账号都应被轮转到
	if len(seen) != 2 {
		t.Errorf("rotation only saw %v", seen)
	}
}

// TestPickExcluding 校验跳过 tried。
func TestPickExcluding(t *testing.T) {
	p := testPool(t)
	addAcct(p, "1", 100)
	addAcct(p, "2", 300)
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
	addAcct(p, "1", 100)
	p.Cooldown("1", CoolHard, time.Hour, ReasonHardCredit)
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

// TestDisable 校验永久禁用不可选、解冻不可恢复。
func TestDisable(t *testing.T) {
	p := testPool(t)
	addAcct(p, "1", 100)
	p.Disable("1", ReasonDisabled)
	if got := p.Pick(); got != nil {
		t.Fatalf("expected nil when disabled")
	}
	p.ReenableIfCredits("1", 500) // 禁用账号不应被解冻
	if got := p.Pick(); got != nil {
		t.Fatalf("disabled account must not be re-enabled by credits")
	}
}

// TestSetCreditsAndReenable 校验 hard_credit 冷却 + 积分恢复自动解冻。
func TestSetCreditsAndReenable(t *testing.T) {
	p := testPool(t)
	addAcct(p, "1", 100)
	p.Cooldown("1", CoolHard, 12*time.Hour, ReasonHardCredit)
	if got := p.Pick(); got != nil {
		t.Fatalf("expected nil during hard cooldown")
	}
	p.ReenableIfCredits("1", 200)
	if got := p.Pick(); got == nil || got.UserID != "1" {
		t.Fatalf("expected re-enabled after credits, got %v", got)
	}
	// 余额 0 不解冻
	addAcct(p, "2", 50)
	p.Cooldown("2", CoolHard, 12*time.Hour, ReasonHardCredit)
	p.ReenableIfCredits("2", 0)
	if got := p.PickExcluding(map[string]bool{"1": true}); got != nil {
		t.Fatalf("zero credits should stay cooled")
	}
}

// TestReenableSoftNotCleared 校验 soft_rate 冷却不受积分刷新影响（F7/P1-6）。
// 429 冷却中的账号，即使 balance>0 也应保持冷却，直到 soft 时长自然到期。
func TestReenableSoftNotCleared(t *testing.T) {
	p := testPool(t)
	addAcct(p, "1", 100)
	p.Cooldown("1", CoolSoft, time.Minute, ReasonSoftRate)
	p.ReenableIfCredits("1", 500)
	st, _ := p.Status("1")
	if !st.Cooling {
		t.Errorf("soft cooldown should survive credit refresh: %+v", st)
	}
	if st.Reason != ReasonSoftRate {
		t.Errorf("reason = %q, want %q", st.Reason, ReasonSoftRate)
	}
	// 余额照常更新
	if st.Credits != 500 {
		t.Errorf("credits = %v, want 500", st.Credits)
	}
	// 冷却未到期 → 不可选
	if got := p.Pick(); got != nil {
		t.Fatalf("account in soft cooldown should not be pickable: %v", got)
	}
}

// TestReenableErrNotCleared 校验 error_threshold 冷却同样不受积分刷新影响（F7）。
func TestReenableErrNotCleared(t *testing.T) {
	p := testPool(t)
	addAcct(p, "1", 100)
	p.Cooldown("1", CoolErr, 10*time.Minute, ReasonErr)
	p.ReenableIfCredits("1", 300)
	st, _ := p.Status("1")
	if !st.Cooling {
		t.Errorf("err cooldown should survive credit refresh: %+v", st)
	}
	if st.Reason != ReasonErr {
		t.Errorf("reason = %q, want %q", st.Reason, ReasonErr)
	}
}

// TestNoteErrorThreshold 校验连续错误达阈值自动冷却。
func TestNoteErrorThreshold(t *testing.T) {
	p := testPool(t)
	addAcct(p, "1", 100)
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
	addAcct(p, "1", 100)
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
	addAcct(p, "1", 100)
	addAcct(p, "2", 300)
	p.SetCredits("2", 250)
	p.Disable("1", ReasonDisabled)

	// 新池加载同一 state 文件
	p2 := New(fp, Config{RPM: 60, ErrThreshold: 5, ErrCooldown: 10 * time.Minute})
	// 加载的 placeholder 通过 Add 更新凭证
	p2.Add(mkAuth("1", 100))
	p2.Add(mkAuth("2", 300))
	st1, ok := p2.Status("1")
	if !ok || !st1.Disabled {
		t.Errorf("uid1 disabled not persisted: %+v", st1)
	}
	st2, ok := p2.Status("2")
	if !ok || st2.Credits != 250 {
		t.Errorf("uid2 credits not persisted: %+v", st2)
	}
}

// TestSyncToDir 校验目录扫描对齐：新增/剔除账号，保留既有状态。
func TestSyncToDir(t *testing.T) {
	p := testPool(t)
	addAcct(p, "1", 100)
	addAcct(p, "2", 200)
	// 新扫描只剩 1 和 3 → 剔除 2、加入 3
	p.SyncToDir([]*auth.Auth{mkAuth("1", 50), mkAuth("3", 300)})
	if _, ok := p.Status("2"); ok {
		t.Errorf("uid2 should be removed by SyncToDir")
	}
	// 已存在账号状态保留（SyncToDir 不重置 credits）
	st1, _ := p.Status("1")
	if st1.Credits != 100 {
		t.Errorf("uid1 credits = %v, want 100 (preserved)", st1.Credits)
	}
	// 新账号初始 0 余额；模拟 scheduler 4110 刷新后选中
	p.SetCredits("3", 300)
	if got := p.Pick(); got == nil || got.UserID != "3" {
		t.Fatalf("Pick = %v, want 3", got)
	}
}

// TestListSorted 校验 List 按 UID 稳定排序。
func TestListSorted(t *testing.T) {
	p := testPool(t)
	addAcct(p, "3", 100)
	addAcct(p, "1", 100)
	addAcct(p, "2", 100)
	got := p.List()
	for i := 1; i < len(got); i++ {
		if got[i-1].UID >= got[i].UID {
			t.Errorf("List not sorted at %d: %v", i, got)
		}
	}
}

// TestStatus 校验脱敏状态字段。
func TestStatus(t *testing.T) {
	p := testPool(t)
	addAcct(p, "42", 777)
	p.Cooldown("42", CoolSoft, time.Minute, ReasonSoftRate)
	st, ok := p.Status("42")
	if !ok {
		t.Fatal("status not found")
	}
	if st.Credits != 777 || !st.Cooling || st.Reason != ReasonSoftRate {
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
	addAcct(p, "1", 100)
	a := p.AuthByUID("1")
	if a == nil || a.SKAPIKey != "sk-test-1" {
		t.Fatalf("AuthByUID = %v", a)
	}
	if p.AuthByUID("ghost") != nil {
		t.Fatalf("expected nil for unknown uid")
	}
}

// TestStateFileNoTempResidue 校验持久化后无 .tmp 残留。
func TestStateFileNoTempResidue(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp, Config{RPM: 60, ErrThreshold: 5, ErrCooldown: 10 * time.Minute})
	addAcct(p, "1", 100)
	p.SetCredits("1", 150)
	if _, err := os.Stat(fp + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp should not remain")
	}
}

// TestPickConcurrent 校验并发 Pick 无数据竞争（F3：PickExcluding 持写锁）。
// N=8 goroutine 各 Pick 100 次，-race 下不得报告 lastPick 竞争。
func TestPickConcurrent(t *testing.T) {
	p := testPool(t)
	for i := 0; i < 8; i++ {
		addAcct(p, fmt.Sprintf("u%d", i), float64(100+i))
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
