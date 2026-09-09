// Package service 是网关的编排层：把「模板渲染 → 限流 → 重试 → 适配器调用 →
// 结构化校验 → 可观测记录」串成一条主流程，HTTP 层只负责搬运。
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"llmgateway/internal/apierr"
	"llmgateway/internal/config"
	"llmgateway/internal/llm"
	"llmgateway/internal/observability"
	"llmgateway/internal/prompt"
	"llmgateway/internal/resilience"
	"llmgateway/internal/structured"
)

// ChatRequest 是网关对外的统一请求体。
// 调用方只面对这一种结构，不需要知道底层走哪种协议。
type ChatRequest struct {
	Model          string              `json:"model"`              // 必填，决定路由到哪个适配器
	Messages       []llm.Message       `json:"messages,omitempty"` // 与 prompt 二选一（也可叠加）
	System         string              `json:"system,omitempty"`   // 额外系统提示词
	Prompt         *prompt.Ref         `json:"prompt,omitempty"`   // 引用提示词模板版本
	Temperature    *float64            `json:"temperature,omitempty"`
	MaxTokens      int                 `json:"max_tokens,omitempty"`
	Stream         bool                `json:"stream,omitempty"`
	ResponseFormat *llm.ResponseFormat `json:"response_format,omitempty"` // 结构化输出约束
}

// ObsSummary 是随响应一起返回的可观测摘要。
type ObsSummary struct {
	LatencyMs float64 `json:"latency_ms"`
	TTFTMs    float64 `json:"ttft_ms,omitempty"`
	Attempts  int     `json:"attempts"`
	Retries   int     `json:"retries"`
	// BilledUsage 只在发生过重试、且被判废的尝试也消耗了 Token 时出现：
	// 此时 usage 是本次结果的用量，BilledUsage 才是这次请求的真实总花销。
	BilledUsage  *llm.Usage           `json:"billed_usage,omitempty"`
	AttemptTrace []resilience.Attempt `json:"attempt_trace,omitempty"`
}

// ChatResponse 是网关对外的统一响应体。
type ChatResponse struct {
	ID            string     `json:"id"`
	RequestID     string     `json:"request_id"`
	Model         string     `json:"model"`
	Protocol      string     `json:"protocol"`
	PromptRef     string     `json:"prompt_ref,omitempty"`
	Content       string     `json:"content"`
	Parsed        any        `json:"parsed,omitempty"` // 结构化输出时给出已解析的对象
	FinishReason  string     `json:"finish_reason"`
	Usage         llm.Usage  `json:"usage"`
	Observability ObsSummary `json:"observability"`
}

// Gateway 持有全部依赖。
type Gateway struct {
	Registry *llm.Registry
	Prompts  *prompt.Store
	Limiter  *resilience.Limiter
	Metrics  *observability.Collector
	Retry    resilience.RetryPolicy
	Cfg      *config.Config
}

// NewRequestID 生成一次调用的追踪 ID。
func NewRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("req_%d", time.Now().UnixNano())
	}
	return "req_" + hex.EncodeToString(b[:])
}

// prepared 是一次调用在进入适配器之前的全部上下文。
type prepared struct {
	route     *llm.ModelRoute
	req       *llm.Request
	promptRef string
}

