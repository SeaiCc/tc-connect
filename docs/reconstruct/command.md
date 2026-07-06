# 命令层分离重构指南 - 方案 A（接口抽象）

## 概述

本文档描述将 `tc-connect/core/engine.go` 中的命令处理逻辑分离到独立 `commands` 包的详细步骤，采用**接口抽象**方式。命令层包含所有 `cmd*` 方法，负责解析用户输入并执行相应业务逻辑。

### 目标结构

```
tc-connect/core/
├── i18n/                 # P0 已分离：独立国际化包
├── types/                # P0 已分离：Card 类型等基础类型
├── renderer/             # P1 已分离：渲染层
├── commands/             # P1 待分离：命令处理层
│   ├── types.go          # 命令上下文和辅助类型定义
│   └── command.go        # 所有 cmd* 方法（核心）
└── engine.go             # 移除命令代码，保留消息分发入口（switch-case）
```

**说明**：`commands` 包仅包含 `types.go` 和 `command.go` 两个文件，保持最小化。原有的 switch-case 路由逻辑保留在 `engine.go` 中。命令处理器采用对象封装风格（`Handler` 结构体），与 renderer 层保持一致。

**包依赖关系**（无循环）：
- `i18n`：无依赖（P0 基础包）
- `types`：无依赖（P0 基础包）
- `renderer`：依赖 `types` 和 `i18n`
- `commands`：依赖 `types`, `i18n`
- `core`（engine.go）：依赖 `commands`, `renderer`, `i18n`, `types`

### 分离的命令函数列表

| 函数名 | 行号（约） | 功能域 |
|--------|-----------|--------|
| `cmdNew` | 4308 | 会话管理 |
| `cmdList` / `cmdListSafe` | 4340-4420 | 会话管理 |
| `cmdSwitch` | 4423 | 会话管理 |
| `cmdName` | 4460 | 会话管理 |
| `cmdCurrent` | 4490 | 会话管理 |
| `cmdStatus` | 4520 | 状态查询 |
| `cmdHistory` | 4550 | 状态查询 |
| `cmdUsage` | 4580 | 状态查询 |
| `cmdWhoami` | 4610 | 状态查询 |
| `cmdModel` / `cmdModelSafe` | 4640-4720 | 模型配置 |
| `cmdReasoning` | 4750 | 模型配置 |
| `cmdMode` | 4780 | 模型配置 |
| `cmdProvider` | 4810 | 模型配置 |
| `cmdLang` | 4840 | 显示配置 |
| `cmdQuiet` | 4870 | 显示配置 |
| `cmdMemory` | 4900 | 高级功能 |
| `cmdCron*` | 4930-5020 | 定时任务 |
| `cmdCommands*` | 5050 | 命令管理 |
| `cmdAlias*` | 5080-5200 | 别名管理 |
| `cmdSearch` | 5230 | 搜索 |
| `cmdShell` | 5260 | 系统工具 |
| `cmdDiff` | 5290 | 系统工具 |
| `cmdShow` | 5320 | 系统工具 |
| `cmdDir` / `cmdDirSafe` | 5350-5380 | 目录管理 |
| `cmdDoctor` | 5410 | 运维命令 |
| `cmdUpgrade` | 5440 | 运维命令 |
| `cmdRestart` | 5470 | 运维命令 |
| `cmdHeartbeat` | 5500 | 运维命令 |
| `cmdCompress` | 5530 | 运维命令 |
| `cmdDelete` / `cmdDeleteBatch` | 5560-5700 | 删除功能 |
| `matchSession()` | 5730 | 辅助函数 |

---

## 前置条件

在开始命令层分离前，请确保已完成：

1. **P0 基础包分离**：
   - `i18n/` 包已分离（包含 `i18n.go` 和 `constants.go`）
   - `types/` 包已分离（包含 `card.go`, `interfaces.go` 等类型定义）

2. **P1 渲染层分离**（可选，但推荐）：
   - `renderer/` 包已分离
   - 所有 `render*Card` 方法已移至 `renderer/cards.go`

---

## 阶段 1：创建命令层基础设施

### 步骤 1.1：创建包结构

```bash
mkdir -p tc-connect/core/commands
touch tc-connect/core/commands/types.go
touch tc-connect/core/commands/command.go
```

