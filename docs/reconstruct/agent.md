# 阶段二：Agent 交互层迁移

## 目标

将 `engine.go` 中与 **Agent 运行时交互** 相关的逻辑迁移到 `agent/` 包中，包括事件循环、命令执行、权限管理等。参考 `docs/reconstruct/RECONSTRUCT2.md` 中的"Agent 交互层"定义。

**核心设计原则**：
- **整体迁移**：先整体移动 `processInteractiveEvents` 及相关方法到 `agent/executor.go`，后续再考虑内部优化
- **依赖注入**：通过 `ExecutorContext` 接口注入重型依赖（避免循环依赖）
- **参考模式**：类似 `core/command/types.go` 中的 `CommandContext` 和阶段一的 `StateContext`

**目标**：将 `engine.go` 减少约 **400-500 行**（配合阶段一总计减少 ~1200-1500 行，达到 ~2400 行目标）。

---

## 文件结构

```
tc-connect/
├── core/
│   ├── engine.go (重构后 ~2400 行)
│   ├── interactive/ (阶段一创建)
│   │   ├── types.go      # StateContext 接口
│   │   ├── state.go      # State, QueuedMessage 类型
│   │   └── manager.go    # Manager (生命周期管理)
│   └── agent/            # 新：Agent 交互层
│       └── executor.go   # Executor (事件循环 + 命令执行 + 权限管理)
```

**注意**：不创建 `eventloop.go`, `permission.go` 等拆分文件，而是先整体放入 `executor.go`，待稳定后再优化。

---

## 步骤 1：定义 ExecutorContext 接口

### 1.1 创建 `core/agent/executor.go`（或 `types.go`）

**设计目标**：定义重型依赖的契约，由 `Engine` 实现。参考 `core/command/types.go` 中的 `CommandContext`。

**接口定义**：
```go
// ExecutorContext Agent 交互上下文接口
// 由 Engine 实现，注入到 agent.Executor 中用于事件处理和命令执行
//
// 职责：
// 1. 提供平台通信能力（send, reply, sendForWorkspace 等）
// 2. 提供渲染能力（streamPreview, compactProgress 等）
// 3. 提供配置访问（i18n, displayCfg, eventIdleTimeout 等）
// 4. 提供命令执行能力（custom commands, shell commands, skills）
// 5. 提供会话管理能力（sessions, cleanup 等）
type ExecutorContext interface {
    // ... 方法定义 ...
}
```

**方法分类**：
- **平台通信**：`Send()`, `Reply()`, `SendForWorkspace()`, `SendWithErrorForWorkspace()`, `SendAskQuestionPrompt()`
- **渲染**：`NewStreamPreview()`, `NewCompactProgress()`（或类似方法，用于创建进度显示）
- **配置**：`I18n()`, `DisplayCfg()`, `EventIdleTimeout()`, `AutoCompressEnabled()`, `AutoCompressMaxTokens()`, `AutoCompressMinGap()`, `ShowContextIndicator()`, `AgentName()`
- **命令执行**：`ExecuteCustomCommand()`, `ExecuteShellCommand()`, `ExecuteSkill()`（如果这些方法需要独立出来）
- **会话管理**：`SessionManager()`（返回 `*session.SessionManager`）, `Cleanup()`（调用阶段一的 `interactive.Manager.Cleanup`）
- **其他**：`EstimateTokensWithPendingAssistant()`, `Context()`（返回 `context.Context`）

---

## 步骤 2：创建 Executor

### 2.1 创建 `core/agent/executor.go`

**提取内容**：
- `processInteractiveEvents()`（事件循环核心，line 1167-1660）
- `executeCustomCommand()`（自定义命令执行）
- `executeShellCommand()`（shell 命令执行）
- `executeSkill()`（技能执行）
- `handlePendingPermission()`（权限处理，如果独立存在）
- `resolveAskQuestionAnswer()`（用户提问回答处理，如果独立存在）

**结构**：
```go
type Executor struct {
    ctx ExecutorContext
}

func NewExecutor(ctx ExecutorContext) *Executor

// ProcessInteractiveEvents 事件循环入口
func (e *Executor) ProcessInteractiveEvents(
    state *interactive.State,
    sess *session.Session,
    sessionKey string,
    prompt string,
    startTime time.Time,
    stopTyping func(),
    sendDone chan error,
    replyCtx any,
)
```

**注意**：
- 先整体移动，保持方法签名和逻辑与 `engine.go` 中一致
- 使用 `e.ctx` 访问依赖，而不是直接字段访问
- 不急于拆分内部方法（如 `handleThinking`, `handleText` 等），待稳定后再优化

---

## 步骤 3：在 Engine 中实现 ExecutorContext 接口

### 3.1 修改 `core/engine.go`

**添加字段**：
```go
type Engine struct {
    // ... 现有字段 ...
    
    // Agent 交互执行器
    executor *agent.Executor
}
```