// prepare 是主流程的第 ①②③ 步。
// 流转顺序：① 校验请求 ② 按 model 动态路由到适配器
// ③ 若引用了提示词模板则取出对应版本并做变量替换，渲染结果与显式 messages 拼接
func (g *Gateway) prepare(in *ChatRequest) (*prepared, *apierr.Error) {
	// ① 基本校验
	if strings.TrimSpace(in.Model) == "" {
		return nil, apierr.New(apierr.CodeInvalidRequest, "model 字段不能为空")
	}
	if in.ResponseFormat != nil {
		switch in.ResponseFormat.Type {
		case "", llm.FormatText, llm.FormatJSONObject:
		case llm.FormatJSONSchema:
			if in.ResponseFormat.JSONSchema == nil || len(in.ResponseFormat.JSONSchema.Schema) == 0 {
				return nil, apierr.New(apierr.CodeInvalidRequest, "response_format.type=json_schema 时必须提供 json_schema.schema")
			}
		default:
			return nil, apierr.New(apierr.CodeInvalidRequest, "不支持的 response_format.type: %q", in.ResponseFormat.Type)
		}
	}

	// ② 动态路由：这一步之后就确定了走哪套协议
	route, err := g.Registry.Resolve(in.Model)
	if err != nil {
		return nil, apierr.Wrap(err, apierr.CodeModelNotFound, "%s", err.Error())
	}

	lreq := &llm.Request{
		Model:          route.Model,
		UpstreamModel:  route.UpstreamModel,
		System:         in.System,
		Temperature:    in.Temperature,
		MaxTokens:      in.MaxTokens,
		Stream:         in.Stream,
		ResponseFormat: in.ResponseFormat,
	}

	// ③ 提示词模板引用：先取版本，再做变量替换
	promptRef := ""
	if in.Prompt != nil && in.Prompt.Name != "" {
		tmpl, terr := g.Prompts.Get(in.Prompt.Name, in.Prompt.Version)
		if terr != nil {
			return nil, apierr.From(terr)
		}
		rendered, rerr := prompt.Render(tmpl, in.Prompt.Variables)
		if rerr != nil {
			return nil, apierr.From(rerr)
		}
		promptRef = fmt.Sprintf("%s@v%d", tmpl.Name, tmpl.Version)
		// 模板的 system 排在调用方 system 之前，模板消息排在显式 messages 之前
		if rendered.System != "" {
			if lreq.System != "" {
				lreq.System = rendered.System + "\n\n" + lreq.System
			} else {
				lreq.System = rendered.System
			}
		}
		lreq.Messages = append(lreq.Messages, rendered.Messages...)
	}
	lreq.Messages = append(lreq.Messages, in.Messages...)

	if len(lreq.Messages) == 0 && lreq.System == "" {
		return nil, apierr.New(apierr.CodeInvalidRequest, "messages 与 prompt 至少要提供一个")
	}
	return &prepared{route: route, req: lreq, promptRef: promptRef}, nil
}

// RateLimitReporter 在限流判定完成后被调用一次（放行与拒绝都会调）。
//
// 为什么要回调而不是直接返回：判定发生在编排层，但 X-RateLimit-* 响应头
// 必须由 HTTP 层写，且成功和失败两条路径都要写——用返回值传递会漏掉错误路径。
// 这和 StreamSink 是同一个套路：编排层回调 HTTP 层。
type RateLimitReporter func(resilience.Decision)

// checkLimit 是主流程第 ④ 步：按模型独立限流。
// 超限直接返回 429，不进入重试（重试只针对上游故障，不针对本地配额）。
func (g *Gateway) checkLimit(model string) (resilience.Decision, *apierr.Error) {
	d := g.Limiter.Allow(model)
	if !d.Allowed {
		return d, resilience.Reject(model, d)
	}
	return d, nil
}

// validateStructured 是主流程第 ⑥ 步：结构化输出兜底校验。
// 流转顺序：① 抽出 JSON 主体（容忍代码围栏） ② schema 校验
// 失败返回 STRUCTURED_INVALID —— 它是 retryable 的，会触发一次重新生成
func validateStructured(rf *llm.ResponseFormat, content string) (string, any, *apierr.Error) {
	if !rf.IsStructured() {
		return content, nil, nil
	}
	// ①
	clean, parsed, err := structured.Extract(content)
	if err != nil {
		return content, nil, apierr.Wrap(err, apierr.CodeStructuredInvalid, "结构化输出校验失败: %s", err.Error())
	}
	// ②
	if rf.Type == llm.FormatJSONSchema && rf.JSONSchema != nil {
		if err := structured.ValidateSchema(parsed, rf.JSONSchema.Schema, "$"); err != nil {
			return clean, parsed, apierr.Wrap(err, apierr.CodeStructuredInvalid, "结构化输出不满足 schema: %s", err.Error())
		}
	}
	return clean, parsed, nil
}

