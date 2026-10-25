package upstream

// traeMessages 迁移（插件 index.mjs traeMessages/merged 口径）：
// Trae 不接受同角色连续两条消息，且工具调用的往返需要按它的形状整形。
//
// 规则：
//  1. 无 tools 声明（native=false）时，assistant 的 tool_calls 以文本协议
//     `<tool_call>{json}</tool_call>` 折进正文（上游历史行为）；
//  2. 有 tools 声明时，tool_calls 回传为 Trae 原生 turn（function_call），
//     无 name 的调用连同其结果整组丢弃；
//  3. 无主/无调用可答的 tool 结果回退为 user 文本；空结果补
//     "(no output)"（Trae 4027：空结果读作未应答的调用）；
//  4. 工具结果里的图片等待整轮结果结束后以一条 user 消息发出
//     （部分模型读不到 tool turn 里的 image_url）；
//  5. 同角色连续消息折叠（assistant 的 text/calls 并入其后继，user 并入
//     前一同角色 turn）；纯空消息丢弃。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

const (
	openToolCall  = "<tool_call>"
	closeToolCall = "</tool_call>"
	// SESSION_HEADER 是客户端命名会话的请求头（插件 x-magpie-trae-session 同语义）。
	SESSION_HEADER          = "X-Session-Id"
	SESSION_HEADER_ALT      = "X-Magpie-Trae-Session"
	noOutputText            = "(no output)"
	imageFollowText         = "(the result's image follows)"
	visionPlaceholderSuffix = "[an image, which this model can't see]"
)

// sessionSalt 每进程随机（插件 SALT 口径）：派生出的 session_id 只在一个
// 进程生命周期内稳定，跨进程/重启换新——Trae 自己的 session id 也是一次
// 运行一份，重启换新只影响归档分段，不影响功能。
var (
	sessionSaltOnce sync.Once
	sessionSalt     string
)

func sessionSaltValue() string {
	sessionSaltOnce.Do(func() {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		sessionSalt = hex.EncodeToString(b)
	})
	return sessionSalt
}

// uuidFromSession 由派生串的 sha256 前 16 字节造一个 v4 UUID 形状的 id
// （session_id 此前承载的就是 randomUUID 的形状）。
func uuidFromSession(s string) string {
	sum := sha256.Sum256([]byte(sessionSaltValue() + "|" + s))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// conversationID 返回一次聊天所属对话的 session_id：
//   - 客户端以 X-Session-Id（或 X-Magpie-Trae-Session）点名会话 → 由会话名派生；
//   - 否则取第一条 user 消息的文本派生（后续每轮都重复携带它）；
//   - 两者皆无（非聊天体、无 user 消息）→ ""，调用方逐请求造新 id——每次
//     请求一个新对话正是要消除的碎片化。
//
// who 是本次请求的账号身份（uid，缺则昵称）：同一段对话被两个账号应答
// （轮转/failover）时按两个对话记，防止 Trae 把一个账号的对话读成另一个的。
func conversationID(msgs []any, session, who string) string {
	account := strings.TrimSpace(who)
	if session != "" {
		return uuidFromSession("magpie-trae-session|" + account + "|" + session)
	}
	for _, mi := range msgs {
		m, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role != "user" {
			continue
		}
		if text := strings.TrimSpace(partText(m["content"])); text != "" {
			return uuidFromSession("magpie-trae-first|" + account + "|" + text)
		}
	}
	return ""
}

// partText 提取 content 的文本：字符串原样；数组拼接 text 部分（含
// input_text），image 部分以占位文本示意。
func partText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		parts := make([]string, 0, len(c))
		for _, p := range c {
			switch pm := p.(type) {
			case string:
				parts = append(parts, pm)
			case map[string]any:
				switch t, _ := pm["type"].(string); t {
				case "text", "input_text":
					if s, _ := pm["text"].(string); s != "" {
						parts = append(parts, s)
					}
				case "image_url", "input_image":
					parts = append(parts, visionPlaceholderSuffix)
				}
			}
		}
		out := make([]string, 0, len(parts))
		for _, s := range parts {
			if s != "" {
				out = append(out, s)
			}
		}
		return strings.Join(out, "\n")
	default:
		if content == nil {
			return ""
		}
		return strings.TrimSpace(plainString(content))
	}
}

