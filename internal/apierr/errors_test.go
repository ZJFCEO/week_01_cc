package apierr

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
)

// TestFromUpstreamStatus 验证上游状态码到统一错误码的分流，以及可重试标记。
func TestFromUpstreamStatus(t *testing.T) {
	cases := []struct {
		status    int
		wantCode  Code
		wantHTTP  int
		retryable bool
	}{
		{http.StatusUnauthorized, CodeUpstreamAuth, 401, false},
		{http.StatusForbidden, CodeUpstreamAuth, 401, false},
		{http.StatusBadRequest, CodeUpstreamInvalid, 400, false},
		{http.StatusTooManyRequests, CodeUpstreamRateLimit, 429, true},
		{http.StatusInternalServerError, CodeUpstreamError, 502, true},
		{http.StatusBadGateway, CodeUpstreamError, 502, true},
		{http.StatusServiceUnavailable, CodeUpstreamUnavail, 503, true},
		{http.StatusGatewayTimeout, CodeUpstreamTimeout, 504, true},
	}
	for _, c := range cases {
		e := FromUpstreamStatus(c.status, "上游错误体")
		if e.Code != c.wantCode {
			t.Errorf("状态 %d 应归一为 %s，实际 %s", c.status, c.wantCode, e.Code)
		}
		if e.HTTPStatus != c.wantHTTP {
			t.Errorf("状态 %d 对外应为 %d，实际 %d", c.status, c.wantHTTP, e.HTTPStatus)
		}
		if e.Retryable != c.retryable {
			t.Errorf("状态 %d 的可重试标记应为 %v", c.status, c.retryable)
		}
		if e.UpstreamStatus != c.status {
			t.Errorf("应保留上游原始状态码 %d", c.status)
		}
	}
}

// TestUpstreamBodyTruncated 验证超长的上游错误体会被截断。
func TestUpstreamBodyTruncated(t *testing.T) {
	long := make([]byte, 2000)
	for i := range long {
		long[i] = 'x'
	}
	e := FromUpstreamStatus(500, string(long))
	if len(e.UpstreamBody) > 600 {
		t.Fatalf("错误体应被截断，实际长度 %d", len(e.UpstreamBody))
	}
}

// TestFromTransport 验证网络层异常的分流。
func TestFromTransport(t *testing.T) {
	if got := FromTransport(context.DeadlineExceeded); got.Code != CodeUpstreamTimeout {
		t.Errorf("DeadlineExceeded 应归一为 UPSTREAM_TIMEOUT，实际 %s", got.Code)
	}
	if got := FromTransport(context.Canceled); got.Code != CodeCanceled {
		t.Errorf("Canceled 应归一为 CANCELED，实际 %s", got.Code)
	}
	if got := FromTransport(errors.New("connection refused")); got.Code != CodeUpstreamUnavail {
		t.Errorf("普通网络错误应归一为 UPSTREAM_UNAVAILABLE，实际 %s", got.Code)
	}
	var netErr net.Error = &net.DNSError{IsTimeout: true}
	if got := FromTransport(netErr); got.Code != CodeUpstreamTimeout {
		t.Errorf("net 超时应归一为 UPSTREAM_TIMEOUT，实际 %s", got.Code)
	}
}

// TestIsRetryable 验证只有 retryable 的错误会被重试器接受。
func TestIsRetryable(t *testing.T) {
	if IsRetryable(New(CodeInvalidRequest, "x")) {
		t.Error("INVALID_REQUEST 不应可重试")
	}
	if IsRetryable(New(CodeRateLimited, "x")) {
		t.Error("网关自身限流不应可重试")
	}
	if !IsRetryable(New(CodeUpstreamError, "x")) {
		t.Error("UPSTREAM_ERROR 应可重试")
	}
	if !IsRetryable(New(CodeStructuredInvalid, "x")) {
		t.Error("STRUCTURED_INVALID 应可重试（等于让模型重新生成）")
	}
}

// TestFromPreservesError 验证 From 不会把已经归一过的错误再包一层。
func TestFromPreservesError(t *testing.T) {
	orig := New(CodeModelNotFound, "没有这个模型")
	if got := From(orig); got != orig {
		t.Fatal("已经是 *Error 应原样返回")
	}
	wrapped := Wrap(errors.New("底层"), CodeUpstreamError, "上层")
	if !errors.Is(From(wrapped), wrapped) {
		t.Fatal("包装错误应可被 errors.Is 找到")
	}
}
