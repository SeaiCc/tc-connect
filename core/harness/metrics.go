package harness

import (
	"math"
	"sync"
	"time"
)

// Metrics 监控指标收集器
type Metrics struct {
	mu sync.RWMutex

	// 全局统计
	totalExecutions    int64          // 总执行次数
	successExecutions  int64          // 成功执行次数
	failedExecutions   int64          // 失败执行次数
	timeoutCount       int64          // 超时次数
	totalStartTime     time.Time      // 第一次执行开始时间
	lastExecutionTime  time.Time      // 最后执行时间

	// 按 Agent 分类的统计
	agentStats map[string]*AgentMetrics // agent_name -> metrics
}

// AgentMetrics 单个 Agent 的详细指标
type AgentMetrics struct {
	mu              sync.RWMutex
	totalExecutions int64           // 总执行次数
	successCount    int64           // 成功次数
	failedCount     int64           // 失败次数
	timeoutCount    int64           // 超时次数
	retryTotal      int64           // 总重试次数
	
	// 时长统计（纳秒）
	durations []int64 // 所有执行时长（用于计算百分位）
	minDuration time.Duration
	maxDuration time.Duration
	totalDuration time.Duration // 用于计算平均值

	// 最近执行时间
	lastSuccessTime time.Time
	lastFailTime    time.Time
}

// NewMetrics 创建新的监控指标收集器
func NewMetrics() *Metrics {
	return &Metrics{
		agentStats: make(map[string]*AgentMetrics),
		totalStartTime: time.Now(),
	}
}

// RecordSuccess 记录成功执行
func (m *Metrics) RecordSuccess(agentName string, duration time.Duration, retryCount int) {
	m.mu.Lock()
	m.totalExecutions++
	m.successExecutions++
	m.lastExecutionTime = time.Now()
	m.mu.Unlock()

	// 获取或创建 Agent 指标
	agentMetrics := m.getOrCreateAgentMetrics(agentName)
	
	agentMetrics.mu.Lock()
	agentMetrics.totalExecutions++
	agentMetrics.successCount++
	agentMetrics.retryTotal += int64(retryCount)
	agentMetrics.lastSuccessTime = time.Now()
	
	// 更新时长统计
	durationNs := duration.Nanoseconds()
	agentMetrics.durations = append(agentMetrics.durations, durationNs)
	agentMetrics.totalDuration += duration
	
	if agentMetrics.minDuration == 0 || duration < agentMetrics.minDuration {
		agentMetrics.minDuration = duration
	}
	if duration > agentMetrics.maxDuration {
		agentMetrics.maxDuration = duration
	}
	agentMetrics.mu.Unlock()
}

// RecordFailure 记录失败执行
func (m *Metrics) RecordFailure(agentName string, duration time.Duration, isTimeout bool, retryCount int) {
	m.mu.Lock()
	m.totalExecutions++
	m.failedExecutions++
	if isTimeout {
		m.timeoutCount++
	}
	m.lastExecutionTime = time.Now()
	m.mu.Unlock()

	// 获取或创建 Agent 指标
	agentMetrics := m.getOrCreateAgentMetrics(agentName)
	
	agentMetrics.mu.Lock()
	agentMetrics.totalExecutions++
	agentMetrics.failedCount++
	if isTimeout {
		agentMetrics.timeoutCount++
	}
	agentMetrics.retryTotal += int64(retryCount)
	agentMetrics.lastFailTime = time.Now()
	
	// 更新时长统计（失败也记录时长）
	durationNs := duration.Nanoseconds()
	agentMetrics.durations = append(agentMetrics.durations, durationNs)
	agentMetrics.totalDuration += duration
	
	if agentMetrics.minDuration == 0 || duration < agentMetrics.minDuration {
		agentMetrics.minDuration = duration
	}
	if duration > agentMetrics.maxDuration {
		agentMetrics.maxDuration = duration
	}
	agentMetrics.mu.Unlock()
}

// getOrCreateAgentMetrics 获取或创建 Agent 指标
func (m *Metrics) getOrCreateAgentMetrics(agentName string) *AgentMetrics {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	if _, exists := m.agentStats[agentName]; !exists {
		m.agentStats[agentName] = &AgentMetrics{
			minDuration: 0,
			durations:   make([]int64, 0),
		}
	}
	return m.agentStats[agentName]
}

// GetGlobalStats 获取全局统计
func (m *Metrics) GetGlobalStats() map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	uptime := time.Since(m.totalStartTime)
	successRate := 0.0
	if m.totalExecutions > 0 {
		successRate = float64(m.successExecutions) / float64(m.totalExecutions) * 100
	}
	
	return map[string]any{
		"total_executions":     m.totalExecutions,
		"success_executions":   m.successExecutions,
		"failed_executions":    m.failedExecutions,
		"timeout_count":        m.timeoutCount,
		"success_rate_percent": successRate,
		"uptime_seconds":       uptime.Seconds(),
		"last_execution":       m.lastExecutionTime,
		"agent_count":          len(m.agentStats),
	}
}

