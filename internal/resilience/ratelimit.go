package resilience

import (
	"sync"
	"time"

	"llmgateway/internal/apierr"
)

// LimitConfig 是单个模型的限流配置。
type LimitConfig struct {
	RequestsPerSecond float64 `json:"requests_per_second"` // 稳态速率
	Burst             int     `json:"burst"`               // 桶容量，允许的瞬时突发
}

// bucket 是一个令牌桶。惰性补充：只在被访问时按经过的时间补令牌，不需要后台 goroutine。
type bucket struct {
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

// Decision 是一次限流判定结果，用于填充 X-RateLimit-* 响应头。
type Decision struct {
	Allowed    bool
	Limit      float64
	Burst      int
	Remaining  int
	RetryAfter time.Duration
}

// Limiter 按模型维度做独立限流：每个模型一个桶，互不影响。
// 这样慢模型被打满时不会牵连快模型。
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	configs map[string]LimitConfig
	now     func() time.Time // 便于测试注入
}

func NewLimiter() *Limiter {
	return &Limiter{
		buckets: make(map[string]*bucket),
		configs: make(map[string]LimitConfig),
		now:     time.Now,
	}
}

// Configure 为某个模型设置限流参数。
func (l *Limiter) Configure(model string, cfg LimitConfig) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cfg.RequestsPerSecond <= 0 {
		return // 未配置视为不限流
	}
	if cfg.Burst <= 0 {
		cfg.Burst = 1
	}
	l.configs[model] = cfg
	l.buckets[model] = &bucket{
		rate:   cfg.RequestsPerSecond,
		burst:  float64(cfg.Burst),
		tokens: float64(cfg.Burst), // 启动时桶是满的
		last:   l.now(),
	}
}

// Config 返回某模型的限流配置，第二个返回值表示是否配置过。
func (l *Limiter) Config(model string) (LimitConfig, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.configs[model]
	return c, ok
}

// Allow 判定该模型的本次请求是否放行。
// 流转顺序：① 未配置限流直接放行 ② 按流逝时间惰性补充令牌
// ③ 有令牌就扣一个放行 ④ 没令牌算出还要等多久，返回 429 所需的 Retry-After
func (l *Limiter) Allow(model string) Decision {
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[model]
	// ① 没配置就是不限流
	if !ok {
		return Decision{Allowed: true, Remaining: -1}
	}
	// ② 惰性补充：经过 dt 秒就补 dt*rate 个令牌，上限为桶容量
	now := l.now()
	dt := now.Sub(b.last).Seconds()
	if dt > 0 {
		b.tokens += dt * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
	}
	d := Decision{Limit: b.rate, Burst: int(b.burst)}
	// ③ 放行
	if b.tokens >= 1 {
		b.tokens -= 1
		d.Allowed = true
		d.Remaining = int(b.tokens)
		return d
	}
	// ④ 拒绝：算出补满 1 个令牌需要的时间作为 Retry-After
	need := 1 - b.tokens
	d.Allowed = false
	d.Remaining = 0
	d.RetryAfter = time.Duration(need / b.rate * float64(time.Second))
	if d.RetryAfter < time.Millisecond {
		d.RetryAfter = time.Millisecond
	}
	return d
}

// Reject 构造统一的限流错误。
func Reject(model string, d Decision) *apierr.Error {
	e := apierr.New(apierr.CodeRateLimited,
		"模型 %s 触发限流（%.2f req/s, burst %d），请 %.0fms 后重试",
		model, d.Limit, d.Burst, float64(d.RetryAfter)/float64(time.Millisecond))
	e.RetryAfterMs = int(d.RetryAfter/time.Millisecond) + 1
	return e
}
