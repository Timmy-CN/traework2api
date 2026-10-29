// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"traework2api/internal/pool"
	"traework2api/internal/scheduler"
	"traework2api/internal/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool         *pool.Pool
	Upstream     *upstream.Client
	APIKey       string        // 空 = 不鉴权
	MaxRotate    int           // 单请求最多换号次数，默认 3
	PlanCooldown time.Duration // 1005 冷却，默认 12h
	SoftCooldown time.Duration // 429 冷却，默认 60s
	ErrThreshold int           // 连续错误阈值，默认 3
	ErrCooldown  time.Duration // 错误冷却，默认 10m
	RefreshSkew  time.Duration // token 预刷新窗口，默认 24h
	DefaultModel string        // 默认 glm-5.2

	// ShowInternalModels 在 /v1/models 透出内部模型配置
	// （custom_model_* / *subagent* / summary）；默认过滤。
	ShowInternalModels bool

	// Scheduler 调度器（admin API 的签到/刷新触发用；nil = admin 触发端点 503）。
	Scheduler *scheduler.Scheduler
}

// maxBodyBytes 请求体大小上限（8MB），超过返回 413。
const maxBodyBytes = 8 << 20

// Handler 主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux
	sch *scheduler.Scheduler
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.PlanCooldown <= 0 {
		cfg.PlanCooldown = 12 * time.Hour
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 24 * time.Hour
	}
	if cfg.DefaultModel == "" {
		cfg.DefaultModel = upstream.DefaultConfigName
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), sch: cfg.Scheduler}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	h.RegisterAdmin(cfg.Scheduler)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey != "" {
			authz := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if len(authz) < len(prefix) || !strings.EqualFold(authz[:len(prefix)], prefix) {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
			key := authz[len(prefix):]
			// 常量时间比较，防时序攻击（本地代理但按规范）。
			if subtle.ConstantTimeCompare([]byte(key), []byte(h.cfg.APIKey)) != 1 {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": h.cfg.Pool.List(),
	})
}

// ---------------------------------------------------------------------------
// 模型映射
// ---------------------------------------------------------------------------

// mapModel 将客户端传入的 model 映射为 config_name（SPEC §4.5）：
//
//	"glm-5.2"（config_name）        → 直接转发
//	"glm-5.2__dev"（内部名）        → 去掉后缀映射回 config_name
//	"auto" / ""                     → 默认模型
//	其他未知                        → 400
func (h *Handler) mapModel(model string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" || model == "auto" {
		return h.cfg.DefaultModel, nil
	}
	// 去掉内部名后缀（__dev / __max 等）
	base := model
	if i := strings.Index(model, "__"); i >= 0 {
		base = model[:i]
	}
	if h.knownModel(base) {
		return base, nil
	}
	// 宽松匹配：下划线 → 横线，大小写不敏感（deepseek_v4_pro → DeepSeek-V4-Pro）
	norm := normalizeModelName(base)
	if h.knownModel(norm) {
		return norm, nil
	}
	return "", fmt.Errorf("unknown model %q", model)
}

// normalizeModelName 将下划线命名的内部名归一化为 config_name 风格（横线分隔）。
func normalizeModelName(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
	}
	return strings.Join(parts, "-")
}

// knownModel 判断 model 是否在动态/静态模型表中。
func (h *Handler) knownModel(model string) bool {
	for _, m := range h.modelList() {
		if m["id"] == model {
			return true
		}
	}
	return false
}