func plainString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// contentParts 把 content 拆成文本与 image_url/image 部分（数组原样解析，
// 字符串只有文本）。
func contentParts(content any) (text string, images []any) {
	switch c := content.(type) {
	case string:
		return c, nil
	case []any:
		var texts []string
		for _, p := range c {
			switch pm := p.(type) {
			case string:
				texts = append(texts, pm)
				continue
			}
			pm, ok := p.(map[string]any)
			if !ok {
				texts = append(texts, plainString(p))
				continue
			}
			switch t, _ := pm["type"].(string); t {
			case "text", "input_text":
				if s, _ := pm["text"].(string); s != "" {
					texts = append(texts, s)
				}
			case "image_url", "input_image":
				images = append(images, pm)
			default:
				texts = append(texts, plainString(pm))
			}
		}
		out := make([]string, 0, len(texts))
		for _, s := range texts {
			if s != "" {
				out = append(out, s)
			}
		}
		return strings.Join(out, "\n"), images
	default:
		if content != nil {
			return plainString(content), nil
		}
		return "", nil
	}
}

// reshapedTurn 是整形过程中的中间消息形态。
type reshapedTurn struct {
	role   string // assistant | tool | user | system
	text   string
	images []any
	calls  []any  // SOLO function_call 对象（assistant 专用）
	callID string // tool 专用
}

