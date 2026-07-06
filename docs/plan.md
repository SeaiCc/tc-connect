# TC-Connect 多 Agent 编排开发计划

## 全局信息
项目根目录: /media/ubuntu/data/gitSourceCode/AIWorkflow/tc-connect

## 1. 项目概述

### 1.1 目标
开发一个连接飞书和多个子 Agent 的中间控制层，实现 AI 工作流自动化编排。

### 1.2 核心功能
- 解析主 Agent 输出的协议格式：`[AGENT_NAME]||[PARAMS]||[SESSION_ID]`
- 通过 `opencode run --agent agent_name` 命令调用已注册的子 Agent
- 启动子 Agent 进程并传递参数
- 会话管理（RESUME true/false）
- 获取并存储 session_id 用于溯源（`opencode session list -n 1`）
- 超时控制和重试机制
- 结果返回给主 Agent 继续流程

### 1.3 工作流程
```
用户飞书输入 /harness <功能>
    ↓
主 Agent 接收并按协议输出
    ↓
tc-connect 解析协议 (AGENT_NAME||PARAMS||RESUME)
    ↓
执行 `opencode run --agent {agent_name}`
    ↓
子 Agent 执行并返回结果
    ↓
获取 session_id (`opencode session list -n 1`)
    ↓
存储 session_id 用于后续溯源
    ↓
tc-connect 返回给主 Agent
    ↓
重复直到主 Agent 输出 END
```

---

## 2. 架构设计

### 2.1 新增模块结构

```
core/
├── harness/
│   ├── orchestrator.go    // 编排器主逻辑
│   ├── protocol.go        // 协议解析器
│   ├── subagent.go        // 子 Agent 管理器
│   ├── session.go         // 会话管理器
│   └── types.go           // 类型定义
```

### 2.2 核心接口

#### 2.2.1 协议格式
```go
type HarnessRequest struct {
    AgentName string // 子 Agent 标识，对应 opencode 已注册的 agent
    Params    string // 执行参数和文件路径
    Resume    bool   // 是否复用上一次 session
}

type SubAgentResult struct {
    Output    string // 子 Agent 执行输出
    SessionID string // 本次执行的 session_id，用于溯源
    Error     error  // 错误信息
}
```

#### 2.2.2 编排器接口
```go
type Orchestrator interface {
    // 启动编排流程
    Start(ctx context.Context, mainAgentOutput string) error
    // 解析协议并执行子 Agent
    ExecuteSubAgent(req HarnessRequest) (*SubAgentResult, error)
    // 停止编排
    Stop() error
}
```

---

## 3. 开发任务分解

### 阶段 1: 核心基础设施 (Priority: High)

#### Task 1.1: 协议解析器
**文件**: `core/harness/protocol.go`

**任务**:
- [x] 实现协议解析函数：`ParseProtocol(input string) (*HarnessRequest, error)`
- [x] 处理协议格式：`agent_name||params||resume`
- [x] 支持 `END` 标识检测
- [x] 错误处理：格式错误、缺失字段等

**验收标准**:
- [x] 能正确解析 `planner||生成项目规划||false`
- [x] 能识别 `END` 并返回 nil 或特殊标记
- [x] 处理边界情况（空字符串、多余分隔符、空白字符、大小写敏感等）

#### Task 1.2: 子 Agent 验证与配置
**文件**: `core/harness/subagent.go`

**任务**:
- [x] 实现子 Agent 可用性检查：`IsSubAgentAvailable(name string) bool`
- [x] 验证 opencode 已注册的子 Agent 列表
- [x] 配置子 Agent 参数映射（超时、重试等）
- [x] 支持子 Agent 别名配置

**验收标准**:
- [x] 能获取 opencode 已注册的所有子 Agent
- [x] 正确验证 agent_name 是否可用
- [x] 支持配置文件中定义子 Agent 别名和参数

#### Task 1.3: 会话管理器
**文件**: `core/harness/session.go`

**任务**:
- [x] 设计会话存储结构（内存 + 可选持久化）
- [x] 执行 `opencode session list -n 1` 获取最新 session_id
- [x] 实现会话创建：`CreateSession(agentName string) (SessionID, error)`
- [x] 实现会话复用逻辑（RESUME=true 时）
- [x] 实现会话清理（超时/手动）
- [x] 支持会话溯源查询接口

