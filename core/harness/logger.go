package harness

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LoggerConfig 日志配置
type LoggerConfig struct {
	Level       string // 日志级别：debug, info, warn, error
	OutputPath  string // 日志文件路径，空则输出到 stdout
	ProjectName string // 项目名称（用于日志文件名和过滤）
}

// HarnessLogger 编排器专用日志器
type HarnessLogger struct {
	logger      *slog.Logger
	config      *LoggerConfig
	projectName string
	outputFile  *os.File // 日志文件句柄
}

// NewHarnessLogger 创建编排器日志器
func NewHarnessLogger(cfg *LoggerConfig) (*HarnessLogger, error) {
	logger := &HarnessLogger{
		config:      cfg,
		projectName: cfg.ProjectName,
	}

	// 设置默认值
	if logger.config.Level == "" {
		logger.config.Level = "info"
	}

	// 解析日志级别
	var level slog.Level
	switch strings.ToLower(logger.config.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	// 设置输出目标
	var handler slog.Handler
	if logger.config.OutputPath != "" {
		// 文件输出
		if err := os.MkdirAll(logger.config.OutputPath, 0755); err != nil {
			return nil, err
		}

		// 生成日志文件名：harness_{projectName}_{date}.log
		dateStr := time.Now().Format("20060102")
		logFileName := filepath.Join(
			logger.config.OutputPath,
			filepath.Join("harness", logger.projectName),
			filepath.Join("harness_"+logger.projectName+"_"+dateStr+".log"),
		)

		// 确保目录存在
		logDir := filepath.Dir(logFileName)
		if err := os.MkdirAll(logDir, 0755); err != nil {
			return nil, err
		}

		// 打开文件（追加模式）
		file, err := os.OpenFile(logFileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return nil, err
		}
		logger.outputFile = file

		// 创建文件 handler
		handler = slog.NewTextHandler(file, &slog.HandlerOptions{
			Level: level,
			ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				// 自定义字段处理
				if a.Key == slog.TimeKey {
					// 格式化时间
					if t, ok := a.Value.Any().(time.Time); ok {
						a.Value = slog.StringValue(t.Format("2006-01-02T15:04:05Z07:00"))
					}
				}
				return a
			},
		})
	} else {
		// 控制台输出
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: level,
		})
	}

	logger.logger = slog.New(handler)

	return logger, nil
}

// Info 记录信息级别日志
func (l *HarnessLogger) Info(msg string, args ...any) {
	l.logger.Info(msg, l.appendCommonFields(args)...)
}

// Debug 记录调试级别日志
func (l *HarnessLogger) Debug(msg string, args ...any) {
	l.logger.Debug(msg, l.appendCommonFields(args)...)
}

// Warn 记录警告级别日志
func (l *HarnessLogger) Warn(msg string, args ...any) {
	l.logger.Warn(msg, l.appendCommonFields(args)...)
}

// Error 记录错误级别日志
func (l *HarnessLogger) Error(msg string, args ...any) {
	l.logger.Error(msg, l.appendCommonFields(args)...)
}

// appendCommonFields 添加公共字段
func (l *HarnessLogger) appendCommonFields(args []any) []any {
	// 总是添加项目名称和组件标识
	commonFields := []any{"component", "harness", "project", l.projectName}
	return append(commonFields, args...)
}

// WithSessionID 返回带 session_id 的日志上下文
func (l *HarnessLogger) WithSessionID(sessionID SessionID) *HarnessLogger {
	return &HarnessLogger{
		logger:      l.logger.With("session_id", string(sessionID)),
		config:      l.config,
		projectName: l.projectName,
	}
}

// WithAgent 返回带 agent 名称的日志上下文
func (l *HarnessLogger) WithAgent(agentName string) *HarnessLogger {
	return &HarnessLogger{
		logger:      l.logger.With("agent", agentName),
		config:      l.config,
		projectName: l.projectName,
	}
}

// LogProtocolParse 记录协议解析日志
func (l *HarnessLogger) LogProtocolParse(input string, success bool, agentName string, params string, resume bool, err error) {
	if success {
		l.Info("protocol parsed successfully",
			"agent_name", agentName,
			"params", params,
			"resume", resume,
			"input_len", len(input),
		)
	} else {
		l.Error("protocol parse failed",
			"input", input,
			"error", err,
		)
	}
}

// LogSubAgentStart 记录子 Agent 启动日志
func (l *HarnessLogger) LogSubAgentStart(agentName string, params string, resume bool, sessionID SessionID) {
	l.Info("sub-agent started",
		"agent_name", agentName,
		"params", params,
		"resume", resume,
		"session_id", string(sessionID),
	)
}

