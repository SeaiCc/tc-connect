# Engine 重构计划 (v2)

## 概述

`engine.go` 当前约 3400 行，包含消息路由、会话管理、Agent 交互、平台通信、压缩、删除模式等多个功能域。本计划旨在通过**横向切片**将核心职责解耦，提取独立模块，降低单文件复杂度。

---

## 重构目标

1. **单文件行数**: 将 `engine.go` 压缩至 **800-1000 行**（仅保留核心编排逻辑）
2. **单一职责**: 每个提取的模块负责一个清晰的功能域
3. **依赖方向**: 从 `engine.go` 向外辐射，避免循环依赖
4. **接口隔离**: 通过接口定义边界，便于测试和替换

---

## 模块划分方案

### 1. 会话管理层 (interactive Layer)
**新文件**: `interactive/types.go` 或 `interactive/state.go`

**职责**: 
- `State` 结构体定义
- 会话生命周期（创建、获取、清理、停止）
- 消息队列管理（`pendingMessages`）
- 会话状态机（`isStopped`, `markStopped`, `stopSignal`）

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

**依赖**:
- 依赖 `platform.Platform` 接口（用于发送消息）
- 依赖 `agent.Agent` 接口（用于关闭会话）
- 依赖 `config` 包（配置项）

---

### 2. Agent 交互层 (Agent Interaction Layer)
**新文件**: `agent/executor.go` 或 `agent/interaction.go`

**职责**:
- Agent 事件循环编排
- 自定义命令、shell 命令、技能执行
- 权限管理（`pendingPermission`）
- 用户问答处理

**提取方法**:
- `processInteractiveEvents()`
- `executeCustomCommand()`
- `executeShellCommand()`
- `executeSkill()`
- `handlePendingPermission()`
- `resolveAskQuestionAnswer()`

**依赖**:
- `command.Registry`（命令注册表）
- `skill.Registry`（技能注册表）
- `session.Manager`（会话状态）
- `platform.Platform`（用于回复）

---

### 3. 平台通信层 (Platform Communication Layer)
**新文件**: `platform/transport.go` 或 `platform/messenger.go`

**职责**:
- 消息发送封装（统一出口）
- 限流控制
- 违禁词过滤
- 发送者提示构建

**提取方法**:
- `reply()`, `replyWithError()`
- `send()`, `sendWithError()`, `sendAlreadyRenderedWithError()`
- `sendWithCard()`, `sendForWorkspace()`, `sendWithErrorForWorkspace()`
- `sendAskQuestionPrompt()`
- `WaitOutgoing()`
- `checkRateLimit()`
- `matchBannedWord()`
- `buildSenderPrompt()`
- `resolveAlias()`

**依赖**:
- `platform.Platform` 接口
- `config` 包（别名、违禁词、管理员配置）
- `renderer.Renderer`（可能用于卡片渲染）

---

### 4. 压缩模块 (Compression Module)
**新文件**: `compress/engine.go` 或 `compress/manager.go`

**职责**:
- 会话上下文压缩逻辑
- 压缩事件处理

**提取方法**:
- `runCompress()`
- `processCompressEvents()`

**依赖**:
- `agent.Agent` 接口
- `session.Manager`（获取会话状态）

---

### 5. 删除模式模块 (Delete Mode Module)
**新文件**: `deletemode/manager.go`

**职责**:
- 删除模式状态管理
- 删除操作执行

**提取方法**:
- `getOrCreateDeleteModeState()`
- `GetDeleteModeState()`
- `executeDeleteModeAction()`
- `submitDeleteModeSelection()`
- `delteSingleSessionReply()`
- `delteSessionDisplayName()`

**依赖**:
- `platform.Platform`（发送删除确认）
- `session.Manager`（获取会话信息）

---

### 6. 定时任务模块 (Cron Module)
**新文件**: `cron/executor.go`

**职责**:
- 定时任务执行

**提取方法**:
- `ExecuteCronJob()`
- `executeCronShell()`

**依赖**:
- `cron.Scheduler` 接口（已存在）
- `platform.Platform`（发送通知）

---

### 7. 配置与工具 (Config & Utilities)
**新文件**: `config/manager.go`（如果未完全分离）或保留在 `engine.go`

**职责**:
- Setter/Getter 方法（配置注入）
- 管理员检查、配置持久化

**保留在 engine.go 的方法**（作为门面）:
- `SetShowContextIndicator()`, `SetBaseWorkDir()`, `SetProjectStateStore()` 等配置注入
- `IsAdmin()`, `GetConfigItems()`
- `ReloadConfig()`
- 各种 `Set*` 和 `Get*` 方法（作为对外接口）

