// Package prompt 实现提示词模板的存储、变量替换和版本引用。
//
// 版本策略：同名模板每次创建自动递增版本号（1,2,3...），历史版本永久保留、不可变。
// 引用时 version 省略或 <=0 表示取 latest，这样线上可以固定引用某个版本，
// 调试时用 latest 快速迭代。
package prompt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"llmgateway/internal/apierr"
	"llmgateway/internal/llm"
)

// varPattern 匹配 {{ variable }} 形式的占位符。
var varPattern = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_.\-]+)\s*\}\}`)

// Template 是一个不可变的提示词模板版本快照。
type Template struct {
	Name        string        `json:"name"`
	Version     int           `json:"version"`
	Description string        `json:"description,omitempty"`
	System      string        `json:"system,omitempty"`
	Messages    []llm.Message `json:"messages"`
	Variables   []string      `json:"variables"` // 从模板文本里自动解析出的变量名
	CreatedAt   time.Time     `json:"created_at"`
}

// Ref 是一次调用里对模板的引用。
type Ref struct {
	Name      string            `json:"name"`
	Version   int               `json:"version,omitempty"` // 省略或 <=0 表示 latest
	Variables map[string]string `json:"variables,omitempty"`
}

// String 返回 name@vN 形式的引用串，用于可观测性记录。
func (r *Ref) String() string {
	if r == nil {
		return ""
	}
	if r.Version <= 0 {
		return r.Name + "@latest"
	}
	return r.Name + "@v" + itoa(r.Version)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// Store 是模板仓库：内存索引 + JSON 文件落盘，重启不丢。
type Store struct {
	mu       sync.RWMutex
	versions map[string][]*Template // name -> 按版本升序排列的全部版本
	path     string
}

// NewStore 创建仓库并尝试从磁盘恢复。
func NewStore(path string) (*Store, error) {
	s := &Store{versions: make(map[string][]*Template), path: path}
	if path == "" {
		return s, nil
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 首次启动没有文件是正常的
		}
		return err
	}
	var all []*Template
	if err := json.Unmarshal(raw, &all); err != nil {
		return err
	}
	for _, t := range all {
		s.versions[t.Name] = append(s.versions[t.Name], t)
	}
	for name := range s.versions {
		sort.Slice(s.versions[name], func(i, j int) bool {
			return s.versions[name][i].Version < s.versions[name][j].Version
		})
	}
	return nil
}

// persist 在锁内调用，把全量模板写回磁盘。
// 必须把错误往上抛：否则会出现「接口返回创建成功、重启后模板消失」这种最难查的问题。
func (s *Store) persist() error {
	if s.path == "" {
		return nil
	}
	var all []*Template
	for _, list := range s.versions {
		all = append(all, list...)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Name != all[j].Name {
			return all[i].Name < all[j].Name
		}
		return all[i].Version < all[j].Version
	})
	raw, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	// 先写临时文件再原子替换，避免写到一半进程崩溃留下半个损坏的 JSON
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ExtractVariables 扫描模板全文，返回去重后的变量名列表。
func ExtractVariables(system string, msgs []llm.Message) []string {
	seen := map[string]bool{}
	var out []string
	scan := func(text string) {
		for _, m := range varPattern.FindAllStringSubmatch(text, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				out = append(out, m[1])
			}
		}
	}
	scan(system)
	for _, m := range msgs {
		scan(m.Content)
	}
	sort.Strings(out)
	return out
}

// Create 新建一个版本。
// 流转顺序：① 校验入参 ② 取当前最大版本号 +1 ③ 解析变量名 ④ 入库并落盘
func (s *Store) Create(name, description, system string, msgs []llm.Message) (*Template, error) {
	// ① 基本校验
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, apierr.New(apierr.CodeInvalidRequest, "模板 name 不能为空")
	}
	if system == "" && len(msgs) == 0 {
		return nil, apierr.New(apierr.CodeInvalidRequest, "模板必须至少包含 system 或一条 message")
	}
	for i, m := range msgs {
		if m.Role != llm.RoleUser && m.Role != llm.RoleAssistant && m.Role != llm.RoleSystem {
			return nil, apierr.New(apierr.CodeInvalidRequest, "messages[%d].role 非法: %q", i, m.Role)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// ② 版本号自增：同名模板永远追加新版本，不覆盖旧版本
	next := 1
	if list := s.versions[name]; len(list) > 0 {
		next = list[len(list)-1].Version + 1
	}
	// ③ 变量名自动解析，便于调用方知道要传什么
	t := &Template{
		Name:        name,
		Version:     next,
		Description: description,
		System:      system,
		Messages:    msgs,
		Variables:   ExtractVariables(system, msgs),
		CreatedAt:   time.Now().UTC(),
	}
	// ④ 入库 + 落盘。落盘失败要把内存改动回滚掉，
	//    保证「接口返回成功」严格等价于「磁盘上真的有这一版」。
	s.versions[name] = append(s.versions[name], t)
	if err := s.persist(); err != nil {
		s.versions[name] = s.versions[name][:len(s.versions[name])-1]
		if len(s.versions[name]) == 0 {
			delete(s.versions, name)
		}
		return nil, apierr.Wrap(err, apierr.CodeInternal, "模板落盘失败: %s", err.Error())
	}
	return t, nil
}

// Get 按 name + version 取模板；version<=0 取 latest。
func (s *Store) Get(name string, version int) (*Template, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	list := s.versions[name]
	if len(list) == 0 {
		return nil, apierr.New(apierr.CodePromptNotFound, "提示词模板 %q 不存在", name)
	}
	// version 省略 -> 取最新版本
	if version <= 0 {
		return list[len(list)-1], nil
	}
	for _, t := range list {
		if t.Version == version {
			return t, nil
		}
	}
	return nil, apierr.New(apierr.CodePromptNotFound, "提示词模板 %q 没有 v%d 版本（当前最新 v%d）",
		name, version, list[len(list)-1].Version)
}

// Summary 是模板列表项。
type Summary struct {
	Name          string    `json:"name"`
	LatestVersion int       `json:"latest_version"`
	VersionCount  int       `json:"version_count"`
	Description   string    `json:"description,omitempty"`
	Variables     []string  `json:"variables"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// List 列出所有模板及其最新版本。