// LogSubAgentComplete 记录子 Agent 完成日志
func (l *HarnessLogger) LogSubAgentComplete(agentName string, sessionID SessionID, outputLen int, duration time.Duration, success bool, err error) {
	if success {
		l.Info("sub-agent completed",
			"agent_name", agentName,
			"session_id", string(sessionID),
			"output_len", outputLen,
			"duration_ms", duration.Milliseconds(),
		)
	} else {
		l.Error("sub-agent failed",
			"agent_name", agentName,
			"session_id", string(sessionID),
			"duration_ms", duration.Milliseconds(),
			"error", err,
		)
	}
}

// LogSubAgentRetry 记录重试日志
func (l *HarnessLogger) LogSubAgentRetry(agentName string, attempt int, maxAttempts int, err error) {
	l.Warn("sub-agent retrying",
		"agent_name", agentName,
		"attempt", attempt,
		"max_attempts", maxAttempts,
		"error", err,
	)
}

// LogSessionCreated 记录会话创建日志
func (l *HarnessLogger) LogSessionCreated(agentName string, tempSessionID SessionID, resumedFrom SessionID) {
	l.Debug("session created",
		"agent_name", agentName,
		"temp_session_id", string(tempSessionID),
		"resumed_from", string(resumedFrom),
	)
}

// LogSessionUpdated 记录会话 ID 更新日志（临时 ID -> 真实 ID）
func (l *HarnessLogger) LogSessionUpdated(tempID SessionID, actualID SessionID, agentName string) {
	l.Info("session id updated",
		"agent_name", agentName,
		"temp_id", string(tempID),
		"actual_id", string(actualID),
	)
}

// LogOrchestrationStart 记录编排开始日志
func (l *HarnessLogger) LogOrchestrationStart(input string) {
	l.Info("orchestration started",
		"initial_input", input,
	)
}

// LogOrchestrationStep 记录编排步骤日志
func (l *HarnessLogger) LogOrchestrationStep(step int, agentName string, success bool, duration time.Duration, err error) {
	if success {
		l.Info("orchestration step completed",
			"step", step,
			"agent_name", agentName,
			"duration_ms", duration.Milliseconds(),
		)
	} else {
		l.Error("orchestration step failed",
			"step", step,
			"agent_name", agentName,
			"duration_ms", duration.Milliseconds(),
			"error", err,
		)
	}
}

// LogOrchestrationEnd 记录编排结束日志
func (l *HarnessLogger) LogOrchestrationEnd(success bool, totalSteps int, sessionIDs []SessionID, duration time.Duration, lastErr error) {
	if success {
		l.Info("orchestration completed",
			"total_steps", totalSteps,
			"session_ids", sessionIDs,
			"total_duration_ms", duration.Milliseconds(),
		)
	} else {
		l.Error("orchestration failed",
			"total_steps", totalSteps,
			"session_ids", sessionIDs,
			"total_duration_ms", duration.Milliseconds(),
			"last_error", lastErr,
		)
	}
}

// LogConcurrentAcquire 记录并发控制日志
func (l *HarnessLogger) LogConcurrentAcquire(agentName string, projectName string, waitDuration time.Duration) {
	l.Debug("concurrent lock acquired",
		"agent_name", agentName,
		"project_name", projectName,
		"wait_ms", waitDuration.Milliseconds(),
	)
}

// LogTimeout 记录超时期限日志
func (l *HarnessLogger) LogTimeout(agentName string, sessionID SessionID, timeout time.Duration) {
	l.Warn("sub-agent timeout",
		"agent_name", agentName,
		"session_id", string(sessionID),
		"timeout_ms", timeout.Milliseconds(),
	)
}

// Close 关闭日志器
func (l *HarnessLogger) Close() error {
	if l.outputFile != nil {
		return l.outputFile.Close()
	}
	return nil
}

// GetLogger 返回底层的 slog.Logger（用于高级用法）
func (l *HarnessLogger) GetLogger() *slog.Logger {
	return l.logger
}

// GetStats 获取日志统计信息
func (l *HarnessLogger) GetStats() map[string]any {
	stats := map[string]any{
		"level":       l.config.Level,
		"output_path": l.config.OutputPath,
		"project":     l.projectName,
	}
	if l.outputFile != nil {
		stats["file_open"] = true
	} else {
		stats["file_open"] = false
	}
	return stats
}