### 步骤 1.2：定义命令上下文接口

在 `commands/types.go` 中定义命令执行所需的所有能力：

```go
// tc-connect/core/commands/types.go
package commands

import (
	"tc-connect/core/i18n"
	"tc-connect/core/types"
)

// CommandContext 定义命令执行所需的所有能力
type CommandContext interface {
	// i18n 能力
	T(key i18n.MsgKey, args ...interface{}) string
	
	// 会话管理
	GetSessionManager() interface{}
	GetAgent() interface{}
	GetCurrentMessage() *types.Message
	
	// 平台通信
	Reply(ctx string, msg string)
	ReplyWithCard(ctx string, card types.Card)
	ReplyWithButtons(ctx string, text string, buttons [][]types.ButtonOption)
	
	// 状态管理
	CleanupInteractiveState(key string)
	GetDeleteModeState() interface{}
	SetDeleteModeState(state interface{})
	
	// 其他依赖
	GetCronScheduler() interface{}
	GetCommandRegistry() interface{}
	GetDirHistory() interface{}
	GetRenderer() interface{}
	GetAdminUsers() []string
	GetPlatform() interface{}
	GetWorkspaceDir() string
	CurrentLang() i18n.Language
}

// 辅助类型（避免命令函数依赖 engine.go 内部类型）
type SessionManager interface {
	Current(sessionKey string) (SessionInfo, error)
	List() ([]SessionInfo, error)
	Create(name, sessionKey string) error
	Switch(sessionKey, target string) error
	Delete(id string) error
}

type SessionInfo struct {
	Name      string
	CreatedAt int64
	UpdatedAt int64
	Platform  string
	UserID    string
	// ... 其他字段
}

type ModelSwitcher interface {
	CurrentModel() string
	SwitchModel(model string) error
}

type Renderer interface {
	RenderCurrentCard(info SessionInfo) types.Card
	RenderListCard(sessionKey string, page int) types.Card
	RenderStatusCard() types.Card
	RenderHistoryCard() types.Card
	RenderModelCard() types.Card
	RenderDeleteModeCard(state *DeleteModeState) types.Card
	// ... 其他渲染方法
}

type CronScheduler interface {
	Add(expr, desc, sessionKey string) (int, error)
	List(sessionKey string) ([]CronJob, error)
	Del(id int) error
}

type CronJob struct {
	ID       int
	Expr     string
	Desc     string
	NextRun  int64
}

type DeleteModeState struct {
	Mode     string
	Selected map[string]bool
	Page     int
	Total    int
}
```

---

## 阶段 2：创建命令处理器（对象封装）

### 核心原则

将所有 `cmd*` 方法从 `engine.go` 移动到 `command.go`，函数签名统一改为：

```go
func CmdXxx(ctx CommandContext, args []string) bool
```

### 步骤 2.1：创建命令处理器结构体

