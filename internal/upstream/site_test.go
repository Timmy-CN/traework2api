package upstream

import (
	"strings"
	"testing"

	"traework2api/internal/auth"
)

// TestSiteOf 判站：region US 前缀 → 全球站；apiHost 落 trae.ai → 全球站；
// CN/空 → CN。
func TestSiteOf(t *testing.T) {
	if got := SiteOf("US-East", ""); got.ID != "trae-global" {
		t.Errorf("US region should be global, got %s", got.ID)
	}
	if got := SiteOf("", "https://coreva-normal.trae.ai"); got.ID != "trae-global" {
		t.Errorf("coreva host should be global, got %s", got.ID)
	}
	if got := SiteOf("", "https://api.trae.com.cn"); got.ID != "trae-cn" {
		t.Errorf("CN host should be cn, got %s", got.ID)
	}
	if got := SiteOf("", ""); got.ID != "trae-cn" {
		t.Errorf("empty should default cn, got %s", got.ID)
	}
	if got := SiteOf("SG", "https://coresg-normal.trae.ai"); got.ID != "trae-global" {
		t.Errorf("SG global host should be global, got %s", got.ID)
	}
	if got := SiteOf("SG", ""); got.ID != "trae-cn" {
		t.Errorf("non-US region without host defaults cn, got %s", got.ID)
	}
}

// TestHostOf 插件 traeHost 白名单：https、无端口/凭据/query/fragment，
// 三域之外拒收。
func TestHostOf(t *testing.T) {
	ok := []string{
		"https://api.trae.com.cn",
		"https://growsg-normal.trae.ai",
		"https://coreva-normal.trae.ai/foo",
		"https://mchost.guru",
		"https://trae-api-x.mchost.guru",
	}
	for _, h := range ok {
		if HostOf(h) == "" {
			t.Errorf("HostOf(%q) should pass", h)
		}
	}
	bad := []string{
		"http://api.trae.com.cn",
		"https://evil.example.com",
		"https://api.trae.com.cn:8080",
		"https://user:pass@api.trae.com.cn",
		"https://api.trae.com.cn/?x=1",
		"",
	}
	for _, h := range bad {
		if HostOf(h) != "" {
			t.Errorf("HostOf(%q) should reject", h)
		}
	}
}

// TestSiteHostSelection 账号级 host 选择：全球站 US 账号走 US 网关/pay host，
// SG 账号走 SG；CN 账号恒默认。
func TestSiteHostSelection(t *testing.T) {
	c := New()
	us := &auth.Auth{UID: "u1", Region: "US"}
	if got := c.agentBaseFor(us); got != "https://coreva-normal.trae.ai" {
		t.Errorf("US agent host = %s", got)
	}
	if got := c.ugBaseFor(us); got != "https://api-us-east.trae.ai" {
		t.Errorf("US pay host = %s", got)
	}
	sg := &auth.Auth{UID: "u2", ApiHost: "https://coresg-normal.trae.ai"}
	if got := c.agentBaseFor(sg); got != "https://coresg-normal.trae.ai" {
		t.Errorf("SG agent host = %s", got)
	}
	if got := c.ugBaseFor(sg); got != "https://api-sg-central.trae.ai" {
		t.Errorf("SG pay host = %s", got)
	}
	cn := &auth.Auth{UID: "u3"}
	if got := c.agentBaseFor(cn); got != AgentHost {
		t.Errorf("CN agent host = %s", got)
	}
	if got := c.ugBaseFor(cn); got != UgHost {
		t.Errorf("CN ug host = %s", got)
	}
}

// TestGlobalSiteNoDeviceExchangeAndNoCheckin 全球站：无 deviceExchange（续期
// 直接普通轮换）、无签到页（CheckinStatus/CheckinClaim/DailyCheckin 报
// ErrCheckinDisabled）。
func TestGlobalSiteNoDeviceExchangeAndNoCheckin(t *testing.T) {
	if len(Sites["trae-global"].DeviceExchange) != 0 {
		t.Error("global site must not carry deviceExchange")
	}
	if Sites["trae-global"].Checkin {
		t.Error("global site must not carry checkin")
	}
	c := New()
	a := &auth.Auth{UID: "u1", Region: "US", AccessToken: "at", RefreshToken: "rt"}
	if _, _, _, err := c.CheckinStatus(a); err != ErrCheckinDisabled {
		t.Errorf("global CheckinStatus err = %v want ErrCheckinDisabled", err)
	}
	if err := c.CheckinClaim(a); err != ErrCheckinDisabled {
		t.Errorf("global CheckinClaim err = %v want ErrCheckinDisabled", err)
	}
	if err := c.DailyCheckin(a); err != ErrCheckinDisabled {
		t.Errorf("global DailyCheckin err = %v want ErrCheckinDisabled", err)
	}
}

// TestDollarEntUsage 全球站美元计费解析：首个带 quota 的包，折算积分
// （(total−used)×100）。
func TestDollarEntUsage(t *testing.T) {
	d, err := parseDollarUsage([]byte(`{"user_entitlement_pack_list":[
		{"display_desc":"Pro","entitlement_base_info":{"end_time":"2026-11-24T00:00:00Z","quota":{"basic_usage_limit":20,"bonus_usage_limit":5}},"usage":{"basic_usage_amount":3.5,"bonus_usage_amount":0.5}},
		{"entitlement_base_info":{"quota":{}}}
	]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if d.Plan != "Pro" || d.Basic != 20 || d.Bonus != 5 || d.Used != 3.5 || d.UsedBonus != 0.5 {
		t.Errorf("dollar usage = %+v", d)
	}
	if d.EndMs == 0 {
		t.Error("end_time should parse")
	}
	// remain 折算：(20+5 − 4) × 100 = 2100
	total, used := d.Total(), d.UsedTotal()
	remain := int64((total - used) * 100)
	if remain != 2100 {
		t.Errorf("remain = %d want 2100", remain)
	}
	if _, err := parseDollarUsage([]byte(`{"user_entitlement_pack_list":[]}`)); err == nil {
		t.Error("empty list should error")
	}
}

// TestWhenOfAny 时间归一（插件 whenOf 口径）。
func TestWhenOfAny(t *testing.T) {
	if got := whenOfAny(1786847930.0); got != 1786847930000 {
		t.Errorf("seconds = %d", got)
	}
	if got := whenOfAny(1786847930141.0); got != 1786847930141 {
		t.Errorf("ms passthrough = %d", got)
	}
	if got := whenOfAny("2026-11-24T00:00:00Z"); got == 0 {
		t.Error("ISO8601 should parse")
	}
	if got := whenOfAny("not-a-date"); got != 0 {
		t.Errorf("garbage = %d", got)
	}
	if got := whenOfAny(nil); got != 0 {
		t.Errorf("nil = %d", got)
	}
}

// TestGlobalSiteIdeVersion 全球站版本头（插件 ideVersion 3.5.51）与 CN 不同。
func TestGlobalSiteIdeVersion(t *testing.T) {
	if Sites["trae-global"].IdeVersion != "3.5.51" || Sites["trae-global"].ClientID != "ono9krqynydwx5" {
		t.Errorf("global site constants: %+v", Sites["trae-global"])
	}
	if Sites["trae-cn"].IdeVersion != "0.1.69" || !Sites["trae-cn"].Checkin {
		t.Errorf("cn site constants: %+v", Sites["trae-cn"])
	}
	if !strings.Contains(Sites["trae-global"].APIHost, "coresg") {
		t.Errorf("global SG host = %s", Sites["trae-global"].APIHost)
	}
}
