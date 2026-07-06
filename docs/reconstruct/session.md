# 阶段一：运行时状态管理层提取

## 目标

在 `core/session/` 包中创建 **运行时状态管理** 模块，提取 `engine.go` 中的 `interactiveState` 生命周期管理逻辑。

**注意**: `core/session/session.go` 已存在 `SessionManager` 用于管理 **持久化对话** (`Session` 类)，本次提取的是 **运行时 Agent 会话状态** (`interactiveState`)，两者职责不同：
- `SessionManager` (已有): 管理 `Session` 对象，持久化到磁盘，存储对话历史
- `RuntimeState` (新建): 管理 `interactiveState` 对象，纯内存，存储 Agent 进程句柄、消息队列、权限状态等

目标：将 `engine.go` 减少约 **800-1000 行**。

---

## 文件结构

```
tc-connect/
├── core/
│   ├── engine.go (重构后 ~2400 行)
│   └── session/
│       ├── session.go    # 已有：持久化 Session 管理 (SessionManager)
│       ├── runtime.go    # 新建：运行时状态 (RuntimeState)
│       ├── manager.go    # 新建：运行时状态管理器 (RuntimeManager)
│       └── queue.go      # 新建：消息队列处理
```

---

## 步骤 1：创建运行时状态类型

### 1.1 创建 `core/session/runtime.go`

**提取内容**:
- `interactiveState` 重命名为 `RuntimeState`
- `queuedMessage` 结构体
- `pendingPermission` 结构体（或移至 `core/agent/`）
- 相关方法：`isStopped()`, `markStopped()`, `stopSignal()`

**文件内容**:
```go
package session

import (
    "sync"
    "time"
    "tc-connect/core/types"
)

// queuedMessage 会话繁忙时持有的消息结构
type queuedMessage struct {
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

// RuntimeState 跟踪运行的 agent 会话（原 interactiveState）
type RuntimeState struct {
    agentSession types.AgentSession
    platform     types.Platform
    replyCtx     any
    workspaceDir string

    mu      sync.Mutex
    stopCh  chan struct{}
    stopped bool

    pending         *PendingPermission
    pendingMessages []queuedMessage
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
func (s *RuntimeState) IsStopped() bool {
    s.mu.Lock()
    defer s.mu.Unlock()
    return s.stopped
}

// MarkStopped 标记为关闭
func (s *RuntimeState) MarkStopped() {
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
func (s *RuntimeState) StopSignal() <-chan struct{} {
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

## 步骤 2：创建运行时状态管理器

### 2.1 创建 `core/session/manager.go`

**提取内容**:
- `RuntimeManager` 结构体（避免与 `session.SessionManager` 命名冲突）
- 运行时状态 CRUD 方法
- 清理方法

**文件内容**:
```go
package session

import (
    "context"
    "fmt"
    "log/slog"
    "sync"
    "time"
    "tc-connect/core/types"
)

// RuntimeManager 管理 RuntimeState 生命周期（原 interactiveState）
type RuntimeManager struct {
    interactiveMu     sync.Mutex
    interactiveStates map[string]*RuntimeState
    
    agent       types.Agent
    i18n        types.I18nProvider
    baseWorkDir string
}

// NewRuntimeManager 创建运行时状态管理器
func NewRuntimeManager(agent types.Agent, i18n types.I18nProvider, baseWorkDir string) *RuntimeManager {
    return &RuntimeManager{
        interactiveStates: make(map[string]*RuntimeState),
        agent:             agent,
        i18n:              i18n,
        baseWorkDir:       baseWorkDir,
    }
}

// GetOrCreate 创建或获取运行时状态（对应 engine.go:getOrCreateInteractiveStateWith）
func (m *RuntimeManager) GetOrCreate(
    sessionKey string,
    p types.Platform,
    replyCtx any,
    agentSession types.AgentSession,
    sess *Session,  // 注意：这里是 core/session.Session
) *RuntimeState {
    m.interactiveMu.Lock()
    defer m.interactiveMu.Unlock()

    state, ok := m.interactiveStates[sessionKey]
    if ok && state.agentSession != nil && state.agentSession.Alive() {
        // 验证 session ID 匹配逻辑（从 engine.go 复制）
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
        delete(m.interactiveStates, sessionKey)
        ok = false
    }

    // 创建新状态
    newState := &RuntimeState{
        agentSession: agentSession,
        platform:     p,
        replyCtx:     replyCtx,
    }
    
    // 处理占位符迁移
    if oldState, exists := m.interactiveStates[sessionKey]; exists && oldState != newState {
        m.adoptPendingFromPlaceholder(oldState, newState)
    }
    
    m.interactiveStates[sessionKey] = newState
    return newState
}

