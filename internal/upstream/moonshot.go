package upstream

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
)

// IsMoonshotModel 判断模型名是否属于 Kimi/Moonshot（插件 moonshotModel 口径）。
// 插件同时看 config、modelName、req.model；调用方应把能拿到的名称都传进来。
func IsMoonshotModel(names ...string) bool {
	for _, name := range names {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "kimi") || strings.Contains(lower, "moonshot") {
			return true
		}
	}
	return false
}

var (
	msMapKeys = []string{
		"properties", "$defs", "definitions", "dependentSchemas", "patternProperties",
	}
	msSubKeys = []string{
		"items", "additionalProperties", "contains", "propertyNames", "not",
		"if", "then", "else", "allOf", "anyOf", "oneOf", "prefixItems",
		"unevaluatedProperties", "unevaluatedItems", "contentSchema",
	}
	msAllTypes = []string{"string", "number", "integer", "boolean", "object", "array", "null"}
)

// msEachSchema 遍历 schema 中所有子 schema（插件 msEachSchema 口径）。
func msEachSchema(node any, f func(map[string]any)) {
	switch n := node.(type) {
	case []any:
		for _, item := range n {
			msEachSchema(item, f)
		}
	case map[string]any:
		f(n)
		for _, key := range msMapKeys {
			if m, ok := n[key].(map[string]any); ok {
				for _, child := range m {
					msEachSchema(child, f)
				}
			}
		}
		for _, key := range msSubKeys {
			if child, ok := n[key]; ok {
				msEachSchema(child, f)
			}
		}
	}
}

// msRefTarget 解析本地 JSON Pointer（# 或 #/...）；外部引用不处理。
func msRefTarget(root any, ref any) (any, bool) {
	s, ok := ref.(string)
	if !ok || (s != "#" && !strings.HasPrefix(s, "#/")) {
		return nil, false
	}
	if s == "#" {
		return root, true
	}
	cur := root
	for _, token := range strings.Split(s, "/")[1:] {
		token = strings.ReplaceAll(token, "~1", "/")
		token = strings.ReplaceAll(token, "~0", "~")
		if decoded, err := url.PathUnescape(token); err == nil {
			token = decoded
		}
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[token]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(node) {
				return nil, false
			}
			cur = node[index]
		default:
			return nil, false
		}
	}
	return cur, true
}

// msHasRefInside 判断 schema 内是否还有 $ref（插件 msHasRefInside 口径）。
func msHasRefInside(v any) bool {
	switch n := v.(type) {
	case map[string]any:
		if _, ok := n["$ref"]; ok {
			return true
		}
		for _, child := range n {
			if msHasRefInside(child) {
				return true
			}
		}
	case []any:
		for _, child := range n {
			if msHasRefInside(child) {
				return true
			}
		}
	}
	return false
}

// msValueType 返回 JSON 值的 schema 类型（插件 msValueType 口径）。
func msValueType(v any) string {
	switch n := v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		if math.Trunc(n) == n {
			return "integer"
		}
		return "number"
	case float32:
		if math.Trunc(float64(n)) == float64(n) {
			return "integer"
		}
		return "number"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return "integer"
	case json.Number:
		if _, err := n.Int64(); err == nil {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "object"
	}
}

// msEnumType 推断 enum/const 值的共同类型；无法推断时返回 false。
func msEnumType(vals []any) (any, bool) {
	t := ""
	hasNull := false
	for _, v := range vals {
		vt := msValueType(v)
		if vt == "null" {
			hasNull = true
			continue
		}
		if vt == "array" || vt == "object" {
			return nil, false
		}
		if t == "" {
			t = vt
			continue
		}
		if (t == "integer" && vt == "number") || (t == "number" && vt == "integer") {
			t = "number"
			continue
		}
		if t != vt {
			return nil, false
		}
	}
	if t == "" {
		return nil, false
	}
	if hasNull {
		return []string{t, "null"}, true
	}
	return t, true
}

// msSchemaTypes 返回 schema 可能具有的类型集合；无法判断时返回 false。
func msSchemaTypes(root any, s map[string]any, depth int) (map[string]bool, bool) {
	if depth > 16 {
		return nil, false
	}
	if typ, ok := s["type"]; ok {
		set := map[string]bool{}
		switch v := typ.(type) {
		case string:
			set[v] = true
		case []any:
			for _, item := range v {
				set[msString(item)] = true
			}
		default:
			set[msString(v)] = true
		}
		return set, true
	}
	if ref, ok := s["$ref"]; ok {
		target, found := msRefTarget(root, ref)
		if !found {
			return nil, false
		}
		targetMap, ok := target.(map[string]any)
		if !ok {
			return nil, false
		}
		return msSchemaTypes(root, targetMap, depth+1)
	}
	if constant, ok := s["const"]; ok {
		return map[string]bool{msValueType(constant): true}, true
	}
	if enum, ok := s["enum"].([]any); ok {
		set := map[string]bool{}
		for _, item := range enum {
			set[msValueType(item)] = true
		}
		return set, true
	}
	for _, key := range []string{"oneOf", "anyOf", "allOf"} {
		branches, ok := s[key].([]any)
		if !ok {
			continue
		}
		set := map[string]bool{}
		for _, branch := range branches {
			branchMap, ok := branch.(map[string]any)
			if !ok {
				if key != "allOf" {
					return nil, false
				}
				continue
			}
			types, ok := msSchemaTypes(root, branchMap, depth+1)
			if !ok {
				if key != "allOf" {
					return nil, false
				}
				continue
			}
			for typ := range types {
				set[typ] = true
			}
		}
		if len(set) > 0 {
			return set, true
		}
	}
	has := func(keys ...string) bool {
		for _, key := range keys {
			if _, ok := s[key]; ok {
				return true
			}
		}
		return false
	}
	if has("properties", "required", "additionalProperties", "patternProperties",
		"minProperties", "maxProperties", "propertyNames", "dependentRequired",
		"dependentSchemas", "unevaluatedProperties") {
		return map[string]bool{"object": true}, true
	}
	if has("items", "prefixItems", "minItems", "maxItems", "uniqueItems",
		"contains", "unevaluatedItems") {
		return map[string]bool{"array": true}, true
	}
	if has("minLength", "maxLength", "pattern", "format", "contentEncoding", "contentMediaType") {
		return map[string]bool{"string": true}, true
	}
	if has("minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf") {
		return map[string]bool{"number": true}, true
	}
	return nil, false
}

