package harness

import (
	"errors"
	"strings"
)

// 协议常量
const (
	ProtocolDelimiter = "||"
	ProtocolEndToken  = "END"
)

// HarnessRequest 表示解析后的协议请求结构
type HarnessRequest struct {
	AgentName string // 子 Agent 标识，对应 opencode 已注册的 agent
	Params    string // 执行参数和文件路径
	Resume    bool   // 是否复用上一次 session
}

// ParseProtocol 解析主 Agent 输出的协议格式
// 协议格式：agent_name||params||resume
//
// 返回:
//   - *HarnessRequest: 解析后的请求结构
//   - error: 解析错误
//
// 示例:
//
//	"planner||生成项目规划||false" -> HarnessRequest{AgentName:"planner", Params:"生成项目规划", Resume:false}
//	"END" -> nil, nil
func ParseProtocol(input string) (*HarnessRequest, error) {
	if input == "" {
		return nil, errors.New("protocol input is empty")
	}

	// 去除首尾空白字符
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return nil, errors.New("protocol input is empty after trimming")
	}

	// 检测 END 标识（区分大小写）
	if trimmed == ProtocolEndToken {
		return nil, nil
	}

	// 按分隔符拆分
	parts := strings.Split(trimmed, ProtocolDelimiter)

	// 验证字段数量（必须为 3 个：agent_name, params, resume）
	if len(parts) != 3 {
		return nil, errors.New("invalid protocol format: expected 3 fields separated by '||', got " + string(rune(len(parts))))
	}

	// 解析各字段
	agentName := strings.TrimSpace(parts[0])
	params := strings.TrimSpace(parts[1])
	resumeStr := strings.TrimSpace(parts[2])

	// 验证 agent_name 不为空
	if agentName == "" {
		return nil, errors.New("agent_name cannot be empty")
	}

	// 解析 resume 字段（支持多种布尔值表示）
	resume := parseResumeField(resumeStr)

	return &HarnessRequest{
		AgentName: agentName,
		Params:    params,
		Resume:    resume,
	}, nil
}

// parseResumeField 解析 resume 字符串为布尔值
// 支持：true/false, True/False, TRUE/FALSE, yes/no, 1/0
func parseResumeField(str string) bool {
	if str == "" {
		return false
	}

	lower := strings.ToLower(str)
	return lower == "true" || lower == "yes" || lower == "1"
}

// IsValidAgentName 验证 agent_name 格式
// 规则：只能包含字母、数字、下划线、连字符，不能以数字开头
func IsValidAgentName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}

	for i, c := range name {
		if i == 0 {
			// 首字符必须是字母或下划线
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_') {
				return false
			}
		} else {
			// 后续字符可以是字母、数字、下划线、连字符
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
				return false
			}
		}
	}
	return true
}

// IsEndToken 检查输入是否为 END 标识
func IsEndToken(input string) bool {
	return strings.TrimSpace(input) == ProtocolEndToken
}
