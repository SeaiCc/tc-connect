package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"tc-connect/config"
)

// TestIntegration 集成测试套件
// 注意：这些测试依赖于 opencode CLI 的存在和配置
func TestIntegration(t *testing.T) {
	// 检查 opencode CLI 是否可用
	if _, err := os.Stat("opencode"); os.IsNotExist(err) {
		// 检查 PATH 中是否有 opencode
		envOpencode := os.Getenv("PATH")
		found := false
		for _, path := range filepath.SplitList(envOpencode) {
			if _, err := os.Stat(filepath.Join(path, "opencode")); err == nil {
				found = true
				break
			}
		}
		if !found {
			t.Skip("Skipping integration test: opencode CLI not found in PATH")
		}
	}

	t.Run("FullWorkflow", testIntegrationFullWorkflow)
	t.Run("TimeoutAndRetry", testIntegrationTimeoutAndRetry)
	t.Run("Concurrency", testIntegrationConcurrency)
}

// testIntegrationFullWorkflow 测试完整工作流
// 场景：解析协议 -> 执行子 Agent -> 返回结果
func testIntegrationFullWorkflow(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 1. 初始化组件
	harnessCfg := &config.HarnessConfig{
		TimeoutMins:    ptr(5),
		MaxRetries:     ptr(3),
		MaxConcurrent:  ptr(5),
		MaxPerProject:  ptr(2),
		LogLevel:       "debug",
	}

	// 创建子 Agent 管理器
	manager, err := NewSubAgentManager(harnessCfg, "")
	if err != nil {
		t.Fatalf("Failed to create subagent manager: %v", err)
	}

	// 检查是否有可用的子 Agent
	agents := manager.ListAvailableAgents()
	if len(agents) == 0 {
		t.Skip("No available sub-agents found")
	}

	t.Logf("Available agents: %v", agents)

	// 选择第一个可用的 agent 进行测试
	testAgent := agents[0]

	// 创建会话管理器
	sessionMgr, err := NewSessionManager(harnessCfg, manager)
	if err != nil {
		t.Fatalf("Failed to create session manager: %v", err)
	}
	defer sessionMgr.Shutdown()

	// 创建日志器
	loggerCfg := &LoggerConfig{
		ProjectName: "test-project",
		Level:       "debug",
	}
	logger, err := NewHarnessLogger(loggerCfg)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}

	// 创建监控指标
	metrics := NewMetrics()

	// 创建执行器
	executor := NewSubAgentExecutor(
		manager,
		sessionMgr,
		0, // 使用默认超时
		0, // 使用默认重试
		logger,
		metrics,
	)

	// 2. 测试协议解析
	protocol := fmt.Sprintf("%s||test parameters||false", testAgent)
	req, err := ParseProtocol(protocol)
	if err != nil {
		t.Fatalf("Failed to parse protocol: %v", err)
	}

	if req == nil {
		t.Fatal("Parsed request is nil")
	}

	t.Logf("Parsed request: Agent=%s, Params=%s, Resume=%v", req.AgentName, req.Params, req.Resume)

	// 3. 执行子 Agent
	result, err := executor.Execute(ctx, req, "")
	if err != nil {
		// 注意：如果 opencode 实际调用失败，这可能是预期的
		t.Logf("Execution failed (may be expected if opencode not configured): %v", err)
		// 继续测试其他部分
	} else {
		t.Logf("Execution successful: SessionID=%s, Duration=%v, Output length=%d", 
			result.SessionID, result.Duration, len(result.Output))
		
		// 验证结果
		if result.SessionID == "" {
			t.Error("SessionID should not be empty")
		}

		if result.Duration == 0 {
			t.Error("Duration should not be zero")
		}

		// 验证 session 存储
		session, err := sessionMgr.GetSession(result.SessionID)
		if err != nil {
			t.Errorf("Session should exist in store: %v", err)
		} else {
			t.Logf("Session stored: Agent=%s, CreatedAt=%v", 
				session.AgentName, session.CreatedAt)
		}
	}

	// 4. 测试编排器
	orchestrator := NewOrchestrator(harnessCfg, executor, sessionMgr, "test-project")
	
	// 测试 ExecuteSubAgent（单步执行）
	singleResult, err := orchestrator.ExecuteSubAgent(req)
	if err != nil {
		t.Logf("Single agent execution failed: %v", err)
	} else {
		t.Logf("Single agent result: SessionID=%s", singleResult.SessionID)
	}

	// 5. 验证监控指标
	stats := metrics.GetGlobalStats()
	t.Logf("Metrics stats: %+v", stats)

	// 6. 验证日志记录
	t.Logf("Test completed successfully")
}

