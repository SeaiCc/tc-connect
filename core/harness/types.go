package harness

import "log/slog"

// 注意：大部分类型定义已在各自文件中定义：
// - protocol.go: HarnessRequest
// - subagent.go: SubAgentManager, AgentMeta
// - session.go: Session, SessionID, SessionStore, SessionManager
// - logger.go: LoggerConfig, HarnessLogger
//
// 此文件保留用于未来扩展或跨模块共享的类型定义

// HarnessResult 编排结果
type HarnessResult struct {
	Success   bool   `json:"success"`
	Message   string `json:"message,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
}

// GlobalLogger 全局日志器（可选，用于模块外部的日志访问）
var GlobalLogger *HarnessLogger

// SetGlobalLogger 设置全局日志器
func SetGlobalLogger(logger *HarnessLogger) {
	GlobalLogger = logger
}

// GetGlobalLogger 获取全局日志器
func GetGlobalLogger() *HarnessLogger {
	return GlobalLogger
}

// DefaultLogger 默认日志器（使用标准 slog）
var DefaultLogger = slog.Default()
