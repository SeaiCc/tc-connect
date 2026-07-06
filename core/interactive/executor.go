package interactive

import (
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"tc-connect/core/i18n"
	"tc-connect/core/session"
	"tc-connect/core/skill"
	"tc-connect/core/streaming"
	"tc-connect/core/types"
	"time"
	"unicode/utf8"
)

const maxPlatformMessageLen = 4000

// 慢-op阈值, 操作超出阈值会产生slog.Warn 以便快速找出瓶颈
const (
	slowAgentSend       = 2 * time.Second  // agentSession.Send
	slowAgentFirstEvent = 15 * time.Second // 从发送第一次agent event开始的计时
	slowPlatformSend    = 2 * time.Second  // 平台响应,发送
)

func (m *Manager) HandlePendingPermission(p types.Platform, msg *types.Message, content string) bool {
	m.mu.Lock()
	state, ok := m.states[msg.SessionKey]
	m.mu.Unlock()
	if !ok || state == nil {
		return false
	}

	state.mu.Lock()
	pending := state.pending
	state.mu.Unlock()
	if pending == nil {
		return false
	}

	// AskUserQuestion: interpret user response as an answer, not a permission decision
	if len(pending.Questions) > 0 {
		curIdx := pending.CurrentQuestion
		q := pending.Questions[curIdx]
		answer := resolveAskQuestionAnswer(q, content)

		if pending.Answers == nil {
			pending.Answers = make(map[int]string)
		}
		pending.Answers[curIdx] = answer

		// More questions remaining — advance to next and send new card
		if curIdx+1 < len(pending.Questions) {
			pending.CurrentQuestion = curIdx + 1
			m.sCtx.Reply(msg.ReplyCtx, fmt.Sprintf("✅ %s: **%s**", q.Question, answer))
			m.sCtx.MsgHandlder().SendAskQuestionPrompt(p, msg.ReplyCtx, pending.Questions, curIdx+1)
			return true
		}

		// All questions answered — build response and resolve
		updatedInput := buildAskQuestionResponse(pending.ToolInput, pending.Questions, pending.Answers)

		if err := state.agentSession.RespondPermission(pending.RequestID, types.PermissionResult{
			Behavior:     "allow",
			UpdatedInput: updatedInput,
		}); err != nil {
			slog.Error("failed to send AskUserQuestion response", "error", err)
			m.sCtx.Reply(msg.ReplyCtx, fmt.Sprintf(m.I18nT(i18n.MsgError), err))
		} else {
			m.sCtx.Reply(msg.ReplyCtx, fmt.Sprintf("✅ %s: **%s**", q.Question, answer))
		}

		state.mu.Lock()
		state.pending = nil
		state.mu.Unlock()
		pending.Resolve()
		return true
	}

	lower := strings.ToLower(strings.TrimSpace(content))

	if isApproveAllResponse(lower) {
		state.mu.Lock()
		state.approveAll = true
		state.mu.Unlock()

		if err := state.agentSession.RespondPermission(pending.RequestID, types.PermissionResult{
			Behavior:     "allow",
			UpdatedInput: pending.ToolInput,
		}); err != nil {
			slog.Error("failed to send permission response", "error", err)
			m.sCtx.Reply(msg.ReplyCtx, fmt.Sprintf(m.I18nT(i18n.MsgError), err))
		} else {
			m.sCtx.Reply(msg.ReplyCtx, m.I18nT(i18n.MsgPermissionApproveAll))
		}
	} else if isAllowResponse(lower) {
		if err := state.agentSession.RespondPermission(pending.RequestID, types.PermissionResult{
			Behavior:     "allow",
			UpdatedInput: pending.ToolInput,
		}); err != nil {
			slog.Error("failed to send permission response", "error", err)
			m.sCtx.Reply(msg.ReplyCtx, fmt.Sprintf(m.I18nT(i18n.MsgError), err))
		} else {
			m.sCtx.Reply(msg.ReplyCtx, m.I18nT(i18n.MsgPermissionAllowed))
		}
	} else if isDenyResponse(lower) {
		if err := state.agentSession.RespondPermission(pending.RequestID, types.PermissionResult{
			Behavior: "deny",
			Message:  "User denied this tool use.",
		}); err != nil {
			slog.Error("failed to send deny response", "error", err)
		}
		m.sCtx.Reply(msg.ReplyCtx, m.I18nT(i18n.MsgPermissionDenied))
	} else {
		m.sCtx.Reply(msg.ReplyCtx, m.I18nT(i18n.MsgPermissionHint))
		return true
	}

	state.mu.Lock()
	state.pending = nil
	state.mu.Unlock()
	pending.Resolve()

	return true
}

