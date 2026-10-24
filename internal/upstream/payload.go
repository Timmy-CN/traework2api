// payload.go OpenAI → SOLO llm_utils_chat 请求体改写。
package upstream

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// PrepareBody 单 pass 改写；无法解析时原样返回。
//
//	OpenAI: {model, messages, stream, tools, tool_choice, ...}
//	SOLO:   {messages, function:"solo_work_lite", stream:true,
//	         config_name:<model>, model:<model>}
//
// 改写规则（SPEC §4.4）：
//  1. messages: content 字符串 → [{"type":"text","text":...}]；已是数组 → 透传
//  2. stream: 强制 true（非流式由服务端聚合）
//  3. model → config_name + model
//  4. function: 固定 "solo_work_lite"
//  5. tools/tool_choice: 归一化（"none" 删 tools；auto/required 保留；function 提取 name）
func PrepareBody(src []byte, session, who string) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	obj["function"] = Function

	// 消息整形（插件 traeMessages 口径）：同角色折叠、工具往返整形、无主
	// 结果回退 user 文本、空结果补占位；并派生 session_id（Trae 按它归档
	// 对话，避免一次多步请求被拆成多条无标题对话）。
	reshapeMessages(obj)
	msgs, _ := obj["messages"].([]any)
	if conv := conversationID(msgs, session, who); conv != "" {
		obj["session_id"] = conv
	} else {
		obj["session_id"] = newRequestUUID()
	}

	model, _ := obj["model"].(string)
	model = strings.TrimSpace(model)
	if model == "" {
		model = DefaultConfigName
	}
	obj["config_name"] = model
	obj["model"] = model

	normalizeToolChoice(obj)
	normalizeTools(obj)
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// newRequestUUID 生成 v4 UUID 形状的逐请求 id（session_id 的兜底形态）。
func newRequestUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// DefaultConfigName 默认模型（glm-5.2，实测可用）。
const DefaultConfigName = "glm-5.2"

// normalizeToolChoice 按上游 Go struct（string 类型）改写 OpenAI tool_choice。
//   - "none" / {"type":"none"} → 删 tool_choice + 删 tools/functions
//   - {"type":"auto"/"required"} → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"
//   - 其他对象/非标量 → 删 tool_choice
func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// normalizeTools 把 OpenAI tools 转为 SOLO 上游格式。
// 实测上游 Go struct: FunctionDefinition.tools[].function.parameters 是 string 类型
// （OpenAI 标准是 object）→ 需把 parameters 对象序列化为 JSON 字符串。
// 同时 tools 条目若不是 map 或缺 function，整体剔除（避免上游反序列化失败）。
func normalizeTools(obj map[string]any) {
	raw, present := obj["tools"]
	if !present {
		return
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return
	}
	out := make([]any, 0, len(list))
	for _, item := range list {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		if params, ok := fn["parameters"]; ok {
			if paramsMap, isMap := params.(map[string]any); isMap {
				if s, err := json.Marshal(paramsMap); err == nil {
					fn["parameters"] = string(s)
				}
			}
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		delete(obj, "tools")
		return
	}
	obj["tools"] = out
}
