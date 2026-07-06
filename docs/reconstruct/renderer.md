# 渲染层分离重构指南 - 方案 A（接口抽象）

## 概述

本文档描述将 `tc-connect/core/engine.go` 中的渲染逻辑分离到独立 `renderer` 包的详细步骤，采用**接口抽象**方式。渲染层包含所有 `render*Card` 方法，负责将数据结构转换为平台 Card 格式。

### 目标结构

```
tc-connect/core/
├── i18n/                 # P0 已分离：独立国际化包
│   ├── i18n.go           # 翻译引擎（171KB）
│   └── constants.go      # 消息键常量（MsgKey 类型）
├── types/
│   └── card.go           # Card 类型定义（Card, CardHeader, CardElement 等）
├── renderer/
│   ├── types.go          # 渲染器类型（Renderer, RenderContext）
│   ├── cards.go          # 所有 Render*Card 方法
│   ├── helpers.go        # 渲染辅助函数
│   └── i18n_bridge.go    # 与 i18n 的桥梁
└── engine.go             # 移除渲染代码，保留调用入口
```

**包依赖关系**（无循环）：
- `i18n`：无依赖（P0 基础包，已分离）
- `types`：无依赖
- `renderer`：依赖 `types` 和 `i18n`
- `core`（engine.go 等）：依赖 `types`, `renderer` 和 `i18n`

### 分离的渲染函数列表

| 函数名 | 行号（约） | 依赖 |
|--------|-----------|------|
| `renderHelpGroupCard` | 2204 | i18n, helpCardGroups |
| `renderModeCard` | 2250 | agent (ModeSwitcher), i18n |
| `renderLangCard` | 2305 | i18n |
| `renderListCard` / `renderListCardSafe` | 2330-2420 | SessionManager, i18n |
| `renderStatusCard` | 2423 | i18n |
| `renderCurrentCard` | 2499 | i18n |
| `renderHistoryCard` | 2514 | i18n |
| `renderCronCard` | 2545 | CronScheduler, i18n |
| `renderCommandsCard` | 2625 | CommandRegistry, i18n |
| `renderAliasCard` | 2659 | i18n |
| `renderConfigCard` | 2775 | configItem 列表，i18n |
| `renderDoctorCard` | 2793 | DoctorInfo, i18n |
| `renderWhoamiCard` | 2804 | Message, i18n |
| `renderVersionCard` | 2834 | VersionInfo, i18n |
| `renderDeleteModeCard` | 2843 | deleteModeState, i18n |
| `renderDeleteModeSelectCard` | 2864 | SessionManager, deleteModeState, AgentSessionInfo |
| `renderDeleteModeConfirmCard` | 2937 | SessionManager, deleteModeState |
| `renderDeleteModeResultCard` | 2954 | deleteModeState |
| `renderCardForPlatform` | 2962 | Platform |
| `renderCardForPlatformWorkspace` | 2966 | Platform, workspaceDir |
| `renderModelCard` | 3009 | agent (ModelSwitcher), i18n |
| `renderHelpCard` | 3053 | helpCardGroups, i18n |
| `renderSkillsCard` | 3057 | SkillRegistry, i18n |
| `renderDirCard` / `renderDirCardSafe` | 3077-3085 | DirHistory, i18n |

---

## 阶段 0：分离 i18n 包（P0 前置依赖，必须最先执行）

**原因**：`renderer/` 依赖 `i18n/` 包获取消息键常量（`i18n.MsgCardBack` 等），且 `i18n.go`（171KB）是核心包中最大的单体文件，必须首先分离。

### 步骤 0.1：创建 i18n 独立包

```bash
# 创建 i18n 目录
mkdir -p tc-connect/core/i18n

# 移动 i18n.go
mv tc-connect/core/i18n.go tc-connect/core/i18n/i18n.go
```

### 步骤 0.2：修改包声明

编辑 `tc-connect/core/i18n/i18n.go`，将第一行改为：

```go
package i18n
```

**注意**：不能只改包名而不移到独立目录，否则同目录下 `engine.go`（package core）会冲突。

### 步骤 0.3：导出消息键类型和常量（关键）

在 `i18n/i18n.go` 顶部确保定义 `MsgKey` 类型（用于类型安全）：

```go
// i18n/i18n.go 第 120 行左右
type MsgKey string

const (
	MsgCardBack MsgKey = "card_back"
	MsgCardPrev MsgKey = "card_prev"
	MsgCardNext MsgKey = "card_next"
	// ... 其他消息键常量
)
```

