package prompt

import (
	"path/filepath"
	"testing"

	"llmgateway/internal/apierr"
	"llmgateway/internal/llm"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "prompts.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func msgs(text string) []llm.Message { return []llm.Message{{Role: llm.RoleUser, Content: text}} }

// TestVersionAutoIncrement 验证同名模板每次创建都追加新版本，旧版本不被覆盖。
func TestVersionAutoIncrement(t *testing.T) {
	s := newStore(t)
	v1, err := s.Create("tpl", "第一版", "系统提示 {{a}}", msgs("你好 {{b}}"))
	if err != nil || v1.Version != 1 {
		t.Fatalf("首次创建应为 v1，实际 %+v err=%v", v1, err)
	}
	v2, _ := s.Create("tpl", "第二版", "系统提示 {{a}} {{c}}", msgs("你好 {{b}}"))
	if v2.Version != 2 {
		t.Fatalf("同名再创建应为 v2，实际 %d", v2.Version)
	}
	// 旧版本内容必须保持不变
	got, err := s.Get("tpl", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.System != "系统提示 {{a}}" {
		t.Fatalf("v1 应保持不变，实际 %q", got.System)
	}
	// 省略版本号取最新
	latest, _ := s.Get("tpl", 0)
	if latest.Version != 2 {
		t.Fatalf("version<=0 应取 latest，实际 v%d", latest.Version)
	}
}

// TestExtractVariables 验证变量自动解析与去重排序。
func TestExtractVariables(t *testing.T) {
	got := ExtractVariables("你好 {{name}}，{{ name }} 再说一次", msgs("翻译成 {{lang}}：{{text}}"))
	want := []string{"lang", "name", "text"}
	if len(got) != len(want) {
		t.Fatalf("变量解析结果 %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("变量解析结果 %v，期望 %v", got, want)
		}
	}
}

// TestRenderReplacesAll 验证变量替换覆盖 system 与全部消息。
func TestRenderReplacesAll(t *testing.T) {
	s := newStore(t)
	tpl, _ := s.Create("t", "", "翻译成{{lang}}。", msgs("原文：{{text}}"))
	r, err := Render(tpl, map[string]string{"lang": "英文", "text": "今天天气很好"})
	if err != nil {
		t.Fatal(err)
	}
	if r.System != "翻译成英文。" {
		t.Errorf("system 替换有误: %q", r.System)
	}
	if r.Messages[0].Content != "原文：今天天气很好" {
		t.Errorf("message 替换有误: %q", r.Messages[0].Content)
	}
}

// TestRenderMissingVariable 验证缺变量时报错而不是把占位符原样发给模型。
func TestRenderMissingVariable(t *testing.T) {
	s := newStore(t)
	tpl, _ := s.Create("t", "", "{{a}}", msgs("{{b}}"))
	_, err := Render(tpl, map[string]string{"a": "有了"})
	if err == nil {
		t.Fatal("缺变量应报错")
	}
	if code := apierr.From(err).Code; code != apierr.CodePromptVarMissing {
		t.Fatalf("错误码应为 PROMPT_VAR_MISSING，实际 %s", code)
	}
}

// TestGetNotFound 验证模板/版本不存在时的错误码。
func TestGetNotFound(t *testing.T) {
	s := newStore(t)
	if _, err := s.Get("nope", 0); apierr.From(err).Code != apierr.CodePromptNotFound {
		t.Fatalf("模板不存在应返回 PROMPT_NOT_FOUND，实际 %v", err)
	}
	s.Create("t", "", "x", nil)
	if _, err := s.Get("t", 99); apierr.From(err).Code != apierr.CodePromptNotFound {
		t.Fatalf("版本不存在应返回 PROMPT_NOT_FOUND，实际 %v", err)
	}
}

// TestPersistence 验证模板落盘后能被新实例加载回来。
func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prompts.json")
	s1, _ := NewStore(path)
	s1.Create("t", "", "v1 内容 {{a}}", nil)
	s1.Create("t", "", "v2 内容 {{a}}", nil)

	s2, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := s2.Get("t", 0)
	if err != nil || latest.Version != 2 {
		t.Fatalf("重新加载后应有 v2，实际 %+v err=%v", latest, err)
	}
	old, _ := s2.Get("t", 1)
	if old.System != "v1 内容 {{a}}" {
		t.Fatalf("v1 内容应完好，实际 %q", old.System)
	}
	// 版本号必须从磁盘上的最大值继续递增
	v3, _ := s2.Create("t", "", "v3", nil)
	if v3.Version != 3 {
		t.Fatalf("重启后新建应为 v3，实际 %d", v3.Version)
	}
}

// TestRefString 验证引用串格式，可观测性里会用到。
func TestRefString(t *testing.T) {
	if got := (&Ref{Name: "t", Version: 2}).String(); got != "t@v2" {
		t.Errorf("期望 t@v2，实际 %q", got)
	}
	if got := (&Ref{Name: "t"}).String(); got != "t@latest" {
		t.Errorf("期望 t@latest，实际 %q", got)
	}
}
