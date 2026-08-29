package resilience

import (
	"context"
	"testing"
	"time"

	"llmgateway/internal/apierr"
)

// TestBackoffRange 验证指数退避：均值按 factor 递增，且抖动不超出 ±Jitter。
func TestBackoffRange(t *testing.T) {
	p := RetryPolicy{MaxRetries: 3, BaseDelay: 200 * time.Millisecond, MaxDelay: 5 * time.Second, Factor: 2, Jitter: 0.2}
	expects := []time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond}
	for i, base := range expects {
		lo := time.Duration(float64(base) * 0.8)
		hi := time.Duration(float64(base) * 1.2)
		for range 50 {
			got := p.Backoff(i + 1)
			if got < lo || got > hi {
				t.Fatalf("第 %d 次退避 %v 超出 [%v, %v]", i+1, got, lo, hi)
			}
		}
	}
}

// TestBackoffCappedByMaxDelay 验证退避有上限，不会指数爆炸。
func TestBackoffCappedByMaxDelay(t *testing.T) {
	p := RetryPolicy{BaseDelay: time.Second, MaxDelay: 2 * time.Second, Factor: 10, Jitter: 0}
	if got := p.Backoff(5); got != 2*time.Second {
		t.Fatalf("应被 MaxDelay 截断为 2s，实际 %v", got)
	}
}

// TestDoRetriesUntilSuccess 验证可重试错误会一直重试到成功。
func TestDoRetriesUntilSuccess(t *testing.T) {
	p := RetryPolicy{MaxRetries: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Factor: 1}
	calls := 0
	attempts, err := Do(context.Background(), p, func(int) error {
		calls++
		if calls < 3 {
			return apierr.New(apierr.CodeUpstreamError, "上游炸了")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("第 3 次应成功，实际 %v", err)
	}
	if calls != 3 || len(attempts) != 3 {
		t.Fatalf("应尝试 3 次，实际 calls=%d attempts=%d", calls, len(attempts))
	}
	if attempts[0].Err != string(apierr.CodeUpstreamError) {
		t.Errorf("首次尝试应记录错误码，实际 %q", attempts[0].Err)
	}
	if attempts[2].Err != "" {
		t.Errorf("末次成功不应有错误码，实际 %q", attempts[2].Err)
	}
}

// TestDoStopsOnNonRetryable 验证不可重试的错误立刻短路，不浪费配额。
func TestDoStopsOnNonRetryable(t *testing.T) {
	p := RetryPolicy{MaxRetries: 3, BaseDelay: time.Millisecond, Factor: 1}
	calls := 0
	attempts, err := Do(context.Background(), p, func(int) error {
		calls++
		return apierr.New(apierr.CodeInvalidRequest, "参数不对")
	})
	if calls != 1 || len(attempts) != 1 {
		t.Fatalf("不可重试错误只应尝试 1 次，实际 %d", calls)
	}
	if apierr.From(err).Code != apierr.CodeInvalidRequest {
		t.Fatalf("错误码应原样透出，实际 %v", err)
	}
}

// TestDoRespectsMaxRetries 验证重试上限：1 次首发 + 最多 3 次重试 = 4 次尝试。
func TestDoRespectsMaxRetries(t *testing.T) {
	p := RetryPolicy{MaxRetries: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Factor: 1}
	calls := 0
	_, err := Do(context.Background(), p, func(int) error {
		calls++
		return apierr.New(apierr.CodeUpstreamError, "一直失败")
	})
	if calls != 4 {
		t.Fatalf("应尝试 4 次，实际 %d", calls)
	}
	if err == nil {
		t.Fatal("重试耗尽后应返回错误")
	}
}

// TestDoCancelStopsRetry 验证退避等待期间可被 context 取消。
func TestDoCancelStopsRetry(t *testing.T) {
	p := RetryPolicy{MaxRetries: 5, BaseDelay: 500 * time.Millisecond, MaxDelay: time.Second, Factor: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Do(ctx, p, func(int) error { return apierr.New(apierr.CodeUpstreamError, "失败") })
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Fatalf("应在 ctx 超时后尽快返回，实际耗时 %v", elapsed)
	}
	if code := apierr.From(err).Code; code != apierr.CodeUpstreamTimeout {
		t.Fatalf("ctx 超时应归一为 UPSTREAM_TIMEOUT，实际 %s", code)
	}
}
