// models.go 多 function 模型目录（插件 index.mjs chatModel/maxModel/levelsOf/
// effortOf/seesOf/liveModels 移植）：
//
//   - 目录来自各聊天 function 的模型列表（batch_get_detail_param 一次取全部，
//     失败/为空再逐 function get_detail_param），合并去重；
//   - 同 id 首次出现者优先（site.functions 顺序）；
//   - 条目带 __max 细目且窗口 > dev 窗口时追加 <id>-max 条目；
//   - reasoning_effort_config（light/high/extra_high）映射为 low/high/xhigh。
package upstream

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// chatFunctions 目录来源的 function 顺序（插件 site.functions 原序）。
var chatFunctions = []string{"chat_v3", "solo_work_lite", "solo_agent", "solo_agent_lite", "solo_coder"}

// devSuffix / maxSuffix 模型内部名后缀（__dev 常规、__max Max 模式）。
const (
	devSuffix  = "__dev"
	maxSuffix  = "__max"
	modelMax   = "-max" // Max 条目的 id 后缀
	promptCap  = "prompt_max_tokens"
	effortSend = "reasoning_effort"
)

// maxTokenLimit Max 条目的 max_tokens（__max 细目的 max_tokens）。
const maxTokenLimit = "max_tokens"

// LEVELS Trae 的 reasoning 等级 → 本插件口径。Trae 客户端 bundle 共五档
//（reasoning_effort.{low,medium,high,extra_high,max}），0.2.23 起插件也提供
// medium/max（#434，DeepSeek V4.1 Flash 以 max 提供）——仍只在目录条目的
// reasoning_effort_config 点名时才发送。
var LEVELS = map[string]string{
	"light": "low", "medium": "medium", "high": "high",
	"extra_high": "xhigh", "max": "max",
}

// devModelRe / maxModelRe 细目名匹配（插件 /__dev$/、/__max$/）。
var (
	devModelRe = regexp.MustCompile(devSuffix + `$`)
	maxModelRe = regexp.MustCompile(maxSuffix + `$`)
	customRe   = regexp.MustCompile(`(?i)^custom_model`)
)

// detailList 是条目的 model_detail_list 细目（__dev/__max）。
type detailList []struct {
	ModelName string `json:"model_name"`
	MaxTokens int64  `json:"max_tokens"`
}

// detailOf 按后缀找细目；找不到返回 nil。
func (l detailList) detailOf(re *regexp.Regexp) *struct {
	ModelName string `json:"model_name"`
	MaxTokens int64  `json:"max_tokens"`
} {
	for i := range l {
		if re.MatchString(l[i].ModelName) {
			return &l[i]
		}
	}
	return nil
}

// rawEntry 是一个 function 列表里的原始条目（json 形态）。
type rawEntry struct {
	ConfigName string `json:"config_name"`
	Usage      string `json:"usage"`
	// ConfigSwitch 缺失 → nil（视为开）；显式 false → 关。
	ConfigSwitch   *bool `json:"config_switch"`
	IsCustomModel  *bool `json:"is_custom_model"`
	InvisibleUser  *bool `json:"is_invisible_to_user"`
	HidesInvisible bool  `json:"-"`
	Multimodal     *bool `json:"multimodal"`
	// PromptMaxTokens 条目自带 prompt_max_tokens（部分旧列表用它表达窗口）。
	PromptMaxTokens int64 `json:"prompt_max_tokens"`
	// ReasoningEffortConfig 缺失/非对象 → 不承载等级。
	ReasoningEffortConfig *struct {
		Options []string `json:"options"`
	} `json:"reasoning_effort_config"`
	// ContextWindowTokens 结构自由（dev/max 等键），按需取数。
	ContextWindowTokens map[string]int64 `json:"context_window_tokens"`
	// ContextWindowSize 结构自由（max.[0]/max），取不到再看 tokens。
	ContextWindowSize sizeMap `json:"context_window_size"`
	// DisplayConfig 结构自由：display_name/multimodal/is_custom_model。
	DisplayConfig displayMap `json:"display_config"`
	// Display* 展示名备选。
	DisplayName      string `json:"display_name"`
	DisplayModelName string `json:"display_model_name"`
	// ModelDetailList __dev/__max 细目。
	ModelDetailList detailList `json:"model_detail_list"`
}

