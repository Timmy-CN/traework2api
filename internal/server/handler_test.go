package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"traework2api/internal/auth"
	"traework2api/internal/pool"
	"traework2api/internal/upstream"
)

// 模拟 SOLO SSE 响应（glm-5.2 回答"你好"）。
const soloSSE = "event:metadata\ndata:{\"model\":\"\",\"session_id\":\"s1\"}\n\n" +
	"event:output\ndata:{\"response\":\"你好\",\"reasoning_content\":\"想一下\",\"tool_calls\":null}\n\n" +
	"event:token_usage\ndata:{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}\n\n" +
	"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"

// newFakeUpstream 返回 ChatStream 走 fake 的 upstream.Client。
func newFakeUpstream(t *testing.T, behavior func(auth string) (status int, body string, isStream bool)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			authz := r.Header.Get("Authorization")
			status, body, isStream := behavior(authz)
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		AgentHost: "https://fake.example",
		UgHost:    "https://fake.example",
		OAuthHost: "https://fake.example",
		ClientID:  upstream.ClientID,
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testPoolWith(auths ...*auth.Auth) *pool.Pool {
	p := pool.New("")
	for _, a := range auths {
		p.Add(a)
		p.SetCredits(a.UID, 1000)
	}
	return p
}

func TestChatNonStreamAggregates(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz != "Cloud-IDE-JWT at1" {
			t.Errorf("auth=%q", authz)
		}
		return 200, soloSSE, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v", resp["object"])
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Errorf("content=%q", msg["content"])
	}
	if msg["reasoning_content"] != "想一下" {
		t.Errorf("reasoning=%q", msg["reasoning_content"])
	}
}

func TestChatStreamPassthrough(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, soloSSE, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("ct=%q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "你好") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body=%q", body)
	}
}

// TestChatRotatesOnPlanLimit 1005 流内错误：先在同账号按顺序换 function 重试
// （插件 wrongFunction 口径，5 个花名册全试），试尽才 plan 冷却换号——
// 「bad」账号 5 次 + 「good」账号 1 次成功。
func TestChatRotatesOnPlanLimit(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Cloud-IDE-JWT at-bad" {
			return 200, "event:error\ndata:{\"code\":1005,\"message\":\"plan limit\",\"extra\":{\"plan\":2}}\n\n", true
		}
		return 200, soloSSE, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// bad 账号试尽全部 function 花名册后才冷却；good 账号一次成功。
	if calls["Cloud-IDE-JWT at-bad"] != len(upstream.ChatFunctions()) || calls["Cloud-IDE-JWT at-good"] != 1 {
		t.Errorf("calls=%v want bad=%d good=1", calls, len(upstream.ChatFunctions()))
	}
	if st, _ := p.Status("bad"); !st.Cooling || st.Reason != "plan 权益不足" {
		t.Errorf("bad 应 plan 冷却: %+v", st)
	}
}

// TestChatStreamCooldownOnStreamError 流式请求中上游 event:error（如 5xx/参数错误）
// 应触发 NoteError 冷却（而非仅透传不冷却）。
func TestChatStreamCooldownOnStreamError(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, "event:error\ndata:{\"code\":5001,\"message\":\"upstream broke\"}\n\n", true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// 流内错误不应中断整个请求的 SSE 输出（仍有 event:error + [DONE]）。
	body := rec.Body.String()
	if !strings.Contains(body, "solo error") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body=%q want error event + [DONE]", body)
	}
	st, _ := p.Status("u1")
	if st.ErrCount == 0 {
		t.Errorf("stream error should bump errCount: %+v", st)
	}
}

func TestChatAllUnavailableReturns503(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 429, `rate limited`, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	var e map[string]any
	json.Unmarshal(rec.Body.Bytes(), &e)
	if e["error"] == nil {
		t.Errorf("want error envelope: %s", rec.Body)
	}
}

