// Package server 是 HTTP 接入层：只负责协议搬运（解析请求、写 SSE、映射状态码），
// 业务编排全部在 service 层。
package server

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"llmgateway/internal/apierr"
	"llmgateway/internal/observability"
	"llmgateway/internal/prompt"
	"llmgateway/internal/service"
)

// Server 持有 HTTP 层需要的依赖。
type Server struct {
	gw      *service.Gateway
	prompts *prompt.Store
	metrics *observability.Collector
}

func New(gw *service.Gateway, prompts *prompt.Store, metrics *observability.Collector) *Server {
	return &Server{gw: gw, prompts: prompts, metrics: metrics}
}

// headerRequestID 是贯穿一次调用的追踪头。
const headerRequestID = "X-Request-Id"

// Router 装配全部路由。
func (s *Server) Router() *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery(), requestIDMiddleware(), accessLogMiddleware())

	r.GET("/healthz", s.handleHealth)   // 存活探针
	r.GET("/v1/models", s.handleModels) // 查看模型路由表（含各自协议与限流配置）

	// 统一调用入口：stream=true 时以 SSE 返回，否则返回 JSON
	r.POST("/v1/chat", s.handleChat)
	r.POST("/v1/chat/completions", s.handleChat) // 兼容习惯路径

	// 提示词模板版本管理
	r.POST("/v1/prompts", s.handleCreatePrompt)                 // 新建一个版本（同名自动 +1）
	r.GET("/v1/prompts", s.handleListPrompts)                   // 列出全部模板
	r.GET("/v1/prompts/:name", s.handleGetPrompt)               // 取某版本（?version=N，省略取 latest）
	r.GET("/v1/prompts/:name/versions", s.handlePromptVersions) // 列出历史版本
	r.POST("/v1/prompts/:name/render", s.handleRenderPrompt)    // 只做渲染预览，不调模型

	// 可观测性
	r.GET("/v1/metrics", s.handleMetrics) // 按模型聚合的 Token/延迟指标
	r.GET("/v1/traces", s.handleTraces)   // 最近 N 次调用明细

	return r
}

// requestIDMiddleware 为每次请求分配追踪 ID，并回写到响应头。
func requestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(headerRequestID)
		if id == "" {
			id = service.NewRequestID()
		}
		c.Set("request_id", id)
		c.Writer.Header().Set(headerRequestID, id)
		c.Next()
	}
}

// accessLogMiddleware 打印一行访问日志，含耗时。
func accessLogMiddleware() gin.HandlerFunc {
	return gin.LoggerWithFormatter(func(p gin.LogFormatterParams) string {
		return "[http] " + p.TimeStamp.Format(time.RFC3339) + " " + p.Method + " " + p.Path +
			" " + itoa(p.StatusCode) + " " + p.Latency.String() + "\n"
	})
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [12]byte
	pos := len(buf)
	neg := i < 0
	if neg {
		i = -i
	}
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

func requestID(c *gin.Context) string {
	if v, ok := c.Get("request_id"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return service.NewRequestID()
}

// errorBody 是统一错误响应体。
type errorBody struct {
	Error errorPayload `json:"error"`
}

type errorPayload struct {
	Code           string `json:"code"`
	Message        string `json:"message"`
	Retryable      bool   `json:"retryable"`
	RequestID      string `json:"request_id"`
	UpstreamStatus int    `json:"upstream_status,omitempty"`
	UpstreamBody   string `json:"upstream_body,omitempty"`
	RetryAfterMs   int    `json:"retry_after_ms,omitempty"`
}

// abortErr 把统一错误码翻译成 HTTP 响应。
// 限流错误额外补 Retry-After 头，方便客户端按提示退避。
func abortErr(c *gin.Context, e *apierr.Error) {
	status := e.HTTPStatus
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	// 限流类错误补 Retry-After（秒）与毫秒级精确值，客户端可据此退避
	if e.Code == apierr.CodeRateLimited || e.Code == apierr.CodeUpstreamRateLimit {
		secs := 1
		if e.RetryAfterMs > 0 {
			c.Writer.Header().Set("X-Retry-After-Ms", itoa(e.RetryAfterMs))
			secs = (e.RetryAfterMs + 999) / 1000
			if secs < 1 {
				secs = 1
			}
		}
		c.Writer.Header().Set("Retry-After", itoa(secs))
	}
	c.AbortWithStatusJSON(status, errorBody{Error: errorPayload{
		Code:           string(e.Code),
		Message:        e.Message,
		Retryable:      e.Retryable,
		RequestID:      requestID(c),
		UpstreamStatus: e.UpstreamStatus,
		UpstreamBody:   e.UpstreamBody,
		RetryAfterMs:   e.RetryAfterMs,
	}})
}

func (s *Server) handleHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok", "time": time.Now().UTC()})
}