// Invoke 执行一次非流式调用，返回统一响应。
//
// 主流程：① 准备（校验/路由/模板） → ② 限流 → ③ 指数退避重试地调用适配器
// → ④ 结构化校验（失败可触发重试） → ⑤ 落可观测记录
func (g *Gateway) Invoke(ctx context.Context, requestID string, in *ChatRequest, onLimit RateLimitReporter) (*ChatResponse, *apierr.Error) {
	start := time.Now()
	rec := observability.Record{
		RequestID:  requestID,
		StartedAt:  start,
		Model:      in.Model,
		Stream:     false,
		Structured: in.ResponseFormat.IsStructured(),
	}

	// ① 准备
	p, aerr := g.prepare(in)
	if aerr != nil {
		g.recordFailure(&rec, start, aerr)
		return nil, aerr
	}
	rec.Protocol = p.route.Protocol
	rec.PromptRef = p.promptRef

	// ② 限流；判定结果无论放行还是拒绝都回传给 HTTP 层去写响应头
	decision, aerr := g.checkLimit(p.route.Model)
	if onLimit != nil {
		onLimit(decision)
	}
	if aerr != nil {
		g.recordFailure(&rec, start, aerr)
		return nil, aerr
	}

	// ③ + ④ 重试包住「调用 + 校验」，这样模型吐出非法 JSON 也能重来一次
	var (
		final   *llm.Response
		cleaned string
		parsed  any
		// billed 累计所有尝试的真实消耗：只要上游返回了响应，Token 就已经产生并计费，
		// 哪怕这次结果因为 JSON 不合法被判废。可观测数据必须反映真实花销。
		billed llm.Usage
	)
	attempts, err := resilience.Do(ctx, g.Retry, func(attempt int) error {
		resp, callErr := p.route.Adapter.Invoke(ctx, p.req)
		if callErr != nil {
			return callErr
		}
		billed.Add(resp.Usage)
		c, pv, verr := validateStructured(p.req.ResponseFormat, resp.Content)
		if verr != nil {
			return verr
		}
		final, cleaned, parsed = resp, c, pv
		return nil
	})
	rec.Attempts = len(attempts)
	rec.AttemptTrace = attempts
	if err != nil {
		aerr := apierr.From(err)
		// 失败也要把已经烧掉的 Token 记进去，否则重试耗尽的请求会显示成 0 消耗
		rec.Usage = billed
		g.recordFailure(&rec, start, aerr)
		return nil, aerr
	}

	// ⑤ 组装响应 + 落可观测记录
	latency := float64(time.Since(start)) / float64(time.Millisecond)
	out := &ChatResponse{
		ID:           final.ID,
		RequestID:    requestID,
		Model:        p.route.Model,
		Protocol:     p.route.Protocol,
		PromptRef:    p.promptRef,
		Content:      cleaned,
		Parsed:       parsed,
		FinishReason: final.FinishReason,
		Usage:        final.Usage,
		Observability: ObsSummary{
			LatencyMs:    round2(latency),
			Attempts:     len(attempts),
			Retries:      max0(len(attempts) - 1),
			AttemptTrace: attempts,
		},
	}
	// 有过被判废的尝试时，额外暴露累计计费用量，避免调用方低估成本
	if billed != final.Usage {
		b := billed
		out.Observability.BilledUsage = &b
	}
	rec.Status = "ok"
	rec.LatencyMs = round2(latency)
	rec.Usage = billed // 指标侧记真实花销，不是最后一次的用量
	g.Metrics.Record(rec)
	return out, nil
}

// StreamSink 是流式输出的下游写入口，由 HTTP 层实现成 SSE 写入器。
type StreamSink interface {
	Meta(requestID, model, protocol, promptRef string) error // 建流后先发一条元信息
	Delta(text string) error                                 // 逐块内容
	Done(resp *ChatResponse) error                           // 结束，带完整用量与观测数据
	Fail(e *apierr.Error) error                              // 流中出错
}

