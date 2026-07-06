package interactive

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"tc-connect/core/i18n"
	"tc-connect/core/mode"
	"tc-connect/core/session"
	"tc-connect/core/types"
	"time"
)

const maxQueuedMessages = 5 // 限制排队消息的数量以控制内存使用

// 使用e.agent 启动session,tcSessionKey 当非空时 用于TC_SESSION_KEY 环境注入,否则使用sessionKey
func (m *Manager) GetOrCreateInteractiveStateWith(sessionKey string, p types.Platform, replyCtx any, curSession *session.Session, sessions *session.SessionManager, agentName, tcSessionKey string) *State {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, ok := m.states[sessionKey]
	if ok && state.agentSession != nil && state.agentSession.Alive() {
		// 验证运行的agent session (sessionManager根据userKey获取的SessionID)
		// 是否匹配当前的active session. /new 和 /switch 之后 active sssion 改变,
		// 但是旧agent 进程可能仍存活, 重用它可能发送消息到错误的对话上下文
		wantID := curSession.GetAgentSessionID()
		currentID := state.agentSession.CurrentSessionID()
		// Reuse 仅当live 进程匹配 Session expects:
		// - IDs 匹配(与Claude session相同), 或 进程还没有报告ID (startup, empty want is OK)
		// 一个正确的ID, 重用会保持 --resume 上下文 - recycle (#238)
		needRecycle := currentID != "" && (wantID == "" || wantID != currentID)
		if !needRecycle { // 匹配成功
			return state
		}
		// 销毁stale agent 然后启动一个以匹配下面的Sesison
		slog.Info("interactive session mismatch, recycling",
			"session_key", sessionKey,
			"want_agent_session", wantID,
			"have_agent_session", currentID,
		)
		state.MarkStopped()
		//  同步关闭防止旧进程在新agent启动时继续输出(issue #237) 发生竞争
		closeAgentSessionWithTimeout(sessionKey, state.agentSession)
		delete(m.states, sessionKey)
		ok = false // 阻止下面读取stale设置
	}

	tcKey := sessionKey
	if tcSessionKey != "" {
		tcKey = tcSessionKey
	}

	// 注入per-session 环境变量, agent子进程可以调用 `tc-connect cron add` 等
	if inj, ok := m.sCtx.Agent().(types.SessionEnvInjector); ok {
		envVars := []string{
			"TC_PROJECT=" + m.sCtx.Name(),
			"TC_SESSION_KEY=" + tcKey,
		}
		if exePath, err := os.Executable(); err == nil {
			binDir := filepath.Dir(exePath)
			if curPath := os.Getenv("PATH"); curPath != "" {
				envVars = append(envVars, "PATH="+binDir+string(filepath.ListSeparator)+curPath)
			} else {
				envVars = append(envVars, "PATH="+binDir)
			}
		}
		inj.SetSessionEnv(envVars)
	}

	// 检查context是否早已取消(如 shutdown/restart 期间)
	if m.sCtx.Context().Err() != nil {
		slog.Debug("skipping session start: context canceled", "session_key", sessionKey)
		newState := &State{platform: p, replyCtx: replyCtx}
		adoptPendingFromPlaceholder(m.states[sessionKey], newState)
		state = newState
		m.states[sessionKey] = state
		return state
	}

	// Resume 仅当有一个正确保存的agent session ID. 如果 session 未绑定, 强制刷新启动
	// 而不是附加到此工作区中"最新的"CLI会话
	startSessionID := curSession.GetAgentSessionID() // core::Session 获取ID，何时赋值的？
	isResume := startSessionID != ""
	startAt := time.Now()
	agentSession, err := m.sCtx.Agent().StartSession(m.sCtx.Context(), startSessionID, agentName)
	startElapsed := time.Since(startAt)
	if err != nil {
		// resume/continue 失败 尝试刷新session作为回退
		if startSessionID != "" {
			slog.Error("session resume failed, falling back to fresh session",
				"session_key", sessionKey, "failed_session_id", startSessionID,
				"error", err, "elapsed", startElapsed)
			startAt = time.Now()
			agentSession, err = m.sCtx.Agent().StartSession(m.sCtx.Context(), "", "")
			startElapsed = time.Since(startAt)
			if err == nil {
				slog.Info("fresh session started after resume failure",
					"session_key", sessionKey, "elapsed", startElapsed)
			}
		}
		if err != nil {
			slog.Error("failed to start interactive session", "error", err, "elapsed", startElapsed)
			// 创建一个空agentSession的interactiveState
			newState := &State{platform: p, replyCtx: replyCtx}
			adoptPendingFromPlaceholder(m.states[sessionKey], newState)
			state = newState
			m.states[sessionKey] = state
			return state
		}
	}
	if startElapsed >= slowAgentStart {
		slog.Warn("slow agent session start", "elapsed", startElapsed, "agent", m.sCtx.Agent().Name(), "session_id", startSessionID)
	}

	if newID := agentSession.CurrentSessionID(); newID != "" {
		if curSession.CompareAndSetAgentSessionID(newID, m.sCtx.Agent().Name()) {
			sessions.Save()
		}
	}

	newState := &State{
		agentSession: agentSession,
		platform:     p,
		replyCtx:     replyCtx,
	}
	adoptPendingFromPlaceholder(m.states[sessionKey], newState)
	state = newState
	m.states[sessionKey] = state

	slog.Info("session spawned", "session_key", sessionKey, "agent_session", curSession.GetAgentSessionID(), "is_resume", isResume, "elapsed", startElapsed)
	return state
}

