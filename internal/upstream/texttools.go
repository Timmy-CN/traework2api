// texttools.go 移植 magpie trae 插件的 TextTools（index.mjs:1299-2400）：把模型把
// 工具调用写成文本的五种协议读回为真正的调用，只对请求声明过的工具生效。
//
//   - <tool_call>{json}</tool_call>（含 <toolcall>、属性 opener <tool_call name="bash">）
//   - GLM 模板 <arg_key>/<arg_value>
//   - DeepSeek DSML <｜DSML｜invoke name="…">
//   - Anthropic <function_calls><invoke name="…">
//   - Qwen <function=…>
//
// 反引号/代码围栏里的一律当文本；截断（CUT_SHORT）或读不出（UNREADABLE）的块不当
// 作答案，返回错误供上层重试（plugins#38）。另有 finishOf 与 Thoughts 去重。
package upstream

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ToolProp 工具参数单个属性的模式（只关心 string 与否：string 类的值按原文收）。
type ToolProp struct {
	Type string `json:"type,omitempty"`
}

// ToolParams 工具参数模式。
type ToolParams struct {
	Properties map[string]ToolProp `json:"properties,omitempty"`
}

// ToolFunc OpenAI 风格的 {name, parameters}。
type ToolFunc struct {
	Name       string      `json:"name"`
	Parameters *ToolParams `json:"parameters,omitempty"`
}

// ToolDef 请求声明的一个工具：OpenAI 包装（{function:{...}}）或平的 {name, parameters}。
type ToolDef struct {
	Function   *ToolFunc   `json:"function,omitempty"`
	Name       string      `json:"name,omitempty"`
	Parameters *ToolParams `json:"parameters,omitempty"`
}

func (t ToolDef) fn() *ToolFunc {
	if t.Function != nil {
		return t.Function
	}
	return &ToolFunc{Name: t.Name, Parameters: t.Parameters}
}

// ParseToolDefs 把请求里 tools 数组的原始 JSON 读成 ToolDef（非数组或空 → nil）。
func ParseToolDefs(raw json.RawMessage) []ToolDef {
	var arr []ToolDef
	if len(raw) == 0 || json.Unmarshal(raw, &arr) != nil {
		return nil
	}
	return arr
}

// TextTools 错误：截断或读不出的文本调用不是答案（plugins#38），上层应以 502 重试。
type ToolTextError struct{ Msg string }

func (e *ToolTextError) Error() string { return e.Msg }

const (
	ttCutShort   = "the model's tool call, written as text, was cut short; it is not sent on as the answer"
	ttUnreadable = "the model's tool call, written as text, can't be read; it is not sent on as the answer"
)

// TTCall 读出的一次文本工具调用。
type TTCall struct {
	Name      string
	Arguments string // JSON 字符串
}

// TTResult Push 的产出。
type TTResult struct {
	Text      string
	Calls     []TTCall
	Reasoning string
	Error     string
}

// ttBlock block/xmlCall 的内部结果。
type ttBlock struct {
	wait bool
	text bool
	call TTCall
	rest string
	err  string
}

// TextTools 把模型的文本切成答案与写成的工具调用，未整块的先扣住。
type TextTools struct {
	buf   string
	after bool
	tools []ToolDef
	sent  string
	bad   map[string]bool
	mark  int
}

func NewTextTools(tools []ToolDef) *TextTools {
	return &TextTools{tools: tools, bad: map[string]bool{}}
}

var (
	reOpenAttr  = regexp.MustCompile(`<tool_?call(\s[^<>{}]*)?>`)
	reOpenJSON  = regexp.MustCompile(`<tool_?call(\s*\{)`)
	reOpenHold  = regexp.MustCompile(`<tool_?call(?:\s[^<>{}\n]{0,200})?$`)
	reStrayAt   = regexp.MustCompile(`[}\]"'>]*\s*</tool_?calls?>`)
	reStrayHold = regexp.MustCompile(`[}\]"'>][\s}\]"'>]*$`)
	reTailJunk  = regexp.MustCompile(`^[\s}\]"'>]*`)
	reReasonAt  = regexp.MustCompile(`\{\s*"reasoning_content"\s*:`)

	dsmlSrc        = `<\s*/?\s*[|｜][\s|｜]*DSML[\s|｜]*`
	reDSMLAt       = regexp.MustCompile(dsmlSrc)
	reDSMLTag      = regexp.MustCompile(`^` + dsmlSrc + `([^\s|｜<>][^<>]*)?>`)
	reDSMLInvoke   = regexp.MustCompile(`^<\s*[|｜][\s|｜]*DSML[\s|｜]*invoke\s+name\s*=\s*"([^"]+)"\s*>`)
	reDSMLInvokeAt = regexp.MustCompile(`<\s*[|｜][\s|｜]*DSML[\s|｜]*invoke\s+name\s*=`)
	reDSMLClose    = regexp.MustCompile(`<\s*/\s*[|｜][\s|｜]*DSML[\s|｜]*invoke\s*>`)
	reDSMLParam    = regexp.MustCompile(`<\s*[|｜][\s|｜]*DSML[\s|｜]*parameter\s+name\s*=\s*"([^"]+)"((?:\s+\w+\s*=\s*"[^"]*")*)\s*>([\s\S]*?)` + dsmlSrc + `parameter\s*>`)

	reXMLAt     = regexp.MustCompile(`<(?:antml:)?(?:function_calls>|invoke\s+name\s*=|function=)`)
	reXMLHold   = regexp.MustCompile(`<(?:antml:)?(?:invoke\s[^<>]{0,300}|function=[^<>]{0,300})$`)
	reXMLNext   = regexp.MustCompile(`<(?:antml:)?invoke\s+name\s*=|<function=`)
	reXMLInvoke = regexp.MustCompile(`^<(?:antml:)?invoke\s+name\s*=\s*"([^"<>]+)"\s*>`)
	reXMLFunc   = regexp.MustCompile(`^<function=([^\s<>]+?)\s*>`)

	reParamOpen = regexp.MustCompile(`<parameter(?:\s+name\s*=\s*"([^"]+)"|=([^\s>]+))\s*>`)
	reParamHead = regexp.MustCompile(`^\s*\{\s*"name"\s*:\s*"([^"]+)"\s*\}?\s*>?\s*(<parameter\b)`)
	reGLMArg    = regexp.MustCompile(`<arg_key>\s*([\s\S]*?)\s*</arg_key>\s*<arg_value>([\s\S]*?)</arg_value>`)

	reNameAttr   = regexp.MustCompile(`\bname\s*=\s*"([^"]+)"`)
	reGlmHead    = regexp.MustCompile(`^\s*[\w.:-]{1,128}\s*(?:<|$)`)
	reGlmErrHead = regexp.MustCompile(`^\s*[\w.:-]{1,128}\s*<arg_key>`)
	reFixEscapes = regexp.MustCompile(`\\(u[0-9a-fA-F]{4}|["\\/bfnrt])|\\'|\\`)
)

