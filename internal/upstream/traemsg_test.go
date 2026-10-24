package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// decodeMessages 解析 PrepareBody 产出的 messages，便于断言。
func decodeMessages(t *testing.T, src string) []map[string]any {
	t.Helper()
	out := PrepareBody([]byte(src), "", "tester")
	var obj struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("解析请求体失败: %v", err)
	}
	return obj.Messages
}

// 同角色连续 user 消息折叠为一条（Trae 拒绝同角色连续两条）。
func TestReshapeMergesConsecutiveUserMessages(t *testing.T) {
	msgs := decodeMessages(t, `{"model":"glm-5.2","messages":[
		{"role":"user","content":"第一段"},
		{"role":"user","content":"第二段"},
		{"role":"assistant","content":"回复"}]}`)
	if len(msgs) != 2 {
		t.Fatalf("应折叠为 2 条, got %d: %+v", len(msgs), msgs)
	}
	first, _ := msgs[0]["content"].([]any)
	if len(first) != 1 {
		t.Fatalf("合并后 content = %+v", first)
	}
	tp, _ := first[0].(map[string]any)
	if tp["text"] != "第一段\n\n第二段" {
		t.Fatalf("text = %v", tp["text"])
	}
}

// 连续 assistant turn（先文本后调用、并行调用拆两条）折叠为一个调用 turn。
func TestReshapeFoldsParallelAssistantCalls(t *testing.T) {
	msgs := decodeMessages(t, `{"model":"glm-5.2","tools":[{"type":"function","function":{"name":"get_weather"}}],"messages":[
		{"role":"assistant","content":"让我查一下"},
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]},
		{"role":"assistant","tool_calls":[{"id":"c2","type":"function","function":{"name":"get_time","arguments":"{}"}}]}]}`)
	if len(msgs) != 1 {
		t.Fatalf("两个 assistant turn 应折叠为一个调用 turn, got %d: %+v", len(msgs), msgs)
	}
	first := msgs[0]
	if first["role"] != "assistant" {
		t.Fatalf("首条应为 assistant: %+v", first)
	}
	calls, _ := first["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("两个调用应并入同一 turn: %+v", calls)
	}
	// id 取 model_parameters 优先级之外的 OpenAI 原值；SOLO 形状 function_call。
	c1, _ := calls[0].(map[string]any)
	fc, _ := c1["function_call"].(map[string]any)
	if fc["name"] != "get_weather" {
		t.Fatalf("function_call.name = %v", fc["name"])
	}
}

