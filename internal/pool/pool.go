// Package pool 账号池：内存索引 + 冷却/禁用状态机 + state.json 持久化。
// 挑选策略：healthy 账号中剩余积分最多者（SPEC §4.7）。
package pool

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"traework2api/internal/auth"
)

// CoolKind 冷却类型。
type CoolKind int

const (
	CoolPlan CoolKind = iota // 1005 plan 权益不足 → 12h 长冷却
	CoolSoft                 // 429/404 → 60s 短冷却（404 不累计 errCount）
	CoolErr                  // 连续错误 → 10m 中冷却
)

func (k CoolKind) String() string {
	switch k {
	case CoolPlan:
		return "plan_limit"
	case CoolSoft:
		return "soft_rate"
	case CoolErr:
		return "error_threshold"
	}
	return "unknown"
}

// Status 单个账号对外暴露的状态（脱敏，不含 token）。
type Status struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Credits  int64  `json:"credits"`
	// Unlimited 权益含无限包（credits_limit = -1）：credits 无意义，展示按
	// 「unlimited」，选号权重恒最高。
	Unlimited bool `json:"unlimited,omitempty"`
	// Plan 套餐名（全球站美元计费 display_desc；CN credits 口径为
	// Credits/Free）；UsageUntil 最近到期（毫秒，0=没说）。
	Plan       string    `json:"plan,omitempty"`
	UsageUntil int64     `json:"usage_until,omitempty"`
	Cooling    bool      `json:"cooling"`
	Until      time.Time `json:"until,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Disabled   bool      `json:"disabled"`
	ErrCount   int       `json:"err_count,omitempty"`
	// CheckinIneligibleDay 该账号当天不可签到的本地日期（9095/9074），
	// 过期后自动失效；空表示可签到。
	CheckinIneligibleDay string `json:"checkin_ineligible_day,omitempty"`
}

type entry struct {
	a          *auth.Auth
	credits    int64
	unlimited  bool   // 权益含无限包（credits_limit = -1）
	plan       string // 套餐名（/status 展示）
	usageUntil int64  // 最近到期毫秒（/status 展示）
	// lastFn 该账号上次聊天成功的 function（轮换候选序第二位，
	// 插件 fnOf 口径；目录归属缺失时的账号级兜底）。
	lastFn   string
	disabled bool
	reason   string
	until    time.Time
	errCount int
	// checkinIneligibleDay 当天不可签到的日期（本地），格式 2006-01-02。
	checkinIneligibleDay string
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
		Credits              int64     `json:"credits"`
		Unlimited            bool      `json:"unlimited,omitempty"`
		Disabled             bool      `json:"disabled"`
		Reason               string    `json:"reason,omitempty"`
		Until                time.Time `json:"until,omitempty"`
		CheckinIneligibleDay string    `json:"checkin_ineligible_day,omitempty"`
	} `json:"accounts"`
}

// Pool 账号池。
type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string
}

// New 构建池；stateFp 非空时尝试加载旧状态。
func New(stateFp string) *Pool {
	p := &Pool{byUID: map[string]*entry{}, stateFp: stateFp}
	if stateFp != "" {
		p.load()
	}
	return p
}

// Add 加入账号；已存在则保留原状态、更新凭证。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a // 保留 credits/cooling 状态
		return
	}
	p.byUID[a.UID] = &entry{a: a}
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, a := range auths {
		seen[a.UID] = true
		if e, ok := p.byUID[a.UID]; ok {
			e.a = a
		} else {
			p.byUID[a.UID] = &entry{a: a}
		}
	}
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
		}
	}
}

// Pick 返回 healthy 中积分最高的账号；无可用返回 nil。
func (p *Pool) Pick() *auth.Auth {
	return p.PickExcluding(nil)
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if best == nil || e.credits > best.credits {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	return best.a
}

// SetCredits 更新账号积分。
func (p *Pool) SetCredits(uid string, credits int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = credits
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
		e.errCount = 0
	}
	p.saveLocked()
}

// Disable 永久禁用（session 失效），需人工重登后手工恢复或文件替换。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
	}
	p.saveLocked()
}

// ReenableIfCredits 签到后解冻：仅当 remain > 0 且账号处于冷却（非禁用）时恢复。
func (p *Pool) ReenableIfCredits(uid string, remain int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = remain
		if remain > 0 && !e.disabled {
			e.until = time.Time{}
			e.reason = ""
			e.errCount = 0
		}
	}
	p.saveLocked()
}

// ReenableUnlimited 记无限包账号：credits 权重取最大（Pick 恒优先），
// Unlimited 置位供状态展示，冷却照常解除。
func (p *Pool) ReenableUnlimited(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = math.MaxInt64
		e.unlimited = true
		if !e.disabled {
			e.until = time.Time{}
			e.reason = ""
			e.errCount = 0
		}
	}
	p.saveLocked()
}

// LastFunction 账号上次聊天成功的 function；没有 → 空。
func (p *Pool) LastFunction(uid string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.lastFn
	}
	return ""
}

// SetLastFunction 记账号上次聊天成功的 function。
func (p *Pool) SetLastFunction(uid, fn string) {
	if fn == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.lastFn = fn
	}
	p.saveLocked()
}

// SetUsageMeta 记账号的套餐名与最近到期（/status 展示；插件 usage.read 口径）。
func (p *Pool) SetUsageMeta(uid, plan string, untilMs int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.plan = plan
		e.usageUntil = untilMs
	}
	p.saveLocked()
}

// MarkCheckinIneligible 记录账号当天不可领取（claim 业务码 9095/9074）：
// 当天后续轮次跳过签到，次日自动恢复。
func (p *Pool) MarkCheckinIneligible(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.checkinIneligibleDay = time.Now().Format("2006-01-02")
	}
	p.saveLocked()
}

// CheckinIneligibleToday 账号是否当天不可领取（过期自动失效）。
func (p *Pool) CheckinIneligibleToday(uid string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	return ok && e.checkinIneligibleDay == time.Now().Format("2006-01-02")
}

// SetCheckinIneligibleDayForTest 直接改写不可领取日期（测试跨天场景用）。
func (p *Pool) SetCheckinIneligibleDayForTest(uid, day string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return fmt.Errorf("pool: unknown uid %q", uid)
	}
	e.checkinIneligibleDay = day
	p.saveLocked()
	return nil
}

// NoteError 记录一次错误；达到 threshold 自动冷却 d 时长。
func (p *Pool) NoteError(uid string, threshold int, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount++
		if e.errCount >= threshold {
			e.until = time.Now().Add(d)
			e.reason = "consecutive errors"
			e.errCount = 0
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
		UID:                  uid,
		Nickname:             e.a.Nickname,
		Credits:              e.credits,
		Unlimited:            e.unlimited,
		Plan:                 e.plan,
		UsageUntil:           e.usageUntil,
		Cooling:              !e.until.IsZero() && now.Before(e.until),
		Until:                e.until,
		Reason:               e.reason,
		Disabled:             e.disabled,
		ErrCount:             e.errCount,
		CheckinIneligibleDay: e.checkinIneligibleDay,
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
	for uid, s := range sf.Accounts {
		p.byUID[uid] = &entry{
			a:                    &auth.Auth{UID: uid}, // placeholder，Add 时会换成完整凭证
			credits:              s.Credits,
			unlimited:            s.Unlimited,
			disabled:             s.Disabled,
			reason:               s.Reason,
			until:                s.Until,
			checkinIneligibleDay: s.CheckinIneligibleDay,
		}
	}
}

func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := stateFile{Accounts: map[string]struct {
		Credits              int64     `json:"credits"`
		Unlimited            bool      `json:"unlimited,omitempty"`
		Disabled             bool      `json:"disabled"`
		Reason               string    `json:"reason,omitempty"`
		Until                time.Time `json:"until,omitempty"`
		CheckinIneligibleDay string    `json:"checkin_ineligible_day,omitempty"`
	}{}}
	for uid, e := range p.byUID {
		sf.Accounts[uid] = struct {
			Credits              int64     `json:"credits"`
			Unlimited            bool      `json:"unlimited,omitempty"`
			Disabled             bool      `json:"disabled"`
			Reason               string    `json:"reason,omitempty"`
			Until                time.Time `json:"until,omitempty"`
			CheckinIneligibleDay string    `json:"checkin_ineligible_day,omitempty"`
		}{
			Credits:              e.credits,
			Unlimited:            e.unlimited,
			Disabled:             e.disabled,
			Reason:               e.reason,
			Until:                e.until,
			CheckinIneligibleDay: e.checkinIneligibleDay,
		}
	}
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(p.stateFp); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.stateFp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p.stateFp)
}