**重要**：`MsgKey` 是自定义的 `string` 别名，用于类型安全。如果 `T()` 方法接受 `MsgKey`，则调用方必须传入该类型。

### 步骤 0.4：更新所有引用

全局替换所有 `i18n.T()` 或 `e.I18n.T()` 的导入：

**原代码**（在 `engine.go` 等文件中）：
```go
package core

// i18n.go 在同一包中，直接调用
result := i18n.T("card_back")
```

**新代码**：
```go
package core

import "tc-connect/core/i18n"

// 方式 1：直接使用（如果 T 函数接受 string）
result := i18n.T("card_back")

// 方式 2：使用常量（推荐，类型安全）
result := i18n.T(i18n.MsgCardBack)

// 方式 3：在 Engine 中封装
func (e *Engine) T(key i18n.MsgKey, args ...interface{}) string {
	return e.I18n.T(key, args...)
}
```

### 步骤 0.5：验证编译

```bash
cd tc-connect
go build ./core/i18n
# 应无错误
```

---

## 阶段 1：准备工作（低风险）

### 步骤 1.1：确认测试覆盖

在开始重构前，确保有测试验证现有行为：

```bash
cd tc-connect
go test ./core -v -run "Test.*Render|Test.*Card"  # 确认现有测试（如有）
```

### 步骤 1.2：创建包结构

```bash
# 创建 types 包存放 Card 类型
mkdir -p tc-connect/core/types

# 创建 renderer 包
mkdir -p tc-connect/core/renderer
touch tc-connect/core/renderer/types.go
touch tc-connect/core/renderer/cards.go
touch tc-connect/core/renderer/helpers.go
```

### 步骤 1.3：解决 Card 类型循环依赖问题（关键）

**问题**：
- `card.go` 属于 `core` 包（`package core`）
- `renderer` 是独立包（`package renderer`）
- 如果 renderer 导入 card，实际是导入整个 `core` 包 → **循环依赖**（core → renderer → core）

**解决方案**：将 Card 类型定义移到独立的基础包 `types`

**执行步骤**：

1. **复制类型定义**：将 `card.go` 中的所有类型定义（`Card`, `CardHeader`, `CardElement`, `CardButton`, `CardActions`, `CardNote`, `CardListItem`, `CardMarkdown`, `CardDivider`, `CardSelect`, `CardSelectOption` 及接口实现方法）复制到 `types/card.go`

2. **修改 package 声明**：
   ```go
   // tc-connect/core/types/card.go
   package types
   ```

3. **更新原 card.go**：保留 `CardBuilder`（如果它依赖 core 包其他内容），修改为使用 `types.Card`：
   ```go
   // tc-connect/core/card.go
   package core
   
   import "tc-connect/core/types"
   
   type CardBuilder struct {
       card types.Card
   }
   
   func NewCard() *CardBuilder { return &CardBuilder{} }
   // ... 其他方法，返回 *types.Card
   ```

4. **更新所有导入**：全局搜索 `card.Card` 改为 `types.Card`（在 engine.go, session.go 等文件中）

**依赖关系**（无循环）：
- `types`：无依赖（纯数据定义）
- `renderer`：依赖 `types`
- `core`：依赖 `types` 和 `renderer`

**验证**：
```bash
go build ./tc-connect
# 应无循环依赖错误
```

---

## 阶段 2：创建渲染器上下文（方案 A）

### 步骤 2.1：定义渲染依赖接口（方案 A - 接口抽象）

在 `renderer/types.go` 中定义渲染器需要的所有能力：

```go
// tc-connect/core/renderer/types.go
package renderer

import (
	"tc-connect/core/i18n"
	"tc-connect/core/types"
)

// RenderContext 定义渲染器需要的所有能力
type RenderContext interface {
	// i18n 能力（使用 i18n.MsgKey 类型确保类型安全）
	T(key i18n.MsgKey, args ...interface{}) string
	
	// 数据访问能力（只读）
	GetSessionManager() interface{}      // 返回 session.SessionManager 接口
	GetCronScheduler() interface{}
	GetCommandRegistry() interface{}
	GetSkillRegistry() interface{}
	GetAgent() interface{}
	GetDirHistory() interface{}
	
	// 配置访问
	GetPlatform() interface{}
	GetWorkspaceDir() string
	
	// 业务状态访问（只读）
	GetDeleteModeState() interface{}
	GetHelpCardGroups() interface{}
}
```

**注意**：`T()` 方法接受 `i18n.MsgKey` 而非 `string`，以利用类型系统防止拼写错误。

