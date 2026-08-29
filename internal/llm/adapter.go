package llm

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Adapter 是统一调用接口。每种上游 API 协议实现一个。
//
// 适配器的职责边界（三件事，缺一不可）：
//  1. 鉴权差异：OpenAI 系用 Authorization: Bearer，Anthropic 用 x-api-key + anthropic-version
//  2. 请求体差异：instructions/input vs system/messages，max_tokens 是否必填等
//  3. 返回体差异：output[].content[].text vs content[].text，以及两套完全不同的 SSE 事件流
type Adapter interface {
	// Protocol 返回协议标识，例如 openai_responses / anthropic_messages。
	Protocol() string
	// Invoke 非流式调用。
	Invoke(ctx context.Context, req *Request) (*Response, error)
	// Stream 流式调用，返回统一事件通道；通道关闭即代表本次流结束。
	Stream(ctx context.Context, req *Request) (<-chan StreamEvent, error)
}

// ModelRoute 是路由表里的一条记录：逻辑模型名 -> 适配器 + 上游模型名。
type ModelRoute struct {
	Model         string  // 对外暴露的逻辑模型名
	UpstreamModel string  // 传给上游的真实模型名
	Protocol      string  // 协议标识
	Adapter       Adapter // 绑定的适配器实例
	Description   string
}

// Registry 是模型路由表，负责按请求里的 model 字段动态分发到对应适配器。
type Registry struct {
	mu     sync.RWMutex
	routes map[string]*ModelRoute
}

func NewRegistry() *Registry {
	return &Registry{routes: make(map[string]*ModelRoute)}
}

// Register 注册一条模型路由。
func (r *Registry) Register(route *ModelRoute) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes[route.Model] = route
}

// Resolve 是动态路由的核心：拿请求里的 model 换出适配器。
// 找不到时返回明确错误，由上层翻译成 MODEL_NOT_FOUND。
func (r *Registry) Resolve(model string) (*ModelRoute, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	route, ok := r.routes[model]
	if !ok {
		return nil, fmt.Errorf("模型 %q 未在路由表中注册", model)
	}
	return route, nil
}

// List 按模型名排序列出全部路由，供 /v1/models 展示。
func (r *Registry) List() []*ModelRoute {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*ModelRoute, 0, len(r.routes))
	for _, v := range r.routes {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}
