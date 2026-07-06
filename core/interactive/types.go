package interactive

import (
	"context"
	"log/slog"
	"sync"
	"tc-connect/core/i18n"
	"tc-connect/core/platform"
	"tc-connect/core/session"
	"tc-connect/core/streaming"
	"tc-connect/core/types"
	"time"
)

// 慢-op阈值, 操作超出阈值会产生slog.Warn 以便快速找出瓶颈
const (
	slowAgentStart = 5 * time.Second // agent.StartSession
	slowAgentClose = 3 * time.Second // agentSession.Close
)

// Processor 重型业务处理器接口
// 由 Engine 实现，注入到 Manager 中用于队列消费
//
// 设计原则：
// 1. 包含所有重型依赖（send, reply, processEvents 等）
// 2. 不包含生命周期管理（GetOrCreate, Cleanup 等）
// 3. 保持与 Engine 方法签名一致，便于实现
type StateContext interface {
	// 轻量依赖（生命周期管理必需）
	Agent() types.Agent
	AutoCompressEnabled() bool
	AutoCompressMaxTokens() int
	AutoCompressMinGap() time.Duration
	BaseWorkDir() string
	Context() context.Context
	DisplayCfg() *types.DisplayCfg
	EventIdleTimeout() time.Duration
	I18n() *i18n.I18n
	InjectSender() bool
	Name() string
	Sessions() *session.SessionManager
	ShowContextIndicator() bool
	StreamPreviewCfg() streaming.StreamPreviewCfg

	// 平台通信
	Send(p types.Platform, replyCtx any, content string)
	Reply(replyCtx any, content string)

	// 其他依赖
	Renderer() types.Renderer
	MsgHandlder() *platform.MessageHandler

	// 自动压缩
	ResetOnIdle() time.Duration
}

type Manager struct {
	// 状态存储（核心）
	states map[string]*State
	mu     sync.Mutex

	// 重型依赖（队列消费）
	sCtx StateContext
}

func NewManager(sCtx StateContext) *Manager {
	return &Manager{
		sCtx:   sCtx,
		states: make(map[string]*State),
	}
}

// ================== 封装注入的Engine依赖 ==================
// 防止代码过长，封装一层
func (m *Manager) I18nT(key i18n.MsgKey) string {
	return m.sCtx.I18n().T(key)
}

// 防止代码过长，封装一层
func (m *Manager) I18nTf(key i18n.MsgKey, args ...interface{}) string {
	return m.sCtx.I18n().Tf(key, args...)
}

func (m *Manager) Renderer() types.Renderer {
	return m.sCtx.Renderer()
}

// ===================== 辅助方法 =====================

//	getOrCreateInteractiveStateWith 接收一个可选的agent agentOverride (multi-workspace mode)
//
// adoptPendingFromPlaceholder从当前的placeholder中复制排队消息 状态到新状态,这样当map entry被
// 替换时排队消息不会丢失.必须在interactiveMu下被调用
func adoptPendingFromPlaceholder(existing, newState *State) {
	if existing == nil || existing == newState {
		return
	}
	existing.mu.Lock()
	if len(existing.pendingMessages) > 0 {
		newState.pendingMessages = existing.pendingMessages
		existing.pendingMessages = nil
	}
	existing.mu.Unlock()
}

func closeAgentSessionAsync(sessionKey string, agentSession types.AgentSession) {
	if agentSession == nil {
		return
	}
	go closeAgentSessionWithTimeout(sessionKey, agentSession)
}

// 带有超时的异步关闭方法
func closeAgentSessionWithTimeout(sessionKey string, agentSession types.AgentSession) {
	if agentSession == nil {
		return
	}

	// 允许足够的时间用于agent自己优雅的关闭流程:
	// stdin close -> stop hook (claude-mem summary 等) -> SIGTERM -> SIGKILL
	// 130s覆盖了默认的120s优雅接卸 + 5s SIGTERM + 5s buffer. 等待提早结束
	// 如果进程立即退出 - 这是上限时间,而非典型事件
	const closeTimeout = 130 * time.Second
	closeStart := time.Now()

	slog.Debug("cleanupInteractiveState: closing agent session", "session", sessionKey)
	done := make(chan struct{})
	go func() {
		agentSession.Close()
		close(done)
	}()

	select {
	case <-done:
		if elapsed := time.Since(closeStart); elapsed >= slowAgentClose {
			slog.Warn("slow agent session close", "elapsed", elapsed, "session", sessionKey)
		}
	case <-time.After(closeTimeout):
		slog.Error("agent session close timeout, abandoning",
			"timeout", closeTimeout, "session", sessionKey)
	}
}