// 静态 SOLO 模型表（2026-09-19 实测 get_detail_param 快照，40 个全局 config_name；
// 动态拉取失败时回退；不含账号级自定义模型，内部配置在响应时过滤）。
var staticModels = []map[string]any{
	{"id": "Doubao-Seed-Evolving", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "Doubao-Seed-2.1-Pro", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "seed-code-pro-0430", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "Doubao-Seed-2.1-Turbo", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "computer_use_subagent", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "Doubao-Seed-2.0-Code", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "browser_use_subagent", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "glm-5.3", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "glm-5-turbo", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "glm-5", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "DeepSeek-V4-Flash-Official", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "DeepSeek-V4-Flash", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "DeepSeek-V4-Pro-Official", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "DeepSeek-V4-Pro", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "kimi-k3", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "kimi-k2.7-code", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "kimi-k2.6", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "minimax-m3", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "qwen3.8-max", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "qwen-3.7-plus", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "sagitta", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "aquila", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_gemini", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_placeholder", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_1M_text", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_1M", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_doubao_1M", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_doubao_256k", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_kimi", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_claude", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_gpt-6", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_gpt-5", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_no-fc", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_deepseek_chat", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_deepseek_reasoner", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_deepseek_v4", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "file_search_agent", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "explore_sub_agent_v2", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "summary", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
}

// dynamicModelsCache 动态模型缓存（成功 1h / 失败负缓存 5min）。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time
	lastFail time.Time
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

// modelList 动态获取模型列表并包装成 OpenAI 格式；失败回退静态表。
// 多 function 目录：各 function 的模型合并，同 id 首现优先；带 __max 细目
// 且窗口更大的模型追加 <id>-max 条目（context 用 Max 窗口）。
func (h *Handler) modelList() []map[string]any {
	if infos := h.fetchDynamicModels(); len(infos) > 0 {
		visible := h.visibleModels(infos)
		if len(visible) == 0 {
			visible = infos // 过滤后为空则不过滤，避免返回空列表
		}
		out := make([]map[string]any, 0, len(visible))
		for _, mi := range visible {
			entry := map[string]any{
				"id":             mi.ID,
				"object":         "model",
				"created":        1753600000,
				"owned_by":       "trae-solo",
				"context_length": mi.ContextWindow,
			}
			if entry["context_length"] == 0 {
				entry["context_length"] = 131072
			}
			// display_name 是官方客户端的展示名（如 glm-5.3 → GLM-5.3），
			// 便于与 IDE 界面对照；OpenAI 客户端不认识该字段，会忽略。
			if mi.Name != "" {
				entry["display_name"] = mi.Name
			}
			out = append(out, entry)
		}
		return out
	}
	return h.staticModelList()
}

// internalModelPrefix 自定义代理模型前缀：custom_model_* 与 custom_<厂商>-<模型>
// （第三方 provider 接入，需额外授权，不可直接对话）。
const internalModelPrefix = "custom_"

// internalModelNames 内部工具配置的精确名（非对话模型，不对外列出）。
var internalModelNames = map[string]bool{
	"summary":            true, // 摘要器
	"title_generation":   true, // 标题生成
	"input_optimization": true, // 输入优化
	"fast_apply":         true, // 快速应用
	"fast_apply_new":     true,
}

// isInternalModel 判定是否为不该在 /v1/models 暴露的内部模型配置：
// 自定义代理（custom_* / is_custom_model）、内部子代理与检索代理
// （*subagent* / *sub_agent* / *_agent）以及工具类配置（summary 等）。
// 官方客户端不会把它们当作可选模型展示。
func isInternalModel(id string, isCustom bool) bool {
	name := strings.TrimSpace(id)
	if name == "" {
		return true
	}
	if isCustom || strings.HasPrefix(name, internalModelPrefix) {
		return true
	}
	if internalModelNames[name] {
		return true
	}
	lower := strings.ToLower(name)
	if strings.Contains(lower, "subagent") || strings.Contains(lower, "sub_agent") {
		return true
	}
	// 内部子代理/检索代理：computer_use_subagent、file_search_agent 等
	return strings.HasSuffix(lower, "_agent")
}

// visibleModels 过滤内部模型配置；ShowInternalModels 打开时原样返回。
func (h *Handler) visibleModels(infos []upstream.ModelInfo) []upstream.ModelInfo {
	if h.cfg.ShowInternalModels {
		return infos
	}
	out := make([]upstream.ModelInfo, 0, len(infos))
	for _, mi := range infos {
		if isInternalModel(mi.ID, mi.IsCustomModel) {
			continue
		}
		out = append(out, mi)
	}
	return out
}

// staticModelList 静态回退表，按同一规则过滤内部条目。
func (h *Handler) staticModelList() []map[string]any {
	if h.cfg.ShowInternalModels {
		return staticModels
	}
	out := make([]map[string]any, 0, len(staticModels))
	for _, m := range staticModels {
		if id, _ := m["id"].(string); isInternalModel(id, false) {
			continue
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return staticModels
	}
	return out
}

// fetchDynamicModels 从池中任一健康账号拉模型列表（get_detail_param），缓存 1h。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	acct := h.cfg.Pool.Pick()
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
	return infos
}

// ---------------------------------------------------------------------------
// chat
// ---------------------------------------------------------------------------

// setModelInBody 将 body 中 model 字段替换为 configName，并返回改写后的 body。
func setModelInBody(body []byte, configName string) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	obj["model"] = configName
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if len(body) > maxBodyBytes {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 8MB limit")
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	configName, err := h.mapModel(peek.Model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	body = setModelInBody(body, configName)

	// Max 语义（插件 named?.max 口径）：仅客户端点名 <id>-max 时进 Max 模式，
	// config_name 用 base id（payload 以 Max.Base 为准）；base 请求绝不携带
	// Max——目录给 base 挂的 __max 细目只说明「有 Max 档」，不代表请求要走
	// Max。fn/sees/levels 一律按 base 条目查（-max 条目未拷 levels）。
	chatID, isMaxReq := upstream.SplitMaxID(configName)
	infos := h.fetchDynamicModels()
	cat := h.catalogOf(infos)
	// REREAD_MS（插件口径）：目录没有该模型（列表漂移/未列名的 -max）时，
	// 按 5min 节流重读一次目录再重建；重读也没有就按现状走（负缓存语义）。
	if cat != nil && !cat.Has(chatID) {
		h.rereadModels()
		if infos = h.fetchDynamicModels(); len(infos) > 0 {
			cat = h.catalogOf(infos)
		}
	}
	fn := h.chatFunction(cat, chatID)
	max := h.maxModeOf(cat, chatID)
	if !isMaxReq {
		max = nil
	}
	sees := h.seesOf(cat, chatID)
	levels := h.levelsOf(cat, chatID)
	// Moonshot schema 判定（插件 moonshotModel 口径）：config/modelName/req.model
	// 三处任一命中即可；目录不可用时 cat.InternalNameOf 返回空。
	moonshot := upstream.IsMoonshotModel(peek.Model, configName, cat.InternalNameOf(chatID))

	tried := map[string]bool{}
	// wrongFunction 回退：同一账号先试目录 function，被 4001/4023/1005 拒时
	// 换其他 function 重试一次；成功后把目录里该模型的 function 归属更新。
	var lastErr error
	for range h.cfg.MaxRotate {
		acct := h.cfg.Pool.PickExcluding(tried)
		if acct == nil {
			break
		}
		tried[acct.UID] = true

		// token 临近过期 → 先 refresh（持锁重查，避免并发重复轮换；失败冷却换号）
		refreshed, err := h.cfg.Upstream.RefreshTokenIfNeeded(acct, h.cfg.RefreshSkew)
		if err != nil {
			lastErr = err
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				h.cfg.Pool.Disable(acct.UID, "refresh session dead")
			} else {
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolErr, h.cfg.ErrCooldown, "refresh: "+err.Error())
			}
			continue
		}
		if refreshed {
			_ = acct.SaveAtomic()
		}

		// 会话 id 来源：客户端的 X-Session-Id / X-Magpie-Trae-Session（截 128 字符，
		// 插件 sessionOf 同口径）；who = 账号身份，防止 Trae 把一个对话记到别的账号头上。
		session := strings.TrimSpace(r.Header.Get(upstream.SESSION_HEADER))
		if session == "" {
			session = strings.TrimSpace(r.Header.Get(upstream.SESSION_HEADER_ALT))
		}
		if len(session) > 128 {
			session = session[:128]
		}
		who := acct.UID
		if who == "" {
			who = acct.Nickname
		}

		// 本账号内 function 轮换：目录 function → 默认 → 完整花名册（去重
		// 保序，插件 fns 口径），wrongFunction 码（4001/4023/1005 = 该
		// function 不收这个模型）按顺序试下一个；1005 在轮换试尽后才按
		// plan 权益冷却换号。已试集合按账号隔离（换号后重新轮换）。
		fnsTried := map[string]bool{}
		curFn := fn
		if curFn == "" {
			curFn = upstream.Function
		}
		for {
			fnsTried[curFn] = true
			rc, status, respBody, terr := h.cfg.Upstream.ChatStream(acct, body, upstream.ChatOpts{
				Function: curFn, Model: configName, Max: max, Sees: sees, Levels: levels,
				Moonshot: moonshot,
			}, session, who)
			if terr != nil {
				lastErr = terr
				h.cfg.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
				break
			}
			if status >= 400 {
				kind := upstream.Classify(status, string(respBody))
				code := upstream.ErrorCodeOf(respBody)
				if kind != upstream.ErrPlanLimit && upstream.WrongFunctionCode(code) {
					if next, ok := h.nextFunction(cat, configName, h.cfg.Pool.LastFunction(acct.UID), fnsTried); ok {
						curFn = next
						continue
					}
				}
				switch kind {
				case upstream.ErrPlanLimit:
					h.cfg.Pool.Cooldown(acct.UID, pool.CoolPlan, h.cfg.PlanCooldown, "plan 权益不足")
					lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				case upstream.ErrSoftRate:
					h.cfg.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
					lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				case upstream.ErrSessionDead:
					h.cfg.Pool.Disable(acct.UID, "session dead")
					lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				case upstream.ErrNotFound:
					// 404 短冷却不累计 errCount（防雪崩）
					h.cfg.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "upstream 404")
					lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				default:
					h.cfg.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
					lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				}
				break
			}
			// 流内错误路由：wrongFunction 码（4001/4023/1005）换 function
			// 重试同账号；轮换试尽后 1005 → plan 冷却换号，其他 → 累计
			// 错误换号。成功后目录里该模型的 function 归属更新。
			se, handled := h.serveChat(w, acct.UID, rc, peek.Stream)
			if handled {
				if se == nil {
					// 成功（插件 listedBy.set/fnOf.set 口径）：目录归属与该账号
					// 的最后成功 function 一并更新——回退成功的 function 下次
					// 直达，不再先吃一个 4001。
					h.reassignFunction(configName, curFn)
					h.cfg.Pool.SetLastFunction(acct.UID, curFn)
				}
				return
			}
			if se != nil && upstream.WrongFunctionCode(se.Code) {
				if next, ok2 := h.nextFunction(cat, configName, h.cfg.Pool.LastFunction(acct.UID), fnsTried); ok2 {
					curFn = next
					continue
				}
				if se.Code == 1005 {
					// 每个可试 function 都拒收：按 plan 权益冷却换号。
					h.cfg.Pool.Cooldown(acct.UID, pool.CoolPlan, h.cfg.PlanCooldown, "plan 权益不足")
					lastErr = &upstream.Error{Kind: upstream.ErrPlanLimit, Status: 200, Msg: se.Error()}
					break
				}
			}
			if se != nil {
				h.routeStreamError(acct.UID, se)
			}
			break
		}
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}

