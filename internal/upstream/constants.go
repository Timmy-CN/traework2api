// constants.go SOLO 上游技术常量（SPEC §1，来自实测，禁止改动）。
package upstream

const (
	AgentHost      = "https://trae-api-cn.mchost.guru"
	UgHost         = "https://api.trae.cn"
	OAuthHost      = "https://api.trae.com.cn"
	ConsoleHost    = "https://www.trae.cn"
	ClientID       = "en1oxy7wnw8j9n" // SOLO stable
	AppID          = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	// IdeVersion / IdeVersionCode 与官方客户端对齐（2026-09 实测 Trae CN 3.3.102；
	// 旧值 0.1.43/20260716 仍可用，但对齐后模型列表多 1 条有效条目、更不易被风控）。
	IdeVersion     = "3.3.102"
	IdeVersionCode = "20260912"
	DeviceBrand    = "System Product Name"
	OSVersion      = "Windows 10 Pro"
	Function       = "solo_work_lite"

	// 端点
	EpChat          = "/api/agent/v3/llm_utils_chat"
	EpModels        = "/api/ide/v1/get_detail_param"
	EpExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	EpCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	EpCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
	EpEntUsage      = "/trae/api/v2/pay/ide_user_ent_usage"

	// CodeCheckinRateLimited 签到 claim 的限流业务码（HTTP 200 返回），可稍后重试。
	CodeCheckinRateLimited = 9074
)
