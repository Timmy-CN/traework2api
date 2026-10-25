// models_test.go 多 function 目录 / Max 模式 / reasoning levels / multimodal
// 门控测试（插件 index.mjs 移植口径）。
package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"traework2api/internal/auth"
)

// entry 构造一个 rawEntry 的 JSON（仅给非零字段）。
func entry(id string, fields map[string]any) string {
	m := map[string]any{"config_name": id}
	for k, v := range fields {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// batchBody 构造 batch_get_detail_param 的响应体。
func batchBody(groups map[string][]string) string {
	gs := make([]string, 0, len(groups))
	for fn, entries := range groups {
		gs = append(gs, `{"function":"`+fn+`","config_info_list":[`+strings.Join(entries, ",")+`]}`)
	}
	return `{"function_configs":[` + strings.Join(gs, ",") + `]}`
}

func testAuth() *auth.Auth {
	return &auth.Auth{AccessToken: "at", UID: "u1", MachineID: "m1", DeviceID: "d1"}
}

// TestBatchFirstFetchModels batch_get_detail_param 成功时不再逐 function 请求。
func TestBatchFirstFetchModels(t *testing.T) {
	var paths []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		if !strings.HasSuffix(r.URL.Path, EpBatchModels) {
			t.Errorf("expected batch endpoint first, got %s", r.URL.Path)
		}
		var req struct {
			Functions []string `json:"functions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Functions) != len(chatFunctions) {
			t.Errorf("functions=%v", req.Functions)
		}
		return jsonResp(200, batchBody(map[string][]string{
			"chat_v3": {entry("glm-5.3", map[string]any{
				"multimodal": true,
				"model_detail_list": []map[string]any{
					{"model_name": "glm-5.3__dev", "max_tokens": 8192},
				},
				"context_window_tokens": map[string]any{"dev": 131072},
			})},
			"solo_agent": {entry("kimi-k3", map[string]any{
				"multimodal": false,
				"model_detail_list": []map[string]any{
					{"model_name": "kimi-k3__dev", "max_tokens": 4096},
				},
			})},
		})), nil
	})
	infos, err := c.FetchModels(testAuth())
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(paths) != 1 {
		t.Errorf("requests=%d want 1 (batch only)", len(paths))
	}
	if len(infos) != 2 {
		t.Fatalf("infos=%d want 2", len(infos))
	}
	if infos[0].ID != "glm-5.3" || infos[0].Function != "chat_v3" || infos[0].InternalName != "glm-5.3__dev" || infos[0].MaxTokens != 8192 || infos[0].ContextWindow != 131072 {
		t.Errorf("glm entry: %+v", infos[0])
	}
	if infos[1].ID != "kimi-k3" || infos[1].Function != "solo_agent" {
		t.Errorf("kimi entry: %+v", infos[1])
	}
}

// TestPerFunctionFallbackBatchEmpty batch 无列表 → 逐 function get_detail_param；
// 同 id 首现者优先。
func TestPerFunctionFallbackBatchEmpty(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, EpBatchModels) {
			return jsonResp(200, `{"function_configs":[]}`), nil
		}
		var req struct {
			Function string `json:"function"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Function {
		case "chat_v3":
			return jsonResp(200, `{"config_info_list":[`+entry("glm-5.3", map[string]any{"display_name": "IDE's name"})+`]}`), nil
		case "solo_work_lite":
			return jsonResp(200, `{"config_info_list":[`+entry("glm-5.3", map[string]any{
				"display_name": "SOLO's name",
				"multimodal":   false,
			})+","+entry("solo-only-model", nil)+`]}`), nil
		case "solo_agent":
			return jsonResp(500, `oops`), nil // 一个失败不影响其他
		default:
			return jsonResp(200, `{"config_info_list":[]}`), nil
		}
	})
	infos, err := c.FetchModels(testAuth())
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("infos=%d want 2: %+v", len(infos), infos)
	}
	// 同 id 首现优先：chat_v3 的条目赢（取它的展示名），后续条目的字段不覆盖。
	if infos[0].Function != "chat_v3" || infos[0].Name != "IDE's name" {
		t.Errorf("first-wins violated: %+v", infos[0])
	}
	if infos[1].ID != "solo-only-model" || infos[1].Function != "solo_work_lite" {
		t.Errorf("solo entry: %+v", infos[1])
	}
}