// handleStreamError 流式响应中的上游业务错误 → pool 冷却状态机。
// 1005 plan 权益不足 → 长冷却；其余（5xx/参数错误等）→ 累计错误冷却。
func (h *Handler) handleStreamError(uid string, se *upstream.SOLOStreamError) {
	switch se.Kind() {
	case upstream.ErrPlanLimit:
		h.cfg.Pool.Cooldown(uid, pool.CoolPlan, h.cfg.PlanCooldown, "plan 权益不足")
	case upstream.ErrSoftRate:
		// 4008/4011（插件 quotaCode → 429 口径）：短冷却
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "quota/rate limit")
	case upstream.ErrSessionDead:
		// 1001/20101（插件 lapsedCode 口径）：禁用等重登
		h.cfg.Pool.Disable(uid, "session dead")
	default:
		h.cfg.Pool.NoteError(uid, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
	}
}

// routeStreamError 流内业务错误（非流式聚合/窥探返回）→ pool 冷却路由：
// 1005 plan → 长冷却；其余 → 累计错误。与 handleStreamError 同状态机，只是
// 调用方已拿到 se。
func (h *Handler) routeStreamError(uid string, se *upstream.SOLOStreamError) {
	h.handleStreamError(uid, se)
}

// ---------------------------------------------------------------------------
// 目录辅助（多 function / Max / multimodal / reasoning levels）
// ---------------------------------------------------------------------------