// msTypeFor 推断一个没有 type 的 schema 应写什么 type（插件 msTypeFor 口径）。
func msTypeFor(root any, s map[string]any) (any, bool) {
	if enum, ok := s["enum"].([]any); ok {
		return msEnumType(enum)
	}
	if constant, ok := s["const"]; ok {
		return msEnumType([]any{constant})
	}
	types, ok := msSchemaTypes(root, s, 0)
	if !ok || len(types) == 0 {
		return append([]string(nil), msAllTypes...), true
	}
	if types["integer"] && types["number"] {
		delete(types, "integer")
	}
	names := make([]string, 0, len(types))
	for _, typ := range msAllTypes {
		if types[typ] {
			names = append(names, typ)
		}
	}
	if len(names) == 1 {
		return names[0], true
	}
	return names, true
}

// msString 模拟 JS String(v)，只用于 type 数组里的标量。
func msString(v any) string {
	if v == nil {
		return "null"
	}
	return fmt.Sprint(v)
}

// cloneJSONValue 深拷贝 JSON 值，保证请求自身的 tools 不被改写。
func cloneJSONValue(v any) any {
	switch n := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(n))
		for key, child := range n {
			out[key] = cloneJSONValue(child)
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, child := range n {
			out[i] = cloneJSONValue(child)
		}
		return out
	default:
		return v
	}
}

// cloneJSONMap 深拷贝 JSON 对象。
func cloneJSONMap(v map[string]any) map[string]any {
	out := make(map[string]any, len(v))
	for key, child := range v {
		out[key] = cloneJSONValue(child)
	}
	return out
}

// moonshotSchema 按 Kimi/Moonshot 的 schema 规则改写参数副本。
// 规则与 plugins/packages/trae/index.mjs 的 moonshotSchema 一致：
//   - $ref 与 type 并存：目标无 $ref 时合并目标，否则删 type
//   - type 与 anyOf 并存：删 type
//   - enum/const 无 type：按值推断
//   - $ref 目标无 type/anyOf/$ref：按目标结构推断并补 type
func moonshotSchema(root map[string]any) bool {
	changed := false
	msEachSchema(root, func(s map[string]any) {
		if _, hasRef := s["$ref"]; hasRef {
			if _, hasType := s["type"]; hasType {
				target, found := msRefTarget(root, s["$ref"])
				targetMap, isMap := target.(map[string]any)
				if found && isMap && !msHasRefInside(targetMap) {
					own := cloneJSONMap(s)
					delete(own, "$ref")
					for key := range s {
						delete(s, key)
					}
					for key, value := range cloneJSONMap(targetMap) {
						s[key] = value
					}
					for key, value := range own {
						s[key] = value
					}
				} else {
					delete(s, "type")
				}
				changed = true
			}
		}
		if _, hasAnyOf := s["anyOf"].([]any); hasAnyOf {
			if _, hasType := s["type"]; hasType {
				delete(s, "type")
				changed = true
			}
		}
	})
	msEachSchema(root, func(s map[string]any) {
		if _, hasType := s["type"]; hasType {
			return
		}
		var values []any
		if enum, ok := s["enum"].([]any); ok {
			values = enum
		} else if constant, ok := s["const"]; ok {
			values = []any{constant}
		} else {
			return
		}
		if typ, ok := msEnumType(values); ok {
			s["type"] = typ
			changed = true
		}
	})
	msEachSchema(root, func(s map[string]any) {
		ref, ok := s["$ref"].(string)
		if !ok {
			return
		}
		target, found := msRefTarget(root, ref)
		targetMap, isMap := target.(map[string]any)
		if !found || !isMap {
			return
		}
		if _, has := targetMap["type"]; has {
			return
		}
		if _, has := targetMap["anyOf"]; has {
			return
		}
		if _, has := targetMap["$ref"]; has {
			return
		}
		if len(targetMap) == 0 {
			return
		}
		if typ, ok := msTypeFor(root, targetMap); ok {
			targetMap["type"] = typ
			changed = true
		}
	})
	return changed
}
