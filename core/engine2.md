# Engine 架构 (v2)

核心引擎组件，桥接 **Agent**（AI/LLM 执行）和 **Platform**（通信渠道如 Slack、Discord 等）。

**文件**: `engine.go` (~3,400 行)

**说明**: `cmd*` 已迁移至 `command/handler.go`，`render*` 已迁移至 `renderer/renderer.go`。

---

## 核心类型

### Engine
主要编排器结构体，在平台和 agent 之间路由消息。协调配置、会话、命令执行、权限管理等。

### interactiveState
跟踪运行的 agent 会话：
- `agentSession` - 活跃的 agent 连接
- `pendingMessages` - 等待队列（最多 5 条）
- `pending` - 待处理权限请求
- `deleteMode` - 删除模式状态
- `approveAll` - 自动批准所有权限

### queuedMessage
会话繁忙时持有的消息结构。

### pendingPermission
等待用户返回的权限请求。

---

## 功能域

### 1. 核心与生命周期

| 方法 | 描述 |
|--------|-------------|
| `Start()` | 初始化引擎并启动平台 |
| `handleMessage()` | 平台消息入口 |
| `handleCommand()` | 路由命令消息（委托给 `cmdHandler`） |
| `processInteractiveMessage()` | 处理交互式消息 |
| `processInteractiveMessageWith()` | 带上下文的完整处理 |

### 2. 会话管理

| 方法 | 描述 |
|--------|-------------|
| `getOrCreateInteractiveStateWith()` | 创建或获取会话状态 |
| `CleanupInteractiveState()` | 清理会话状态 |
| `cleanupWithLock()` | 带锁清理（防竞争） |
| `StopInteractiveSession()` | 停止特定会话 |
| `closeAgentSessionAsync()` | 异步关闭（带超时） |
| `closeAgentSessionWithTimeout()` | 同步关闭（带超时） |
| `maybeAutoResetSessionOnIdle()` | 空闲后自动重置 |
| `drainOrphanedQueue()` | 处理孤立队列消息 |
| `SendToSessionWithAttachments()` | 发送消息到会话（带附件） |

### 3. 消息处理与排队

| 方法 | 描述 |
|--------|-------------|
| `queueMessageForBusySession()` | 会话繁忙时排队 |
| `drainPendingMessages()` | 轮次结束后排空队列 |
| `drainQueuedMessagesAfterCompress()` | 压缩后排空队列 |
| `ensureInteractiveStateForQueueing()` | 确保队列状态存在 |
| `notifyDroppedQueuedMessage()` | 消息丢弃通知 |
| `notifyDroppedQueuedMessages()` | 批量通知 |

### 4. Agent 交互与执行

| 方法 | 描述 |
|--------|-------------|
| `processInteractiveEvents()` | agent 主事件循环 |
| `executeCustomCommand()` | 执行自定义命令 |
| `executeShellCommand()` | 执行 shell 命令 |
| `executeSkill()` | 执行技能 |
| `handlePendingPermission()` | 处理权限请求 |
| `resolveAskQuestionAnswer()` | 解析用户问题回答 |

### 5. 平台通信

| 方法 | 描述 |
|--------|-------------|
| `reply()` | 回复消息（带速率限制） |
| `replyWithError()` | 带错误格式化的回复 |
| `send()` | 发送消息 |
| `sendWithError()` | 带错误处理的发送 |
| `sendAlreadyRenderedWithError()` | 发送已渲染内容 |
| `sendWithCard()` | 发送卡片 UI |
| `sendForWorkspace()` | 发送到工作区（带路径上下文） |
| `sendWithErrorForWorkspace()` | 带错误处理的工作区发送 |
| `sendAskQuestionPrompt()` | 发送 AskUserQuestion 提示 |
| `WaitOutgoing()` | 等待出站消息（频率限制） |
| `checkRateLimit()` | 限流检查 |
| `matchBannedWord()` | 违禁词过滤 |
| `buildSenderPrompt()` | 构建发送者提示 |
| `resolveAlias()` | 展开别名 |

### 6. 压缩

| 方法 | 描述 |
|--------|-------------|
| `runCompress()` | 执行压缩 |
| `processCompressEvents()` | 处理压缩事件 |

### 7. 删除模式