**新结构**（commands/command.go）：
```go
// tc-connect/core/commands/command.go
package commands

import (
	"tc-connect/core/i18n"
	"tc-connect/core/types"
)

// Handler 命令处理器，封装 CommandContext
type Handler struct {
	ctx CommandContext
}

// NewHandler 创建命令处理器
func NewHandler(ctx CommandContext) *Handler {
	return &Handler{ctx: ctx}
}

// CmdCurrent 示例：简单查询命令
func (h *Handler) CmdCurrent(args []string) bool {
	msg := h.ctx.GetCurrentMessage()
	
	if len(args) > 0 {
		h.ctx.Reply(msg.Context, h.ctx.T(i18n.MsgUsageCurrent))
		return true
	}
	
	sm := h.ctx.GetSessionManager()
	if sm == nil {
		h.ctx.Reply(msg.Context, h.ctx.T(i18n.MsgErrorSessionNotAvailable))
		return true
	}
	
	// 类型断言
	mgr, ok := sm.(SessionManager)
	if !ok {
		return false
	}
	
	current, err := mgr.Current(msg.SessionKey)
	if err != nil {
		h.ctx.Reply(msg.Context, h.ctx.T(i18n.MsgErrorNoCurrentSession))
		return true
	}
	
	// 调用 renderer
	renderer := h.ctx.GetRenderer()
	if renderer != nil {
		r, ok := renderer.(Renderer)
		if ok {
			card := r.RenderCurrentCard(current)
			h.ctx.ReplyWithCard(msg.Context, card)
		}
	}
	
	return true
}

// CmdList 示例：列表命令
func (h *Handler) CmdList(args []string) bool {
	msg := h.ctx.GetCurrentMessage()
	page := 1
	
	if len(args) > 0 {
		if p, err := strconv.Atoi(args[0]); err == nil && p > 0 {
			page = p
		}
	}
	
	renderer := h.ctx.GetRenderer()
	if renderer != nil {
		r, ok := renderer.(Renderer)
		if ok {
			card := r.RenderListCard(msg.SessionKey, page)
			h.ctx.ReplyWithCard(msg.Context, card)
			return true
		}
	}
	return false
}

// CmdModel 示例：模型配置命令
func (h *Handler) CmdModel(args []string) bool {
	msg := h.ctx.GetCurrentMessage()
	
	if len(args) == 0 {
		agent := h.ctx.GetAgent()
		if agent != nil {
			switcher, ok := agent.(ModelSwitcher)
			if ok {
				model := switcher.CurrentModel()
				h.ctx.Reply(msg.Context, h.ctx.T(i18n.MsgCurrentModel)+": "+model)
				return true
			}
		}
		return false
	}
	
	model := strings.Join(args, " ")
	agent := h.ctx.GetAgent()
	if agent != nil {
		switcher, ok := agent.(ModelSwitcher)
		if ok {
			if err := switcher.SwitchModel(model); err != nil {
				h.ctx.Reply(msg.Context, h.ctx.T(i18n.MsgErrorSwitchModel, err))
				return true
			}
			h.ctx.Reply(msg.Context, h.ctx.T(i18n.MsgModelSwitched, model))
			return true
		}
	}
	return false
}

// CmdCron 示例：子命令处理
func (h *Handler) CmdCron(args []string) bool {
	if len(args) == 0 {
		h.ctx.Reply(h.ctx.GetCurrentMessage().Context, h.ctx.T(i18n.MsgUsageCron))
		return true
	}
	
	switch args[0] {
	case "list":
		return h.CmdCronList(args[1:])
	case "add":
		return h.CmdCronAdd(args[1:])
	case "del":
		return h.CmdCronDel(args[1:])
	default:
		h.ctx.Reply(h.ctx.GetCurrentMessage().Context, h.ctx.T(i18n.MsgUsageCron))
		return true
	}
}

func (h *Handler) CmdCronAdd(args []string) bool {
	if len(args) < 2 {
		h.ctx.Reply(h.ctx.GetCurrentMessage().Context, h.ctx.T(i18n.MsgUsageCronAdd))
		return true
	}
	
	cronExpr := args[0]
	desc := strings.Join(args[1:], " ")
	
	cronScheduler := h.ctx.GetCronScheduler()
	if cronScheduler == nil {
		return false
	}
	
	scheduler, ok := cronScheduler.(CronScheduler)
	if !ok {
		return false
	}
	
	id, err := scheduler.Add(cronExpr, desc, h.ctx.GetCurrentMessage().SessionKey)
	if err != nil {
		h.ctx.Reply(h.ctx.GetCurrentMessage().Context, h.ctx.T(i18n.MsgErrorAddCron, err))
		return true
	}
	
	h.ctx.Reply(h.ctx.GetCurrentMessage().Context, h.ctx.T(i18n.MsgCronAdded, id))
	return true
}

// CmdDelete 示例：复杂状态命令
func (h *Handler) CmdDelete(args []string) bool {
	state := h.ctx.GetDeleteModeState()
	
	if state != nil {
		return h.handleDeleteMode(state, args)
	}
	
	if len(args) == 0 {
		return h.enterDeleteMode()
	}
	
	if args[0] == "batch" {
		return h.handleBatchDelete(args[1:])
	}
	
	return h.handleSingleDelete(args[0])
}

func (h *Handler) enterDeleteMode() bool {
	deleteState := &DeleteModeState{
		Mode:     "select",
		Selected: make(map[string]bool),
		Page:     1,
	}
	h.ctx.SetDeleteModeState(deleteState)
	
	renderer := h.ctx.GetRenderer()
	if renderer != nil {
		r, ok := renderer.(Renderer)
		if ok {
			card := r.RenderDeleteModeCard(deleteState)
			h.ctx.ReplyWithCard(h.ctx.GetCurrentMessage().Context, card)
		}
	}
	return true
}
```