// 移除给定sessionKey的交互状态关闭他的agent session.当expected state提供时，如果map entry
// 被一个不同的state替换，cleanup跳过，这防止了stale goroutine(/new 之后仍然运行，并且一个新的
// turn从它启动)以免意外破坏替换状态
func (m *Manager) CleanupWithLock(sessionKey string, expected ...*State) {
	m.mu.Lock()
	state, ok := m.states[sessionKey]
	if len(expected) > 0 && expected[0] != nil && state != expected[0] {
		// 已经被替换，跳过
		m.mu.Unlock()
		return
	}
	// 任意futher进程之前捕获agent session
	var agentSession types.AgentSession
	if ok && state != nil {
		agentSession = state.agentSession
	}
	m.mu.Unlock()

	// 通知任意排队的message的sender 永远不会被处理
	if ok && state != nil {
		state.MarkStopped()
		m.notifyDroppedQueuedMessage(state, fmt.Errorf("session reset"))
	}

	// 从map删除前关闭agent session 之前，先关闭代理会话
	// 这可以防止清理过程出现竞争条件，即/stop 看到的是一个空映射
	// 并报告"No execution in processing",而agent session的Close()
	// 仍处于阻塞状态(130s)
	if agentSession != nil {
		closeAgentSessionWithTimeout(sessionKey, agentSession)
	}

	// session关闭之后从map删除state
	m.mu.Lock()
	// 再次检查关闭时state没有被代替
	currentState, curentOk := m.states[sessionKey]
	if curentOk && len(expected) > 0 && expected[0] != nil && currentState != expected[0] {
		// 关闭过程中另一个trun 已经替换了state - 不要删除
		m.mu.Unlock()
		return
	}
	delete(m.states, sessionKey)
	m.mu.Lock()
}

func (m *Manager) StopInteractiveSession(sessionKey string, quietReplyCtx any) bool {
	m.mu.Lock()
	state, ok := m.states[sessionKey]
	if !ok || state == nil {
		m.mu.Unlock()
		return false
	}

	state.mu.Lock()
	pending := state.pending
	state.pending = nil
	agentSession := state.agentSession
	state.mu.Unlock()

	state.MarkStopped()
	delete(m.states, sessionKey)
	m.mu.Unlock()

	if pending != nil {
		pending.Resolve()
	}
	m.notifyDroppedQueuedMessage(state, fmt.Errorf("session reset"))
	closeAgentSessionAsync(sessionKey, agentSession)
	return true
}

// 从状态中清理排队消息并向排队的message sender都发送错误通知, 当event循环正常退出(EvnetError, channel close)
// 并且排队信息无法代理到agent时调用
func (m *Manager) notifyDroppedQueuedMessage(state *State, reason error) {
	state.mu.Lock()
	remaining := state.pendingMessages
	state.pendingMessages = nil
	state.mu.Unlock()
	for _, q := range remaining {
		m.sCtx.Send(q.platform, q.replyCtx, fmt.Sprintf(m.I18nT(i18n.MsgError), reason))
	}
}

