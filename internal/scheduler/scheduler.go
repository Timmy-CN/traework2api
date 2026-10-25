// Package scheduler 定时任务：每日签到 + token 预刷新。
// 签到成功后重新查积分，积分 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"log"
	"time"

	"traework2api/internal/auth"
	"traework2api/internal/pool"
	"traework2api/internal/upstream"
)

// Config 调度器依赖。
type Config struct {
	Pool         *pool.Pool
	Upstream     *upstream.Client
	CheckinHour  int           // 每日签到小时，默认 9
	RefreshHours []int         // token 预刷新小时，默认 [3]
	RefreshSkew  time.Duration // 预刷新窗口，默认 24h
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config
}

// New 构建。
func New(cfg Config) *Scheduler {
	if cfg.CheckinHour < 0 {
		cfg.CheckinHour = 9
	}
	if len(cfg.RefreshHours) == 0 {
		cfg.RefreshHours = []int{3}
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 24 * time.Hour
	}
	return &Scheduler{cfg: cfg}
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// Run 主循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	all := append(append([]int{}, s.cfg.RefreshHours...), s.cfg.CheckinHour)
	for {
		next := nextFire(time.Now(), all)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			h := time.Now().Hour()
			if contains(s.cfg.RefreshHours, h) {
				s.RunRefreshNow()
			}
			if s.cfg.CheckinHour == h {
				s.RunCheckinNow()
			}
		}
	}
}

func contains(hours []int, h int) bool {
	for _, v := range hours {
		if v == h {
			return true
		}
	}
	return false
}

// RunCheckinNow 立即对所有账号执行签到 + 积分刷新 + 解冻。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
func (s *Scheduler) RunCheckinNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		if a := s.cfg.Pool.AuthByUID(st.UID); a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		s.checkinAccount(st)
	}
}

// CheckinOne 触发单账号签到（admin API 用）；账号不存在返回 false。
func (s *Scheduler) CheckinOne(uid string) bool {
	for _, st := range s.cfg.Pool.List() {
		if st.UID != uid {
			continue
		}
		if a := s.cfg.Pool.AuthByUID(st.UID); a == nil || a.RefreshTokenValue() == "" {
			return false
		}
		s.checkinAccount(st)
		return true
	}
	return false
}

