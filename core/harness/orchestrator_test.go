package harness

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"tc-connect/config"
)

// TestOrchestrator 测试编排器主循环
func TestOrchestrator(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		expectedSteps int
		expectedError bool
		expectedState OrchestratorState
	}{
		{
			name:          "正常结束 - END 标识",
			input:         "END",
			expectedSteps: 0,
			expectedError: false,
			expectedState: StateEnd,
		},
		{
			name:          "协议解析失败",
			input:         "invalid-protocol",
			expectedSteps: 0,
			expectedError: true,
			expectedState: StateError,
		},
		{
			name:          "空输入",
			input:         "",
			expectedSteps: 0,
			expectedError: true,
			expectedState: StateError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 准备配置
			cfg := &config.HarnessConfig{
				Enabled: ptrBool(true),
			}

			// 创建编排器（不使用实际的 executor 和 sessionMgr）
			orch := NewOrchestrator(cfg, nil, nil, "test-project")

			// 执行
			ctx := context.Background()
			result, err := orch.Start(ctx, tt.input)

			// 验证结果
			if tt.expectedError {
				if err == nil {
					t.Errorf("expected error but got nil")
				}
				if orch.GetState() != tt.expectedState {
					t.Errorf("expected state %s but got %s", tt.expectedState, orch.GetState())
				}
			} else {
				if err != nil {
					t.Errorf("expected no error but got: %v", err)
				}
				if result == nil {
					t.Fatal("expected result but got nil")
				}
				if !result.Success {
					t.Errorf("expected success but got false")
				}
				if result.Steps != tt.expectedSteps {
					t.Errorf("expected %d steps but got %d", tt.expectedSteps, result.Steps)
				}
				if orch.GetState() != tt.expectedState {
					t.Errorf("expected state %s but got %s", tt.expectedState, orch.GetState())
				}
			}
		})
	}
}

// ptrBool 辅助函数，返回 bool 指针
func ptrBool(b bool) *bool {
	return &b
}

// ptrInt 辅助函数
func ptrInt(i int) *int {
	return &i
}

// TestOrchestratorStateTransitions 测试状态转换
func TestOrchestratorStateTransitions(t *testing.T) {
	cfg := &config.HarnessConfig{
		Enabled: ptrBool(true),
	}

	// 创建编排器（不使用实际的 executor 和 sessionMgr）
	orch := NewOrchestrator(cfg, nil, nil, "test-project")

	// 初始状态应该是 IDLE
	if orch.GetState() != StateIdle {
		t.Errorf("expected initial state IDLE but got %s", orch.GetState())
	}

	// 测试 Stop 方法
	err := orch.Stop()
	if err != nil {
		t.Errorf("expected no error on Stop but got: %v", err)
	}

	// 状态应该仍然是 IDLE（因为没有在运行）
	if orch.GetState() != StateIdle {
		t.Errorf("expected state IDLE after Stop but got %s", orch.GetState())
	}
}

// TestOrchestratorConcurrentControl 测试并发控制
func TestOrchestratorConcurrentControl(t *testing.T) {
	cfg := &config.HarnessConfig{
		Enabled:       ptrBool(true),
		MaxConcurrent: ptrInt(2),
	}

	// 创建编排器（配置最大并发数为 2）
	orch := NewOrchestrator(cfg, nil, nil, "test-project")

	// 验证并发控制器已初始化
	stats := orch.GetStats()
	if concurrentStats, ok := stats["concurrent"]; !ok {
		t.Error("expected concurrent stats in GetStats")
	} else if maxGlobal, ok := concurrentStats.(map[string]any)["max_global"]; !ok || maxGlobal != 2 {
		t.Errorf("expected max_global to be 2, got %v", maxGlobal)
	}
}

// TestOrchestratorGetStats 测试统计信息获取
func TestOrchestratorGetStats(t *testing.T) {
	cfg := &config.HarnessConfig{
		Enabled: ptrBool(true),
	}

	// 创建编排器（配置最大并发数为 5）
	orch := NewOrchestrator(cfg, nil, nil, "test-project")

	stats := orch.GetStats()

	// 验证统计信息包含必要字段
	expectedFields := []string{
		"state", "total_steps", "success_steps", "error_steps",
		"start_time", "end_time", "duration", "session_ids", "last_error", "project_name", "concurrent",
	}

	for _, field := range expectedFields {
		if _, ok := stats[field]; !ok {
			t.Errorf("expected stats to contain field %q", field)
		}
	}

	// 验证初始值
	if stats["state"] != "IDLE" {
		t.Errorf("expected initial state IDLE but got %v", stats["state"])
	}

	// 验证并发控制器配置
	if concurrentStats, ok := stats["concurrent"].(map[string]any); ok {
		if maxGlobal, ok := concurrentStats["max_global"]; !ok || maxGlobal != 5 {
			t.Errorf("expected max_global 5 but got %v", maxGlobal)
		}
	} else {
		t.Error("expected concurrent stats to be a map")
	}

	// 验证项目名
	if stats["project_name"] != "test-project" {
		t.Errorf("expected project_name 'test-project' but got %v", stats["project_name"])
	}
}