// 重置Session
func (m *Manager) MaybeAutoResetSessionOnIdle(p types.Platform, msg *types.Message, sessions *session.SessionManager, interactiveKey string, curSession *session.Session) *session.Session {
	if m.sCtx.ResetOnIdle() <= 0 || curSession == nil {
		return nil
	}

	hasBackend := curSession.GetAgentSessionID() != ""
	hasHistory := len(curSession.GetHistory(1)) > 0
	if !hasBackend && !hasHistory {
		return nil
	}

	lastActive := curSession.GetUpdatedAt()
	if lastActive.IsZero() || time.Since(lastActive) < m.sCtx.ResetOnIdle() {
		return nil
	}

	slog.Info("auto-resetting idle session",
		"session_key", msg.SessionKey,
		"session_id", curSession.ID,
		"idle_for", time.Since(lastActive),
		"threshold", m.sCtx.ResetOnIdle(),
	)

	// Check if the old session has an agent process that needs graceful
	// shutdown. If so, tell the user we're wrapping up before blocking.
	m.mu.Lock()
	state, hasState := m.states[interactiveKey]
	hasAgent := hasState && state != nil && state.agentSession != nil && state.agentSession.Alive()
	m.mu.Unlock()

	if hasAgent {
		// Notify the user before the potentially long close. The close
		// returns as soon as the process exits (usually seconds), but
		// Stop hooks can take up to 120s.
		m.sCtx.Reply(msg.ReplyCtx, m.I18nT(i18n.MsgSessionClosingGraceful))
	}

	m.CleanupWithLock(interactiveKey)
	curSession.UnlockWithoutUpdate()

	newSession := sessions.NewSession(msg.SessionKey, "")
	if !newSession.TryLock() {
		slog.Error("failed to lock new session after idle auto-reset", "session_key", msg.SessionKey, "new_session", newSession.ID)
		return nil
	}

	m.sCtx.Reply(msg.ReplyCtx, m.I18nTf(i18n.MsgSessionAutoResetIdle, int(m.sCtx.ResetOnIdle()/time.Minute)))
	return newSession
}

// 从state中移除pendingMessages 并给每一个队列中消息的发送者发送错误通知, 当 event loop 异常退出
// (EventError, channel closed) 并且排队消息不在分发给agent时调用
func (m *Manager) notifyDroppedQueuedMessages(state *State, reason error) {
	state.mu.Lock()
	remaining := state.pendingMessages
	state.pendingMessages = nil
	state.mu.Unlock()
	for _, q := range remaining {
		m.sCtx.Send(q.platform, q.replyCtx, fmt.Sprintf(m.I18nT(i18n.MsgError), reason))
	}
}

// ============================ message drain ============================

// 处理state的pendingMessages队列中所有的排队消息. 当queue为空时(当holding state.mu) 解锁session 来关闭"queue empty" 和
// "session unlocked"之间的竞争. 如果session被此调用解锁, 返回true
func (m *Manager) drainPendingMessages(state *State, session *session.Session, sessions *session.SessionManager, sessionKey string) bool {
	for {
		state.mu.Lock()
		if len(state.pendingMessages) == 0 {
			session.Unlock()
			state.mu.Unlock()
			return true
		}
		queued := state.pendingMessages[0]
		state.pendingMessages = state.pendingMessages[1:]
		state.platform = queued.platform
		state.replyCtx = queued.replyCtx
		state.fromVoice = queued.fromVoice
		state.mu.Unlock()

		m.sCtx.I18n().DetectAndSet(queued.content)
		prompt := m.buildSenderPrompt(queued.content, queued.userID, queued.msgPlatform, queued.msgSessionKey)

		if state.agentSession == nil || !state.agentSession.Alive() {
			m.sCtx.Send(queued.platform, queued.replyCtx, fmt.Sprintf(m.I18nT(i18n.MsgError), "agent session ended"))
			m.notifyDroppedQueuedMessages(state, fmt.Errorf("agent session ended"))
			return false
		}

		types.DrainEvents(state.agentSession.Events())

		session.AddHistory("user", queued.content)

		sendDone := make(chan error, 1)
		go func() {
			sendDone <- state.agentSession.Send(prompt, queued.images, queued.files)
		}()

		var stopTyping func()
		if ti, ok := queued.platform.(types.TypingIndicator); ok {
			stopTyping = ti.StartTyping(m.sCtx.Context(), queued.replyCtx)
		}

		slog.Info("processing queued message", "session", sessionKey)
		m.processInteractiveEvents(state, session, sessions, sessionKey, "", time.Now(), stopTyping, sendDone, queued.replyCtx)
	}
}