func TestChatSessionDeadDisables(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 401, `{"code":1001,"msg":"login required"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("code=%d", rec.Code)
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("account should be disabled: %+v", st)
	}
}

func TestChatUnknownModel400(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: upstream.New()})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"does-not-exist-xyz","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	var e map[string]any
	json.Unmarshal(rec.Body.Bytes(), &e)
	if e["error"] == nil {
		t.Errorf("want error envelope: %s", rec.Body)
	}
}

func TestModelsEndpoint(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: upstream.New()})
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["object"] != "list" {
		t.Errorf("object=%v", resp["object"])
	}
	data := resp["data"].([]any)
	if len(data) != 21 {
		t.Errorf("models count=%d want 21 (static table with internal entries filtered)", len(data))
	}
	found := false
	for _, m := range data {
		id := m.(map[string]any)["id"].(string)
		if id == "glm-5.2" {
			found = true
		}
		if isInternalModel(id, false) {
			t.Errorf("internal model %q must not be listed", id)
		}
	}
	if !found {
		t.Error("glm-5.2 missing")
	}
}

// TestModelsInternalEntriesFiltered 回归：静态回退表里的内部配置
// （custom_model_* / *subagent* / summary）不得出现在 /v1/models。
func TestModelsInternalEntriesFiltered(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	banned := map[string]bool{
		"custom_model_placeholder": true, "custom_model_claude": true, "custom_model_gpt-6": true,
		"browser_use_subagent": true, "computer_use_subagent": true, "explore_sub_agent_v2": true,
		"file_search_agent": true, "summary": true,
	}
	for _, m := range resp["data"].([]any) {
		id := m.(map[string]any)["id"].(string)
		if banned[id] {
			t.Errorf("internal model %q leaked into /v1/models", id)
		}
	}
}

// TestModelsShowInternalModels 开关打开时应返回全量 32 条静态表。
func TestModelsShowInternalModels(t *testing.T) {
	h := NewHandler(Config{
		Pool:               testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:           upstream.New(),
		ShowInternalModels: true,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp["data"].([]any)) != 40 {
		t.Errorf("models count=%d want 40 with ShowInternalModels", len(resp["data"].([]any)))
	}
}

// TestModelsDisplayName 动态结果应带 display_name（便于与 IDE 界面对照），
// 同时验证内部条目在动态路径上同样被过滤。
func TestModelsDisplayName(t *testing.T) {
	reset := func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = nil
		dynamicModelsCache.fetched = time.Time{}
		dynamicModelsCache.lastFail = time.Time{}
		dynamicModelsCache.Unlock()
	}
	reset()
	defer reset()

	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"config_info_list":[
					{"config_name":"glm-5.3","display_config":{"display_name":"GLM-5.3"}},
					{"config_name":"custom_model_x","display_config":{"display_name":"Custom"}},
					{"config_name":"summary","display_config":{"display_name":""}}
				]}`)),
			}, nil
		})},
		AgentHost: "https://fake.example",
		UgHost:    "https://fake.example",
		OAuthHost: "https://fake.example",
		ClientID:  upstream.ClientID,
	}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Data) != 1 {
		t.Fatalf("models=%d want 1 (custom_/summary filtered on dynamic path)", len(resp.Data))
	}
	if resp.Data[0]["id"] != "glm-5.3" {
		t.Errorf("id=%v", resp.Data[0]["id"])
	}
	if resp.Data[0]["display_name"] != "GLM-5.3" {
		t.Errorf("display_name=%v want GLM-5.3", resp.Data[0]["display_name"])
	}
}

// TestIsInternalModel 过滤谓词的边界。
func TestIsInternalModel(t *testing.T) {
	cases := []struct {
		id       string
		isCustom bool
		want     bool
	}{
		{"glm-5.2", false, false},
		{"glm-5-turbo", false, false},
		{"Doubao-Seed-2.1-Pro", false, false},
		{"", false, true},                       // 空名
		{"custom_model_claude", false, true},    // 前缀兜底
		{"claude-sonnet", true, true},           // is_custom_model 标记
		{"browser_use_subagent", false, true},   // 子代理
		{"Explore_Sub_Agent_v13", false, true},  // 子代理（大小写不敏感）
		{"computer_use_subagent", false, true},  // 子代理
		{"file_search_agent", false, true},      // 检索代理（_agent 后缀）
		{"summary", false, true},                // 摘要器
		{"fast_apply", false, true},             // 工具配置
		{"fast_apply_new", false, true},         // 工具配置
		{"input_optimization", false, true},     // 工具配置
		{"title_generation", false, true},       // 工具配置
		{"custom_claude-sonnet-4", false, true}, // custom_ 前缀（custom_model_ 之外）
		{"custom_gemini-2.5-pro", false, true},  // 同上
		{"summarize-pro", false, false},         // summary 是整名匹配，前缀不算
		{"sagitta", false, false},               // 真实模型
		{"qwen3.8-max", false, false},           // 真实模型
	}
	for _, c := range cases {
		if got := isInternalModel(c.id, c.isCustom); got != c.want {
			t.Errorf("isInternalModel(%q,%v)=%v want %v", c.id, c.isCustom, got, c.want)
		}
	}
}