// TestCatalogMaxEntryAppend __max 细目 + 更大窗口 → 追加 <id>-max。
func TestCatalogMaxEntryAppend(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, batchBody(map[string][]string{
			"chat_v3": {entry("deepseek-v4.1-flash", map[string]any{
				"model_detail_list": []map[string]any{
					{"model_name": "ds__dev", "max_tokens": 8192},
				},
				"context_window_tokens": map[string]any{"dev": 131072},
			})},
			"solo_agent": {entry("deepseek-v4.1-flash", map[string]any{
				"model_detail_list": []map[string]any{
					{"model_name": "ds__max", "max_tokens": 32768},
				},
				"context_window_tokens": map[string]any{"dev": 131072, "max": 262144},
			})},
		})), nil
	})
	infos, err := c.FetchModels(testAuth())
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("infos=%d want 2 (base + max): %+v", len(infos), infos)
	}
	max := infos[1]
	if max.ID != "deepseek-v4.1-flash-max" || !max.IsMax || max.InternalName != "ds__max" || max.MaxTokens != 32768 || max.ContextWindow != 262144 {
		t.Errorf("max entry: %+v", max)
	}
	if max.Function != "chat_v3" {
		t.Errorf("max should be listed by first function listing the base, got %q", max.Function)
	}
}

// TestCatalogNoMaxWhenWindowNotBigger __max 有但窗口不大于 dev → 不追加。
func TestCatalogNoMaxWhenWindowNotBigger(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, batchBody(map[string][]string{
			"chat_v3": {entry("m1", map[string]any{
				"model_detail_list": []map[string]any{
					{"model_name": "m1__dev", "max_tokens": 8192},
					{"model_name": "m1__max", "max_tokens": 8192},
				},
				"context_window_tokens": map[string]any{"dev": 262144, "max": 131072},
			})},
		})), nil
	})
	infos, err := c.FetchModels(testAuth())
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(infos) != 1 {
		t.Errorf("infos=%d want 1 (no max)", len(infos))
	}
}

// TestChatModelFilter 过滤：custom_model_ 前缀、usage 非 chat_completion、
// config_switch=false、is_invisible_to_user。
func TestChatModelFilter(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		no := false
		return jsonResp(200, batchBody(map[string][]string{
			"chat_v3": {
				entry("custom_model_gpt", nil),
				entry("summary", map[string]any{"usage": "summary"}),
				entry("off", map[string]any{"config_switch": no}),
				entry("hidden", map[string]any{"is_invisible_to_user": true}),
				entry("keep", nil),
			},
		})), nil
	})
	infos, err := c.FetchModels(testAuth())
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(infos) != 1 || infos[0].ID != "keep" {
		t.Errorf("infos=%+v want only keep", infos)
	}
}

// TestEffortOfLevels reasoning_effort_config → low/high/xhigh 映射与翻译。
func TestEffortOfLevels(t *testing.T) {
	e := entry("m1", map[string]any{
		"reasoning_effort_config": map[string]any{"options": []string{"light", "high", "extra_high"}},
	})
	var raw rawEntry
	if err := json.Unmarshal([]byte(e), &raw); err != nil {
		t.Fatal(err)
	}
	levels := raw.levelsOf()
	if len(levels) != 3 || levels[0] != "low" || levels[1] != "high" || levels[2] != "xhigh" {
		t.Errorf("levels=%v", levels)
	}
	if got := effortOf(levels, "low"); got != "light" {
		t.Errorf("low → %q want light", got)
	}
	if got := effortOf(levels, "xhigh"); got != "extra_high" {
		t.Errorf("xhigh → %q want extra_high", got)
	}
	if got := effortOf(levels, "high"); got != "high" {
		t.Errorf("high → %q", got)
	}
	if got := effortOf(levels, "bogus"); got != "" {
		t.Errorf("bogus → %q want empty", got)
	}
	// Trae 自己的拼写（客户端从列表抄的）也认。
	if got := effortOf(levels, "extra_high"); got != "extra_high" {
		t.Errorf("extra_high passthrough → %q", got)
	}
	// 条目没说 → 不发。
	var none rawEntry
	_ = json.Unmarshal([]byte(entry("m2", nil)), &none)
	if got := effortOf(none.levelsOf(), "high"); got != "" {
		t.Errorf("no config → %q want empty", got)
	}
}

