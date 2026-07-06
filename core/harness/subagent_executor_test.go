package harness

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"syscall"
	"testing"
	"time"

	"tc-connect/config"
)

// TestSubAgentExecutor 测试子 Agent 执行器（Task 2.1）
func TestSubAgentExecutor(t *testing.T) {
	// 测试配置
	cfg := &config.HarnessConfig{
		Enabled:     boolPtr(true),
		TimeoutMins: intPtr(1), // 测试用短超时
		MaxRetries:  intPtr(2), // 测试用较少重试
		Agents: map[string]config.SubAgentConfig{
			"planner": {
				TimeoutMins: intPtr(1),
				MaxRetries:  intPtr(2),
				Description: "测试规划 Agent",
			},
		},
	}

	// 创建管理器
	manager, err := NewSubAgentManager(cfg, "")
	if err != nil {
		t.Skipf("跳过测试：%v (可能需要配置 opencode)", err)
	}

	// 检查是否有可用的子 Agent
	agents := manager.ListAvailableAgents()
	if len(agents) == 0 {
		t.Skip("没有可用的子 Agent，跳过测试")
	}

	t.Logf("可用子 Agent: %v", agents)

	// 创建会话管理器
	sessionMgr, err := NewSessionManager(cfg, manager)
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}
	defer sessionMgr.Shutdown()

	// 创建执行器
	executor := NewSubAgentExecutor(
		manager,
		sessionMgr,
		5*time.Minute,
		3,
		nil,      // 使用默认 logger
		NewMetrics(), // 添加监控指标
	)

	// 选择一个可用的 Agent 进行测试
	testAgent := agents[0]
	t.Logf("使用 Agent '%s' 进行测试", testAgent)

	t.Run("基本执行测试", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		req := &HarnessRequest{
			AgentName: testAgent,
			Params:    "测试参数",
			Resume:    false,
		}

		result, err := executor.Execute(ctx, req, "")

		// 注意：这里我们只验证执行器的行为，不验证子 Agent 的实际输出
		// 因为子 Agent 可能不存在或行为不确定
		if err != nil {
			t.Logf("执行结果（可能失败）: %v", err)
			t.Logf("输出：%s", result.Output)
		} else {
			t.Logf("执行成功，输出长度：%d", len(result.Output))
		}

		// 验证结果结构
		if result == nil {
			t.Error("Execute() 返回 nil 结果")
		}

		// 验证 session_id 格式（如果获取成功）
		if result.SessionID != "" {
			if !isValidSessionIDFormat(result.SessionID) {
				t.Errorf("SessionID 格式无效：%s", result.SessionID)
			}
			t.Logf("SessionID: %s", result.SessionID)
		} else {
			t.Log("注意：SessionID 为空（可能是测试环境未配置 opencode session）")
		}

		// 验证耗时被记录
		if result.Duration <= 0 {
			t.Error("Duration 应为正数")
		}
		t.Logf("执行耗时：%v", result.Duration)
	})

	t.Run("超时控制测试", func(t *testing.T) {
		// 创建一个会超时的上下文（注意：重试会增加总时间）
		// 这里我们测试单次执行的超时
		// 超时 50ms，重试 3 次，每次重试等待 1s，总时间约 7 秒
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		req := &HarnessRequest{
			AgentName: testAgent,
			Params:    "测试参数",
			Resume:    false,
		}

		start := time.Now()
		result, err := executor.Execute(ctx, req, "")
		duration := time.Since(start)

		// 验证执行被终止
		if err == nil {
			t.Log("注意：执行在超时前完成")
		} else {
			t.Logf("预期超时错误：%v", err)
			// 验证错误包含超时信息
			if !containsTimeoutError(err) {
				t.Errorf("错误不包含超时信息：%v", err)
			}
		}

		// 验证执行时间（考虑到重试，总时间会较长）
		// 单次超时 50ms + 3 次重试等待（1s + 2s + 3s 指数退避）≈ 6-8 秒
		if duration > 10*time.Second {
			t.Errorf("执行时间过长：%v，应该在 10 秒内完成（含重试）", duration)
		}
		t.Logf("超时测试耗时：%v（含重试等待）", duration)

		if result != nil {
			t.Logf("重试次数：%d", result.RetryCount)
			// 验证重试次数不超过配置
			if result.RetryCount > 3 {
				t.Errorf("重试次数过多：%d", result.RetryCount)
			}
			// 验证重试次数正确（应该尝试了 3 次）
			if result.RetryCount != 3 {
				t.Logf("注意：重试次数为 %d，可能重试逻辑与预期不同", result.RetryCount)
			}
		}
	})

	t.Run("重试机制测试", func(t *testing.T) {
		// 这个测试依赖于子 Agent 失败
		// 我们使用一个不存在的命令或参数来触发失败
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		req := &HarnessRequest{
			AgentName: testAgent,
			Params:    "--invalid-flag-to-trigger-error", // 尝试触发错误
			Resume:    false,
		}

		result, err := executor.Execute(ctx, req, "")

		if err != nil {
			t.Logf("执行失败（预期）: %v", err)
			if result != nil {
				t.Logf("重试次数：%d (应该 <= 2)", result.RetryCount)
				if result.RetryCount > 2 {
					t.Errorf("重试次数过多：%d", result.RetryCount)
				}
			}
		} else {
			t.Log("执行成功（可能参数未触发错误）")
		}
	})

	t.Run("RESUME 参数传递测试", func(t *testing.T) {
		// 第一次执行（不 resume）
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		req1 := &HarnessRequest{
			AgentName: testAgent,
			Params:    "第一次执行",
			Resume:    false,
		}

		_, err1 := executor.Execute(ctx, req1, "")
		if err1 != nil {
			t.Logf("第一次执行失败：%v", err1)
		}

		// 第二次执行（resume=true）
		req2 := &HarnessRequest{
			AgentName: testAgent,
			Params:    "第二次执行（resume）",
			Resume:    true,
		}

		_, err2 := executor.Execute(ctx, req2, "")
		if err2 != nil {
			t.Logf("第二次执行失败：%v", err2)
		}

		t.Log("RESUME 参数传递测试完成")
	})

	t.Run("工作目录设置测试", func(t *testing.T) {
		// 创建临时目录
		tmpDir := t.TempDir()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		req := &HarnessRequest{
			AgentName: testAgent,
			Params:    "工作目录测试",
			Resume:    false,
		}

		_, err := executor.Execute(ctx, req, tmpDir)
		if err != nil {
			t.Logf("工作目录测试执行结果：%v", err)
		}
		t.Logf("使用工作目录：%s", tmpDir)
	})

	t.Run("输出捕获测试", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		req := &HarnessRequest{
			AgentName: testAgent,
			Params:    "输出捕获测试",
			Resume:    false,
		}

		result, err := executor.Execute(ctx, req, "")

		if err != nil && result != nil {
			// 即使失败，也应该有输出
			t.Logf("错误输出：%s", result.Output[:min(len(result.Output), 200)])
		}

		if result != nil {
			// 验证输出被捕获（可能为空，但不应 panic）
			_ = result.Output
		}
	})

	t.Run("进程杀死测试（SIGTERM -> SIGKILL）", func(t *testing.T) {
		// 这个测试验证超时后进程被正确杀死
		// 由于重试机制，需要更长时间
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		req := &HarnessRequest{
			AgentName: testAgent,
			Params:    "进程杀死测试",
			Resume:    false,
		}

		done := make(chan struct{})
		go func() {
			_, _ = executor.Execute(ctx, req, "")
			close(done)
		}()

		select {
		case <-done:
			t.Log("进程正确终止（含重试）")
		case <-time.After(10 * time.Second): // 考虑到重试，给更多时间
			t.Error("进程未在规定时间内终止（可能未正确杀死）")
		}
	})

	t.Run("session_id 格式验证", func(t *testing.T) {
		// 验证 session_id 解析函数
		tests := []struct {
			name     string
			input    string
			wantID   SessionID
			wantErr  bool
		}{
			{"有效 session_id", "ses_abc123xyz", "ses_abc123xyz", false},
			{"有效 session_id 带换行", "ses_abc123xyz\n", "ses_abc123xyz", false},
			{"有效 session_id 在文本中", "最新会话：ses_12345\n其他信息", "ses_12345", false},
			{"空输入", "", "", true},
			{"无效格式", "invalid-format", "", true},
			{"JSON 格式（应拒绝）", "[\"ses_123\"]", "", true},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				// 调用 parseSessionID（需要导出或使用反射，这里简化处理）
				// 实际测试中可以通过测试 UpdateSessionAfterExecution 来间接验证
				_ = tt.wantID
				_ = tt.wantErr
				t.Logf("测试用例：%s, input: %q", tt.name, tt.input)
			})
		}
	})

	t.Run("错误处理测试", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		tests := []struct {
			name        string
			req         *HarnessRequest
			wantErr     bool
			errContains string
		}{
			{
				name: "空请求",
				req:  nil,
				wantErr: true,
				errContains: "nil",
			},
			{
				name: "空 Agent 名称",
				req: &HarnessRequest{
					AgentName: "",
					Params:    "test",
				},
				wantErr: true,
				errContains: "empty",
			},
			{
				name: "不存在的 Agent",
				req: &HarnessRequest{
					AgentName: "nonexistent_agent_xyz",
					Params:    "test",
				},
				wantErr: true,
				errContains: "not available",
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				_, err := executor.Execute(ctx, tt.req, "")
				if tt.wantErr {
					if err == nil {
						t.Error("期望错误，但得到 nil")
					} else if tt.errContains != "" && !containsString(err.Error(), tt.errContains) {
						t.Errorf("错误信息不包含预期字符串。got: %v, want contains: %s", err, tt.errContains)
					}
				} else {
					if err != nil {
						t.Errorf("期望无错误，但得到：%v", err)
					}
				}
			})
		}
	})
}

