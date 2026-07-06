package mode

import (
	"tc-connect/core/session"
	"tc-connect/core/types"
)

// DeleteModeState 删除模式状态（按 sessionKey 隔离）
type DeleteModeState struct {
	Page        int
	SelectedIDs map[string]struct{} // agentSessionID -> {}
	Phase       string              // "select", "confirm", "result"
	Hint        string
	Result      string
}

// NewDeleteModeState 创建初始状态
func NewDeleteModeState() *DeleteModeState {
	return &DeleteModeState{
		Page:        1,
		SelectedIDs: make(map[string]struct{}),
		Phase:       "select",
	}
}

// DeleteSessionDisplayName 获取 session 的显示名称（通用逻辑）
func DeleteSessionDisplayName(sessions *session.SessionManager, matched *types.AgentSessionInfo) string {
	displayName := sessions.GetSessionName(matched.ID)
	if displayName == "" {
		displayName = matched.Summary
	}
	if displayName == "" {
		shortID := matched.ID
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		displayName = shortID
	}
	return displayName
}

// DeleteModeSelectionNames 返回选中项的名称列表（deleteMode 专用）
func DeleteModeSelectionNames(sessions *session.SessionManager, dm *DeleteModeState, agentSessions []types.AgentSessionInfo) []string {
	names := make([]string, 0, len(dm.SelectedIDs))
	for i := range agentSessions {
		if _, ok := dm.SelectedIDs[agentSessions[i].ID]; ok {
			names = append(names, "- "+DeleteSessionDisplayName(sessions, &agentSessions[i]))
		}
	}
	return names
}
