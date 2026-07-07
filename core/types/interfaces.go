package types

import "context"

// 表示表示一个inline可点击的button
type ButtonOption struct {
	Text string // button上的文本
	Data string // 点击时的回调数据(≤64 bytes for Telegram)
}

// ======================== Platform ========================

// 抽象一个信息平台
type Platform interface {
	Name() string
	Start(handler MessageHandler) error
	Reply(ctx context.Context, replyCtx any, content string) error
	Send(ctx context.Context, replyCtx any, content string) error
	Stop() error
}

// 可选接口用于平台可以给sessionkey重新创建一个reply context.
// 当定时任务给用户发送一个不带收到消息的message时需要
type ReplyContextReconstructor interface {
	ReconstructReplyCtx(sessionKey string) (any, error)
}

// 从异步可恢复平台接收就绪状态转换。
type PlatformLifecycleHandler interface {
	OnPlatformReady(p Platform)
	OnPlatformUnavailable(p Platform, err error)
}

// AsyncRecoverablePlatform是一个可选接口，适用于启动后台恢复循环并随后报告就绪状态或不可用的平台。
// 实现此接口的平台可能会在它们实际准备好接收流量之前从Start()返回。
// 调用者必须将OnPlatformReady视为一个信号，表明延迟的平台功能可能已初始化，平台可用。
// 因此，Start()返回值为nil意味着恢复循环已成功启动，但并不一定表示已建立初始连接。
type AsyncRecoverablePlatform interface {
	Platform
	SetLifecycleHandler(h PlatformLifecycleHandler)
}

// 用于平台的可选接口，该接口可在代理工作时显示一个“处理中”指示器（如打字气泡、表情符号反应等）。
// 处理开始时调用 StartTyping，并返回一个停止函数，调用者必须在处理结束时调用该函数。
type TypingIndicator interface {
	StartTyping(ctx context.Context, replyCtx any) (stop func())
}

// ProgressUpdater 用于长任务的进度更新接口（如多 Agent 编排）
type ProgressUpdater interface {
	// UpdateProgress 更新进度卡片（流式更新）
	// progress: 当前步骤，total: 总步骤，message: 当前步骤描述
	UpdateProgress(ctx context.Context, replyCtx any, progress int, total int, message string) error
	// SendErrorCard 发送错误告警卡片
	SendErrorCard(ctx context.Context, replyCtx any, title string, errorDetail string, suggestions []string) error
}

// 平台实现可以更新消息的接口
type MessageUpdater interface {
	UpdateMessage(ctx context.Context, replyCtx any, content string) error
}

// 平台支持富卡片(如飞书交互卡片). 不支持的平台会通过Card.RenderText()收到plain-text
type CardSender interface {
	SendCard(ctx context.Context, replyCtx any, card *Card) error
	ReplyCard(ctx context.Context, replyCtx any, card *Card) error
}

// expose 一种用于中间进度渲染的首选样式。
// 典型值：“legacy”、“compact”、“card”。
type ProgressStyleProvider interface {
	ProgressStyle() string
}

// 平台支持发送文件接口
type FileSender interface {
	SendFile(ctx context.Context, replyCtx any, file FileAttachment) error
}

// 平台支持发送音频接口
type ImageSender interface {
	SendImage(ctx context.Context, replyCtx any, img ImageAttachment) error
}

// ======================== Platform - Card ========================

// 被平台调用来原地渲染一个卡片 (e.g. Feishu card.action.trigger callback)
// action string 像"nav:/model" or "act:/model 3"的前缀
type CardNavigationHandler func(action string, sessionKey string) *Card

// 可选interface用于平台支持原地card navigation(更新当前卡片而不是发送新消息)
type CardNavigable interface {
	SetCardNavigationHandler(h CardNavigationHandler)
}

// ======================== Platform - Message ========================

// 当新消息到达时被platforms调用
type MessageHandler func(p Platform, msg *Message)

// ======================== Agent ========================

// 抽象一个AI coding 助手
// 所有agents必须通过StartSession支持双向持久会话
type Agent interface {
	Name() string
	// 创建或回复一个交互session
	StartSession(ctx context.Context, sessionID, agentName string) (AgentSession, error)
	// 返回代理后端已知的会话
	ListSessions() ([]AgentSessionInfo, error)
	Stop() error
}

// 表示一个具有持久进程的正在运行的交互式代理会话
type AgentSession interface {
	// 发送用户信息(可带有图片和文件)来运行agent 进程
	Send(prompt string, images []ImageAttachment, files []FileAttachment) error
	// 给agent进程返回一个决策权限
	RespondPermission(requestID string, result PermissionResult) error
	// 返回触发agent事件的channel (在truns之间保持打开)
	Events() <-chan Event
	// 返回当前agent端的会话ID
	CurrentSessionID() string
	// 如果underlying 进程仍然运行则返回true
	Alive() bool
	// 关闭会话和底层进程
	Close() error
}

// 用于agent接收 per-session 环境变量(如 TC_PROJECT, TC_SESSION_KEY)
type SessionEnvInjector interface {
	SetSessionEnv(env []string)
}