// chatModel 判断条目是不是可对话模型（插件 chatModel 口径）：无 config_name、
// custom_model 前缀、usage 非 chat_completion、config_switch=false、
// 自定义模型、is_invisible_to_user 的条目都不算。
func (e *rawEntry) chatModel() bool {
	id := strings.TrimSpace(e.ConfigName)
	if id == "" || customRe.MatchString(id) {
		return false
	}
	if e.Usage != "" && e.Usage != "chat_completion" {
		return false
	}
	if e.ConfigSwitch != nil && !*e.ConfigSwitch {
		return false
	}
	if (e.IsCustomModel != nil && *e.IsCustomModel) || e.DisplayConfig.bool("is_custom_model") {
		return false
	}
	if e.HidesInvisible && e.InvisibleUser != nil && *e.InvisibleUser {
		return false
	}
	return true
}

// nameOf 条目的展示名（插件 nameOf 口径）。
func (e *rawEntry) nameOf() string {
	if v := e.DisplayConfig.str("display_name"); v != "" {
		return v
	}
	if e.DisplayName != "" {
		return e.DisplayName
	}
	if e.DisplayModelName != "" {
		return e.DisplayModelName
	}
	return e.ConfigName
}

// devModel 条目的 __dev 细目；nil 表示列表没说 Trae 用哪个内部模型伺服。
func (e *rawEntry) devModel() *struct {
	ModelName string `json:"model_name"`
	MaxTokens int64  `json:"max_tokens"`
} {
	return e.ModelDetailList.detailOf(devModelRe)
}

// seesOf 条目是否说明模型收图：multimodal（含 display_config 内）true/false；
// 没说 → null。
func (e *rawEntry) seesOf() *bool {
	if e.Multimodal != nil {
		return e.Multimodal
	}
	if v := e.DisplayConfig.boolPtr("multimodal"); v != nil {
		return v
	}
	return nil
}

// levelsOf 条目承载的等级（本插件口径 low/high/xhigh，Trae 原序）。
// 空 = 条目没说；nil = 说了但都拼不出（按不承载处理）。
func (e *rawEntry) levelsOf() []string {
	cfg := e.ReasoningEffortConfig
	if cfg == nil {
		return []string{}
	}
	known := make([]string, 0, len(cfg.Options))
	for _, o := range cfg.Options {
		if lv, ok := LEVELS[o]; ok {
			known = append(known, lv)
		}
	}
	if len(known) > 0 {
		return known
	}
	if len(cfg.Options) > 0 {
		return nil
	}
	return []string{}
}

// effortOf 请求携带的 effort 名 → 上游要的拼写（light/high/extra_high）；
// 模型不承载该等级 → ""。两种拼写都认（low↔light、xhigh↔extra_high）。
func effortOf(levels []string, effort string) string {
	if effort == "" || levels == nil || len(levels) == 0 {
		return ""
	}
	// effort 可能是本插件口径（low/high/xhigh）或 Trae 的拼写
	//（light/extra_high），都翻到本口径再对照等级表。
	mine := effort
	for tra, ours := range LEVELS {
		if ours == effort || tra == effort {
			mine = ours
			break
		}
	}
	for _, lv := range levels {
		if lv == mine {
			for tra, ours := range LEVELS {
				if ours == mine {
					return tra
				}
			}
		}
	}
	return ""
}