// 无 name 的调用连同其 tool 结果整组丢弃。
func TestReshapeDropsNamelessCallAndResult(t *testing.T) {
	msgs := decodeMessages(t, `{"model":"glm-5.2","tools":[{"type":"function","function":{"name":"get_weather"}}],"messages":[
		{"role":"user","content":"查天气"},
		{"role":"assistant","tool_calls":[{"id":"bad","type":"function","function":{"arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"bad","content":"结果"},
		{"role":"user","content":"继续"}]}`)
	if len(msgs) != 1 {
		t.Fatalf("无名调用与其结果应整组消失, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0]["role"] != "user" {
		t.Fatalf("应只剩 user: %+v", msgs)
	}
	if !strings.Contains(textOf(t, msgs[0]), "查天气") || !strings.Contains(textOf(t, msgs[0]), "继续") {
		t.Fatalf("两条 user 文本都应保留: %+v", msgs[0])
	}
}

// 无主 tool 结果（回答的是历史里不存在的调用）回退为 user 文本。
func TestReshapeUnmatchedResultBecomesUserText(t *testing.T) {
	msgs := decodeMessages(t, `{"model":"glm-5.2","tools":[{"type":"function","function":{"name":"get_weather"}}],"messages":[
		{"role":"user","content":"hi"},
		{"role":"tool","tool_call_id":"ghost","name":"get_weather","content":"42 度"}]}`)
	// 无主结果回退为 user 文本后，与前面相邻的 user 消息折叠成一条（同角色
	// 合并口径）。
	if len(msgs) != 1 {
		t.Fatalf("应折叠为 1 条, got %d: %+v", len(msgs), msgs)
	}
	if !strings.Contains(textOf(t, msgs[0]), "Result of get_weather (call ghost):") {
		t.Fatalf("应带工具名与调用 id: %+v", msgs[0])
	}
}

// 空 tool 结果补 "(no output)"（Trae 4027：空结果读作未应答的调用）。
func TestReshapePadsEmptyToolResult(t *testing.T) {
	msgs := decodeMessages(t, `{"model":"glm-5.2","tools":[{"type":"function","function":{"name":"get_weather"}}],"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":""}]}`)
	if len(msgs) != 3 {
		t.Fatalf("got %d: %+v", len(msgs), msgs)
	}
	if !strings.Contains(textOf(t, msgs[2]), "(no output)") {
		t.Fatalf("空结果应补占位: %+v", msgs[2])
	}
}

// 无 tools 声明（native=false）：assistant 的 tool_calls 折成文本协议块，
// 其结果回退 user 文本（历史行为）。
func TestReshapeNonNativeToolCallsBecomeText(t *testing.T) {
	msgs := decodeMessages(t, `{"model":"glm-5.2","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"晴天"}]}`)
	if len(msgs) != 3 {
		t.Fatalf("got %d: %+v", len(msgs), msgs)
	}
	block := textOf(t, msgs[1])
	if !strings.HasPrefix(block, "<tool_call>") || !strings.HasSuffix(block, "</tool_call>") {
		t.Fatalf("应折成文本协议块: %+v", block)
	}
	if !strings.Contains(block, `"name":"get_weather"`) || !strings.Contains(block, `"city":"北京"`) {
		t.Fatalf("文本块应含调用名与参数: %s", block)
	}
	if msgs[2]["role"] != "user" {
		t.Fatalf("native=false 时结果应回退 user 文本: %+v", msgs[2])
	}
	if !strings.Contains(textOf(t, msgs[2]), "Result of get_weather (call c1):") {
		t.Fatalf("回退文本应带工具名与调用 id: %+v", msgs[2])
	}
}

// 会话 id：同一 session 头派生稳定 id；who 参与派生（跨账号不串）；无会话头
// 时取首条 user 消息；两者皆无时逐请求新 id。
func TestReshapeSessionIDDeterministic(t *testing.T) {
	src := `{"model":"glm-5.2","messages":[{"role":"user","content":"会话首条"}]}`
	a := decodeSessionID(t, src, "sess-1", "uid-a")
	b := decodeSessionID(t, src, "sess-1", "uid-a")
	if a == "" || a != b {
		t.Fatalf("同会话应派生稳定 id: %q vs %q", a, b)
	}
	c := decodeSessionID(t, src, "sess-1", "uid-b")
	if c == a {
		t.Fatalf("不同账号应派生不同 id")
	}
	noHeader := decodeSessionID(t, src, "", "uid-a")
	if noHeader == "" || noHeader == a {
		t.Fatalf("无会话头应由首条 user 消息派生: %q", noHeader)
	}
	// 派生 id 是 v4 UUID 形状（8-4-4-4-12）。
	if len(a) != 36 || strings.Count(a, "-") != 4 {
		t.Fatalf("session_id 应为 UUID 形状: %q", a)
	}
}

func decodeSessionID(t *testing.T, src, session, who string) string {
	t.Helper()
	out := PrepareBody([]byte(src), session, who)
	var obj struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("解析 session_id: %v", err)
	}
	return obj.SessionID
}

func textOf(t *testing.T, msg map[string]any) string {
	t.Helper()
	switch c := msg["content"].(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			if pm, ok := p.(map[string]any); ok {
				if s, _ := pm["text"].(string); s != "" {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