// 通过本地文件（e.g. .claude/cmmands/*.md）暴露自定义/ 命令
// agent 扫描返回的路径中的*.md文件，注册/命令
type CommandProvider interface {
	CommandDirs() []string
}

// 一个可选的接口用于agents支持运行时工作路径切换，路径更改生效与下次session开始
// 当前运行的session自动地被engine关闭
type WorkDirSwitcher interface {
	SetWorkDir(dir string)
	GetWorkDir() string
}

// agent接口:压缩一个运行session中的上下文, CompressCommand 返回原生的 / 命令 /compact /compress
type ContextCompressor interface {
	CompressCommand() string
}

// 描述一个可选的模型
type ModelOption struct {
	Name  string // 传递给CLI的模型名称
	Desc  string // 简单描述 (展示或空)
	Alias string // 可选的短别名 用于 /model 命令(如"codex" for "gpt-5.3-codex")
}

// 运行时模型切换, 生效于下次会话 (当前会话保持其模型)
type ModelSwitcher interface {
	SetModel(model string)
	GetModel() string
	// 尝试从provider API fetch Model
	// 若失败, 回退到build-in list
	AvailableModels(ctx context.Context) []ModelOption
}

// 用于展示的权限
type PermissionModeInfo struct {
	Key    string
	Name   string
	NameZh string
	Desc   string
	DescZh string
}

// 可选接口用于agents支持删除sessions
type SessionDeleter interface {
	DeleteSession(sessionID string) error
}

// agent 运行时权限切换
type ModeSwitcher interface {
	SetMode(mode string)
	GetMode() string
	PermissionModes() []PermissionModeInfo
}

// 可选接口用于运行一个无需重启进程就能应用mode 改变的agent sessions
type LiveModeSwitcher interface {
	SetLiveMode(mode string) bool
}

// 表示用户对权限请求的决定 allow/deny
type PermissionResult struct {
	Behavior     string         `json:"behavior"`               // allow or deny
	UpdatedInput map[string]any `json:"updatedInput,omitempty"` // 允许回传
	Message      string         `json:"message,omitempty"`      // 拒绝原因
}

// 给agent提供 持久化指令文件（如CLAUDE.md、AGENTS.md、GEMINI.md等）。引擎使用这些路径执行/memory命令。
type MemoryFileProvider interface {
	ProjectMemoryFile() string // project-level instruction file (e.g., <work_dir>/CLAUDE.md)
	GlobalMemoryFile() string  // user-level instruction file (e.g., ~/.claude/CLAUDE.md)
}

// 返回系统提示此框架,指示agent tc-connect的能力(定时任务等)
// 该提示词设计于添加到agent已有的系统提示词上
func AgentSystemPrompt() string {
	return `You are running inside tc-connect, a bridge that connects you to messaging platforms.
Your normal text responses are automatically delivered to the user — just reply normally, do NOT use tc-connect send for ordinary text replies.

## Available tools

### Send generated images or files back to the user
When you generate a local image or file that should be sent to the user, use:

  tc-connect send --image /absolute/path/to/image.png
  tc-connect send --file /absolute/path/to/report.pdf
  tc-connect send --file /absolute/path/to/report.pdf --image /absolute/path/to/chart.png

You may repeat --image / --file multiple times. Use this only for generated attachments that need to be delivered to the user.
If you include --message, do not repeat the exact same sentence again in your normal reply, because your normal reply is also delivered automatically.

### Scheduled tasks (cron)
When the user asks you to do something on a schedule (e.g. "每天早上6点帮我总结GitHub trending"), use the Bash tool to run:

  tc-connect cron add --cron "<min> <hour> <day> <month> <weekday>" --prompt "<task description>" --desc "<short label>"

Environment variables TC_PROJECT and TC_SESSION_KEY are already set, so you do NOT need to specify --project or --session-key.

Optional flags:
  --session-mode <mode>     reuse (default) or new-per-run (fresh session each trigger)
  --timeout-mins <n>        max wait per run in minutes (default 30, 0 = unlimited)
  --exec <command>          run a shell command directly instead of --prompt

Examples:
  tc-connect cron add --cron "0 6 * * *" --prompt "Collect GitHub trending repos and send a summary" --desc "Daily GitHub Trending"
  tc-connect cron add --cron "0 9 * * 1" --prompt "Generate a weekly project status report" --desc "Weekly Report"
  tc-connect cron add --cron "*/2 * * * *" --exec "ipconfig" --session-mode new-per-run --desc "Every 2 min ipconfig"

You can also list or delete cron jobs:
  tc-connect cron list
  tc-connect cron del <job-id>

### Bot-to-bot relay
When you need to communicate with another bot (e.g. ask another AI agent a question), use:

  tc-connect relay send --to <target_project> "<message>"

IMPORTANT: <target_project> must be the EXACT project name from the /bind command output.
Do NOT guess or modify the name — use it exactly as shown (e.g. "gemini", not "gemini-bot").

This sends a message to the target bot and waits for its response (printed to stdout).
The conversation is visible in the group chat and each bot maintains its own relay session.

Environment variables TC_PROJECT and TC_SESSION_KEY are already set, so the relay knows which group chat to use.
`
}