// catalogOf 从 FetchModels 的动态列表重建目录查询（缓存命中时零成本）。
// 目录不可用 → nil（调用方全部按默认）。
func (h *Handler) catalogOf(infos []upstream.ModelInfo) upstream.Catalog {
	if len(infos) == 0 {
		return nil
	}
	return upstream.CatalogFrom(infos)
}

// chatFunction 模型的目录 function（列出者）；目录不可用 → 空（走默认）。
func (h *Handler) chatFunction(cat upstream.Catalog, id string) string {
	if cat == nil {
		return ""
	}
	return cat.FunctionOf(id)
}

// maxModeOf 模型的 Max 模式（<id>-max 请求时）；无 → nil。
func (h *Handler) maxModeOf(cat upstream.Catalog, id string) *upstream.MaxMode {
	if cat == nil {
		return nil
	}
	return cat.MaxOf(id)
}

// seesOf 模型是否收图（目录 multimodal）；目录没说 → nil（按收图透传，
// 插件 sight.get 缺省口径）。
func (h *Handler) seesOf(cat upstream.Catalog, id string) *bool {
	if cat == nil {
		return nil
	}
	return cat.SeesOf(id)
}

// levelsOf 模型承载的 reasoning 等级；目录不可用 → 空（不发 effort）。
func (h *Handler) levelsOf(cat upstream.Catalog, id string) []string {
	if cat == nil {
		return nil
	}
	return cat.LevelsOf(id)
}