// InvokeStream 执行一次流式调用。
//
// 主流程：① 准备 → ② 限流 → ③ 建流（首 Token 之前失败仍可重试）
// → ④ 逐块转发并测量首 Token 延迟 → ⑤ 结构化校验 → ⑥ 落可观测记录
func (g *Gateway) InvokeStream(ctx context.Context, requestID string, in *ChatRequest, sink StreamSink, onLimit RateLimitReporter) *apierr.Error {
	start := time.Now()
	rec := observability.Record{
		RequestID:  requestID,
		StartedAt:  start,
		Model:      in.Model,
		Stream:     true,
		Structured: in.ResponseFormat.IsStructured(),
	}

	// ① 准备
	p, aerr := g.prepare(in)
	if aerr != nil {
		g.recordFailure(&rec, start, aerr)
		return aerr
	}
	p.req.Stream = true
	rec.Protocol = p.route.Protocol
	rec.PromptRef = p.promptRef

	// ② 限流；必须在 sink 写出第一个字节之前完成，否则响应头就锁死了
	//    （首个字节由下面重试循环里的 sink.Meta 写出）
	decision, aerr := g.checkLimit(p.route.Model)
	if onLimit != nil {
		onLimit(decision)
	}
	if aerr != nil {
		g.recordFailure(&rec, start, aerr)
		return aerr
	}

	var (
		emitted  int           // 已经吐给客户端的块数，>0 之后不能再重试
		metaSent bool          // meta 事件只发一次，重试时不重复发
		ttft     time.Duration // 首 Token 延迟
		final    *llm.Response
		sinkErr  error
		billed   llm.Usage // 与非流式一致：所有产生过用量的尝试都要累计
	)

	// ③ + ④ 重试只在「一块都还没吐出去」时生效
	attempts, err := resilience.Do(ctx, g.Retry, func(attempt int) error {
		ch, streamErr := p.route.Adapter.Stream(ctx, p.req)
		if streamErr != nil {
			// 建流失败，可重试。
			// 关键：此刻还没往连接里写过任何字节，响应头没提交，
			// 所以重试全部耗尽时上层仍能返回正确的 HTTP 状态码（502/429/401…），
			// 而不是先写个 200 再用 SSE error 事件找补。
			return streamErr
		}
		// 上游确认接单之后才对客户端开流。meta 依然是客户端收到的第一个事件，
		// 只是推迟到握手结果明朗之后再发——成功路径的事件顺序完全不变。
		if !metaSent {
			if err := sink.Meta(requestID, p.route.Model, p.route.Protocol, p.promptRef); err != nil {
				return apierr.Wrap(err, apierr.CodeCanceled, "客户端已断开")
			}
			metaSent = true
		}
		var attemptErr error
		// 必须把通道读到关闭，否则适配器的 goroutine 会阻塞泄漏
		for ev := range ch {
			switch ev.Type {
			case llm.EventDelta:
				if attemptErr != nil || sinkErr != nil {
					continue
				}
				if emitted == 0 {
					ttft = time.Since(start) // 首 Token 延迟：从收到请求到第一块内容
				}
				emitted++
				if werr := sink.Delta(ev.Delta); werr != nil {
					sinkErr = werr
				}
			case llm.EventDone:
				final = ev.Response
				billed.Add(ev.Response.Usage)
			case llm.EventError:
				attemptErr = ev.Err
			}
		}
		if sinkErr != nil {
			// 客户端断开：没必要重试
			return apierr.Wrap(sinkErr, apierr.CodeCanceled, "客户端已断开")
		}
		if attemptErr != nil {
			if emitted > 0 {
				// 已经吐过内容，重试会导致输出重复，标记为不可重试
				e := apierr.From(attemptErr)
				noRetry := apierr.New(e.Code, "%s（已输出 %d 块，放弃重试）", e.Message, emitted)
				noRetry.Retryable = false
				return noRetry
			}
			return attemptErr
		}
		if final == nil {
			return apierr.New(apierr.CodeUpstreamError, "上游流结束但没有返回完整结果")
		}
		return nil
	})
	rec.Attempts = len(attempts)
	rec.AttemptTrace = attempts
	rec.TTFTMs = round2(float64(ttft) / float64(time.Millisecond))

	if err != nil {
		aerr := apierr.From(err)
		rec.Usage = billed // 失败也记已经烧掉的 Token
		g.recordFailure(&rec, start, aerr)
		_ = sink.Fail(aerr)
		return aerr
	}

	// ⑤ 结构化校验：流式下内容已经吐出去了，只能在收尾时报错，不做重试
	cleaned, parsed, verr := validateStructured(p.req.ResponseFormat, final.Content)
	if verr != nil {
		rec.Usage = billed
		g.recordFailure(&rec, start, verr)
		_ = sink.Fail(verr)
		return verr
	}

	// ⑥ 收尾
	latency := float64(time.Since(start)) / float64(time.Millisecond)
	out := &ChatResponse{
		ID:           final.ID,
		RequestID:    requestID,
		Model:        p.route.Model,
		Protocol:     p.route.Protocol,
		PromptRef:    p.promptRef,
		Content:      cleaned,
		Parsed:       parsed,
		FinishReason: final.FinishReason,
		Usage:        final.Usage,
		Observability: ObsSummary{
			LatencyMs:    round2(latency),
			TTFTMs:       rec.TTFTMs,
			Attempts:     len(attempts),
			Retries:      max0(len(attempts) - 1),
			AttemptTrace: attempts,
		},
	}
	if billed != final.Usage {
		b := billed
		out.Observability.BilledUsage = &b
	}
	rec.Status = "ok"
	rec.LatencyMs = round2(latency)
	rec.Usage = billed
	g.Metrics.Record(rec)
	// 最后一步把 done 事件写给客户端；写失败只说明客户端已经走了，不影响指标已落库
	if werr := sink.Done(out); werr != nil {
		return apierr.Wrap(werr, apierr.CodeCanceled, "客户端已断开")
	}
	return nil
}

// recordFailure 把失败也计入可观测数据（限流、参数错、上游故障都要留痕）。
func (g *Gateway) recordFailure(rec *observability.Record, start time.Time, e *apierr.Error) {
	rec.Status = "error"
	rec.ErrorCode = string(e.Code)
	rec.LatencyMs = round2(float64(time.Since(start)) / float64(time.Millisecond))
	if rec.Attempts == 0 {
		rec.Attempts = 1
	}
	g.Metrics.Record(*rec)
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

func max0(v int) int {
	if v < 0 {
		return 0
	}
	return v
}
