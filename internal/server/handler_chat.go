package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"llmgateway/internal/apierr"
	"llmgateway/internal/resilience"
	"llmgateway/internal/service"
)

// handleChat 是统一调用入口。
// 流转顺序：① 解析统一请求体 ② 回写限流配额头 ③ 按 stream 分流到 SSE 或 JSON 两条路径
func (s *Server) handleChat(c *gin.Context) {
	var req service.ChatRequest
	// ① 解析
	if err := c.ShouldBindJSON(&req); err != nil {
		abortErr(c, apierr.New(apierr.CodeInvalidRequest, "请求体不是合法 JSON: %s", err.Error()))
		return
	}
	// ② 限流配额透出到响应头。判定在编排层做，结果回调到这里；
	//    放行和被拒都会写，所以客户端拿到 429 时同样能看到完整配额状态。
	//    Limit/Burst 是静态配置（读一次即可），Remaining/Reset 是动态状态（每次都变）。
	writeRateLimitHeaders := func(d resilience.Decision) {
		if d.Remaining < 0 {
			return // 该模型没配限流，不写这组头，免得客户端误以为有配额约束
		}
		h := c.Writer.Header()
		h.Set("X-RateLimit-Limit", strconv.FormatFloat(d.Limit, 'f', -1, 64))
		h.Set("X-RateLimit-Burst", strconv.Itoa(d.Burst))
		h.Set("X-RateLimit-Remaining", strconv.Itoa(d.Remaining))
		// Reset 用「秒」表示桶补满还需多久，秒级以下的桶也能表达
		h.Set("X-RateLimit-Reset", strconv.FormatFloat(d.ResetAfter.Seconds(), 'f', 3, 64))
	}

	rid := requestID(c)
	// ③ 流式分支
	if req.Stream {
		sink := newSSESink(c)
		if aerr := s.gw.InvokeStream(c.Request.Context(), rid, &req, sink, writeRateLimitHeaders); aerr != nil {
			// 还没往连接里写过任何字节，就还能返回标准 JSON 错误（带正确状态码）
			if !sink.started {
				abortErr(c, aerr)
			}
			return
		}
		return
	}

	// ③ 非流式分支
	resp, aerr := s.gw.Invoke(c.Request.Context(), rid, &req, writeRateLimitHeaders)
	if aerr != nil {
		abortErr(c, aerr)
		return
	}
	// 把关键观测值同时放到响应头，方便 curl -i 直接看
	c.Writer.Header().Set("X-Latency-Ms", strconv.FormatFloat(resp.Observability.LatencyMs, 'f', 2, 64))
	c.Writer.Header().Set("X-Upstream-Protocol", resp.Protocol)
	c.Writer.Header().Set("X-Retry-Count", strconv.Itoa(resp.Observability.Retries))
	c.JSON(http.StatusOK, resp)
}

// sseSink 把统一流式事件写成 SSE 报文。
// 采用「懒写头」：只有真正开始输出内容时才提交 200 响应头，
// 这样准备阶段（路由/模板/限流）的错误仍然能以标准 JSON + 正确状态码返回。
type sseSink struct {
	c       *gin.Context
	started bool
}

func newSSESink(c *gin.Context) *sseSink { return &sseSink{c: c} }

// ensureStarted 首次写入时提交 SSE 响应头。
func (s *sseSink) ensureStarted() {
	if s.started {
		return
	}
	s.started = true
	h := s.c.Writer.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // 关掉 nginx 之类的缓冲，保证逐块下发
	s.c.Writer.WriteHeader(http.StatusOK)
	s.c.Writer.Flush()
}

// write 输出一个 SSE 事件并立刻 flush，保证「逐块返回」而不是攒到最后。
func (s *sseSink) write(event string, payload any) error {
	s.ensureStarted()
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.c.Writer, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return err
	}
	s.c.Writer.Flush()
	return nil
}

// Meta 建流后的第一条事件：告诉客户端这次实际路由到了哪套协议。
func (s *sseSink) Meta(requestID, model, protocol, promptRef string) error {
	return s.write("meta", gin.H{
		"request_id": requestID,
		"model":      model,
		"protocol":   protocol,
		"prompt_ref": promptRef,
	})
}

// Delta 逐块内容。
func (s *sseSink) Delta(text string) error {
	return s.write("delta", gin.H{"content": text})
}

// Done 结束事件：带完整文本、用量与可观测数据（含首 Token 延迟）。
func (s *sseSink) Done(resp *service.ChatResponse) error {
	if err := s.write("done", resp); err != nil {
		return err
	}
	// 补一个 [DONE] 哨兵，兼容按 OpenAI 习惯写的客户端
	s.ensureStarted()
	_, _ = fmt.Fprint(s.c.Writer, "data: [DONE]\n\n")
	s.c.Writer.Flush()
	return nil
}

// Fail 流中出错：以 error 事件下发统一错误码。
func (s *sseSink) Fail(e *apierr.Error) error {
	return s.write("error", errorBody{Error: errorPayload{
		Code:           string(e.Code),
		Message:        e.Message,
		Retryable:      e.Retryable,
		RequestID:      requestID(s.c),
		UpstreamStatus: e.UpstreamStatus,
	}})
}

// handleModels 输出模型路由表：每个模型走哪套协议、上游地址、限流参数。
func (s *Server) handleModels(c *gin.Context) {
	routes := s.gw.Registry.List()
	out := make([]gin.H, 0, len(routes))
	for _, r := range routes {
		item := gin.H{
			"model":          r.Model,
			"upstream_model": r.UpstreamModel,
			"protocol":       r.Protocol,
			"description":    r.Description,
		}
		if cfg, ok := s.gw.Limiter.Config(r.Model); ok {
			item["rate_limit"] = gin.H{
				"requests_per_second": cfg.RequestsPerSecond,
				"burst":               cfg.Burst,
			}
		}
		out = append(out, item)
	}
	c.JSON(http.StatusOK, gin.H{
		"models": out,
		"retry_policy": gin.H{
			"max_retries":   s.gw.Retry.MaxRetries,
			"base_delay_ms": s.gw.Retry.BaseDelay.Milliseconds(),
			"max_delay_ms":  s.gw.Retry.MaxDelay.Milliseconds(),
			"factor":        s.gw.Retry.Factor,
			"jitter":        s.gw.Retry.Jitter,
		},
	})
}