**验收标准**:
- [x] 支持多个子 Agent 并发会话
- [x] 能正确捕获并存储每次执行的 session_id
- [x] RESUME=true 时能正确关联历史上下文
- [x] 会话超时自动清理（可配置）
- [x] 支持通过 session_id 回溯调试

---

### 阶段 2: 编排引擎 (Priority: High)

#### Task 2.1: 子 Agent 执行器
**文件**: `core/harness/subagent.go`

**任务**:
- [x] 封装 `opencode run --agent <name>` 命令调用
- [x] 支持工作目录设置 (`--work-dir`)
- [x] 实现进程启动和输出捕获
- [x] 实现超时控制（可配置，默认 5 分钟）
- [x] 实现重试逻辑（最多 3 次）
- [x] 支持会话复用参数传递
- [x] 执行后调用 `opencode session list -n 1` 获取 session_id
- [x] 返回执行结果包含 session_id 用于溯源

**验收标准**:
- [x] 能调用 opencode CLI 启动指定子 Agent
- [x] 超时自动杀死进程（SIGTERM -> SIGKILL）
- [x] 失败重试 3 次后返回错误
- [x] 捕获标准输出和错误输出
- [x] 正确传递 RESUME 参数
- [x] 每次执行后返回有效的 session_id（格式如 ses_16f661969ffeziRL4Yo5qkpPWg）
- [x] 支持后续通过 `opencode run -s session_id` 溯源

#### Task 2.2: 编排器主循环
**文件**: `core/harness/orchestrator.go`

**任务**:
- [x] 实现主编排循环
- [x] 协议解析 -> 子 Agent 执行 -> 结果返回
- [x] 状态机管理（IDLE, RUNNING, END）
- [x] 错误处理和回滚

**验收标准**:
- [x] 能处理完整的工作流：解析 -> 执行 -> 返回
- [x] 遇到 END 标识正确退出
- [x] 错误时返回清晰信息给主 Agent

#### Task 2.3: 并发控制
**文件**: `core/harness/orchestrator.go`

**任务**:
- [x] 实现并发限制（semaphore 或 worker pool）
- [x] 支持多个 tc-connect 实例同时处理
- [x] 资源隔离（不同项目/会话）

**验收标准**:
- [x] 支持配置最大并发数（默认 5）
- [x] 超过限制时排队等待
- [x] 内存使用可控

---

### 阶段 3: 与现有系统集成 (Priority: Medium)

#### Task 3.1: Engine 集成
**文件**: `core/engine.go`

**任务**:
- [x] 在 Engine 中添加 Orchestrator 字段
- [x] 添加 `/harness` 命令入口
- [x] 拦截主 Agent 输出，检测协议格式
- [x] 触发编排流程

**验收标准**:
- [x] 用户输入 `/harness <prompt>` 能触发主 Agent
- [x] 主 Agent 输出协议格式时自动进入编排模式
- [x] 编排完成返回结果给飞书

#### Task 3.2: 飞书平台适配
**文件**: `platform/feishu/feishu.go`

**任务**:
- [x] 支持长任务流式输出（typing indicator）
- [x] 支持进度更新（卡片或文本）
- [x] 错误时发送告警卡片

**验收标准**:
- [x] 用户能看到"正在处理"状态
- [x] 超时/错误时收到明确通知

#### Task 3.3: 配置管理
**文件**: `config/config.go`

**任务**:
- [x] 添加 harness 配置段
  ```toml
  [projects.agent.options.harness]
  enabled = true
  timeout_mins = 5
  max_retries = 3
  max_concurrent = 5
  
  [projects.agent.options.harness.agents]
  planner.timeout_mins = 10
  developer.timeout_mins = 15
  ```
- [x] 支持运行时配置重载
- [x] 支持子 Agent 独立配置

**验收标准**:
- [x] 配置项可灵活调整
- [x] 不影响现有功能
- [x] 不同子 Agent 可配置不同超时时间

---