// TestOrchestratorReset 测试重置功能
func TestOrchestratorReset(t *testing.T) {
	cfg := &config.HarnessConfig{
		Enabled: ptrBool(true),
	}

	// 创建编排器
	orch := NewOrchestrator(cfg, nil, nil, "test-project")

	// 验证 Reset 方法存在且不 panic
	orch.Reset()

	if orch.GetState() != StateIdle {
		t.Errorf("expected state IDLE after reset but got %s", orch.GetState())
	}
}

// TestOrchestratorContextCancellation 测试上下文取消
func TestOrchestratorContextCancellation(t *testing.T) {
	cfg := &config.HarnessConfig{
		Enabled: ptrBool(true),
	}

	// 创建编排器
	orch := NewOrchestrator(cfg, nil, nil, "test-project")

	// 创建已取消的上下文
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	// 尝试启动编排（应该立即返回错误）
	_, err := orch.Start(ctx, "END")

	if err == nil {
		t.Error("expected error for cancelled context but got nil")
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled error but got: %v", err)
	}
}

// TestOrchestratorResultChannel 测试结果通道
func TestOrchestratorResultChannel(t *testing.T) {
	cfg := &config.HarnessConfig{
		Enabled: ptrBool(true),
	}

	// 创建编排器
	orch := NewOrchestrator(cfg, nil, nil, "test-project")

	// 启动编排器以初始化通道
	ctx := context.Background()
	_, _ = orch.Start(ctx, "END")

	// 验证 GetResultChannel 不 panic
	ch := orch.GetResultChannel()
	if ch == nil {
		t.Error("expected non-nil result channel")
	}
}

// TestOrchestratorInvalidState 测试无效状态
func TestOrchestratorInvalidState(t *testing.T) {
	// 创建一个正在运行的编排器
	orch := &OrchestratorImpl{
		state: StateRunning,
	}

	// 尝试启动（应该失败，因为不在 IDLE 状态）
	_, err := orch.Start(context.Background(), "END")

	if err == nil {
		t.Error("expected error for non-IDLE state but got nil")
	}

	if orch.GetState() != StateRunning {
		t.Errorf("expected state to remain RUNNING but got %s", orch.GetState())
	}
}

// 辅助测试：验证 OrchestratorResult 结构
func TestOrchestratorResultStructure(t *testing.T) {
	result := &OrchestratorResult{
		Success:    true,
		Steps:      3,
		Duration:   5 * time.Second,
		SessionIDs: []SessionID{"ses_123", "ses_456", "ses_789"},
		LastError:  nil,
		Outputs:    []string{"output1", "output2", "output3"},
	}

	// 验证字段
	if !result.Success {
		t.Error("expected Success to be true")
	}
	if result.Steps != 3 {
		t.Errorf("expected Steps to be 3 but got %d", result.Steps)
	}
	if len(result.SessionIDs) != 3 {
		t.Errorf("expected 3 SessionIDs but got %d", len(result.SessionIDs))
	}
	if len(result.Outputs) != 3 {
		t.Errorf("expected 3 Outputs but got %d", len(result.Outputs))
	}
}

// 辅助测试：验证状态字符串转换
func TestOrchestratorStateString(t *testing.T) {
	tests := []struct {
		state       OrchestratorState
		expectedStr string
	}{
		{StateIdle, "IDLE"},
		{StateRunning, "RUNNING"},
		{StateError, "ERROR"},
		{StateEnd, "END"},
		{OrchestratorState(999), "UNKNOWN"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("state_%s", tt.expectedStr), func(t *testing.T) {
			if tt.state.String() != tt.expectedStr {
				t.Errorf("expected %s but got %s", tt.expectedStr, tt.state.String())
			}
		})
	}
}

// TestOrchestratorWorkflow 测试完整工作流（需要集成测试环境）
// 这个测试在实际环境中验证：解析 -> 执行 -> 返回
func TestOrchestratorWorkflow(t *testing.T) {
	t.Skip("集成测试，需要 opencode CLI 环境")

	// 这里应该是集成测试代码，验证完整的编排流程
	// 由于需要真实的 opencode CLI 和子 Agent，这里跳过
}
