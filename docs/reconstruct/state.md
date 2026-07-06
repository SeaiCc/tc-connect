# 阶段一：运行时状态管理层提取

## 目标

在 `core/interactive/` 包中创建**运行时状态管理**模块，提取 `engine.go` 中的 `interactiveState` 生命周期管理逻辑，同时通过依赖注入解耦重型业务逻辑。

**核心设计原则**：
- **分层架构**：`Manager`（轻量 CRUD）+ `StateContext`（重型业务）
- **职责分离**：`Manager` 管"生老病死"，`StateContext` 管"吃饭干活"
- **依赖注入**：通过 `StateContext` 接口注入重型依赖（发送、事件循环）
- **参考模式**：类似 `core/command/types.go` 中的 `CommandContext` 模式

目标：将 `engine.go` 减少约 **800-1000 行**。

---

## 文件结构

```
tc-connect/
├── core/
│   ├── engine.go (重构后 ~2400 行)
│   └── interactive/
│       ├── types.go      # 状态上下文接口 (StateContext)
│       ├── state.go      # 运行时状态类型 (State, QueuedMessage)
│       └── manager.go    # 状态管理器 (Manager)
```

**注意**：不修改 `core/session/` 包，该包专门管理持久化 `Session`。

---

## 步骤 1：定义状态上下文接口

### 1.1 创建 `core/interactive/types.go`

**设计目标**：定义重型业务逻辑的契约，由 `Engine` 实现。参考 `core/command/types.go` 中的 `CommandContext`。

**文件内容**：
```go
package interactive

import (
    "context"
    "time"
    "tc-connect/core/session"
    "tc-connect/core/types"
)

// StateContext 运行时状态上下文接口
// 由 Engine 实现，注入到 Manager 中用于队列消费和生命周期管理
//
// 设计原则：
// 1. 包含所有重型依赖（send, reply, processEvents 等）
// 2. 包含轻量依赖（ctx, agent, i18n 等）
// 3. 不包含生命周期管理（GetOrCreate, Cleanup 等）
// 4. 保持与 Engine 方法签名一致，便于实现
type StateContext interface {
    // ================== 轻量依赖（生命周期管理） ==================
    
    // 上下文
    Context() context.Context
    
    // Agent 启动新的 agent session
    Agent() types.Agent
    
    // 国际化
    I18n() types.I18nProvider
    
    // 配置
    BaseWorkDir() string
    
    // ================== 重型依赖（队列消费） ==================
    
    // 平台通信
    Send(p types.Platform, replyCtx any, content string)
    SendWithError(p types.Platform, replyCtx any, content string) error
    Reply(p types.Platform, replyCtx any, content string)
    
    // 消息构建
    BuildSenderPrompt(content, userID, msgPlatform, msgSessionKey string) string
    
    // 事件循环（重型）
    ProcessInteractiveEvents(
        state *State,
        sess *session.Session,
        sessions *session.SessionManager,
        sessionKey string,
        prompt string,
        startTime time.Time,
        stopTyping func(),
        sendDone chan error,
        replyCtx any,
    )
    
    // 语言检测
    DetectLanguage(content string)
    
    // 配置访问
    GetDisplayCfg() *types.DisplayCfg
    GetEventIdleTimeout() time.Duration
    IsAutoCompressEnabled() bool
    
    // 其他重型依赖
    SendForWorkspace(p types.Platform, replyCtx any, content, workspaceDir string)
    SendWithErrorForWorkspace(p types.Platform, replyCtx any, content, workspaceDir string) error
    SendAskQuestionPrompt(p types.Platform, replyCtx any, questions []types.UserQuestion, qIdx int)
    
    // 删除模式
    SubmitDeleteModeSelection(p types.Platform, replyCtx any, sess *session.Session, selection string, delState *types.DeleteModeState, i18n types.I18nProvider)
    ExecuteDeleteModeAction(action int, delState *types.DeleteModeState)
}
```

---

## 步骤 2：定义核心类型

### 2.1 创建 `core/interactive/state.go`

**提取内容**：
- `interactiveState` 重命名为 `State`
- `queuedMessage` 重命名为 `QueuedMessage`
- `pendingPermission`

