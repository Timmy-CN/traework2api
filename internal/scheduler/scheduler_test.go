package scheduler

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"traework2api/internal/auth"
	"traework2api/internal/pool"
	"traework2api/internal/upstream"
)

func TestNextFire(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, loc)
	next := nextFire(now, []int{9, 21})
	if next.Hour() != 21 || next.Day() != 27 {
		t.Errorf("next=%v want 21:00 same day", next)
	}
	now = time.Date(2026, 7, 27, 22, 0, 0, 0, loc)
	next = nextFire(now, []int{9, 21})
	if next.Hour() != 9 || next.Day() != 28 {
		t.Errorf("next=%v want 09:00 next day", next)
	}
	now = time.Date(2026, 7, 27, 9, 0, 0, 0, loc)
	next = nextFire(now, []int{9})
	if next.Day() != 28 {
		t.Errorf("exact match should roll to next day: %v", next)
	}
}

func TestNextFireMergesSchedules(t *testing.T) {
	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.Local)
	next := nextFire(now, []int{9, 21, 22})
	if next.Hour() != 21 {
		t.Errorf("next=%v want 21 (earliest of 21/22)", next)
	}
}

// fakeUpstream 同时模拟 checkin/ent_usage/refresh。
type fakeUpstream struct {
	checkinCalls   atomic.Int32
	claimCalls     atomic.Int32
	refreshCalls   atomic.Int32
	resourceRemain int64
	// checkedIn 模拟服务端签到状态：claim 成功后翻转 true，
	// 让 DailyCheckin 的二次复核有真实语义（而不是恒定未签导致误报失败）。
	checkedIn atomic.Bool
	// claimBody 覆盖 claim 响应体（默认成功），用于构造业务失败场景。
	claimBody string
}

func (f *fakeUpstream) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/checkin_credits/status"):
			f.checkinCalls.Add(1)
			checked := "false"
			if f.checkedIn.Load() {
				checked = "true"
			}
			w.Write([]byte(`{"checked_in":` + checked + `,"credits":200,"enable":true}`))
		case strings.HasSuffix(r.URL.Path, "/checkin_credits/claim"):
			f.claimCalls.Add(1)
			if f.claimBody != "" {
				w.Write([]byte(f.claimBody))
				return
			}
			f.checkedIn.Store(true)
			w.Write([]byte(`{"code":0,"message":"success"}`))
		case strings.HasSuffix(r.URL.Path, "/ide_user_ent_usage"):
			w.Write([]byte(`{"is_credits_billing":true,"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":` +
				jsonI64(f.resourceRemain) + `}}}]}`))
		case strings.HasSuffix(r.URL.Path, "/ExchangeToken"):
			f.refreshCalls.Add(1)
			w.Write([]byte(`{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786805537,"TokenExpireDuration":1209600}}`))
		default:
			http.Error(w, "not found: "+r.URL.Path, 404)
		}
	}))
}

func jsonI64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func newTestScheduler(f *fakeUpstream, p *pool.Pool, srv *httptest.Server) *Scheduler {
	up := &upstream.Client{
		HTTP:      srv.Client(),
		AgentHost: srv.URL,
		UgHost:    srv.URL,
		OAuthHost: srv.URL,
		ClientID:  upstream.ClientID,
	}
	return New(Config{
		Pool:         p,
		Upstream:     up,
		CheckinHour:  9,
		RefreshHours: []int{3},
		RefreshSkew:  time.Hour,
	})
}

func TestRunCheckinReenablesCoolingAccount(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, CheckinDeviceID: "cd-u1"}
	p.Add(a)
	p.Cooldown("u1", pool.CoolPlan, time.Hour, "plan limit")

	s := newTestScheduler(f, p, srv)
	s.RunCheckinNow()
	// DailyCheckin：签到前 status + claim 后复核 status，共 2 次
	if f.checkinCalls.Load() != 2 {
		t.Errorf("checkin status calls=%d", f.checkinCalls.Load())
	}
	if f.claimCalls.Load() != 1 {
		t.Errorf("claim calls=%d", f.claimCalls.Load())
	}
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Errorf("account should be reenabled after checkin with credits: %+v", st)
	}
	if st.Credits != 500 {
		t.Errorf("credits=%d want 500", st.Credits)
	}
}

func TestRunCheckinDoesNotLogSuccessWhenStatusDoesNotChange(t *testing.T) {
	// claimBody 设为成功响应但不翻转服务端状态：模拟「claim 接受但实际没签上」，
	// DailyCheckin 的复核必须拦截，调度器不得输出 checkin ok。
	f := &fakeUpstream{resourceRemain: 500, claimBody: `{"code":0,"message":"success"}`}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, CheckinDeviceID: "cd-u1"})

	var logs strings.Builder
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)

	s := newTestScheduler(f, p, srv)
	s.RunCheckinNow()
	if strings.Contains(logs.String(), "checkin u1: ok") {
		t.Fatalf("false success log: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "checkin verify uid=u1") {
		t.Fatalf("missing verification failure: %s", logs.String())
	}
}

