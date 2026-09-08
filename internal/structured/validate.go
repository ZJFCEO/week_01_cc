// Package structured 负责结构化输出的兜底：从模型返回里抽出 JSON，
// 校验它是合法 JSON，并按 JSON Schema 做轻量校验。
//
// 为什么网关要再校验一遍：不同协议对结构化输出的支持强度不一样
// （Responses 原生 json_schema，Messages 靠工具调用模拟，Chat 只有 json_object），
// 只有在网关这一层统一校验，调用方才能拿到一致的保证。
package structured

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// Extract 从模型返回文本里抽出 JSON 主体。
// 流转顺序：① 去掉 Markdown 代码围栏 ② 直接尝试解析
// ③ 失败则退化为「截取第一个 { 或 [ 到最后一个 } 或 ]」再试
func Extract(content string) (string, any, error) {
	trimmed := strings.TrimSpace(content)
	// ① 剥掉 ```json ... ``` 围栏
	if strings.HasPrefix(trimmed, "```") {
		if idx := strings.Index(trimmed, "\n"); idx >= 0 {
			trimmed = trimmed[idx+1:]
		}
		trimmed = strings.TrimSuffix(strings.TrimSpace(trimmed), "```")
		trimmed = strings.TrimSpace(trimmed)
	}
	// ② 直接解析
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err == nil {
		return trimmed, v, nil
	}
	// ③ 截取最外层括号再试一次
	if sub := sliceOutermost(trimmed); sub != "" {
		if err := json.Unmarshal([]byte(sub), &v); err == nil {
			return sub, v, nil
		}
	}
	return "", nil, fmt.Errorf("模型返回不是合法 JSON")
}

// sliceOutermost 截取字符串中第一个 {/[ 到最后一个 }/] 之间的片段。
func sliceOutermost(s string) string {
	start := strings.IndexAny(s, "{[")
	if start < 0 {
		return ""
	}
	end := strings.LastIndexAny(s, "}]")
	if end <= start {
		return ""
	}
	return s[start : end+1]
}

// ValidateSchema 按 JSON Schema 的常用子集做校验。
// 支持 type / required / properties / items / enum / additionalProperties=false /
// minimum / maximum / minLength / maxLength。不追求完备，够覆盖作业场景且能给出明确路径。
func ValidateSchema(value any, schema map[string]any, path string) error {
	if len(schema) == 0 {
		return nil
	}
	if path == "" {
		path = "$"
	}
	// ① type 校验
	if t, ok := schema["type"].(string); ok {
		if err := checkType(value, t, path); err != nil {
			return err
		}
	}
	// ② enum 校验
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		matched := false
		for _, e := range enum {
			if jsonEqual(e, value) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s 的值 %v 不在 enum 允许范围内", path, value)
		}
	}
	// ③ 数值范围
	if num, ok := value.(float64); ok {
		if m, ok := toFloat(schema["minimum"]); ok && num < m {
			return fmt.Errorf("%s = %v 小于 minimum %v", path, num, m)
		}
		if m, ok := toFloat(schema["maximum"]); ok && num > m {
			return fmt.Errorf("%s = %v 大于 maximum %v", path, num, m)
		}
	}
	// ④ 字符串长度
	if str, ok := value.(string); ok {
		if m, ok := toFloat(schema["minLength"]); ok && float64(len([]rune(str))) < m {
			return fmt.Errorf("%s 长度小于 minLength %v", path, m)
		}
		if m, ok := toFloat(schema["maxLength"]); ok && float64(len([]rune(str))) > m {
			return fmt.Errorf("%s 长度大于 maxLength %v", path, m)
		}
	}
	// ⑤ 对象：required + properties 递归
	if obj, ok := value.(map[string]any); ok {
		if req, ok := schema["required"].([]any); ok {
			for _, r := range req {
				key, _ := r.(string)
				if _, exists := obj[key]; !exists {
					return fmt.Errorf("%s 缺少必填字段 %q", path, key)
				}
			}
		}
		props, _ := schema["properties"].(map[string]any)
		if props != nil {
			for key, sub := range props {
				v, exists := obj[key]
				if !exists {
					continue // 非必填字段缺失不算错
				}
				subSchema, _ := sub.(map[string]any)
				if err := ValidateSchema(v, subSchema, path+"."+key); err != nil {
					return err
				}
			}
			// additionalProperties=false 时禁止出现 schema 之外的字段
			if allow, ok := schema["additionalProperties"].(bool); ok && !allow {
				for key := range obj {
					if _, declared := props[key]; !declared {
						return fmt.Errorf("%s 出现未声明的字段 %q（additionalProperties=false）", path, key)
					}
				}
			}
		}
	}
	// ⑥ 数组：逐元素递归
	if arr, ok := value.([]any); ok {
		if items, ok := schema["items"].(map[string]any); ok {
			for i, item := range arr {
				if err := ValidateSchema(item, items, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// checkType 校验 JSON 值的类型。注意 encoding/json 把所有数字解析成 float64。
func checkType(value any, want string, path string) error {
	got := jsonTypeOf(value)
	switch want {
	case "integer":
		if f, ok := value.(float64); ok && f == float64(int64(f)) {
			return nil
		}
		return fmt.Errorf("%s 期望 integer，实际是 %s", path, got)
	case "number":
		if got == "number" {
			return nil
		}
	case "":
		return nil
	default:
		if got == want {
			return nil
		}
	}
	return fmt.Errorf("%s 期望 %s，实际是 %s", path, want, got)
}

// jsonEqual 按 JSON 语义比较两个值：先比类型再比值。
// 不能用 fmt.Sprintf("%v") 比较——那样数字 1 和字符串 "1" 会被判为相等，
// enum 约束就形同虚设。
func jsonEqual(a, b any) bool {
	if jsonTypeOf(a) != jsonTypeOf(b) {
		return false
	}
	switch av := a.(type) {
	case nil:
		return true
	case bool:
		bv, _ := b.(bool)
		return av == bv
	case float64:
		bv, _ := b.(float64)
		return av == bv
	case string:
		bv, _ := b.(string)
		return av == bv
	}
	// 数组/对象作为 enum 取值极少见，退回深比较
	return reflect.DeepEqual(a, b)
}

func jsonTypeOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

func toFloat(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}