| 方法 | 描述 |
|--------|-------------|
| `EnterDeleteMode()` | 进入删除模式 |
| `GetDeleteModeState()` | 获取删除模式状态 |
| `executeDeleteModeAction()` | 执行删除操作 |
| `submitDeleteModeSelection()` | 提交选择 |
| `delteSingleSessionReply()` | 删除回复 |
| `delteSessionDisplayName()` | 获取显示名称 |

### 8. 定时任务 (Cron)

| 方法 | 描述 |
|--------|-------------|
| `ExecuteCronJob()` | 执行定时任务 |
| `executeCronShell()` | 执行 cron shell 命令 |

### 9. 工具与辅助

| 方法 | 描述 |
|--------|-------------|
| `markPlatformReady()` | 标记平台就绪 |
| `onPlatformReady()` | 平台就绪处理器 |
| `IsAdmin()` | 管理员检查 |
| `GetConfigItems()` | 获取配置项 |
| `GetAliases()`, `ListAliases()` | 获取别名副本 |
| `SaveAddAlias()`, `SaveDelAlias()` | 别名持久化 |
| `SaveDisplayCfg()` | 显示配置持久化 |
| `SaveCommand()`, `DelCommand()` | 命令持久化 |
| `ReloadConfig()` | 重载配置 |
| `Reply()` | 快捷回复（调用 reply） |

### 10. 配置注入 (Setter/Getter)

| 方法 | 描述 |
|--------|-------------|
| `SetShowContextIndicator()` | 控制 [ctx: ~N%] 显示 |
| `SetBaseWorkDir()` | 设置根路径 |
| `SetProjectStateStore()` | 设置运行状态存储 |
| `SetLanguageSaveFunc()` | 设置语言保存方法 |
| `SetPlatform()` | 设置平台 |
| `AddCommand()` | 注册自定义命令 |
| `SetCommandSaveAddFunc/DelFunc()` | 命令持久化回调 |
| `SetCronScheduler()` | 设置 cron 调度器 |
| `SetAdminFrom()` | 设置管理员白名单 |
| `SetConfigReloadFunc()` | 设置配置重载回调 |
| `SetDisplayConfig()` | 设置显示配置 |
| `SetAutoCompressConfig()` | 配置自动压缩 |
| `SetResetOnIdle()` | 配置空闲重置 |
| `SetInjectSender()` | 控制发送者注入 |
| `ClearCommands()` | 清除命令（按源） |
| `ClearAliases()` | 清除所有别名 |
| `AddAlias()` | 注册别名 |
| `SetUserRoles()` | 设置用户角色管理器 |
| `Agent()`, `Platform()`, `Sessions()`, `I18n()`, `Renderer()`, `CommandRegistry()`, `SkillRegistry()` | 外部组件访问入口 |

### 11. interactiveState 方法

| 方法 | 描述 |
|--------|-------------|
| `isStopped()` | 检查是否已停止 |
| `markStopped()` | 标记为关闭 |
| `stopSignal()` | 获取停止信号 channel |

---

## 关键常量

| 常量 | 值 | 用途 |
|----------|-------|---------|
| `maxPlatformMessageLen` | 4000 | 最大消息长度 |
| `maxQueuedMessages` | 5 | 每会话最大排队消息数 |
| `listPageSize` | 20 | 列表命令分页大小 |
| `slowPlatformSend` | 2s | 慢平台发送阈值 |
| `slowAgentStart` | 5s | 慢 agent 启动阈值 |
| `slowAgentClose` | 3s | 慢 agent 关闭阈值 |
| `slowAgentSend` | 2s | 慢 agent 发送阈值 |
| `slowAgentFirstEvent` | 15s | 慢首事件阈值 |

---

## 架构流程

```
平台消息
     ↓
handleMessage()
     ├── /command → handleCommand() → cmdHandler.Handle() [已迁移]
     └── 普通消息 → processInteractiveMessage()
                        ↓
                getOrCreateInteractiveStateWith()
                        ↓
                processInteractiveEvents()
                        ├── Agent 事件循环
                        ├── handlePendingPermission()
                        ├── 平台回复 (reply/send)
                        └── 自动压缩检查
```

---

**engine.go 当前职责**:
- 消息路由和编排
- 会话生命周期管理
- Agent 事件循环
- 权限管理
- 平台通信封装
- 配置注入和依赖管理