func (s *Store) List() []Summary {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Summary, 0, len(s.versions))
	for name, list := range s.versions {
		latest := list[len(list)-1]
		out = append(out, Summary{
			Name:          name,
			LatestVersion: latest.Version,
			VersionCount:  len(list),
			Description:   latest.Description,
			Variables:     latest.Variables,
			UpdatedAt:     latest.CreatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Versions 列出某个模板的全部历史版本。
func (s *Store) Versions(name string) ([]*Template, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	list := s.versions[name]
	if len(list) == 0 {
		return nil, apierr.New(apierr.CodePromptNotFound, "提示词模板 %q 不存在", name)
	}
	out := make([]*Template, len(list))
	copy(out, list)
	return out, nil
}

// Rendered 是模板渲染结果。
type Rendered struct {
	System   string
	Messages []llm.Message
}

// Render 做变量替换。
// 流转顺序：① 收集模板里出现的全部变量 ② 检查是否有缺失，缺则报错（不静默留空）
// ③ 逐字段替换 {{var}}
func Render(t *Template, vars map[string]string) (*Rendered, error) {
	// ① + ② 先做缺失检查，避免把 {{xxx}} 原样发给模型
	var missing []string
	for _, name := range t.Variables {
		if _, ok := vars[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, apierr.New(apierr.CodePromptVarMissing,
			"模板 %s@v%d 缺少变量: %s", t.Name, t.Version, strings.Join(missing, ", "))
	}
	// ③ 替换
	replace := func(text string) string {
		return varPattern.ReplaceAllStringFunc(text, func(m string) string {
			sub := varPattern.FindStringSubmatch(m)
			if len(sub) < 2 {
				return m
			}
			return vars[sub[1]]
		})
	}
	r := &Rendered{System: replace(t.System)}
	for _, m := range t.Messages {
		r.Messages = append(r.Messages, llm.Message{Role: m.Role, Content: replace(m.Content)})
	}
	return r, nil
}