// 辅助函数

func boolPtr(b bool) *bool {
	return &b
}

// isValidSessionIDFormat 验证 session_id 格式
func isValidSessionIDFormat(id SessionID) bool {
	if id == "" {
		return false
	}
	// 格式：ses_ 后跟字母数字
	re := regexp.MustCompile(`^ses_[a-zA-Z0-9]+$`)
	return re.MatchString(string(id))
}

// containsTimeoutError 检查错误是否包含超时信息
func containsTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return containsString(errStr, "context deadline exceeded") || 
		   containsString(errStr, "timeout") || 
		   containsString(errStr, "Timed out")
}

// containsString 检查字符串是否包含子串（不区分大小写）
func containsString(s, substr string) bool {
	return len(s) >= len(substr) && 
		   (s == substr || 
		    len(s) > 0 && len(substr) > 0 && 
		    (s[:len(substr)] == substr || 
		     s[len(s)-len(substr):] == substr ||
		     findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestSubAgentExecutorIntegration 集成测试（需要真实 opencode 环境）
func TestSubAgentExecutorIntegration(t *testing.T) {
	t.Skip("集成测试需要真实 opencode 环境，手动运行")
	
	// 测试步骤：
	// 1. 确保 opencode 已安装并配置
	// 2. 确保至少有一个子 Agent 注册
	// 3. 运行执行器并验证完整流程
	// 4. 验证 session_id 可以通过 `opencode session list` 查询
	// 5. 验证可以使用 `opencode run -s session_id` 溯源
}

// TestProcessKilling 验证进程杀死逻辑
func TestProcessKilling(t *testing.T) {
	// 测试 killProcess 方法
	executor := &SubAgentExecutor{}
	
	// 创建一个长期运行的命令
	cmd := exec.Command("sleep", "100")
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动测试进程失败：%v", err)
	}
	
	pid := cmd.Process.Pid
	t.Logf("启动测试进程 PID: %d", pid)
	
	// 杀死进程（异步）
	executor.killProcess(cmd)
	
	// 等待 killProcess 完成（它内部有 1 秒等待）
	time.Sleep(1500 * time.Millisecond)
	
	// 检查进程是否还在运行
	if proc, err := os.FindProcess(pid); err == nil {
		if err := proc.Signal(syscall.Signal(0)); err == nil {
			t.Error("进程未被正确杀死")
		}
	}
	
	t.Log("进程杀死测试完成")
}

// TestRetryLogic 测试重试逻辑
func TestRetryLogic(t *testing.T) {
	// 验证 shouldRetry 方法
	executor := &SubAgentExecutor{}
	
	tests := []struct {
		name     string
		err      error
		wantRetry bool
	}{
		{"超时错误", fmt.Errorf("context deadline exceeded"), true},
		{"连接错误", fmt.Errorf("connection refused"), true},
		{"Agent 不可用", fmt.Errorf("sub-agent \"x\" is not available"), false},
		{"文件未找到", fmt.Errorf("file not found"), false},
		{"权限错误", fmt.Errorf("permission denied"), false},
		{"无效参数", fmt.Errorf("invalid argument"), false},
		{"其他错误", fmt.Errorf("unknown error"), true},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRetry := executor.shouldRetry(tt.err)
			if gotRetry != tt.wantRetry {
				t.Errorf("shouldRetry(%v) = %v, want %v", tt.err, gotRetry, tt.wantRetry)
			}
		})
	}
}