func (m *Manager) ExecuteSkill(p types.Platform, msg *types.Message, excSkill *skill.Skill, args []string) {
	prompt := skill.BuildSkillInvocationPrompt(excSkill, args)

	session := m.sCtx.Sessions().GetOrCreateActive(msg.SessionKey)
	if !session.TryLock() {
		m.sCtx.Reply(msg.ReplyCtx, m.I18nT(i18n.MsgPreviousProcessing))
		return
	}

	slog.Info("executing skill",
		"skill", excSkill.Name,
		"source", excSkill.Source,
		"user", msg.UserName,
	)

	msg.Content = prompt
	go m.ProcessInteractiveMessage(p, msg, session)
}

func (m *Manager) ProcessInteractiveMessage(p types.Platform, msg *types.Message, session *session.Session) {
	m.ProcessInteractiveMessageWith(p, msg, session, m.sCtx.Agent(), m.sCtx.Sessions(), msg.SessionKey, "", "")
}

// 核心交互处理循环, 接收显示agent, ineractiveKey (用于interactiveStates map) 和workDir
// multi-workspace 可以路由到每个工作区agents. ccSessionKey, 当非空时, 使用环境变量的
// CC_SESSION_KEY, 否则使用interactiveKey
func (m *Manager) ProcessInteractiveMessageWith(p types.Platform, msg *types.Message, session *session.Session, agent types.Agent, sessions *session.SessionManager, interactiveKey string, workspaceDir string, tcSessionKey string) {
	// session.Unlock 不在此处deffered - 在下面的drain loop 显示调用, 当holding state.mu
	// 关闭"queue is empty"和"session unlocked" 之间的竞争窗口. deffered 回退 确保
	// lock 在 early-return paths 释放
	unlocked := false
	defer func() {
		if !unlocked {
			session.Unlock()
		}
	}()

	if m.sCtx.Context().Err() != nil {
		return
	}

	turnStart := time.Now()

	m.sCtx.I18n().DetectAndSet(msg.Content)
	// 当前消息添加至历史消息
	session.AddHistory("user", msg.Content)

	// 根据 platform侧提供的key,获取当前interactiveState 或创建新的
	state := m.GetOrCreateInteractiveStateWith(interactiveKey, p, msg.ReplyCtx, session, sessions, msg.AgentName, tcSessionKey)

	// 更新此轮的reply context
	state.mu.Lock()
	state.platform = p
	state.replyCtx = msg.ReplyCtx
	state.mu.Unlock()

	if state.agentSession == nil {
		m.sCtx.Reply(msg.ReplyCtx, m.I18nT(i18n.MsgFailedToStartAgentSession))
		return
	}

	// 应用per-message 权限mode重写(如 带有 mode = "bypassPermissions"的cron jobs)
	// 仅当override的SetLiveMode成功时 defer restores, 目前仅ExecuteCronJob使用此字段
	if msg.ModeOverride != "" {
		if switcher, ok := state.agentSession.(types.LiveModeSwitcher); ok {
			if switcher.SetLiveMode(msg.ModeOverride) {
				defer func() {
					defaultMode := "default"
					if ma, ok := m.sCtx.Agent().(interface{ GetMode() string }); ok {
						if m := ma.GetMode(); m != "" {
							defaultMode = m
						}
					}
					switcher.SetLiveMode(defaultMode)
				}()
			}
		}
	}

	// 打字机效果支持
	// 所有权被转移给processInteractiveEvents，该组件负责在队列消息轮次中管理//停止/重新启动该事件。
	var stopTyping func()
	if ti, ok := p.(types.TypingIndicator); ok {
		stopTyping = ti.StartTyping(m.sCtx.Context(), msg.ReplyCtx)
	}
	defer func() {
		// 如果所有权未转移到processInteractiveEvents 停止打字机效果
		// (如 调用之前停止)
		if stopTyping != nil {
			stopTyping()
		}
	}()

	// 清除上一轮留在通道中的所有过期事件。
	// 这可以防止下一个processInteractiveEvents读取在上一个回合已经返回之后推送的旧EventResult。
	types.DrainEvents(state.agentSession.Events())

	promptContent := m.buildSenderPrompt(msg.Content, msg.UserID, msg.Platform, msg.SessionKey)

	sendStart := time.Now()
	state.mu.Lock()
	state.fromVoice = msg.FromVoice
	state.sideText = ""
	state.mu.Unlock()

	// 和processInteractiveEvents并行发送,某些agent 内部阻塞Send直到prompt 关闭
	// (如 ACP session/prompt); 当阻塞时可能触发 EventPermissionRequest - event loop
	// 必须并行运行
	sendDone := make(chan error, 1)
	go func() {
		sendDone <- state.agentSession.Send(promptContent, msg.Images, msg.Files)
	}()

	m.processInteractiveEvents(state, session, sessions, interactiveKey, msg.MessageID, turnStart, stopTyping, sendDone, msg.ReplyCtx)
	if elapsed := time.Since(sendStart); elapsed >= slowAgentSend {
		slog.Warn("slow agent send", "elapsed", elapsed, "session", msg.SessionKey, "content_len", len(msg.Content))
	}
	stopTyping = nil // ownership transferred; prevent defer from double-stopping

	// 防止narrow race: 一条消息可能在processInteractiveEvent 观察空队列 和
	// 在此处返回间排队, (session仍被阻塞, handleMessage的TryLock失败并路由
	// 消息到queueMessageForBusySession), 移除所有这样的孤儿消息
	if m.drainPendingMessages(state, session, sessions, interactiveKey) {
		unlocked = true
	}
}

