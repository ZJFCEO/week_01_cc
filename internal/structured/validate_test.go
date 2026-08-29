package structured

import "testing"

// TestExtractPlainJSON 验证直接就是 JSON 的情况。
func TestExtractPlainJSON(t *testing.T) {
	clean, parsed, err := Extract(`{"a":1}`)
	if err != nil || clean != `{"a":1}` {
		t.Fatalf("解析失败: %q err=%v", clean, err)
	}
	if parsed.(map[string]any)["a"].(float64) != 1 {
		t.Fatal("解析结果有误")
	}
}

// TestExtractStripsCodeFence 验证剥掉 Markdown 代码围栏。
func TestExtractStripsCodeFence(t *testing.T) {
	if _, _, err := Extract("```json\n{\"a\":1}\n```"); err != nil {
		t.Fatalf("应能剥掉代码围栏: %v", err)
	}
}

// TestExtractSlicesOutermost 验证前后带解释文字时截取最外层括号。
func TestExtractSlicesOutermost(t *testing.T) {
	clean, _, err := Extract(`好的，结果如下：{"a":1} 希望有帮助`)
	if err != nil || clean != `{"a":1}` {
		t.Fatalf("应截出 JSON 主体，实际 %q err=%v", clean, err)
	}
}

// TestExtractRejectsGarbage 验证真正的非法 JSON 会报错。
func TestExtractRejectsGarbage(t *testing.T) {
	if _, _, err := Extract("抱歉，我先解释一下：{ 这不是合法 JSON"); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

// TestValidateSchemaRequired 验证必填字段校验。
func TestValidateSchemaRequired(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"a": map[string]any{"type": "string"}},
		"required":   []any{"a"},
	}
	if err := ValidateSchema(map[string]any{"a": "x"}, schema, "$"); err != nil {
		t.Fatalf("合法数据不应报错: %v", err)
	}
	if err := ValidateSchema(map[string]any{"b": "x"}, schema, "$"); err == nil {
		t.Fatal("缺必填字段应报错")
	}
}

// TestValidateSchemaTypes 验证类型与 integer 的特殊处理。
func TestValidateSchemaTypes(t *testing.T) {
	if err := ValidateSchema("x", map[string]any{"type": "number"}, "$"); err == nil {
		t.Fatal("字符串不应通过 number 校验")
	}
	if err := ValidateSchema(float64(3), map[string]any{"type": "integer"}, "$"); err != nil {
		t.Fatalf("3 应通过 integer 校验: %v", err)
	}
	if err := ValidateSchema(3.5, map[string]any{"type": "integer"}, "$"); err == nil {
		t.Fatal("3.5 不应通过 integer 校验")
	}
}

// TestValidateSchemaEnum 验证 enum 约束。
func TestValidateSchemaEnum(t *testing.T) {
	schema := map[string]any{"type": "string", "enum": []any{"positive", "negative"}}
	if err := ValidateSchema("positive", schema, "$"); err != nil {
		t.Fatalf("enum 内的值不应报错: %v", err)
	}
	if err := ValidateSchema("unknown", schema, "$"); err == nil {
		t.Fatal("enum 外的值应报错")
	}
}

// TestValidateSchemaNested 验证嵌套对象与数组的递归校验。
func TestValidateSchemaNested(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"items": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"type": "number"}}, "required": []any{"n"}},
			},
		},
	}
	good := map[string]any{"items": []any{map[string]any{"n": float64(1)}}}
	if err := ValidateSchema(good, schema, "$"); err != nil {
		t.Fatalf("合法嵌套数据不应报错: %v", err)
	}
	bad := map[string]any{"items": []any{map[string]any{"n": "字符串"}}}
	if err := ValidateSchema(bad, schema, "$"); err == nil {
		t.Fatal("数组元素类型不符应报错")
	}
}

// TestValidateSchemaAdditionalProperties 验证 additionalProperties=false。
func TestValidateSchemaAdditionalProperties(t *testing.T) {
	schema := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"a": map[string]any{"type": "string"}},
		"additionalProperties": false,
	}
	if err := ValidateSchema(map[string]any{"a": "x", "extra": 1}, schema, "$"); err == nil {
		t.Fatal("未声明的字段应被拒绝")
	}
}
