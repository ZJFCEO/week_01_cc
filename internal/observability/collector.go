// Package observability 负责记录每次调用的 Token 消耗与延迟指标。
//
// 记录两类数据：
//  1. 明细（Record）：最近 N 次调用的完整轨迹，含首 Token 延迟、重试轨迹、错误码
//  2. 聚合（ModelStats）：按模型维度的计数、Token 分类总量、延迟分位数
package observability

import (
	"encoding/json"
	"log"
	"math"
	"sort"
	"sync"
	"time"

	"llmgateway/internal/llm"
	"llmgateway/internal/resilience"
)

// Record 是一次调用的可观测明细。
type Record struct {
	RequestID    string               `json:"request_id"`
	StartedAt    time.Time            `json:"started_at"`
	Model        string               `json:"model"`
	Protocol     string               `json:"protocol"`
	Stream       bool                 `json:"stream"`
	Structured   bool                 `json:"structured"`
	PromptRef    string               `json:"prompt_ref,omitempty"` // 形如 translator@v2
	Status       string               `json:"status"`               // ok / error
	ErrorCode    string               `json:"error_code,omitempty"`
	Attempts     int                  `json:"attempts"` // 总尝试次数（含首次）
	Retries      int                  `json:"retries"`  // 重试次数 = Attempts-1
	AttemptTrace []resilience.Attempt `json:"attempt_trace,omitempty"`
	LatencyMs    float64              `json:"latency_ms"`        // 端到端总延迟
	TTFTMs       float64              `json:"ttft_ms,omitempty"` // 首 Token 延迟（流式才有意义）
	Usage        llm.Usage            `json:"usage"`
}

// ModelStats 是按模型聚合后的指标。
type ModelStats struct {
	Model           string         `json:"model"`
	Protocol        string         `json:"protocol"`
	Requests        int            `json:"requests"`
	Success         int            `json:"success"`
	Failed          int            `json:"failed"`
	StreamRequests  int            `json:"stream_requests"`
	StructuredCalls int            `json:"structured_calls"`
	TotalRetries    int            `json:"total_retries"`
	RateLimited     int            `json:"rate_limited"`
	Tokens          llm.Usage      `json:"tokens"`
	LatencyMs       Percentile     `json:"latency_ms"`
	TTFTMs          Percentile     `json:"ttft_ms"`
	ErrorCodeCounts map[string]int `json:"error_code_counts,omitempty"`
	LastRequestAt   time.Time      `json:"last_request_at"`
}