**文件内容**：
```go
package interactive

import (
    "sync"
    "time"
    "tc-connect/core/types"
)

// QueuedMessage 会话繁忙时持有的消息结构
type QueuedMessage struct {
    replyCtx      any
    platform      types.Platform
    content       string
    images        []types.ImageAttachment
    files         []types.FileAttachment
    fromVoice     bool
    userID        string
    msgPlatform   string
    msgSessionKey string
}

// State 跟踪运行的 agent 会话（原 interactiveState）
type State struct {
    agentSession types.AgentSession
    platform     types.Platform
    replyCtx     any
    workspaceDir string

    mu      sync.Mutex
    stopCh  chan struct{}
    stopped bool

    pending         *PendingPermission
    pendingMessages []QueuedMessage
    approveAll      bool

    fromVoice bool
    sideText  string

    deleteMode *types.DeleteModeState

    lastAutoCompressAt     time.Time
    lastAutoCompressTokens int
}

// PendingPermission 等待用户返回的权限请求
type PendingPermission struct {
    RequestID       string
    ToolName        string
    ToolInput       map[string]any
    InputPreview    string
    Questions       []types.UserQuestion
    Answers         map[int]string
    CurrentQuestion int
    Resolved        chan struct{}
    resolveOnce     sync.Once
}

func (pp *PendingPermission) Resolve() {
    pp.resolveOnce.Do(func() { close(pp.Resolved) })
}

// IsStopped 检查是否已停止
func (s *State) IsStopped() bool {
    s.mu.Lock()
    defer s.mu.Unlock()
    return s.stopped
}

// MarkStopped 标记为关闭
func (s *State) MarkStopped() {
    s.mu.Lock()
    defer s.mu.Unlock()
    if s.stopped {
        return
    }
    s.stopped = true
    if s.stopCh == nil {
        s.stopCh = make(chan struct{})
    }
    close(s.stopCh)
}

// StopSignal 获取停止信号 channel
func (s *State) StopSignal() <-chan struct{} {
    s.mu.Lock()
    defer s.mu.Unlock()
    if s.stopCh == nil {
        s.stopCh = make(chan struct{})
        if s.stopped {
            close(s.stopCh)
        }
    }
    return s.stopCh
}
```

---

## 步骤 3：创建状态管理器

### 3.1 创建 `core/interactive/manager.go`

**设计目标**：轻量级 CRUD 管理器，只依赖 `StateContext` 接口。

**提取方法**:
- `getOrCreateInteractiveStateWith()`
- `CleanupInteractiveState()` / `cleanupWithLock()`
- `StopInteractiveSession()`
- `closeAgentSessionAsync()` / `closeAgentSessionWithTimeout()`
- `maybeAutoResetSessionOnIdle()`
- `drainOrphanedQueue()`
- `SendToSessionWithAttachments()`
- `queueMessageForBusySession()`
- `drainPendingMessages()` / `drainQueuedMessagesAfterCompress()`
- `ensureInteractiveStateForQueueing()`
- `notifyDroppedQueuedMessage()` / `notifyDroppedQueuedMessages()`