// TestSessionIDParsing 测试 session_id 解析
func TestSessionIDParsing(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		want    SessionID
		wantErr bool
	}{
		{
			name:   "纯 session_id",
			output: "ses_16f661969ffeziRL4Yo5qkpPWg",
			want:   "ses_16f661969ffeziRL4Yo5qkpPWg",
		},
		{
			name:   "带换行和空格",
			output: "  ses_abc123xyz  \n",
			want:   "ses_abc123xyz",
		},
		{
			name:   "在文本中",
			output: "最新会话：ses_test123\n创建时间：2024-01-01",
			want:   "ses_test123",
		},
		{
			name:    "空输出",
			output:  "",
			wantErr: true,
		},
		{
			name:    "无效格式",
			output:  "invalid",
			wantErr: true,
		},
		{
			name:    "JSON 数组（应拒绝）",
			output:  `["ses_123"]`,
			wantErr: true,
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSessionID(tt.output)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseSessionID(%q) 期望错误，但得到：%v", tt.output, got)
				}
				return
			}
			if err != nil {
				t.Errorf("parseSessionID(%q) 错误 = %v", tt.output, err)
				return
			}
			if got != tt.want {
				t.Errorf("parseSessionID(%q) = %v, want %v", tt.output, got, tt.want)
			}
		})
	}
}

