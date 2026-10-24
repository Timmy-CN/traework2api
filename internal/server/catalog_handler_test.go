// catalog_handler_test.go handler 侧目录行为测试：/v1/models 的多 function
// 合并与 <id>-max 条目、聊天 wrongFunction（4001/4023/1005）换 function 重试。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"traework2api/internal/auth"
	"traework2api/internal/upstream"
)

// setDynamicCache 预置动态模型缓存（绕过真实 FetchModels）。
func setDynamicCache(t *testing.T, infos []upstream.ModelInfo) {
	t.Helper()
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
	t.Cleanup(func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = nil
		dynamicModelsCache.fetched = time.Time{}
		dynamicModelsCache.lastFail = time.Time{}
		dynamicModelsCache.Unlock()
	})
}

// TestModelsMaxEntryListed 带 __max 的模型在 /v1/models 追加 <id>-max 条目，
// context 用 Max 窗口。
func TestModelsMaxEntryListed(t *testing.T) {
	setDynamicCache(t, []upstream.ModelInfo{
		{ID: "glm-5.3", Name: "GLM-5.3", ContextWindow: 131072, MaxTokens: 8192, InternalName: "glm-5.3__dev", Function: "chat_v3"},
		{ID: "glm-5.3-max", Name: "GLM-5.3 (Max)", ContextWindow: 262144, MaxTokens: 32768, InternalName: "glm-5.3__max", Function: "chat_v3", IsMax: true},
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	var maxEntry map[string]any
	for _, m := range resp.Data {
		if m["id"] == "glm-5.3-max" {
			maxEntry = m
		}
	}
	if maxEntry == nil {
		t.Fatalf("max entry missing: %v", resp.Data)
	}
	if maxEntry["context_length"] != float64(262144) {
		t.Errorf("max context=%v want 262144", maxEntry["context_length"])
	}
}

// TestChatWrongFunctionRetries 模型目录 function 被上游以 4001 拒（HTTP 200
// + event:error）时换其他 function 重试同账号一次，成功后目录归属更新。
func TestChatWrongFunctionRetries(t *testing.T) {
	var mu sync.Mutex
	var gotFunctions []string
	// behavior 拿不到请求体，直接自带读取 function 的 transport。
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			var body struct {
				Function string `json:"function"`
			}
			_ = json.Unmarshal(raw, &body)
			mu.Lock()
			gotFunctions = append(gotFunctions, body.Function)
			mu.Unlock()
			if body.Function == "chat_v3" {
				// HTTP 200 + 流内 wrongFunction 错误。
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader("event:error\ndata:{\"code\":4001,\"message\":\"wrong function\"}\n\n")),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(soloSSE)),
			}, nil
		})},
		AgentHost: "https://fake.example",
		UgHost:    "https://fake.example",
		OAuthHost: "https://fake.example",
		ClientID:  upstream.ClientID,
	}
	setDynamicCache(t, []upstream.ModelInfo{
		{ID: "glm-5.3", Name: "GLM-5.3", InternalName: "glm-5.3__dev", Function: "chat_v3"},
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.3","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gotFunctions) < 2 || gotFunctions[0] != "chat_v3" {
		t.Fatalf("functions=%v want chat_v3 first", gotFunctions)
	}
	if gotFunctions[1] == "chat_v3" {
		t.Errorf("second attempt should use another function: %v", gotFunctions)
	}
}

// TestChatWrongFunctionHTTPStatus wrongFunction 也可以是 HTTP 400 形态。
func TestChatWrongFunctionHTTPStatus(t *testing.T) {
	mu := &sync.Mutex{}
	var gotFunctions []string
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			var body struct {
				Function string `json:"function"`
			}
			_ = json.Unmarshal(raw, &body)
			mu.Lock()
			gotFunctions = append(gotFunctions, body.Function)
			mu.Unlock()
			if body.Function == "solo_work_lite" {
				return jsonStatus(400, `{"code":4023,"message":"model not in function"}`), nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(soloSSE)),
			}, nil
		})},
		AgentHost: "https://fake.example",
		UgHost:    "https://fake.example",
		OAuthHost: "https://fake.example",
		ClientID:  upstream.ClientID,
	}
	setDynamicCache(t, []upstream.ModelInfo{
		{ID: "glm-5.2", Function: "solo_work_lite"},
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gotFunctions) < 2 || gotFunctions[0] != "solo_work_lite" {
		t.Fatalf("functions=%v", gotFunctions)
	}
}

// TestChatModelWithoutCatalog 目录不可用（无缓存）→ 默认 function 一次成功。
func TestChatModelWithoutCatalog(t *testing.T) {
	setDynamicCache(t, nil)
	mu := &sync.Mutex{}
	var gotFunctions []string
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if !strings.HasSuffix(r.URL.Path, "/llm_utils_chat") {
				// 模型/签到等其他端点：给个空 JSON 免得干扰捕获。
				return jsonStatus(200, `{}`), nil
			}
			raw, _ := io.ReadAll(r.Body)
			var body struct {
				Function string `json:"function"`
			}
			_ = json.Unmarshal(raw, &body)
			mu.Lock()
			gotFunctions = append(gotFunctions, body.Function)
			mu.Unlock()
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(soloSSE)),
			}, nil
		})},
		AgentHost: "https://fake.example",
		UgHost:    "https://fake.example",
		OAuthHost: "https://fake.example",
		ClientID:  upstream.ClientID,
	}
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gotFunctions) != 1 || gotFunctions[0] != upstream.Function {
		t.Errorf("functions=%v want one %q", gotFunctions, upstream.Function)
	}
}

// jsonStatus 构造 JSON 错误响应（handler 测试用）。
func jsonStatus(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
