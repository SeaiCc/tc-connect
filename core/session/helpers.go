package session

import (
	"strconv"
	"strings"
	"tc-connect/core/types"
)

// 将用户请求解析到agent session, 优先级
// 1. 数字需要(1-base, 匹配 /list output) 2. 完全自定义名称匹配(不区分大小写)
// 3. sessionID 前缀匹配 4. 自定义名称前缀匹配(不区分大小写)
// 5. summary子串匹配
func MatchSession(sessions []types.AgentSessionInfo, manager *SessionManager, query string) *types.AgentSessionInfo {
	if len(sessions) == 0 {
		return nil
	}

	// 1. 数字匹配
	if idx, err := strconv.Atoi(query); err == nil && idx >= 1 && idx <= len(sessions) {
		return &sessions[idx-1]
	}
	queryLower := strings.ToLower(query)

	// 2. 自定义名称匹配
	for i := range sessions {
		name := manager.GetSessionName(sessions[i].ID)
		if name != "" && strings.ToLower(name) == queryLower {
			return &sessions[i]
		}
	}

	// 3. sessionID prefix
	for i := range sessions {
		if strings.HasPrefix(sessions[i].ID, query) {
			return &sessions[i]
		}
	}

	// 4. 自定义名称前缀匹配
	for i := range sessions {
		name := manager.GetSessionName(sessions[i].ID)
		if name != "" && strings.HasPrefix(strings.ToLower(name), queryLower) {
			return &sessions[i]
		}
	}

	// 5. summary 字串匹配
	for i := range sessions {
		if sessions[i].Summary != "" && strings.Contains(strings.ToLower(sessions[i].Summary), queryLower) {
			return &sessions[i]
		}
	}
	return nil
}