var ttMarks = []string{
	"<tool_call>", "</tool_call>", "<toolcall>", "</toolcall>",
	"<｜DSML｜", "<|DSML|", "</｜DSML｜", "</|DSML|",
	`{"reasoning_content"`,
	"<function_calls>", "</function_calls>", "<antml:function_calls>", "</antml:function_calls>",
	"<invoke ", "<antml:invoke ", "</invoke>", "</antml:invoke>",
	"<function=", "</function>",
}

var xmlStarts = []string{"<invoke ", "<antml:invoke ", "<function="}

var ttSeq int64

// ttCallID 生成 OpenAI 风格的 tool_call id。
func ttCallID() string {
	ttSeq++
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("call_%x%d", time.Now().UnixNano(), ttSeq)
	}
	return "call_" + hex.EncodeToString(b) + strconv.FormatInt(ttSeq, 36)
}

var ttWraps = []string{"arguments", "input", "parameters", "args"}

// findOpen 在 s 里找 <tool_call> opener：属性分支（…>）优先，其次 JSON 直随分支
// （opener 止于 \s*\{ 之前）。返回下标、opener 长度与属性串。
func findOpen(s string) (idx, length int, attrs string) {
	idx = -1
	if m := reOpenAttr.FindStringSubmatchIndex(s); m != nil {
		idx, length = m[0], m[1]-m[0]
		if m[2] >= 0 {
			attrs = s[m[2]:m[3]]
		}
	}
	if m := reOpenJSON.FindStringSubmatchIndex(s); m != nil && (idx < 0 || m[0] < idx) {
		idx = m[0]
		length = m[2] - m[0]
		attrs = ""
	}
	return
}

func reIndex(re *regexp.Regexp, s string) int {
	if m := re.FindStringIndex(s); m != nil {
		return m[0]
	}
	return -1
}

// leftoverLen LEFTOVER：一次调用之后剩下的空白、close、别人的收尾标签。
func leftoverLen(s string) int {
	i := 0
	for i < len(s) {
		n := 0
		for i+n < len(s) && isJSSpace(rune(s[i+n])) {
			n++
		}
		if n > 0 {
			i += n
			continue
		}
		rest := s[i:]
		if strings.HasPrefix(rest, "</tool_call>") || strings.HasPrefix(rest, "</toolcall>") ||
			strings.HasPrefix(rest, "<tool_calls>") || strings.HasPrefix(rest, "<toolcall>") {
			i += len("</tool_call>")
			continue
		}
		if strings.HasPrefix(rest, "</function_calls>") || strings.HasPrefix(rest, "</antml:function_calls>") ||
			strings.HasPrefix(rest, "</function>") {
			i += len("</function_calls>")
			continue
		}
		if strings.HasPrefix(rest, "</invoke>") || strings.HasPrefix(rest, "</antml:invoke>") ||
			strings.HasPrefix(rest, "</antml:function_calls") {
			i += len("</invoke>")
			continue
		}
		if m := reDSMLTag.FindStringSubmatch(s[i:]); m != nil && !isDSMLInvokeTail(m[1]) {
			i += len(m[0])
			continue
		}
		break
	}
	return i
}

