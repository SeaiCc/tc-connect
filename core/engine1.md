# Engine 架构

核心引擎组件，桥接 **Agent**（AI/LLM 执行）和 **Platform**（通信渠道如 Slack、Discord 等）。

**文件**: `engine.go` (~6,350 行)

---

## 核心类型

### Engine
主要编排器结构体，在平台和 agent 之间路由消息。

### interactiveState
跟踪运行的 agent 会话，包括：
- `agentSession` - 活跃的 agent 连接
- `platform` - 通信平台实例
- `pendingMessages` - 等待繁忙会话的消息队列
- `stopCh`/`stopped` - 优雅关闭信号
- `pending` - 待处理的权限请求
- `deleteMode` - 删除模式状态

### queuedMessage
会话繁忙时持有的消息结构（最多 5 条）。

### ConfigReloadResult
配置重载操作的结果。

---

## 功能域

### 1. 核心与生命周期
入口点和消息路由。

| 方法 | 描述 |
|--------|-------------|
| `Start()` | 初始化引擎并启动平台 |
| `handleMessage(p Platform, msg *Message)` | 平台消息的主要入口 |
| `handleCommand(p Platform, msg *Message, raw string) bool` | 路由命令消息 |
| `processInteractiveMessage()` | 处理交互式（非命令）消息 |
| `processInteractiveMessageWith()` | 带上下文的完整交互式处理 |

### 2. 会话管理
创建、跟踪和清理 agent 会话。

| 方法 | 描述 |
|--------|-------------|
| `getOrCreateInteractiveStateWith()` | 创建或获取会话状态 |
| `cleanupInteractiveState()` | 清理交互式状态 |
| `stopInteractiveSession()` | 停止特定会话 |
| `closeAgentSessionAsync()` | 异步关闭（带超时） |
| `closeAgentSessionWithTimeout()` | 同步关闭（带超时） |
| `maybeAutoResetSessionOnIdle()` | 空闲时长后自动重置 |
| `drainOrphanedQueue()` | 处理孤立的队列消息 |

### 3. 消息处理与排队
会话繁忙时的消息流控制。

| 方法 | 描述 |
|--------|-------------|
| `queueMessageForBusySession()` | 会话繁忙时排队消息 |
| `drainPendingMessages()` | 轮次结束后排空队列消息 |
| `drainQueuedMessagesAfterCompress()` | 压缩后排空队列 |
| `ensureInteractiveStateForQueueing()` | 确保队列状态存在 |
| `notifyDroppedQueuedMessage()` | 消息丢弃通知 |
| `notifyDroppedQueuedMessages()` | 批量通知 |

### 4. Agent 交互
执行命令和处理 agent 事件。

| 方法 | 描述 |
|--------|-------------|
| `processInteractiveEvents()` | agent 主事件循环 |
| `executeCustomCommand()` | 执行自定义命令 |
| `executeShellCommand()` | 执行 shell 命令 |
| `executeSkill()` | 执行技能 |
| `handlePendingPermission()` | 处理权限请求 |
| `commandWorkDir()` | 解析工作目录 |

### 5. 平台通信
向通信平台发送/接收消息。

| 方法 | 描述 |
|--------|-------------|
| `reply(p, ctx, content)` | 回复消息 |
| `replyWithError()` | 带错误格式化的回复 |
| `replyWithButtons()` | 带交互按钮的回复 |
| `send(p, ctx, content)` | 发送消息 |
| `sendWithError()` | 带错误处理的发送 |
| `sendWithCard()` | 发送卡片 UI |
| `replyWithCard()` | 用卡片 UI 回复 |
| `waitOutgoing(p Platform) error` | 等待出站消息 |
| `checkRateLimit(msg *Message) bool` | 限流检查 |
| `matchBannedWord(content string) string` | 违禁词过滤 |
| `buildSenderPrompt()` | 构建发送者提示 |
| `resolveAskQuestionAnswer()` | 解析用户问题回答 |

### 6. UI/渲染
基于卡片的命令和状态 UI。

| 方法 | 描述 |
|--------|-------------|
| `renderHelpCard()` | 帮助文档 |
| `renderModeCard()` | 权限模式 |
| `renderLangCard()` | 语言选择 |
| `renderListCard()` | 会话列表 |
| `renderStatusCard()` | 状态显示 |
| `renderHistoryCard()` | 历史视图 |
| `renderCommandsCard()` | 自定义命令 |
| `renderConfigCard()` | 配置 UI |
| `renderDeleteModeCard()` | 删除模式 UI |
| `renderModelCard()` | 模型选择 |
| `simpleCard()` | 基础卡片构建器 |
| `handleCardNav()` | 卡片导航 |
| `renderCardForPlatform()` | 平台特定渲染 |