func (m *Manager) processInteractiveEvents(state *State, session *session.Session, sessions *session.SessionManager, sessionKey string, msgID string, turnStart time.Time, stopTypingFn func(), sendDone <-chan error, replyCtx any) {
	var textParts []string
	// textParts 的index: 此前的text已经被发送/展示
	var segmentStart int
	toolCount := 0
	waitStart := time.Now()
	firstEventLogged := false
	triggerAutoCompress := false
	pendingSend := sendDone

	// stopTyping 追踪当前turn的 打字机indicator 这样一个 queue message 可以启动给一个新turn
	stopTyping := stopTypingFn
	defer func() {
		if stopTyping != nil {
			stopTyping()
		}
	}()

	state.mu.Lock()
	workspaceDir := state.workspaceDir
	workspaceRenderer := func(content string) string {
		return m.Renderer().RenderOutgoingContentForWorkspace(state.platform, content, workspaceDir)
	}
	sendWorkspace := func(p types.Platform, replyCtx any, content string) {
		m.sCtx.MsgHandlder().SendForWorkspace(p, replyCtx, content, workspaceDir)
	}
	sendWorkspaceWithError := func(p types.Platform, replyCtx any, content string) error {
		return m.sCtx.MsgHandlder().SendWithErrorForWorkspace(p, replyCtx, content, workspaceDir)
	}
	sp := streaming.NewStreamPreview(m.sCtx.StreamPreviewCfg(), state.platform, state.replyCtx, m.sCtx.Context(), workspaceRenderer)
	cp := NewCompactProgressWriter(m.sCtx.Context(), state.platform, state.replyCtx, m.sCtx.Agent().Name(), m.sCtx.I18n().CurrentLang(), workspaceRenderer)
	state.mu.Unlock()

	// 交互计时 Idle timeout: 0 = disabled
	var idleTimer *time.Timer
	var idleCh <-chan time.Time
	if m.sCtx.EventIdleTimeout() > 0 {
		idleTimer = time.NewTimer(m.sCtx.EventIdleTimeout())
		defer idleTimer.Stop()
		idleCh = idleTimer.C
	}

	// 处理event 和 stop 信号
	events := state.agentSession.Events()
	stopCh := state.StopSignal()
	for {
		var event types.Event
		var ok bool

		select {
		case <-stopCh:
			sp.Discard()
			return
		case event, ok = <-events:
			if !ok {
				goto channelClosed
			}
		case err := <-pendingSend:
			pendingSend = nil
			if err != nil {
				slog.Error("failed to send prompt", "error", err, "session_key", sessionKey)
				sp.Discard()
				if stopTyping != nil {
					stopTyping()
					stopTyping = nil
				}
				m.notifyDroppedQueuedMessages(state, err)
				if state.agentSession == nil || !state.agentSession.Alive() {
					m.CleanupWithLock(sessionKey, state)
				}
				state.mu.Lock()
				p := state.platform
				state.mu.Unlock()
				m.sCtx.Send(p, replyCtx, fmt.Sprintf(m.I18nT(i18n.MsgError), err))
				return
			}
			continue
		case <-idleCh:
			slog.Error("agent session idle timeout: no events for too long, killing session",
				"session_key", sessionKey, "timeout", m.sCtx.EventIdleTimeout(), "elapsed", time.Since(turnStart))
			cp.Finalize(ProgressCardStateFailed)
			sp.Discard()
			state.mu.Lock()
			p := state.platform
			state.mu.Unlock()
			m.sCtx.Send(p, replyCtx, fmt.Sprintf(m.I18nT(i18n.MsgError), "agent session timed out (no response)"))
			m.CleanupWithLock(sessionKey, state)
			return
		case <-m.sCtx.Context().Done():
			return
		}

		if state.isStopped() {
			sp.Discard()
			return
		}

		// 接收到一个event之后重置idle timer
		if idleTimer != nil {
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(m.sCtx.EventIdleTimeout())
		}

		// 首次事件时间判断
		if !firstEventLogged {
			firstEventLogged = true
			if elapsed := time.Since(waitStart); elapsed >= slowAgentFirstEvent {
				slog.Warn("slow agent first event", "elapsed", elapsed, "session", sessionKey, "event_type", event.Type)
			}
		}

		state.mu.Lock()
		p := state.platform
		state.mu.Unlock()

		switch event.Type {
		case types.EventThinking:
			if m.sCtx.DisplayCfg().ThinkingMessages && event.Content != "" {
				// 刷新思考显示前的累积文本段
				previewActive := sp.CanPreview()
				if len(textParts) > segmentStart {
					if !previewActive {
						segment := strings.Join(textParts[segmentStart:], "")
						if segment != "" {
							for _, chunk := range splitMessage(segment, maxPlatformMessageLen) {
								sendWorkspace(p, replyCtx, chunk)
							}
						}
					}
					segmentStart = len(textParts)
				}
				sp.Freeze()
				if previewActive {
					sp.DetachPreview() // 将冻结预览作为永久消息保持可见
				}
				preview := truncateIf(event.Content, m.sCtx.DisplayCfg().ThinkingMaxLen)
				thinkingMsg := fmt.Sprintf(m.I18nT(i18n.MsgThinking), preview)
				if !cp.AppendEvent(ProgressEntryThinking, preview, "", thinkingMsg) {
					sendWorkspace(p, replyCtx, thinkingMsg)
				}
			}

		// TODO: ToolUse ToolResult
		case types.EventText:
			if event.Content != "" {
				textParts = append(textParts, event.Content)
				if sp.CanPreview() {
					sp.AppendText(event.Content)
				}
			}
			if event.SessionID != "" {
				if session.CompareAndSetAgentSessionID(event.SessionID, m.sCtx.Agent().Name()) {
					pendingName := session.GetName()
					if pendingName != "" && pendingName != "session" && pendingName != "default" {
						sessions.SetSessionName(event.SessionID, pendingName)
					}
					sessions.Save()
				}
			}

		case types.EventPermissionRequest:
			// PermissionRequest -> AskUserQuestion ??
			isAskQuestion := event.ToolName == "AskUserQuestion" && len(event.Questions) > 0

			state.mu.Lock()
			// autoApprove := state.approveAll
			autoApprove := false
			state.mu.Unlock()

			if autoApprove && !isAskQuestion {
				slog.Debug("auto-approving (approve-all)", "request_id", event.RequestID, "tool", event.ToolName)
				_ = state.agentSession.RespondPermission(event.RequestID, types.PermissionResult{
					Behavior:     "allow",
					UpdatedInput: event.ToolInputRaw,
				})
				continue
			}

			// flush permission prompt 之前的累积文本段落
			previewActive := sp.CanPreview()
			if len(textParts) > segmentStart {
				if !previewActive {
					segment := strings.Join(textParts[segmentStart:], "")
					if segment != "" {
						for _, chunk := range splitMessage(segment, maxPlatformMessageLen) {
							sendWorkspace(p, replyCtx, chunk)
						}
					}
				}
				segmentStart = len(textParts)
			}
			sp.Freeze()
			if previewActive {
				sp.DetachPreview() // 将冻结预览作为永久消息保持可见
			}

			slog.Info("permission request",
				"request_id", event.RequestID,
				"tool", event.ToolName,
			)

			pending := &pendingPermission{
				RequestID:    event.RequestID,
				ToolName:     event.ToolName,
				ToolInput:    event.ToolInputRaw,
				InputPreview: event.ToolInput,
				Questions:    event.Questions,
				Resolved:     make(chan struct{}),
			}
			state.mu.Lock()
			state.pending = pending
			state.mu.Unlock()

			if isAskQuestion {
				m.sCtx.MsgHandlder().SendAskQuestionPrompt(p, replyCtx, event.Questions, 0)
			} else {
				permLimit := m.sCtx.DisplayCfg().ToolMaxLen
				if permLimit > 0 {
					permLimit = permLimit * 8 / 5
				}
				toolInput := truncateIf(event.ToolInput, permLimit)
				prompt := fmt.Sprintf(m.I18nT(i18n.MsgPermissionPrompt), event.ToolName, toolInput)
				m.sCtx.Send(p, replyCtx, prompt)
			}

			// 当等待用户permission 响应是停止idle计时器, 用户可能需要化较长时间决定,
			// 防止因为超时kill会话
			if idleTimer != nil {
				idleTimer.Stop()
			}

			<-pending.Resolved
			slog.Info("permission resolved", "request_id", event.RequestID)

			// 重启计时器
			if idleTimer != nil {
				idleTimer.Reset(m.sCtx.EventIdleTimeout())
			}

		case types.EventResult:
			cp.Finalize(ProgressCardStateCompleted)
			// 使用 state.agentSession.CurrnetSessionID() 而不是 event.SessionID
			// event.SessionID 可能在某些情况下为空,造成agent_session_id 不被持久化到磁盘
			// 打断下次重启时的恢复
			if state != nil && state.agentSession != nil {
				if currentID := state.agentSession.CurrentSessionID(); currentID != "" {
					session.SetAgentSessionID(currentID, m.sCtx.Agent().Name())
					sessions.Save()
				}
			}

			fullResponse := event.Content
			if fullResponse == "" && len(textParts) > 0 {
				fullResponse = strings.Join(textParts, "")
			}
			if fullResponse == "" {
				fullResponse = m.I18nT(i18n.MsgEmptyResponse)
			}

			// Context 使用指示器: 优先SDK tokens -> self-reported
			sdkPlausible := event.InputTokens >= 100
			selfPct := parseSelfReportedCtx(fullResponse)
			cleanResponse := ctxSelfReportRe.ReplaceAllString(fullResponse, "")
			cleanResponse = strings.TrimRight(cleanResponse, "\n ")

			// 评估自动压缩trigger(基于user+assistant文本的token estimate, 包括当前turn的
			// 在添加到history之前的assistant 响应)
			if m.sCtx.AutoCompressEnabled() && m.sCtx.AutoCompressMaxTokens() > 0 {
				estimate := estimateTokensWithPendingAssistant(session.GetHistory(0), cleanResponse)
				now := time.Now()
				state.mu.Lock()
				last := state.lastAutoCompressAt
				state.mu.Unlock()
				if estimate >= m.sCtx.AutoCompressMaxTokens() && (last.IsZero() || now.Sub(last) >= m.sCtx.AutoCompressMinGap()) {
					triggerAutoCompress = true
					state.mu.Lock()
					state.lastAutoCompressTokens = estimate
					state.mu.Unlock()
				}
			}

			session.AddHistory("assistant", cleanResponse)
			sessions.Save()

			if m.sCtx.ShowContextIndicator() {
				if sdkPlausible {
					cleanResponse += contextIndicator(event.InputTokens)
				} else if selfPct > 0 {
					cleanResponse += fmt.Sprintf("\n[ctx: ~%d%%]", selfPct)
				}
			}
			fullResponse = cleanResponse

			turnDuration := time.Since(turnStart)
			slog.Info("turn complete",
				"session", session.ID,
				"agent_session", session.GetAgentSessionID(),
				"msg_id", msgID,
				"tools", toolCount,
				"response_len", len(fullResponse),
				"turn_duration", turnDuration,
				"input_tokens", event.InputTokens,
				"output_tokens", event.OutputTokens,
			)

			replyStart := time.Now()
			normalizedResponse := strings.TrimSpace(fullResponse)
			state.mu.Lock()
			suppressDuplicate := normalizedResponse != "" && normalizedResponse == state.sideText
			state.sideText = ""
			state.mu.Unlock()

			// When tool calls happened and prior text was already surfaced in segments,
			// only send the unsent remainder. When tool progress is hidden, tool events don't surface
			// side-channel messages and segmentStart stays 0, so keep normal finalize flow.
			if toolCount > 0 && segmentStart > 0 {
				sp.Discard()
				if segmentStart < len(textParts) {
					unsent := strings.Join(textParts[segmentStart:], "")
					if unsent != "" {
						for _, chunk := range splitMessage(unsent, maxPlatformMessageLen) {
							if err := sendWorkspaceWithError(p, replyCtx, chunk); err != nil {
								return
							}
						}
					}
				}
			} else if suppressDuplicate {
				sp.Discard()
				slog.Debug("EventResult: suppressed duplicate side-channel text", "response_len", len(fullResponse))
			} else if sp.Finish(fullResponse) {
				slog.Debug("EventResult: finalized via stream preview", "response_len", len(fullResponse))
			} else {
				slog.Debug("EventResult: sending via p.Send (preview inactive or failed)", "response_len", len(fullResponse), "chunks", len(splitMessage(fullResponse, maxPlatformMessageLen)))
				for _, chunk := range splitMessage(fullResponse, maxPlatformMessageLen) {
					if err := sendWorkspaceWithError(p, replyCtx, chunk); err != nil {
						return
					}
				}
			}

			if elapsed := time.Since(replyStart); elapsed >= slowPlatformSend {
				slog.Warn("slow final reply send", "platform", p.Name(), "elapsed", elapsed, "response_len", len(fullResponse))
			}

			//TODO: TTS

			// 结束一个turn 发送任意queued 消息之前自动压缩
			if triggerAutoCompress {
				// TODO: Agent压缩
				slog.Warn("Do not support AutoCompress.")
			}

			// 检查排队消息 - 如果存在,继续时间循环而不是返回
			state.mu.Lock()
			if len(state.pendingMessages) > 0 {
				queued := state.pendingMessages[0]
				state.pendingMessages = state.pendingMessages[1:]
				remainingQueue := len(state.pendingMessages)
				state.platform = queued.platform
				state.replyCtx = queued.replyCtx
				state.fromVoice = queued.fromVoice
				state.mu.Unlock()

				// 停止先前的打字机效果
				if stopTyping != nil {
					stopTyping()
					stopTyping = nil
				}
				// 启动typing indicator 用于排队的消息上下文
				if ti, ok := queued.platform.(types.TypingIndicator); ok {
					stopTyping = ti.StartTyping(m.sCtx.Context(), queued.replyCtx)
				}

				// 启动新turn之前, 先处理到过时的时间. 在// EventResult和Send()之间，
				// 唯一缓冲的事件将是过时的剩余事件（例如，来自cmd.Wait()的延迟EventError）。
				types.DrainEvents(state.agentSession.Events())

				if pendingSend != nil {
					if err := <-pendingSend; err != nil {
						slog.Debug("async send error before queued turn", "error", err)
					}
				}

				queuedPrompt := m.buildSenderPrompt(queued.content, queued.userID, queued.msgPlatform, queued.msgSessionKey)

				nextSend := make(chan error, 1)
				go func() {
					nextSend <- state.agentSession.Send(queuedPrompt, queued.images, queued.files)
				}()
				pendingSend = nextSend

				// 立即检测语言（从队列时间推迟，以避免在前一轮仍在运行时切换区域设置）。
				m.sCtx.I18n().DetectAndSet(queued.content)

				// 给下一次trun 重置 per-turrn
				textParts = nil
				segmentStart = 0
				toolCount = 0
				turnStart = time.Now()
				firstEventLogged = false
				waitStart = time.Now()
				queuedRenderer := func(content string) string {
					return m.Renderer().RenderOutgoingContentForWorkspace(queued.platform, content, workspaceDir)
				}
				sp = streaming.NewStreamPreview(m.sCtx.StreamPreviewCfg(), queued.platform, queued.replyCtx, m.sCtx.Context(), queuedRenderer)
				cp = NewCompactProgressWriter(m.sCtx.Context(), queued.platform, queued.replyCtx, m.sCtx.Agent().Name(), m.sCtx.I18n().CurrentLang(), queuedRenderer)

				session.AddHistory("user", queued.content)

				if idleTimer != nil {
					if !idleTimer.Stop() {
						select {
						case <-idleTimer.C:
						default:
						}
					}
					idleTimer.Reset(m.sCtx.EventIdleTimeout())
				}

				slog.Info("processing queued message",
					"session", sessionKey,
					"remaining_queue", remainingQueue,
				)
				continue
			}
			state.mu.Unlock()

			if pendingSend != nil {
				if err := <-pendingSend; err != nil {
					slog.Debug("async send error after EventResult", "error", err)
				}
			}
			return

		case types.EventError:
			cp.Finalize(ProgressCardStateFailed)
			sp.Discard()
			if event.Error != nil {
				slog.Error("agent error", "error", event.Error)
				m.sCtx.Send(p, replyCtx, fmt.Sprintf(m.I18nT(i18n.MsgError), event.Error))
			}
			// Only drop queued messages if the agent session is dead.
			// Some agents (e.g. Codex) emit EventError for per-turn failures
			// while keeping the session alive for subsequent turns.
			if state.agentSession == nil || !state.agentSession.Alive() {
				m.notifyDroppedQueuedMessages(state, event.Error)
			}
			return
		}
	}

channelClosed:
	// Channel closed - process exited unexpectedly
	slog.Warn("agent process exited", "session_key", sessionKey)
	m.notifyDroppedQueuedMessages(state, fmt.Errorf("agent process exited"))
	m.CleanupWithLock(sessionKey, state)

	if len(textParts) > 0 {
		state.mu.Lock()
		p := state.platform
		state.mu.Unlock()

		fullResponse := strings.Join(textParts, "")
		session.AddHistory("assistant", fullResponse)

		if toolCount > 0 && segmentStart > 0 {
			sp.Discard()
			if segmentStart < len(textParts) {
				unsent := strings.Join(textParts[segmentStart:], "")
				if unsent != "" {
					for _, chunk := range splitMessage(unsent, maxPlatformMessageLen) {
						if err := sendWorkspaceWithError(p, replyCtx, chunk); err != nil {
							return
						}
					}
				}
			}
		} else if sp.Finish(fullResponse) {
			slog.Debug("stream preview: finalized in-place (process exited)")
		} else {
			for _, chunk := range splitMessage(fullResponse, maxPlatformMessageLen) {
				if err := sendWorkspaceWithError(p, replyCtx, chunk); err != nil {
					return
				}
			}
		}
	}
}