// isDSMLInvokeTail DSML 标签的剩余是否是 invoke（下一个调用的开头，不能丢）。
func isDSMLInvokeTail(tail string) bool {
	tail = strings.TrimLeft(tail, " \t\r\n|｜")
	return tail == "invoke" || strings.HasPrefix(tail, "invoke\x00") || (len(tail) >= 6 && tail[:6] == "invoke" &&
		(len(tail) == 6 || !isWordByte(tail[6])))
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isJSSpace(r rune) bool {
	return unicode.IsSpace(r)
}

// marksPrefix buf 是否还可能是某个标记的开头（流里先扣住）。
func marksPrefix(s string) bool {
	for _, m := range ttMarks {
		if strings.HasPrefix(m, s) {
			return true
		}
	}
	return false
}

// ttQuoted text 是否止于代码围栏或行内代码里：后面的是展示，不是执行。
func ttQuoted(text string) bool {
	if strings.Count(text, "```")%2 == 1 {
		return true
	}
	line := text
	if i := strings.LastIndex(text, "\n"); i >= 0 {
		line = text[i+1:]
	}
	line = strings.ReplaceAll(line, "```", "")
	return strings.Count(line, "`")%2 == 1
}

// closeAt 下一个 </tool_call> 或 </toolcall> 的下标与长度；没有 → (-1, 0)。
func closeAt(s string, from int) (int, int) {
	m := reCloseTag.FindStringSubmatchIndex(s[from:])
	if m == nil {
		return -1, 0
	}
	return from + m[0], m[1] - m[0]
}

var reCloseTag = regexp.MustCompile(`</tool_?call>`)

// objectEnd 从 from（跳过空白）起的 JSON 对象在哪结束：闭括号后一位；未完 → -1；根本没有 → -2。
func objectEnd(s string, from int) int {
	k := from
	for k < len(s) && isJSSpace(rune(s[k])) {
		k++
	}
	if k >= len(s) {
		return -1
	}
	if s[k] != '{' {
		return -2
	}
	depth, str, esc := 0, false, false
	for ; k < len(s); k++ {
		ch := s[k]
		if str {
			if esc {
				esc = false
			} else if ch == '\\' {
				esc = true
			} else if ch == '"' {
				str = false
			}
			continue
		}
		switch ch {
		case '"':
			str = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return k + 1
			}
		}
	}
	return -1
}

// escapeRaw 转义 JSON 字符串里的控制字符。
func escapeRaw(s string) string {
	var out strings.Builder
	str, esc := false, false
	for _, ch := range s {
		if str && !esc && ch < 0x20 {
			switch ch {
			case '\n':
				out.WriteString(`\n`)
			case '\r':
				out.WriteString(`\r`)
			case '\t':
				out.WriteString(`\t`)
			default:
				out.WriteString(`\u` + fmt.Sprintf("%04x", ch))
			}
			continue
		}
		if str && ch == '\\' && !esc {
			esc = true
		} else {
			if ch == '"' && !esc {
				str = !str
			}
			esc = false
		}
		out.WriteRune(ch)
	}
	return out.String()
}

// looseJSON 模型写的 JSON：字符串里裸换行/制表按已转义读。不是对象 → nil。
func looseJSON(s string) map[string]any {
	for _, t := range []string{s, escapeRaw(s)} {
		var v any
		if json.Unmarshal([]byte(t), &v) == nil {
			if m, ok := v.(map[string]any); ok {
				return m
			}
		}
	}
	return nil
}

// fixEscapes 什么也转义不出的反斜杠改成自己的反斜杠（\' 是它本意的引号）。
func fixEscapes(s string) string {
	return reFixEscapes.ReplaceAllStringFunc(s, func(m string) string {
		if m == `\'` {
			return "'"
		}
		if len(m) >= 2 && m[0] == '\\' {
			switch m[1] {
			case 'u', '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				return m
			}
		}
		return `\\`
	})
}

// quoteFix 转义模型留在字符串里的引号：引号收键只在冒号前，收值只在
// 闭括号/逗号这些可能跟在最后一个值后的东西前。
func quoteFix(s string) string {
	var out strings.Builder
	var stack []byte
	key := false
	for i := 0; i < len(s); {
		ch := s[i]
		if ch != '"' {
			switch ch {
			case '{':
				stack = append(stack, '{')
				key = true
			case '[':
				stack = append(stack, '[')
			case '}', ']':
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
			case ',':
				key = len(stack) > 0 && stack[len(stack)-1] == '{'
			case ':':
				key = false
			}
			out.WriteByte(ch)
			i++
			continue
		}
		isKey := key && len(stack) > 0 && stack[len(stack)-1] == '{'
		inArray := len(stack) > 0 && stack[len(stack)-1] == '['
		out.WriteByte(ch)
		i++
		for i < len(s) {
			c := s[i]
			if c == '\\' {
				out.WriteByte(c)
				if i+1 < len(s) {
					out.WriteByte(s[i+1])
					i++
				}
				i++
				continue
			}
			if c != '"' {
				out.WriteByte(c)
				i++
				continue
			}
			after := s[i+1:]
			ends := false
			if isKey {
				ends = regexp.MustCompile(`^\s*:`).MatchString(after)
			} else if inArray {
				ends = regexp.MustCompile(`^\s*(?:[}\]]\s*)*(?:$|,\s*["{[\d\-tfn])`).MatchString(after)
			} else {
				ends = regexp.MustCompile(`^\s*(?:[}\]]\s*)*(?:$|,\s*")`).MatchString(after)
			}
			if ends {
				out.WriteByte(c)
				i++
				break
			}
			out.WriteString(`\"`)
			i++
		}
		key = false
	}
	return out.String()
}

