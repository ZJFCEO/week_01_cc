package resilience

import (
	"testing"
	"time"
)

// TestLimiterBurstThenReject 验证令牌桶：先放行 burst 个，随后拒绝。
func TestLimiterBurstThenReject(t *testing.T) {
	l := NewLimiter()
	now := time.Now()
	l.now = func() time.Time { return now }
	l.Configure("m", LimitConfig{RequestsPerSecond: 2, Burst: 2})

	for i := range 2 {
		if d := l.Allow("m"); !d.Allowed {
			t.Fatalf("第 %d 次应放行", i+1)
		}
	}
	d := l.Allow("m")
	if d.Allowed {
		t.Fatal("超出 burst 后应被拒绝")
	}
	if d.RetryAfter <= 0 {
		t.Fatal("拒绝时应给出 Retry-After")
	}
}

// TestLimiterRefill 验证令牌按流逝时间惰性补充。
func TestLimiterRefill(t *testing.T) {
	l := NewLimiter()
	now := time.Now()
	l.now = func() time.Time { return now }
	l.Configure("m", LimitConfig{RequestsPerSecond: 2, Burst: 2})

	l.Allow("m")
	l.Allow("m")
	if l.Allow("m").Allowed {
		t.Fatal("桶应已空")
	}
	now = now.Add(time.Second) // 1 秒补 2 个令牌
	if !l.Allow("m").Allowed {
		t.Fatal("补充后应放行")
	}
	if !l.Allow("m").Allowed {
		t.Fatal("补充后应放行第二个")
	}
	if l.Allow("m").Allowed {
		t.Fatal("补充的令牌用完后应再次拒绝")
	}
}

// TestLimiterPerModelIsolation 验证限流按模型独立：一个模型打满不影响另一个。
func TestLimiterPerModelIsolation(t *testing.T) {
	l := NewLimiter()
	now := time.Now()
	l.now = func() time.Time { return now }
	l.Configure("slow", LimitConfig{RequestsPerSecond: 1, Burst: 1})
	l.Configure("fast", LimitConfig{RequestsPerSecond: 10, Burst: 10})

	l.Allow("slow")
	if l.Allow("slow").Allowed {
		t.Fatal("slow 应已被限流")
	}
	for i := range 10 {
		if !l.Allow("fast").Allowed {
			t.Fatalf("fast 不应被 slow 牵连，第 %d 次被拒", i+1)
		}
	}
}

// TestLimiterUnconfiguredPassthrough 验证未配置限流的模型一律放行。
func TestLimiterUnconfiguredPassthrough(t *testing.T) {
	l := NewLimiter()
	for range 100 {
		if !l.Allow("never-configured").Allowed {
			t.Fatal("未配置限流不应拦截")
		}
	}
}

// TestDecisionResetAfter 验证 X-RateLimit-Reset 的来源：桶补满还需多久。
func TestDecisionResetAfter(t *testing.T) {
	l := NewLimiter()
	now := time.Now()
	l.now = func() time.Time { return now }
	l.Configure("m", LimitConfig{RequestsPerSecond: 2, Burst: 4})

	// 满桶时拿一个：还剩 3，补满 1 个需要 0.5s
	d := l.Allow("m")
	if d.Remaining != 3 {
		t.Fatalf("扣减后应剩 3，实际 %d", d.Remaining)
	}
	if d.ResetAfter != 500*time.Millisecond {
		t.Fatalf("补满 1 个令牌应需 500ms，实际 %v", d.ResetAfter)
	}
	// 掏空：还剩 0，补满 4 个需要 2s
	for range 3 {
		d = l.Allow("m")
	}
	if d.Remaining != 0 || d.ResetAfter != 2*time.Second {
		t.Fatalf("掏空后应 Remaining=0 / ResetAfter=2s，实际 %d / %v", d.Remaining, d.ResetAfter)
	}
	// 再要就被拒，此时 Reset 仍是补满全桶的时间，RetryAfter 才是补 1 个的时间
	d = l.Allow("m")
	if d.Allowed {
		t.Fatal("桶已空应拒绝")
	}
	if d.RetryAfter != 500*time.Millisecond {
		t.Errorf("补 1 个令牌应需 500ms，实际 %v", d.RetryAfter)
	}
	if d.ResetAfter != 2*time.Second {
		t.Errorf("补满全桶应需 2s，实际 %v", d.ResetAfter)
	}
}

// TestDecisionUnconfiguredRemainingNegative 未配置限流时 Remaining 用 -1 标记，
// HTTP 层据此决定不写 X-RateLimit-* 这组头。
func TestDecisionUnconfiguredRemainingNegative(t *testing.T) {
	if d := NewLimiter().Allow("no-such-model"); !d.Allowed || d.Remaining != -1 {
		t.Fatalf("未配置限流应放行且 Remaining=-1，实际 %+v", d)
	}
}