// checkinAccount 单账号签到闭环（RunCheckinNow / CheckinOne 共用）：
// 设备兜底 → 签到前兜底刷新 → DailyCheckin → ent-usage + 解冻。
func (s *Scheduler) checkinAccount(st pool.Status) {
	a := s.cfg.Pool.AuthByUID(st.UID)
	if a == nil || a.RefreshTokenValue() == "" {
		return
	}
	// 签到门控：9095（本设备今日已签，done 口径）/ 9074（需在 CN app 内签）
	// 当天不可领取，当天后续轮次跳过签到，但状态/额度查询照常，次日自动恢复。
	if s.cfg.Pool.CheckinIneligibleToday(st.UID) {
		log.Printf("checkin %s: skip, 当天不可领取（9095/9074），次日恢复", st.UID)
	} else if a.CheckinDeviceID == "" {
		// 签到设备门控（PR #9）：凭证缺独立签到设备标识时，尝试从本机已登录的
		// 官方客户端自动读取并写回凭证（Docker 容器内读不到宿主机客户端数据，
		// 会走到下面的跳过分支）。
		if id, brand, typ, ok := auth.DetectCheckinDeviceForUID(st.UID); ok {
			a.CheckinDeviceID, a.CheckinDeviceBrand, a.CheckinDeviceType = id, brand, typ
			if err := a.SaveAtomic(); err != nil {
				log.Printf("checkin %s: 自动读取签到设备ID成功但写回失败: %v", st.UID, err)
			} else {
				log.Printf("checkin %s: 已从本机官方客户端自动读取签到设备ID并写回凭证", st.UID)
			}
		}
	}
	if a.CheckinDeviceID == "" {
		// 插件口径（#194/#361）：本机客户端没有以该账号登录时，签到走
		// 账号自己的派生设备 id（登录设备 + uid 派生，16 位，稳定不轮换，
		// 绝不是别的账号的设备），而不是跳过签到。派生结果不落盘——
		// 之后 profile 匹配到真客户端设备时仍以真设备为准。
		if derived := auth.UgDevice(a.DeviceID, st.UID); derived != "" {
			a.CheckinDeviceID = derived
			log.Printf("checkin %s: 本机客户端未以该账号登录，使用派生签到设备ID签到", st.UID)
		}
	}
	if a.CheckinDeviceID == "" {
		// 凭证缺少独立签到设备标识且本机检测不到官方客户端：多账号签到
		// 要求各账号使用不同的设备 ID，缺字段时明确跳过并说明，不发无设备头的
		// 无效请求，也不生成随机假 ID（上游无法识别，签到永远不会生效）。
		log.Printf("checkin %s: skip, 缺签到设备ID且本机未检测到官方客户端（在已登录官方客户端的机器上跑 signin.sh 可自动读取并写回凭证；手动获取见 README「签到设备ID」）", st.UID)
	} else if !s.cfg.Pool.CheckinIneligibleToday(st.UID) {
		// 签到前兜底刷新：凌晨预刷新失败时，避免拿着过期 token 签到。
		if a.NeedsRefresh(2 * time.Hour) {
			if err := s.cfg.Upstream.RefreshToken(a); err != nil {
				log.Printf("checkin refresh %s: %v", st.UID, err)
			} else if err := a.SaveAtomic(); err != nil {
				log.Printf("checkin refresh %s save: %v", st.UID, err)
			}
		}
		// 签到闭环：status → claim → status 复核，以复核结果为最终结论。
		// 已签到不算失败；claim 返回 200 但复核未签上会报错（HTTP 200 业务失败不再漏判）。
		switch err := s.cfg.Upstream.DailyCheckin(a); {
		case err == nil:
			log.Printf("checkin %s: ok", st.UID)
		case errors.Is(err, upstream.ErrCheckinUnverified):
			// claim 已发出但读回未能确认（复核未签上/读回失败）：当天
			// 不可再领取，避免重复领取（插件 0.2.22 口径）。
			s.cfg.Pool.MarkCheckinIneligible(st.UID)
			log.Printf("checkin verify uid=%s: claim 已发出但读回未确认，当天不可再领取: %v", st.UID, err)
		case errors.Is(err, upstream.ErrAlreadyCheckedIn):
			log.Printf("checkin %s: already checked in", st.UID)
		case errors.Is(err, upstream.ErrCheckinDisabled):
			log.Printf("checkin %s: disabled", st.UID)
		default:
			var ce *upstream.CheckinError
			if errors.As(err, &ce) && ce.Code == upstream.CodeCheckinIneligibleDevice {
				// 本设备今日已签：done 口径，记为当天不可领取。
				s.cfg.Pool.MarkCheckinIneligible(st.UID)
				log.Printf("checkin %s: already checked in on this device (9095)", st.UID)
			} else if errors.As(err, &ce) && ce.Code == upstream.CodeCheckinInAppOnly {
				// 需在 CN app 内签：当天不再问。
				s.cfg.Pool.MarkCheckinIneligible(st.UID)
				log.Printf("checkin %s: ineligible, 需在 Trae CN app 内签到 (9074)", st.UID)
			} else {
				log.Printf("checkin %s: %v", st.UID, err)
			}
		}
	}
	// 查积分 + 解冻（无限包账号按无限口径：credits 无意义，恒解冻）；
	// 顺带记套餐名与最近到期（/status 展示，插件 usage.read 口径）。
	remain, unlimited, err := s.cfg.Upstream.UserEntUsage(a)
	if err != nil {
		log.Printf("ent-usage %s: %v", st.UID, err)
		return
	}
	if site := s.cfg.Upstream.SiteMeta(a); site.DollarUsage {
		if d, derr := s.cfg.Upstream.DollarEntUsage(a); derr == nil {
			plan := d.Plan
			if plan == "" {
				plan = "Free"
			}
			s.cfg.Pool.SetUsageMeta(st.UID, plan, d.EndMs)
		}
	} else if until, uerr := s.cfg.Upstream.CreditsUntil(a); uerr == nil {
		s.cfg.Pool.SetUsageMeta(st.UID, "Credits", until)
	}
	if unlimited {
		log.Printf("ent-usage %s: unlimited", st.UID)
		s.cfg.Pool.ReenableUnlimited(st.UID)
	} else {
		log.Printf("ent-usage %s: remain=%d", st.UID, remain)
		s.cfg.Pool.ReenableIfCredits(st.UID, remain)
	}
}

// RunRefreshNow 立即对所有账号刷新 token；session 失效的自动禁用。
func (s *Scheduler) RunRefreshNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		if a := s.cfg.Pool.AuthByUID(st.UID); a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		s.refreshAccount(st, false)
	}
}

// RefreshOne 触发单账号刷新（admin API 用，force 忽略预刷新窗口）；
// 账号不存在返回 false。
func (s *Scheduler) RefreshOne(uid string) bool {
	for _, st := range s.cfg.Pool.List() {
		if st.UID != uid {
			continue
		}
		if a := s.cfg.Pool.AuthByUID(st.UID); a == nil || a.RefreshTokenValue() == "" {
			return false
		}
		return s.refreshAccount(st, true)
	}
	return false
}

// refreshAccount 单账号刷新；session 失效自动禁用。返回是否真正刷新。
func (s *Scheduler) refreshAccount(st pool.Status, force bool) bool {
	a := s.cfg.Pool.AuthByUID(st.UID)
	if a == nil || a.RefreshTokenValue() == "" {
		return false
	}
	if !force && !a.NeedsRefresh(s.cfg.RefreshSkew) {
		return false
	}
	if err := s.cfg.Upstream.RefreshToken(a); err != nil {
		log.Printf("refresh %s: %v", st.UID, err)
		var ue *upstream.Error
		if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
			s.cfg.Pool.Disable(st.UID, "session dead")
		}
		return false
	}
	if err := a.SaveAtomic(); err != nil {
		log.Printf("refresh %s save: %v", st.UID, err)
		return false
	}
	log.Printf("refresh %s: ok", st.UID)
	return true
}
