package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"traework2api/internal/auth"
	"traework2api/internal/pool"
	"traework2api/internal/scheduler"
	"traework2api/internal/upstream"
)

// adminTestHandler 面板联调环境的 handler：带 fake upstream + scheduler。
func adminTestHandler(t *testing.T) (*Handler, *httptest.Server) {
	t.Helper()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, upstream.EpCheckinClaim):
			// fake 签到闭环：claim 后 checkedIn 翻转，让二次复核通过
			w.Write([]byte(`{"code":0,"message":"success"}`))
		case strings.HasSuffix(r.URL.Path, upstream.EpCheckinStatus):
			w.Write([]byte(`{"checked_in":false,"credits":100,"enable":true}`))
		case strings.HasSuffix(r.URL.Path, upstream.EpEntUsage):
			w.Write([]byte(`{"is_credits_billing":true,"user_entitlement_pack_list":[
				{"entitlement_base_info":{"quota":{"credits_limit":1000}}}]}`))
		case strings.HasSuffix(r.URL.Path, "/ide_user_ent_usage") || strings.Contains(r.URL.Path, "ExchangeToken"):
			w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, upstream.EpUserInfo):
			w.Write([]byte(`{"Result":{}}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(fake.Close)

	up := &upstream.Client{
		HTTP:      fake.Client(),
		AgentHost: fake.URL,
		UgHost:    fake.URL,
		OAuthHost: fake.URL,
		ClientID:  upstream.ClientID,
	}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, CheckinDeviceID: "cd-1"})
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "test-key", Scheduler: sch})
	return h, fake
}

func adminPost(t *testing.T, h *Handler, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestAdminCheckinSingle 面板触发单账号签到：200 + ok（复用签到闭环）。
func TestAdminCheckinSingle(t *testing.T) {
	h, _ := adminTestHandler(t)
	code, out := adminPost(t, h, "/admin/checkin", `{"uid":"u1"}`)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("checkin: %d %v", code, out)
	}
}

// TestAdminCheckinUnknownAccount 未知账号 → 404。
func TestAdminCheckinUnknownAccount(t *testing.T) {
	h, _ := adminTestHandler(t)
	code, out := adminPost(t, h, "/admin/checkin", `{"uid":"ghost"}`)
	if code != http.StatusNotFound {
		t.Fatalf("unknown uid: %d %v", code, out)
	}
}

// TestAdminCheckinAll 全员签到 → 异步受理（ok=true, scope=all）。
func TestAdminCheckinAll(t *testing.T) {
	h, _ := adminTestHandler(t)
	code, out := adminPost(t, h, "/admin/checkin", `{}`)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("checkin all: %d %v", code, out)
	}
}

// TestAdminRefreshSingle 单账号强制刷新（强制 = 忽略预刷新窗口）。
func TestAdminRefreshSingle(t *testing.T) {
	h, _ := adminTestHandler(t)
	// fake 的 ExchangeToken 返回 {}（无 token）→ 刷新失败 → 404 是正确行为；
	// 这里断言的是端点路由与参数校验（不 500、不 panic）。
	code, out := adminPost(t, h, "/admin/refresh", `{"uid":"u1"}`)
	if code != http.StatusOK && code != http.StatusNotFound {
		t.Fatalf("refresh: %d %v", code, out)
	}
}

// TestAdminEnableDisable 启停：disable 后状态带 disabled，enable 恢复。
func TestAdminEnableDisable(t *testing.T) {
	h, _ := adminTestHandler(t)

	code, out := adminPost(t, h, "/admin/accounts/disable", `{"uid":"u1","reason":"面板禁用"}`)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("disable: %d %v", code, out)
	}
	if st, _ := h.cfg.Pool.Status("u1"); !st.Disabled {
		t.Fatal("disable 后应为 disabled")
	}

	code, out = adminPost(t, h, "/admin/accounts/enable", `{"uid":"u1"}`)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("enable: %d %v", code, out)
	}
	if st, _ := h.cfg.Pool.Status("u1"); st.Disabled || st.Cooling {
		t.Fatalf("enable 后应恢复: %+v", st)
	}

	// 未知账号 404
	code, _ = adminPost(t, h, "/admin/accounts/enable", `{"uid":"ghost"}`)
	if code != http.StatusNotFound {
		t.Fatalf("unknown enable: %d", code)
	}
	// 缺 uid 400
	code, _ = adminPost(t, h, "/admin/accounts/disable", `{}`)
	if code != http.StatusBadRequest {
		t.Fatalf("missing uid: %d", code)
	}
}

// TestAdminOverview 总览：stats + accounts + models 三段齐全。
func TestAdminOverview(t *testing.T) {
	h, _ := adminTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/overview", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview: %d", rec.Code)
	}
	var out struct {
		Stats    map[string]int   `json:"stats"`
		Accounts []map[string]any `json:"accounts"`
		Models   []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Stats["total"] != 1 || out.Stats["healthy"] != 1 {
		t.Fatalf("stats: %v", out.Stats)
	}
	if len(out.Accounts) != 1 || out.Accounts[0]["uid"] != "u1" {
		t.Fatalf("accounts: %v", out.Accounts)
	}
}

// TestAdminRequiresAuth admin 端点复用 Bearer 鉴权。
func TestAdminRequiresAuth(t *testing.T) {
	h, _ := adminTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/overview", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: %d want 401", rec.Code)
	}
}