// 凭证缺 checkinDeviceId 且无法从本机官方客户端读取时：按插件 #194/#361
// 口径，签到改用账号自身的派生设备 id（登录设备+uid 稳定派生的 16 位，
// 不落盘、绝不取别的账号的设备），只有派生不出（登录设备与 uid 都为空）
// 的账号才跳过并打印明确提示。
func TestRunCheckinDerivedDeviceWithoutClientProfile(t *testing.T) {
	t.Setenv("TW2A_DISABLE_CHECKIN_DETECT", "1") // 隔离本机官方客户端数据，保持用例 hermetic
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}) // 无 UID：派生不出设备

	var logs strings.Builder
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)

	s := newTestScheduler(f, p, srv)
	s.RunCheckinNow()
	if f.claimCalls.Load() != 1 {
		t.Errorf("u1 应以派生设备签到一次, claims=%d", f.claimCalls.Load())
	}
	if !strings.Contains(logs.String(), "派生签到设备ID") {
		t.Fatalf("missing derived-device hint: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "缺签到设备ID") {
		t.Fatalf("empty-uid account should still skip: %s", logs.String())
	}
}

func TestRunCheckinSkipsDisabled(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Disable("u1", "session dead")

	s := newTestScheduler(f, p, srv)
	s.RunCheckinNow()
	if f.checkinCalls.Load() != 0 {
		t.Errorf("disabled account should be skipped, calls=%d", f.checkinCalls.Load())
	}
}

func TestRunRefreshRefreshesTokens(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1, ApiHost: srv.URL}
	p.Add(a)

	s := newTestScheduler(f, p, srv)
	s.RunRefreshNow()
	if f.refreshCalls.Load() != 1 {
		t.Errorf("refresh calls=%d", f.refreshCalls.Load())
	}
	if a.AccessToken != "newat" {
		t.Errorf("token not updated: %s", a.AccessToken)
	}
}

func TestRunRefreshSkipsFreshToken(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "fresh", RefreshToken: "rt", ExpiresAt: 9999999999, ApiHost: srv.URL})

	s := newTestScheduler(f, p, srv)
	s.RunRefreshNow()
	if f.refreshCalls.Load() != 0 {
		t.Errorf("fresh token should not refresh, calls=%d", f.refreshCalls.Load())
	}
}

func TestRunRefreshSessionDeadDisables(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"code":20101,"msg":"login required"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1, ApiHost: srv.URL})

	up := &upstream.Client{HTTP: srv.Client(), AgentHost: srv.URL, UgHost: srv.URL, OAuthHost: srv.URL, ClientID: upstream.ClientID}
	s := New(Config{Pool: p, Upstream: up})
	s.RunRefreshNow()
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("should disable session-dead account: %+v", st)
	}
}

// TestRunCheckinBusinessFailureNotOk 回归：claim 返回 HTTP 200 但业务失败时，
// 不得再输出 "checkin ok"（曾因丢弃响应体把实际未签到误报为成功）。
func TestRunCheckinBusinessFailureNotOk(t *testing.T) {
	f := &fakeUpstream{claimBody: `{"code":1005,"message":"活动已结束"}`}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	s := newTestScheduler(f, p, srv)
	s.RunCheckinNow()

	logs := buf.String()
	if strings.Contains(logs, "checkin u1: ok") {
		t.Errorf("business failure must not be logged as ok:\n%s", logs)
	}
	if !strings.Contains(logs, "code=1005") {
		t.Errorf("business failure should be logged with upstream code:\n%s", logs)
	}
}

// TestRunCheckinUnlimitedEntUsage 无限包（credits_limit = -1）账号：
// ent-usage 记 unlimited 并恒解冻（冷却解除、状态带 Unlimited），
// 不再出现「无限账号被算成 remain=0」的旧口径。
func TestRunCheckinUnlimitedEntUsage(t *testing.T) {
	f := &fakeUpstream{resourceRemain: -1}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, CheckinDeviceID: "cd-u1"})
	p.Cooldown("u1", pool.CoolPlan, time.Hour, "plan limit")

	var logs strings.Builder
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)

	s := newTestScheduler(f, p, srv)
	s.RunCheckinNow()

	if !strings.Contains(logs.String(), "ent-usage u1: unlimited") {
		t.Fatalf("missing unlimited log: %s", logs.String())
	}
	st, _ := p.Status("u1")
	if !st.Unlimited || st.Cooling {
		t.Fatalf("unlimited 账号应解冻且带 Unlimited: %+v", st)
	}
}
