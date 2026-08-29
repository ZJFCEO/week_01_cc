// Package apierr 定义网关的统一错误码体系。
//
// 设计目标：不管底层走的是 OpenAI Responses 协议还是 Anthropic Messages 协议，
// 上游千奇百怪的错误结构（HTTP 状态码 / error.type / error.code / 网络异常）
// 都要在适配器边界被翻译成同一套 Code，调用方只需要认这一套码。
package apierr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// Code 是网关对外暴露的统一错误码。
type Code string

const (
	CodeInvalidRequest    Code = "INVALID_REQUEST"      // 请求体本身不合法
	CodeModelNotFound     Code = "MODEL_NOT_FOUND"      // model 字段没有命中路由表
	CodePromptNotFound    Code = "PROMPT_NOT_FOUND"     // 引用的提示词模板/版本不存在
	CodePromptVarMissing  Code = "PROMPT_VAR_MISSING"   // 模板变量没有全部提供
	CodeRateLimited       Code = "RATE_LIMITED"         // 触发网关按模型的独立限流
	CodeStructuredInvalid Code = "STRUCTURED_INVALID"   // 上游返回的内容不是合法 JSON / 不满足 schema
	CodeUpstreamAuth      Code = "UPSTREAM_AUTH"        // 上游鉴权失败（Key 无效）
	CodeUpstreamInvalid   Code = "UPSTREAM_INVALID"     // 上游认为请求不合法（4xx，不可重试）
	CodeUpstreamRateLimit Code = "UPSTREAM_RATE_LIMIT"  // 上游 429
	CodeUpstreamError     Code = "UPSTREAM_ERROR"       // 上游 5xx
	CodeUpstreamTimeout   Code = "UPSTREAM_TIMEOUT"     // 上游超时
	CodeUpstreamUnavail   Code = "UPSTREAM_UNAVAILABLE" // 连不上上游（DNS/连接被拒）
	CodeCanceled          Code = "CANCELED"             // 客户端主动断开
	CodeInternal          Code = "INTERNAL"             // 网关自身 bug
)

// spec 描述一个错误码的默认 HTTP 状态与是否可重试。
// retryable=true 的错误才会进入 resilience 的指数退避重试。
type spec struct {
	status    int
	retryable bool
}

var specs = map[Code]spec{
	CodeInvalidRequest:    {http.StatusBadRequest, false},
	CodeModelNotFound:     {http.StatusNotFound, false},
	CodePromptNotFound:    {http.StatusNotFound, false},
	CodePromptVarMissing:  {http.StatusBadRequest, false},
	CodeRateLimited:       {http.StatusTooManyRequests, false}, // 网关自己限的流不重试，直接把 429 抛给调用方
	CodeStructuredInvalid: {http.StatusBadGateway, true},       // 模型没吐出合法 JSON，重试一次往往就好了
	CodeUpstreamAuth:      {http.StatusUnauthorized, false},
	CodeUpstreamInvalid:   {http.StatusBadRequest, false},
	CodeUpstreamRateLimit: {http.StatusTooManyRequests, true},
	CodeUpstreamError:     {http.StatusBadGateway, true},
	CodeUpstreamTimeout:   {http.StatusGatewayTimeout, true},
	CodeUpstreamUnavail:   {http.StatusServiceUnavailable, true},
	CodeCanceled:          {499, false},
	CodeInternal:          {http.StatusInternalServerError, false},
}

// Error 是网关内部流转的统一错误对象。
type Error struct {
	Code           Code   `json:"code"`
	Message        string `json:"message"`
	Retryable      bool   `json:"retryable"`
	HTTPStatus     int    `json:"-"`
	UpstreamStatus int    `json:"upstream_status,omitempty"` // 保留上游原始状态码，便于排查
	UpstreamBody   string `json:"upstream_body,omitempty"`   // 上游原始错误片段（截断）
	RetryAfterMs   int    `json:"retry_after_ms,omitempty"`  // 限流时告诉客户端多久后再来
	cause          error
}

func (e *Error) Error() string {
	if e.UpstreamStatus > 0 {
		return fmt.Sprintf("%s: %s (upstream=%d)", e.Code, e.Message, e.UpstreamStatus)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

// New 按错误码查表补齐 HTTP 状态与可重试标记。
func New(code Code, format string, args ...any) *Error {
	s, ok := specs[code]
	if !ok {
		s = specs[CodeInternal]
	}
	return &Error{
		Code:       code,
		Message:    fmt.Sprintf(format, args...),
		Retryable:  s.retryable,
		HTTPStatus: s.status,
	}
}

// Wrap 在保留底层 error 的同时套上统一错误码。
func Wrap(cause error, code Code, format string, args ...any) *Error {
	e := New(code, format, args...)
	e.cause = cause
	return e
}

// From 把任意 error 归一化成 *Error；已经是 *Error 的直接返回。
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var ae *Error
	if errors.As(err, &ae) {
		return ae
	}
	return Wrap(err, CodeInternal, "%s", err.Error())
}

// FromTransport 把 HTTP 客户端层面的异常（超时/连接失败/取消）分流到不同错误码。
// ① context 取消 → 区分是超时还是客户端主动断开
// ② net.Error 且 Timeout → 上游超时（可重试）
// ③ 其余网络错误 → 上游不可用（可重试）
func FromTransport(err error) *Error {
	if err == nil {
		return nil
	}
	// ① 先看 context：DeadlineExceeded 视为超时，Canceled 视为客户端断开
	if errors.Is(err, context.DeadlineExceeded) {
		return Wrap(err, CodeUpstreamTimeout, "上游请求超时")
	}
	if errors.Is(err, context.Canceled) {
		return Wrap(err, CodeCanceled, "请求已被取消")
	}
	// ② 网络层超时
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return Wrap(err, CodeUpstreamTimeout, "上游请求超时: %s", err.Error())
	}
	// ③ 其它一律当作上游不可用，允许退避重试
	return Wrap(err, CodeUpstreamUnavail, "无法连接上游: %s", err.Error())
}

// FromUpstreamStatus 把上游返回的 HTTP 状态码翻译成统一错误码。
// 这是「协议差异被适配器吃掉」的关键一环：两种协议的错误体结构不同，
// 但状态码语义是一致的，先按状态码分流，再把原始错误体截断保留。
func FromUpstreamStatus(status int, body string) *Error {
	body = strings.TrimSpace(body)
	if len(body) > 512 {
		body = body[:512] + "...(truncated)"
	}
	var code Code
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		code = CodeUpstreamAuth
	case status == http.StatusTooManyRequests:
		code = CodeUpstreamRateLimit
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		code = CodeUpstreamTimeout
	case status == http.StatusServiceUnavailable:
		code = CodeUpstreamUnavail
	case status >= 500:
		code = CodeUpstreamError
	case status >= 400:
		code = CodeUpstreamInvalid
	default:
		code = CodeUpstreamError
	}
	e := New(code, "上游返回 %d", status)
	e.UpstreamStatus = status
	e.UpstreamBody = body
	return e
}

// IsRetryable 供重试器判断该错误是否值得再试一次。
func IsRetryable(err error) bool {
	ae := From(err)
	return ae != nil && ae.Retryable
}
