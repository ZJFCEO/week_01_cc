// Command gateway 是 LLM 统一模型调用服务的入口。
//
// 启动流程：① 加载配置 ② 按路由表构造各协议适配器 ③ 配置按模型限流
// ④ 装配提示词仓库与指标收集器 ⑤ 起 HTTP 服务 ⑥ 监听信号优雅退出
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"llmgateway/internal/config"
	"llmgateway/internal/llm"
	"llmgateway/internal/observability"
	"llmgateway/internal/prompt"
	"llmgateway/internal/resilience"
	"llmgateway/internal/server"
	"llmgateway/internal/service"
)

func main() {
	var (
		configPath = flag.String("config", "", "配置文件路径，留空使用内置默认配置")
		listen     = flag.String("listen", "", "监听地址，覆盖配置文件")
		release    = flag.Bool("release", false, "以 gin release 模式运行")
	)
	flag.Parse()

	if *release {
		gin.SetMode(gin.ReleaseMode)
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	// ① 加载配置
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	// ② 按路由表构造适配器：model -> protocol -> adapter
	registry := llm.NewRegistry()
	limiter := resilience.NewLimiter()
	missingKey := false // 是否存在「上游是远端却没配 Key」的模型
	for _, m := range cfg.Models {
		opt := llm.Options{
			BaseURL: m.ResolveBaseURL(),
			APIKey:  m.ResolveAPIKey(),
			Timeout: m.Timeout(),
		}
		var adapter llm.Adapter
		switch m.Protocol {
		case llm.ProtocolOpenAIResponses:
			adapter = llm.NewOpenAIResponsesAdapter(opt)
		case llm.ProtocolAnthropicMessages:
			adapter = llm.NewAnthropicMessagesAdapter(opt)
		case llm.ProtocolOpenAIChat:
			adapter = llm.NewOpenAIChatAdapter(opt)
		default:
			log.Fatalf("模型 %s 的协议 %s 没有对应适配器", m.Model, m.Protocol)
		}
		upstreamModel := m.UpstreamModel
		if upstreamModel == "" {
			upstreamModel = m.Model
		}
		registry.Register(&llm.ModelRoute{
			Model:         m.Model,
			UpstreamModel: upstreamModel,
			Protocol:      m.Protocol,
			Adapter:       adapter,
			Description:   m.Description,
		})
		// ③ 每个模型一个独立令牌桶，互不干扰
		limiter.Configure(m.Model, m.RateLimit)

		// Key 状态分三种，必须区分清楚，否则「没读到 Key」会被误当成正常状态：
		// ① 已加载 ② 没读到但上游是本机 mock（不需要 Key）③ 没读到且上游是远端（调用必然 401）
		keyState := fmt.Sprintf("已加载(%s, 长度 %d)", m.APIKeyEnv, len(opt.APIKey))
		if opt.APIKey == "" {
			if isLocalUpstream(opt.BaseURL) {
				keyState = "未设置（上游是本机 mock，不需要 Key）"
			} else {
				keyState = fmt.Sprintf("⚠ 未设置：上游是远端，但没读到 %s，调用会 401", m.APIKeyEnv)
				missingKey = true
			}
		}
		log.Printf("[boot] 注册模型 %-20s 协议=%-20s 上游=%s 限流=%.2f req/s(burst %d) Key=%s",
			m.Model, m.Protocol, opt.BaseURL, m.RateLimit.RequestsPerSecond, m.RateLimit.Burst, keyState)
	}

	// 集中提示一次，避免每个模型都刷一遍同样的排查建议
	if missingKey {
		log.Printf("[boot] ⚠ 从 IDE 启动时 GUI 应用不会加载 ~/.zshrc，环境变量读不到是常见原因。")
		log.Printf("[boot]   解法：在运行配置的 Environment variables 里补上 Key，或改用终端 `make run` 启动。")
	}

	// ④ 提示词仓库（落盘）与指标收集器
	store, err := prompt.NewStore(cfg.PromptStorePath)
	if err != nil {
		log.Fatalf("初始化提示词仓库失败: %v", err)
	}
	metrics := observability.NewCollector(cfg.MetricsCapacity, cfg.LogMetrics)

	gw := &service.Gateway{
		Registry: registry,
		Prompts:  store,
		Limiter:  limiter,
		Metrics:  metrics,
		Retry:    cfg.Retry.Policy(),
		Cfg:      cfg,
	}

	// ⑤ 起服务
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(gw, store, metrics).Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("[boot] LLM 统一网关监听 %s（重试最多 %d 次，退避 %v 起步）",
			cfg.Listen, gw.Retry.MaxRetries, gw.Retry.BaseDelay)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务异常退出: %v", err)
		}
	}()

	// ⑥ 优雅退出
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("[shutdown] 收到退出信号，等待在途请求结束...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("[shutdown] 强制退出: %v", err)
	}
	log.Println("[shutdown] 已退出")
}

// isLocalUpstream 判断上游是不是本机（内置 mock）。
// 用来决定「没读到 Key」到底是正常状态还是配置事故。
func isLocalUpstream(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1", "0.0.0.0", "":
		return true
	}
	return false
}