// Cleanup 清理运行时状态（对应 engine.go:CleanupInteractiveState, cleanupWithLock）
func (m *RuntimeManager) Cleanup(sessionKey string, expected ...*RuntimeState) {
    m.interactiveMu.Lock()
    state, ok := m.interactiveStates[sessionKey]
    
    // 检查预期状态（防竞争）
    if len(expected) > 0 && expected[0] != nil && state != expected[0] {
        m.interactiveMu.Unlock()
        return
    }
    
    var agentSession types.AgentSession
    if ok && state != nil {
        agentSession = state.agentSession
    }
    m.interactiveMu.Unlock()

    // 标记停止
    if ok && state != nil {
        state.MarkStopped()
        // TODO: 通知丢弃的消息（需要 Messenger 依赖）
    }

    // 关闭 agent session
    if agentSession != nil {
        m.closeAgentSessionWithTimeout(sessionKey, agentSession)
    }

    // 从 map 删除（二次检查）
    m.interactiveMu.Lock()
    currentState, currentOk := m.interactiveStates[sessionKey]
    if currentOk && len(expected) > 0 && expected[0] != nil && currentState != expected[0] {
        m.interactiveMu.Unlock()
        return
    }
    delete(m.interactiveStates, sessionKey)
    m.interactiveMu.Unlock()
}

// Stop 停止运行时状态（对应 engine.go:StopInteractiveSession）
func (m *RuntimeManager) Stop(sessionKey string) bool {
    m.interactiveMu.Lock()
    state, ok := m.interactiveStates[sessionKey]
    if !ok || state == nil {
        m.interactiveMu.Unlock()
        return false
    }

    state.mu.Lock()
    pending := state.pending
    state.pending = nil
    agentSession := state.agentSession
    state.mu.Unlock()

    state.MarkStopped()
    delete(m.interactiveStates, sessionKey)
    m.interactiveMu.Unlock()

    if pending != nil {
        pending.Resolve()
    }
    
    // TODO: 通知丢弃的消息
    m.closeAgentSessionAsync(sessionKey, agentSession)
    return true
}

// Get 获取运行时状态（只读）
func (m *RuntimeManager) Get(sessionKey string) (*RuntimeState, bool) {
    m.interactiveMu.Lock()
    defer m.interactiveMu.Unlock()
    state, ok := m.interactiveStates[sessionKey]
    return state, ok
}

// ListKeys 列出所有 session key
func (m *RuntimeManager) ListKeys() []string {
    m.interactiveMu.Lock()
    defer m.interactiveMu.Unlock()
    keys := make([]string, 0, len(m.interactiveStates))
    for k := range m.interactiveStates {
        keys = append(keys, k)
    }
    return keys
}

// 辅助方法

