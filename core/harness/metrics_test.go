package harness

import (
	"testing"
	"time"
)

func TestMetrics_NewMetrics(t *testing.T) {
	m := NewMetrics()
	if m == nil {
		t.Fatal("expected non-nil metrics")
	}
	if m.agentStats == nil {
		t.Fatal("expected non-nil agentStats")
	}
}

func TestMetrics_RecordSuccess(t *testing.T) {
	m := NewMetrics()
	
	// 记录成功
	m.RecordSuccess("agent1", 100*time.Millisecond, 0)
	m.RecordSuccess("agent1", 200*time.Millisecond, 1)
	m.RecordSuccess("agent2", 150*time.Millisecond, 0)
	
	// 检查全局统计
	global := m.GetGlobalStats()
	if global["total_executions"] != int64(3) {
		t.Errorf("expected total_executions=3, got %v", global["total_executions"])
	}
	if global["success_executions"] != int64(3) {
		t.Errorf("expected success_executions=3, got %v", global["success_executions"])
	}
	
	// 检查成功率
	successRate := global["success_rate_percent"].(float64)
	if successRate != 100.0 {
		t.Errorf("expected success_rate=100.0, got %v", successRate)
	}
	
	// 检查 agent1 统计
	agent1Stats := m.GetAgentStats("agent1")
	if agent1Stats == nil {
		t.Fatal("expected agent1 stats")
	}
	if agent1Stats["total_executions"] != int64(2) {
		t.Errorf("expected agent1 total_executions=2, got %v", agent1Stats["total_executions"])
	}
	if agent1Stats["success_count"] != int64(2) {
		t.Errorf("expected agent1 success_count=2, got %v", agent1Stats["success_count"])
	}
	
	// 检查时长统计
	avgDuration := agent1Stats["avg_duration_ms"].(int64)
	if avgDuration != 150 { // (100+200)/2 = 150
		t.Errorf("expected avg_duration_ms=150, got %v", avgDuration)
	}
	
	minDuration := agent1Stats["min_duration_ms"].(int64)
	if minDuration != 100 {
		t.Errorf("expected min_duration_ms=100, got %v", minDuration)
	}
	
	maxDuration := agent1Stats["max_duration_ms"].(int64)
	if maxDuration != 200 {
		t.Errorf("expected max_duration_ms=200, got %v", maxDuration)
	}
	
	// 检查重试总次数
	retryTotal := agent1Stats["total_retry_count"].(int64)
	if retryTotal != 1 {
		t.Errorf("expected total_retry_count=1, got %v", retryTotal)
	}
}

func TestMetrics_RecordFailure(t *testing.T) {
	m := NewMetrics()
	
	// 记录失败
	m.RecordFailure("agent1", 300*time.Millisecond, false, 0)
	m.RecordFailure("agent1", 500*time.Millisecond, true, 2) // 超时
	m.RecordFailure("agent2", 400*time.Millisecond, false, 0)
	
	// 检查全局统计
	global := m.GetGlobalStats()
	if global["failed_executions"] != int64(3) {
		t.Errorf("expected failed_executions=3, got %v", global["failed_executions"])
	}
	if global["timeout_count"] != int64(1) {
		t.Errorf("expected timeout_count=1, got %v", global["timeout_count"])
	}
	
	// 检查 agent1 统计
	agent1Stats := m.GetAgentStats("agent1")
	if agent1Stats["failed_count"] != int64(2) {
		t.Errorf("expected agent1 failed_count=2, got %v", agent1Stats["failed_count"])
	}
	if agent1Stats["timeout_count"] != int64(1) {
		t.Errorf("expected agent1 timeout_count=1, got %v", agent1Stats["timeout_count"])
	}
	if agent1Stats["total_retry_count"] != int64(2) {
		t.Errorf("expected agent1 total_retry_count=2, got %v", agent1Stats["total_retry_count"])
	}
}

func TestMetrics_GetAllAgentStats(t *testing.T) {
	m := NewMetrics()
	
	m.RecordSuccess("agent1", 100*time.Millisecond, 0)
	m.RecordSuccess("agent2", 200*time.Millisecond, 0)
	m.RecordFailure("agent3", 300*time.Millisecond, true, 1)
	
	allStats := m.GetAllAgentStats()
	
	if len(allStats) != 3 {
		t.Errorf("expected 3 agents, got %d", len(allStats))
	}
	
	if _, exists := allStats["agent1"]; !exists {
		t.Error("expected agent1 in stats")
	}
	if _, exists := allStats["agent2"]; !exists {
		t.Error("expected agent2 in stats")
	}
	if _, exists := allStats["agent3"]; !exists {
		t.Error("expected agent3 in stats")
	}
}

