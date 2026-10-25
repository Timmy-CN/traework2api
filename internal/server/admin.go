package server

// admin.go 管理 API（unified-admin 面板用）：签到/刷新/启停/总览。
// 鉴权复用现有 withAuth（Bearer $TW2A_API_KEY）；端点形状对齐
// codearts2api / xiaohuanxiong2api 的既有面板接口。

import (
	"encoding/json"
	"net/http"
	"strings"

	"traework2api/internal/scheduler"
)

// uidBody {uid, reason}。
type uidBody struct {
	UID    string `json:"uid"`
	Reason string `json:"reason"`
}

func readBody(r *http.Request) uidBody {
	var b uidBody
	_ = json.NewDecoder(r.Body).Decode(&b)
	return b
}

// writeAdminJSON 统一输出。
func writeAdminJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// RegisterAdmin 挂载 admin 路由（在 NewHandler 里调用）。
func (h *Handler) RegisterAdmin(sch *scheduler.Scheduler) {
	h.sch = sch
	h.mux.HandleFunc("GET /admin/overview", h.withAuth(h.adminOverview))
	h.mux.HandleFunc("POST /admin/checkin", h.withAuth(h.adminCheckin))
	h.mux.HandleFunc("POST /admin/refresh", h.withAuth(h.adminRefresh))
	h.mux.HandleFunc("POST /admin/accounts/enable", h.withAuth(h.adminEnable))
	h.mux.HandleFunc("POST /admin/accounts/disable", h.withAuth(h.adminDisable))
}

// adminOverview GET /admin/overview：/status 全量 + 汇总（面板总览主数据源）。
func (h *Handler) adminOverview(w http.ResponseWriter, r *http.Request) {
	accounts := h.cfg.Pool.List()
	total, healthy, disabled, cooling := 0, 0, 0, 0
	for _, a := range accounts {
		total++
		switch {
		case a.Disabled:
			disabled++
		case a.Cooling:
			cooling++
		default:
			healthy++
		}
	}
	models := h.modelList()
	ids := make([]map[string]any, 0, len(models))
	for _, m := range models {
		ids = append(ids, map[string]any{"id": m["id"]})
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{
		"service": "traework2api",
		"region":  "cn",
		"stats": map[string]any{
			"total":    total,
			"healthy":  healthy,
			"disabled": disabled,
			"cooling":  cooling,
		},
		"accounts": accounts,
		"models":   ids,
	})
}

// adminCheckin POST /admin/checkin：{"uid":"..."} 单账号；uid 空 = 全员
// （异步触发，立即返回——RunCheckinNow 本身就是全池闭环）。
func (h *Handler) adminCheckin(w http.ResponseWriter, r *http.Request) {
	b := readBody(r)
	if h.sch == nil {
		writeAdminJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "message": "scheduler not wired"})
		return
	}
	if b.UID != "" {
		if !h.sch.CheckinOne(b.UID) {
			writeAdminJSON(w, http.StatusNotFound, map[string]any{"ok": false, "message": "account not found: " + b.UID})
			return
		}
		writeAdminJSON(w, http.StatusOK, map[string]any{"ok": true, "scope": "uid:" + b.UID})
		return
	}
	// 全员签到可能耗时（每账号 status→claim→复核→ent-usage），异步执行。
	go safeRun(h.sch.RunCheckinNow)
	writeAdminJSON(w, http.StatusOK, map[string]any{"ok": true, "scope": "all"})
}

// adminRefresh POST /admin/refresh：{"uid":"..."} 单账号（强制，忽略窗口）；
// uid 空 = 全员（仅临期账号）。
func (h *Handler) adminRefresh(w http.ResponseWriter, r *http.Request) {
	b := readBody(r)
	if h.sch == nil {
		writeAdminJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "message": "scheduler not wired"})
		return
	}
	if b.UID != "" {
		if !h.sch.RefreshOne(b.UID) {
			writeAdminJSON(w, http.StatusNotFound, map[string]any{"ok": false, "message": "account not found or refresh failed: " + b.UID})
			return
		}
		writeAdminJSON(w, http.StatusOK, map[string]any{"ok": true, "scope": "uid:" + b.UID})
		return
	}
	go safeRun(h.sch.RunRefreshNow)
	writeAdminJSON(w, http.StatusOK, map[string]any{"ok": true, "scope": "all"})
}

// safeRun 面板触发的后台任务兜底 recover（goroutine panic 不能带崩服务）。
func safeRun(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// adminEnable POST /admin/accounts/enable：{"uid":"..."} 解禁+清冷却。
func (h *Handler) adminEnable(w http.ResponseWriter, r *http.Request) {
	b := readBody(r)
	if b.UID == "" {
		writeAdminJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "uid required"})
		return
	}
	if _, ok := h.cfg.Pool.Status(b.UID); !ok {
		writeAdminJSON(w, http.StatusNotFound, map[string]any{"ok": false, "message": "account not found: " + b.UID})
		return
	}
	h.cfg.Pool.Enable(b.UID)
	writeAdminJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": b.UID, "enabled": true})
}

// adminDisable POST /admin/accounts/disable：{"uid":"...","reason":"..."}。
func (h *Handler) adminDisable(w http.ResponseWriter, r *http.Request) {
	b := readBody(r)
	if b.UID == "" {
		writeAdminJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "uid required"})
		return
	}
	if _, ok := h.cfg.Pool.Status(b.UID); !ok {
		writeAdminJSON(w, http.StatusNotFound, map[string]any{"ok": false, "message": "account not found: " + b.UID})
		return
	}
	reason := strings.TrimSpace(b.Reason)
	if reason == "" {
		reason = "disabled via admin api"
	}
	h.cfg.Pool.Disable(b.UID, reason)
	writeAdminJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": b.UID, "enabled": false})
}