---

## 重构后的 engine.go 结构

重构后，`engine.go` 仅保留**核心编排器**职责：

```go
// engine.go (目标 ~800 行)

type Engine struct {
    // 配置字段
    cfg config.Config
    
    // 依赖注入（接口）
    platform     platform.Platform
    agentFactory agent.Factory      // 创建 agent 会话
    sessionMgr   session.Manager    // 会话管理
    messenger    platform.Messenger // 平台通信
    executor     agent.Executor     // Agent 交互执行
    compressor   compress.Manager   // 压缩
    deleteMgr    deletemode.Manager // 删除模式
    cronExec     cron.Executor      // 定时任务
    
    // 只读引用（已迁移至组件）
    // - 命令注册表、技能注册表、渲染器等通过组件访问
}

// 核心入口（保留）
func (e *Engine) Start() error
func (e *Engine) handleMessage()     // 消息入口
func (e *Engine) handleCommand()     // 委托给 command/handler.go
func (e *Engine) processInteractiveMessage() // 委托给 sessionMgr + executor

// 配置注入接口（保留作为门面）
func (e *Engine) SetPlatform(p platform.Platform)
func (e *Engine) SetAgentFactory(f agent.Factory)
// ... 其他 Set* 方法

// 只读访问器（保留）
func (e *Engine) Platform() platform.Platform
func (e *Engine) Agent() agent.Agent
// ...
```

---

## 依赖图

```
                    ┌──────────────┐
                    │  engine.go   │ (核心编排，~800 行)
                    └──────┬───────┘
                           │ 依赖注入
         ┌─────────────────┼──────────────────┐
         ▼                 ▼                  ▼
┌─────────────┐   ┌──────────────┐   ┌──────────────┐
│ session/    │   │ platform/    │   │ command/     │
│ manager.go  │   │ messenger.go │   │ handler.go   │ (已存在)
└──────┬──────┘   └──────┬───────┘   └──────────────┘
       │                 │
       ▼                 ▼
┌─────────────┐   ┌──────────────┐
│ agent/      │   │ renderer/    │ (已存在)
│ executor.go │   │ renderer.go  │
└──────┬──────┘   └──────────────┘
       │
       ▼
┌─────────────┐
│ skill/      │ (已存在)
│ registry.go │
└─────────────┘

其他模块：compress/, deletemode/, cron/ (依赖上述核心模块)
```

---

## 实施步骤

### Phase 1: 提取会话管理层 (Priority: High)
1. 创建 `session/manager.go`
2. 移动 `interactiveState` 及相关队列逻辑
3. 更新 `engine.go` 调用为 `e.sessionMgr.GetOrCreate(...)`
4. 编写单元测试覆盖会话生命周期

### Phase 2: 提取平台通信层 (Priority: High)
1. 创建 `platform/messenger.go`
2. 提取所有 `send*`, `reply*` 方法
3. 定义 `platform.Messenger` 接口
4. 更新 engine 和其他模块使用新接口

### Phase 3: 提取 Agent 交互层 (Priority: Medium)
1. 创建 `agent/executor.go`
2. 移动 `processInteractiveEvents()` 及执行相关方法
3. 确保与 `command/handler.go` 的边界清晰

### Phase 4: 提取辅助模块 (Priority: Low)
1. 创建 `compress/engine.go`
2. 创建 `deletemode/manager.go`
3. 创建 `cron/executor.go`

### Phase 5: 精简 engine.go
1. 移除已提取代码
2. 保留门面模式（Setter/Getter）
3. 确保核心编排逻辑清晰

---

## 注意事项

1. **不要移动**: `cmd*` 和 `render*` 已明确迁移至其他包，不要再移回
2. **接口优先**: 提取的模块应通过接口依赖，而非具体实现
3. **测试先行**: 每个提取的模块应有独立测试，确保行为不变
4. **并发安全**: `interactiveState` 的锁机制需保留（`sync.RWMutex`）
5. **配置依赖**: 配置项访问应通过 `config.Config` 接口，避免全局变量

---

## 验收标准

- [ ] `engine.go` 行数 < 1000 行
- [ ] 每个提取的模块有独立单元测试（覆盖率 > 80%）
- [ ] 无编译错误，原有测试全部通过
- [ ] `go mod tidy` 后无循环依赖
- [ ] 文档更新（README 或架构文档）

---

**版本**: v2  
**创建日期**: 2026-06-29