// 准备sender 识别头 到content 当 injectSender可用 userID 非空
func (m *Manager) buildSenderPrompt(content, userID, platform, sessionKey string) string {
	if !m.sCtx.InjectSender() || userID == "" {
		return content
	}
	chatID := types.ChannelID(sessionKey)
	return fmt.Sprintf("[tc-connect sender_id=%s platform=%s chat_id=%s]\n%s", userID, platform, chatID, content)
}

// ================================= 辅助方法 =================================

// 将用户输入转换为答案文本, 处理button 回调("askq:qIdx:optIdx"), 数字选择("1", "1,3"), 和free text
func resolveAskQuestionAnswer(q types.UserQuestion, input string) string {
	input = strings.TrimSpace(input)

	// Handle card button callback: "askq:qIdx:optIdx"
	if strings.HasPrefix(input, "askq:") {
		parts := strings.SplitN(input, ":", 3)
		if len(parts) == 3 {
			if idx, err := strconv.Atoi(parts[2]); err == nil && idx >= 1 && idx <= len(q.Options) {
				return q.Options[idx-1].Label
			}
		}
		// Legacy format "askq:N"
		if len(parts) == 2 {
			if idx, err := strconv.Atoi(parts[1]); err == nil && idx >= 1 && idx <= len(q.Options) {
				return q.Options[idx-1].Label
			}
		}
	}

	// Try numeric index(es)
	if q.MultiSelect {
		parts := strings.FieldsFunc(input, func(r rune) bool { return r == ',' || r == '，' || r == ' ' })
		var labels []string
		allNumeric := true
		for _, p := range parts {
			p = strings.TrimSpace(p)
			idx, err := strconv.Atoi(p)
			if err != nil || idx < 1 || idx > len(q.Options) {
				allNumeric = false
				break
			}
			labels = append(labels, q.Options[idx-1].Label)
		}
		if allNumeric && len(labels) > 0 {
			return strings.Join(labels, ", ")
		}
	} else {
		if idx, err := strconv.Atoi(input); err == nil && idx >= 1 && idx <= len(q.Options) {
			return q.Options[idx-1].Label
		}
	}

	return input
}

