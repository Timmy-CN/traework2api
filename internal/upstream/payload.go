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
	return PrepareBodyFn(src, "", session, who)
}

// ChatOpts 是聊天请求体改写的目录依赖项（插件 chatBody 的 fn/modelName/most/
// max/sees/levels 形参）。
type ChatOpts struct {
	Function string // 本次请求的 function；空 → Function 默认
	Model    string // config_name / model（已映射好的请求模型 id）
	// Max 非 nil → Max 模式请求（<id>-max）：config_name/model 用 base id，
	// model_name 用 __max 内部名，max_tokens 恒设，
	// user_message_context.model_info.prompt_max_tokens = 窗口 − max_tokens。
	Max *MaxMode
	// Sees 模型是否收图（目录 multimodal）；nil = 目录没说 → 透传。
	Sees *bool
	// Levels 模型承载的 reasoning 等级（low/high/xhigh 口径）；按它翻译
	// 请求的 reasoning_effort/reasoningEffort，条目没说不发。
	Levels []string
	// Moonshot 模型是否 Kimi/Moonshot（插件 moonshotModel 口径）；为 true 时
	// tools[].function.parameters 按 Moonshot schema 规则改写。
	Moonshot bool
}

// MaxMode 一个模型的 Max 模式。
type MaxMode struct {
	Base   string // 实际发上游的 config_name（base 模型 id）
	Name   string // __max 内部模型名（请求体的 model_name）
	Most   int64  // __max 的 max_tokens
	Window int64  // Max 窗口（context_window_tokens.max）
}

// PrepareBodyFn 带 function 与目录信息的改写。
func PrepareBodyFn(src []byte, fn string, session, who string) []byte {
	model := configNameOf(src)
	return PrepareBodyOpts(src, ChatOpts{
		Function: fn,
		Model:    model,
		Moonshot: IsMoonshotModel(model),
	}, session, who)
}

// configNameOf 取请求体里已映射好的 model 字段。
func configNameOf(src []byte) string {
	var peek struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(src, &peek) != nil {
		return ""
	}
	return peek.Model
}

// PrepareBodyOpts 完整改写（插件 chatBody 口径）：
//
//	OpenAI: {model, messages, stream, tools, tool_choice, ...}
//	SOLO:   {messages, function, stream:true, config_name, model,
//	         [model_name], [reasoning_effort], [max_tokens],
//	         [user_message_context.model_info.prompt_max_tokens]}
func PrepareBodyOpts(src []byte, opts ChatOpts, session, who string) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	obj["function"] = opts.Function
	if obj["function"] == "" {
		obj["function"] = Function
	}

	// 消息整形（插件 traeMessages 口径）：同角色折叠、工具往返整形、无主
	// 结果回退 user 文本、空结果补占位；并派生 session_id（Trae 按它归档
	// 对话，避免一次多步请求被拆成多条无标题对话）。
	// sees 由目录按模型传入：仅目录显式 multimodal=false 的模型图片替换
	// 占位文本；目录没说或不可用按收图透传（插件 sight.get 缺省口径）。
	reshapeMessages(obj, opts.Sees == nil || *opts.Sees)
	msgs, _ := obj["messages"].([]any)
	if conv := conversationID(msgs, session, who); conv != "" {
		obj["session_id"] = conv
	} else {
		obj["session_id"] = newRequestUUID()
	}

	// config_name / model：Max 模式用 base id；model_name 用 __max 内部名。
	config := opts.Model
	if opts.Max != nil && opts.Max.Base != "" {
		config = opts.Max.Base
		if opts.Max.Name != "" {
			obj["model_name"] = opts.Max.Name
		}
	}
	if config == "" {
		config = DefaultConfigName
	}
	obj["config_name"] = config
	obj["model"] = config

	// reasoning_effort：按目录条目的 reasoning_effort_config 翻译
	// （low→light、high→high、xhigh→extra_high）；条目没说或等级不被承载
	// → 不发（请求带来的字段一并删掉）。
	if effort := effortOf(opts.Levels, reasoningEffortOf(obj)); effort != "" {
		obj[effortSend] = effort
	} else {
		delete(obj, effortSend)
	}
	delete(obj, "reasoningEffort")

	// max_tokens：Max 模式恒设（插件 3027-3043），否则透传请求值；
	// Max 模式补 user_message_context.model_info.prompt_max_tokens。
	most := int64(0)
	if opts.Max != nil {
		most = opts.Max.Most
	}
	asked := maxTokensOf(obj)
	if asked > 0 {
		if most > 0 && asked > most {
			asked = most
		}
		obj[maxTokenLimit] = asked
	} else if opts.Max != nil && most > 0 {
		obj[maxTokenLimit] = most
	}
	delete(obj, "max_completion_tokens")
	if opts.Max != nil && opts.Max.Window > 0 {
		window := opts.Max.Window
		mt, _ := obj[maxTokenLimit].(int64)
		if mt == 0 {
			if f, ok := obj[maxTokenLimit].(float64); ok {
				mt = int64(f)
			}
		}
		obj["user_message_context"] = map[string]any{
			"model_info": map[string]any{promptCap: window - mt},
		}
	}

	normalizeToolChoice(obj)
	moonshot := opts.Moonshot || IsMoonshotModel(opts.Model, configNameOf(src))
	normalizeTools(obj, moonshot)
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// reasoningEffortOf 取请求的 reasoning_effort / reasoningEffort（camelCase 兼容）。
func reasoningEffortOf(obj map[string]any) string {
	if s, ok := obj[effortSend].(string); ok {
		return s
	}
	s, _ := obj["reasoningEffort"].(string)
	return s
}

// maxTokensOf 取请求的 max_tokens / max_completion_tokens（>0 才有效）。
func maxTokensOf(obj map[string]any) int64 {
	for _, k := range []string{"max_tokens", "max_completion_tokens"} {
		switch v := obj[k].(type) {
		case float64:
			if v > 0 {
				return int64(v)
			}
		case int64:
			if v > 0 {
				return v
			}
		case int:
			if v > 0 {
				return int64(v)
			}
		}
	}
	return 0
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
// moonshot 为 true 时，先按插件 0.2.24 的 Moonshot schema 规则改写参数副本；
// 请求自身的 tools 对象保持不变。
func normalizeTools(obj map[string]any, moonshot bool) {
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
				if moonshot {
					paramsMap = cloneJSONMap(paramsMap)
					moonshotSchema(paramsMap)
				}
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