**文件内容**：
```go
package interactive

import (
    "context"
    "fmt"
    "log/slog"
    "sync"
    "time"
    "tc-connect/core/session"
    "tc-connect/core/types"
)

const MaxQueuedMessages = 5

// Manager 管理 State 生命周期（轻量级）
//
// 职责：
// 1. State 的 CRUD（Create, Read, Update, Delete）
// 2. 状态回收与防竞争检查
// 3. 队列排空（调用 StateContext 处理业务逻辑）
//
// 依赖：
// - StateContext: 通过接口注入（轻量 + 重型依赖）
type Manager struct {
    states   map[string]*State
    mu       sync.Mutex
    provider StateContext
}

// NewManager 创建状态管理器
func NewManager(provider StateContext) *Manager {
    return &Manager{
        states:   make(map[string]*State),
        provider: provider,
    }
}

// GetOrCreate 创建或获取状态（原 getOrCreateInteractiveStateWith）
//
// 职责：
// 1. 检查是否存在且存活
// 2. 验证 Session ID 匹配（防 stale session）
// 3. 回收不匹配的旧进程
// 4. 创建新状态
func (m *Manager) GetOrCreate(
    sessionKey string,
    p types.Platform,
    replyCtx any,
    agentSession types.AgentSession,
    sess *session.Session,
) *State {
    m.mu.Lock()
    defer m.mu.Unlock()

    state, ok := m.states[sessionKey]
    if ok && state.agentSession != nil && state.agentSession.Alive() {
        // 验证 Session ID 匹配
        wantID := sess.GetAgentSessionID()
        currentID := state.agentSession.CurrentSessionID()
        needRecycle := currentID != "" && (wantID == "" || wantID != currentID)
        
        if !needRecycle {
            return state
        }
        
        // 需要回收：销毁 stale agent
        slog.Info("interactive session mismatch, recycling",
            "session_key", sessionKey,
            "want_agent_session", wantID,
            "have_agent_session", currentID,
        )
        state.MarkStopped()
        m.closeAgentSessionWithTimeout(sessionKey, state.agentSession)
        delete(m.states, sessionKey)
        ok = false
    }

    // 创建新状态
    newState := &State{
        agentSession: agentSession,
        platform:     p,
        replyCtx:     replyCtx,
    }
    
    // 处理占位符迁移
    if oldState, exists := m.states[sessionKey]; exists && oldState != newState {
        m.adoptPendingFromPlaceholder(oldState, newState)
    }
    
    m.states[sessionKey] = newState
    return newState
}

// Cleanup 清理状态（原 cleanupWithLock）
func (m *Manager) Cleanup(sessionKey string, expected ...*State) {
    m.mu.Lock()
    state, ok := m.states[sessionKey]
    
    // 防竞争检查
    if len(expected) > 0 && expected[0] != nil && state != expected[0] {
        m.mu.Unlock()
        return
    }
    
    var agentSession types.AgentSession
    if ok && state != nil {
        agentSession = state.agentSession
    }
    m.mu.Unlock()

    // 标记停止
    if ok && state != nil {
        state.MarkStopped()
    }

    // 关闭 agent session（同步）
    if agentSession != nil {
        m.closeAgentSessionWithTimeout(sessionKey, agentSession)
    }

    // 从 map 删除（二次检查）
    m.mu.Lock()
    currentState, currentOk := m.states[sessionKey]
    if currentOk && len(expected) > 0 && expected[0] != nil && currentState != expected[0] {
        m.mu.Unlock()
        return
    }
    delete(m.states, sessionKey)
    m.mu.Unlock()
}

// Stop 停止状态（原 StopInteractiveSession）
func (m *Manager) Stop(sessionKey string) bool {
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
    
    // 异步关闭
    m.closeAgentSessionAsync(sessionKey, agentSession)
    return true
}

// QueueMessage 排队消息（原 queueMessageForBusySession）
func (m *Manager) QueueMessage(
    sessionKey string,
    p types.Platform,
    replyCtx any,
    content string,
    images []types.ImageAttachment,
    files []types.FileAttachment,
    fromVoice bool,
    userID string,
    msgPlatform string,
    msgSessionKey string,
) (bool, error) {
    m.mu.Lock()
    state, ok := m.states[sessionKey]
    m.mu.Unlock()
    
    if !ok || state == nil {
        return false, fmt.Errorf("session not found")
    }
    
    // 允许在 agentSession 为 nil 时排队（启动期间）
    if state.agentSession != nil && !state.agentSession.Alive() {
        return false, fmt.Errorf("session not alive")
    }

    state.mu.Lock()
    if len(state.pendingMessages) >= MaxQueuedMessages {
        state.mu.Unlock()
        return false, fmt.Errorf("queue full")
    }
    
    state.pendingMessages = append(state.pendingMessages, QueuedMessage{
        replyCtx:      replyCtx,
        platform:      p,
        content:       content,
        images:        images,
        files:         files,
        fromVoice:     fromVoice,
        userID:        userID,
        msgPlatform:   msgPlatform,
        msgSessionKey: msgSessionKey,
    })
    queueDepth := len(state.pendingMessages)
    state.mu.Unlock()

    slog.Info("message queued for busy session",
        "session", sessionKey,
        "queue_depth", queueDepth,
    )
    return true, nil
}

// DrainPendingMessages 排空队列（原 drainPendingMessages）
func (m *Manager) DrainPendingMessages(
    state *State,
    sess *session.Session,
    sessionKey string,
) bool {
    for {
        state.mu.Lock()
        if len(state.pendingMessages) == 0 {
            sess.Unlock()
            state.mu.Unlock()
            return true
        }
        queued := state.pendingMessages[0]
        state.pendingMessages = state.pendingMessages[1:]
        state.platform = queued.platform
        state.replyCtx = queued.replyCtx
        state.fromVoice = queued.fromVoice
        state.mu.Unlock()

        // 使用注入的 StateContext 处理业务逻辑
        m.provider.DetectLanguage(queued.content)
        prompt := m.provider.BuildSenderPrompt(
            queued.content, queued.userID, queued.msgPlatform, queued.msgSessionKey,
        )

        if state.agentSession == nil || !state.agentSession.Alive() {
            m.provider.Send(queued.platform, queued.replyCtx, 
                fmt.Sprintf(m.provider.I18n().T("error"), "agent session ended"))
            return false
        }

        // 清空旧事件
        drainEvents(state.agentSession.Events())
        
        // 启动事件循环（重型）
        sendDone := make(chan error, 1)
        m.provider.ProcessInteractiveEvents(
            state, sess, m.provider.Sessions(), sessionKey, prompt,
            time.Now(), func() {}, sendDone, queued.replyCtx,
        )
    }
}

// ================== 辅助方法 ==================

func (m *Manager) closeAgentSessionWithTimeout(sessionKey string, agentSession types.AgentSession) {
    timeoutCtx, cancel := context.WithTimeout(m.provider.Context(), 130*time.Second)
    defer cancel()
    agentSession.Close(timeoutCtx)
}

func (m *Manager) closeAgentSessionAsync(sessionKey string, agentSession types.AgentSession) {
    go func() {
        timeoutCtx, cancel := context.WithTimeout(m.provider.Context(), 130*time.Second)
        defer cancel()
        agentSession.Close(timeoutCtx)
    }()
}

func (m *Manager) adoptPendingFromPlaceholder(oldState, newState *State) {
    oldState.mu.Lock()
    newState.mu.Lock()
    newState.pendingMessages = oldState.pendingMessages
    oldState.pendingMessages = nil
    oldState.mu.Unlock()
    newState.mu.Unlock()
}

func drainEvents(events chan types.Event) {
    for {
        select {
        case <-events:
        default:
            return
        }
    }
}
```

