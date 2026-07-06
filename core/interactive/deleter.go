package interactive

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"tc-connect/core/i18n"
	"tc-connect/core/mode"
	"tc-connect/core/session"
	"tc-connect/core/types"
)

func (m *Manager) GetOrCreateDeleteModeState(sessionKey string, p types.Platform, replyCtx any) *mode.DeleteModeState {
	m.mu.Lock()
	state, ok := m.states[sessionKey]
	if !ok || state == nil {
		state = &State{platform: p, replyCtx: replyCtx}
		m.states[sessionKey] = state
	} else {
		state.platform = p
		state.replyCtx = replyCtx
	}
	m.mu.Unlock()

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.deleteMode == nil {
		state.deleteMode = &mode.DeleteModeState{}
	}
	dm := state.deleteMode
	dm.Page = 1
	dm.Phase = "select"
	dm.Hint = ""
	dm.Result = ""
	dm.SelectedIDs = make(map[string]struct{})
	return dm
}

// 根据传入的sessionKey 执行对应args中的操作
func (m *Manager) ExecuteDeleteModeAction(sessionKey, args string) {
	m.mu.Lock()
	state := m.states[sessionKey]
	m.mu.Unlock()
	if state == nil {
		return
	}

	// 解析参数
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return // 没解析到参数
	}
	// 判断state的 deleteMode
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.deleteMode == nil {
		return
	}

	dm := state.deleteMode
	switch fields[0] {
	case "toggle":
		if len(fields) < 2 { // args: toggle {id}
			return
		}
		id := fields[1]
		if _, ok := dm.SelectedIDs[id]; ok {
			delete(dm.SelectedIDs, id)
		} else {
			dm.SelectedIDs[id] = struct{}{}
		}
		dm.Phase = "select"
		dm.Hint = ""
	case "page":
		if len(fields) < 2 { // args: page {pageid}
			return
		}
		if n, err := strconv.Atoi(fields[1]); err == nil && n > 0 {
			dm.Page = n
		}
		dm.Phase = "select"
	case "confirm":
		if len(dm.SelectedIDs) == 0 {
			dm.Phase = "select"
			dm.Hint = m.I18nT(i18n.MsgDeleteModeEmptySelection) // 请选择至少一个会话?
			return
		}
		dm.Phase = "confirm"
		dm.Hint = ""
	case "back":
		dm.Phase = "select"
	case "submit":
		lines := m.submitDeleteModeSelection(sessionKey, dm)
		dm.SelectedIDs = make(map[string]struct{})
		dm.Result = strings.Join(lines, "\n")
		dm.Hint = ""
		dm.Phase = "result"
	case "form-submit":
		dm.SelectedIDs = parseDeleteModeSelectedIDs(fields[1:])
		if len(dm.SelectedIDs) == 0 {
			dm.Phase = "select"
			dm.Hint = m.I18nT(i18n.MsgDeleteModeEmptySelection)
			return
		}
		dm.Phase = "confirm"
		dm.Hint = ""
	case "cancel":
		state.deleteMode = nil
	}
}

func (m *Manager) submitDeleteModeSelection(sessionKey string, dm *mode.DeleteModeState) []string {
	agent, sessions := m.sCtx.Agent(), m.sCtx.Sessions()
	deleter, ok := agent.(types.SessionDeleter)
	// 不支持删除session
	if !ok {
		return []string{m.I18nT(i18n.MsgDeleteNotSupported)}
	}
	// 通过opencode 命令列出session
	agentSessions, err := agent.ListSessions()
	if err != nil {
		return []string{m.I18nTf(i18n.MsgError, err)}
	}
	// 过滤tc-connect 拥有的Session
	agentSessions = types.FilterOwnedSessions(agentSessions, sessions.KnownAgentSessionIDs())
	seen := make(map[string]struct{}, len(agentSessions))
	lines := make([]string, 0, len(dm.SelectedIDs))
	for i := range agentSessions {
		seen[agentSessions[i].ID] = struct{}{}
		if _, ok := dm.SelectedIDs[agentSessions[i].ID]; !ok {
			continue
		}
		if line := m.delteSingleSessionReply(&types.Message{SessionKey: sessionKey}, deleter, &agentSessions[i]); line != "" {
			lines = append(lines, line)
		}
	}
	// 未找到的session
	missingIDs := make([]string, 0)
	for id := range dm.SelectedIDs {
		if _, ok := seen[id]; ok {
			continue
		}
		missingIDs = append(missingIDs, id)
	}
	sort.Strings(missingIDs)
	for _, id := range missingIDs {
		lines = append(lines, fmt.Sprintf(m.I18nT(i18n.MsgDeleteModeMissingSession), id))
	}
	if len(lines) == 0 {
		lines = append(lines, m.I18nT(i18n.MsgDeleteModeEmptySelection))
	}
	return lines
}

// 带返回的结果的删除方法
func (m *Manager) delteSingleSessionReply(msg *types.Message, deleter types.SessionDeleter, matched *types.AgentSessionInfo) string {
	if matched == nil {
		return ""
	}

	// 防止删除当前激活的session
	activeSession := m.sCtx.Sessions().GetOrCreateActive(msg.SessionKey)
	if activeSession.GetAgentSessionID() == matched.ID {
		return m.I18nT(i18n.MsgDeleteActiveDenied) // 不能删除当前的session
	}

	displayName := m.delteSessionDisplayName(m.sCtx.Sessions(), matched)
	if err := deleter.DeleteSession(matched.ID); err != nil {
		return m.I18nTf(i18n.MsgFailedToDeleteSession, displayName, err) // 删除失败
	}

	// 使用agent-side deletion 保持本地session 快照
	m.sCtx.Sessions().DeleteByAgentSessionID(matched.ID)
	m.sCtx.Sessions().SetSessionName(matched.ID, "")
	return fmt.Sprintf(m.I18nT(i18n.MsgDeleteSuccess), displayName)
}

// 尝试获取名称 Name -> Summary -> shortID, TODO: 移除Name直接用ID
func (m *Manager) delteSessionDisplayName(sessions *session.SessionManager, matched *types.AgentSessionInfo) string {
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

// ============================ 辅助方法 ============================

// 从参数字符串中解析要删除的id
func parseDeleteModeSelectedIDs(args []string) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, arg := range args {
		for _, id := range strings.Split(arg, ",") {
			if id == "" {
				continue
			}
			ids[id] = struct{}{}
		}

	}
	return ids
}
