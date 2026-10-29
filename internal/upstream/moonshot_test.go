package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, raw)
	}
	return out
}

func toolParameters(t *testing.T, body []byte) map[string]any {
	t.Helper()
	obj := decodeObject(t, body)
	tools, ok := obj["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("tools missing: %#v", obj["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	params, ok := fn["parameters"].(string)
	if !ok {
		t.Fatalf("parameters should be string, got %T", fn["parameters"])
	}
	return decodeObject(t, []byte(params))
}

func anyStrings(v any) []string {
	switch list := v.(type) {
	case []string:
		return list
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func nestedMap(t *testing.T, obj map[string]any, path ...string) map[string]any {
	t.Helper()
	cur := obj
	for _, key := range path {
		next, ok := cur[key].(map[string]any)
		if !ok {
			t.Fatalf("%q is not an object: %#v", key, cur[key])
		}
		cur = next
	}
	return cur
}

func TestMoonshotSchemaEnumAndConstGetType(t *testing.T) {
	const schema = `{
		"type":"object",
		"properties":{"ops":{"type":"array","items":{"type":"object","properties":{
			"op":{"enum":["add","remove"]},
			"n":{"enum":[1,2]},
			"f":{"enum":[1,2.5]},
			"b":{"const":true},
			"nullable":{"enum":["x",null]},
			"mixed":{"enum":["x",1]}
		}}}}
	}`
	var root map[string]any
	if err := json.Unmarshal([]byte(schema), &root); err != nil {
		t.Fatal(err)
	}
	if !moonshotSchema(root) {
		t.Fatal("moonshotSchema should report a change")
	}
	props := nestedMap(t, root, "properties", "ops", "items", "properties")
	if got := nestedMap(t, props, "op")["type"]; got != "string" {
		t.Errorf("op.type=%v want string", got)
	}
	if got := nestedMap(t, props, "n")["type"]; got != "integer" {
		t.Errorf("n.type=%v want integer", got)
	}
	if got := nestedMap(t, props, "f")["type"]; got != "number" {
		t.Errorf("f.type=%v want number", got)
	}
	if got := nestedMap(t, props, "b")["type"]; got != "boolean" {
		t.Errorf("b.type=%v want boolean", got)
	}
	nullable := anyStrings(nestedMap(t, props, "nullable")["type"])
	if len(nullable) != 2 || nullable[0] != "string" || nullable[1] != "null" {
		t.Errorf("nullable.type=%v want [string null]", nestedMap(t, props, "nullable")["type"])
	}
	if _, has := nestedMap(t, props, "mixed")["type"]; has {
		t.Errorf("mixed values should not get a type: %v", nestedMap(t, props, "mixed")["type"])
	}
}

func TestMoonshotSchemaRefAndAnyOf(t *testing.T) {
	const schema = `{
		"type":"object",
		"properties":{
			"n":{"type":"integer","maximum":5,"$ref":"#/$defs/s0"},
			"u":{"type":"string","anyOf":[{"type":"string"},{"type":"null"}]}
		},
		"$defs":{"s0":{"type":"number"}}
	}`
	var root map[string]any
	if err := json.Unmarshal([]byte(schema), &root); err != nil {
		t.Fatal(err)
	}
	moonshotSchema(root)
	props := nestedMap(t, root, "properties")
	n := nestedMap(t, props, "n")
	if _, has := n["$ref"]; has {
		t.Errorf("n should not keep $ref: %v", n)
	}
	if n["type"] != "integer" || n["maximum"] != float64(5) {
		t.Errorf("n=%v want type integer maximum 5", n)
	}
	u := nestedMap(t, props, "u")
	if _, has := u["type"]; has {
		t.Errorf("u should not keep type: %v", u)
	}
	if _, has := u["anyOf"]; !has {
		t.Errorf("u should keep anyOf: %v", u)
	}
}

func TestMoonshotSchemaRefTargetWithNoTypeGetsInferred(t *testing.T) {
	const schema = `{
		"type":"object",
		"properties":{"x":{"$ref":"#/$defs/s1"}},
		"$defs":{"s1":{"enum":["a","b"]}}
	}`
	var root map[string]any
	if err := json.Unmarshal([]byte(schema), &root); err != nil {
		t.Fatal(err)
	}
	moonshotSchema(root)
	target := nestedMap(t, root, "$defs", "s1")
	if target["type"] != "string" {
		t.Errorf("$ref target type=%v want string", target["type"])
	}
}

func TestMoonshotSchemaRefTargetWithRefOnlyLosesType(t *testing.T) {
	const schema = `{
		"type":"object",
		"properties":{"x":{"type":"integer","$ref":"#/$defs/s0"}},
		"$defs":{"s0":{"$ref":"#/$defs/s1"},"s1":{"type":"string"}}
	}`
	var root map[string]any
	if err := json.Unmarshal([]byte(schema), &root); err != nil {
		t.Fatal(err)
	}
	moonshotSchema(root)
	x := nestedMap(t, root, "properties", "x")
	if _, has := x["type"]; has {
		t.Errorf("x should lose type when target carries $ref: %v", x)
	}
	if _, has := x["$ref"]; !has {
		t.Errorf("x should keep $ref: %v", x)
	}
}

func TestMoonshotSchemaLeavesValidSchemaAlone(t *testing.T) {
	const schema = `{"type":"object","properties":{
		"a":{"type":"string","enum":["x"]},
		"b":{"anyOf":[{"type":"string"},{"type":"null"}]}
	}}`
	var root map[string]any
	if err := json.Unmarshal([]byte(schema), &root); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(root)
	if moonshotSchema(root) {
		t.Errorf("valid schema should not change: %s", before)
	}
	after, _ := json.Marshal(root)
	if string(before) != string(after) {
		t.Errorf("valid schema changed:\nbefore=%s\nafter=%s", before, after)
	}
}

func TestNormalizeToolsClonesMoonshotParameters(t *testing.T) {
	const tools = `{"tools":[{"type":"function","function":{"name":"edit","parameters":{
		"type":"object","properties":{"op":{"enum":["add","remove"]}}
	}}}]}`
	var obj map[string]any
	if err := json.Unmarshal([]byte(tools), &obj); err != nil {
		t.Fatal(err)
	}
	toolsList := obj["tools"].([]any)
	tool := toolsList[0].(map[string]any)
	fn := tool["function"].(map[string]any)
	params := fn["parameters"].(map[string]any)
	op := nestedMap(t, params, "properties", "op")
	normalizeTools(obj, true)
	if _, has := op["type"]; has {
		t.Errorf("original parameters were mutated: %v", op)
	}
	sent, ok := fn["parameters"].(string)
	if !ok || !strings.Contains(sent, `"type":"string"`) {
		t.Errorf("sent parameters missing inferred type: %v", fn["parameters"])
	}
}

func moonshotRequest(model string) []byte {
	return []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"edit","parameters":{"type":"object","properties":{"op":{"enum":["add","remove"]}}}}}]}`)
}

func TestPrepareBodyMoonshotOnlyForKimi(t *testing.T) {
	out := PrepareBodyOpts(moonshotRequest("kimi-k3"), ChatOpts{Model: "kimi-k3"}, "", "")
	if got := nestedMap(t, toolParameters(t, out), "properties", "op")["type"]; got != "string" {
		t.Errorf("Kimi op.type=%v want string", got)
	}

	out = PrepareBodyOpts(moonshotRequest("glm-5"), ChatOpts{Model: "glm-5"}, "", "")
	got := toolParameters(t, out)
	if _, has := nestedMap(t, got, "properties", "op")["type"]; has {
		t.Errorf("non-Kimi schema should be unchanged: %v", got)
	}
}

func TestIsMoonshotModel(t *testing.T) {
	if !IsMoonshotModel("Kimi-K3", "", "") {
		t.Error("Kimi should match")
	}
	if !IsMoonshotModel("", "moonshot-v1", "") {
		t.Error("moonshot should match")
	}
	if IsMoonshotModel("glm-5", "DeepSeek-V4.1-Flash-1017", "") {
		t.Error("non-Moonshot should not match")
	}
}