### 7. 命令处理器
~50+ 方法处理 `/command` 调用。

#### 会话命令
- `cmdNew()` - 创建新会话
- `cmdList()` - 列出会话
- `cmdSwitch()` - 切换会话
- `cmdCurrent()` - 显示当前
- `cmdStatus()` - 显示状态
- `cmdHistory()` - 显示历史
- `cmdSearch()` - 搜索会话

#### 配置
- `cmdMode()` - 设置权限模式
- `cmdModel()` - 设置模型
- `cmdReasoning()` - 推理设置
- `cmdLang()` - 设置语言
- `cmdQuiet()` - 安静模式
- `cmdProvider()` - 提供商设置
- `cmdMemory()` - 记忆操作
- `cmdConfig()` - 查看配置
- `cmdConfigReload()` - 重载配置

#### 自定义
- `cmdCommands()` - 列出自定义命令
- `cmdCommandsAdd()` - 添加命令
- `cmdCommandsDel()` - 删除命令
- `cmdAlias()` - 管理别名
- `cmdSkills()` - 列出技能

#### 管理
- `cmdCron()` - Cron 任务管理
- `cmdDoctor()` - 诊断
- `cmdUpgrade()` - 升级检查
- `cmdRestart()` - 重启请求
- `cmdHarness()` - Harness 模式

#### 控制
- `cmdStop()` - 停止会话
- `cmdCompress()` - 压缩上下文
- `cmdDelete()` - 删除会话
- `cmdAllow()` - 允许权限
- `cmdUsage()` - 使用信息
- `cmdHelp()` - 帮助
- `cmdWhoami()` - 身份信患
- `cmdShell()` - Shell 执行
- `cmdDiff()` - Diff 显示
- `cmdShow()` - 显示文件
- `cmdDir()` - 目录列表
- `cmdWeb()` - 网络搜索

### 8. 压缩
上下文压缩以管理 token 使用。

| 方法 | 描述 |
|--------|-------------|
| `runCompress()` | 执行压缩 |
| `processCompressEvents()` | 处理压缩事件 |

### 9. 工具与辅助
支持函数。

| 方法 | 描述 |
|--------|-------------|
| `resolveAlias(content string) string` | 展开别名 |
| `setupMemoryFile()` | 设置记忆文件 |
| `showMemoryFile()` | 显示记忆文件 |
| `appendMemoryFile()` | 追加到记忆 |
| `diff2html()` | 转换为 HTML |
| `onPlatformReady()` | 平台就绪处理器 |
| `markPlatformReady()` | 标记平台就绪 |
| `initPlatformCapabilities()` | 初始化能力 |
| `applyLiveModeChange()` | 应用模式变更 |
| `isAdmin(userID string) bool` | 管理员检查 |
| `formatWhoamiText()` | 格式化身份文本 |
| `configItems()` | 获取配置项 |
| `cardBackButton()` | 返回按钮构建器 |
| `cardPrevButton()` | 上一页按钮构建器 |
| `cardNextButton()` | 下一页按钮构建器 |
| `modeUsageText()` | 模式使用文本 |

### 10. 删除模式
交互式会话删除。

| 方法 | 描述 |
|--------|-------------|
| `getDeleteModeState()` | 获取删除模式状态 |
| `executeDeleteModeAction()` | 执行删除操作 |
| `submitDeleteModeSelection()` | 提交选择 |
| `matchSession()` | 匹配查询会话 |
| `delteSingleSessionReply()` | 删除回复 |
| `deleteSessionDisplayName()` | 获取显示名称 |
| `deleteModeSelectionNames()` | 获取选择名称 |
| `renderDeleteModeSelectCard()` | 选择卡片 |
| `renderDeleteModeConfirmCard()` | 确认卡片 |
| `renderDeleteModeResultCard()` | 结果卡片 |

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
     ├── /command → handleCommand() → cmd*()
     └── 普通消息 → processInteractiveMessage()
                    ↓
        getOrCreateInteractiveStateWith()
                    ↓
        processInteractiveEvents()
                    ├── Agent 事件循环
                    ├── handlePendingPermission()
                    └── 平台回复
```

---

## 依赖
- `tc-connect/config` - 配置管理
- `tc-connect/core/harness` - Harness 接口
- 标准库：`context`, `sync`, `time`, `regexp`, `exec` 等