// windowOf 条目的上下文窗口：dev 优先（请求实际拿到的窗口），没有再取
// max（Max 模式窗口）、prompt_max_tokens。全没有 → 0。
func (e *rawEntry) windowOf(key string) int64 {
	if key == "dev" {
		if n, ok := e.ContextWindowTokens["dev"]; ok && n > 0 {
			return n
		}
		if n := e.ContextWindowSize.num("max", 0); n > 0 {
			return n
		}
		if n, ok := e.ContextWindowTokens["max"]; ok && n > 0 {
			return n
		}
		if e.PromptMaxTokens > 0 {
			return e.PromptMaxTokens
		}
		return 0
	}
	if n, ok := e.ContextWindowTokens[key]; ok && n > 0 {
		return n
	}
	if key == "max" {
		if n := e.ContextWindowSize.num("max", 0); n > 0 {
			return n
		}
	}
	return 0
}

// catalogEntry 目录里一个模型的合并视图。
type catalogEntry struct {
	Model  ModelInfo
	raw    *rawEntry
	fn     string // 列出该模型的 function
	levels []string
	sees   *bool
	max    *maxInfo
}

// maxInfo 一个模型的 Max 模式（插件 maxModel 口径）。
type maxInfo struct {
	name   string // __max 内部模型名
	most   int64  // __max 的 max_tokens
	window int64  // Max 窗口（context_window_tokens.max）
}

// Catalog 是目录的对外别名（server 按它查询 function/Max/sees/levels）。
type Catalog = *catalog

// Reassign 导出 catalog.reassign（聊天回退成功后更新归属）。
func (c *catalog) Reassign(id, fn string) { c.reassign(id, fn) }

// catalog 多 function 模型目录：id → 条目（site.functions 顺序合并，同 id
// 首现优先）。
type catalog struct {
	entries map[string]*catalogEntry
	order   []string
}

func newCatalog() *catalog { return &catalog{entries: map[string]*catalogEntry{}} }

func (c *catalog) get(id string) (*catalogEntry, bool) {
	e, ok := c.entries[id]
	return e, ok
}

// add 按 functions 顺序合并各 function 的列表。同 id 首现者优先；条目先过
// chatModel 过滤。
func (c *catalog) add(lists map[string][]rawEntry) {
	for _, fn := range chatFunctions {
		for i := range lists[fn] {
			e := &lists[fn][i]
			e.HidesInvisible = true // trae-cn 的 IDE 拾取器隐藏 is_invisible_to_user
			if !e.chatModel() {
				continue
			}
			id := strings.TrimSpace(e.ConfigName)
			if _, ok := c.entries[id]; ok {
				continue
			}
			mi := ModelInfo{
				ID:            id,
				Name:          e.nameOf(),
				ContextWindow: e.windowOf("dev"),
				MaxTokens:     0,
				IsCustomModel: (e.IsCustomModel != nil && *e.IsCustomModel) || e.DisplayConfig.bool("is_custom_model"),
			}
			if dev := e.devModel(); dev != nil {
				mi.InternalName = dev.ModelName
				mi.MaxTokens = dev.MaxTokens
			}
			c.entries[id] = &catalogEntry{
				Model:  mi,
				raw:    e,
				fn:     fn,
				levels: e.levelsOf(),
				sees:   e.seesOf(),
			}
			c.order = append(c.order, id)
		}
	}
	// Max 条目：任一 function 的列表给该模型 __max 细目且窗口 > dev 窗口时，
	// 追加 <id>-max（插件 maxModel/写入 3027-3043 口径）。
	for _, id := range c.order {
		e := c.entries[id]
		if _, ok := c.entries[id+modelMax]; ok {
			continue
		}
		max := maxOf(id, lists)
		if max == nil {
			continue
		}
		e.max = max
		c.entries[id+modelMax] = &catalogEntry{
			Model: ModelInfo{
				ID:            id + modelMax,
				Name:          e.Model.Name + " (Max)",
				ContextWindow: max.window,
				MaxTokens:     max.most,
				InternalName:  max.name,
				IsCustomModel: e.Model.IsCustomModel,
			},
			raw:  e.raw,
			fn:   e.fn,
			max:  max,
			sees: e.sees,
		}
		c.order = append(c.order, id+modelMax)
	}
}

