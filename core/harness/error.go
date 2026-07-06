package harness

import "fmt"

// ErrInvalidProtocol 协议解析错误
type ErrInvalidProtocol struct {
	Raw string
}

func (e ErrInvalidProtocol) Error() string {
	return fmt.Sprintf("invalid protocol: %s", e.Raw)
}

// ErrAgentNotFound 未找到 Agent
type ErrAgentNotFound struct {
	AgentName string
}

func (e ErrAgentNotFound) Error() string {
	return fmt.Sprintf("agent not found: %s", e.AgentName)
}

// ErrOrchestrationFailed 编排错误
type ErrOrchestrationFailed struct {
	Step  int
	Err   error
}

func (e ErrOrchestrationFailed) Error() string {
	return fmt.Sprintf("orchestration failed at step %d: %v", e.Step, e.Err)
}

// ErrSessionNotFound 会话未找到
type ErrSessionNotFound struct {
	SessionID SessionID
}

func (e ErrSessionNotFound) Error() string {
	return fmt.Sprintf("session not found: %s", e.SessionID)
}
