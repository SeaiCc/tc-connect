package harness

import (
	"testing"
	"time"
)

// TestNewHarnessLogger 测试日志器创建
func TestNewHarnessLogger(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *LoggerConfig
		wantErr bool
	}{
		{
			name: "默认配置",
			cfg: &LoggerConfig{
				ProjectName: "test-project",
			},
			wantErr: false,
		},
		{
			name: "文件输出",
			cfg: &LoggerConfig{
				ProjectName: "test-project",
				OutputPath:  t.TempDir(),
				Level:       "debug",
			},
			wantErr: false,
		},
		{
			name: "无效路径",
			cfg: &LoggerConfig{
				ProjectName: "test-project",
				OutputPath:  "/root/nonexistent/path",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, err := NewHarnessLogger(tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error but got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("expected no error but got: %v", err)
			}
			if logger == nil {
				t.Fatal("logger should not be nil")
			}

			// 测试基本日志方法不 panic
			logger.Info("test info")
			logger.Debug("test debug")
			logger.Warn("test warn")
			logger.Error("test error")

			// 关闭日志器
			if err := logger.Close(); err != nil {
				t.Errorf("close error: %v", err)
			}
		})
	}
}

// TestLoggerWithContext 测试带上下文的日志
func TestLoggerWithContext(t *testing.T) {
	cfg := &LoggerConfig{
		ProjectName: "test-project",
		Level:       "debug",
	}

	logger, err := NewHarnessLogger(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()

	// 测试 WithSessionID
	loggerWithSession := logger.WithSessionID("ses_test123")
	loggerWithSession.Info("test with session")

	// 测试 WithAgent
	loggerWithAgent := logger.WithAgent("planner")
	loggerWithAgent.Info("test with agent")
}

// TestLoggerLogProtocolParse 测试协议解析日志
func TestLoggerLogProtocolParse(t *testing.T) {
	cfg := &LoggerConfig{
		ProjectName: "test-project",
		Level:       "debug",
	}

	logger, err := NewHarnessLogger(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()

	// 测试成功解析
	logger.LogProtocolParse("planner||test params||false", true, "planner", "test params", false, nil)

	// 测试失败解析
	logger.LogProtocolParse("invalid", false, "", "", false, ErrInvalidProtocol{})
}

// TestLoggerLogSubAgent 测试子 Agent 日志
func TestLoggerLogSubAgent(t *testing.T) {
	cfg := &LoggerConfig{
		ProjectName: "test-project",
		Level:       "debug",
	}

	logger, err := NewHarnessLogger(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()

	sessionID := SessionID("ses_test123")

	// 测试启动
	logger.LogSubAgentStart("planner", "test params", false, sessionID)

	// 测试完成
	logger.LogSubAgentComplete("planner", sessionID, 100, 5*time.Second, true, nil)

	// 测试失败
	logger.LogSubAgentComplete("planner", sessionID, 0, 5*time.Second, false, ErrInvalidProtocol{})

	// 测试重试
	logger.LogSubAgentRetry("planner", 1, 3, ErrInvalidProtocol{})
}

// TestLoggerLogOrchestration 测试编排日志
func TestLoggerLogOrchestration(t *testing.T) {
	cfg := &LoggerConfig{
		ProjectName: "test-project",
		Level:       "debug",
	}

	logger, err := NewHarnessLogger(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()

	// 测试编排开始
	logger.LogOrchestrationStart("test input")

	// 测试步骤执行
	logger.LogOrchestrationStep(1, "planner", true, 100*time.Millisecond, nil)

	// 测试编排结束
	logger.LogOrchestrationEnd(true, 1, []SessionID{"ses_1"}, 2*time.Second, nil)

	// 测试失败结束
	logger.LogOrchestrationEnd(false, 1, []SessionID{"ses_1"}, 2*time.Second, ErrInvalidProtocol{})
}

// TestLoggerLogSession 测试会话日志
func TestLoggerLogSession(t *testing.T) {
	cfg := &LoggerConfig{
		ProjectName: "test-project",
		Level:       "debug",
	}

	logger, err := NewHarnessLogger(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()

	// 测试会话创建
	logger.LogSessionCreated("planner", "ses_test123", "")

	// 测试会话更新
	logger.LogSessionUpdated("ses_old", "ses_new", "planner")

	// 测试错误记录（使用标准 slog 方法）
	logger.Error("test error", "error", ErrInvalidProtocol{})
}