// maxOf 一个模型的 Max 信息：首个带 __max 细目的条目的 __max 名与 max_tokens，
// 窗口取各条目 context_window_tokens.max 的首个正值；不大于 dev 窗口 → 无 Max。
// （插件 maxModel：窗口逐条目取首个 >0 的 max，再逐条目取首个 >0 的 dev。）
func maxOf(id string, lists map[string][]rawEntry) *maxInfo {
	var at *rawEntry
	var maxDetail *struct {
		ModelName string `json:"model_name"`
		MaxTokens int64  `json:"max_tokens"`
	}
	window := int64(0)
	dev := int64(0)
	for _, fn := range chatFunctions {
		for i := range lists[fn] {
			e := &lists[fn][i]
			if strings.TrimSpace(e.ConfigName) != id || !e.chatModel() {
				continue
			}
			if at == nil {
				if d := e.ModelDetailList.detailOf(maxModelRe); d != nil {
					at = e
					maxDetail = d
				}
			}
			if window == 0 {
				window = e.windowOf("max")
			}
			if dev == 0 {
				dev = e.windowOf("dev")
			}
		}
	}
	if at == nil || window <= dev {
		return nil
	}
	return &maxInfo{name: maxDetail.ModelName, most: maxDetail.MaxTokens, window: window}
}

// CatalogFrom 由 FetchModels 的结果重建可查询目录（server 侧用；Function
// 字段指出列出者，IsMax 指出 <id>-max 条目，窗口/max_tokens 已带）。
func CatalogFrom(infos []ModelInfo) Catalog {
	cat := newCatalog()
	for i := range infos {
		mi := infos[i]
		base, isMax := takeMax(mi.ID)
		e := &catalogEntry{Model: mi, levels: mi.Levels, sees: mi.Sees}
		if fn := mi.Function; fn != "" {
			e.fn = fn
		} else {
			e.fn = chatFunctions[0]
		}
		cat.entries[mi.ID] = e
		cat.order = append(cat.order, mi.ID)
		if isMax {
			// Max 条目的 Max 信息挂到 base 上，供请求侧取 model_name/most/window。
			if be, ok := cat.entries[base]; ok && be.max == nil {
				be.max = &maxInfo{name: mi.InternalName, most: mi.MaxTokens, window: mi.ContextWindow}
			}
			e.max = &maxInfo{name: mi.InternalName, most: mi.MaxTokens, window: mi.ContextWindow}
		}
	}
	return cat
}

// ChatFunctions 目录来源的 function 顺序（导出，供 server 轮换）。
func ChatFunctions() []string { return chatFunctions }

// FunctionOf 模型由哪个 function 列出（导出）。目录没有 → 空。
func (c *catalog) FunctionOf(id string) string { return c.fnOf(id) }

// Has 目录是否收录该 id（导出；server 判断 REREAD_MS 重读）。
func (c *catalog) Has(id string) bool {
	_, ok := c.entries[id]
	return ok
}

// SeesOf 模型是否收图（导出）。目录没说 → nil。
func (c *catalog) SeesOf(id string) *bool { return c.seesOf(id) }

// LevelsOf 模型承载的 reasoning 等级（导出）。条目没说 → 空。
func (c *catalog) LevelsOf(id string) []string { return c.levelsOf(id) }

// infos 目录的 OpenAI 侧列表（ModelInfo 切片，目录顺序）。
func (c *catalog) infos() []ModelInfo {
	out := make([]ModelInfo, 0, len(c.order))
	for _, id := range c.order {
		mi := c.entries[id].Model
		mi.Levels = c.entries[id].levels
		mi.Sees = c.entries[id].sees
		out = append(out, mi)
	}
	return out
}

// fnOf 模型由哪个 function 列出（聊天请求默认走它）。
func (c *catalog) fnOf(id string) string {
	if e, ok := c.entries[id]; ok {
		return e.fn
	}
	return ""
}