// repairedJSON looseJSON 之外，再按模型写错的样子读（plugins#38）：全转义引号、
// 什么也转义不了的斜杠、字符串里没转义的引号。
func repairedJSON(s string) map[string]any {
	if v := looseJSON(s); v != nil {
		return v
	}
	t := escapeRaw(s)
	var tries []string
	if regexp.MustCompile(`^\s*\{\s*\\"`).MatchString(s) {
		clean := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s), `{"`), `"}"`)
		_ = clean
		inner := regexp.MustCompile(`[\n\r\t]`).ReplaceAllStringFunc(strings.TrimSpace(s), func(c string) string {
			switch c {
			case "\n":
				return `\\n`
			case "\r":
				return `\\r`
			default:
				return `\\t`
			}
		})
		if u, ok := jsonStr(`"` + inner + `"`); ok {
			tries = append(tries, u, escapeRaw(u))
		}
		tries = append(tries, strings.ReplaceAll(s, `\"`, `"`))
	}
	ft := fixEscapes(t)
	tries = append(tries, ft, quoteFix(ft))
	for _, x := range tries {
		var v any
		if json.Unmarshal([]byte(x), &v) == nil {
			if m, ok := v.(map[string]any); ok {
				return m
			}
		}
	}
	return nil
}

// jsonStr 读一个 JSON 字符串字面量为其内容。
func jsonStr(s string) (string, bool) {
	var v string
	if json.Unmarshal([]byte(s), &v) == nil {
		return v, true
	}
	return "", false
}

// callJSON 一块的调用（{name,…}），再错也读不出 → nil。
func callJSON(s string) map[string]any {
	v := repairedJSON(s)
	if v == nil {
		return nil
	}
	if n, ok := v["name"].(string); ok && n != "" {
		return v
	}
	return nil
}

// callArgs 带标签一块的 arguments：arguments（对象或字符串）、input、parameters，
// 否则 name/type/id 之外的键。
func callArgs(v map[string]any) string {
	a, has := v["arguments"]
	if !has || a == nil {
		a, has = v["input"]
	}
	if !has || a == nil {
		a, has = v["parameters"]
	}
	if !has || a == nil {
		rest := map[string]any{}
		for k, val := range v {
			if k != "name" && k != "type" && k != "id" {
				rest[k] = val
			}
		}
		if len(rest) == 0 {
			return "{}"
		}
		b, _ := json.Marshal(rest)
		return string(b)
	}
	if s, ok := a.(string); ok {
		t := strings.TrimSpace(s)
		if t == "" {
			return "{}"
		}
		if m := looseJSON(t); m != nil {
			b, _ := json.Marshal(m)
			return string(b)
		}
		return s
	}
	if a == nil {
		return "{}"
	}
	b, _ := json.Marshal(a)
	return string(b)
}