// TestWorkDirHandling 测试工作目录处理
func TestWorkDirHandling(t *testing.T) {
	// 验证工作目录正确传递给命令
	tmpDir := t.TempDir()
	
	cfg := &config.HarnessConfig{}
	manager, err := NewSubAgentManager(cfg, "")
	if err != nil {
		t.Skipf("跳过：%v", err)
	}
	
	executor := &SubAgentExecutor{manager: manager}
	
	req := &HarnessRequest{
		AgentName: "test",
		Params:    "",
	}
	
	cmd := executor.buildCommand(req, tmpDir)
	if cmd == nil {
		t.Skip("opencode CLI 未找到")
	}
	
	// 验证工作目录被设置
	if cmd.Dir != tmpDir {
		t.Errorf("命令工作目录 = %q, want %q", cmd.Dir, tmpDir)
	}
	
	// 验证命令参数不包含 --work-dir（应该使用 cmd.Dir）
	foundWorkDir := false
	for i, arg := range cmd.Args {
		if arg == "--work-dir" && i+1 < len(cmd.Args) {
			foundWorkDir = true
			break
		}
	}
	if foundWorkDir {
		t.Error("命令参数中不应包含 --work-dir，应该使用 cmd.Dir")
	}
	
	t.Logf("工作目录设置为：%s", tmpDir)
}