// seesOf 模型是否收图；目录没说 → null。
func (c *catalog) seesOf(id string) *bool {
	if e, ok := c.entries[id]; ok {
		return e.sees
	}
	return nil
}

// levelsOf 模型承载的等级（本插件口径）；条目没说 → 空。
func (c *catalog) levelsOf(id string) []string {
	if e, ok := c.entries[id]; ok {
		return e.levels
	}
	return []string{}
}

// effortOf 按 models.go 目录翻译请求 effort（模型不承载 → ""）。
func (c *catalog) effortOf(id, effort string) string {
	if e, ok := c.entries[id]; ok {
		return effortOf(e.levels, effort)
	}
	return ""
}

// reassign 聊天回退成功后更新目录里模型的 function 归属。
func (c *catalog) reassign(id, fn string) {
	if e, ok := c.entries[id]; ok && fn != "" {
		e.fn = fn
	}
}

// SplitMaxID 剥客户端模型 id 的 -max 后缀：返回 base id 与是否 Max 请求
// （server 聊天入口用；Max 语义见 handler）。
func SplitMaxID(id string) (string, bool) { return takeMax(id) }

// takeMax 剥 <id>-max：返回 base id 与是否 Max 请求。
func takeMax(id string) (string, bool) {
	if strings.HasSuffix(id, modelMax) {
		return strings.TrimSuffix(id, modelMax), true
	}
	return id, false
}

// MaxOf 模型的 Max 模式（目录）；无 → nil。
func (c *catalog) MaxOf(id string) *MaxMode {
	e, ok := c.entries[id]
	if !ok || e.max == nil {
		return nil
	}
	return &MaxMode{Base: id, Name: e.max.name, Most: e.max.most, Window: e.max.window}
}

// WrongFunctionCode 是否为「该 function 不收这个模型」的业务码
// （插件 wrongFunction：4001/4023/1005）。
func WrongFunctionCode(code int64) bool {
	return code == 4001 || code == 4023 || code == 1005
}

// ErrorCodeOf 从上游错误 body 提取业务码（HTTP 非 2xx 或 HTTP 200 + JSON
// 错误形态均可；解析不出 → 0）。插件 errorOf 口径：error.code / code /
// ResponseMetadata.Error.Code。
func ErrorCodeOf(body []byte) int64 {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return 0
	}
	// ResponseMetadata.Error.Code
	if rm, ok := raw["ResponseMetadata"].(map[string]any); ok {
		if em, ok := rm["Error"].(map[string]any); ok {
			return codeAsInt(em["code"])
		}
	}
	if em, ok := raw["error"].(map[string]any); ok {
		return codeAsInt(em["code"])
	}
	return codeAsInt(raw["code"])
}

// codeAsInt 业务码的宽松取数（数字或 "4001" 形态的字符串）。
func codeAsInt(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case string:
		var i int64
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "%d", &i); err == nil {
			return i
		}
	}
	return 0
}

// jsonNumber / jsonValue 自由形态的 JSON 数值/值（窗口与 display_config
// 的结构上游多变，只按需取字段）。sizeMap / displayMap 是承载辅助方法的
// 包装类型。
type (
	jsonNumber = int64
	jsonValue  = any
	sizeMap    map[string]jsonNumber
	displayMap map[string]jsonValue
)

// num 取 context_window_size.max（数组形态的键后缀 0 或数值）；没有 → 0。
func (m sizeMap) num(key string, alt int) int64 {
	if m == nil {
		return 0
	}
	if v, ok := m[key]; ok && v > 0 {
		return int64(v)
	}
	return 0
}

// str 取 display_config 的字符串字段。
func (m displayMap) str(key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// bool / boolPtr 取 display_config 的布尔字段。
func (m displayMap) bool(key string) bool {
	v := m.boolPtr(key)
	return v != nil && *v
}

func (m displayMap) boolPtr(key string) *bool {
	if m == nil {
		return nil
	}
	v, ok := m[key].(bool)
	if !ok {
		return nil
	}
	return &v
}
