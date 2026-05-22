package core

import "context"

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
// 处理开始时调用StartTyping，并返回一个停止函数，调用者必须在处理结束时调用该函数。
type TypingIndicator interface {
	StartTyping(ctx context.Context, replyCtx any) (stop func())
}

// 平台实现可以更新消息的接口
type MessageUpdater interface {
	UpdateMessage(ctx context.Context, replyCtx any, content string) error
}

// expose 一种用于中间进度渲染的首选样式。
// 典型值：“legacy”、“compact”、“card”。
type ProgressStyleProvider interface {
	ProgressStyle() string
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
	StartSession(ctx context.Context, sessionID string) (AgentSession, error)
	// 返回代理后端已知的会话
	ListSessions(ctx context.Context) ([]AgentSessionInfo, error)
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

// 用于agent接收 per-session 环境变量(如 CC_PROJECT, CC_SESSION_KEY)
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

// 压缩一个运行session中的上下文, CompressCommand 返回原生的 / 命令 /compact /compress
type ContextCompressor interface {
	CompressCommand() string
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
	DeleteSession(ctx context.Context, sessionID string) error
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

