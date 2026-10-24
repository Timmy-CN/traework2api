package scheduler

import (
	"bytes"
	"log"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"traework2api/internal/auth"
	"traework2api/internal/pool"
	"traework2api/internal/upstream"
)

// fakeServer 包装 httptest.Server，让多个调度器复用同一 upstream 地址。
type fakeServer struct{ *httptest.Server }

func newFakeServer(t *testing.T, f *fakeUpstream) *fakeServer {
	t.Helper()
	srv := f.server()
	t.Cleanup(srv.Close)
	return &fakeServer{srv}
}

// newTestSchedulerAt 同 newTestScheduler，但复用已有 server（同一 fake 状态续跑第二轮）。
func newTestSchedulerAt(f *fakeUpstream, p *pool.Pool, fs *fakeServer) *Scheduler {
	up := &upstream.Client{HTTP: fs.Client(), AgentHost: fs.URL, UgHost: fs.URL, OAuthHost: fs.URL, ClientID: upstream.ClientID}
	return New(Config{Pool: p, Upstream: up, CheckinHour: 9, RefreshHours: []int{3}, RefreshSkew: time.Hour})
}

// runSecondRound 在捕获日志的前提下再跑一轮签到，返回本轮日志。
func runSecondRound(f *fakeUpstream, p *pool.Pool, fs *fakeServer) string {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	newTestSchedulerAt(f, p, fs).RunCheckinNow()
	return buf.String()
}

// runIneligibleCheckin 用指定 claim 响应跑一轮签到，返回 fake、池与日志。
// 每次调用各自起一个 httptest Server，调用方须用返回的 fake 构造后续调度器。
func runIneligibleCheckin(t *testing.T, claimBody string) (*fakeUpstream, *pool.Pool, string, *fakeServer) {
	t.Helper()
	f := &fakeUpstream{resourceRemain: 500, claimBody: claimBody}
	fs := newFakeServer(t, f)

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, CheckinDeviceID: "cd-u1"})

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	s := newTestSchedulerAt(f, p, fs)
	s.RunCheckinNow()
	return f, p, buf.String(), fs
}

// TestRunCheckin9095MarkedIneligibleToday 9095 = 本设备今日已签（done 口径）：
// 当天记为不可领取，后续轮次跳过签到，但状态/额度查询照常。
func TestRunCheckin9095MarkedIneligibleToday(t *testing.T) {
	f, p, logs, fs := runIneligibleCheckin(t, `{"code":9095,"message":"this device has checked in today"}`)

	if !p.CheckinIneligibleToday("u1") {
		t.Error("u1 should be marked ineligible after 9095")
	}
	st, _ := p.Status("u1")
	if st.CheckinIneligibleDay == "" {
		t.Errorf("status should carry ineligible day: %+v", st)
	}
	if !strings.Contains(logs, "9095") {
		t.Errorf("log should mention 9095:\n%s", logs)
	}

	// 后续轮次：跳过签到（status/claim 不再调用），额度查询照常。
	beforeStatus, beforeClaim := f.checkinCalls.Load(), f.claimCalls.Load()
	skipLogs := runSecondRound(f, p, fs)
	if f.checkinCalls.Load() != beforeStatus || f.claimCalls.Load() != beforeClaim {
		t.Errorf("ineligible account must skip checkin, status %d→%d claim %d→%d",
			beforeStatus, f.checkinCalls.Load(), beforeClaim, f.claimCalls.Load())
	}
	if !strings.Contains(skipLogs, "当天不可领取") {
		t.Errorf("skip should be logged:\n%s", skipLogs)
	}
}

// TestRunCheckin9074MarkedIneligibleToday 9074 = 需在 CN app 内签：当天不再问。
func TestRunCheckin9074MarkedIneligibleToday(t *testing.T) {
	f, p, _, fs := runIneligibleCheckin(t, `{"code":9074,"message":"check in app"}`)

	if !p.CheckinIneligibleToday("u1") {
		t.Error("u1 should be marked ineligible after 9074")
	}
	beforeStatus, beforeClaim := f.checkinCalls.Load(), f.claimCalls.Load()
	runSecondRound(f, p, fs)
	if f.checkinCalls.Load() != beforeStatus || f.claimCalls.Load() != beforeClaim {
		t.Errorf("9074 account must skip later rounds, status %d→%d claim %d→%d",
			beforeStatus, f.checkinCalls.Load(), beforeClaim, f.claimCalls.Load())
	}
}

// TestRunCheckinIneligibleRecoversNextDay 次日（日期变化）恢复正常重试：
// 把记录的日期改成昨天，下一轮应重新发起签到。
func TestRunCheckinIneligibleRecoversNextDay(t *testing.T) {
	f, p, _, fs := runIneligibleCheckin(t, `{"code":9074,"message":"check in app"}`)
	if !p.CheckinIneligibleToday("u1") {
		t.Fatal("expected ineligible after 9074")
	}

	// 模拟跨天：把记录的日期回退为昨天。
	if err := p.SetCheckinIneligibleDayForTest("u1", time.Now().AddDate(0, 0, -1).Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}
	if p.CheckinIneligibleToday("u1") {
		t.Fatal("yesterday's mark must not read as ineligible today")
	}

	beforeStatus, beforeClaim := f.checkinCalls.Load(), f.claimCalls.Load()
	runSecondRound(f, p, fs)
	// 次日重试走 status → claim；claim 仍 9074 则不再复核（+1 status，+1 claim），
	// 关键是签到确实被重新发起，而不是被当天门控跳过。
	if f.checkinCalls.Load() != beforeStatus+1 || f.claimCalls.Load() != beforeClaim+1 {
		t.Errorf("next day should retry checkin, status %d→%d (want +1), claim %d→%d (want +1)",
			beforeStatus, f.checkinCalls.Load(), beforeClaim, f.claimCalls.Load())
	}
	// 重试仍 9074：会重新记为「今天」不可领取——这正是预期（门控继续生效到今天结束）。
	if !p.CheckinIneligibleToday("u1") {
		t.Error("repeated 9074 on retry should mark today as ineligible again")
	}
}

// TestRunCheckinNormalSuccessUnaffected 正常成功路径不回归：不标记 ineligible。
func TestRunCheckinNormalSuccessUnaffected(t *testing.T) {
	_, p, logs, _ := runIneligibleCheckin(t, "")
	if p.CheckinIneligibleToday("u1") {
		t.Error("successful checkin must not mark ineligible")
	}
	if !strings.Contains(logs, "checkin u1: ok") {
		t.Errorf("success path should still log ok:\n%s", logs)
	}
}