### 步骤 2.2：创建渲染器结构体并解决循环依赖

**核心原则**：renderer 包**绝不能**导入 engine 包，否则形成循环依赖（engine → renderer → engine）。

在 `renderer/types.go` 中继续添加结构体定义：

```go
// 继续添加到 renderer/types.go

type Renderer struct {
	ctx RenderContext  // 直接持有接口值，不是指针
}

func NewRenderer(rCtx RenderContext) *Renderer {
	return &Renderer{ctx: rCtx}
}

// 封装 i18n 调用，支持类型安全
func (r *Renderer) T(key i18n.MsgKey, args ...interface{}) string {
	return r.ctx.T(key, args...)
}

// 按钮辅助方法（使用 i18n 常量）
func (r *Renderer) cardBackButton() types.CardButton {
	return types.DefaultBtn(r.ctx.T(i18n.MsgCardBack), "nav:/help")
}

func (r *Renderer) cardPrevButton(action string) types.CardButton {
	return types.DefaultBtn(r.ctx.T(i18n.MsgCardPrev), action)
}

func (r *Renderer) cardNextButton(action string) types.CardButton {
	return types.DefaultBtn(r.ctx.T(i18n.MsgCardNext), action)
}
```

然后在 `engine.go` 中实现该接口：

```go
// tc-connect/core/engine.go
package core

import (
	"tc-connect/core/i18n"
	"tc-connect/core/renderer"
)

// 实现 renderer.RenderContext 接口
func (e *Engine) T(key i18n.MsgKey, args ...interface{}) string {
	return e.I18n.T(key, args...)
}

func (e *Engine) GetSessionManager() interface{} {
	return e.SessionManager
}

func (e *Engine) GetCronScheduler() interface{} {
	return e.CronScheduler
}
// ... 其他 GetXXX 方法

// 初始化 renderer
func (e *Engine) initRenderer() {
	// Engine 实现了 RenderContext 接口，可以直接传入
	e.renderer = renderer.NewRenderer(e)
}
```

**推荐**：方案 A（接口抽象）最灵活，适合 renderer 需要调用 Engine 的**方法**的场景。

---

### 步骤 2.3：创建 i18n 桥梁（可选）

如果采用方案 A，可以简化 i18n 调用：

```go
// tc-connect/core/renderer/i18n_bridge.go
package renderer

func (r *Renderer) T(key string, args ...interface{}) string {
	return r.ctx.T(key, args...)
}
```

---

## 阶段 3：逐步迁移渲染函数（高风险，需逐个进行）

### 迁移顺序建议（从简单到复杂）

1. **纯信息展示类**：`renderStatusCard`, `renderCurrentCard`, `renderHistoryCard`, `renderVersionCard`, `renderWhoamiCard`
2. **简单列表类**：`renderConfigCard`, `renderDoctorCard`
3. **带交互类**：`renderCronCard`, `renderCommandsCard`, `renderAliasCard`
4. **复杂业务类**：`renderDeleteMode*Card` 系列
5. **依赖最多类**：`renderListCard`, `renderHelpCard`, `renderSkillsCard`, `renderModelCard`

### 步骤 3.1：迁移 `renderStatusCard` 作为示例（方案 A）

**原函数**（engine.go:2423）：
```go
func (e *Engine) renderStatusCard() *Card {
	var elements []CardElement
	if e.SessionManager != nil {
		elements = append(elements, CardListItem{
			Text:     e.T("current_session"),
			BtnText:  "查看",
			BtnType:  "primary",
			BtnValue: "session",
		})
	}
	return &Card{
		Header: &CardHeader{Title: e.T("status"), Color: "blue"},
		Elements: elements,
	}
}
```

**新函数**（方案 A - 接口抽象）：
```go
func (r *Renderer) RenderStatusCard() *types.Card {
	var elements []types.CardElement
	if sm := r.ctx.GetSessionManager(); sm != nil {
		elements = append(elements, types.CardListItem{
			Text:     r.T(i18n.MsgCurrentSession),
			BtnText:  "查看",
			BtnType:  "primary",
			BtnValue: "session",
		})
	}
	return &types.Card{
		Header: &types.CardHeader{Title: r.T(i18n.MsgStatus), Color: "blue"},
		Elements: elements,
	}
}
```

**注意**：使用 `i18n.MsgXXX` 常量而非字符串字面量，确保编译期检查。