### 步骤 2.6：定义辅助类型

在 `command.go` 顶部或单独文件定义需要的接口：

```go
// 辅助类型定义
type SessionManager interface {
	Current(sessionKey string) (SessionInfo, error)
	List() ([]SessionInfo, error)
	Create(name, sessionKey string) error
	Switch(sessionKey, target string) error
	Delete(id string) error
}

type SessionInfo struct {
	Name      string
	CreatedAt int64
	// ...
}

type ModelSwitcher interface {
	CurrentModel() string
	SwitchModel(model string) error
}

type Renderer interface {
	RenderCurrentCard(info SessionInfo) types.Card
	RenderListCard(sessionKey string, page int) types.Card
	RenderDeleteModeCard(state *DeleteModeState) types.Card
	// ...
}

type CronScheduler interface {
	Add(expr, desc, sessionKey string) (int, error)
	List(sessionKey string) ([]CronJob, error)
	Del(id int) error
}

type CronJob struct {
	ID       int
	Expr     string
	Desc     string
	NextRun  int64
}

type DeleteModeState struct {
	Mode     string
	Selected map[string]bool
	Page     int
}
```

---

## 阶段 3：更新 Engine 集成

### 步骤 3.1：在 Engine 中实现 CommandContext

```go
// tc-connect/core/engine.go
package core

import (
	"tc-connect/core/commands"
	"tc-connect/core/i18n"
	"tc-connect/core/types"
)

type Engine struct {
	// ... 现有字段
	cmdHandler *commands.Handler  // 命令处理器
}

// 实现 commands.CommandContext 接口

func (e *Engine) T(key i18n.MsgKey, args ...interface{}) string {
	return e.I18n.T(key, args...)
}

func (e *Engine) GetCurrentMessage() *types.Message {
	return e.currentMessage
}

func (e *Engine) Reply(ctx string, msg string) {
	e.reply(e.currentPlatform, ctx, msg)
}

func (e *Engine) ReplyWithCard(ctx string, card types.Card) {
	e.replyWithCard(e.currentPlatform, ctx, card)
}

func (e *Engine) ReplyWithButtons(ctx string, text string, buttons [][]types.ButtonOption) {
	e.replyWithButtons(e.currentPlatform, ctx, text, buttons)
}

func (e *Engine) GetSessionManager() interface{} {
	return e.SessionManager
}

func (e *Engine) GetAgent() interface{} {
	return e.Agent
}

func (e *Engine) GetCronScheduler() interface{} {
	return e.CronScheduler
}

func (e *Engine) GetCommandRegistry() interface{} {
	return e.Commands
}

func (e *Engine) GetRenderer() interface{} {
	return e.renderer
}

func (e *Engine) GetDirHistory() interface{} {
	return e.dirHistory
}

func (e *Engine) GetAdminUsers() []string {
	return e.adminUsers
}

func (e *Engine) GetPlatform() interface{} {
	return e.currentPlatform
}

func (e *Engine) GetWorkspaceDir() string {
	return e.workspaceDir
}

func (e *Engine) CurrentLang() i18n.Language {
	return e.currentLang
}

func (e *Engine) CleanupInteractiveState(key string) {
	e.cleanupInteractiveState(key)
}

func (e *Engine) GetDeleteModeState() interface{} {
	return e.deleteModeState
}

func (e *Engine) SetDeleteModeState(state interface{}) {
	e.deleteModeState = state
}
```

### 步骤 3.2：初始化命令处理器

```go
func NewEngine() *Engine {
	e := &Engine{
		// ... 其他初始化
	}
	// 创建命令处理器（与 renderer.NewRenderer 对称）
	e.cmdHandler = commands.NewHandler(e)
	return e
}
```

### 步骤 3.3：更新 handleMessage（保留 switch-case）

**原代码**：
```go
func (e *Engine) handleMessage(p types.Platform, msg *types.Message) {
	if strings.HasPrefix(msg.Text, "/") {
		cmd := strings.Fields(msg.Text)[0][1:]
		args := strings.Fields(msg.Text)[1:]
		switch cmd {
		case "new":
			e.cmdNew(p, msg, args)
		case "list":
			e.cmdList(p, msg, args)
		// ... 180 个 case
		}
	}
}
```