// 给AskUserQuestion control_response 构建一个updateInput
func buildAskQuestionResponse(originalInput map[string]any, questions []types.UserQuestion, collected map[int]string) map[string]any {
	result := make(map[string]any)
	for k, v := range originalInput {
		result[k] = v
	}
	answers := make(map[string]any)
	for idx, ans := range collected {
		answers[strconv.Itoa(idx)] = ans
	}
	result["answers"] = answers
	return result
}

func isApproveAllResponse(s string) bool {
	for _, w := range []string{
		"allow all", "allowall", "approve all", "yes all",
		"允许所有", "允许全部", "全部允许", "所有允许", "都允许", "全部同意",
	} {
		if s == w {
			return true
		}
	}
	return false
}

func isAllowResponse(s string) bool {
	for _, w := range []string{"allow", "yes", "y", "ok", "允许", "同意", "可以", "好", "好的", "是", "确认", "approve"} {
		if s == w {
			return true
		}
	}
	return false
}

func isDenyResponse(s string) bool {
	for _, w := range []string{"deny", "no", "n", "reject", "拒绝", "不允许", "不行", "不", "否", "取消", "cancel"} {
		if s == w {
			return true
		}
	}
	return false
}

func splitMessage(text string, maxLen int) []string {
	runes := []rune(text)
	if len(runes) <= maxLen {
		return []string{text}
	}
	var chunks []string

	for len(runes) > 0 {
		if len(runes) <= maxLen {
			chunks = append(chunks, string(runes))
			break
		}

		end := maxLen

		// 尝试在rune 窗口以换行符为边界进行拆分
		// 将候选块转换回字符串，以便进行换行符搜索。
		candidate := string(runes[:end])
		if idx := strings.LastIndex(candidate, "\n"); idx > 0 {
			// idx is a byte offset within candidate; convert to rune offset.
			runeIdx := utf8.RuneCountInString(candidate[:idx])
			if runeIdx >= end/2 {
				end = runeIdx + 1
			}
		}

		chunks = append(chunks, string(runes[:end]))
		runes = runes[end:]
	}
	return chunks
}