func TestAPIKeyAuth(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "test-key",
	})
	// 无 key → 401
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("no key: code=%d", rec.Code)
	}
	// 错 key → 401
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("wrong key: code=%d", rec.Code)
	}
	// 对 key → 200（models）
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("right key: code=%d", rec.Code)
	}
}

func TestRequestBodyTooLarge(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: upstream.New()})
	big := strings.Repeat("a", maxBodyBytes+1)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(big))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	var e map[string]any
	json.Unmarshal(rec.Body.Bytes(), &e)
	if e["error"] == nil {
		t.Errorf("want error envelope: %s", rec.Body)
	}
}

func TestAPIKeyAuthCaseInsensitivePrefix(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "test-key",
	})
	// 大小写不同的 Bearer 前缀也应接受（按规范，前缀大小写不敏感）。
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "bearer test-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("lowercase bearer: code=%d", rec.Code)
	}
	// 错误 key 仍拒绝。
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("wrong key: code=%d", rec.Code)
	}
}

func TestStatusEndpoint(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 42)
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	req := httptest.NewRequest("GET", "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"uid":"u1"`) || !strings.Contains(body, `"credits":42`) {
		t.Errorf("body=%s", body)
	}
	if strings.Contains(body, "AccessToken") || strings.Contains(body, `"at"`) {
		t.Error("token leaked in status output")
	}
}

func TestHealthz(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Errorf("code=%d", rec.Code)
	}
}

// fakeCatalogUpstream 返回 batch_get_detail_param 目录 + 按记录的 function
// 调用序列应答的 fake upstream。catalog 提供模型归属；answer 按「function →
// 应答」给定，未给的 function 返回 4001 wrongFunction 流。
func fakeCatalogUpstream(t *testing.T, catalogFn string, answer map[string]func() (int, string, bool), calls *[]string) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			isStream := strings.HasSuffix(r.URL.Path, "/llm_utils_chat")
			if !isStream {
				// 模型目录（batch_get_detail_param）
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{"function_configs":[{"function":"` + catalogFn +
						`","config_info_list":[{"config_name":"glm-5.2","usage":"chat_completion"}]}]}`)),
				}, nil
			}
			*calls = append(*calls, r.Header.Get("X-Function"))
			if r.Header.Get("X-Function") == "" {
				// headers.go 不发 function 头；按请求体里的 function 区分
				*calls = append((*calls)[:len(*calls)-1], "body:"+bodyFunction(r))
			}
			fn := bodyFunction(r)
			if a, ok := answer[fn]; ok {
				status, body, stream := a()
				ct := "application/json"
				if stream {
					ct = "text/event-stream"
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{ct}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			}
			// 未给定：wrongFunction 流内错误（该 function 不收这个模型）
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("event:error\ndata:{\"code\":4001,\"message\":\"wrong function\"}\n\n")),
			}, nil
		})},
		AgentHost: "https://fake.example",
		UgHost:    "https://fake.example",
		OAuthHost: "https://fake.example",
		ClientID:  upstream.ClientID,
	}
}

// bodyFunction 从请求体提取 function 字段。
func bodyFunction(r *http.Request) string {
	b, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(strings.NewReader(string(b)))
	var peek struct {
		Function string `json:"function"`
	}
	json.Unmarshal(b, &peek)
	return peek.Function
}

// TestChatRotatesFunctionInCatalogOrder 回归（插件 fns 口径）：目录归属的
// function 被拒后，按 花名册顺序 继续试未试过的，而不是在两个 function 间
// 乒乓——solo_coder 独有的模型也轮得到。
func TestChatRotatesFunctionInCatalogOrder(t *testing.T) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	var calls []string
	up := fakeCatalogUpstream(t, "solo_agent", map[string]func() (int, string, bool){
		// chat_v3 / solo_agent 拒 4001；solo_work_lite 成功——验证顺序
		// [solo_agent, solo_work_lite]，而不是先试 solo_work_lite 再回 solo_agent。
		"solo_work_lite": func() (int, string, bool) { return 200, soloSSE, true },
	}, &calls)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	want := []string{"body:solo_agent", "body:solo_work_lite"}
	if len(calls) != len(want) {
		t.Fatalf("calls=%v want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls=%v want %v", calls, want)
		}
	}
}

