// client.go SOLO 上游客户端：llm_utils_chat / get_detail_param / ExchangeToken /
// checkin_credits / ide_user_ent_usage + 错误分类。
package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"traework2api/internal/auth"
)

// 签到业务错误哨兵。上游签到接口用 HTTP 200 + body 里的 code/message 表达失败，
// 调用方需要区分「今天已签过」（视为成功）和「真的失败」。
var (
	// ErrAlreadyCheckedIn 今日已签到。
	ErrAlreadyCheckedIn = errors.New("already checked in")
	// ErrCheckinDisabled 该账号签到活动未开启。
	ErrCheckinDisabled = errors.New("checkin disabled")
)

// CheckinError 签到接口的业务错误（HTTP 200 但 code != 0 或 success == false）。
type CheckinError struct {
	Code int    // 上游业务码；0 表示无 code 字段，仅 success=false
	Msg  string // 上游 message/msg
}

func (e *CheckinError) Error() string {
	if e.Msg == "" {
		return fmt.Sprintf("checkin business error code=%d", e.Code)
	}
	return fmt.Sprintf("checkin business error code=%d msg=%s", e.Code, e.Msg)
}

// IsAlreadyCheckedIn 判定错误是否表示「今日已签到」。
// 优先用哨兵判定；同时对上游文案兜底（不同版本返回中文/英文提示）。
func IsAlreadyCheckedIn(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrAlreadyCheckedIn) {
		return true
	}
	var ce *CheckinError
	if errors.As(err, &ce) {
		return containsAlreadyMarker(ce.Msg)
	}
	return containsAlreadyMarker(err.Error())
}

// containsAlreadyMarker 仅匹配明确表示「今日已签到」的标记，避免 429/5xx 等
// 响应体里偶然出现 "checkin" 字样被误判。
func containsAlreadyMarker(msg string) bool {
	s := strings.ToLower(msg)
	return strings.Contains(msg, "已签到") ||
		strings.Contains(s, "already check") ||
		strings.Contains(s, "already signed")
}

// ErrKind 错误分类，pool 据此决定冷却时长（SPEC §4.3）。
type ErrKind int

const (
	ErrNone        ErrKind = iota // 成功
	ErrPlanLimit                  // 1005 + plan → 权益不足（硬冷却 12h）
	ErrSoftRate                   // 429 → 短冷却 60s
	ErrSessionDead                // 401 + Cloud-IDE-JWT 失效 → 禁用
	ErrNotFound                   // 404 → 短冷却 60s 不累计 errCount
	ErrServer                     // 5xx
	ErrClient                     // 其他 4xx
)

func (k ErrKind) String() string {
	switch k {
	case ErrPlanLimit:
		return "plan_limit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

var sessionDeadMarkers = []string{"login", "token 失效", "token invalid", "session", "unauthorized", "401"}

// Classify 按 HTTP 状态码 + body 判定错误类别（SPEC §4.3）。
func Classify(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	// 1005 plan 权益不足
	if strings.Contains(body, `"code":1005`) || (strings.Contains(body, "1005") && strings.Contains(lower, "plan")) {
		return ErrPlanLimit
	}
	// session 失效
	if status == http.StatusUnauthorized {
		for _, m := range sessionDeadMarkers {
			if strings.Contains(lower, strings.ToLower(m)) {
				return ErrSessionDead
			}
		}
		return ErrSessionDead
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	if status >= 400 {
		return ErrClient
	}
	return ErrNone
}

// Client SOLO 上游 HTTP 客户端。Host 字段可覆盖便于测试。
type Client struct {
	// HTTP 用于短 JSON 请求（ExchangeToken/模型/签到/积分），有总超时兜底。
	HTTP *http.Client
	// StreamHTTP 用于 SSE 流式对话：不设总超时，避免长流被截断；
	// 通过 Transport.ResponseHeaderTimeout 兜底「上游一直不返回首字节」的悬挂。
	// 与 HTTP 共享同一 Transport（连接池复用）。nil 时 ChatStream 回退 HTTP。
	StreamHTTP *http.Client

	AgentHost string // https://trae-api-cn.mchost.guru
	UgHost    string // https://api.trae.cn
	OAuthHost string // https://api.trae.com.cn
	ClientID  string // en1oxy7wnw8j9n

	// CheckinRetryDelay 签到业务码 9074（限流）后的重试等待；0 表示不重试。
	CheckinRetryDelay time.Duration
}

// New 生产默认值。配置连接池减少 TLS 握手。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second, // 首字节兜底（长推理预留），不限制整流时长
	}
	return &Client{
		HTTP:              &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP:        &http.Client{Transport: tr}, // 无总超时
		AgentHost:         AgentHost,
		UgHost:            UgHost,
		OAuthHost:         OAuthHost,
		ClientID:          ClientID,
		CheckinRetryDelay: 8 * time.Second,
	}
}

func (c *Client) agentBase() string { return c.AgentHost }
func (c *Client) ugBase() string    { return c.UgHost }
func (c *Client) oauthBase() string { return c.OAuthHost }

// doJSON 发请求并解 JSON；HTTP 非 2xx 时返回带 body 片段的 *Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	return raw, nil
}