// 处理在 /compress 操作期间排队的任意messages, 发送每一个到agent并运行完整的交互时间循环
func (m *Manager) drainQueuedMessagesAfterCompress(state *State, session *session.Session, sessions *session.SessionManager, sessionKey string, unlocked *bool) {
	if m.drainPendingMessages(state, session, sessions, sessionKey) {
		*unlocked = true
	}
}

// 当消息排队但是drain loop 早已退出时调用, 处理所有排队的message,
// 与processInteractiveMessageWith中的drain loop类似，但作为一个独立的goroutine。
func (m *Manager) DrainOrphanedQueue(session *session.Session, sessions *session.SessionManager, interactiveKey string, agent types.Agent, workspaceDir string) {
	unlocked := false
	defer func() {
		if !unlocked {
			session.Unlock()
		}
	}()

	m.mu.Lock()
	state, hasState := m.states[interactiveKey]
	m.mu.Unlock()

	if !hasState || state == nil || state.agentSession == nil || !state.agentSession.Alive() {
		if hasState && state != nil {
			m.notifyDroppedQueuedMessages(state, fmt.Errorf("agent session ended"))
		}
		return
	}

	unlocked = m.drainPendingMessages(state, session, sessions, interactiveKey)
}

// 当session繁忙时排队message以便延后, 排队时message不会发送到agent stdin
// 当前turn的EventResult接收后事件循环发送它, 如果message成功排队返回true, 失败返回falszMe
func (m *Manager) QueueMessageForBusySession(p types.Platform, msg *types.Message, interactiveKey string) bool {
	m.mu.Lock()
	state, hasState := m.states[interactiveKey]
	m.mu.Unlock()

	if !hasState || state == nil {
		return false
	}
	// Allow queueing when agentSession is nil (session is starting up,
	// issue #565). Only reject if the session was established and died.
	if state.agentSession != nil && !state.agentSession.Alive() {
		return false
	}

	// Only queue metadata — do NOT send to agent stdin yet.
	// The agent CLI may treat a mid-turn stdin message as part of the
	// current turn, causing the event loop to hang waiting for a second
	// EventResult that never arrives. Instead, the event loop sends the
	// message after the current turn's EventResult is received.
	state.mu.Lock()
	if len(state.pendingMessages) >= maxQueuedMessages {
		state.mu.Unlock()
		return false // fall back to "previous processing" reply
	}
	state.pendingMessages = append(state.pendingMessages, queuedMessage{
		platform:      p,
		replyCtx:      msg.ReplyCtx,
		content:       msg.Content,
		images:        msg.Images,
		files:         msg.Files,
		fromVoice:     msg.FromVoice,
		userID:        msg.UserID,
		msgPlatform:   msg.Platform,
		msgSessionKey: msg.SessionKey,
	})
	queueDepth := len(state.pendingMessages)
	state.mu.Unlock()

	slog.Info("message queued for busy session",
		"session", msg.SessionKey,
		"user", msg.UserName,
		"queue_depth", queueDepth,
	)
	m.sCtx.Reply(msg.ReplyCtx, m.I18nT(i18n.MsgMessageQueued))
	return true
}

// 创建一个placeholder interactiveState entry 若不存在, 这使得在代理会话
// 启动过程中到达的消息能够被排队，而不会被丢弃（问题#565）。
func (m *Manager) EnsureInteractiveStateForQueueing(key string, p types.Platform, replyCtx any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.states[key]; !ok {
		m.states[key] = &State{
			platform: p,
			replyCtx: replyCtx,
		}
	}
}

func (m *Manager) GetDeleteModeState(sessionKey string) *mode.DeleteModeState {
	m.mu.Lock()
	state := m.states[sessionKey]
	m.mu.Unlock()
	if state == nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.deleteMode == nil {
		return nil
	}
	cp := &mode.DeleteModeState{
		Page:        state.deleteMode.Page,
		SelectedIDs: make(map[string]struct{}, len(state.deleteMode.SelectedIDs)),
		Phase:       state.deleteMode.Phase,
		Hint:        state.deleteMode.Hint,
		Result:      state.deleteMode.Result,
	}
	for id := range state.deleteMode.SelectedIDs {
		cp.SelectedIDs[id] = struct{}{}
	}
	return cp
}