// TestChatPlanCooldownAfterFunctionsTried 回归（插件 1005 口径）：流内 1005
// 先按顺序换 function 重试同账号；试尽后才按 plan 冷却换号——而不是一上来
// 就 12h 冷却。
func TestChatPlanCooldownAfterFunctionsTried(t *testing.T) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	var calls []string
	planErr := func() (int, string, bool) {
		return 200, "event:error\ndata:{\"code\":1005,\"message\":\"plan limit\",\"extra\":{\"plan\":2}}\n\n", true
	}
	up := fakeCatalogUpstream(t, "", map[string]func() (int, string, bool){
		"solo_work_lite": planErr, "chat_v3": planErr, "solo_agent": planErr, "solo_agent_lite": planErr, "solo_coder": planErr,
	}, &calls)
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	p.SetCredits("u1", 2000)
	p.SetCredits("u2", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up, PlanCooldown: time.Hour})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// u1 的 5 个 function 全被 1005 拒后冷却，u2 接手；u2 同样 5 个全拒 → 503。
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if len(calls) != 10 { // 5 functions × 2 accounts
		t.Fatalf("calls=%d (%v) want 10", len(calls), calls)
	}
	st, _ := p.Status("u1")
	if !st.Cooling || st.Reason != "plan 权益不足" {
		t.Errorf("u1 应 plan 冷却: %+v", st)
	}
}

// maxCatalogJSON 带最大窗口细目与 reasoning 等级的目录（glm-5.2 归属 catalogFn）。
func maxCatalogJSON(catalogFn string) string {
	return `{"function_configs":[{"function":"` + catalogFn + `","config_info_list":[` +
		`{"config_name":"glm-5.2","usage":"chat_completion",` +
		`"reasoning_effort_config":{"options":["light","extra_high"]},` +
		`"model_detail_list":[{"model_name":"glm-5.2__dev","max_tokens":8192}],` +
		`"context_window_tokens":{"dev":131072,"max":262144}},` +
		`{"config_name":"glm-5.2","usage":"chat_completion",` +
		`"model_detail_list":[{"model_name":"glm-5.2__max","max_tokens":32768}],` +
		`"context_window_tokens":{"dev":131072,"max":262144}}]}]}`
}

// fakeChatCapture 按记录聊天请求体应答：answer(fn) 给出应答，未给的
// function 回 4001 wrongFunction。
func fakeChatCapture(t *testing.T, catalogJSON string, answer map[string]func() (int, string, bool), bodies *[]map[string]any) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if !strings.HasSuffix(r.URL.Path, "/llm_utils_chat") {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(catalogJSON))}, nil
			}
			b, _ := io.ReadAll(r.Body)
			var doc map[string]any
			_ = json.Unmarshal(b, &doc)
			*bodies = append(*bodies, doc)
			fn, _ := doc["function"].(string)
			if a, ok := answer[fn]; ok {
				status, body, stream := a()
				ct := "application/json"
				if stream {
					ct = "text/event-stream"
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{ct}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event:error\ndata:{\"code\":4001,\"message\":\"wrong function\"}\n\n"))}, nil
		})},
		AgentHost: "https://fake.example",
		UgHost:    "https://fake.example",
		OAuthHost: "https://fake.example",
		ClientID:  upstream.ClientID,
	}
}

func resetModelCache(t *testing.T) {
	t.Helper()
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
}