func (m *RuntimeManager) adoptPendingFromPlaceholder(existing, newState *RuntimeState) {
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

func (m *RuntimeManager) closeAgentSessionAsync(sessionKey string, agentSession types.AgentSession) {
    go func() {
        ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
        defer cancel()
        // 异步关闭逻辑
    }()
}

func (m *RuntimeManager) closeAgentSessionWithTimeout(sessionKey string, agentSession types.AgentSession) {
    // 同步关闭逻辑，超时 3 秒
    // 具体实现参考 engine.go:2470
}
```

---

## 步骤 3：提取消息队列处理

### 3.1 创建 `core/session/queue.go`

**提取内容**:
- 消息排队
- 队列消费
- 孤儿队列处理

**文件内容**:
```go
package session

import (
    "fmt"
    "log/slog"
    "tc-connect/core/types"
)

const MaxQueuedMessages = 5

// QueueManager 管理消息队列
type QueueManager struct {
    i18n      types.I18nProvider
    messenger types.Messenger // 用于发送通知
}

// NewQueueManager 创建队列管理器
func NewQueueManager(i18n types.I18nProvider, messenger types.Messenger) *QueueManager {
    return &QueueManager{
        i18n:      i18n,
        messenger: messenger,
    }
}

// QueueMessage 排队消息（对应 engine.go:2689）
func (q *QueueManager) QueueMessage(
    state *RuntimeState,
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
    state.mu.Lock()
    defer state.mu.Unlock()

    if len(state.pendingMessages) >= MaxQueuedMessages {
        return false, fmt.Errorf("queue full")
    }

    state.pendingMessages = append(state.pendingMessages, queuedMessage{
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
    slog.Info("message queued for busy session",
        "queue_depth", queueDepth,
    )
    
    // TODO: 发送排队通知（需要 messenger）
    return true, nil
}

// DrainPendingMessages 排空队列（对应 engine.go:2644）
func (q *QueueManager) DrainPendingMessages(
    state *RuntimeState,
    sess *Session,  // core/session.Session
    sessionKey string,
) bool {
    // 实现 drain 逻辑（从 engine.go:2644 复制）
    // ...
    return true
}

// NotifyDropped 通知丢弃的消息（对应 engine.go:2593）
func (q *QueueManager) NotifyDropped(state *RuntimeState, reason error) {
    state.mu.Lock()
    remaining := state.pendingMessages
    state.pendingMessages = nil
    state.mu.Unlock()

    for _, qm := range remaining {
        // TODO: 发送错误通知
        q.messenger.SendWithError(qm.platform, qm.replyCtx, 
            fmt.Sprintf(q.i18n.T("error"), reason))
    }
}
```

### 3.2 将 EnsurePlaceholder 移至 RuntimeManager

在 `manager.go` 中添加：

```go
// EnsurePlaceholder 确保占位符存在（对应 engine.go:2762）
func (m *RuntimeManager) EnsurePlaceholder(key string, p types.Platform, replyCtx any) {
    m.interactiveMu.Lock()
    defer m.interactiveMu.Unlock()
    if _, ok := m.interactiveStates[key]; !ok {
        m.interactiveStates[key] = &RuntimeState{
            platform: p,
            replyCtx: replyCtx,
        }
    }
}
```

---

## 步骤 4：修改 engine.go

### 4.1 删除已提取的代码

在 `engine.go` 中：

1. **删除类型定义**:
   - 删除 `queuedMessage` 结构体（行 56-66）
   - 删除 `interactiveState` 结构体（行 69-90）
   - 删除 `pendingPermission` 结构体（行 126-137，或移至 `core/agent/`）

2. **删除方法**:
   ```go
   // 删除以下方法（共约 20 个）
   - CleanupInteractiveState (568)
   - StopInteractiveSession (600)
   - cleanupWithLock (629)
   - getOrCreateInteractiveStateWith (1208)
   - closeAgentSessionAsync (2462)
   - closeAgentSessionWithTimeout (2470)
   - queueMessageForBusySession (2689)
   - drainPendingMessages (2644)
   - drainQueuedMessagesAfterCompress (2940)
   - ensureInteractiveStateForQueueing (2762)
   - notifyDroppedQueuedMessage (2593)
   - notifyDroppedQueuedMessages (2632)
   - drainOrphanedQueue (2738)
   - adoptPendingFromPlaceholder (3227)
   - interactiveState 的 isStopped/markStopped/stopSignal (92-124)
   - pendingPermission 的 resolve (139-141)
   ```

3. **保留并修改**:
   - `EnterDeleteMode` (574) - 暂时保留，后续移至 deletemode 模块
   - `maybeAutoResetSessionOnIdle` (2539) - 暂时保留（依赖 RuntimeState）
   - `SendToSessionWithAttachments` (683) - 暂时保留

### 4.2 添加依赖注入

在 `Engine` 结构体中添加：

```go
type Engine struct {
    // ... 现有字段 ...
    
    // 会话管理（已有）
    sessions *session.SessionManager  // 管理持久化 Session
    
    // 运行时状态管理（新增）
    runtimeMgr *session.RuntimeManager  // 管理 RuntimeState
    queueMgr   *session.QueueManager    // 管理消息队列
}
```

在 `NewEngine` 或初始化函数中：

```go
func NewEngine(cfg Config) *Engine {
    e := &Engine{
        // ... 现有初始化 ...
        sessions:   session.NewSessionManager(storePath),  // 已有
        runtimeMgr: session.NewRuntimeManager(agent, i18n, baseWorkDir),  // 新增
        queueMgr:   session.NewQueueManager(i18n, messenger),              // 新增
    }
    return e
}
```

### 4.3 替换调用

**示例 1**: `handleMessage` 中的调用替换

原代码:
```go
state := e.getOrCreateInteractiveStateWith(sessionKey, p, replyCtx, sess, sessions, agentName, tcSessionKey)
```

新代码:
```go
// 先启动 agent session（这部分逻辑保留在 engine.go，处理 resume/fallback）
agentSession, err := e.agent.StartSession(e.ctx, startSessionID, agentName)
if err != nil {
    // 错误处理，可能 fallback 到 fresh session
}

// 然后创建/获取运行时状态
state := e.runtimeMgr.GetOrCreate(sessionKey, p, replyCtx, agentSession, sess)
```

**示例 2**: 清理调用替换

原代码:
```go
e.cleanupWithLock(sessionKey, expected)
```

新代码:
```go
e.runtimeMgr.Cleanup(sessionKey, expected)
```

**示例 3**: 排队调用替换

原代码:
```go
if e.queueMessageForBusySession(p, msg, interactiveKey) {
```

新代码:
```go
state, ok := e.runtimeMgr.Get(interactiveKey)
if !ok {
    return false
}
if queued, err := e.queueMgr.QueueMessage(state, p, msg.ReplyCtx, msg.Content, ...); !queued {
    // 处理队列满的情况
}
```

**示例 4**: 占位符调用替换

原代码:
```go
e.ensureInteractiveStateForQueueing(key, p, replyCtx)
```

新代码:
```go
e.runtimeMgr.EnsurePlaceholder(key, p, replyCtx)
```

### 4.4 重命名 interactiveState 为 RuntimeState

所有原 `interactiveState` 类型的使用替换为 `session.RuntimeState`：

```go
// 原 engine.go
var interactiveStates map[string]*interactiveState

// 新 engine.go（如果还有残留引用）
// 实际上已移至 e.runtimeMgr
```

---

## 步骤 5：处理依赖循环

### 5.1 定义接口

在 `core/types/` 中定义必要接口，避免循环依赖：

```go
// core/types/interfaces.go

type I18nProvider interface {
    T(key string) string
    Tf(key string, args ...any) string
    DetectAndSet(content string)
}

type Messenger interface {
    SendWithError(p Platform, replyCtx any, content string) error
    Send(p Platform, replyCtx any, content string)
}
```

### 5.2 调整导入

确保 `core/session/runtime.go`, `manager.go`, `queue.go` 只依赖：
- `core/types`
- `log/slog`
- 标准库

**注意**: `manager.go` 会导入同包的 `session.Session`（持久化会话类），这是允许的（同包导入）。

---

## 步骤 6：编写测试

### 6.1 运行时状态管理器测试

```go
// core/session/manager_test.go
package session

import (
    "testing"
    "time"
    "tc-connect/core/types"
    "github.com/stretchr/testify/assert"
)

func TestRuntimeManager_GetOrCreate(t *testing.T) {
    // 测试创建新运行时状态
    // 测试获取已存在状态
    // 测试 session ID 不匹配时的回收
}

func TestRuntimeManager_Cleanup(t *testing.T) {
    // 测试清理逻辑
    // 测试防竞争逻辑（expected 参数）
}

func TestRuntimeManager_Stop(t *testing.T) {
    // 测试停止运行时状态
    // 测试权限请求解析
}
```

### 6.2 队列管理器测试

```go
// core/session/queue_test.go
package session

func TestQueueManager_QueueMessage(t *testing.T) {
    // 测试正常排队
    // 测试队列满（MaxQueuedMessages）
}

func TestQueueManager_DrainPendingMessages(t *testing.T) {
    // 测试排空队列
}
```

---

## 步骤 7：验证与清理

### 7.1 编译检查

```bash
cd /media/ubuntu/data/gitSourceCode/AIWorkflow/tc-connect
go build ./...
```

### 7.2 运行测试

```bash
go test ./core/session/... -v -race
go test ./core -v -run TestEngine  # 确保 engine.go 的测试仍通过
```

### 7.3 代码格式化

```bash
go fmt ./core/session/...
goimports -w ./core/session/...
```

---

## 注意事项

1. **命名区分**: 
   - `session.SessionManager`: 管理 **持久化** `Session` 对象（对话历史、磁盘存储）
   - `session.RuntimeManager`: 管理 **运行时** `RuntimeState` 对象（Agent 进程、内存队列）

2. **锁层级**: `interactiveMu`（map 锁）和 `state.mu`（状态锁）的层级关系必须保持

3. **防竞争**: `Cleanup` 的 `expected` 参数逻辑必须保留，防止 stale goroutine 误删

4. **孤儿队列**: `drainOrphanedQueue` 的异步处理逻辑需保留

5. **占位符**: `EnsurePlaceholder` 用于处理启动期间的消息队列（issue #565），不能丢失

6. **agentSession 关闭**: 必须确保在删除 map entry 之前关闭 agentSession，防止竞争

---

## 验收标准

- [ ] `core/session/runtime.go`, `manager.go`, `queue.go` 创建完成
- [ ] `engine.go` 减少 ~800-1000 行
- [ ] `session.SessionManager`（已有）功能不受影响
- [ ] 所有单元测试通过
- [ ] 无循环依赖
- [ ] `go vet` 无警告