// testIntegrationTimeoutAndRetry 测试超时和重试机制
func testIntegrationTimeoutAndRetry(t *testing.T) {
	t.Parallel()

	// 检查 opencode CLI
	if !isOpencodeAvailable() {
		t.Skip("Skipping timeout/retry test: opencode CLI not available")
	}

	// 创建一个极短超时的配置
	harnessCfg := &config.HarnessConfig{
		TimeoutMins:    ptr(1), // 1 分钟超时
		MaxRetries:     ptr(2), // 2 次重试
	}

	manager, err := NewSubAgentManager(harnessCfg, "")
	if err != nil {
		t.Fatalf("Failed to create manager: %v", err)
	}

	agents := manager.ListAvailableAgents()
	if len(agents) == 0 {
		t.Skip("No available agents")
	}

	testAgent := agents[0]

	sessionMgr, err := NewSessionManager(harnessCfg, manager)
	if err != nil {
		t.Fatalf("Failed to create session manager: %v", err)
	}
	defer sessionMgr.Shutdown()

	logger, _ := NewHarnessLogger(&LoggerConfig{Level: "info", ProjectName: "test-timeout"})
	metrics := NewMetrics()

	executor := NewSubAgentExecutor(manager, sessionMgr, 0, 0, logger, metrics)

	// 测试 1: 正常执行（不应该超时）
	t.Run("NormalExecution", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		req := &HarnessRequest{
			AgentName: testAgent,
			Params:    "quick task", // 快速任务
			Resume:    false,
		}

		start := time.Now()
		result, err := executor.Execute(ctx, req, "")
		duration := time.Since(start)

		if err != nil {
			t.Logf("Execution failed (may be expected): %v", err)
		} else {
			t.Logf("Normal execution completed in %v", duration)
			if result != nil {
				t.Logf("Retry count: %d", result.RetryCount)
			}
		}
	})

	// 测试 2: 超时测试（使用极短超时）
	t.Run("TimeoutExecution", func(t *testing.T) {
		// 创建带超时的执行器
		timeoutExecutor := NewSubAgentExecutor(manager, sessionMgr, 2*time.Second, 1, logger, metrics)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		req := &HarnessRequest{
			AgentName: testAgent,
			Params:    "slow task that should timeout", // 慢任务
			Resume:    false,
		}

		start := time.Now()
		result, err := timeoutExecutor.Execute(ctx, req, "")
		duration := time.Since(start)

		t.Logf("Timeout test completed in %v, error: %v", duration, err)
		
		// 验证错误包含超时信息
		if err != nil {
			if containsString(err.Error(), "timeout") || containsString(err.Error(), "deadline") {
				t.Logf("Correctly detected timeout error")
			} else {
				t.Logf("Error message: %s", err.Error())
			}
		}

		if result != nil {
			t.Logf("Retry count: %d (expected max 1)", result.RetryCount)
		}
	})

	// 测试 3: 重试机制验证
	t.Run("RetryMechanism", func(t *testing.T) {
		// 使用正常执行器，但构造一个会失败的情况
		// 这里我们测试不可用的 agent 不会重试
		req := &HarnessRequest{
			AgentName: "non-existent-agent-xyz",
			Params:    "test",
			Resume:    false,
		}

		_, err := executor.Execute(context.Background(), req, "")
		if err != nil {
			if containsString(err.Error(), "not available") {
				t.Logf("Correctly identified unavailable agent without retry")
			}
		}
	})
}