**注意**：`StateContext` 接口需要补充 `Sessions()` 方法：
```go
type StateContext interface {
    // ...
    Sessions() *session.SessionManager
}
```

---

## 步骤 4：在 Engine 中实现 StateContext 接口

### 4.1 修改 `core/engine.go`

**添加字段**：
```go
type Engine struct {
    // ... 现有字段 ...
    
    // 运行时状态管理器
    interactiveMgr *interactive.Manager
}
```

**实现 StateContext 接口**：
```go
// ================== StateContext 接口实现 ==================

// Context
func (e *Engine) Context() context.Context {
    return e.ctx
}

// Agent
func (e *Engine) Agent() types.Agent {
    return e.agent
}

// I18n
func (e *Engine) I18n() types.I18nProvider {
    return e.i18n
}

// BaseWorkDir
func (e *Engine) BaseWorkDir() string {
    return e.baseWorkDir
}

// Sessions
func (e *Engine) Sessions() *session.SessionManager {
    return e.sessions
}

// Send
func (e *Engine) Send(p types.Platform, replyCtx any, content string) {
    e.send(p, replyCtx, content)
}

// SendWithError
func (e *Engine) SendWithError(p types.Platform, replyCtx any, content string) error {
    return e.sendWithError(p, replyCtx, content)
}

// Reply
func (e *Engine) Reply(p types.Platform, replyCtx any, content string) {
    e.reply(p, replyCtx, content)
}

// BuildSenderPrompt
func (e *Engine) BuildSenderPrompt(content, userID, msgPlatform, msgSessionKey string) string {
    return e.buildSenderPrompt(content, userID, msgPlatform, msgSessionKey)
}

// ProcessInteractiveEvents - 重型事件循环
func (e *Engine) ProcessInteractiveEvents(
    state *interactive.State,
    sess *session.Session,
    sessions *session.SessionManager,
    sessionKey string,
    prompt string,
    startTime time.Time,
    stopTyping func(),
    sendDone chan error,
    replyCtx any,
) {
    // 调用原有的 processInteractiveEvents（line 2539）
    e.processInteractiveEvents(state, sess, sessions, sessionKey, prompt, startTime, stopTyping, sendDone, replyCtx)
}

// DetectLanguage
func (e *Engine) DetectLanguage(content string) {
    e.i18n.DetectAndSet(content)
}

// GetDisplayCfg
func (e *Engine) GetDisplayCfg() *types.DisplayCfg {
    return e.displayCfg
}

// GetEventIdleTimeout
func (e *Engine) GetEventIdleTimeout() time.Duration {
    return e.eventIdleTimeout
}

// IsAutoCompressEnabled
func (e *Engine) IsAutoCompressEnabled() bool {
    return e.autoCompressEnabled
}

// SendForWorkspace
func (e *Engine) SendForWorkspace(p types.Platform, replyCtx any, content, workspaceDir string) {
    e.sendForWorkspace(p, replyCtx, content, workspaceDir)
}

// SendWithErrorForWorkspace
func (e *Engine) SendWithErrorForWorkspace(p types.Platform, replyCtx any, content, workspaceDir string) error {
    return e.sendWithErrorForWorkspace(p, replyCtx, content, workspaceDir)
}

// SendAskQuestionPrompt
func (e *Engine) SendAskQuestionPrompt(p types.Platform, replyCtx any, questions []types.UserQuestion, qIdx int) {
    e.sendAskQuestionPrompt(p, replyCtx, questions, qIdx)
}

// SubmitDeleteModeSelection
func (e *Engine) SubmitDeleteModeSelection(p types.Platform, replyCtx any, sess *session.Session, selection string, delState *types.DeleteModeState, i18n types.I18nProvider) {
    e.submitDeleteModeSelection(p, replyCtx, sess, selection, delState, i18n)
}

// ExecuteDeleteModeAction
func (e *Engine) ExecuteDeleteModeAction(action int, delState *types.DeleteModeState) {
    e.executeDeleteModeAction(action, delState)
}
```