// rereadInterval 目录重读节流（插件 REREAD_MS = 5min）。
const rereadInterval = 5 * time.Minute

var lastModelsReread atomic.Int64 // UnixNano；0 = 没读过

// rereadModels 强制重读模型目录（绕过成功 TTL 与失败负缓存），按
// rereadInterval 节流；并发请求共享同一次重读（插件 readList 的 in-flight
// 共享语义在节流层面等价）。
func (h *Handler) rereadModels() {
	now := time.Now().UnixNano()
	last := lastModelsReread.Load()
	if last != 0 && time.Since(time.Unix(0, last)) < rereadInterval {
		return
	}
	if !lastModelsReread.CompareAndSwap(last, now) {
		return // 别的请求刚触发过
	}
	acct := h.cfg.Pool.Pick()
	if acct == nil {
		return
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		return
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
}

// reassignFunction 聊天成功后更新缓存目录里该模型的 function 归属；
// 缓存未命中（目录刚失效/静态回退）时静默跳过。
func (h *Handler) reassignFunction(id, fn string) {
	if fn == "" {
		return
	}
	dynamicModelsCache.Lock()
	defer dynamicModelsCache.Unlock()
	for i := range dynamicModelsCache.ids {
		if dynamicModelsCache.ids[i].ID == id {
			dynamicModelsCache.ids[i].Function = fn
			return
		}
	}
}

// functionCandidates wrongFunction 回退的候选顺序（插件 fns 口径）：
// 目录里该模型的归属 function → 该账号上次成功的 function → 默认 function
// → 完整花名册，去重保序。
func functionCandidates(cat upstream.Catalog, id, lastFn string) []string {
	var fns []string
	seen := map[string]bool{}
	add := func(fn string) {
		if fn != "" && !seen[fn] {
			seen[fn] = true
			fns = append(fns, fn)
		}
	}
	if cat != nil {
		add(cat.FunctionOf(id))
	}
	add(lastFn)
	add(upstream.Function)
	for _, fn := range upstream.ChatFunctions() {
		add(fn)
	}
	return fns
}

// nextFunction wrongFunction 回退的下一个 function：候选顺序里第一个
// 未试过的（插件 for-fn 轮换同口径）；全试过 → false。tried 由调用方
// 维护（按账号隔离，换号后重新轮换）。
func (h *Handler) nextFunction(cat upstream.Catalog, id, lastFn string, tried map[string]bool) (string, bool) {
	for _, fn := range functionCandidates(cat, id, lastFn) {
		if !tried[fn] {
			return fn, true
		}
	}
	return "", false
}

// serveChat 提交响应。流式请求（peekStream）不做窥探重试：错误事件原样
// 透传给客户端（SSE 恒 200），仅经回调冷却账号。非流式聚合先窥探首个
// event:error（插件 first(it) 口径）交给调用方路由——wrongFunction 码换
// function 重试、1005 plan 冷却换号——返回 handled=false；窥探到的字节与
// bufio 缓冲随 rest 回放给消费者，不能丢。
func (h *Handler) serveChat(w http.ResponseWriter, uid string, rc io.ReadCloser, peekStream bool) (se *upstream.SOLOStreamError, handled bool) {
	if peekStream {
		h.cfg.Pool.NoteSuccess(uid)
		_ = upstream.StreamWithError(w, rc, func(sse *upstream.SOLOStreamError) {
			h.handleStreamError(uid, sse)
		})
		rc.Close()
		return nil, true
	}
	replay, errEv, rest := peekFirstEvent(rc)
	if errEv != nil {
		rc.Close()
		return errEv, false
	}
	body := io.MultiReader(bytes.NewReader(replay), rest)
	h.cfg.Pool.NoteSuccess(uid)
	resp, err := upstream.Aggregate(body)
	rc.Close()
	if err != nil {
		var ase *upstream.SOLOStreamError
		if errors.As(err, &ase) {
			return ase, false
		}
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
		return nil, true
	}
	writeJSON(w, http.StatusOK, resp)
	return nil, true
}

// peekFirstEvent 读首个 SSE 事件（至首个空行）：replay 是已读的原始字节
// （须回放给消费者），rest 是持有 bufio 缓冲的剩余流；event:error → se。
func peekFirstEvent(rc io.Reader) (replay []byte, se *upstream.SOLOStreamError, rest io.Reader) {
	br := bufio.NewReaderSize(rc, 64<<10)
	var buf bytes.Buffer
	var event, data string
	flush := func() *upstream.SOLOStreamError {
		if data == "" {
			return nil
		}
		ev, err := upstream.ParseSOLOLine(event, data)
		if err != nil || ev == nil {
			return nil
		}
		if ev.Event == "error" {
			return &upstream.SOLOStreamError{Code: ev.ErrorCode, Msg: ev.ErrorMessage}
		}
		return nil
	}
	for {
		line, err := br.ReadString('\n')
		buf.WriteString(line)
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case trimmed == "":
			return buf.Bytes(), flush(), br
		case strings.HasPrefix(trimmed, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		case strings.HasPrefix(trimmed, "data:"):
			if data != "" {
				data += "\n"
			}
			data += strings.TrimPrefix(strings.TrimPrefix(trimmed, "data:"), " ")
		}
		if err != nil {
			return buf.Bytes(), flush(), br
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}