// toolNamed 请求对模型所叫名字的自己的叫法（Read 对 read）；没有像的照旧。
func toolNamed(name string, tools []ToolDef) string {
	var names []string
	for _, t := range tools {
		if n := t.fn().Name; n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return name
	}
	for _, n := range names {
		if n == name {
			return name
		}
	}
	norm := func(n string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(n) {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	for _, n := range names {
		if norm(n) == norm(name) {
			return n
		}
	}
	return name
}

// glmCall GLM 模板的调用（{name, arguments}），不是 → nil。值在模式说 string 时按原文，
// 否则是它的 JSON（模板写的本不是字符串）。
func glmCall(s string, tools []ToolDef) (string, map[string]any, bool) {
	s = strings.TrimRight(s, "")
	if m := regexp.MustCompile(`</tool_?call>\s*$`).FindStringIndex(s); m != nil {
		s = s[:m[0]]
	}
	k := strings.Index(s, "<arg_key>")
	name := s
	if k >= 0 {
		name = s[:k]
	}
	name = strings.TrimSpace(name)
	if !regexp.MustCompile(`^[\w.:-]{1,128}$`).MatchString(name) {
		return "", nil, false
	}
	var props map[string]ToolProp
	declared := false
	for _, t := range tools {
		if f := t.fn(); f.Name == name {
			declared = true
			if f.Parameters != nil {
				props = f.Parameters.Properties
			}
			break
		}
	}
	if k < 0 && !declared {
		return "", nil, false
	}
	args := map[string]any{}
	for _, m := range reGLMArg.FindAllStringSubmatch(s, -1) {
		key, raw := m[1], m[2]
		if props != nil && props[key].Type == "string" {
			args[key] = raw
			continue
		}
		var v any
		if json.Unmarshal([]byte(strings.TrimSpace(raw)), &v) == nil {
			args[key] = v
		} else {
			args[key] = raw
		}
	}
	if k >= 0 && len(args) == 0 {
		return "", nil, false
	}
	return name, args, true
}

// paramArgs <parameter name="k">v</parameter>（或 <parameter=k>）的参数；值跑到
// 它的 close、下一个参数或结尾。模式说 string 的值是原文，否则是 JSON。closed 说每个都有 close。
func paramArgs(s, name string, tools []ToolDef) (map[string]any, bool) {
	var props map[string]ToolProp
	for _, t := range tools {
		if f := t.fn(); f.Name == name {
			if f.Parameters != nil {
				props = f.Parameters.Properties
			}
			break
		}
	}
	locs := reParamOpen.FindAllStringSubmatchIndex(s, -1)
	args := map[string]any{}
	closed := len(locs) > 0
	for k, loc := range locs {
		from := loc[1]
		to := len(s)
		if k+1 < len(locs) {
			to = locs[k+1][0]
		}
		v := s[from:to]
		if c := strings.Index(v, "</parameter>"); c >= 0 {
			v = v[:c]
		} else {
			closed = false
		}
		v = strings.TrimPrefix(strings.TrimSuffix(v, "\r\n"), "\r\n")
		v = strings.TrimPrefix(strings.TrimSuffix(v, "\n"), "\n")
		key := s[loc[2]:loc[3]]
		if loc[2] < 0 {
			key = s[loc[4]:loc[5]]
		}
		if props != nil && props[key].Type == "string" {
			args[key] = v
			continue
		}
		var jv any
		if json.Unmarshal([]byte(strings.TrimSpace(v)), &jv) == nil {
			args[key] = jv
		} else {
			args[key] = v
		}
	}
	return args, closed
}

// dsmlArgs DSML invoke 的参数：string="true" 的参数是原文，其余是 JSON（不是 JSON 就是原文）。
func dsmlArgs(body string) string {
	args := map[string]any{}
	for _, m := range reDSMLParam.FindAllStringSubmatch(body, -1) {
		if regexp.MustCompile(`string\s*=\s*"true"`).MatchString(m[2]) {
			args[m[1]] = m[3]
			continue
		}
		var v any
		if json.Unmarshal([]byte(strings.TrimSpace(m[3])), &v) == nil {
			args[m[1]] = v
		} else {
			args[m[1]] = m[3]
		}
	}
	b, _ := json.Marshal(args)
	return string(b)
}

// xmlOpener at 开头的第一个标签（<…>，没闭就到结尾）。
func xmlOpener(at string) string {
	if i := strings.IndexByte(at, '>'); i >= 0 {
		return at[:i+1]
	}
	return at
}

// xmlCall Anthropic 的标签（<function_calls> 包着一个或多个 <invoke …>）或 Qwen 的
// （<function=…>）：{call,rest}、还可能是一个时的 {wait}、不是调用时 {text}、截断时 {error}。
func (t *TextTools) xmlCall(s string, end bool) ttBlock {
	if m := regexp.MustCompile(`^<(?:antml:)?function_calls>`).FindString(s); m != "" {
		rest := strings.TrimLeft(s[len(m):], " \t\r\n")
		if !end && (rest == "" || hasPrefixAny(rest, xmlStarts)) {
			return ttBlock{wait: true}
		}
		return t.xmlInvoke(rest, end)
	}
	return t.xmlInvoke(s, end)
}

func hasPrefixAny(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func (t *TextTools) xmlInvoke(s string, end bool) ttBlock {
	m := reXMLInvoke.FindStringSubmatch(s)
	closeRe := regexp.MustCompile(`^</(?:antml:)?invoke\s*>`)
	if m == nil {
		m = reXMLFunc.FindStringSubmatch(s)
		closeRe = regexp.MustCompile(`^</function\s*>`)
	}
	if m == nil {
		if !end && (hasPrefixAny(s, xmlStarts) ||
			regexp.MustCompile(`^<(?:antml:)?(?:invoke\s|function=)[^<>]{0,300}$`).MatchString(s)) {
			return ttBlock{wait: true}
		}
		return ttBlock{text: true}
	}
	name := toolNamed(strings.TrimSpace(m[1]), t.tools)
	declared := false
	for _, tl := range t.tools {
		if tl.fn().Name == name {
			declared = true
			break
		}
	}
	if !declared {
		return ttBlock{text: true}
	}
	body := s[len(m[0]):]
	var c []int
	if mm := closeRe.FindStringIndex(body); mm != nil {
		c = mm
	}
	n := reXMLNext.FindStringIndex(body)
	stop := -1
	if c != nil {
		stop = c[0]
	}
	if n != nil && (stop < 0 || n[0] < stop) {
		stop = n[0]
	}
	if stop < 0 {
		if !end {
			return ttBlock{wait: true}
		}
		stop = len(body)
	}
	whole := c != nil && c[0] == stop
	region := regexp.MustCompile(`<(\/?)antml:parameter\b`).ReplaceAllString(body[:stop], `<$1parameter`)
	pargs, pclosed := paramArgs(region, name, t.tools)
	if !whole && !pclosed {
		if !regexp.MustCompile(`<parameter\b`).MatchString(region) {
			return ttBlock{text: true}
		}
		return ttBlock{err: ttCutShort}
	}
	b, _ := json.Marshal(pargs)
	rest := body[stop:]
	if whole {
		rest = body[c[1]:]
	}
	return ttBlock{call: TTCall{Name: name, Arguments: string(b)}, rest: rest}
}

// block 读 <tool_call> opener 之后的东西。named 是 opener 给出的名字。
func (t *TextTools) block(body, named string, end bool) ttBlock {
	if !end && strings.TrimSpace(body) == "" {
		return ttBlock{wait: true}
	}
	closeIdx, closeLen := closeAt(body, 0)
	if named != "" {
		if closeIdx < 0 && !end {
			return ttBlock{wait: true}
		}
		region := body
		rest := ""
		if closeIdx >= 0 {
			region = body[:closeIdx]
			rest = body[closeIdx+closeLen:]
		}
		if strings.HasPrefix(strings.TrimLeft(region, " \t\r\n"), "{") {
			v := repairedJSON(strings.TrimSpace(region))
			if v == nil {
				return ttBlock{err: errOf(closeIdx >= 0)}
			}
			if n, ok := v["name"].(string); ok && n != "" {
				return ttBlock{call: TTCall{Name: n, Arguments: callArgs(v)}, rest: rest}
			}
			b, _ := json.Marshal(v)
			return ttBlock{call: TTCall{Name: named, Arguments: string(b)}, rest: rest}
		}
		if strings.Contains(region, "<arg_key>") {
			if n, args, ok := glmCall(named+region, t.tools); ok {
				v := map[string]any{"name": n, "arguments": args}
				return ttBlock{call: TTCall{Name: n, Arguments: callArgs(v)}, rest: rest}
			}
			return ttBlock{err: errOf(closeIdx >= 0)}
		}
		pargs, pclosed := paramArgs(region, named, t.tools)
		if closeIdx < 0 && !pclosed {
			return ttBlock{err: ttCutShort}
		}
		b, _ := json.Marshal(pargs)
		return ttBlock{call: TTCall{Name: named, Arguments: string(b)}, rest: rest}
	}
	if strings.HasPrefix(strings.TrimLeft(body, " \t\r\n"), "{") {
		if ph := reParamHead.FindStringSubmatch(body); ph != nil {
			if closeIdx < 0 && !end {
				return ttBlock{wait: true}
			}
			start := len(ph[0]) - len(ph[2])
			endPos := len(body)
			if closeIdx >= 0 {
				endPos = closeIdx
			}
			region := body[start:endPos]
			pargs, pclosed := paramArgs(region, ph[1], t.tools)
			if closeIdx < 0 && !pclosed {
				return ttBlock{err: ttCutShort}
			}
			b, _ := json.Marshal(pargs)
			rest := ""
			if closeIdx >= 0 {
				rest = body[closeIdx+closeLen:]
			}
			return ttBlock{call: TTCall{Name: ph[1], Arguments: string(b)}, rest: rest}
		}
		o := objectEnd(body, 0)
		badKey := ":" + strconv.Itoa(o)
		if o > 0 && !t.bad[badKey] {
			v := callJSON(body[:o])
			after := ""
			if o < len(body) {
				after = body[o:]
			}
			if v != nil && !end && len(v) == 1 &&
				strings.HasPrefix("<parameter", strings.TrimLeft(strings.TrimPrefix(strings.TrimLeft(after, " \t\r\n"), ">"), " \t\r\n")) {
				return ttBlock{wait: true}
			}
			if v != nil {
				tail := after
				junk := reTailJunk.FindString(tail)
				next := tail[len(junk):]
				call := TTCall{Name: fmt.Sprint(v["name"]), Arguments: callArgs(v)}
				if !end && (next == "" || strings.HasPrefix("</tool_call>", next) || strings.HasPrefix("</toolcall>", next)) {
					return ttBlock{wait: true}
				}
				c2, n2 := closeAt(next, 0)
				if c2 == 0 {
					return ttBlock{call: call, rest: next[n2:]}
				}
				if !regexp.MustCompile(`\S`).MatchString(junk) && !regexp.MustCompile(`^[^\s<]`).MatchString(next) {
					return ttBlock{call: call, rest: tail}
				}
				if closeIdx < 0 && end {
					return ttBlock{call: call, rest: tail}
				}
			} else {
				t.bad[badKey] = true
			}
		}
		for j, n := closeIdx, closeLen; j >= 0; j, n = closeAt(body, j+1) {
			if t.bad["<"+strconv.Itoa(j)] {
				continue
			}
			if v := callJSON(body[:j]); v != nil {
				return ttBlock{call: TTCall{Name: fmt.Sprint(v["name"]), Arguments: callArgs(v)}, rest: body[j+n:]}
			}
			t.bad["<"+strconv.Itoa(j)] = true
		}
		if !end {
			return ttBlock{wait: true}
		}
		if v := callJSON(body); v != nil {
			return ttBlock{call: TTCall{Name: fmt.Sprint(v["name"]), Arguments: callArgs(v)}, rest: ""}
		}
		return ttBlock{err: errOf(closeIdx >= 0)}
	}
	if !reGlmHead.MatchString(body) {
		return ttBlock{text: true}
	}
	if closeIdx < 0 && !end {
		return ttBlock{wait: true}
	}
	region := body
	rest := ""
	if closeIdx >= 0 {
		region = body[:closeIdx]
		rest = body[closeIdx+closeLen:]
	}
	if n, args, ok := glmCall(region, t.tools); ok {
		v := map[string]any{"name": n, "arguments": args}
		return ttBlock{call: TTCall{Name: n, Arguments: callArgs(v)}, rest: rest}
	}
	if reGlmErrHead.MatchString(body) {
		return ttBlock{err: errOf(closeIdx >= 0)}
	}
	return ttBlock{text: true}
}

func errOf(readable bool) string {
	if readable {
		return ttUnreadable
	}
	return ttCutShort
}

// Push 切一段文本；end 时流已尽，未整的块按截断或收尾读。
func (t *TextTools) Push(s string, end bool) TTResult {
	t.buf += s
	var text, reasoning strings.Builder
	var calls []TTCall
	fail := ""
	for {
		if t.after {
			if n := leftoverLen(t.buf); n > 0 {
				t.buf = t.buf[n:]
			}
			trimmed := strings.TrimRight(t.buf, " \t\r\n")
			if t.buf == "" || (!end && marksPrefix(t.buf)) {
				break
			}
			if end && len(t.buf) > 1 && marksPrefix(trimmed) {
				t.buf = ""
				break
			}
			t.after = false
		}
		i, olen, oattrs := findOpen(t.buf)
		d := reIndex(reDSMLAt, t.buf)
		r := reIndex(reReasonAt, t.buf)
		s := -1
		smStr := ""
		if m := reStrayAt.FindStringIndex(t.buf); m != nil {
			s = m[0]
			smStr = t.buf[m[0]:m[1]]
		}
		x := reIndex(reXMLAt, t.buf)

		if s >= 0 && beforeAll(s, i, d, r, x) {
			head := t.buf[:s]
			tail := strings.ContainsAny(smStr[:1], `}]"'>`) ||
				strings.TrimSpace((t.sent + text.String() + head)[min(t.mark, len(t.sent)+text.Len()):]) == ""
			if !tail || ttQuoted(t.sent+text.String()+head) {
				text.WriteString(t.buf[:s+len(smStr)])
				t.buf = t.buf[s+len(smStr):]
				continue
			}
			text.WriteString(head)
			t.buf = t.buf[s+len(smStr):]
			t.after = true
			continue
		}
		if r >= 0 && beforeAll(r, i, d, x) {
			o := objectEnd(t.buf, r)
			if o == -1 && !end {
				text.WriteString(t.buf[:r])
				t.buf = t.buf[r:]
				break
			}
			var v map[string]any
			if o > 0 {
				v = looseJSON(t.buf[r:o])
			}
			if v != nil {
				if rc, ok := v["reasoning_content"].(string); ok {
					text.WriteString(t.buf[:r])
					reasoning.WriteString(rc)
					t.buf = t.buf[o:]
					continue
				}
			}
			text.WriteString(t.buf[:r+1])
			t.buf = t.buf[r+1:]
			continue
		}
		if i < 0 && d < 0 && x < 0 {
			break
		}
		if x >= 0 && beforeAll(x, i, d) {
			head := t.buf[:x]
			at := t.buf[x:]
			opener := xmlOpener(at)
			if ttQuoted(t.sent + text.String() + head) {
				text.WriteString(head + opener)
				t.buf = at[len(opener):]
				continue
			}
			b := t.xmlCall(at, end)
			if b.wait {
				break
			}
			if b.text {
				text.WriteString(head + opener)
				t.buf = at[len(opener):]
				continue
			}
			text.WriteString(head)
			if b.err != "" {
				t.buf = ""
				t.sent += text.String()
				return TTResult{Text: text.String(), Calls: calls, Reasoning: reasoning.String(), Error: b.err}
			}
			calls = append(calls, b.call)
			t.mark = len(t.sent) + text.Len()
			t.buf = b.rest
			t.after = true
			continue
		}
		if d >= 0 && (i < 0 || d < i) {
			text.WriteString(t.buf[:d])
			t.buf = t.buf[d:]
			tag := reDSMLTag.FindString(t.buf)
			if tag == "" {
				next := strings.Index(t.buf[1:], "<")
				if next < 0 && !end {
					break
				}
				if next < 0 {
					t.buf = ""
				} else {
					t.buf = t.buf[1+next:]
				}
				continue
			}
			inv := reDSMLInvoke.FindStringSubmatch(tag)
			if inv == nil {
				t.buf = t.buf[len(tag):]
				t.after = true
				continue
			}
			body := t.buf[len(tag):]
			cm := reDSMLClose.FindStringIndex(body)
			nm := reDSMLInvokeAt.FindStringIndex(body)
			stop := -1
			if cm != nil {
				stop = cm[0]
			}
			if nm != nil && (stop < 0 || nm[0] < stop) {
				stop = nm[0]
			}
			if stop < 0 {
				if !end {
					break
				}
				stop = len(body)
			}
			calls = append(calls, TTCall{Name: inv[1], Arguments: dsmlArgs(body[:stop])})
			t.mark = len(t.sent) + text.Len()
			if cm != nil && cm[0] == stop {
				t.buf = body[stop+len(reDSMLClose.FindString(body)):]
			} else {
				t.buf = body[stop:]
			}
			t.after = true
			continue
		}
		head := t.buf[:i]
		body := t.buf[i+olen:]
		if ttQuoted(t.sent + text.String() + head) {
			text.WriteString(head + t.buf[i:i+olen])
			t.buf = body
			continue
		}
		named := ""
		if m := reNameAttr.FindStringSubmatch(oattrs); m != nil {
			named = m[1]
		}
		inner := strings.TrimLeft(body, " \t\r\n")
		if named == "" && (strings.HasPrefix(inner, "<") || (end && inner == "")) {
			text.WriteString(head)
			t.buf = inner
			t.after = true
			continue
		}
		b := t.block(body, named, end)
		if b.wait {
			break
		}
		for k := range t.bad {
			delete(t.bad, k)
		}
		if b.text {
			text.WriteString(head + t.buf[i:i+olen])
			t.buf = body
			continue
		}
		text.WriteString(head)
		if b.err != "" {
			t.buf = ""
			t.sent += text.String()
			return TTResult{Text: text.String(), Calls: calls, Reasoning: reasoning.String(), Error: b.err}
		}
		calls = append(calls, b.call)
		t.mark = len(t.sent) + text.Len()
		t.buf = b.rest
		t.after = true
	}
	if fail != "" {
		return TTResult{Text: text.String(), Calls: calls, Reasoning: reasoning.String(), Error: fail}
	}
	if end {
		if h := reIndex(reOpenHold, t.buf); h >= 0 && !ttQuoted(t.sent+text.String()+t.buf[:h]) {
			text.WriteString(t.buf[:h])
			t.buf = ""
			t.sent += text.String()
			return TTResult{Text: text.String(), Calls: calls, Reasoning: reasoning.String(), Error: ttCutShort}
		}
		text.WriteString(t.buf)
		t.buf = ""
	} else {
		holds := []int{
			reIndex(reOpenJSONAsAt(), t.buf), reIndex(reOpenHold, t.buf),
			reIndex(reDSMLAt, t.buf), reIndex(reReasonAt, t.buf),
			reIndex(reXMLAt, t.buf), reIndex(reXMLHold, t.buf),
		}
		keep := 0
		best := -1
		for _, h := range holds {
			if h >= 0 && (best < 0 || h < best) {
				best = h
			}
		}
		if best >= 0 {
			keep = len(t.buf) - best
		}
		if keep == 0 {
			max := len(t.buf)
			if max > markMax-1 {
				max = markMax - 1
			}
			for k := max; k > 0; k-- {
				if marksPrefix(t.buf[len(t.buf)-k:]) {
					keep = k
					break
				}
			}
		}
		if sh := reStrayHold.FindStringIndex(t.buf[:len(t.buf)-keep]); sh != nil {
			keep = len(t.buf) - sh[0]
		}
		text.WriteString(t.buf[:len(t.buf)-keep])
		t.buf = t.buf[len(t.buf)-keep:]
	}
	t.sent += text.String()
	return TTResult{Text: text.String(), Calls: calls, Reasoning: reasoning.String()}
}

// reOpenJSONAsAt hold-back 用的 OPEN_AT 的 JSON 分支（<tool_?call 直跟 \s*\{）。
func reOpenJSONAsAt() *regexp.Regexp { return reOpenJSON }

var markMax = func() int {
	m := 0
	for _, s := range ttMarks {
		if len(s) > m {
			m = len(s)
		}
	}
	return m
}()

func beforeAll(v int, others ...int) bool {
	for _, o := range others {
		if o >= 0 && o < v {
			return false
		}
	}
	return true
}

// unwrapped 参数外面模型又包的一层（arguments/input/parameters/args）剥掉。
func unwrapped(args, name string, tools []ToolDef) string {
	var fn *ToolFunc
	for _, t := range tools {
		if f := t.fn(); f.Name == name {
			fn = f
			break
		}
	}
	if fn == nil {
		return args
	}
	var props map[string]ToolProp
	if fn.Parameters != nil {
		props = fn.Parameters.Properties
	}
	v := looseJSON(args)
	changed := false
	for k := 0; k < 3 && v != nil; k++ {
		if len(v) != 1 {
			break
		}
		var key string
		var inner any
		for kk, vv := range v {
			key, inner = kk, vv
		}
		wrapOK := false
		for _, w := range ttWraps {
			if w == key {
				wrapOK = true
				break
			}
		}
		if !wrapOK {
			break
		}
		if _, isProp := props[key]; isProp {
			break
		}
		if s, ok := inner.(string); ok {
			inner = looseJSON(s)
		}
		m, ok := inner.(map[string]any)
		if !ok {
			break
		}
		v = m
		changed = true
	}
	if changed {
		b, _ := json.Marshal(v)
		return string(b)
	}
	return args
}

// finishOf Trae 最后事件给的说法到 OpenAI 的 finish_reason；没说的原样透传。
func finishOf(f string) string {
	switch strings.ToLower(strings.TrimSpace(f)) {
	case "length", "max_tokens", "max_output_tokens", "max_length":
		return "length"
	case "content_filter", "sensitive":
		return "content_filter"
	}
	return f
}

// Thoughts 推理去重：重发的只发新增（index.mjs:2205-2220）。
type Thoughts struct {
	all  string
	last string
}

func (t *Thoughts) Add(r string) string {
	last := t.last
	t.last = r
	if r == last && len(r) >= 8 {
		return ""
	}
	if len(t.all) >= 16 && len(r) >= 16 && strings.HasSuffix(t.all, r) {
		return ""
	}
	if len(t.all) >= 16 && strings.HasPrefix(r, t.all) {
		r = r[len(t.all):]
	}
	t.all += r
	return r
}
