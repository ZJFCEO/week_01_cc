// Package resilience 提供韧性能力：指数退避重试 + 按模型独立限流。
package resilience

import (
	"context"
	"math"
	"math/rand"
	"time"

	"llmgateway/internal/apierr"
)

// RetryPolicy 描述指数退避策略。
type RetryPolicy struct {
	MaxRetries int           // 最多重试次数（不含首次），作业要求最多 3
	BaseDelay  time.Duration // 首次退避基数
	MaxDelay   time.Duration // 退避上限，防止指数爆炸
	Factor     float64       // 退避倍率
	Jitter     float64       // 抖动比例 0~1，打散并发重试避免惊群
}

// DefaultRetryPolicy 是默认策略：200ms 起步，倍率 2，最多重试 3 次。
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxRetries: 3, BaseDelay: 200 * time.Millisecond, MaxDelay: 5 * time.Second, Factor: 2, Jitter: 0.2}
}

// Attempt 记录一次尝试的结果，用于可观测性。
type Attempt struct {
	Index   int           `json:"index"`              // 从 1 开始
	Err     string        `json:"error,omitempty"`    // 本次失败的错误码
	DelayMs float64       `json:"delay_ms,omitempty"` // 本次失败后实际睡了多久
	Elapsed time.Duration `json:"-"`                  // 本次耗时
}

// Backoff 计算第 n 次重试（n 从 1 开始）的退避时长。
// 公式：min(BaseDelay * Factor^(n-1), MaxDelay) * (1 ± Jitter)
func (p RetryPolicy) Backoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	d := float64(p.BaseDelay) * math.Pow(p.Factor, float64(n-1))
	if maxD := float64(p.MaxDelay); p.MaxDelay > 0 && d > maxD {
		d = maxD
	}
	if p.Jitter > 0 {
		// 抖动区间 [1-J, 1+J]
		d *= 1 + p.Jitter*(2*rand.Float64()-1)
	}
	if d < 0 {
		d = 0
	}
	return time.Duration(d)
}

// Do 执行带指数退避的重试。
//
// 流转顺序：
//
//	① 执行一次业务函数 fn
//	② 成功则立即返回；失败先判断错误码是否 retryable（4xx 参数错不重试）
//	③ 还有重试额度就按指数退避睡一觉，睡的过程中响应 ctx 取消
//	④ 额度用尽，把最后一次错误返回，并带上全部尝试记录
func Do(ctx context.Context, p RetryPolicy, fn func(attempt int) error) ([]Attempt, error) {
	var attempts []Attempt
	var lastErr error

	for i := 0; i <= p.MaxRetries; i++ {
		start := time.Now()
		// ① 执行
		err := fn(i + 1)
		rec := Attempt{Index: i + 1, Elapsed: time.Since(start)}
		// ② 成功即返回
		if err == nil {
			attempts = append(attempts, rec)
			return attempts, nil
		}
		lastErr = err
		ae := apierr.From(err)
		rec.Err = string(ae.Code)

		// ② 不可重试的错误（如 INVALID_REQUEST / MODEL_NOT_FOUND）直接短路
		if !ae.Retryable {
			attempts = append(attempts, rec)
			return attempts, err
		}
		// ④ 额度用尽
		if i == p.MaxRetries {
			attempts = append(attempts, rec)
			break
		}
		// ③ 退避等待，期间可被取消
		delay := p.Backoff(i + 1)
		rec.DelayMs = float64(delay) / float64(time.Millisecond)
		attempts = append(attempts, rec)

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return attempts, apierr.FromTransport(ctx.Err())
		case <-timer.C:
		}
	}
	return attempts, lastErr
}
