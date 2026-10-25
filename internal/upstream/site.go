package upstream

import (
	"net/url"
	"strings"
)

// site.go Trae 双站点配置（插件 SITES 口径）：CN（trae.cn）与全球
//（trae.ai）。账号落在哪个站点由登录区域（region）与登录 host 决定，
// 各 host 按账号取（agentBaseFor/ugBaseFor/payBaseFor），不再全局唯一。
//
// 全球站与 CN 的关键差异：
//   - 续期走普通 ExchangeToken（无 deviceExchange / DeviceProof）；
//   - 模型网关按 region 分 SG（coresg）/ US（coreva）；
//   - 用量走美元计费（/trae/api/v1/pay/user_current_entitlement_list），
//     US 账号在自己 pay host 上问，其余在中央 pay host；
//   - 无 /trae/api/… 自己的 JSON 页（无签到）。

// Site 一个 Trae 部署的全部常量。
type Site struct {
	ID           string // "trae-cn" / "trae-global"
	Realm        string // 错误/文案里的站点名
	WebHost      string // 授权页裸 host
	AuthHost     string // token/账号 host
	APIHost      string // 模型网关（CN 的 mchost.guru；Global 的 SG）
	USAPIHost    string // Global 的 US 模型网关（CN 为空）
	PayHost      string // 用量 host（CN 用 AuthHost，不单列）
	USPayHost    string // Global 的 US 用量 host（CN 为空）
	ClientID     string // 账号的 IDE client
	AppID        string
	IdeVersion   string
	IdeVerCode   string
	ClientVer    string
	ClientVerCod string
	// DeviceExchange 全球站为 nil：续期走普通轮换。
	DeviceExchange []DeviceExchangeAs
	// Checkin 全球站为 false：无 /trae/api/v2/ug 签到页。
	Checkin bool
	// DollarUsage 全球站 true：用量走美元计费端点。
	DollarUsage bool
}

// DeviceExchangeAs 设备绑定续期的 client 身份（依次尝试）。
type DeviceExchangeAs struct {
	ClientID string
	Platform string
	Version  string
}

// Sites 两站点表（照插件 SITES 抄录）。
var Sites = map[string]*Site{
	"trae-cn": {
		ID:           "trae-cn",
		Realm:        "Trae CN",
		WebHost:      "https://www.trae.cn",
		AuthHost:     "https://api.trae.cn",
		APIHost:      "https://trae-api-cn.mchost.guru",
		ClientID:     "en1oxy7wnw8j9n",
		AppID:        "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8",
		IdeVersion:   "0.1.69",
		IdeVerCode:   "20260917",
		ClientVer:    "0.1.69",
		ClientVerCod: "20260917",
		DeviceExchange: []DeviceExchangeAs{
			{ClientID: "en1oxy7wnw8j9n", Platform: "SOLO_PC", Version: "0.1.64"},
			{ClientID: "ono9krqynydwx5", Platform: "IDE_PC", Version: "3.3.99"},
		},
		Checkin: true,
	},
	"trae-global": {
		ID:           "trae-global",
		Realm:        "Trae Global",
		WebHost:      "https://www.trae.ai",
		AuthHost:     "https://growsg-normal.trae.ai",
		APIHost:      "https://coresg-normal.trae.ai",
		USAPIHost:    "https://coreva-normal.trae.ai",
		PayHost:      "https://api-sg-central.trae.ai",
		USPayHost:    "https://api-us-east.trae.ai",
		ClientID:     "ono9krqynydwx5",
		AppID:        "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8",
		IdeVersion:   "3.5.51",
		IdeVerCode:   "20260401",
		ClientVer:    "0.1.69",
		ClientVerCod: "20260917",
		// 全球站：普通轮换续期，无签到页，美元计费。
		DollarUsage: true,
	},
}

// SiteOf 账号所属站点：region 以 US 开头 → 全球站；登录 host 落在 trae.ai →
// 全球站；其余（含空）→ CN。插件 apiOf/regionOf 口径。
func SiteOf(region, apiHost string) *Site {
	r := strings.ToUpper(strings.TrimSpace(region))
	if strings.HasPrefix(r, "US") {
		return Sites["trae-global"]
	}
	if h := HostOf(apiHost); h != "" && strings.Contains(h, "trae.ai") {
		return Sites["trae-global"]
	}
	return Sites["trae-cn"]
}

// SiteForRegion 纯 region 判站（无账号时用）。
func SiteForRegion(region string) *Site {
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(region)), "US") {
		return Sites["trae-global"]
	}
	return Sites["trae-cn"]
}

// HostOf 归一 https origin（插件 traeHost 白名单：https、无端口/凭据/query/
// fragment，trae.cn / trae.ai / mchost.guru 三域）；不合规 → ""。
func HostOf(h string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return ""
	}
	u, err := url.Parse(h)
	if err != nil || u.Scheme != "https" || u.Port() != "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || u.Hostname() == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	// trae.com.cn 是 Go 侧在用的 CN 续期 host（插件白名单之外的历史域）。
	for _, d := range []string{"trae.cn", "trae.ai", "trae.com.cn", "mchost.guru"} {
		if host == d || strings.HasSuffix(host, "."+d) {
			return "https://" + host + strings.TrimSuffix(u.Path, "/")
		}
	}
	return ""
}