// RefreshToken 通过 ExchangeToken 强制刷新 access token（refreshToken 轮换）。
// 成功时更新 a 的字段；调用方负责 SaveAtomic。全程持 a 写锁。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	return c.refreshLocked(a)
}

// RefreshTokenIfNeeded 仅当 token 在 skew 内即将过期（或已过期）时才刷新，
// 返回是否真正刷新。持锁内重查，避免并发请求对同一账号重复 ExchangeToken 轮换。
// 调用方仅在 returned 为 true 时需要 SaveAtomic。
func (c *Client) RefreshTokenIfNeeded(a *auth.Auth, skew time.Duration) (bool, error) {
	a.Lock()
	defer a.Unlock()
	if !a.NeedsRefreshLocked(skew) {
		return false, nil
	}
	if err := c.refreshLocked(a); err != nil {
		return false, err
	}
	return true, nil
}

// refreshLocked 是 RefreshToken 的持锁内部实现；调用方必须已持有 a 写锁。
// 任何失败路径都不改写 a 字段，保证旧 refreshToken 可重试。
func (c *Client) refreshLocked(a *auth.Auth) error {
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	host := a.ApiHost
	if host == "" {
		host = c.oauthBase()
	}
	body := map[string]any{
		"ClientID":     c.ClientID,
		"RefreshToken": a.RefreshToken, // 已持 a 写锁，直接读
		"ClientSecret": "-",
		"UserID":       "",
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpExchange, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	OAuthHeaders(req)
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var resp struct {
		Result struct {
			Token                string `json:"Token"`
			TokenExpireAt        int64  `json:"TokenExpireAt"`
			TokenExpireDuration  int64  `json:"TokenExpireDuration"`
			RefreshToken         string `json:"RefreshToken"`
			RefreshExpireAt      int64  `json:"RefreshExpireAt"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("exchange parse: %w", err)
	}
	if resp.Result.Token == "" {
		return fmt.Errorf("refresh_failed: no token in response — re-login required")
	}
	a.AccessToken = resp.Result.Token
	if resp.Result.RefreshToken != "" {
		a.RefreshToken = resp.Result.RefreshToken
	}
	// 过期时间：优先 TokenExpireAt（上游返回毫秒，需归一化为 Unix 秒）
	if resp.Result.TokenExpireAt > 0 {
		a.ExpiresAt = normalizeExpiresAt(resp.Result.TokenExpireAt)
	} else if resp.Result.TokenExpireDuration > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(resp.Result.TokenExpireDuration) * time.Second).Unix()
	}
	return nil
}

// normalizeExpiresAt 把 ExchangeToken 的 TokenExpireAt 归一化为 Unix 秒。
// 上游返回毫秒（如 1786847930141），auth 文件用秒（1786847930）。
// 毫秒时间戳 ~1.7e12，秒时间戳 ~1.7e9，用 1e12 区分。
func normalizeExpiresAt(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

// ChatStream 发 llm_utils_chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc 为 nil、body 为上游响应体（供调用方 Classify）、err 为 nil；
// 只有传输层失败才返回 err。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpChat, bytes.NewReader(PrepareBody(body)))
	if err != nil {
		return nil, 0, nil, err
	}
	SOLOHeaders(req, a, true)
	// 用专用流客户端（无总超时），避免长 SSE 流被 HTTP.Timeout 截断。
	hc := c.HTTP
	if c.StreamHTTP != nil {
		hc = c.StreamHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		log.Printf("chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("chat_stream uid=%s: upstream %d %s body=%s",
			a.UID, resp.StatusCode, kind, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// ModelInfo 动态模型信息。
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64 // = maxInputTokens
	MaxTokens     int64 // = maxOutputTokens
	// IsCustomModel 自定义模型（第三方代理，需额外授权）；
	// 部分模型上游不返回该字段，调用方可用 custom_model_ 前缀兜底。
	IsCustomModel bool
}

// FetchModels 拉 SOLO 模型表（get_detail_param，32 配置）。
// 按 config_name 去重：上游可能为同一模型返回多条配置。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	body := map[string]any{
		"function":            Function,
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpModels, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	SOLOHeaders(req, a, false)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		ConfigInfoList []struct {
			ConfigName    string `json:"config_name"`
			DisplayConfig struct {
				DisplayName   string `json:"display_name"`
				IsCustomModel bool   `json:"is_custom_model"`
			} `json:"display_config"`
			ModelDetailList []struct {
				ModelName string `json:"model_name"`
			} `json:"model_detail_list"`
		} `json:"config_info_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	seen := make(map[string]bool, len(resp.ConfigInfoList))
	out := make([]ModelInfo, 0, len(resp.ConfigInfoList))
	for _, cfg := range resp.ConfigInfoList {
		name := strings.TrimSpace(cfg.ConfigName)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, ModelInfo{
			ID:            name,
			Name:          cfg.DisplayConfig.DisplayName,
			IsCustomModel: cfg.DisplayConfig.IsCustomModel,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

// checkinResp 签到 status/claim 的公共响应字段。
// 上游失败时仍返回 HTTP 200，错误只体现在 code / success / message 里，
// 因此必须解析 body，不能只看状态码。
// CheckedIn/Enable 用指针区分「字段缺失」与显式 false：status 响应缺字段属于
// 异常响应，必须报错而不是当成「未签到」去 claim。
type checkinResp struct {
	CheckedIn *bool  `json:"checked_in"`
	Credits   int64  `json:"credits"`
	Enable    *bool  `json:"enable"`
	Code      int    `json:"code"`
	Message   string `json:"message"`
	Msg       string `json:"msg"`
	Success   *bool  `json:"success"` // 指针区分「字段缺失」与显式 false
}

// text 返回上游文案（message 优先，兼容 msg）。
func (r checkinResp) text() string {
	if s := strings.TrimSpace(r.Message); s != "" {
		return s
	}
	return strings.TrimSpace(r.Msg)
}

// businessErr 按业务字段判定失败；无业务错误时返回 nil。
func (r checkinResp) businessErr() error {
	if r.Code != 0 {
		return &CheckinError{Code: r.Code, Msg: r.text()}
	}
	if r.Success != nil && !*r.Success {
		return &CheckinError{Code: 0, Msg: r.text()}
	}
	return nil
}

// CheckinStatus 查询签到状态。业务码非零或 success=false 时返回错误。
func (c *Client) CheckinStatus(a *auth.Auth) (checkedIn bool, credits int64, enable bool, err error) {
	// 官方客户端签到请求体为 {"req_source":1}（抓包确认），非 {}。
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinStatus, bytes.NewReader([]byte(`{"req_source":1}`)))
	if err != nil {
		return false, 0, false, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		log.Printf("checkin status uid=%s: %v", a.UID, err)
		return false, 0, false, err
	}
	var resp checkinResp
	if err := json.Unmarshal(data, &resp); err != nil {
		err = fmt.Errorf("checkin status parse: %w", err)
		log.Printf("checkin status uid=%s: %v", a.UID, err)
		return false, 0, false, err
	}
	if berr := resp.businessErr(); berr != nil {
		log.Printf("checkin status uid=%s: %v", a.UID, berr)
		return false, 0, false, berr
	}
	if resp.CheckedIn == nil || resp.Enable == nil {
		err := fmt.Errorf("checkin status: missing checked_in or enable")
		log.Printf("checkin status uid=%s: %v", a.UID, err)
		return false, 0, false, err
	}
	log.Printf("checkin status uid=%s checked_in=%t credits=%d enable=%t",
		a.UID, *resp.CheckedIn, resp.Credits, *resp.Enable)
	return *resp.CheckedIn, resp.Credits, *resp.Enable, nil
}

// CheckinClaim 执行签到。
// 解析业务码：code 非零或 success=false 一律返回错误；code=9074 是限流，
// 等 CheckinRetryDelay 后重试一次。返回 nil 只表示「上游接受了本次请求」，
// 是否真正签到成功由 DailyCheckin 的二次 status 复核裁决。
func (c *Client) CheckinClaim(a *auth.Auth) error {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinClaim, bytes.NewReader([]byte(`{"req_source":1}`)))
		if err != nil {
			return err
		}
		UgHeaders(req, a)
		data, err := c.doJSON(req)
		if err != nil {
			log.Printf("checkin claim uid=%s: %v", a.UID, err)
			return err
		}
		var resp checkinResp
		if err := json.Unmarshal(data, &resp); err != nil {
			err = fmt.Errorf("checkin claim parse: %w", err)
			log.Printf("checkin claim uid=%s: %v", a.UID, err)
			return err
		}
		if resp.Code == CodeCheckinRateLimited && attempt == 0 && c.CheckinRetryDelay > 0 {
			log.Printf("checkin claim uid=%s: rate limited (code=%d), retry after %s",
				a.UID, resp.Code, c.CheckinRetryDelay)
			time.Sleep(c.CheckinRetryDelay)
			continue
		}
		if berr := resp.businessErr(); berr != nil {
			log.Printf("checkin claim uid=%s: %v", a.UID, berr)
			return berr
		}
		log.Printf("checkin claim uid=%s accepted code=%d msg=%s", a.UID, resp.Code, resp.text())
		return nil
	}
}

// DailyCheckin 完整签到：查状态 → （未签则）claim → 复核状态。
// 以二次 status 的 checked_in 为最终结论，避免「claim 返回 200 但实际没签上」。
// 已签到时返回 ErrAlreadyCheckedIn，未开启活动返回 ErrCheckinDisabled。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	checkedIn, _, enable, err := c.CheckinStatus(a)
	if err != nil {
		return err
	}
	if checkedIn {
		return ErrAlreadyCheckedIn
	}
	if !enable {
		return ErrCheckinDisabled
	}
	if err := c.CheckinClaim(a); err != nil {
		return err
	}
	verified, _, _, err := c.CheckinStatus(a)
	if err != nil {
		return fmt.Errorf("checkin verification: %w", err)
	}
	if !verified {
		err := fmt.Errorf("checkin verification failed: checked_in=false after claim")
		log.Printf("checkin verify uid=%s: %v", a.UID, err)
		return err
	}
	log.Printf("checkin verified uid=%s", a.UID)
	return nil
}

// UserEntUsage 聚合剩余积分（ide_user_ent_usage）。
// 上游每个包的 quota.credits_limit 为总额度，usage.credits_amount 为已消耗，
// 剩余 = Σ(credits_limit) − Σ(usage.credits_amount)。
// 旧实现只求和 credits_limit，导致把总额度当剩余且永不变动，故修正。
func (c *Client) UserEntUsage(a *auth.Auth) (remain int64, err error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpEntUsage, bytes.NewReader([]byte("{}")))
	if err != nil {
		return 0, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return 0, err
	}
	var resp struct {
		IsCreditsBilling        bool `json:"is_credits_billing"`
		UserEntitlementPackList []struct {
			EntitlementBaseInfo struct {
				Quota struct {
					CreditsLimit int64 `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage struct {
				CreditsAmount float64 `json:"credits_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("ent usage parse: %w", err)
	}
	var limit, used float64
	for _, p := range resp.UserEntitlementPackList {
		limit += float64(p.EntitlementBaseInfo.Quota.CreditsLimit)
		used += p.Usage.CreditsAmount
	}
	// 剩余不能为负；API 仅提供总额度与已用量，无独立剩余字段。
	remain = int64(limit - used)
	if remain < 0 {
		remain = 0
	}
	return remain, nil
}

// GetUserInfo 查询账号信息（登录用）。
func (c *Client) GetUserInfo(a *auth.Auth) (uid, nickname, enterpriseID string, err error) {
	host := a.ApiHost
	if host == "" {
		host = c.oauthBase()
	}
	body := map[string]any{"ReqSource": "IDE", "IDEVersion": IdeVersion}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpUserInfo, bytes.NewReader(raw))
	if err != nil {
		return "", "", "", err
	}
	OAuthHeaders(req)
	req.Header.Set("X-Cloudide-Token", a.JWT()) // 读锁快照
	data, err := c.doJSON(req)
	if err != nil {
		return "", "", "", err
	}
	var resp struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", "", fmt.Errorf("userinfo parse: %w", err)
	}
	return resp.Result.UserID, resp.Result.ScreenName, resp.Result.EnterpriseID, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