**实现接口方法**：
- 为 `ExecutorContext` 定义的每个方法添加包装器（类似阶段一 `StateContext` 的实现方式）
- 例如：
  ```go
  func (e *Engine) Send(p types.Platform, replyCtx any, content string) {
      e.send(p, replyCtx, content)
  }
  
  func (e *Engine) I18n() types.I18nProvider {
      return e.i18n
  }
  // ...
  ```

**初始化 Executor**（在 `NewEngine` 中）：
```go
func NewEngine(...) *Engine {
    e := &Engine{...}
    
    // 阶段一
    e.interactiveMgr = interactive.NewManager(e)
    
    // 阶段二
    e.executor = agent.NewExecutor(e)
    
    return e
}
```

---

## 步骤 4：更新调用链

### 4.1 修改 `interactive.Manager`

在 `core/interactive/manager.go` 的 `DrainPendingMessages` 方法中：

**原调用**（直接调用 `engine.processInteractiveEvents`）：
```go
// 伪代码
e.processInteractiveEvents(state, sess, sessions, sessionKey, prompt, ...)
```

**新调用**（通过 `ExecutorContext` 接口）：
```go
// 伪代码
m.provider.Executor().ProcessInteractiveEvents(state, sess, sessionKey, prompt, ...)
```

**需要在 `StateContext` 接口中添加**（`core/interactive/types.go`）：
```go
type StateContext interface {
    // ... 现有方法 ...
    
    // Agent 交互执行器
    Executor() *agent.Executor
}
```

---

## 步骤 5：从 Engine 中删除方法

从 `engine.go` 中删除以下方法（约 **400-500 行**）：
- `processInteractiveEvents()`（line 1167-1660，核心事件循环）
- `executeCustomCommand()`（如果存在）
- `executeShellCommand()`（如果存在）
- `executeSkill()`（如果存在）
- 其他辅助方法（如 `handlePendingPermission`, `resolveAskQuestionAnswer` 等）

**保留**：
- `streamPreview` 和 `compactProgressWriter` 的内部实现（如果它们依赖于 `engine.go` 特有的字段）
- 或者将它们也迁移到 `agent/` 包，通过 `ExecutorContext` 注入依赖

---

## 依赖关系

### 依赖图

```
core/types
    ↑
core/interactive (State, StateContext)
    ↑
core/agent (Executor, ExecutorContext)
    ↑
core (Engine 实现 ExecutorContext)
```

**关键点**：
- `core/agent` 只依赖 `core/types` 和 `core/interactive`（类型定义）
- `core/agent` **不依赖** `core`（具体实现）
- `Engine` 实现 `ExecutorContext` 接口，单向依赖

### 与现有模块的关系

| 模块 | 职责 | 依赖方向 |
|------|------|---------|
| `core/command` | 处理 `/` 命令（用户输入解析） | `command` → `core` |
| `core/interactive` | 会话生命周期管理（CRUD） | `interactive` → `core` |
| `core/agent` | Agent 运行时交互（事件循环） | `agent` → `core` |

**区别**：
- `command`：用户输入 → 命令解析 → 执行（同步）
- `interactive`：会话创建/销毁/队列管理（生命周期）
- `agent`：Agent 事件流处理 → 响应生成（异步事件循环）

---

## 注意事项

1. **不要拆分**：先整体移动 `processInteractiveEvents`，不要急于拆分成 `eventloop.go`, `permission.go` 等小文件
2. **命名**：使用 `ExecutorContext` 而不是 `RenderContext`，因为职责是执行（execution）而非渲染（rendering）
3. **依赖注入**：通过接口注入，不要直接字段注入（如 `ctx, agent, i18n` 三个独立参数）
4. **测试**：确保原有测试通过后再删除 `engine.go` 中的代码

---

## 验证清单

- [ ] `core/agent/executor.go` 创建完成
- [ ] `ExecutorContext` 接口定义完成
- [ ] `Engine` 实现 `ExecutorContext` 接口
- [ ] `NewEngine` 中初始化 `executor`
- [ ] `interactive.Manager` 通过 `ExecutorContext` 调用 `executor.ProcessInteractiveEvents`
- [ ] 删除 `engine.go` 中的 `processInteractiveEvents` 及相关方法
- [ ] `engine.go` 行数 < 2500
- [ ] 编译通过
- [ ] 原有测试通过

---

## 参考文件

- `/media/ubuntu/data/gitSourceCode/AIWorkflow/tc-connect/docs/reconstruct/RECONSTRUCT2.md`: Agent 交互层定义
- `/media/ubuntu/data/gitSourceCode/AIWorkflow/tc-connect/docs/reconstruct/state.md`: 阶段一（StateContext 模式）
- `/media/ubuntu/data/gitSourceCode/AIWorkflow/tc-connect/core/engine.go`: 源文件（processInteractiveEvents at line 1167）
- `/media/ubuntu/data/gitSourceCode/AIWorkflow/tc-connect/core/command/types.go`: CommandContext 模式参考