package observability

import (
	"testing"
	"time"

	"llmgateway/internal/llm"
)

func rec(model string, status string, latency, ttft float64, u llm.Usage, attempts int, code string) Record {
	return Record{
		Model: model, Protocol: "p", StartedAt: time.Now(), Status: status,
		LatencyMs: latency, TTFTMs: ttft, Usage: u, Attempts: attempts, ErrorCode: code,
	}
}

// TestAggregatePerModel 验证按模型聚合与 Token 分类累加。
func TestAggregatePerModel(t *testing.T) {
	c := NewCollector(100, false)
	c.Record(rec("a", "ok", 10, 0, llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, CachedTokens: 2, ReasoningTokens: 1}, 1, ""))
	c.Record(rec("a", "ok", 30, 0, llm.Usage{PromptTokens: 20, CompletionTokens: 5, TotalTokens: 25, CachedTokens: 3, ReasoningTokens: 2}, 1, ""))
	c.Record(rec("b", "error", 5, 0, llm.Usage{}, 4, "UPSTREAM_ERROR"))

	snap := c.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("应有 2 个模型，实际 %d", len(snap))
	}
	var a ModelStats
	for _, s := range snap {
		if s.Model == "a" {
			a = s
		}
	}
	if a.Requests != 2 || a.Success != 2 || a.Failed != 0 {
		t.Errorf("模型 a 计数有误: %+v", a)
	}
	if a.Tokens.PromptTokens != 30 || a.Tokens.CompletionTokens != 10 || a.Tokens.TotalTokens != 40 {
		t.Errorf("Token 累加有误: %+v", a.Tokens)
	}
	if a.Tokens.CachedTokens != 5 || a.Tokens.ReasoningTokens != 3 {
		t.Errorf("分类 Token 累加有误: %+v", a.Tokens)
	}

	total := c.Totals()
	if total.Requests != 3 || total.Failed != 1 {
		t.Errorf("全局汇总有误: %+v", total)
	}
	if total.TotalRetries != 3 {
		t.Errorf("重试次数应为 attempts-1=3，实际 %d", total.TotalRetries)
	}
	if total.ErrorCodeCounts["UPSTREAM_ERROR"] != 1 {
		t.Errorf("错误码分布有误: %+v", total.ErrorCodeCounts)
	}
}

// TestPercentiles 验证延迟分位数计算。
func TestPercentiles(t *testing.T) {
	c := NewCollector(100, false)
	for i := 1; i <= 100; i++ {
		c.Record(rec("m", "ok", float64(i), float64(i)/2, llm.Usage{}, 1, ""))
	}
	s := c.Snapshot()[0]
	if s.LatencyMs.Count != 100 {
		t.Fatalf("样本数应为 100，实际 %d", s.LatencyMs.Count)
	}
	if s.LatencyMs.P50 != 50 {
		t.Errorf("p50 应为 50，实际 %v", s.LatencyMs.P50)
	}
	if s.LatencyMs.P95 != 95 {
		t.Errorf("p95 应为 95，实际 %v", s.LatencyMs.P95)
	}
	if s.LatencyMs.Max != 100 {
		t.Errorf("max 应为 100，实际 %v", s.LatencyMs.Max)
	}
	if s.LatencyMs.Avg != 50.5 {
		t.Errorf("avg 应为 50.5，实际 %v", s.LatencyMs.Avg)
	}
	if s.TTFTMs.Count != 100 {
		t.Errorf("首 Token 延迟应单独统计，实际样本数 %d", s.TTFTMs.Count)
	}
}

// TestTTFTOnlyForStream 验证 ttft=0 的非流式调用不进入 TTFT 统计。
func TestTTFTOnlyForStream(t *testing.T) {
	c := NewCollector(10, false)
	c.Record(rec("m", "ok", 10, 0, llm.Usage{}, 1, ""))
	c.Record(rec("m", "ok", 10, 4, llm.Usage{}, 1, ""))
	s := c.Snapshot()[0]
	if s.TTFTMs.Count != 1 {
		t.Fatalf("只有 1 条有首 Token 延迟，实际 %d", s.TTFTMs.Count)
	}
}

// TestRecentRingBuffer 验证明细环形缓冲：只保留最近 N 条且按时间倒序。
func TestRecentRingBuffer(t *testing.T) {
	c := NewCollector(3, false)
	for _, id := range []string{"r1", "r2", "r3", "r4", "r5"} {
		r := rec("m", "ok", 1, 0, llm.Usage{}, 1, "")
		r.RequestID = id
		c.Record(r)
	}
	got := c.Recent(10)
	if len(got) != 3 {
		t.Fatalf("容量为 3 应只保留 3 条，实际 %d", len(got))
	}
	if got[0].RequestID != "r5" || got[2].RequestID != "r3" {
		t.Fatalf("应按时间倒序返回最近 3 条，实际 %v %v %v", got[0].RequestID, got[1].RequestID, got[2].RequestID)
	}
	if len(c.Recent(2)) != 2 {
		t.Error("limit 应生效")
	}
}

// TestRetriesDerived 验证 Retries 由 Attempts 派生。
func TestRetriesDerived(t *testing.T) {
	c := NewCollector(10, false)
	c.Record(rec("m", "ok", 1, 0, llm.Usage{}, 3, ""))
	if got := c.Recent(1)[0].Retries; got != 2 {
		t.Fatalf("attempts=3 应派生 retries=2，实际 %d", got)
	}
}