// reshapeMessages 把 OpenAI messages 整形为 Trae llm_utils_chat 的形状。
// native 表示请求是否向 Trae 声明了 tools（声明了才用原生工具往返）。
// sees 表示模型是否收图（目录 multimodal 口径）：不收图的模型图片替换占位
// 文本（"[an image, which this model can't see]"），收图或目录没说才透传。
func reshapeMessages(obj map[string]any, sees bool) {
	rawMsgs, _ := obj["messages"].([]any)

	native := false
	if tools, ok := obj["tools"].([]any); ok && len(tools) > 0 {
		if choice, ok := obj["tool_choice"].(string); !ok || choice != "none" {
			native = true
		} else if cm, ok := obj["tool_choice"].(map[string]any); ok {
			if t, _ := cm["type"].(string); t != "none" {
				native = true
			}
		}
	}

	names := map[string]string{} // call id -> 工具名
	dropped := map[string]bool{} // 无法发送的调用 id（其结果整组带走）
	out := make([]reshapedTurn, 0, len(rawMsgs))
	pendingImages := []any{}
	flushImages := func() {
		if len(pendingImages) > 0 {
			out = append(out, reshapedTurn{role: "user", images: pendingImages})
			pendingImages = []any{}
		}
	}

	for _, mi := range rawMsgs {
		m, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role == "developer" {
			role = "system"
		}
		text, images := contentParts(m["content"])
		if !sees && len(images) > 0 {
			// 模型不收图（目录 multimodal=false）：图片换成占位文本。
			for range images {
				text = joinNonEmpty(text, visionPlaceholderSuffix)
			}
			images = nil
		}

		tcs, _ := m["tool_calls"].([]any)
		if role == "assistant" && len(tcs) > 0 {
			if !native {
				// 无 tools 声明：调用写成文本协议块（历史行为）。
				lines := make([]string, 0, len(tcs)+1)
				if text != "" {
					lines = append(lines, text)
				}
				for _, tci := range tcs {
					tc, ok := tci.(map[string]any)
					if !ok {
						continue
					}
					fn, _ := tc["function"].(map[string]any)
					callName, _ := fn["name"].(string)
					if tcID, ok := tc["id"].(string); ok && tcID != "" {
						names[tcID] = callName
					}
					args := any("{}")
					if fn != nil {
						if a, ok := fn["arguments"]; ok && a != nil {
							args = a
						}
					}
					if as, ok := args.(string); ok && as != "" {
						var parsed any
						if json.Unmarshal([]byte(as), &parsed) == nil {
							args = parsed
						}
					}
					line := openToolCall + plainString(map[string]any{"name": callName, "arguments": args}) + closeToolCall
					lines = append(lines, line)
				}
				out = append(out, reshapedTurn{role: "assistant", text: strings.Join(lines, "\n"), images: images})
				continue
			}
			// 原生：tool_calls 回传为 Trae 的 assistant 调用 turn。
			calls := make([]any, 0, len(tcs))
			for _, tci := range tcs {
				tc, ok := tci.(map[string]any)
				if !ok {
					continue
				}
				fn, _ := tc["function"].(map[string]any)
				callName := ""
				if fn != nil {
					callName, _ = fn["name"].(string)
				}
				if callName == "" {
					if rawName, ok := tc["name"].(string); ok {
						callName = rawName
					}
				}
				callName = strings.TrimSpace(callName)
				if callName == "" {
					// 无法发送的调用：其结果整组带走。
					if tcID, ok := tc["id"].(string); ok && tcID != "" {
						dropped[tcID] = true
					}
					continue
				}
				callID := ""
				if tcID, ok := tc["id"].(string); ok {
					callID = tcID
				}
				if callID == "" {
					callID = "call_" + itoa(len(names)+1)
				}
				names[callID] = callName
				args := any("{}")
				if fn != nil {
					if a, ok := fn["arguments"]; ok && a != nil {
						args = a
					}
				} else if a, ok := tc["arguments"]; ok && a != nil {
					args = a
				}
				if as, ok := args.(string); !ok || as == "" {
					args = "{}"
				}
				calls = append(calls, map[string]any{
					"id":            callID,
					"type":          "function",
					"function_call": map[string]any{"name": callName, "arguments": args},
				})
			}
			if len(calls) > 0 {
				out = append(out, reshapedTurn{role: "assistant", text: text, images: images, calls: calls})
			} else if text != "" || len(images) > 0 {
				out = append(out, reshapedTurn{role: "assistant", text: text, images: images})
			}
			continue
		}
		if role == "assistant" {
			flushImages()
			out = append(out, reshapedTurn{role: "assistant", text: text, images: images})
			continue
		}
		if role == "tool" {
			id, _ := m["tool_call_id"].(string)
			if dropped[id] {
				continue
			}
			// 可答性只看本次调用表 names（插件 names.has 同口径）；m.name
			// 只用于无主结果回退文本里的工具名展示。
			callName := names[id]
			// 无主结果（text 形态的历史，或调用被裁掉）：不是 Trae 接受的
			// turn，回退为 user 文本。
			if !native || callName == "" {
				flushImages()
				out = append(out, reshapedTurn{
					role: "user",
					text: fmt.Sprintf("Result of %s (call %s):\n%s",
						orDefault(callName, m["name"], "the tool"), orDefault(id, "", "?"), text),
					images: images,
				})
				continue
			}
			// 工具结果里的图片等整轮结果结束（Trae 不允许 result 之间插消息）。
			if len(images) > 0 {
				pendingImages = append(pendingImages, images...)
			}
			// 空结果补说明（Trae 4027：空结果读作未应答的调用，plugins#251）。
			body := text
			if body == "" {
				if len(images) > 0 {
					body = imageFollowText
				} else {
					body = noOutputText
				}
			}
			out = append(out, reshapedTurn{role: "tool", callID: id, text: body})
			continue
		}
		flushImages()
		if role != "system" && role != "user" {
			role = "user"
		}
		out = append(out, reshapedTurn{role: role, text: text, images: images})
	}
	flushImages()

	// 合并：Trae 不接受同角色连续两条。assistant turn 折进其后继（text 并入
	// 它引出的调用 turn、并行调用并成一列），tool 结果各自成行。
	merged := make([]reshapedTurn, 0, len(out))
	for _, m := range out {
		if m.role == "tool" {
			merged = append(merged, m)
			continue
		}
		if m.role == "assistant" {
			if last := lastTurn(merged); last != nil && last.role == "assistant" {
				last.text = joinNonEmpty(last.text, m.text)
				last.images = append(last.images, m.images...)
				last.calls = append(last.calls, m.calls...)
				if last.text == "" && len(last.images) == 0 && len(last.calls) == 0 {
					merged = merged[:len(merged)-1]
				}
				continue
			}
			if m.text == "" && len(m.images) == 0 && len(m.calls) == 0 {
				continue
			}
			merged = append(merged, reshapedTurn{role: "assistant", text: m.text, images: m.images, calls: m.calls})
			continue
		}
		if m.text == "" && len(m.images) == 0 {
			continue
		}
		if last := lastTurn(merged); last != nil && last.role == m.role && m.role != "system" {
			last.text = joinNonEmpty(last.text, m.text)
			last.images = append(last.images, m.images...)
			continue
		}
		merged = append(merged, reshapedTurn{role: m.role, text: m.text, images: m.images})
	}

	// 输出为 SOLO 消息：content = [text, images...]；带调用的 assistant 在
	// 无正文时 content 为 null（上游接受）；tool 结果连续成列，中间不得插消息。
	msgs := make([]any, 0, len(merged))
	for _, m := range merged {
		content := []any{}
		if m.text != "" {
			content = append(content, map[string]any{"type": "text", "text": m.text})
		}
		content = append(content, m.images...)
		if m.role == "assistant" && len(m.calls) > 0 {
			var contentAny any = content
			if len(content) == 0 {
				contentAny = nil
			}
			msgs = append(msgs, map[string]any{"role": "assistant", "content": contentAny, "tool_calls": m.calls})
			continue
		}
		if m.role == "tool" {
			msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": m.callID, "content": content})
			continue
		}
		msgs = append(msgs, map[string]any{"role": m.role, "content": content})
	}
	obj["messages"] = msgs
}

func lastTurn(turns []reshapedTurn) *reshapedTurn {
	if len(turns) == 0 {
		return nil
	}
	return &turns[len(turns)-1]
}

func joinNonEmpty(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n\n")
}

func orDefault(v, alt any, fallback string) string {
	if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
		return s
	}
	if s, ok := alt.(string); ok && strings.TrimSpace(s) != "" {
		return s
	}
	return fallback
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