func TestMetrics_GetTopSlowestAgents(t *testing.T) {
	m := NewMetrics()
	
	// 记录不同 Agent 的执行时长
	for i := 0; i < 10; i++ {
		m.RecordSuccess("slow-agent", 1000*time.Millisecond, 0)
	}
	for i := 0; i < 10; i++ {
		m.RecordSuccess("medium-agent", 500*time.Millisecond, 0)
	}
	for i := 0; i < 10; i++ {
		m.RecordSuccess("fast-agent", 100*time.Millisecond, 0)
	}
	
	topSlowest := m.GetTopSlowestAgents(2)
	
	if len(topSlowest) != 2 {
		t.Errorf("expected 2 agents, got %d", len(topSlowest))
	}
	
	// 第一个应该是最慢的
	firstAgent := topSlowest[0]["agent_name"].(string)
	if firstAgent != "slow-agent" {
		t.Errorf("expected first agent to be 'slow-agent', got %s", firstAgent)
	}
	
	// 第二个应该是中等的
	secondAgent := topSlowest[1]["agent_name"].(string)
	if secondAgent != "medium-agent" {
		t.Errorf("expected second agent to be 'medium-agent', got %s", secondAgent)
	}
}

func TestMetrics_Reset(t *testing.T) {
	m := NewMetrics()
	
	m.RecordSuccess("agent1", 100*time.Millisecond, 0)
	m.RecordFailure("agent2", 200*time.Millisecond, false, 0)
	
	if m.GetGlobalStats()["total_executions"] != int64(2) {
		t.Fatal("expected 2 executions before reset")
	}
	
	m.Reset()
	
	if m.GetGlobalStats()["total_executions"] != int64(0) {
		t.Errorf("expected total_executions=0 after reset, got %v", m.GetGlobalStats()["total_executions"])
	}
	
	if len(m.GetAllAgentStats()) != 0 {
		t.Errorf("expected no agents after reset, got %d", len(m.GetAllAgentStats()))
	}
}

func TestMetrics_ResetAgent(t *testing.T) {
	m := NewMetrics()
	
	m.RecordSuccess("agent1", 100*time.Millisecond, 0)
	m.RecordSuccess("agent2", 200*time.Millisecond, 0)
	
	m.ResetAgent("agent1")
	
	allStats := m.GetAllAgentStats()
	if len(allStats) != 1 {
		t.Errorf("expected 1 agent after reset, got %d", len(allStats))
	}
	if _, exists := allStats["agent1"]; exists {
		t.Error("expected agent1 to be removed")
	}
	if _, exists := allStats["agent2"]; !exists {
		t.Error("expected agent2 to still exist")
	}
}

func TestMetrics_Export(t *testing.T) {
	m := NewMetrics()
	
	m.RecordSuccess("agent1", 100*time.Millisecond, 0)
	
	exported := m.Export()
	
	if exported["global"] == nil {
		t.Error("expected global stats in export")
	}
	if exported["agents"] == nil {
		t.Error("expected agents stats in export")
	}
	if exported["export_time"] == nil {
		t.Error("expected export_time in export")
	}
}

func TestMetrics_CalculatePercentiles(t *testing.T) {
	m := NewMetrics()
	
	// 记录 10 个不同时长
	durations := []time.Duration{
		10, 20, 30, 40, 50, 60, 70, 80, 90, 100, // 毫秒
	}
	
	for _, d := range durations {
		m.RecordSuccess("test-agent", d*time.Millisecond, 0)
	}
	
	agentStats := m.GetAgentStats("test-agent")
	
	p50 := agentStats["p50_duration_ms"].(int64)
	if p50 < 40 || p50 > 60 {
		t.Errorf("expected p50 between 40-60, got %d", p50)
	}
	
	p95 := agentStats["p95_duration_ms"].(int64)
	if p95 < 90 || p95 > 100 {
		t.Errorf("expected p95 between 90-100, got %d", p95)
	}
	
	p99 := agentStats["p99_duration_ms"].(int64)
	if p99 != 100 {
		t.Errorf("expected p99=100, got %d", p99)
	}
}

func TestMetrics_ConcurrentSafety(t *testing.T) {
	m := NewMetrics()
	
	// 并发记录
	done := make(chan bool)
	for i := 0; i < 100; i++ {
		go func(idx int) {
			agentName := "agent-" + string(rune('0'+idx%10))
			m.RecordSuccess(agentName, time.Duration(idx)*time.Millisecond, 0)
			done <- true
		}(i)
	}
	
	// 等待所有完成
	for i := 0; i < 100; i++ {
		<-done
	}
	
	// 检查总数
	global := m.GetGlobalStats()
	if global["total_executions"] != int64(100) {
		t.Errorf("expected total_executions=100, got %v", global["total_executions"])
	}
}
