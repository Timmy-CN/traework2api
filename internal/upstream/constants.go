// constants.go SOLO 上游技术常量（SPEC §1，来自实测，禁止改动）。
package upstream

const (
	AgentHost   = "https://trae-api-cn.mchost.guru"
	UgHost      = "https://api.trae.cn"
	OAuthHost   = "https://api.trae.com.cn"
	ConsoleHost = "https://www.trae.cn"
	ClientID    = "en1oxy7wnw8j9n" // SOLO stable
	AppID       = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	// IdeVersion / IdeVersionCode 与插件对齐：TRAE SOLO CN 0.1.69（20260917）。
	// Trae 只对足够新的客户端开放新模型；旧值 0.1.43/20260716 与 IDE 3.3.102
	// 也可用，但模型列表条数可能少 1 条。
	IdeVersion     = "0.1.69"
	IdeVersionCode = "20260917"
	DeviceBrand    = "System Product Name"
	OSVersion      = "Windows 10 Pro"
	Function       = "solo_work_lite"

	// 端点
	EpChat          = "/api/agent/v3/llm_utils_chat"
	EpModels        = "/api/ide/v1/get_detail_param"
	EpBatchModels   = "/api/ide/v1/batch_get_detail_param"
	EpExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	EpCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	EpCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
	EpEntUsage      = "/trae/api/v2/pay/ide_user_ent_usage"
	// EpDollarUsage 全球站的美元计费端点（插件 SITES["trae-global"].usage）。
	EpDollarUsage = "/trae/api/v1/pay/user_current_entitlement_list"

	// CodeCheckinRateLimited 签到 claim 的限流业务码（HTTP 200 返回）：需在
	// Trae CN app 内签到，当天不再问（插件口径 ineligible）。
	CodeCheckinRateLimited = 9074

	// CodeCheckinIneligibleDevice claim 9095：本设备今天已签（done 口径，当天不再签）。
	CodeCheckinIneligibleDevice = 9095
	// CodeCheckinInAppOnly 同 9074 的别名（插件口径 ineligible）。
	CodeCheckinInAppOnly = CodeCheckinRateLimited
)