// truncateIf 函数将字符串 s 截断至最多 maxLen 个字符。0 表示不截断。
func truncateIf(s string, maxLen int) string {
	if maxLen <= 0 {
		return s
	}
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}
	return string([]rune(s)[:maxLen]) + "..."
}

// ======================= 压缩相关 =======================

// 正则匹配类似 "[ctx: ~42%]
var ctxSelfReportRe = regexp.MustCompile(`(?m)\n?\[ctx: ~\d+%\]`)

// 从self-reported "[ctx: ~XX%]" 行 提取百分比
func parseSelfReportedCtx(s string) int {
	m := ctxSelfReportRe.FindString(s)
	if m == "" {
		return 0
	}
	start := strings.Index(m, "~") + 1
	end := strings.Index(m, "%")
	if start <= 0 || end <= start {
		return 0
	}
	v, _ := strconv.Atoi(m[start:end])
	return v
}

// 类似estimateTokens但是包括一个还没有写入history的assistant信息(AddHistory之前在EventResult使用)
func estimateTokensWithPendingAssistant(entries []types.HistoryEntry, pendingAssistant string) int {
	// Heuristic: ~1 token per 4 characters in mixed English/Chinese.
	count := 0
	for _, h := range entries {
		count += len([]rune(h.Content))
	}
	if pendingAssistant != "" {
		count += len([]rune(pendingAssistant))
	}
	if count == 0 {
		return 0
	}
	return (count + 3) / 4
}

const modelContextWindow = 200_000 // Claude 上下文窗口, Opencode - 65535??

// 介于SDK-reported 输出token 返回一个类似于 \n[ctx ~42%]的后缀
func contextIndicator(inputTokens int) string {
	if inputTokens <= 0 {
		return ""
	}
	pct := inputTokens * 100 / modelContextWindow
	pct = min(pct, 100)
	return fmt.Sprintf("\n[ctx: ~%d%%]", pct)
}