// GetAgentStats 获取指定 Agent 的详细统计
func (m *Metrics) GetAgentStats(agentName string) map[string]any {
	m.mu.RLock()
	metrics, exists := m.agentStats[agentName]
	m.mu.RUnlock()
	
	if !exists {
		return nil
	}
	
	metrics.mu.RLock()
	defer metrics.mu.RUnlock()
	
	total := metrics.totalExecutions
	successRate := 0.0
	if total > 0 {
		successRate = float64(metrics.successCount) / float64(total) * 100
	}
	
	// 计算平均时长
	avgDuration := time.Duration(0)
	if total > 0 {
		avgDuration = metrics.totalDuration / time.Duration(total)
	}
	
	// 计算百分位
	p50, p95, p99 := m.calculatePercentiles(metrics.durations, 50), m.calculatePercentiles(metrics.durations, 95), m.calculatePercentiles(metrics.durations, 99)
	
	return map[string]any{
		"agent_name":           agentName,
		"total_executions":     total,
		"success_count":        metrics.successCount,
		"failed_count":         metrics.failedCount,
		"timeout_count":        metrics.timeoutCount,
		"success_rate_percent": successRate,
		"total_retry_count":    metrics.retryTotal,
		"min_duration_ms":      metrics.minDuration.Milliseconds(),
		"max_duration_ms":      metrics.maxDuration.Milliseconds(),
		"avg_duration_ms":      avgDuration.Milliseconds(),
		"p50_duration_ms":      p50.Milliseconds(),
		"p95_duration_ms":      p95.Milliseconds(),
		"p99_duration_ms":      p99.Milliseconds(),
		"last_success_time":    metrics.lastSuccessTime,
		"last_fail_time":       metrics.lastFailTime,
	}
}

// GetAllAgentStats 获取所有 Agent 的统计
func (m *Metrics) GetAllAgentStats() map[string]map[string]any {
	m.mu.RLock()
	agentNames := make([]string, 0, len(m.agentStats))
	for name := range m.agentStats {
		agentNames = append(agentNames, name)
	}
	m.mu.RUnlock()
	
	result := make(map[string]map[string]any)
	for _, name := range agentNames {
		stats := m.GetAgentStats(name)
		if stats != nil {
			result[name] = stats
		}
	}
	return result
}

// calculatePercentiles 计算百分位（纳秒）
func (m *Metrics) calculatePercentiles(durations []int64, percentile int) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	
	// 创建副本避免修改原数据
	sorted := make([]int64, len(durations))
	copy(sorted, durations)
	
	// 简单排序（对于监控数据量不大时足够快）
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[i] > sorted[j] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	
	// 使用 nearest rank 方法计算索引（向上取整）
	index := int(math.Ceil(float64(percentile) * float64(len(sorted)) / 100.0)) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	
	return time.Duration(sorted[index])
}

// GetTopSlowestAgents 获取最慢的 N 个 Agent（按平均时长）
func (m *Metrics) GetTopSlowestAgents(n int) []map[string]any {
	m.mu.RLock()
	agentNames := make([]string, 0, len(m.agentStats))
	for name := range m.agentStats {
		agentNames = append(agentNames, name)
	}
	m.mu.RUnlock()
	
	if n > len(agentNames) {
		n = len(agentNames)
	}
	
	// 收集所有 Agent 的平均时长
	type agentAvg struct {
		name  string
		avgMs float64
	}
	var avgs []agentAvg
	
	for _, name := range agentNames {
		stats := m.GetAgentStats(name)
		if stats != nil {
			if avg, ok := stats["avg_duration_ms"].(int64); ok {
				avgs = append(avgs, agentAvg{name: name, avgMs: float64(avg)})
			}
		}
	}
	
	// 简单排序（降序）
	for i := 0; i < len(avgs); i++ {
		for j := i + 1; j < len(avgs); j++ {
			if avgs[i].avgMs < avgs[j].avgMs {
				avgs[i], avgs[j] = avgs[j], avgs[i]
			}
		}
	}
	
	// 取前 N 个
	result := make([]map[string]any, 0, n)
	for i := 0; i < n && i < len(avgs); i++ {
		stats := m.GetAgentStats(avgs[i].name)
		if stats != nil {
			result = append(result, stats)
		}
	}
	
	return result
}

// Reset 重置所有指标（用于测试或定期清理）
func (m *Metrics) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	m.totalExecutions = 0
	m.successExecutions = 0
	m.failedExecutions = 0
	m.timeoutCount = 0
	m.totalStartTime = time.Now()
	m.lastExecutionTime = time.Time{}
	m.agentStats = make(map[string]*AgentMetrics)
}

// ResetAgent 重置指定 Agent 的指标
func (m *Metrics) ResetAgent(agentName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	delete(m.agentStats, agentName)
}

// Export 导出所有指标（用于持久化或上报）
func (m *Metrics) Export() map[string]any {
	return map[string]any{
		"global":        m.GetGlobalStats(),
		"agents":        m.GetAllAgentStats(),
		"export_time":   time.Now(),
	}
}