### 4.2 初始化 Manager

**修改 `NewEngine`**（line 3227）：
```go
func NewEngine(ctx context.Context, agent types.Agent, i18n types.I18nProvider, baseWorkDir string, ...) *Engine {
    e := &Engine{
        ctx:         ctx,
        agent:       agent,
        i18n:        i18n,
        baseWorkDir: baseWorkDir,
        // ... 其他初始化 ...
    }
    
    // 创建交互状态管理器，注入 Engine 自身作为 StateContext
    e.interactiveMgr = interactive.NewManager(e)
    
    return e
}
```

---

## 步骤 5：从 Engine 中删除方法

### 5.1 删除的方法列表

从 `engine.go` 中删除以下方法（共约 **800-1000 行**）：

| 方法 | 行数 | 说明 |
|------|------|------|
| `getOrCreateInteractiveStateWith` | ~50 | line 1208 |
| `queueMessageForBusySession` | ~40 | line 600 |
| `drainPendingMessages` | ~60 | line 629 |
| `cleanupWithLock` | ~50 | line 2462 |
| `StopInteractiveSession` | ~40 | line 2470 |
| `closeAgentSessionWithTimeout` | ~35 | line 2462 |
| `closeAgentSessionAsync` | ~20 | line 2496 |
| `adoptPendingFromPlaceholder` | ~30 | - |
| `drainEvents` | ~15 | - |

### 5.2 替换调用

**在 `handleMessage` 中**（line 629）：
```go
// 原代码
e.queueMessageForBusySession(sessionKey, p, replyCtx, content, images, files, fromVoice, userID, msgPlatform, msgSessionKey)

// 替换为
_, _ = e.interactiveMgr.QueueMessage(sessionKey, p, replyCtx, content, images, files, fromVoice, userID, msgPlatform, msgSessionKey)
```

**在 `handleMessage` 中**（line 1208）：
```go
// 原代码
state := e.getOrCreateInteractiveStateWith(ctx, sessionKey, p, replyCtx, sess, ...)

// 替换为
state := e.interactiveMgr.GetOrCreate(sessionKey, p, replyCtx, sess, agentSession)
```

**在 `handleMessage` 中**（line 629）：
```go
// 原代码
e.drainPendingMessages(state, sess, sessionKey, ...)

// 替换为
e.interactiveMgr.DrainPendingMessages(state, sess, sessionKey)
```