**在 engine.go 中调用**：
```go
// 方案 A（Engine 实现 RenderContext 接口）
e.renderer = renderer.NewRenderer(e)  // 直接传入 Engine 实例
```

**在 engine.go 中替换调用**：
```go
// 原调用
card := e.renderStatusCard()

// 新调用
card := e.renderer.RenderStatusCard()
```

### 步骤 3.2：迁移列表类渲染函数（方案 A - 需要类型断言）

`renderListCard` 依赖 `SessionManager.List()`，迁移时注意：

**方案 A**（接口需要类型断言）：
```go
func (r *Renderer) RenderListCard() *types.Card {
	sm := r.ctx.GetSessionManager()
	if sm == nil {
		return r.renderErrorCard(r.T("no_data"), nil)
	}
	
	// 类型断言
	mgr, ok := sm.(session.Manager)
	if !ok {
		return r.renderErrorCard(r.T("type_error"), nil)
	}
	
	sessions, err := mgr.List()
	// ... 构建 []types.CardElement 并返回 *types.Card
}
```

### 步骤 3.3：迁移删除模式系列（复杂状态处理）

**方案 A（接口）**：在 RenderContext 接口中添加 `GetDeleteModeState() interface{}`，返回当前状态。

### 步骤 3.4：迁移 `renderHelpCard` 和 `renderHelpGroupCard`（跨包数据共享）

**方案 3**：Engine 准备数据后传递给 renderer（最解耦）
```go
// engine.go
func (e *Engine) renderHelpCard() *types.Card {
	data := e.getHelpData()  // 内部方法，调用 helpCardGroups()
	return e.renderer.RenderHelp(data)
}
```

---

## 阶段 4：更新 Engine 调用（中风险）

### 步骤 4.1：在 Engine 中初始化 renderer（方案 A）

```go
// engine.go (在 Engine 结构中添加)
type Engine struct {
	// ... 现有字段
	renderer *renderer.Renderer
}

// 方案 A：Engine 实现 renderer.RenderContext 接口
func (e *Engine) initRenderer() {
	// Engine 实现了 RenderContext 接口，直接传入
	e.renderer = renderer.NewRenderer(e)
}

// Engine 实现 RenderContext 接口的方法
func (e *Engine) T(key string, args ...interface{}) string {
	return e.I18n.T(key, args...)
}

func (e *Engine) GetSessionManager() interface{} {
	return e.SessionManager
}
// ... 其他 GetXXX 方法
```

### 步骤 4.2：逐个替换调用

找到所有 `e.render*Card()` 调用并替换为 `e.renderer.Render*Card()`：

```bash
# 查找所有调用
grep -n "e\.render" tc-connect/core/engine.go
```

替换示例：
- `e.renderStatusCard()` → `e.renderer.RenderStatusCard()`
- `e.renderListCard()` → `e.renderer.RenderListCard()`

### 步骤 4.3：删除原渲染函数

每迁移一个函数，删除 engine.go 中对应的原函数，编译验证：

```bash
go build ./tc-connect
```

---

## 阶段 5：清理和验证

### 步骤 5.1：移除 engine.go 的渲染依赖

确认 engine.go 不再导入纯渲染相关的包（如 `github.com/xxx/platform` 的 Card 类型，如果已移动到 renderer）。

### 步骤 5.2：运行测试

```bash
cd tc-connect
go test ./core/renderer -v
go test ./core -v  # 确保 engine.go 仍正常工作
```

### 步骤 5.3：代码审查检查点

- [ ] 所有 `render*Card` 函数已从 engine.go 移除
- [ ] renderer 包通过依赖注入获得所有必需依赖
- [ ] 没有循环依赖（renderer → engine → renderer）
- [ ] i18n 调用通过 Builder.T() 或 Deps.T() 统一处理
- [ ] Card 类型定义在 `types/card.go`，所有包使用 `types.Card`

---

## 回滚方案

如果遇到问题：

1. **单函数回滚**：将 renderer 中的函数复制回 engine.go，恢复原调用
2. **整体回滚**：删除 `core/renderer` 目录，恢复 engine.go 到重构前版本

---

## 下一步

完成渲染层分离后，继续执行 RECONSTRUCT.md 中的 Phase 2（数据管理层分离）。

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

### 依赖图（计划）

```
i18n/          [base, no deps]
   ↓
types/         [base, no deps]
   ↓
renderer/      [depends on i18n/, types/]
   ↓
engine.go      [depends on renderer/, i18n/, types/, session/, cron/, command/, skill/]
   ↓
server/        [depends on engine.go]
```