// Percentile 是一组延迟样本的统计摘要。
type Percentile struct {
	Count int     `json:"count"`
	Avg   float64 `json:"avg"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	Max   float64 `json:"max"`
}

// modelAgg 是聚合过程中的可变状态。
type modelAgg struct {
	stats     ModelStats
	latencies []float64
	ttfts     []float64
}

// Collector 是线程安全的指标收集器。明细用环形缓冲，避免长跑内存无限增长。
type Collector struct {
	mu       sync.RWMutex
	records  []Record
	capacity int
	next     int
	filled   bool
	models   map[string]*modelAgg
	logJSON  bool
}

func NewCollector(capacity int, logJSON bool) *Collector {
	if capacity <= 0 {
		capacity = 500
	}
	return &Collector{
		records:  make([]Record, capacity),
		capacity: capacity,
		models:   make(map[string]*modelAgg),
		logJSON:  logJSON,
	}
}

// Record 写入一条调用明细并更新聚合。
// 流转顺序：① 补齐派生字段 ② 写环形缓冲 ③ 更新该模型的聚合桶 ④ 打一行结构化日志
func (c *Collector) Record(r Record) {
	// ① 派生字段
	if r.Attempts > 0 {
		r.Retries = r.Attempts - 1
	}
	if r.Status == "" {
		r.Status = "ok"
	}

	c.mu.Lock()
	// ② 环形缓冲写入
	c.records[c.next] = r
	c.next = (c.next + 1) % c.capacity
	if c.next == 0 {
		c.filled = true
	}
	// ③ 聚合
	agg, ok := c.models[r.Model]
	if !ok {
		agg = &modelAgg{stats: ModelStats{Model: r.Model, ErrorCodeCounts: map[string]int{}}}
		c.models[r.Model] = agg
	}
	s := &agg.stats
	if r.Protocol != "" {
		s.Protocol = r.Protocol
	}
	s.Requests++
	s.LastRequestAt = r.StartedAt
	if r.Stream {
		s.StreamRequests++
	}
	if r.Structured {
		s.StructuredCalls++
	}
	s.TotalRetries += r.Retries
	if r.Status == "ok" {
		s.Success++
	} else {
		s.Failed++
		if r.ErrorCode != "" {
			s.ErrorCodeCounts[r.ErrorCode]++
			if r.ErrorCode == "RATE_LIMITED" {
				s.RateLimited++
			}
		}
	}
	s.Tokens.Add(r.Usage)
	if r.LatencyMs > 0 {
		agg.latencies = append(agg.latencies, r.LatencyMs)
	}
	if r.TTFTMs > 0 {
		agg.ttfts = append(agg.ttfts, r.TTFTMs)
	}
	c.mu.Unlock()

	// ④ 结构化日志，便于用 grep/jq 直接看单次调用
	if c.logJSON {
		if line, err := json.Marshal(r); err == nil {
			log.Printf("[metric] %s", line)
		}
	}
}

// Snapshot 返回按模型聚合的当前指标。
func (c *Collector) Snapshot() []ModelStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]ModelStats, 0, len(c.models))
	for _, agg := range c.models {
		s := agg.stats
		// 深拷贝 map，避免调用方拿到内部引用
		ec := make(map[string]int, len(s.ErrorCodeCounts))
		for k, v := range s.ErrorCodeCounts {
			ec[k] = v
		}
		s.ErrorCodeCounts = ec
		s.LatencyMs = summarize(agg.latencies)
		s.TTFTMs = summarize(agg.ttfts)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// Totals 返回全局汇总（所有模型合并）。
func (c *Collector) Totals() ModelStats {
	stats := c.Snapshot()
	total := ModelStats{Model: "__all__", ErrorCodeCounts: map[string]int{}}
	var lat, ttft []float64
	c.mu.RLock()
	for _, agg := range c.models {
		lat = append(lat, agg.latencies...)
		ttft = append(ttft, agg.ttfts...)
	}
	c.mu.RUnlock()
	for _, s := range stats {
		total.Requests += s.Requests
		total.Success += s.Success
		total.Failed += s.Failed
		total.StreamRequests += s.StreamRequests
		total.StructuredCalls += s.StructuredCalls
		total.TotalRetries += s.TotalRetries
		total.RateLimited += s.RateLimited
		total.Tokens.Add(s.Tokens)
		for k, v := range s.ErrorCodeCounts {
			total.ErrorCodeCounts[k] += v
		}
	}
	total.LatencyMs = summarize(lat)
	total.TTFTMs = summarize(ttft)
	return total
}

// Recent 返回最近 limit 条调用明细，按时间倒序。
func (c *Collector) Recent(limit int) []Record {
	c.mu.RLock()
	defer c.mu.RUnlock()

	n := c.next
	total := c.capacity
	if !c.filled {
		total = n
	}
	out := make([]Record, 0, total)
	// 从最新往回走：环形缓冲的写指针前一格就是最新的一条
	for i := 0; i < total; i++ {
		idx := (n - 1 - i + c.capacity*2) % c.capacity
		out = append(out, c.records[idx])
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// summarize 计算一组样本的 avg/p50/p95/max。
func summarize(vals []float64) Percentile {
	if len(vals) == 0 {
		return Percentile{}
	}
	s := make([]float64, len(vals))
	copy(s, vals)
	sort.Float64s(s)
	var sum float64
	for _, v := range s {
		sum += v
	}
	return Percentile{
		Count: len(s),
		Avg:   round2(sum / float64(len(s))),
		P50:   round2(quantile(s, 0.50)),
		P95:   round2(quantile(s, 0.95)),
		Max:   round2(s[len(s)-1]),
	}
}

// quantile 用最近秩法取分位数（样本量小时比线性插值更稳）。
func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(q*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