**新代码**（保留 switch-case，调用命令处理器方法）：
```go
func (e *Engine) handleMessage(p types.Platform, msg *types.Message) {
	e.currentPlatform = p
	e.currentMessage = msg
	
	if strings.HasPrefix(msg.Text, "/") {
		cmd := strings.Fields(msg.Text)[0][1:]
		args := strings.Fields(msg.Text)[1:]
		
		switch cmd {
		case "new":
			e.cmdHandler.CmdNew(args)
		case "list":
			e.cmdHandler.CmdList(args)
		case "switch":
			e.cmdHandler.CmdSwitch(args)
		case "name":
			e.cmdHandler.CmdName(args)
		case "current":
			e.cmdHandler.CmdCurrent(args)
		case "status":
			e.cmdHandler.CmdStatus(args)
		// ... 其他命令
		default:
			e.reply(p, msg.Context, e.T(i18n.MsgCommandNotFound, cmd))
		}
		return
	}
	
	e.processAgentMessage(p, msg)
}
```

---

## 阶段 4：辅助函数迁移

将 `engine.go` 中的命令辅助函数（如 `parseDeleteBatchIndices`、`matchSubCommand` 等）移到 `commands/command.go` 文件底部，或根据需要内联到对应命令函数中。

---

## 阶段 5：清理和验证

### 步骤 5.1：删除原命令函数

```bash
grep -n "^func (e \*Engine) cmd" tc-connect/core/engine.go
```

逐个删除所有 `cmd*` 函数，每删除一批编译验证：
```bash
go build ./tc-connect
```

### 步骤 5.2：运行测试

```bash
cd tc-connect
go test ./core/commands -v
go test ./core -v
```

### 步骤 5.3：代码审查检查点

- [ ] 所有 `cmd*` 函数已从 `engine.go` 移除
- [ ] `command.go` 包含所有命令处理逻辑（约 2000 行）
- [ ] `types.go` 包含 `CommandContext` 接口和辅助类型
- [ ] `engine.go` 中 switch-case 改为调用 `commands.CmdXxx` 函数
- [ ] 没有循环依赖
- [ ] `Engine` 正确实现 `CommandContext` 接口

---

## 回滚方案

1. **单命令回滚**：将 `command.go` 中的 `CmdXxx` 函数复制回 `engine.go` 作为方法 `cmdXxx`
2. **整体回滚**：删除 `core/commands` 目录，恢复 `engine.go` 到重构前版本

---

## core 包拆分参考

根据 `CLASSIFICATION.md` 的分析，除 `engine.go` 外，core 包下其他文件的拆分优先级如下：

| 优先级 | 文件 | 目标包 | 大小 | 理由 |
|--------|------|--------|------|------|
| **P0** | `i18n.go` | `core/i18n/` | 171KB | 独立基础组件，无依赖 |
| **P0** | `session.go` | `core/session/` | 12KB | 清晰的边界，被 engine 重度使用 |
| **P0** | `interfaces.go` + `message.go` | `core/types/` | 17KB | 纯类型定义，零依赖 |
| **P1** | `cron.go` | `core/cron/` | 17KB | 完整的业务单元（CronStore, CronScheduler） |
| **P1** | `command.go` | `core/command/` | 6KB | 独立注册表（CommandRegistry） |
| **P1** | `skill.go` | `core/skill/` | 5KB | 独立注册表（SkillRegistry） |
| **P2** | `progress_compact.go` + `reference_render.go` | `core/renderer/` | 32KB | 渲染逻辑（进度卡、文件引用格式化） |
| **P2** | `streaming.go` | `core/renderer/` 或 `core/streaming/` | 11KB | 流式处理 |
| **P3** | `api.go` + `bridge.go` | `core/server/` | 9KB | 网络服务（HTTP API、WebSocket） |
| **P3** | `doctor.go` | `core/doctor/` | 11KB | 健康检查 |
| **P4** | `ratelimit.go` | `core/utils/` 或独立 | 3KB | 工具类（限流器） |

---

## 下一步

检查engine.go 中可分离方法 未使用 e.** 方法

---

**文档版本**: v1.0  
**创建日期**: 2026-06-25  
**状态**: 待实施