**在 `handleDeleteModeAction` 中**（line 2644）：
```go
// 原代码
e.cleanupWithLock(sessionKey, &state)

// 替换为
e.interactiveMgr.Cleanup(sessionKey, &state)
```

**在 `handleStopSession` 中**（line 2689）：
```go
// 原代码
e.StopInteractiveSession(sessionKey)

// 替换为
e.interactiveMgr.Stop(sessionKey)
```

---

## 步骤 6：处理特殊依赖

### 6.1 `maybeAutoResetSessionOnIdle`

**问题**：该方法依赖 `e.resetOnIdle`, `e.reply`, `e.cleanupWithLock`。

**方案**：保留在 `engine.go`，但调用 `interactiveMgr` 的方法：
```go
func (e *Engine) maybeAutoResetSessionOnIdle(sessionKey string, state *interactive.State, sess *session.Session) {
    if !e.resetOnIdle {
        return
    }
    
    // ... 逻辑 ...
    
    // 调用 Manager 的 Cleanup
    e.interactiveMgr.Cleanup(sessionKey, &state)
    
    // 调用 Engine 的 Reply（保留）
    e.reply(p, replyCtx, message)
}
```

### 6.2 `executeDeleteModeAction`

**问题**：该方法依赖 `e.i18n.T`, `e.submitDeleteModeSelection`, `e.sessions`。

**方案**：通过 `StateContext` 接口注入（已在步骤 4 中实现）：
```go
// 在 StateContext 接口中
ExecuteDeleteModeAction(action int, delState *types.DeleteModeState)

// 在 Engine 中实现
func (e *Engine) ExecuteDeleteModeAction(action int, delState *types.DeleteModeState) {
    e.executeDeleteModeAction(action, delState)
}

// 在 Manager 中调用（如果需要）
func (m *Manager) ExecuteDeleteModeAction(state *State, action int) {
    m.provider.ExecuteDeleteModeAction(action, state.DeleteMode)
}
```

---

## 为什么没有循环依赖？

### 依赖图

```
core/types (定义 Agent, I18nProvider, Platform 等接口)
    ↑
    | (导入)
core/interactive (定义 StateContext 接口，使用 core/types 中的类型)
    ↑
    | (导入)
core (Engine 实现 StateContext 接口，注入到 Manager)
```

**关键点**：
1. `core/interactive` 只导入 `core/types`（类型定义），**不导入 `core`**（具体实现）
2. `core` 导入 `core/interactive`
3. `Engine` 实现 `StateContext` 接口（单向依赖）

### 对比 `core/command`

| 包 | 接口定义 | 接口实现 | 依赖方向 |
|----|---------|---------|---------|
| `core/command` | `CommandContext` | `Engine` | `command` → `core` |
| `core/interactive` | `StateContext` | `Engine` | `interactive` → `core` |

两者都遵循相同模式：
- 子包定义接口（只依赖 `core/types`）
- `core` 包实现接口
- 单向依赖，无循环

---

## 验证清单

- [ ] `core/interactive/types.go` 创建完成（StateContext 接口）
- [ ] `core/interactive/state.go` 创建完成（State, QueuedMessage 类型）
- [ ] `core/interactive/manager.go` 创建完成（Manager 实现）
- [ ] `Engine` 实现 `StateContext` 接口（~20 个方法）
- [ ] `NewEngine` 中注入 `e` 到 `interactiveMgr`
- [ ] 删除 `engine.go` 中的 9 个方法（~300 行）
- [ ] 替换所有调用点（handleMessage, handleDeleteModeAction, handleStopSession 等）
- [ ] `engine.go` 行数 < 2500
- [ ] 编译通过
- [ ] 单元测试通过

---

## 参考文件

- `/media/ubuntu/data/gitSourceCode/AIWorkflow/tc-connect/core/engine.go`: 源文件
- `/media/ubuntu/data/gitSourceCode/AIWorkflow/tc-connect/core/engine2.md`: 结构文档
- `/media/ubuntu/data/gitSourceCode/AIWorkflow/tc-connect/core/session/session.go`: 持久化 Session 管理（不修改）
- `/media/ubuntu/data/gitSourceCode/AIWorkflow/tc-connect/core/command/types.go`: CommandContext 模式参考