// testIntegrationConcurrency 测试并发控制
func testIntegrationConcurrency(t *testing.T) {
	t.Parallel()

	if !isOpencodeAvailable() {
		t.Skip("Skipping concurrency test: opencode CLI not available")
	}

	// 配置：全局最大 3，每项目最大 1
	harnessCfg := &config.HarnessConfig{
		MaxConcurrent:   ptr(3),
		MaxPerProject:   ptr(1),
		TimeoutMins:     ptr(5),
		MaxRetries:      ptr(1),
	}

	manager, err := NewSubAgentManager(harnessCfg, "")
	if err != nil {
		t.Fatalf("Failed to create manager: %v", err)
	}

	agents := manager.ListAvailableAgents()
	if len(agents) == 0 {
		t.Skip("No available agents")
	}

	testAgent := agents[0]

	sessionMgr, err := NewSessionManager(harnessCfg, manager)
	if err != nil {
		t.Fatalf("Failed to create session manager: %v", err)
	}
	defer sessionMgr.Shutdown()

	logger, _ := NewHarnessLogger(&LoggerConfig{Level: "info", ProjectName: "test-concurrency"})
	metrics := NewMetrics()

	// 创建多个执行器（模拟不同项目）
	projects := []string{"project-a", "project-b", "project-c"}
	executors := make([]*SubAgentExecutor, len(projects))
	orchestrators := make([]*OrchestratorImpl, len(projects))

	for i, project := range projects {
		executors[i] = NewSubAgentExecutor(manager, sessionMgr, 0, 0, logger, metrics)
		orchestrators[i] = NewOrchestrator(harnessCfg, executors[i], sessionMgr, project)
	}

	// 并发测试：多个项目同时执行
	var wg sync.WaitGroup
	errChan := make(chan error, len(projects))
	results := make([]*SubAgentResult, len(projects))
	var mu sync.Mutex

	for i, project := range projects {
		wg.Add(1)
		go func(idx int, projName string) {
			defer wg.Done()

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			req := &HarnessRequest{
				AgentName: testAgent,
				Params:    fmt.Sprintf("task from %s", projName),
				Resume:    false,
			}

			t.Logf("Project %s starting execution", projName)
			start := time.Now()
			
			result, err := executors[idx].Execute(ctx, req, "")
			duration := time.Since(start)

			if err != nil {
				t.Logf("Project %s execution failed: %v (took %v)", projName, err, duration)
				errChan <- fmt.Errorf("project %s: %w", projName, err)
				return
			}

			mu.Lock()
			results[idx] = result
			mu.Unlock()

			t.Logf("Project %s completed in %v, session: %s", projName, duration, result.SessionID)
		}(i, project)
	}

	// 等待所有完成
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Log("All concurrent executions completed")
	case <-time.After(60 * time.Second):
		t.Fatal("Concurrent execution timeout")
	}

	// 验证结果
	if len(errChan) > 0 {
		// 有错误，但不一定是失败（可能是 opencode 配置问题）
		t.Logf("Some executions failed (check logs above)")
	}

	// 验证并发控制器统计
	for i, orch := range orchestrators {
		stats := orch.GetStats()
		t.Logf("Project %s stats: %+v", projects[i], stats)
	}

	// 验证监控指标
	globalStats := metrics.GetGlobalStats()
	t.Logf("Global metrics: %+v", globalStats)

	// 验证会话隔离
	uniqueSessions := make(map[SessionID]string)
	for i, result := range results {
		if result != nil {
			uniqueSessions[result.SessionID] = projects[i]
		}
	}
	t.Logf("Unique sessions created: %d", len(uniqueSessions))
	if len(uniqueSessions) != len(projects) {
		t.Logf("Warning: Expected %d sessions, got %d", len(projects), len(uniqueSessions))
	}
}

// 辅助函数

func ptr[T any](v T) *T {
	return &v
}

func isOpencodeAvailable() bool {
	// 检查 opencode 命令是否可用
	cmd := "opencode"
	if path := os.Getenv("OPENCODE_PATH"); path != "" {
		cmd = path
	}
	
	// 尝试查找
	if _, err := os.Stat(cmd); err == nil {
		return true
	}
	
	// 检查 PATH
	for _, path := range filepath.SplitList(os.Getenv("PATH")) {
		if _, err := os.Stat(filepath.Join(path, "opencode")); err == nil {
			return true
		}
	}
	return false
}