// TestChatMaxRequestsUseBaseConfigName Max 语义（插件 chatBody/named 口径）：
// 点名 <id>-max → config_name 用 base、model_name 用 __max、恒设 max_tokens、
// prompt_max_tokens = 窗口−max_tokens，effort 按 base 条目翻译；点名 base →
// 绝不带 model_name/prompt_max_tokens（目录挂了 __max 细目也不进 Max 模式）。
func TestChatMaxRequestsUseBaseConfigName(t *testing.T) {
	resetModelCache(t)
	var bodies []map[string]any
	ok := func() (int, string, bool) { return 200, soloSSE, true }
	up := fakeChatCapture(t, maxCatalogJSON("chat_v3"), map[string]func() (int, string, bool){"chat_v3": ok}, &bodies)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	// -max 请求
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2-max","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"xhigh"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("max request code=%d body=%s", rec.Code, rec.Body)
	}
	b := bodies[len(bodies)-1]
	if b["config_name"] != "glm-5.2" || b["model"] != "glm-5.2" {
		t.Errorf("max request config_name=%v want base glm-5.2", b["config_name"])
	}
	if b["model_name"] != "glm-5.2__max" {
		t.Errorf("model_name=%v want glm-5.2__max", b["model_name"])
	}
	if mt, _ := b["max_tokens"].(float64); mt != 32768 {
		t.Errorf("max_tokens=%v want 32768", b["max_tokens"])
	}
	if umc, ok := b["user_message_context"].(map[string]any); ok {
		if mi, ok := umc["model_info"].(map[string]any); ok {
			if got, _ := mi["prompt_max_tokens"].(float64); got != 262144-32768 {
				t.Errorf("prompt_max_tokens=%v want %d", mi["prompt_max_tokens"], 262144-32768)
			}
		}
	} else {
		t.Error("max request missing user_message_context")
	}
	// effort 按 base 条目（light→low / extra_high→xhigh）翻译，-max 请求也生效
	if b["reasoning_effort"] != "extra_high" {
		t.Errorf("reasoning_effort=%v want extra_high", b["reasoning_effort"])
	}

	// base 请求：绝不进 Max 模式
	req2 := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("base request code=%d", rec2.Code)
	}
	b2 := bodies[len(bodies)-1]
	if _, has := b2["model_name"]; has {
		t.Errorf("base request must not carry model_name: %v", b2["model_name"])
	}
	if _, has := b2["user_message_context"]; has {
		t.Error("base request must not carry user_message_context")
	}
}

// TestChatKimiRewritesMoonshotToolSchema 验证 handler 把 Kimi 模型判定传到
// ChatOpts，且上游收到的是补过 type 的 Moonshot schema（插件 0.2.24）。
func TestChatKimiRewritesMoonshotToolSchema(t *testing.T) {
	resetModelCache(t)
	var bodies []map[string]any
	ok := func() (int, string, bool) { return 200, soloSSE, true }
	up := fakeChatCapture(t, "", map[string]func() (int, string, bool){
		"solo_work_lite": ok,
	}, &bodies)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{
		"model":"kimi-k3",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"edit","parameters":{
			"type":"object","properties":{"op":{"enum":["add","remove"]}}
		}}}]
	}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	b := bodies[len(bodies)-1]
	tools := b["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	params, isString := fn["parameters"].(string)
	if !isString {
		t.Fatalf("parameters should be string, got %T", fn["parameters"])
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(params), &schema); err != nil {
		t.Fatalf("unmarshal parameters: %v", err)
	}
	props := schema["properties"].(map[string]any)
	op := props["op"].(map[string]any)
	if op["type"] != "string" {
		t.Errorf("op.type=%v want string; schema=%s", op["type"], params)
	}
}

// TestChatReassignsFunctionAfterFallback 回归：回退成功后目录归属更新——
// 第二次请求直达成功的 function，不再先吃一个 4001（插件 listedBy.set）。
func TestChatReassignsFunctionAfterFallback(t *testing.T) {
	resetModelCache(t)
	var bodies []map[string]any
	ok := func() (int, string, bool) { return 200, soloSSE, true }
	up := fakeChatCapture(t, maxCatalogJSON("chat_v3"), map[string]func() (int, string, bool){"solo_work_lite": ok}, &bodies)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	mk := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := mk(); rec.Code != 200 {
		t.Fatalf("first request code=%d", rec.Code)
	}
	if len(bodies) != 2 { // chat_v3 拒 → solo_work_lite 成
		t.Fatalf("first pass calls=%d want 2", len(bodies))
	}
	if rec := mk(); rec.Code != 200 {
		t.Fatalf("second request code=%d", rec.Code)
	}
	if len(bodies) != 3 { // 归属已更新：直达 solo_work_lite
		t.Fatalf("second pass calls=%d want 3 (direct hit)", len(bodies))
	}
	if fn, _ := bodies[2]["function"].(string); fn != "solo_work_lite" {
		t.Fatalf("second pass function=%q want solo_work_lite", fn)
	}
}