### 阶段 4: 监控和日志 (Priority: Medium)

#### Task 4.1: 日志系统
**任务**:
- [x] 添加编排日志（slog）
- [x] 记录协议解析、子 Agent 启动、执行结果
- [x] 记录每次执行的 session_id
- [x] 支持日志级别配置
- [x] 支持 session_id 查询日志

**验收标准**:
- [x] 日志系统正常工作

#### Task 4.2: 监控指标
**任务**:
- [x] 子 Agent 执行时长统计
- [x] 成功率/失败率统计
- [x] 超时次数统计

**验收标准**:
- [x] 监控指标正常工作

---

### 阶段 5: 测试与优化 (Priority: Low)

#### Task 5.1: 单元测试
**任务**:
- [x] protocol.go 解析测试
- [x] 子 Agent 执行器测试
- [x] 会话管理测试

**验收标准**:
- [x] 单元测试全部通过

#### Task 5.2: 集成测试
**任务**:
- [x] 完整工作流测试
- [x] 超时和重试测试
- [x] 并发压力测试

**验收标准**:
- [ ] 集成测试全部通过

#### Task 5.3: 性能优化
**任务**:
- [ ] 子 Agent 进程复用（可选）
- [ ] 提示词缓存
- [ ] 内存泄漏检查

---

## 4. 风险控制

### 4.1 子 Agent 卡死
- **措施**: 超时检测 + 进程杀死 + 3 次重试
- **回滚**: 返回错误给飞书，不阻塞主流程

### 4.2 内容超限
- **措施**: 提示词规范约束 + 输出截断告警
- **监控**: 检测到超长输出时记录日志

### 4.3 资源耗尽
- **措施**: 并发限制 + 连接池
- **保护**: 每个项目独立限制

---

## 5. 开发顺序建议

1. **先实现协议解析器**（Task 1.1）- 基础，独立测试
2. **实现子 Agent 验证与配置**（Task 1.2）- 集成 opencode CLI
3. **实现子 Agent 执行器**（Task 2.1）- 核心功能
4. **实现编排器**（Task 2.2）- 串联流程
5. **集成到 Engine**（Task 3.1）- 端到端测试
6. **完善配置和监控**（其他任务）

---

## 6. 依赖关系图

```
阶段 1 (基础)
  ├─ Task 1.1 (协议解析) ───┐
  ├─ Task 1.2 (子 Agent 加载) ├─→ 阶段 2
  └─ Task 1.3 (会话管理) ───┘
        
阶段 2 (核心)
  ├─ Task 2.1 (执行器) ───┐
  ├─ Task 2.2 (编排器) ───┼─→ 阶段 3
  └─ Task 2.3 (并发) ─────┘
        
阶段 3 (集成)
  ├─ Task 3.1 (Engine) ─┐
  ├─ Task 3.2 (飞书) ───┼─→ 阶段 4
  └─ Task 3.3 (配置) ───┘
        
阶段 4 (完善)
  └─ Task 4.1-5.3
```

---

## 7. 验收标准

### 功能验收
- [ ] 用户输入 `/harness 生成规划`，主 Agent 输出协议，子 Agent 正确执行
- [ ] RESUME=true 时能正确复用 session
- [ ] 每次执行后返回有效的 session_id（格式 ses_xxx）
- [ ] 超时 5 分钟后自动终止子 Agent
- [ ] 重试 3 次失败后返回错误
- [ ] END 标识正确终止流程
- [ ] 支持通过 session_id 溯源查询

### 性能验收
- [ ] 支持 5 个并发编排任务
- [ ] 内存占用 < 500MB
- [ ] 协议解析延迟 < 10ms

### 稳定性验收
- [ ] 连续运行 24 小时无崩溃
- [ ] 内存无泄漏
- [ ] 错误处理完善

---

## 8. 参考文档

- [HARNESS.md](./HARNESS.md) - 需求规格
- [subagent/timer.md](../subagent/timer.md) - 子 Agent 提示词示例
- [cmd/tc-connect/main.go](../cmd/tc-connect/main.go) - 主入口
- [core/interfaces.go](../core/interfaces.go) - 接口定义
