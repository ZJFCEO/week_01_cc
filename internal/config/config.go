// Package config 负责加载网关配置：模型路由表、限流参数、重试策略。
//
// 配置优先级：环境变量 > 配置文件 > 内置默认值。
// API Key 一律只写「环境变量名」，绝不落到配置文件里。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"llmgateway/internal/resilience"
)

// ModelConfig 是路由表里的一条模型配置。
type ModelConfig struct {
	Model         string                 `json:"model"`                    // 对外逻辑模型名
	UpstreamModel string                 `json:"upstream_model,omitempty"` // 传给上游的真实模型名，留空则同 Model
	Protocol      string                 `json:"protocol"`                 // openai_responses / anthropic_messages / openai_chat
	BaseURL       string                 `json:"base_url"`                 // 上游根地址
	BaseURLEnv    string                 `json:"base_url_env,omitempty"`   // 若该环境变量非空则覆盖 BaseURL
	APIKeyEnv     string                 `json:"api_key_env,omitempty"`    // 从哪个环境变量读 Key
	TimeoutMs     int                    `json:"timeout_ms,omitempty"`
	RateLimit     resilience.LimitConfig `json:"rate_limit"`
	Description   string                 `json:"description,omitempty"`
}

// Timeout 返回该模型的超时时长。
func (m ModelConfig) Timeout() time.Duration {
	if m.TimeoutMs <= 0 {
		return 60 * time.Second
	}
	return time.Duration(m.TimeoutMs) * time.Millisecond
}

// ResolveBaseURL 按「环境变量 > 配置文件」的优先级决定上游地址。
func (m ModelConfig) ResolveBaseURL() string {
	if m.BaseURLEnv != "" {
		if v := strings.TrimSpace(os.Getenv(m.BaseURLEnv)); v != "" {
			return v
		}
	}
	return m.BaseURL
}

// ResolveAPIKey 从环境变量读取 Key；读不到返回空串（离线 mock 上游不校验 Key）。
func (m ModelConfig) ResolveAPIKey() string {
	if m.APIKeyEnv == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv(m.APIKeyEnv))
}

// RetryConfig 是重试策略的可配置形式。
type RetryConfig struct {
	MaxRetries  int     `json:"max_retries"`
	BaseDelayMs int     `json:"base_delay_ms"`
	MaxDelayMs  int     `json:"max_delay_ms"`
	Factor      float64 `json:"factor"`
	Jitter      float64 `json:"jitter"`
}

// Policy 转成 resilience 层使用的策略对象。
func (r RetryConfig) Policy() resilience.RetryPolicy {
	p := resilience.DefaultRetryPolicy()
	if r.MaxRetries >= 0 {
		p.MaxRetries = r.MaxRetries
	}
	if r.BaseDelayMs > 0 {
		p.BaseDelay = time.Duration(r.BaseDelayMs) * time.Millisecond
	}
	if r.MaxDelayMs > 0 {
		p.MaxDelay = time.Duration(r.MaxDelayMs) * time.Millisecond
	}
	if r.Factor > 0 {
		p.Factor = r.Factor
	}
	if r.Jitter >= 0 {
		p.Jitter = r.Jitter
	}
	return p
}

// Config 是网关的全量配置。
type Config struct {
	Listen          string        `json:"listen"`
	PromptStorePath string        `json:"prompt_store_path"`
	MetricsCapacity int           `json:"metrics_capacity"`
	LogMetrics      bool          `json:"log_metrics"`
	Retry           RetryConfig   `json:"retry"`
	Models          []ModelConfig `json:"models"`
}

// Default 返回内置默认配置：两个作业要求的协议各一个模型，都指向内置 mock 上游。
func Default() *Config {
	return &Config{
		Listen:          ":8080",
		PromptStorePath: "data/prompts.json",
		MetricsCapacity: 500,
		LogMetrics:      true,
		Retry:           RetryConfig{MaxRetries: 3, BaseDelayMs: 200, MaxDelayMs: 5000, Factor: 2, Jitter: 0.2},
		Models: []ModelConfig{
			{
				Model:       "deepseek-v4-pro",
				Protocol:    "openai_responses",
				BaseURL:     "http://127.0.0.1:9090",
				BaseURLEnv:  "UPSTREAM_RESPONSES_BASE_URL",
				APIKeyEnv:   "DEEPSEEK_API_KEY",
				TimeoutMs:   60000,
				RateLimit:   resilience.LimitConfig{RequestsPerSecond: 5, Burst: 5},
				Description: "走 OpenAI Responses API 协议",
			},
			{
				Model:       "deepseek-v4-flash",
				Protocol:    "anthropic_messages",
				BaseURL:     "http://127.0.0.1:9090",
				BaseURLEnv:  "UPSTREAM_MESSAGES_BASE_URL",
				APIKeyEnv:   "DEEPSEEK_API_KEY",
				TimeoutMs:   60000,
				RateLimit:   resilience.LimitConfig{RequestsPerSecond: 2, Burst: 2},
				Description: "走 Anthropic Messages API 协议",
			},
		},
	}
}

// Load 从文件加载配置；path 为空时用默认配置。
// 流转顺序：① 读文件 ② 反序列化 ③ 环境变量覆盖监听地址 ④ 校验路由表
func Load(path string) (*Config, error) {
	cfg := Default()
	// ① + ② 有配置文件就整体替换（不做字段级合并，避免"半配置"引起困惑）
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
		}
		var fileCfg Config
		if err := json.Unmarshal(raw, &fileCfg); err != nil {
			return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
		}
		cfg = &fileCfg
	}
	// ③ 环境变量覆盖
	if v := strings.TrimSpace(os.Getenv("GATEWAY_LISTEN")); v != "" {
		cfg.Listen = v
	}
	if v := strings.TrimSpace(os.Getenv("GATEWAY_PROMPT_STORE")); v != "" {
		cfg.PromptStorePath = v
	}
	if v := strings.TrimSpace(os.Getenv("GATEWAY_METRICS_CAPACITY")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MetricsCapacity = n
		}
	}
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	// ④ 校验
	if len(cfg.Models) == 0 {
		return nil, fmt.Errorf("配置里没有任何模型路由")
	}
	seen := map[string]bool{}
	for i, m := range cfg.Models {
		if m.Model == "" {
			return nil, fmt.Errorf("models[%d].model 不能为空", i)
		}
		if seen[m.Model] {
			return nil, fmt.Errorf("模型 %q 重复配置", m.Model)
		}
		seen[m.Model] = true
		switch m.Protocol {
		case "openai_responses", "anthropic_messages", "openai_chat":
		default:
			return nil, fmt.Errorf("模型 %q 的 protocol %q 不支持", m.Model, m.Protocol)
		}
		if m.ResolveBaseURL() == "" {
			return nil, fmt.Errorf("模型 %q 没有配置 base_url", m.Model)
		}
	}
	return cfg, nil
}
