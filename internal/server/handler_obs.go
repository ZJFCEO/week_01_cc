package server

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// handleMetrics 输出可观测聚合数据：按模型的调用量、Token 分类消耗、延迟与首 Token 延迟分位数。
func (s *Server) handleMetrics(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"total":    s.metrics.Totals(),
		"by_model": s.metrics.Snapshot(),
	})
}

// handleTraces 输出最近若干次调用的明细，含每次重试的退避时长与错误码。
func (s *Server) handleTraces(c *gin.Context) {
	limit := 20
	if raw := c.Query("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	records := s.metrics.Recent(limit)
	c.JSON(http.StatusOK, gin.H{"count": len(records), "traces": records})
}
