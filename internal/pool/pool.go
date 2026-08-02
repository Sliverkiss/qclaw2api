// Package pool 账号池：内存索引 + 冷却/禁用状态机 + state.json 持久化 + 60rpm 令牌桶。
// 挑选策略：healthy 中 lastPick 最早者（纯 round-robin）。
package pool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"qclaw2api/internal/auth"
)

// CoolKind 冷却类型。
type CoolKind int

const (
	CoolCredit CoolKind = iota // 积分不足 → 冷却至下月1号（keepalive 探测恢复）
	CoolRate                   // 429 / rpm 超限 → 短冷却
	CoolErr                    // 连续错误 / api_key_inactive → 中冷却
)

func (k CoolKind) String() string {
	switch k {
	case CoolCredit:
		return "credit"
	case CoolRate:
		return "soft_rate"
	case CoolErr:
		return "error_threshold"
	}
	return "unknown"
}

// Reason 常量（状态机 reason 字段）。
const (
	ReasonCredit   = "credit: no balance"
	ReasonSoftRate = "soft_rate: rate limited"
	ReasonErr      = "error_threshold: consecutive errors"
	ReasonDisabled = "disabled: session expired, relogin required"
)

// Status 单个账号对外暴露的状态（脱敏）。
type Status struct {
	UID      string    `json:"uid"`
	Nickname string    `json:"nickname,omitempty"`
	Cooling  bool      `json:"cooling"`
	Until    time.Time `json:"until,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Disabled bool      `json:"disabled"`
	ErrCount int       `json:"err_count,omitempty"`
	LastPick time.Time `json:"last_pick,omitempty"`
}

type entry struct {
	a        *auth.Auth
	disabled bool
	reason   string
	coolKind CoolKind // 当前冷却类型
	until    time.Time
	errCount int
	lastPick time.Time
	active   bool // 黏性选号：当前在用账号（healthy 则持续复用）

	// 60rpm 令牌桶（每账号本地保守保护）
	tokens     float64
	lastRefill time.Time
}

func (e *entry) healthy(now time.Time) bool {
	if e.disabled {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	return true
}

// stateFile 持久化格式。
type stateFile struct {
	Accounts map[string]struct {
		Disabled bool      `json:"disabled"`
		Reason   string    `json:"reason,omitempty"`
		CoolKind CoolKind  `json:"cool_kind,omitempty"`
		Until    time.Time `json:"until,omitempty"`
		LastPick time.Time `json:"last_pick,omitempty"`
	} `json:"accounts"`
}

// Config pool 构造参数。
type Config struct {
	// RPM 每账号每分钟请求上限（4075.rpm_limit，默认 60）。
	RPM int
	// ErrThreshold 连续错误触发冷却的阈值。
	ErrThreshold int
	// ErrCooldown 连续错误冷却时长。
	ErrCooldown time.Duration
}

// Pool 账号池。
type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string
	rpm     int
	errThr  int
	errDur  time.Duration
}

// New 构建池；stateFp 非空时尝试加载旧状态。
func New(stateFp string, cfg Config) *Pool {
	if cfg.RPM <= 0 {
		cfg.RPM = 60
	}
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 5
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	p := &Pool{
		byUID:   map[string]*entry{},
		stateFp: stateFp,
		rpm:     cfg.RPM,
		errThr:  cfg.ErrThreshold,
		errDur:  cfg.ErrCooldown,
	}
	if stateFp != "" {
		p.load()
	}
	return p
}

// Add 加入账号；已存在则保留原状态、更新凭证。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if e, ok := p.byUID[a.UserID]; ok {
		e.a = a // 保留 cooling/disabled 状态
		return
	}
	p.byUID[a.UserID] = &entry{
		a:          a,
		lastRefill: now,
		tokens:     float64(p.rpm),
	}
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, a := range auths {
		seen[a.UserID] = true
		if e, ok := p.byUID[a.UserID]; ok {
			e.a = a
		} else {
			now := time.Now()
			p.byUID[a.UserID] = &entry{
				a:          a,
				lastRefill: now,
				tokens:     float64(p.rpm),
			}
		}
	}
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
		}
	}
}

// Pick 返回当前在用 healthy 账号；无在用则选一个 healthy 账号并标记为在用（黏性）。
// 冷却/禁用的账号永不返回。无可用返回 nil。
func (p *Pool) Pick() *auth.Auth {
	return p.PickExcluding(nil)
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
// 黏性策略：若已有 active 账号且 healthy 且未被 tried → 直接返回它（持续复用，不轮换）；
// 否则在剩余 healthy 候选中选一个并标记为 active。已冷却/禁用账号永不返回。
// 持写锁：末尾会更新 active/lastPick。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	// 黏性：当前在用账号 healthy 且未被 tried → 持续复用
	for _, e := range p.byUID {
		if e.active && e.healthy(now) && (tried == nil || !tried[e.a.UserID]) {
			e.lastPick = now
			return e.a
		}
	}

	// 否则在 healthy 候选中选一个（fallback 顺序：原 round-robin 语义）
	cand := make([]*entry, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		cand = append(cand, e)
	}
	if len(cand) == 0 {
		return nil
	}
	var best *entry
	for _, e := range cand {
		if best == nil || e.lastPick.Before(best.lastPick) {
			best = e
		}
	}
	best.active = true
	best.lastPick = now
	return best.a
}

// ReserveToken 为账号尝试消耗一个 rpm 令牌；超限返回 false（调用方应触发 soft_rate）。
func (p *Pool) ReserveToken(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return true // 未知账号不阻塞
	}
	now := time.Now()
	// 令牌桶：按流逝时间补充，容量 rpm
	elapsed := now.Sub(e.lastRefill).Seconds()
	e.tokens += elapsed * float64(p.rpm) / 60.0
	if e.tokens > float64(p.rpm) {
		e.tokens = float64(p.rpm)
	}
	e.lastRefill = now
	if e.tokens >= 1 {
		e.tokens--
		return true
	}
	return false
}

// NextMonthStart 返回 now 所在月份的下月 1 号 00:00（本地时区）。
// time.Date 自动处理跨年（12 月 → 次年 1 月）。
func NextMonthStart(now time.Time) time.Time {
	y, m, _ := now.Date()
	return time.Date(y, m+1, 1, 0, 0, 0, 0, now.Location())
}

// CooldownCredit 积分不足冷却至下月 1 号 00:00，由 keepalive 每日探测恢复。
func (p *Pool) CooldownCredit(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = NextMonthStart(time.Now())
		e.reason = reason
		e.coolKind = CoolCredit
		e.errCount = 0
		e.active = false // 冷却后不再是当前在用账号，下次选号切换到其他 healthy 号
	}
	p.saveLocked()
}

// Cooldown 冷却账号至 now+d。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.reason = reason
		e.coolKind = kind // F7：记录冷却类型，供积分刷新判断是否可解冻
		e.errCount = 0
		e.active = false // 冷却后切换账号
	}
	p.saveLocked()
}

// Disable 永久禁用（session 死亡），需人工重登后手工恢复或文件替换。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
		e.coolKind = 0
		e.until = time.Time{}
		e.active = false // 禁用后不再选号
	}
	p.saveLocked()
}

// ClearCooldown 清除账号冷却（测试/手动恢复用）。
func (p *Pool) ClearCooldown(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Time{}
		e.reason = ""
		e.coolKind = 0
		e.errCount = 0
	}
	p.saveLocked()
}

// CoolingUIDs 返回冷却中且未禁用的账号 uid（scheduler keepalive 探测用）。
func (p *Pool) CoolingUIDs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var uids []string
	for uid, e := range p.byUID {
		if e.disabled {
			continue
		}
		if !e.until.IsZero() && now.Before(e.until) {
			uids = append(uids, uid)
		}
	}
	sort.Strings(uids)
	return uids
}

// Recover 清除账号冷却标记（keepalive 探测通过后恢复；也清 errCount）。
func (p *Pool) Recover(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Time{}
		e.reason = ""
		e.coolKind = 0
		e.errCount = 0
	}
	p.saveLocked()
}

// NoteError 记录一次非余额/非 429 错误；达到 threshold 自动冷却 errDur。
func (p *Pool) NoteError(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount++
		if e.errCount >= p.errThr {
			e.until = time.Now().Add(p.errDur)
			e.reason = ReasonErr
			e.coolKind = CoolErr
			e.errCount = 0
			e.active = false // 连续错误冷却后切换账号
		}
	}
	p.saveLocked()
}

// NoteSuccess 成功请求重置错误计数。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount = 0
	}
}

// Status 查询单账号状态。
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// AuthByUID 返回账号的完整凭证（给调度器/运维接口用）。
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// UIDs 返回全部账号 uid（调度器遍历用）。
func (p *Pool) UIDs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// List 返回所有账号状态（按 UID 排序，稳定输出）。
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}

func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	return Status{
		UID:      uid,
		Nickname: e.a.Nickname,
		Cooling:  !e.until.IsZero() && now.Before(e.until),
		Until:    e.until,
		Reason:   e.reason,
		Disabled: e.disabled,
		ErrCount: e.errCount,
		LastPick: e.lastPick,
	}
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	now := time.Now()
	for uid, s := range sf.Accounts {
		p.byUID[uid] = &entry{
			a:          &auth.Auth{UserID: uid}, // placeholder，Add 时会换成完整凭证
			disabled:   s.Disabled,
			reason:     s.Reason,
			coolKind:   s.CoolKind,
			until:      s.Until,
			lastPick:   s.LastPick,
			lastRefill: now,
			tokens:     float64(p.rpm),
		}
	}
}

// saveLocked 原子写 state.json（tmp+rename 0600）。
func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := stateFile{Accounts: map[string]struct {
		Disabled bool      `json:"disabled"`
		Reason   string    `json:"reason,omitempty"`
		CoolKind CoolKind  `json:"cool_kind,omitempty"`
		Until    time.Time `json:"until,omitempty"`
		LastPick time.Time `json:"last_pick,omitempty"`
	}{}}
	for uid, e := range p.byUID {
		sf.Accounts[uid] = struct {
			Disabled bool      `json:"disabled"`
			Reason   string    `json:"reason,omitempty"`
			CoolKind CoolKind  `json:"cool_kind,omitempty"`
			Until    time.Time `json:"until,omitempty"`
			LastPick time.Time `json:"last_pick,omitempty"`
		}{
			Disabled: e.disabled,
			Reason:   e.reason,
			CoolKind: e.coolKind,
			Until:    e.until,
			LastPick: e.lastPick,
		}
	}
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return
	}
	if err := atomicWriteFile(p.stateFp, raw); err != nil {
		return
	}
}

// atomicWriteFile 原子写文件（tmp+rename）。
func atomicWriteFile(fp string, raw []byte) error {
	if dir := filepath.Dir(fp); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	tmp := fp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, fp)
}