// TestPrepareBodyOptsMaxMode Max 模式请求体：base id、model_name、恒设
// max_tokens、prompt_max_tokens = 窗口 − max_tokens。
func TestPrepareBodyOptsMaxMode(t *testing.T) {
	out := PrepareBodyOpts([]byte(`{"model":"glm-5.3-max","messages":[{"role":"user","content":"hi"}]}`), ChatOpts{
		Function: "chat_v3",
		Model:    "glm-5.3-max",
		Max:      &MaxMode{Base: "glm-5.3", Name: "glm-5.3__max", Most: 8192, Window: 262144},
		Levels:   []string{"low", "high"},
	}, "", "tester")
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if body["function"] != "chat_v3" {
		t.Errorf("function=%v", body["function"])
	}
	if body["config_name"] != "glm-5.3" || body["model"] != "glm-5.3" {
		t.Errorf("config=%v model=%v want base", body["config_name"], body["model"])
	}
	if body["model_name"] != "glm-5.3__max" {
		t.Errorf("model_name=%v", body["model_name"])
	}
	if mt, _ := body["max_tokens"].(float64); mt != 8192 {
		t.Errorf("max_tokens=%v want 8192 (恒设)", body["max_tokens"])
	}
	umc, ok := body["user_message_context"].(map[string]any)
	if !ok {
		t.Fatalf("no user_message_context: %v", body)
	}
	mi, _ := umc["model_info"].(map[string]any)
	if got, _ := mi["prompt_max_tokens"].(float64); got != 262144-8192 {
		t.Errorf("prompt_max_tokens=%v want %d", mi["prompt_max_tokens"], 262144-8192)
	}
}

// TestPrepareBodyOptsReasoningEffort 请求 effort 按目录翻译；条目没说不发。
func TestPrepareBodyOptsReasoningEffort(t *testing.T) {
	out := PrepareBodyOpts([]byte(`{"model":"m1","messages":[],"reasoning_effort":"xhigh"}`), ChatOpts{
		Function: "solo_work_lite", Model: "m1", Levels: []string{"low", "xhigh"},
	}, "", "")
	var body map[string]any
	_ = json.Unmarshal(out, &body)
	if body["reasoning_effort"] != "extra_high" {
		t.Errorf("reasoning_effort=%v want extra_high", body["reasoning_effort"])
	}
	// 无等级：不发。
	out = PrepareBodyOpts([]byte(`{"model":"m1","messages":[],"reasoning_effort":"high"}`), ChatOpts{
		Function: "solo_work_lite", Model: "m1", Levels: []string{},
	}, "", "")
	body = nil
	_ = json.Unmarshal(out, &body)
	if _, ok := body["reasoning_effort"]; ok {
		t.Errorf("effort should be dropped: %v", body["reasoning_effort"])
	}
}

// TestPrepareBodyOptsSeesGating 不收图的模型图片替换占位文本；收图透传。
func TestPrepareBodyOptsSeesGating(t *testing.T) {
	src := []byte(`{"model":"m1","messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}]}`)
	no := false
	yes := true
	out := PrepareBodyOpts(src, ChatOpts{Function: "solo_work_lite", Model: "m1", Sees: &no}, "", "")
	var body map[string]any
	_ = json.Unmarshal(out, &body)
	msgs := body["messages"].([]any)
	m := msgs[0].(map[string]any)
	content := m["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content=%v want text only", content)
	}
	if txt, _ := content[0].(map[string]any)["text"].(string); !strings.Contains(txt, visionPlaceholderSuffix) {
		t.Errorf("text=%q want placeholder", txt)
	}
	if strings.Contains(string(out), "data:image") {
		t.Errorf("image leaked to non-multimodal model: %s", out)
	}
	// 收图：image_url 保留。
	out = PrepareBodyOpts(src, ChatOpts{Function: "solo_work_lite", Model: "m1", Sees: &yes}, "", "")
	if !strings.Contains(string(out), "data:image") {
		t.Errorf("image dropped for multimodal model: %s", out)
	}
}

// TestWrongFunctionCodes 4001/4023/1005 判定。
func TestWrongFunctionCodes(t *testing.T) {
	for _, code := range []int64{4001, 4023, 1005} {
		if !WrongFunctionCode(code) {
			t.Errorf("%d should be wrongFunction", code)
		}
	}
	if WrongFunctionCode(4027) || WrongFunctionCode(0) {
		t.Error("other codes should not match")
	}
	if got := ErrorCodeOf([]byte(`{"error":{"code":4023,"message":"nope"}}`)); got != 4023 {
		t.Errorf("ErrorCodeOf=%d", got)
	}
	if got := ErrorCodeOf([]byte(`{"code":"4001"}`)); got != 4001 {
		t.Errorf("ErrorCodeOf string=%d", got)
	}
}

// TestChatStreamBodyCarriesFunction ChatStream 经 ChatOpts 携带 function。
func TestChatStreamBodyCarriesFunction(t *testing.T) {
	var gotBody []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("event:done\ndata:{\"finish_reason\":\"stop\"}\n\n")),
		}, nil
	})
	rc, _, _, err := c.ChatStream(testAuth(), []byte(`{"model":"glm-5.3","messages":[]}`), ChatOpts{Function: "chat_v3", Model: "glm-5.3"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if !strings.Contains(string(gotBody), `"function":"chat_v3"`) {
		t.Errorf("body=%s", gotBody)
	}
}
