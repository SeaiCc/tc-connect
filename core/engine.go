package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

)

const listPageSize = 20

const maxPlatformMessageLen = 4000
const maxQueuedMessages = 5 // 限制排队消息的数量以控制内存使用

// main启动时设置以便/version工作
var VersionInfo string

// semvar tag, 由main设置 (如 "v1.2.0-beta.1")
var CurrentVersion string

// RestartCh is signaled when /restart is invoked. main listens on it
// to perform a graceful shutdown followed by syscall.Exec.
var RestartCh = make(chan RestartRequest, 1)

// 慢-op阈值, 操作超出阈值会产生slog.Warn 以便快速找出瓶颈
const (
	slowPlatformSend    = 2 * time.Second  // 平台响应,发送
	slowAgentStart      = 5 * time.Second  // agent.StartSession
	slowAgentClose      = 3 * time.Second  // agentSession.Close
	slowAgentSend       = 2 * time.Second  // agentSession.Send
	slowAgentFirstEvent = 15 * time.Second // 从发送第一次agent event开始的计时
)

// 当session繁忙时hold到达的消息, 排队时不发送到agent的stdin,
// 时间循环在当前turn完成后发送它以避免trun当中的干扰
type queuedMessage struct {
	replyCtx      any
	platform      Platform
	content       string
	images        []ImageAttachment
	files         []FileAttachment
	fromVoice     bool
	userID        string
	msgPlatform   string // 用于发送injection的平台名称
	msgSessionKey string // 用于提取chatID的session key
}

// 追踪一个运行的agent sesion 及其 权限状态
type interactiveState struct {
	agentSession AgentSession
	platform     Platform
	replyCtx     any
	workspaceDir string

	mu      sync.Mutex
	stopCh  chan struct{}
	stopped bool

	pending         *pendingPermission
	pendingMessages []queuedMessage // session繁忙时排队
	approveAll      bool            // 当为true时, 自动统一所有的权限请求

	fromVoice bool // 是否未语音来源
	sideText  string

	deleteMode *deleteModeState

	lastAutoCompressAt     time.Time // 上次压缩时间
	lastAutoCompressTokens int       // 上次压缩数量
}

func (s *interactiveState) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

// 标记为关闭
func (s *interactiveState) markStopped() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return // 已经关闭，返回
	}
	s.stopped = true
	if s.stopCh == nil {
		// 创建一个chan
		s.stopCh = make(chan struct{})
	}
	// close需要传入一个chan
	close(s.stopCh)
}

func (s *interactiveState) stopSignal() <-chan struct{} {
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

// 表示等待用户返回的权限请求
type pendingPermission struct {
	RequestID       string
	ToolName        string
	ToolInput       map[string]any
	InputPreview    string
	Questions       []UserQuestion
	Answers         map[int]string
	CurrentQuestion int
	Resolved        chan struct{}
	resolveOnce     sync.Once
}

// 安全地关闭一次channel
func (pp *pendingPermission) resolve() {
	pp.resolveOnce.Do(func() { close(pp.Resolved) })
}

type deleteModeState struct {
	page        int
	selectedIDs map[string]struct{}
	phase       string
	hint        string
	result      string
}

type DisplayCfg struct {
	ThinkingMessages bool
	ThinkingMaxLen   int // thing语言的最大runes数量; 0 = 不截断
	ToolMaxLen       int // tool使用预览的最大runes数量; 0 = 不截断
	ToolMessages     bool
}

// 描述一个可配置的运行时参数
type configItem struct {
	key     string
	desc    string // en description
	descZh  string // zh description
	getFunc func() string
	setFunc func(string) error
}

func (ci configItem) description(isZh bool) string {
	if isZh && ci.descZh != "" {
		return ci.descZh
	}
	return ci.desc
}

// 用于在平台和agent之间路由数据
type Engine struct {
	name         string
	agent        Agent
	platform     Platform
	sessions     *SessionManager
	ctx          context.Context
	cancel       context.CancelFunc
	i18n         *I18n
	display      DisplayCfg
	injectSender bool
	startedAt    time.Time

	displaySaveFunc func(thinkingMessages *bool, thinkingMaxLen, toolMaxLen *int, toolMessages *bool) error

	cronScheduler *CronScheduler

	commands *CommandRegistry
	aliases  map[string]string // trigger -> command (帮助 -> /help)
	aliasMu  sync.RWMutex

	bannedWords []string
	bannedMu    sync.RWMutex

	streamPreview    StreamPreviewCfg
	eventIdleTimeout time.Duration
	baseWorkDir      string
	projectState     *ProjectStateStore

	// 自动压缩
	autoCompressEnabled   bool
	autoCompressMaxTokens int
	autoCompressMinGap    time.Duration // 两次压缩最小时间
	resetOnIdle           time.Duration

	// 为ture时，添加 [ctx: ~N%] (或者模型自己报告) 到assistant响应，展示在平台上
	showContextIndicator bool

	observeEnabled    bool
	observeProjectDir string // ~/.opencode/project/{projectKey}

	// 交互agent session 管理
	interactiveMu     sync.Mutex
	interactiveStates map[string]*interactiveState // key = sessionKey

	userRoles    *UserRoleManager // nil = legacy mode (no per-user policies)
	userRolesMu  sync.RWMutex     // protects userRoles, disabledCmds, and adminFrom

	rateLimiter *RateLimiter
	outgoingRL  *OutgoingRateLimiter
	references  ReferenceRenderCfg

	platformLifecycleMu sync.Mutex
	platformReady       map[Platform]bool
	stopping            bool
}

// RestartRequest carries info needed to send a post-restart notification.
type RestartRequest struct {
	SessionKey string `json:"session_key"`
	Platform   string `json:"platform"`
}

func NewEngine(name string, ag Agent, platform Platform, sessionStorePath string, lang Language) *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{
		name:     name, // 项目名
		agent:    ag,
		platform: platform,
		sessions: NewSessionManager(sessionStorePath),
		ctx:      ctx,
		cancel:   cancel,
		i18n:     NewI18n(lang),
		platformReady: make(map[Platform]bool),
		interactiveStates:     make(map[string]*interactiveState),
	}

	// 不检查AgentType 默认为opencode

	// 不使用md文件中定义的 /slash (定制化)
	return e
}

// ======================= Engine 公共方法 =======================

// Engine启动
func (e *Engine) Start() error {
	p := e.platform

	if err := p.Start(e.handleMessage); err != nil {
		slog.Warn("platform start failed", "project", e.name, "platform", p.Name(), "error", err)
		return err
	}
	e.onPlatformReady(p)

	slog.Info("engine started", "project", e.name, "agent", e.agent.Name(), "platform", e.platform.Name())

	return nil
}

// 控制 assistant响应包含 [ctx: ~N%] suffix.
func (e *Engine) SetShowContextIndicator(show bool) {
	e.showContextIndicator = show
}

// 设置根路径
func (e *Engine) SetBaseWorkDir(dir string) {
	e.baseWorkDir = dir
}

// 设置运行状态ProjectStateStore storePath
func (e *Engine) SetProjectStateStore(store *ProjectStateStore) {
	e.projectState = store
}

// 设置语言保存方法
func (e *Engine) SetLanguageSaveFunc(fn func(Language) error) {
	e.i18n.SetSaveFunc(fn)
}

func (e *Engine) SetPlatform(p Platform) {
	e.platform = p
}

// ======================= Engine Helpers =======================

func (e *Engine) SendToSessionWithAttachments(sessionKey, message string, images []ImageAttachment, files []FileAttachment) error {
	// 核心代码
	// if err := e.waitOutgoing(p); err != nil {
	// 	return err
	// }
	// if err := p.Send(e.ctx, replyCtx, message); err != nil {
	// 	return err
	// }
	return nil
}

// 检查内容或者第一个单词匹配别名并替换
func (e *Engine) resolveAlias(content string) string {
	e.aliasMu.RLock()
	defer e.aliasMu.RUnlock()

	if len(e.aliases) == 0 {
		return content
	}

	// 匹配整个内容
	if cmd, ok := e.aliases[content]; ok {
		return cmd
	}

	// 匹配首单词,添加保留args
	parts := strings.SplitN(content, " ", 2)
	if cmd, ok := e.aliases[parts[0]]; ok {
		if len(parts) > 1 {
			return cmd + " " + parts[1]
		}
		return cmd
	}
	return content
}


// ======================= 内部方法 =======================

// 标记一个异步平台就绪,并且每次ready循环初始化平台级别的能力
func (e *Engine) onPlatformReady(p Platform) {
	if !e.markPlatformReady(p) {
		return
	}
	slog.Info("platform ready", "project", e.name, "platform", p.Name())
	e.initPlatformCapabilities(p)
}

// 尝试标记ready
func (e *Engine) markPlatformReady(p Platform) bool {
	e.platformLifecycleMu.Lock()
	defer e.platformLifecycleMu.Unlock()
	// 若正在停止或ctx Err
	if e.stopping || e.ctx.Err() != nil {
		return false
	}
	// 若已经被标记过true
	if e.platformReady[p] {
		return false
	}
	slog.Info("e.platformReady::", "len", len(e.platformReady))
	e.platformReady[p] = true
	return true
}

// 获取删除Mode状态
func (e *Engine) getDeleteModeState(sessionKey string) *deleteModeState {
	e.interactiveMu.Lock()
	state := e.interactiveStates[sessionKey]
	e.interactiveMu.Unlock()
	if state == nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.deleteMode == nil {
		return nil
	}
	cp := &deleteModeState{
		page:        state.deleteMode.page,
		selectedIDs: make(map[string]struct{}, len(state.deleteMode.selectedIDs)),
		phase:       state.deleteMode.phase,
		hint:        state.deleteMode.hint,
		result:      state.deleteMode.result,
	}
	for id := range state.deleteMode.selectedIDs {
		cp.selectedIDs[id] = struct{}{}
	}
	return cp
}

// 删除session Display Name
func (e *Engine) deleteSessionDisplayName(sessions *SessionManager, matched *AgentSessionInfo) string {
	displayName := sessions.GetSessionName(matched.ID)
	if displayName == "" {
		displayName = matched.Summary
	}
	if displayName == "" {
		shortID := matched.ID
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		displayName = shortID
	}
	return displayName
}

// 返回要删除的names
func (e *Engine) deleteModeSelectionNames(sessions *SessionManager, dm *deleteModeState, agentSessions []AgentSessionInfo) []string {
	names := make([]string, 0, len(dm.selectedIDs))
	for i := range agentSessions {
		if _, ok := dm.selectedIDs[agentSessions[i].ID]; ok {
			names = append(names, "- "+e.deleteSessionDisplayName(sessions, &agentSessions[i]))
		}
	}
	return names
}

// 可用时 阻塞 per-platform outgoing 频率限制
func (e *Engine) waitOutgoing(p Platform) error {
	if e.outgoingRL == nil {
		return nil
	}
	return e.outgoingRL.Wait(e.ctx)
}

// 检查是否超出频率限制, 先价差per-user role-base 显示, 然后 回退到全局限制器
func (e *Engine) checkRateLimit(msg *Message) bool {
	e.userRolesMu.RLock()
	urm := e.userRoles
	e.userRolesMu.RUnlock()

	// Try role-specific rate limit first
	if urm != nil {
		// Use userID if available, else fall back to sessionKey for unidentified users.
		// NOTE: sessionKey fallback means anonymous users get separate buckets per
		// session, which is less strict than per-user limiting. Platforms should
		// provide UserID for effective rate limiting.
		rateKey := msg.UserID
		if rateKey == "" {
			rateKey = msg.SessionKey
			slog.Debug("rate limit: no UserID, falling back to sessionKey", "session_key", msg.SessionKey)
		}
		allowed, handled := urm.AllowRate(rateKey)
		if handled {
			return allowed
		}
		// Role has no rate_limit config — fall through to global, keyed by user
	}
	// Global rate limiter
	if e.rateLimiter == nil {
		return true
	}
	// When users config active: key by userID (per-user); otherwise sessionKey (legacy)
	key := msg.SessionKey
	if urm != nil && msg.UserID != "" {
		key = msg.UserID
	}
	return e.rateLimiter.Allow(key)
}

// 返回content中找到的第一个banned单词,或者为""
func (e *Engine) matchBannedWord(content string) string {
	e.bannedMu.RLock()
	defer e.bannedMu.RUnlock()
	if len(e.bannedWords) == 0 {
		return ""
	}
	lower := strings.ToLower(content)
	for _, w := range e.bannedWords {
		if strings.Contains(lower, w) {
			return w
		}
	}
	return ""
}

// 准备sender 识别头 到content 当 injectSender可用 userID 非空
func (e *Engine) buildSenderPrompt(content, userID, platform, sessionKey string) string {
	if !e.injectSender || userID == "" {
		return content
	}
	chatID := extractChannelID(sessionKey)
	return fmt.Sprintf("[tc-connect sender_id=%s platform=%s chat_id=%s]\n%s", userID, platform, chatID, content)
}

// 将用户输入转换为答案文本, 处理button 回调("askq:qIdx:optIdx"), 数字选择("1", "1,3"), 和free text
func (e *Engine) resolveAskQuestionAnswer(q UserQuestion, input string) string {
	input = strings.TrimSpace(input)

	// Handle card button callback: "askq:qIdx:optIdx"
	if strings.HasPrefix(input, "askq:") {
		parts := strings.SplitN(input, ":", 3)
		if len(parts) == 3 {
			if idx, err := strconv.Atoi(parts[2]); err == nil && idx >= 1 && idx <= len(q.Options) {
				return q.Options[idx-1].Label
			}
		}
		// Legacy format "askq:N"
		if len(parts) == 2 {
			if idx, err := strconv.Atoi(parts[1]); err == nil && idx >= 1 && idx <= len(q.Options) {
				return q.Options[idx-1].Label
			}
		}
	}

	// Try numeric index(es)
	if q.MultiSelect {
		parts := strings.FieldsFunc(input, func(r rune) bool { return r == ',' || r == '，' || r == ' ' })
		var labels []string
		allNumeric := true
		for _, p := range parts {
			p = strings.TrimSpace(p)
			idx, err := strconv.Atoi(p)
			if err != nil || idx < 1 || idx > len(q.Options) {
				allNumeric = false
				break
			}
			labels = append(labels, q.Options[idx-1].Label)
		}
		if allNumeric && len(labels) > 0 {
			return strings.Join(labels, ", ")
		}
	} else {
		if idx, err := strconv.Atoi(input); err == nil && idx >= 1 && idx <= len(q.Options) {
			return q.Options[idx-1].Label
		}
	}

	return input
}

// 给AskUserQuestion control_response 构建一个updateInput
func buildAskQuestionResponse(originalInput map[string]any, questions []UserQuestion, collected map[int]string) map[string]any {
	result := make(map[string]any)
	for k, v := range originalInput {
		result[k] = v
	}
	answers := make(map[string]any)
	for idx, ans := range collected {
		answers[strconv.Itoa(idx)] = ans
	}
	result["answers"] = answers
	return result
}

// ======================== 内部方法 核心处理流程 ========================

func (e *Engine) handleMessage(p Platform, msg *Message) {
	// 打印message信息
	slog.Info("message received",
		"platform", msg.Platform, "msg_id", msg.MessageID,
		"session", msg.SessionKey, "user", msg.UserName,
		"content_len", len(msg.Content),
		"has_images", len(msg.Images) > 0,
	)
	// content格式清理
	content := strings.TrimSpace(msg.Content)
	if content == "" && len(msg.Images) == 0 && len(msg.Files) == 0 {
		return
	}

	// 解析别名(意图识别)  帮助 -> /help
	content = e.resolveAlias(content)
	if msg.ExtraContent != "" {
		if content == "" {
			msg.Content = msg.ExtraContent
		} else {
			msg.Content = msg.ExtraContent + "\n" + content
		}
	} else {
		msg.Content = content
	}

	// Rate 频率 限流 限制检查
	if !e.checkRateLimit(msg) {
		slog.Info("message rate limited",
			"session", msg.SessionKey, "user_id", msg.UserID, "user", msg.UserName)
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgRateLimited))
		return
	}

	// 处理 / 命令
	if !strings.HasPrefix(content, "/") {
		if word := e.matchBannedWord(content); word != "" {
			slog.Info("message blocked by banned word", "word", word, "user", msg.UserName)
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgBannedWordBlocked))
			return
		}
	}

	// FIXME: Multi-workspace
	var resolvedWorkspace string

	// 权限申请处理
	if e.handlePendingPermission(p, msg, content) {
		return
	}

	// 选择sessionmanager和agent
	sessions := e.sessions
	agent := e.agent
	interactiveKey := msg.SessionKey
	
	// 尝试锁session
	session := sessions.GetOrCreateActive(msg.SessionKey)
	sessions.UpdateUserMeta(msg.SessionKey, msg.UserName, msg.ChatName)
	if !session.TryLock() {
		// 尝试在运行的trun排队消息 - session is busy
		// 这样当前turn结束后可以立即执行
		if e.queueMessageForBusySession(p, msg, interactiveKey) {
			// 竞争保护：processInteractiveMessageWith 中的耗尽循环可能 
			// 刚刚在我们的 TryLock 失败和队列添加之间完成（会话已解锁）。
			// 重试 TryLock — 如果成功，则没有人在耗尽队列，因此我们必须自己启动一个处理器。
			if session.TryLock() {
				go e.drainOrphanedQueue(session, sessions, interactiveKey, agent, resolvedWorkspace)
			}
			return
		}
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgPreviousProcessing))
		return
	}

	// AutoResetSessionOnIdle
	if rotated := e.maybeAutoResetSessionOnIdle(p, msg, sessions, interactiveKey, session); rotated != nil {
		session = rotated
	}

	// 在启动异步处理器之前，确保存在一个interactiveState条目，
	// 以便在会话启动期间到达的消息可以排队，而不是被丢弃
	e.ensureInteractiveStateForQueueing(interactiveKey, p, msg.ReplyCtx)

	slog.Info("processing message",
		"platform", msg.Platform,
		"user", msg.UserName,
		"session", session.ID,
	)

	// 开始处理 （并发）
	go e.processInteractiveMessageWith(p, msg, session, agent, sessions, interactiveKey, resolvedWorkspace, msg.SessionKey)
}

// 核心交互处理循环, 接收显示agent, ineractiveKey (用于interactiveStates map) 和workDir
// multi-workspace 可以路由到每个工作区agents. ccSessionKey, 当非空时, 使用环境变量的
// CC_SESSION_KEY, 否则使用interactiveKey
func (e *Engine) processInteractiveMessageWith(p Platform, msg *Message, session *Session, agent Agent, sessions *SessionManager, interactiveKey string, workspaceDir string, ccSessionKey string) {
	// session.Unlock 不在此处deffered - 在下面的drain loop 显示调用, 当holding state.mu
	// 关闭"queue is empty"和"session unlocked" 之间的竞争窗口. deffered 回退 确保
	// lock 在 early-return paths 释放
	unlocked := false
	defer func() {
		if !unlocked {
			session.Unlock()
		}
	}()

	if e.ctx.Err() != nil {
		return
	}

	turnStart := time.Now()

	e.i18n.DetectAndSet(msg.Content)
	session.AddHistory("user", msg.Content)

	// 使用agent重写 (multi-workspace mode)
	var agentOverride Agent
	if agent != e.agent {
		agentOverride = agent
	}
	// 获取交互状态 {agentSesison, platform, replyCtx}
	state := e.getOrCreateInteractiveStateWith(interactiveKey, p, msg.ReplyCtx, session, sessions, agentOverride, ccSessionKey)

	// 更新此轮的reply context
	state.mu.Lock()
	state.platform = p
	state.replyCtx = msg.ReplyCtx
	state.mu.Unlock()

	if state.agentSession == nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgFailedToStartAgentSession))
		return
	}

	// 应用per-message 权限mode重写(如 带有 mode = "bypassPermissions"的cron jobs)
	// 仅当override的SetLiveMode成功时 defer restores
	if msg.ModeOverride != "" {
		if switcher, ok := state.agentSession.(LiveModeSwitcher); ok {
			if switcher.SetLiveMode(msg.ModeOverride) {
				defer func() {
					defaultMode := "default"
					if ma, ok := e.agent.(interface{ GetMode() string }); ok {
						if m := ma.GetMode(); m != "" {
							defaultMode = m
						}
					}
					switcher.SetLiveMode(defaultMode)
				}()
			}
		}
	}

	// 打字机效果支持
	// 所有权被转移给processInteractiveEvents，该组件负责在队列消息轮次中管理//停止/重新启动该事件。
	var stopTyping func()
	if ti, ok := p.(TypingIndicator); ok {
		stopTyping = ti.StartTyping(e.ctx, msg.ReplyCtx)
	}
	defer func() {
		// 如果所有权未转移到processInteractiveEvents 停止打字机效果
		// (如 调用之前停止)
		if stopTyping != nil {
			stopTyping()
		}
	}()

	// 清除上一轮留在通道中的所有过期事件。
	// 这可以防止下一个processInteractiveEvents读取在上一个回合已经返回之后推送的旧EventResult。
	drainEvents(state.agentSession.Events())

	promptContent := e.buildSenderPrompt(msg.Content, msg.UserID, msg.Platform, msg.SessionKey)

	sendStart := time.Now()
	state.mu.Lock()
	state.fromVoice = msg.FromVoice
	state.sideText = ""
	state.mu.Unlock()

	// 和processInteractiveEvents并行发送,某些agent 内部阻塞Send直到prompt 关闭
	// (如 ACP session/prompt); 当阻塞时可能触发 EventPermissionRequest - event loop
	// 必须并行运行
	sendDone := make(chan error, 1)
	go func() {
		sendDone <- state.agentSession.Send(promptContent, msg.Images, msg.Files)
	}()

	e.processInteractiveEvents(state, session, sessions, interactiveKey, msg.MessageID, turnStart, stopTyping, sendDone, msg.ReplyCtx)
	if elapsed := time.Since(sendStart); elapsed >= slowAgentSend {
		slog.Warn("slow agent send", "elapsed", elapsed, "session", msg.SessionKey, "content_len", len(msg.Content))
	}
	stopTyping = nil // ownership transferred; prevent defer from double-stopping

	// 防止narrow race: 一条消息可能在processInteractiveEvent 观察空队列 和
	// 在此处返回间排队, (session仍被阻塞, handleMessage的TryLock失败并路由
	// 消息到queueMessageForBusySession), 移除所有这样的孤儿消息
	if e.drainPendingMessages(state, session, sessions, interactiveKey) {
		unlocked = true
	}
}

// 使用e.agent 启动session, ccSessionKey 当非空时 用于CC_SESSION_KEY 环境注入,否则使用sessionKey
func (e *Engine) getOrCreateInteractiveStateWith(sessionKey string, p Platform, replyCtx any, session *Session, sessions *SessionManager, agentOverride Agent, ccSessionKey string) *interactiveState {
	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()

	state, ok := e.interactiveStates[sessionKey]
	if ok && state.agentSession != nil && state.agentSession.Alive() {
		// 验证运行的agent session 是否陪陪当前的active session. /new 和 /switch 之后
		// active sssion 改变, 但是旧agent 进程可能仍存活, 重用它可能发送消息到错误的
		// 对话上下文
		wantID := session.GetAgentSessionID()
		currentID := state.agentSession.CurrentSessionID()
		// Reuse 仅当live 进程匹配 Session expects:
		// - IDs 匹配(与Claude session相同), 或
		// - 进程还没有报告ID (startup, empty want is OK)
		// 一个正确的ID, 重用会保持 --resume 上下文 - recycle (#238)
		needRecycle := currentID != "" && (wantID == "" || wantID != currentID)
		if !needRecycle {
			return state
		}
		// Tear down the stale agent so we start one that matches the Session below.
		// 销毁stale agent 然后启动一个以匹配下面的Sesison
		slog.Info("interactive session mismatch, recycling",
			"session_key", sessionKey,
			"want_agent_session", wantID,
			"have_agent_session", currentID,
		)
		state.markStopped()
		//  同步关闭防止旧进程在新agent启动时继续输出(issue #237) 发生竞争
		e.closeAgentSessionWithTimeout(sessionKey, state.agentSession)
		delete(e.interactiveStates, sessionKey)
		ok = false // 阻止下面读取stale设置
	}

	ccKey := sessionKey
	if ccSessionKey != "" {
		ccKey = ccSessionKey
	}

	// 注入per-session 环境变量, agent子进程可以调用 `tc-connect cron add` 等
	if inj, ok := e.agent.(SessionEnvInjector); ok {
		envVars := []string{
			"TC_PROJECT=" + e.name,
			"TC_SESSION_KEY=" + ccKey,
		}
		if exePath, err := os.Executable(); err == nil {
			binDir := filepath.Dir(exePath)
			if curPath := os.Getenv("PATH"); curPath != "" {
				envVars = append(envVars, "PATH="+binDir+string(filepath.ListSeparator)+curPath)
			} else {
				envVars = append(envVars, "PATH="+binDir)
			}
		}
		inj.SetSessionEnv(envVars)
	}

	// 检查context是否早已取消(如 shutdown/restart 期间)
	if e.ctx.Err() != nil {
		slog.Debug("skipping session start: context canceled", "session_key", sessionKey)
		newState := &interactiveState{platform: p, replyCtx: replyCtx}
		adoptPendingFromPlaceholder(e.interactiveStates[sessionKey], newState)
		state = newState
		e.interactiveStates[sessionKey] = state
		return state
	}

	// Resume 仅当有一个正确保存的agent session ID. 如果 session 未绑定, 强制刷新启动
	// 而不是附加到此工作区中"最新的"CLI会话
	startSessionID := session.GetAgentSessionID()
	isResume := startSessionID != ""
	startAt := time.Now()
	agentSession, err := e.agent.StartSession(e.ctx, startSessionID)
	startElapsed := time.Since(startAt)
	if err != nil {
		// resume/continue 失败 尝试刷新session作为回退
		if startSessionID != "" {
			slog.Error("session resume failed, falling back to fresh session",
				"session_key", sessionKey, "failed_session_id", startSessionID,
				"error", err, "elapsed", startElapsed)
			startAt = time.Now()
			agentSession, err = e.agent.StartSession(e.ctx, "")
			startElapsed = time.Since(startAt)
			if err == nil {
				slog.Info("fresh session started after resume failure",
					"session_key", sessionKey, "elapsed", startElapsed)
			}
		}
		if err != nil {
			slog.Error("failed to start interactive session", "error", err, "elapsed", startElapsed)
			newState := &interactiveState{platform: p, replyCtx: replyCtx}
			adoptPendingFromPlaceholder(e.interactiveStates[sessionKey], newState)
			state = newState
			e.interactiveStates[sessionKey] = state
			return state
		}
	}
	if startElapsed >= slowAgentStart {
		slog.Warn("slow agent session start", "elapsed", startElapsed, "agent", e.agent.Name(), "session_id", startSessionID)
	}

	if newID := agentSession.CurrentSessionID(); newID != "" {
		if session.CompareAndSetAgentSessionID(newID, e.agent.Name()) {
			sessions.Save()
		}
	}

	newState := &interactiveState{
		agentSession: agentSession,
		platform:     p,
		replyCtx:     replyCtx,
	}
	adoptPendingFromPlaceholder(e.interactiveStates[sessionKey], newState)
	state = newState
	e.interactiveStates[sessionKey] = state

	slog.Info("session spawned", "session_key", sessionKey, "agent_session", session.GetAgentSessionID(), "is_resume", isResume, "elapsed", startElapsed)
	return state
}

// 应用outgoing 比率限制和p.Reply
func (e *Engine) replyWithError(p Platform, replyCtx any, content string) error {
	// context 错误
	if err := e.waitOutgoing(p); err != nil {
		slog.Warn("outgoing rate limit: context cancelled", "platform", p.Name(), "error", err)
		return err
	}
	start := time.Now()
	// reply错误
	if err := p.Reply(e.ctx, replyCtx, content); err != nil {
		slog.Error("platform reply failed", "platform", p.Name(), "error", err, "content_len", len(content))
		return err
	}
	// 执行Reply所需时间较长
	if elapsed := time.Since(start); elapsed >= slowPlatformSend {
		slog.Warn("slow platform reply", "platform", p.Name(), "elapsed", elapsed, "content_len", len(content))
	}
	return nil
}

// 使用error logging, slow-operation warnings 和outgoing rate limiting 包装p.Reply
func (e *Engine) reply(p Platform, replyCtx any, content string) {
	_ = e.replyWithError(p, replyCtx, content)
}

// 使用plain text reply 发送返回消息
func (e *Engine) replyWithButtons(p Platform, replyCtx any, content string) {
	if err := e.waitOutgoing(p); err != nil {
		slog.Warn("outgoing rate limit: context cancelled", "platform", p.Name(), "error", err)
		return
	}
	e.reply(p, replyCtx, content)
}

func (e *Engine) processInteractiveEvents(state *interactiveState, session *Session, sessions *SessionManager, sessionKey string, msgID string, turnStart time.Time, stopTypingFn func(), sendDone <-chan error, replyCtx any) {
	var textParts []string
	// textParts 的index: 此前的text已经被发送/展示
	var segmentStart int
	toolCount := 0
	waitStart := time.Now()
	firstEventLogged := false
	triggerAutoCompress := false
	pendingSend := sendDone

	// stopTyping 追踪当前turn的 打字机indicator 这样一个 queue message 可以启动给一个新turn
	stopTyping := stopTypingFn
	defer func() {
		if stopTyping != nil {
			stopTyping()
		}
	}()

	state.mu.Lock()
	workspaceDir := state.workspaceDir
	workspaceRenderer := func(content string) string {
		return e.renderOutgoingContentForWorkspace(state.platform, content, workspaceDir)
	}
	sendWorkspace := func(p Platform, replyCtx any, content string) {
		e.sendForWorkspace(p, replyCtx, content, workspaceDir)
	}
	sendWorkspaceWithError := func(p Platform, replyCtx any, content string) error {
		return e.sendWithErrorForWorkspace(p, replyCtx, content, workspaceDir)
	}
	sp := newStreamPreview(e.streamPreview, state.platform, state.replyCtx, e.ctx, workspaceRenderer)
	cp := newCompactProgressWriter(e.ctx, state.platform, state.replyCtx, e.agent.Name(), e.i18n.CurrentLang(), workspaceRenderer)
	state.mu.Unlock()

	// 交互计时 Idle timeout: 0 = disabled
	var idleTimer *time.Timer
	var idleCh <-chan time.Time
	if e.eventIdleTimeout > 0 {
		idleTimer = time.NewTimer(e.eventIdleTimeout)
		defer idleTimer.Stop()
		idleCh = idleTimer.C
	}

	// 处理event 和 stop 信号
	events := state.agentSession.Events()
	stopCh := state.stopSignal()
	for {
		var event Event
		var ok bool

		select {
		case <-stopCh:
			sp.discard()
			return
		case event, ok = <-events:
			if !ok {
				goto channelClosed
			}
		case err := <-pendingSend:
			pendingSend = nil
			if err != nil {
				slog.Error("failed to send prompt", "error", err, "session_key", sessionKey)
				sp.discard()
				if stopTyping != nil {
					stopTyping()
					stopTyping = nil
				}
				e.notifyDroppedQueuedMessages(state, err)
				if state.agentSession == nil || !state.agentSession.Alive() {
					e.cleanupInteractiveState(sessionKey, state)
				}
				state.mu.Lock()
				p := state.platform
				state.mu.Unlock()
				e.send(p, replyCtx, fmt.Sprintf(e.i18n.T(MsgError), err))
				return
			}
			continue
		case <-idleCh:
			slog.Error("agent session idle timeout: no events for too long, killing session",
				"session_key", sessionKey, "timeout", e.eventIdleTimeout, "elapsed", time.Since(turnStart))
			cp.Finalize(ProgressCardStateFailed)
			sp.discard()
			state.mu.Lock()
			p := state.platform
			state.mu.Unlock()
			e.send(p, replyCtx, fmt.Sprintf(e.i18n.T(MsgError), "agent session timed out (no response)"))
			e.cleanupInteractiveState(sessionKey, state)
			return
		case <-e.ctx.Done():
			return
		}

		if state.isStopped() {
			sp.discard()
			return
		}

		// 接收到一个event之后重置idle timer
		if idleTimer != nil {
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(e.eventIdleTimeout)
		}

		// 首次事件时间判断
		if !firstEventLogged {
			firstEventLogged = true
			if elapsed := time.Since(waitStart); elapsed >= slowAgentFirstEvent {
				slog.Warn("slow agent first event", "elapsed", elapsed, "session", sessionKey, "event_type", event.Type)
			}
		}

		state.mu.Lock()
		p := state.platform
		state.mu.Unlock()

		switch event.Type {
		case EventThinking:
			if e.display.ThinkingMessages && event.Content != "" {
				// 刷新思考显示前的累积文本段
				previewActive := sp.canPreview()
				if len(textParts) > segmentStart {
					if !previewActive {
						segment := strings.Join(textParts[segmentStart:], "")
						if segment != "" {
							for _, chunk := range splitMessage(segment, maxPlatformMessageLen) {
								sendWorkspace(p, replyCtx, chunk)
							}
						}
					}
					segmentStart = len(textParts)
				}
				sp.freeze()
				if previewActive {
					sp.detachPreview() // 将冻结预览作为永久消息保持可见
				}
				preview := truncateIf(event.Content, e.display.ThinkingMaxLen)
				thinkingMsg := fmt.Sprintf(e.i18n.T(MsgThinking), preview)
				if !cp.AppendEvent(ProgressEntryThinking, preview, "", thinkingMsg) {
					sendWorkspace(p, replyCtx, thinkingMsg)
				}
			}

		// TODO: ToolUse ToolResult
		case EventText:
			if event.Content != "" {
				textParts = append(textParts, event.Content)
				if sp.canPreview() {
					sp.appendText(event.Content)
				}
			}
			if event.SessionID != "" {
				if session.CompareAndSetAgentSessionID(event.SessionID, e.agent.Name()) {
					pendingName := session.GetName()
					if pendingName != "" && pendingName != "session" && pendingName != "default" {
						sessions.SetSessionName(event.SessionID, pendingName)
					}
					sessions.Save()
				}
			}

		case EventPermissionRequest:
			// PermissionRequest -> AskUserQuestion ??
			isAskQuestion := event.ToolName == "AskUserQuestion" && len(event.Questions) > 0

			state.mu.Lock()
			// autoApprove := state.approveAll
			autoApprove := false
			state.mu.Unlock()

			if autoApprove && !isAskQuestion {
				slog.Debug("auto-approving (approve-all)", "request_id", event.RequestID, "tool", event.ToolName)
				_ = state.agentSession.RespondPermission(event.RequestID, PermissionResult{
					Behavior:     "allow",
					UpdatedInput: event.ToolInputRaw,
				})
				continue
			}

			// flush permission prompt 之前的累积文本段落
			previewActive := sp.canPreview()
			if len(textParts) > segmentStart {
				if !previewActive {
					segment := strings.Join(textParts[segmentStart:], "")
					if segment != "" {
						for _, chunk := range splitMessage(segment, maxPlatformMessageLen) {
							sendWorkspace(p, replyCtx, chunk)
						}
					}
				}
				segmentStart = len(textParts)
			}
			sp.freeze()
			if previewActive {
				sp.detachPreview() // 将冻结预览作为永久消息保持可见
			}

			slog.Info("permission request",
				"request_id", event.RequestID,
				"tool", event.ToolName,
			)

			pending := &pendingPermission{
				RequestID:    event.RequestID,
				ToolName:     event.ToolName,
				ToolInput:    event.ToolInputRaw,
				InputPreview: event.ToolInput,
				Questions:    event.Questions,
				Resolved:     make(chan struct{}),
			}
			state.mu.Lock()
			state.pending = pending
			state.mu.Unlock()

			if isAskQuestion {
				e.sendAskQuestionPrompt(p, replyCtx, event.Questions, 0)
			} else {
				permLimit := e.display.ToolMaxLen
				if permLimit > 0 {
					permLimit = permLimit * 8 / 5
				}
				toolInput := truncateIf(event.ToolInput, permLimit)
				prompt := fmt.Sprintf(e.i18n.T(MsgPermissionPrompt), event.ToolName, toolInput)
				e.send(p, replyCtx, prompt)
			}

			// 当等待用户permission 响应是停止idle计时器, 用户可能需要化较长时间决定,
			// 防止因为超时kill会话
			if idleTimer != nil {
				idleTimer.Stop()
			}

			<-pending.Resolved
			slog.Info("permission resolved", "request_id", event.RequestID)

			// 重启计时器
			if idleTimer != nil {
				idleTimer.Reset(e.eventIdleTimeout)
			}

		case EventResult:
			cp.Finalize(ProgressCardStateCompleted)
			// 使用 state.agentSession.CurrnetSessionID() 而不是 event.SessionID
			// event.SessionID 可能在某些情况下为空,造成agent_session_id 不被持久化到磁盘
			// 打断下次重启时的恢复
			if state != nil && state.agentSession != nil {
				if currentID := state.agentSession.CurrentSessionID(); currentID != "" {
					session.SetAgentSessionID(currentID, e.agent.Name())
					sessions.Save()
				}
			}

			fullResponse := event.Content
			if fullResponse == "" && len(textParts) > 0 {
				fullResponse = strings.Join(textParts, "")
			}
			if fullResponse == "" {
				fullResponse = e.i18n.T(MsgEmptyResponse)
			}

			// Context 使用指示器: 优先SDK tokens -> self-reported
			sdkPlausible := event.InputTokens >= 100
			selfPct := parseSelfReportedCtx(fullResponse)
			cleanResponse := ctxSelfReportRe.ReplaceAllString(fullResponse, "")
			cleanResponse = strings.TrimRight(cleanResponse, "\n ")

			// 评估自动压缩trigger(基于user+assistant文本的token estimate, 包括当前turn的
			// 在添加到history之前的assistant 响应)
			if e.autoCompressEnabled && e.autoCompressMaxTokens > 0 {
				estimate := estimateTokensWithPendingAssistant(session.GetHistory(0), cleanResponse)
				now := time.Now()
				state.mu.Lock()
				last := state.lastAutoCompressAt
				state.mu.Unlock()
				if estimate >= e.autoCompressMaxTokens && (last.IsZero() || now.Sub(last) >= e.autoCompressMinGap) {
					triggerAutoCompress = true
					state.mu.Lock()
					state.lastAutoCompressTokens = estimate
					state.mu.Unlock()
				}
			}

			session.AddHistory("assistant", cleanResponse)
			sessions.Save()

			if e.showContextIndicator {
				if sdkPlausible {
					cleanResponse += contextIndicator(event.InputTokens)
				} else if selfPct > 0 {
					cleanResponse += fmt.Sprintf("\n[ctx: ~%d%%]", selfPct)
				}
			}
			fullResponse = cleanResponse

			turnDuration := time.Since(turnStart)
			slog.Info("turn complete",
				"session", session.ID,
				"agent_session", session.GetAgentSessionID(),
				"msg_id", msgID,
				"tools", toolCount,
				"response_len", len(fullResponse),
				"turn_duration", turnDuration,
				"input_tokens", event.InputTokens,
				"output_tokens", event.OutputTokens,
			)

			replyStart := time.Now()
			normalizedResponse := strings.TrimSpace(fullResponse)
			state.mu.Lock()
			suppressDuplicate := normalizedResponse != "" && normalizedResponse == state.sideText
			state.sideText = ""
			state.mu.Unlock()

			// When tool calls happened and prior text was already surfaced in segments,
			// only send the unsent remainder. When tool progress is hidden, tool events don't surface
			// side-channel messages and segmentStart stays 0, so keep normal finalize flow.
			if toolCount > 0 && segmentStart > 0 {
				sp.discard()
				if segmentStart < len(textParts) {
					unsent := strings.Join(textParts[segmentStart:], "")
					if unsent != "" {
						for _, chunk := range splitMessage(unsent, maxPlatformMessageLen) {
							if err := sendWorkspaceWithError(p, replyCtx, chunk); err != nil {
								return
							}
						}
					}
				}
			} else if suppressDuplicate {
				sp.discard()
				slog.Debug("EventResult: suppressed duplicate side-channel text", "response_len", len(fullResponse))
			} else if sp.finish(fullResponse) {
				slog.Debug("EventResult: finalized via stream preview", "response_len", len(fullResponse))
			} else {
				slog.Debug("EventResult: sending via p.Send (preview inactive or failed)", "response_len", len(fullResponse), "chunks", len(splitMessage(fullResponse, maxPlatformMessageLen)))
				for _, chunk := range splitMessage(fullResponse, maxPlatformMessageLen) {
					if err := sendWorkspaceWithError(p, replyCtx, chunk); err != nil {
						return
					}
				}
			}

			if elapsed := time.Since(replyStart); elapsed >= slowPlatformSend {
				slog.Warn("slow final reply send", "platform", p.Name(), "elapsed", elapsed, "response_len", len(fullResponse))
			}

			//TODO: TTS

			// 结束一个turn 发送任意queued 消息之前自动压缩
			if triggerAutoCompress {
				compressor, ok := e.agent.(ContextCompressor)
				if ok && compressor.CompressCommand() != "" {
					if pendingSend != nil {
						if err := <-pendingSend; err != nil {
							slog.Debug("async send error before compress", "error", err)
						}
					}
					state.mu.Lock()
					state.lastAutoCompressAt = time.Now()
					tokenEst := state.lastAutoCompressTokens
					state.mu.Unlock()
					slog.Info("auto-compress: triggering", "session", sessionKey)

					// 压缩前通知用户
					compressNotice := e.i18n.T(MsgCompressing)
					if tokenEst > 0 {
						compressNotice = fmt.Sprintf("%s (~%dk tokens)", compressNotice, tokenEst/1000)
					}
					e.send(state.platform, state.replyCtx, compressNotice)

					// 若session 仍被锁,运行inline compress
					e.runCompress(state, session, sessions, sessionKey, state.platform, state.replyCtx, true)
					return
				}
			}

			// 检查排队消息 - 如果存在,继续时间循环而不是返回
			state.mu.Lock()
			if len(state.pendingMessages) > 0 {
				queued := state.pendingMessages[0]
				state.pendingMessages = state.pendingMessages[1:]
				remainingQueue := len(state.pendingMessages)
				state.platform = queued.platform
				state.replyCtx = queued.replyCtx
				state.fromVoice = queued.fromVoice
				state.mu.Unlock()

				// 停止先前的打字机效果
				if stopTyping != nil {
					stopTyping()
					stopTyping = nil
				}
				// 启动typing indicator 用于排队的消息上下文
				if ti, ok := queued.platform.(TypingIndicator); ok {
					stopTyping = ti.StartTyping(e.ctx, queued.replyCtx)
				}

				// 启动新turn之前, 先处理到过时的时间. 在// EventResult和Send()之间，
				// 唯一缓冲的事件将是过时的剩余事件（例如，来自cmd.Wait()的延迟EventError）。
				drainEvents(state.agentSession.Events())

				if pendingSend != nil {
					if err := <-pendingSend; err != nil {
						slog.Debug("async send error before queued turn", "error", err)
					}
				}

				queuedPrompt := e.buildSenderPrompt(queued.content, queued.userID, queued.msgPlatform, queued.msgSessionKey)

				nextSend := make(chan error, 1)
				go func() {
					nextSend <- state.agentSession.Send(queuedPrompt, queued.images, queued.files)
				}()
				pendingSend = nextSend

				// 立即检测语言（从队列时间推迟，以避免在前一轮仍在运行时切换区域设置）。
				e.i18n.DetectAndSet(queued.content)

				// 给下一次trun 重置 per-turrn
				textParts = nil
				segmentStart = 0
				toolCount = 0
				turnStart = time.Now()
				firstEventLogged = false
				waitStart = time.Now()
				queuedRenderer := func(content string) string {
					return e.renderOutgoingContentForWorkspace(queued.platform, content, workspaceDir)
				}
				sp = newStreamPreview(e.streamPreview, queued.platform, queued.replyCtx, e.ctx, queuedRenderer)
				cp = newCompactProgressWriter(e.ctx, queued.platform, queued.replyCtx, e.agent.Name(), e.i18n.CurrentLang(), queuedRenderer)

				session.AddHistory("user", queued.content)

				if idleTimer != nil {
					if !idleTimer.Stop() {
						select {
						case <-idleTimer.C:
						default:
						}
					}
					idleTimer.Reset(e.eventIdleTimeout)
				}

				slog.Info("processing queued message",
					"session", sessionKey,
					"remaining_queue", remainingQueue,
				)
				continue
			}
			state.mu.Unlock()

			if pendingSend != nil {
				if err := <-pendingSend; err != nil {
					slog.Debug("async send error after EventResult", "error", err)
				}
			}
			return

		case EventError:
			cp.Finalize(ProgressCardStateFailed)
			sp.discard()
			if event.Error != nil {
				slog.Error("agent error", "error", event.Error)
				e.send(p, replyCtx, fmt.Sprintf(e.i18n.T(MsgError), event.Error))
			}
			// Only drop queued messages if the agent session is dead.
			// Some agents (e.g. Codex) emit EventError for per-turn failures
			// while keeping the session alive for subsequent turns.
			if state.agentSession == nil || !state.agentSession.Alive() {
				e.notifyDroppedQueuedMessages(state, event.Error)
			}
			return
		}
	}

channelClosed:
	// Channel closed - process exited unexpectedly
	slog.Warn("agent process exited", "session_key", sessionKey)
	e.notifyDroppedQueuedMessages(state, fmt.Errorf("agent process exited"))
	e.cleanupInteractiveState(sessionKey, state)

	if len(textParts) > 0 {
		state.mu.Lock()
		p := state.platform
		state.mu.Unlock()

		fullResponse := strings.Join(textParts, "")
		session.AddHistory("assistant", fullResponse)

		if toolCount > 0 && segmentStart > 0 {
			sp.discard()
			if segmentStart < len(textParts) {
				unsent := strings.Join(textParts[segmentStart:], "")
				if unsent != "" {
					for _, chunk := range splitMessage(unsent, maxPlatformMessageLen) {
						if err := sendWorkspaceWithError(p, replyCtx, chunk); err != nil {
							return
						}
					}
				}
			}
		} else if sp.finish(fullResponse) {
			slog.Debug("stream preview: finalized in-place (process exited)")
		} else {
			for _, chunk := range splitMessage(fullResponse, maxPlatformMessageLen) {
				if err := sendWorkspaceWithError(p, replyCtx, chunk); err != nil {
					return
				}
			}
		}
	}
}

// ======================= 内部方法 Card navigation =======================

func (e *Engine) initPlatformCapabilities(p Platform) {
	// 注册card 处理方法
	if nav, ok := p.(CardNavigable); ok {
		nav.SetCardNavigationHandler(e.handleCardNav)
	}
}

// handleCardNav is called by platforms that support in-place card updates.
// It routes nav: and act: prefixed actions to the appropriate render function.
func (e *Engine) handleCardNav(action string, sessionKey string) *Card {
	var prefix, body string
	if i := strings.Index(action, ":"); i >= 0 {
		prefix = action[:i]
		body = action[i+1:]
	} else {
		return nil
	}

	cmd, args := body, ""
	// body中找' '位置 拆分cmd args
	if i := strings.IndexByte(body, ' '); i >= 0 {
		cmd = body[:i]
		args = strings.TrimSpace(body[i+1:])
	}
	// 如果前缀为act,先执行 executeCardAction
	if prefix == "act" { // 如 act:/lang
		e.executeCardAction(cmd, args, sessionKey)
	}

	switch cmd {
	case "/help":
		return e.renderHelpGroupCard(args)
	case "/model": // NOT SUPPORT
		slog.Error("engine:: not support /model")
		return nil
	case "/reasoning": // NOT SUPPORT
		slog.Error("engine:: not support /reasoning")
		return nil
	case "/mode":
		return e.renderModeCard()
	case "/lang":
		return e.renderLangCard()
	case "/status":
		return e.renderStatusCard(sessionKey, extractUserID(sessionKey))
	case "/list":
		page := 1
		if args != "" {
			if n, err := strconv.Atoi(args); err == nil && n > 0 {
				page = n
			}
		}
		return e.renderListCardSafe(sessionKey, page)
	case "/dir": // NOT SUPPORT
		slog.Error("engine:: not support /dir")
		return nil
	case "/current":
		return e.renderCurrentCard(sessionKey)
	case "/history":
		return e.renderHistoryCard(sessionKey)
	case "/provider": // NOT SUPPORT
		slog.Error("engine:: not support /provider")
		return nil
	case "/cron":
		return e.renderCronCard(sessionKey, extractUserID(sessionKey))
	case "/heartbeat": // NOT SUPPORT
		slog.Error("engine:: do not support /heartbeat")
		return nil
	case "/commands":
		return e.renderCommandsCard()
	case "/alias":
		return e.renderAliasCard()
	case "/config":
		return e.renderConfigCard()
	case "/skills": // NOT SUPPORT
		slog.Error("engine:: not support /skills")
		return nil
	case "/doctor":
		return e.renderDoctorCard()
	case "/whoami":
		return e.renderWhoamiCard(&Message{
			SessionKey: sessionKey,
			UserID:     extractUserID(sessionKey),
			Platform:   extractPlatformName(sessionKey),
		})
	case "/version":
		return e.renderVersionCard()
	case "/new":
		return e.renderCurrentCard(sessionKey)
	case "/switch":
		return e.renderListCardSafe(sessionKey, 1) // ??
	case "/delete-mode":
		if strings.HasPrefix(args, "cancel") {
			return e.renderListCardSafe(sessionKey, 1)
		}
		return e.renderDeleteModeCard(sessionKey)
	case "/stop":
		return e.renderStatusCard(sessionKey, extractUserID(sessionKey))
	case "/upgrade": // develop version NOT SUPPORT
		slog.Error("engine:: do not support /upgrade")
		return nil
	}
	return nil
}

// 在卡片重新渲染之前，对带有前缀的动作执行副作用（例如切换模型/模式/语言）
func (e *Engine) executeCardAction(cmd, args, sessionKey string) {
	switch cmd {
	case "/model": // NOT SUPPORT
		slog.Error("Do not support to switch model")
		return

	case "/reasoning": // NOT SUPPORT
		slog.Error("Do not support to switch reasoning")
		return

	case "/mode":
		if args == "" {
			return
		}
		switcher, ok := e.agent.(ModeSwitcher)
		if !ok {
			return
		}
		newMode := strings.ToLower(args)
		switcher.SetMode(newMode)
		if e.applyLiveModeChange(sessionKey, switcher.GetMode()) {
			e.cleanupInteractiveState(sessionKey)
			return
		}
		e.cleanupInteractiveState(sessionKey)
		// Mode change requires a new session to take effect
		s := e.sessions.GetOrCreateActive(sessionKey)
		s.SetAgentSessionID("", "")
		s.ClearHistory()
		e.sessions.Save()

	case "/lang":
		if args == "" {
			return
		}
		target := strings.ToLower(strings.TrimSpace(args))
		var lang Language
		switch target {
		case "en", "english":
			lang = LangEnglish
		case "zh", "cn", "chinese":
			lang = LangChinese
		case "zh-tw", "zh_tw", "zhtw":
			lang = LangTraditionalChinese
		case "ja", "jp", "japanese":
			lang = LangJapanese
		case "auto":
			lang = LangAuto
		default:
			return
		}
		e.i18n.SetLang(lang)

	case "/provider": // NOT SUPPORT
		slog.Error("Do not support to switch provider")
		return

	case "/new":
		sessions := e.sessions
		e.cleanupInteractiveState(sessionKey)
		sessions.NewSession(sessionKey, "")

	case "/delete-mode":
		// 例: /delete-mode toggle <id>
		e.executeDeleteModeAction(sessionKey, args)

	case "/switch":
		if args == "" {
			return
		}
		agentSessions, err := e.agent.ListSessions(e.ctx)
		if err != nil || len(agentSessions) == 0 {
			return
		}
		// 移除外部CLI创建的session
		agentSessions = filterOwnedSessions(agentSessions, e.sessions.KnownAgentSessionIDs())
		matched := e.matchSession(agentSessions, e.sessions, args)
		if matched == nil {
			return
		}
		// sessionKey?? 当前sessionKey 还是要切换的key
		e.cleanupInteractiveState(sessionKey)
		session := e.sessions.GetOrCreateActive(sessionKey)
		// 设置agentID agentType name
		session.SetAgentInfo(matched.ID, e.agent.Name(), matched.Summary)
		session.ClearHistory()
		e.sessions.Save()

	case "/dir":
		slog.Error("Do not support to change dir ")

	case "/stop":
		e.stopInteractiveSession(sessionKey, nil, nil)

	case "/heartbeat":
		slog.Error("Do not support heartbeat between projects")
		return

	case "/cron":
		if e.cronScheduler == nil || args == "" {
			return
		}
		subArgs := strings.Fields(args)
		if len(subArgs) < 2 {
			return
		}
		sub, id := subArgs[0], subArgs[1]
		switch sub {
		case "enable":
			_ = e.cronScheduler.EnableJob(id)
		case "disable":
			_ = e.cronScheduler.DisableJob(id)
		case "delete":
			e.cronScheduler.RemoveJob(id)
		case "mute":
			e.cronScheduler.Store().SetMute(id, true)
		case "unmute":
			e.cronScheduler.Store().SetMute(id, false)
		}
	}
}

// ======================== Engine 卡片渲染相关 ========================

// 返回到 nav:/help card ??
func (e *Engine) cardBackButton() CardButton {
	return DefaultBtn(e.i18n.T(MsgCardBack), "nav:/help")
}

func (e *Engine) cardPrevButton(action string) CardButton {
	return DefaultBtn(e.i18n.T(MsgCardPrev), action)
}

func (e *Engine) cardNextButton(action string) CardButton {
	return DefaultBtn(e.i18n.T(MsgCardNext), action)
}

func (e *Engine) simpleCard(title, color, content string) *Card {
	return NewCard().Title(title, color).Markdown(content).Buttons(e.cardBackButton()).Build()
}

// 渲染帮助卡片
func (e *Engine) renderHelpGroupCard(groupKey string) *Card {
	sectionTitle := func(key MsgKey) string {
		section := e.i18n.T(key)
		// 找到最后的一行返回
		if idx := strings.IndexByte(section, '\n'); idx >= 0 {
			return section[:idx]
		}
		return section
	}
	tabLabel := func(key MsgKey) string {
		return strings.Trim(sectionTitle(key), "*")
	}
	commandText := func(command string) string {
		return "**" + command + "**  " + e.i18n.T(MsgKey(strings.TrimPrefix(command, "/")))
	}

	groups := helpCardGroups()
	current := groups[0] // session
	normalizedGroup := strings.ToLower(strings.TrimSpace(groupKey))
	// 从全部cardgroup中找到groupKey
	for _, group := range groups {
		if group.key == normalizedGroup {
			current = group
			break
		}
	}
	//
	cb := NewCard().Title(e.i18n.T(MsgHelpTitle), "blue") // help_title
	var tabs []CardButton
	for _, group := range groups {
		btnType := "default"
		if group.key == current.key {
			btnType = "primary"
		}
		tabs = append(tabs, Btn(tabLabel(group.titleKey), btnType, "nav:/help "+group.key))
	}
	for _, row := range splitHelpTabRows(true, tabs) {
		cb.ButtonsEqual(row...)
	}
	for _, item := range current.items {
		cb.ListItem(commandText(item.command), "▶", item.action)
	}
	cb.Note(e.i18n.T(MsgHelpTip))
	return cb.Build()
}

func (e *Engine) renderModeCard() *Card {
	switcher, ok := e.agent.(ModeSwitcher)
	if !ok {
		return e.simpleCard(e.i18n.T(MsgCardTitleMode), "violet", e.i18n.T(MsgModeNotSupported))
	}

	current := switcher.GetMode()
	modes := switcher.PermissionModes()
	zhLike := e.i18n.IsZhLike()

	var sb strings.Builder
	for _, m := range modes {
		marker := "◻"
		if m.Key == current {
			marker = "▶"
		}
		if zhLike {
			sb.WriteString(fmt.Sprintf("%s **%s** — %s\n", marker, m.NameZh, m.DescZh))
		} else {
			sb.WriteString(fmt.Sprintf("%s **%s** — %s\n", marker, m.Name, m.Desc))
		}
	}

	var opts []CardSelectOption
	initVal := ""
	for _, m := range modes {
		label := m.Name
		if zhLike {
			label = m.NameZh
		}
		val := "act:/mode " + m.Key
		opts = append(opts, CardSelectOption{Text: label, Value: val})
		if m.Key == current {
			initVal = val
		}
	}

	cb := NewCard().Title(e.i18n.T(MsgCardTitleMode), "violet").
		Markdown(sb.String()).
		Select(e.i18n.T(MsgModeSelectPlaceholder), opts, initVal).
		Buttons(e.cardBackButton())
	cb.Note(e.modeUsageText(modes))
	return cb.Build()
}

// 切换模式文本
func (e *Engine) modeUsageText(modes []PermissionModeInfo) string {
	keys := make([]string, 0, len(modes))
	for _, mode := range modes {
		keys = append(keys, "`"+mode.Key+"`")
	}
	return e.i18n.Tf(MsgModeUsage, strings.Join(keys, " / "))
}

// 渲染语言卡片
func (e *Engine) renderLangCard() *Card {
	cur := e.i18n.CurrentLang()
	name := langDisplayName(cur)

	langs := []struct{ code, label string }{
		{"en", "English"}, {"zh", "中文"}, {"zh-TW", "繁體中文"},
		{"ja", "日本語"}, {"es", "Español"}, {"auto", "Auto"},
	}
	var opts []CardSelectOption
	initVal := ""
	for _, l := range langs {
		opts = append(opts, CardSelectOption{Text: l.label, Value: "act:/lang " + l.code})
		if string(cur) == l.code || (cur == LangAuto && l.code == "auto") {
			initVal = "act:/lang " + l.code
		}
	}

	return NewCard().
		Title(e.i18n.T(MsgCardTitleLanguage), "wathet").
		Markdown(e.i18n.Tf(MsgLangCurrent, name)).
		Select(e.i18n.T(MsgLangSelectPlaceholder), opts, initVal).
		Buttons(e.cardBackButton()).
		Build()
}

func (e *Engine) renderListCardSafe(sessionKey string, page int) *Card {
	card, err := e.renderListCard(sessionKey, page)
	// 回退simpleCard, 列出session列表
	if err != nil {
		return e.simpleCard(e.i18n.Tf(MsgCardTitleSessions, e.agent.Name(), 0), "red", err.Error())
	}
	return card
}

// 展示session列表
func (e *Engine) renderListCard(sessionKey string, page int) (*Card, error) {
	agentSessions, err := e.agent.ListSessions(e.ctx)
	if err != nil {
		return nil, fmt.Errorf(e.i18n.T(MsgListError), err)
	}
	// 剔除外部CLI创建的session
	agentSessions = filterOwnedSessions(agentSessions, e.sessions.KnownAgentSessionIDs())
	if len(agentSessions) == 0 {
		return e.simpleCard(e.i18n.Tf(MsgCardTitleSessions, e.agent.Name(), 0), "turquoise", e.i18n.T(MsgListEmpty)), nil
	}
	// 分页
	total := len(agentSessions)
	totalPages := (total + listPageSize - 1) / listPageSize
	if page > totalPages {
		page = totalPages
	}

	start := (page - 1) * listPageSize
	end := start + listPageSize

	end = min(end, total)

	agentName := e.agent.Name()
	activeSession := e.sessions.GetOrCreateActive(sessionKey)
	activeAgentID := activeSession.GetAgentSessionID()

	var titleStr string
	if totalPages > 1 {
		titleStr = e.i18n.Tf(MsgCardTitleSessionsPaged, agentName, total, page, totalPages)
	} else {
		titleStr = e.i18n.Tf(MsgCardTitleSessions, agentName, total)
	}

	cb := NewCard().Title(titleStr, "turquoise")
	for i := start; i < end; i++ {
		s := agentSessions[i]
		marker := "◻"
		if s.ID == activeAgentID {
			marker = "▶"
		}
		displayName := e.sessions.GetSessionName(s.ID)
		if displayName != "" {
			displayName = "📌 " + displayName
		} else {
			displayName = strings.ReplaceAll(s.Summary, "\n", " ")
			displayName = strings.Join(strings.Fields(displayName), " ")
			if displayName == "" {
				displayName = e.i18n.T(MsgListEmptySummary)
			}
			if len([]rune(displayName)) > 40 {
				displayName = string([]rune(displayName)[:40]) + "…"
			}
		}
		btnType := "default"
		if s.ID == activeAgentID {
			btnType = "primary"
		}
		cb.ListItemBtn(
			e.i18n.Tf(MsgListItem, marker, i+1, displayName, s.MessageCount, s.ModifiedAt.Format("01-02 15:04")),
			fmt.Sprintf("#%d", i+1),
			btnType,
			fmt.Sprintf("act:/switch %d", i+1),
		)
	}

	var navBtns []CardButton
	if page > 1 {
		navBtns = append(navBtns, e.cardPrevButton(fmt.Sprintf("nav:/list %d", page-1)))
	}
	navBtns = append(navBtns, e.cardBackButton())
	if page < totalPages {
		navBtns = append(navBtns, e.cardNextButton(fmt.Sprintf("nav:/list %d", page+1)))
	}
	cb.Buttons(navBtns...)

	if totalPages > 1 {
		cb.Note(fmt.Sprintf(e.i18n.T(MsgListPageHint), page, totalPages))
	}

	return cb.Build(), nil
}

// 渲染状态卡片,具体条目见 i18n.go MsgStatusTitle
func (e *Engine) renderStatusCard(sessionKey string, userID string) *Card {

	platformStr := e.platform.Name()

	uptimeStr := formatDurationI18n(time.Since(e.startedAt), e.i18n.CurrentLang())

	cur := e.i18n.CurrentLang()
	langStr := fmt.Sprintf("%s (%s)", string(cur), langDisplayName(cur))

	var modeStr string
	if ms, ok := e.agent.(ModeSwitcher); ok {
		mode := ms.GetMode()
		if mode != "" {
			modeStr = e.i18n.Tf(MsgStatusMode, mode)
		}
	}
	thinkingStr := e.i18n.T(MsgDisabledShort)
	if e.display.ThinkingMessages {
		thinkingStr = e.i18n.T(MsgEnabledShort)
	}
	toolStr := e.i18n.T(MsgDisabledShort)
	if e.display.ToolMessages {
		toolStr = e.i18n.T(MsgEnabledShort)
	}
	modeStr += e.i18n.Tf(MsgStatusThinkingMessages, thinkingStr)
	modeStr += e.i18n.Tf(MsgStatusToolMessages, toolStr)

	s := e.sessions.GetOrCreateActive(sessionKey)
	sessionDisplayName := e.sessions.GetSessionName(s.GetAgentSessionID())
	if sessionDisplayName == "" {
		sessionDisplayName = s.GetName()
	}
	sessionStr := e.i18n.Tf(MsgStatusSession, sessionDisplayName, len(s.History))

	var cronStr string
	if e.cronScheduler != nil {
		if jobs := e.cronScheduler.Store().ListBySessionKey(sessionKey); len(jobs) > 0 {
			enabledCount := 0
			for _, j := range jobs {
				if j.Enabled {
					enabledCount++
				}
			}
			cronStr = e.i18n.Tf(MsgStatusCron, len(jobs), enabledCount)
		}
	}

	sessionKeyStr := e.i18n.Tf(MsgStatusSessionKey, sessionKey)

	userIDStr := ""
	if userID != "" {
		userIDStr = e.i18n.Tf(MsgStatusUserID, userID)
	}

	statusText := e.i18n.Tf(MsgStatusTitle,
		e.name,
		e.agent.Name(),
		platformStr,
		uptimeStr,
		langStr,
		modeStr,
		sessionStr,
		cronStr,
		sessionKeyStr,
		userIDStr,
	)
	title, body := splitCardTitleBody(statusText)

	return NewCard().
		Title(title, "green").
		Markdown(body).
		Buttons(e.cardBackButton()).
		Build()
}

// 展示当前session Name agentID 历史对话
func (e *Engine) renderCurrentCard(sessionKey string) *Card {
	s := e.sessions.GetOrCreateActive(sessionKey)
	agentID := s.GetAgentSessionID()
	if agentID == "" {
		agentID = e.i18n.T(MsgSessionNotStarted)
	}
	content := fmt.Sprintf(e.i18n.T(MsgCurrentSession), s.Name, agentID, len(s.History))
	return NewCard().
		Title(e.i18n.T(MsgCardTitleCurrentSession), "turquoise").
		Markdown(content).
		Buttons(e.cardBackButton()).
		Build()
}

// 渲染历史对话消息
func (e *Engine) renderHistoryCard(sessionKey string) *Card {
	s := e.sessions.GetOrCreateActive(sessionKey)
	entries := s.GetHistory(10)

	// OpenCode Agent do not support History provider

	if len(entries) == 0 {
		return e.simpleCard(e.i18n.T(MsgCardTitleHistory), "turquoise", e.i18n.T(MsgHistoryEmpty))
	}

	var sb strings.Builder
	for _, h := range entries {
		icon := "👤"
		if h.Role == "assistant" {
			icon = "🤖"
		}
		content := h.Content
		if len([]rune(content)) > 200 {
			content = string([]rune(content)[:200]) + "..."
		}
		sb.WriteString(fmt.Sprintf("%s [%s]\n%s\n\n", icon, h.Timestamp.Format("15:04:05"), content))
	}

	return NewCard().
		Title(e.i18n.Tf(MsgCardTitleHistoryLast, len(entries)), "turquoise").
		Markdown(sb.String()).
		Buttons(e.cardBackButton()).
		Build()
}

// 渲染定时任务卡片
func (e *Engine) renderCronCard(sessionKey string, userID string) *Card {
	// 判CronScheduler空
	if e.cronScheduler == nil {
		return e.simpleCard(e.i18n.T(MsgCardTitleCron), "orange", e.i18n.T(MsgCronNotAvailable))
	}
	// 判断CronStore中的job是否为空
	jobs := e.cronScheduler.Store().ListBySessionKey(sessionKey)
	if len(jobs) == 0 {
		return e.simpleCard(e.i18n.T(MsgCardTitleCron), "orange", e.i18n.T(MsgCronEmpty))
	}

	lang := e.i18n.CurrentLang()
	now := time.Now()
	// 初始化卡片
	cb := NewCard().Title(e.i18n.T(MsgCardTitleCron), "orange")
	cb.Markdown(fmt.Sprintf(e.i18n.T(MsgCronListTitle), len(jobs)))

	for _, j := range jobs {
		status := "✅"
		// job 状态修改图标
		if !j.Enabled {
			status = "⏸"
		}
		// 获取job描述: Job.Description -> 命令(shell job) -> prompt
		desc := j.Description
		if desc == "" {
			if j.IsShellJob() {
				desc = "🖥 " + truncateStr(j.Exec, 60)
			} else {
				desc = truncateStr(j.Prompt, 60)
			}
		}
		// Mute 加后缀
		if j.Mute {
			desc += " [mute]"
		}
		// 将 min, hour, dom, month, dow 五个字段的表示转化为人类可读形式
		human := CronExprToHuman(j.CronExpr, lang)

		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("%s %s\n", status, desc))
		sb.WriteString(e.i18n.Tf(MsgCronIDLabel, j.ID))
		sb.WriteString(e.i18n.Tf(MsgCronScheduleLabel, human, j.CronExpr))
		nextRun := e.cronScheduler.NextRun(j.ID)
		if !nextRun.IsZero() {
			fmtStr := cronTimeFormat(nextRun, now)
			sb.WriteString(e.i18n.Tf(MsgCronNextRunLabel, nextRun.Format(fmtStr)))
		}
		if !j.LastRun.IsZero() {
			fmtStr := cronTimeFormat(j.LastRun, now)
			sb.WriteString(e.i18n.Tf(MsgCronLastRunLabel, j.LastRun.Format(fmtStr)))
			if j.LastError != "" {
				sb.WriteString(e.i18n.Tf(MsgCronFailedSuffix, truncateStr(j.LastError, 40)))
			}
			sb.WriteString("\n")
		}
		cb.Markdown(sb.String())

		var btns []CardButton
		if j.Enabled {
			btns = append(btns, DefaultBtn(e.i18n.T(MsgCronBtnDisable), fmt.Sprintf("act:/cron disable %s", j.ID)))
		} else {
			btns = append(btns, PrimaryBtn(e.i18n.T(MsgCronBtnEnable), fmt.Sprintf("act:/cron enable %s", j.ID)))
		}
		if j.Mute {
			btns = append(btns, DefaultBtn(e.i18n.T(MsgCronBtnUnmute), fmt.Sprintf("act:/cron unmute %s", j.ID)))
		} else {
			btns = append(btns, DefaultBtn(e.i18n.T(MsgCronBtnMute), fmt.Sprintf("act:/cron mute %s", j.ID)))
		}
		btns = append(btns, DangerBtn(e.i18n.T(MsgCronBtnDelete), fmt.Sprintf("act:/cron delete %s", j.ID)))
		cb.ButtonsEqual(btns...)
	}

	cb.Divider()
	cb.Note(e.i18n.T(MsgCronCardHint))
	cb.Buttons(e.cardBackButton())
	return cb.Build()
}

// 渲染命令卡片
func (e *Engine) renderCommandsCard() *Card {
	cmds := e.commands.ListAll()
	if len(cmds) == 0 {
		return e.simpleCard(e.i18n.T(MsgCardTitleCommands), "purple", e.i18n.T(MsgCommandsEmpty))
	}

	var sb strings.Builder
	sb.WriteString(e.i18n.Tf(MsgCommandsTitle, len(cmds)))
	for _, c := range cmds {
		tag := ""
		if c.Source == "agent" {
			tag = e.i18n.T(MsgCommandsTagAgent)
		} else if c.Exec != "" {
			tag = e.i18n.T(MsgCommandsTagShell)
		}
		desc := c.Description
		if desc == "" {
			if c.Exec != "" {
				desc = "$ " + truncateStr(c.Exec, 60)
			} else {
				desc = truncateStr(c.Prompt, 60)
			}
		}
		sb.WriteString(fmt.Sprintf("/%s%s — %s\n", c.Name, tag, desc))
	}

	return NewCard().Title(e.i18n.T(MsgCardTitleCommands), "purple").
		Markdown(sb.String()).
		Note(e.i18n.T(MsgCommandsHint)).
		Buttons(e.cardBackButton()).
		Build()
}

// 渲染别名卡片(帮助 -> /help)
func (e *Engine) renderAliasCard() *Card {
	e.aliasMu.RLock()
	defer e.aliasMu.RUnlock()

	if len(e.aliases) == 0 {
		return e.simpleCard(e.i18n.T(MsgCardTitleAlias), "purple", e.i18n.T(MsgAliasEmpty))
	}

	names := make([]string, 0, len(e.aliases))
	for n := range e.aliases {
		names = append(names, n)
	}
	sort.Strings(names)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(e.i18n.T(MsgAliasListHeader), len(e.aliases)))
	sb.WriteString("\n")
	for _, n := range names {
		sb.WriteString(fmt.Sprintf("`%s` → `%s`\n", n, e.aliases[n]))
	}

	return NewCard().Title(e.i18n.T(MsgCardTitleAlias), "purple").
		Markdown(sb.String()).
		Buttons(e.cardBackButton()).
		Build()
}

// 渲染卡片配置所需预置文本
func (e *Engine) configItems() []configItem {
	return []configItem{
		{
			key:    "thinking_messages",
			desc:   "Whether thinking messages are shown (true/false)",
			descZh: "是否显示思考消息 (true/false)",
			getFunc: func() string {
				return fmt.Sprintf("%t", e.display.ThinkingMessages)
			},
			setFunc: func(v string) error {
				b, err := strconv.ParseBool(v)
				if err != nil {
					return fmt.Errorf("invalid boolean: %s", v)
				}
				e.display.ThinkingMessages = b
				if e.displaySaveFunc != nil {
					return e.displaySaveFunc(&b, nil, nil, nil)
				}
				return nil
			},
		},
		{
			key:    "thinking_max_len",
			desc:   "Max chars for thinking messages (0=no truncation)",
			descZh: "思考消息最大长度 (0=不截断)",
			getFunc: func() string {
				return fmt.Sprintf("%d", e.display.ThinkingMaxLen)
			},
			setFunc: func(v string) error {
				n, err := strconv.Atoi(v)
				if err != nil {
					return fmt.Errorf("invalid integer: %s", v)
				}
				if n < 0 {
					return fmt.Errorf("value must be >= 0")
				}
				e.display.ThinkingMaxLen = n
				if e.displaySaveFunc != nil {
					return e.displaySaveFunc(nil, &n, nil, nil)
				}
				return nil
			},
		},
		{
			key:    "tool_messages",
			desc:   "Whether tool progress messages are shown (true/false)",
			descZh: "是否显示工具进度消息 (true/false)",
			getFunc: func() string {
				return fmt.Sprintf("%t", e.display.ToolMessages)
			},
			setFunc: func(v string) error {
				b, err := strconv.ParseBool(v)
				if err != nil {
					return fmt.Errorf("invalid boolean: %s", v)
				}
				e.display.ToolMessages = b
				if e.displaySaveFunc != nil {
					return e.displaySaveFunc(nil, nil, nil, &b)
				}
				return nil
			},
		},
		{
			key:    "tool_max_len",
			desc:   "Max chars for tool use messages (0=no truncation)",
			descZh: "工具消息最大长度 (0=不截断)",
			getFunc: func() string {
				return fmt.Sprintf("%d", e.display.ToolMaxLen)
			},
			setFunc: func(v string) error {
				n, err := strconv.Atoi(v)
				if err != nil {
					return fmt.Errorf("invalid integer: %s", v)
				}
				if n < 0 {
					return fmt.Errorf("value must be >= 0")
				}
				e.display.ToolMaxLen = n
				if e.displaySaveFunc != nil {
					return e.displaySaveFunc(nil, nil, &n, nil)
				}
				return nil
			},
		},
	}
}

// 渲染配置卡片
func (e *Engine) renderConfigCard() *Card {
	items := e.configItems()
	isZh := e.i18n.IsZhLike()

	var sb strings.Builder
	sb.WriteString(e.i18n.T(MsgConfigTitle))
	for _, item := range items {
		sb.WriteString(fmt.Sprintf("`%s` = `%s`\n  %s\n\n", item.key, item.getFunc(), item.description(isZh)))
	}

	return NewCard().Title(e.i18n.T(MsgCardTitleConfig), "grey").
		Markdown(sb.String()).
		Note(e.i18n.T(MsgConfigHint)).
		Buttons(e.cardBackButton()).
		Build()
}

// 渲染doctor 卡片
func (e *Engine) renderDoctorCard() *Card {
	results := RunDoctorChecks(e.ctx, e.agent, e.platform)
	report := FormatDoctorResults(results, e.i18n)
	return NewCard().
		Title(e.i18n.T(MsgCardTitleDoctor), "orange").
		Markdown(report).
		Buttons(e.cardBackButton()).
		Build()
}

// 渲染用户信息卡片
func (e *Engine) renderWhoamiCard(msg *Message) *Card {
	userID := msg.UserID
	if userID == "" {
		userID = "(unknown)"
	}

	var body strings.Builder
	body.WriteString(fmt.Sprintf("**User ID:**  `%s`\n", userID))
	if msg.UserName != "" {
		body.WriteString(fmt.Sprintf("**%s:**  %s\n", e.i18n.T(MsgWhoamiName), msg.UserName))
	}
	if msg.Platform != "" {
		body.WriteString(fmt.Sprintf("**%s:**  %s\n", e.i18n.T(MsgWhoamiPlatform), msg.Platform))
	}
	chatID := extractChannelID(msg.SessionKey)
	if chatID != "" {
		body.WriteString(fmt.Sprintf("**Chat ID:**  `%s`\n", chatID))
	}
	body.WriteString(fmt.Sprintf("**Session Key:**  `%s`\n", msg.SessionKey))

	return NewCard().
		Title(e.i18n.T(MsgWhoamiCardTitle), "blue").
		Markdown(body.String()).
		Divider().
		Note(e.i18n.T(MsgWhoamiUsage)).
		Buttons(e.cardBackButton()).
		Build()
}

// 渲染版本卡片
func (e *Engine) renderVersionCard() *Card {
	return NewCard().
		Title(e.i18n.T(MsgCardTitleVersion), "grey").
		Markdown(VersionInfo).
		Buttons(e.cardBackButton()).
		Build()
}

// 渲染删除Mode卡片
func (e *Engine) renderDeleteModeCard(sessionKey string) *Card {
	agentSessions, err := e.agent.ListSessions(e.ctx)
	if err != nil {
		return e.simpleCard(e.i18n.T(MsgDeleteModeTitle), "red", err.Error())
	}
	agentSessions = filterOwnedSessions(agentSessions, e.sessions.KnownAgentSessionIDs())
	dm := e.getDeleteModeState(sessionKey)
	if dm == nil {
		return e.simpleCard(e.i18n.T(MsgDeleteModeTitle), "red", e.i18n.T(MsgDeleteUsage))
	}
	switch dm.phase {
	case "confirm":
		return e.renderDeleteModeConfirmCard(e.sessions, dm, agentSessions)
	case "result":
		return e.renderDeleteModeResultCard(dm)
	default:
		return e.renderDeleteModeSelectCard(sessionKey, e.sessions, dm, agentSessions)
	}
}

// 默认展示delte 卡片
func (e *Engine) renderDeleteModeSelectCard(sessionKey string, sessions *SessionManager, dm *deleteModeState, agentSessions []AgentSessionInfo) *Card {
	if len(agentSessions) == 0 {
		return e.simpleCard(e.i18n.T(MsgDeleteModeTitle), "red", e.i18n.T(MsgListEmpty))
	}
	total := len(agentSessions)
	totalPages := (total + listPageSize - 1) / listPageSize
	page := dm.page
	page = max(page, 1)
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * listPageSize
	end := start + listPageSize
	end = min(end, total)

	cb := NewCard().Title(e.i18n.T(MsgDeleteModeTitle), "carmine")
	activeAgentID := sessions.GetOrCreateActive(sessionKey).GetAgentSessionID()
	selectedCount := 0
	for i := start; i < end; i++ {
		s := agentSessions[i]
		isActive := activeAgentID == s.ID
		isSelected := false
		if !isActive {
			_, isSelected = dm.selectedIDs[s.ID]
		}
		marker := "◻"
		if isActive {
			marker = "▶"
		} else if isSelected {
			marker = "☑"
			selectedCount++
		}
		btnText := e.i18n.T(MsgDeleteModeSelect)
		btnType := "default"
		action := fmt.Sprintf("act:/delete-mode toggle %s", s.ID)
		if isActive {
			btnText = e.i18n.T(MsgCardTitleCurrentSession)
			btnType = "primary"
			action = fmt.Sprintf("act:/delete-mode noop %s", s.ID)
		} else if isSelected {
			btnText = e.i18n.T(MsgDeleteModeSelected)
			btnType = "primary"
		}
		cb.ListItemBtn(
			e.i18n.Tf(MsgListItem, marker, i+1, e.deleteSessionDisplayName(sessions, &s), s.MessageCount, s.ModifiedAt.Format("01-02 15:04")),
			btnText,
			btnType,
			action,
		)
	}
	cb.TaggedNote("delete-mode-selected-count", e.i18n.Tf(MsgDeleteModeSelectedCount, selectedCount))
	if dm.hint != "" {
		cb.Note(dm.hint)
	}
	cb.Buttons(
		DangerBtn(e.i18n.T(MsgDeleteModeDeleteSelected), "act:/delete-mode confirm"),
		DefaultBtn(e.i18n.T(MsgDeleteModeCancel), "act:/delete-mode cancel"),
	)

	var navBtns []CardButton
	if page > 1 {
		navBtns = append(navBtns, DefaultBtn(e.i18n.T(MsgCardPrev), fmt.Sprintf("act:/delete-mode page %d", page-1)))
	}
	if page < totalPages {
		navBtns = append(navBtns, DefaultBtn(e.i18n.T(MsgCardNext), fmt.Sprintf("act:/delete-mode page %d", page+1)))
	}
	if len(navBtns) > 0 {
		cb.Buttons(navBtns...)
	}
	return cb.Build()
}

// 提交删除
func (e *Engine) renderDeleteModeConfirmCard(sessions *SessionManager, dm *deleteModeState, agentSessions []AgentSessionInfo) *Card {
	selectedNames := e.deleteModeSelectionNames(sessions, dm, agentSessions)
	body := strings.Join(selectedNames, "\n")
	if body == "" {
		body = e.i18n.T(MsgDeleteModeEmptySelection)
	}
	return NewCard().
		Title(e.i18n.T(MsgDeleteModeConfirmTitle), "carmine").
		Markdown(body).
		Buttons(
			DangerBtn(e.i18n.T(MsgDeleteModeConfirmButton), "act:/delete-mode submit"),
			DefaultBtn(e.i18n.T(MsgDeleteModeBackButton), "act:/delete-mode back"),
		).
		Build()
}

// 展示删除结果
func (e *Engine) renderDeleteModeResultCard(dm *deleteModeState) *Card {
	return NewCard().
		Title(e.i18n.T(MsgDeleteModeResultTitle), "turquoise").
		Markdown(dm.result).
		Buttons(DefaultBtn(e.i18n.T(MsgCardBack), "nav:/list 1")).
		Build()
}

// ======================== Engine CronJob相关 ========================

// 通过注入同步消息到engine运行一个定时任务, 重构reply context, 处理消息就像用户发送的它
func (e *Engine) ExecuteCronJob(job *CronJob) error {
	sessionKey := job.SessionKey
	platformName := ""
	if idx := strings.Index(sessionKey, ":"); idx > 0 {
		platformName = sessionKey[:idx]
	}

	var targetPlatform Platform
	if e.platform.Name() == platformName {
		targetPlatform = e.platform
	}

	if targetPlatform == nil {
		return fmt.Errorf("platform %q not match for session %q", platformName, sessionKey)
	}

	rc, ok := targetPlatform.(ReplyContextReconstructor)
	if !ok {
		return fmt.Errorf("platform %q dose not support proactive messaging (cron)", platformName)
	}

	runSessionKey := sessionKey
	var replyCtx any
	var err error

	replyCtx, err = rc.ReconstructReplyCtx(runSessionKey)
	if err != nil {
		return fmt.Errorf("reconstruct reply context: %w", err)
	}

	// 当muted 时 包装platform忽略所有outgoing message
	effectivePlatform := targetPlatform
	if job.Mute {
		effectivePlatform = &mutePlatform{targetPlatform}
	}

	// 通知用户一个定时任务正在执行(除非 silent/muted)
	if !job.Mute {
		silent := false
		if e.cronScheduler != nil {
			silent = e.cronScheduler.IsSilent(job)
		}
		if !silent {
			desc := job.Description
			if desc != "" {
				if job.IsShellJob() {
					desc = truncateStr(job.Exec, 40)
				} else {
					desc = truncateStr(job.Prompt, 40)
				}
			}
			e.send(targetPlatform, replyCtx, fmt.Sprintf("⏰ %s", desc))
		}
	}

	if job.IsShellJob() {
		return e.executeCronShell(effectivePlatform, replyCtx, job)
	}

	msg := &Message{
		SessionKey:   sessionKey,
		Platform:     platformName,
		UserID:       "cron",
		UserName:     "cron",
		Content:      job.Prompt,
		ReplyCtx:     replyCtx,
		ModeOverride: job.Mode,
	}

	useNewSession := false
	if e.cronScheduler != nil {
		useNewSession = e.cronScheduler.UsesNewSession(job)
	} else {
		useNewSession = job.UsesNewSessionPerRun()
	}

	if useNewSession {
		msg.SessionKey = runSessionKey
		// 给runSessionKey注册一个新的key
		session := e.sessions.NewSideSession(runSessionKey, "cron-"+job.ID)
		if !session.TryLock() {
			return fmt.Errorf("session %q is busy", runSessionKey)
		}
		iKey := fmt.Sprintf("%s#cron:%s", runSessionKey, session.ID)
		e.processInteractiveMessageWith(effectivePlatform, msg, session, e.agent, e.sessions, iKey, "", runSessionKey)
		e.cleanupInteractiveState(iKey)
		return nil
	}

	session := e.sessions.GetOrCreateActive(sessionKey)
	if !session.TryLock() {
		return fmt.Errorf("session %q is busy", sessionKey)
	}

	e.processInteractiveMessageWith(effectivePlatform, msg, session, e.agent, e.sessions, sessionKey, "", sessionKey)
	return nil

}

// 运行shell命令,发送输出
func (e *Engine) executeCronShell(p Platform, replyCtx any, job *CronJob) error {
	workDir := job.WorkDir
	if workDir == "" {
		if wd, ok := e.agent.(interface{ GetWorkDir() string }); ok {
			workDir = wd.GetWorkDir()
		}
	}
	if workDir == "" {
		workDir, _ = os.Getwd()
	}

	timeout := job.ExecutionTimeout()
	var ctx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(e.ctx, timeout)
	} else {
		ctx, cancel = context.WithCancel(e.ctx)
	}
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", job.Exec)
	cmd.Dir = workDir
	output, err := cmd.CombinedOutput()

	if ctx.Err() == context.DeadlineExceeded {
		e.send(p, replyCtx, fmt.Sprintf("⏰ ⚠️ timeout: `%s`", truncateStr(job.Exec, 60)))
		return fmt.Errorf("shell command timed out")
	}

	result := strings.TrimSpace(string(output))
	if err != nil {
		if result != "" {
			e.send(p, replyCtx, fmt.Sprintf("⏰ ❌ `%s`\n\n%s\n\nerror: %v", truncateStr(job.Exec, 60), truncateStr(result, 3000), err))
		} else {
			e.send(p, replyCtx, fmt.Sprintf("⏰ ❌ `%s`\nerror: %v", truncateStr(job.Exec, 60), err))
		}
		return fmt.Errorf("shell: %w", err)
	}

	if result == "" {
		result = "(no output)"
	}
	e.send(p, replyCtx, fmt.Sprintf("⏰ ✅ `%s`\n\n%s", truncateStr(job.Exec, 60), truncateStr(result, 3000)))
	return nil
}

// ======================== Engine Mode相关 ========================

// 获取sessionKey对应的sessionState并切换的到mode
func (e *Engine) applyLiveModeChange(sessionKey, mode string) bool {
	e.interactiveMu.Lock()
	state, ok := e.interactiveStates[sessionKey]
	e.interactiveMu.Unlock()
	if !ok || state == nil || state.agentSession == nil || !state.agentSession.Alive() {
		return false
	}
	switcher, ok := state.agentSession.(LiveModeSwitcher)
	if !ok {
		return false
	}
	return switcher.SetLiveMode(mode)
}

// 根据传入的sessionKey 执行对应args中的操作
func (e *Engine) executeDeleteModeAction(sessionKey, args string) {
	e.interactiveMu.Lock()
	state := e.interactiveStates[sessionKey]
	e.interactiveMu.Unlock()
	if state == nil {
		return
	}

	// 解析参数
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return // 没解析到参数
	}
	// 判断state的 deleteMode
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.deleteMode == nil {
		return
	}

	dm := state.deleteMode
	switch fields[0] {
	case "toggle":
		if len(fields) < 2 { // args: toggle {id}
			return
		}
		id := fields[1]
		if _, ok := dm.selectedIDs[id]; ok {
			delete(dm.selectedIDs, id)
		} else {
			dm.selectedIDs[id] = struct{}{}
		}
		dm.phase = "select"
		dm.hint = ""
	case "page":
		if len(fields) < 2 { // args: page {pageid}
			return
		}
		if n, err := strconv.Atoi(fields[1]); err == nil && n > 0 {
			dm.page = n
		}
		dm.phase = "select"
	case "confirm":
		if len(dm.selectedIDs) == 0 {
			dm.phase = "select"
			dm.hint = e.i18n.T(MsgDeleteModeEmptySelection) // 请选择至少一个会话?
			return
		}
		dm.phase = "confirm"
		dm.hint = ""
	case "back":
		dm.phase = "select"
	case "submit":
		lines := e.submitDeleteModeSelection(sessionKey, dm)
		dm.selectedIDs = make(map[string]struct{})
		dm.result = strings.Join(lines, "\n")
		dm.hint = ""
		dm.phase = "result"
	case "form-submit":
		dm.selectedIDs = parseDeleteModeSelectedIDs(fields[1:])
		if len(dm.selectedIDs) == 0 {
			dm.phase = "select"
			dm.hint = e.i18n.T(MsgDeleteModeEmptySelection)
			return
		}
		dm.phase = "confirm"
		dm.hint = ""
	case "cancel":
		state.deleteMode = nil
	}
}

func (e *Engine) submitDeleteModeSelection(sessionKey string, dm *deleteModeState) []string {
	agent, sessions := e.agent, e.sessions
	deleter, ok := agent.(SessionDeleter)
	// 不支持删除session
	if !ok {
		return []string{e.i18n.T(MsgDeleteNotSupported)}
	}
	// 通过opencode 命令列出session
	agentSessions, err := agent.ListSessions(e.ctx)
	if err != nil {
		return []string{e.i18n.Tf(MsgError, err)}
	}
	// 过滤tc-connect 拥有的Session
	agentSessions = filterOwnedSessions(agentSessions, sessions.KnownAgentSessionIDs())
	seen := make(map[string]struct{}, len(agentSessions))
	lines := make([]string, 0, len(dm.selectedIDs))
	for i := range agentSessions {
		seen[agentSessions[i].ID] = struct{}{}
		if _, ok := dm.selectedIDs[agentSessions[i].ID]; !ok {
			continue
		}
		if line := e.delteSingleSessionReply(&Message{SessionKey: sessionKey}, deleter, &agentSessions[i]); line != "" {
			lines = append(lines, line)
		}
	}
	// 未找到的session
	missingIDs := make([]string, 0)
	for id := range dm.selectedIDs {
		if _, ok := seen[id]; ok {
			continue
		}
		missingIDs = append(missingIDs, id)
	}
	sort.Strings(missingIDs)
	for _, id := range missingIDs {
		lines = append(lines, fmt.Sprintf(e.i18n.T(MsgDeleteModeMissingSession), id))
	}
	if len(lines) == 0 {
		lines = append(lines, e.i18n.T(MsgDeleteModeEmptySelection))
	}
	return lines
}

// 移除给定sessionKey的交互状态关闭他的agent session.当expected state提供时，如果map entry
// 被一个不同的state替换，cleanup跳过，这防止了stale goroutine(/new 之后仍然运行，并且一个新的
// turn从它启动)以免意外破坏替换状态
func (e *Engine) cleanupInteractiveState(sessionKey string, expected ...*interactiveState) {
	e.interactiveMu.Lock()
	state, ok := e.interactiveStates[sessionKey]
	if len(expected) > 0 && expected[0] != nil && state != expected[0] {
		// 已经被替换，跳过
		e.interactiveMu.Unlock()
		return
	}
	// 任意futher进程之前捕获agent session
	var agentSession AgentSession
	if ok && state != nil {
		agentSession = state.agentSession
	}
	e.interactiveMu.Unlock()

	// 通知任意排队的message的sender 永远不会被处理
	if ok && state != nil {
		state.markStopped()
		e.notifyDroppedQueuedMessage(state, fmt.Errorf("session reset"))
	}

	// 从map删除前关闭agent session 之前，先关闭代理会话
	// 这可以防止清理过程出现竞争条件，即/stop 看到的是一个空映射
	// 并报告"No execution in processing",而agent session的Close()
	// 仍处于阻塞状态(130s)
	if agentSession != nil {
		e.closeAgentSessionWithTimeout(sessionKey, agentSession)
	}

	// session关闭之后从map删除state
	e.interactiveMu.Lock()
	// 再次检查关闭时state没有被代替
	currentState, curentOk := e.interactiveStates[sessionKey]
	if curentOk && len(expected) > 0 && expected[0] != nil && currentState != expected[0] {
		// 关闭过程中另一个trun 已经替换了state - 不要删除
		e.interactiveMu.Unlock()
		return
	}
	delete(e.interactiveStates, sessionKey)
	e.interactiveMu.Lock()
}

// ======================== Engine Session相关 ========================

func (e *Engine) closeAgentSessionAsync(sessionKey string, agentSession AgentSession) {
	if agentSession == nil {
		return
	}
	go e.closeAgentSessionWithTimeout(sessionKey, agentSession)
}

// 带有超时的异步关闭方法
func (e *Engine) closeAgentSessionWithTimeout(sessionKey string, agentSession AgentSession) {
	if agentSession == nil {
		return
	}

	// 允许足够的时间用于agent自己优雅的关闭流程:
	// stdin close -> stop hook (claude-mem summary 等) -> SIGTERM -> SIGKILL
	// 130s覆盖了默认的120s优雅接卸 + 5s SIGTERM + 5s buffer. 等待提早结束
	// 如果进程立即退出 - 这是上限时间,而非典型事件
	const closeTimeout = 130 * time.Second
	closeStart := time.Now()

	slog.Debug("cleanupInteractiveState: closing agent session", "session", sessionKey)
	done := make(chan struct{})
	go func() {
		agentSession.Close()
		close(done)
	}()

	select {
	case <-done:
		if elapsed := time.Since(closeStart); elapsed >= slowAgentClose {
			slog.Warn("slow agent session close", "elapsed", elapsed, "session", sessionKey)
		}
	case <-time.After(closeTimeout):
		slog.Error("agent session close timeout, abandoning",
			"timeout", closeTimeout, "session", sessionKey)
	}
}

// 带返回的结果的删除方法
func (e *Engine) delteSingleSessionReply(msg *Message, deleter SessionDeleter, matched *AgentSessionInfo) string {
	if matched == nil {
		return ""
	}

	// 防止删除当前激活的session
	activeSession := e.sessions.GetOrCreateActive(msg.SessionKey)
	if activeSession.GetAgentSessionID() == matched.ID {
		return e.i18n.T(MsgDeleteActiveDenied) // 不能删除当前的session
	}

	displayName := e.delteSessionDisplayName(e.sessions, matched)
	if err := deleter.DeleteSession(e.ctx, matched.ID); err != nil {
		return e.i18n.Tf(MsgFailedToDeleteSession, displayName, err) // 删除失败
	}

	// 使用agent-side deletion 保持本地session 快照
	e.sessions.DeleteByAgentSessionID(matched.ID)
	e.sessions.SetSessionName(matched.ID, "")
	return fmt.Sprintf(e.i18n.T(MsgDeleteSuccess), displayName)
}

// 尝试获取名称 Name -> Summary -> shortID, TODO: 移除Name直接用ID
func (e *Engine) delteSessionDisplayName(sessions *SessionManager, matched *AgentSessionInfo) string {
	displayName := sessions.GetSessionName(matched.ID)
	if displayName == "" {
		displayName = matched.Summary
	}
	if displayName == "" {
		shortID := matched.ID
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		displayName = shortID
	}
	return displayName
}

// 将用户请求解析到agent session, 优先级
// 1. 数字需要(1-base, 匹配 /list output) 2. 完全自定义名称匹配(不区分大小写)
// 3. sessionID 前缀匹配 4. 自定义名称前缀匹配(不区分大小写)
// 5. summary子串匹配
func (e *Engine) matchSession(sessions []AgentSessionInfo, manager *SessionManager, query string) *AgentSessionInfo {
	if len(sessions) == 0 {
		return nil
	}

	// 1. 数字匹配
	if idx, err := strconv.Atoi(query); err == nil && idx >= 1 && idx <= len(sessions) {
		return &sessions[idx-1]
	}
	queryLower := strings.ToLower(query)

	// 2. 自定义名称匹配
	for i := range sessions {
		name := manager.GetSessionName(sessions[i].ID)
		if name != "" && strings.ToLower(name) == queryLower {
			return &sessions[i]
		}
	}

	// 3. sessionID prefix
	for i := range sessions {
		if strings.HasPrefix(sessions[i].ID, query) {
			return &sessions[i]
		}
	}

	// 4. 自定义名称前缀匹配
	for i := range sessions {
		name := manager.GetSessionName(sessions[i].ID)
		if name != "" && strings.HasPrefix(strings.ToLower(name), queryLower) {
			return &sessions[i]
		}
	}

	// 5. summary 字串匹配
	for i := range sessions {
		if sessions[i].Summary != "" && strings.Contains(strings.ToLower(sessions[i].Summary), queryLower) {
			return &sessions[i]
		}
	}
	return nil
}

func (e *Engine) stopInteractiveSession(sessionKey string, quietPlatform Platform, quietReplyCtx any) bool {
	e.interactiveMu.Lock()
	state, ok := e.interactiveStates[sessionKey]
	if !ok || state == nil {
		e.interactiveMu.Unlock()
		return false
	}

	state.mu.Lock()
	pending := state.pending
	state.pending = nil
	agentSession := state.agentSession
	state.mu.Unlock()

	state.markStopped()
	delete(e.interactiveStates, sessionKey)
	e.interactiveMu.Unlock()

	if pending != nil {
		pending.resolve()
	}
	e.notifyDroppedQueuedMessage(state, fmt.Errorf("session reset"))
	e.closeAgentSessionAsync(sessionKey, agentSession)
	return true
}

func (e *Engine) maybeAutoResetSessionOnIdle(p Platform, msg *Message, sessions *SessionManager, interactiveKey string, session *Session) *Session {
	if e.resetOnIdle <= 0 || session == nil {
		return nil
	}

	hasBackend := session.GetAgentSessionID() != ""
	hasHistory := len(session.GetHistory(1)) > 0
	if !hasBackend && !hasHistory {
		return nil
	}

	lastActive := session.GetUpdatedAt()
	if lastActive.IsZero() || time.Since(lastActive) < e.resetOnIdle {
		return nil
	}

	slog.Info("auto-resetting idle session",
		"session_key", msg.SessionKey,
		"session_id", session.ID,
		"idle_for", time.Since(lastActive),
		"threshold", e.resetOnIdle,
	)

	// Check if the old session has an agent process that needs graceful
	// shutdown. If so, tell the user we're wrapping up before blocking.
	e.interactiveMu.Lock()
	state, hasState := e.interactiveStates[interactiveKey]
	hasAgent := hasState && state != nil && state.agentSession != nil && state.agentSession.Alive()
	e.interactiveMu.Unlock()

	if hasAgent {
		// Notify the user before the potentially long close. The close
		// returns as soon as the process exits (usually seconds), but
		// Stop hooks can take up to 120s.
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgSessionClosingGraceful))
	}

	e.cleanupInteractiveState(interactiveKey)
	session.UnlockWithoutUpdate()

	newSession := sessions.NewSession(msg.SessionKey, "")
	if !newSession.TryLock() {
		slog.Error("failed to lock new session after idle auto-reset", "session_key", msg.SessionKey, "new_session", newSession.ID)
		return nil
	}

	e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgSessionAutoResetIdle, int(e.resetOnIdle/time.Minute)))
	return newSession
}

// ======================== Engine 消息相关 ========================

// 从状态中清理排队消息并向排队的message sender都发送错误通知, 当event循环正常退出(EvnetError, channel close)
// 并且排队信息无法代理到agent时调用
func (e *Engine) notifyDroppedQueuedMessage(state *interactiveState, reason error) {
	state.mu.Lock()
	remaining := state.pendingMessages
	state.pendingMessages = nil
	state.mu.Unlock()
	for _, q := range remaining {
		e.sendWithError(q.platform, q.replyCtx, fmt.Sprintf(e.i18n.T(MsgError), reason))
	}
}

// 应用了速率限制和p.Send. 打印 等待取消和平台失败,并返回一个非nil的错误
func (e *Engine) sendWithError(p Platform, replyCtx any, content string) error {
	if err := e.waitOutgoing(p); err != nil {
		slog.Warn("outgoing rate limit: context cancelled", "platform", p.Name, "error", err)
		return err
	}
	return e.sendAlreadyRenderedWithError(p, replyCtx, content)
}

// 调用 Send判断是否发送失败或响应发送是否较慢
func (e *Engine) sendAlreadyRenderedWithError(p Platform, replyCtx any, content string) error {
	start := time.Now()
	if err := p.Send(e.ctx, replyCtx, content); err != nil {
		slog.Error("platform send failed", "platform", p.Name(), "error", err, "content_len", len(content))
		return err
	}
	if elapsed := time.Since(start); elapsed >= slowPlatformSend {
		slog.Warn("slow platform send", "platform", p.Name(), "elapsed", elapsed, "content_len", len(content))
	}
	return nil
}

// 对p.Send封装,加入错误日志记录,满操作和超速率限制
func (e *Engine) send(p Platform, replyCtx any, content string) {
	_ = e.sendWithError(p, replyCtx, content)
}

// 从state中移除pendingMessages 并给每一个队列中消息的发送者发送错误通知, 当 event loop 异常退出
// (EventError, channel closed) 并且排队消息不在分发给agent时调用
func (e *Engine) notifyDroppedQueuedMessages(state *interactiveState, reason error) {
	state.mu.Lock()
	remaining := state.pendingMessages
	state.pendingMessages = nil
	state.mu.Unlock()
	for _, q := range remaining {
		e.send(q.platform, q.replyCtx, fmt.Sprintf(e.i18n.T(MsgError), reason))
	}
}

// 处理state的pendingMessages队列中所有的排队消息. 当queue为空时(当holding state.mu) 解锁session 来关闭"queue empty" 和
// "session unlocked"之间的竞争. 如果session被此调用解锁, 返回true
func (e *Engine) drainPendingMessages(state *interactiveState, session *Session, sessions *SessionManager, sessionKey string) bool {
	for {
		state.mu.Lock()
		if len(state.pendingMessages) == 0 {
			session.Unlock()
			state.mu.Unlock()
			return true
		}
		queued := state.pendingMessages[0]
		state.pendingMessages = state.pendingMessages[1:]
		state.platform = queued.platform
		state.replyCtx = queued.replyCtx
		state.fromVoice = queued.fromVoice
		state.mu.Unlock()

		e.i18n.DetectAndSet(queued.content)
		prompt := e.buildSenderPrompt(queued.content, queued.userID, queued.msgPlatform, queued.msgSessionKey)

		if state.agentSession == nil || !state.agentSession.Alive() {
			e.send(queued.platform, queued.replyCtx, fmt.Sprintf(e.i18n.T(MsgError), "agent session ended"))
			e.notifyDroppedQueuedMessages(state, fmt.Errorf("agent session ended"))
			return false
		}

		drainEvents(state.agentSession.Events())

		session.AddHistory("user", queued.content)

		sendDone := make(chan error, 1)
		go func() {
			sendDone <- state.agentSession.Send(prompt, queued.images, queued.files)
		}()

		var stopTyping func()
		if ti, ok := queued.platform.(TypingIndicator); ok {
			stopTyping = ti.StartTyping(e.ctx, queued.replyCtx)
		}

		slog.Info("processing queued message", "session", sessionKey)
		e.processInteractiveEvents(state, session, sessions, sessionKey, "", time.Now(), stopTyping, sendDone, queued.replyCtx)
	}
}

// 当session繁忙时排队message以便延后, 排队时message不会发送到agent stdin
// 当前turn的EventResult接收后事件循环发送它, 如果message成功排队返回true, 失败返回falszMe
func (e *Engine) queueMessageForBusySession(p Platform, msg *Message, interactiveKey string) bool {
	e.interactiveMu.Lock()
	state, hasState := e.interactiveStates[interactiveKey]
	e.interactiveMu.Unlock()

	if !hasState || state == nil {
		return false
	}
	// Allow queueing when agentSession is nil (session is starting up,
	// issue #565). Only reject if the session was established and died.
	if state.agentSession != nil && !state.agentSession.Alive() {
		return false
	}

	// Only queue metadata — do NOT send to agent stdin yet.
	// The agent CLI may treat a mid-turn stdin message as part of the
	// current turn, causing the event loop to hang waiting for a second
	// EventResult that never arrives. Instead, the event loop sends the
	// message after the current turn's EventResult is received.
	state.mu.Lock()
	if len(state.pendingMessages) >= maxQueuedMessages {
		state.mu.Unlock()
		return false // fall back to "previous processing" reply
	}
	state.pendingMessages = append(state.pendingMessages, queuedMessage{
		platform:      p,
		replyCtx:      msg.ReplyCtx,
		content:       msg.Content,
		images:        msg.Images,
		files:         msg.Files,
		fromVoice:     msg.FromVoice,
		userID:        msg.UserID,
		msgPlatform:   msg.Platform,
		msgSessionKey: msg.SessionKey,
	})
	queueDepth := len(state.pendingMessages)
	state.mu.Unlock()

	slog.Info("message queued for busy session",
		"session", msg.SessionKey,
		"user", msg.UserName,
		"queue_depth", queueDepth,
	)
	e.reply(p, msg.ReplyCtx, e.i18n.T(MsgMessageQueued))
	return true
}

// 当消息排队但是drain loop 早已退出时调用, 处理所有排队的message, 
// 与processInteractiveMessageWith中的drain loop类似，但作为一个独立的goroutine。
func (e *Engine) drainOrphanedQueue(session *Session, sessions *SessionManager, interactiveKey string, agent Agent, workspaceDir string) {
	unlocked := false
	defer func() {
		if !unlocked {
			session.Unlock()
		}
	}()

	e.interactiveMu.Lock()
	state, hasState := e.interactiveStates[interactiveKey]
	e.interactiveMu.Unlock()

	if !hasState || state == nil || state.agentSession == nil || !state.agentSession.Alive() {
		if hasState && state != nil {
			e.notifyDroppedQueuedMessages(state, fmt.Errorf("agent session ended"))
		}
		return
	}

	unlocked = e.drainPendingMessages(state, session, sessions, interactiveKey)
}

// 创建一个placeholder interactiveState entry 若不存在, 这使得在代理会话
// 启动过程中到达的消息能够被排队，而不会被丢弃（问题#565）。
func (e *Engine) ensureInteractiveStateForQueueing(key string, p Platform, replyCtx any) {
	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()
	if _, ok := e.interactiveStates[key]; !ok {
		e.interactiveStates[key] = &interactiveState{
			platform: p,
			replyCtx: replyCtx,
		}
	}
}

// ======================== Engine 上下文相关 ========================

// 发送agent的压缩命令并处理结果,若autoTriggered 为真,  隐藏用户可见的“压缩”和补全消息。
func (e *Engine) runCompress(state *interactiveState, session *Session, sessions *SessionManager, iKey string, p Platform, replyCtx any, auto bool) {
	// 当持有state.mu 来关闭竞争窗口时session.Unlock() 在 drainQueuedMessagesAfterCompress 内部被调用
	// 确保锁在early-return paths 被释放
	compressUnlocked := false
	defer func() {
		if !compressUnlocked {
			session.Unlock()
		}
	}()

	state.mu.Lock()
	state.platform = p
	state.replyCtx = replyCtx
	state.mu.Unlock()

	drainEvents(state.agentSession.Events())

	compressor, ok := e.agent.(ContextCompressor)
	if !ok || compressor.CompressCommand() == "" {
		if !auto {
			e.reply(p, replyCtx, e.i18n.T(MsgCompressNotSupported))
		}
		return
	}

	cmd := compressor.CompressCommand()
	if err := state.agentSession.Send(cmd, nil, nil); err != nil {
		if !auto {
			e.reply(p, replyCtx, fmt.Sprintf(e.i18n.T(MsgError), err))
		}
		if !state.agentSession.Alive() {
			e.cleanupInteractiveState(iKey)
		}
		return
	}

	e.processCompressEvents(state, session, sessions, iKey, p, replyCtx, &compressUnlocked, auto)
}

// compress command 命令之后移除agent events. 不同于processInteractiveEvents, 她布基洛历史并
// 将空结果视作成功而不是 (empty response)
func (e *Engine) processCompressEvents(state *interactiveState, session *Session, sessions *SessionManager, sessionKey string, p Platform, replyCtx any, unlocked *bool, auto bool) {

	var textParts []string
	events := state.agentSession.Events()
	stopCh := state.stopSignal()

	var idleTimer *time.Timer
	var idleCh <-chan time.Time
	if e.eventIdleTimeout > 0 {
		idleTimer = time.NewTimer(e.eventIdleTimeout)
		defer idleTimer.Stop()
		idleCh = idleTimer.C
	}

	for {
		var event Event
		var ok bool

		select {
		case <-stopCh:
			return
		case event, ok = <-events:
			if !ok {
				e.cleanupInteractiveState(sessionKey, state)
				if !auto {
					if len(textParts) > 0 {
						e.send(p, replyCtx, strings.Join(textParts, ""))
					} else {
						e.reply(p, replyCtx, e.i18n.T(MsgCompressDone))
					}
				}
				e.notifyDroppedQueuedMessages(state, fmt.Errorf("agent process exited during compress"))
				return
			}
		case <-idleCh:
			if !auto {
				e.send(p, replyCtx, fmt.Sprintf(e.i18n.T(MsgError), "compress timed out"))
			}
			e.cleanupInteractiveState(sessionKey, state)
			e.notifyDroppedQueuedMessages(state, fmt.Errorf("compress timed out"))
			return
		case <-e.ctx.Done():
			return
		}

		if state.isStopped() {
			return
		}

		if idleTimer != nil {
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(e.eventIdleTimeout)
		}

		switch event.Type {
		case EventText:
			if !auto && event.Content != "" {
				textParts = append(textParts, event.Content)
			}
		// FIXME: EventToolResult
		case EventResult:
			result := event.Content
			if result == "" && len(textParts) > 0 {
				result = strings.Join(textParts, "")
			}
			if !auto {
				if result != "" {
					e.send(p, replyCtx, result)
				} else {
					e.reply(p, replyCtx, e.i18n.T(MsgCompressDone))
				}
			}

			// 压缩完毕之后,处理排队message而不是丢失
			e.drainQueuedMessagesAfterCompress(state, session, sessions, sessionKey, unlocked)
			return
		case EventError:
			if !auto && event.Error != nil {
				e.reply(p, replyCtx, fmt.Sprintf(e.i18n.T(MsgError), event.Error))
			}
			// 如果agent dead 则丢弃排队消息,某些 agents 触发 per-turn EventError 当保持staying alive
			if !state.agentSession.Alive() {
				e.notifyDroppedQueuedMessages(state, event.Error)
			} else {
				// Agent survived — try to process queued messages.
				e.drainQueuedMessagesAfterCompress(state, session, sessions, sessionKey, unlocked)
			}
			return
		case EventPermissionRequest:
			_ = state.agentSession.RespondPermission(event.RequestID, PermissionResult{
				Behavior:     "allow",
				UpdatedInput: event.ToolInputRaw,
			})
		}
	}
}

// 处理在 /compress 操作期间排队的任意messages, 发送每一个到agent并运行完整的交互时间循环
func (e *Engine) drainQueuedMessagesAfterCompress(state *interactiveState, session *Session, sessions *SessionManager, sessionKey string, unlocked *bool) {
	if e.drainPendingMessages(state, session, sessions, sessionKey) {
		*unlocked = true
	}
}

func (e *Engine) renderOutgoingContentForWorkspace(p Platform, content, workspaceDir string) string {
	if strings.TrimSpace(content) == "" {
		return content
	}
	return TransformLocalReferences(content, e.references, e.agent.Name(), p.Name(), workspaceDir)
}

func (e *Engine) sendWithErrorForWorkspace(p Platform, replyCtx any, content, workspaceDir string) error {
	if err := e.waitOutgoing(p); err != nil {
		slog.Warn("outgoing rate limit: context cancelled", "platform", p.Name(), "error", err)
		return err
	}
	content = e.renderOutgoingContentForWorkspace(p, content, workspaceDir)
	return e.sendAlreadyRenderedWithError(p, replyCtx, content)
}

func (e *Engine) sendForWorkspace(p Platform, replyCtx any, content, workspaceDir string) {
	_ = e.sendWithErrorForWorkspace(p, replyCtx, content, workspaceDir)
}

// ======================== Engine 权限相关 ========================

// 从AskUserQuestion list渲染一个问题(根据index), qIdx 是0-based 的用于展示的问题下标
func (e *Engine) sendAskQuestionPrompt(p Platform, replyCtx any, questions []UserQuestion, qIdx int) {
	if qIdx >= len(questions) {
		return
	}
	q := questions[qIdx]
	total := len(questions)

	titleSuffix := ""
	if total > 1 {
		titleSuffix = fmt.Sprintf(" (%d/%d)", qIdx+1, total)
	}

	// Plain text fallback
	var sb strings.Builder
	sb.WriteString("❓ **")
	sb.WriteString(q.Question)
	sb.WriteString("**")
	sb.WriteString(titleSuffix)
	if q.MultiSelect {
		sb.WriteString(e.i18n.T(MsgAskQuestionMulti))
	}
	sb.WriteString("\n\n")
	for i, opt := range q.Options {
		sb.WriteString(fmt.Sprintf("%d. **%s**", i+1, opt.Label))
		if opt.Description != "" {
			sb.WriteString(" — ")
			sb.WriteString(opt.Description)
		}
		sb.WriteString("\n")
	}
	sb.WriteString(fmt.Sprintf("\n%s", e.i18n.T(MsgAskQuestionNote)))
	e.send(p, replyCtx, sb.String())
}


func (e *Engine) handlePendingPermission(p Platform, msg *Message, content string) bool {
	e.interactiveMu.Lock()
	state, ok := e.interactiveStates[msg.SessionKey]
	e.interactiveMu.Unlock()
	if !ok || state == nil {
		return false
	}

	state.mu.Lock()
	pending := state.pending
	state.mu.Unlock()
	if pending == nil {
		return false
	}

	// AskUserQuestion: interpret user response as an answer, not a permission decision
	if len(pending.Questions) > 0 {
		curIdx := pending.CurrentQuestion
		q := pending.Questions[curIdx]
		answer := e.resolveAskQuestionAnswer(q, content)

		if pending.Answers == nil {
			pending.Answers = make(map[int]string)
		}
		pending.Answers[curIdx] = answer

		// More questions remaining — advance to next and send new card
		if curIdx+1 < len(pending.Questions) {
			pending.CurrentQuestion = curIdx + 1
			e.reply(p, msg.ReplyCtx, fmt.Sprintf("✅ %s: **%s**", q.Question, answer))
			e.sendAskQuestionPrompt(p, msg.ReplyCtx, pending.Questions, curIdx+1)
			return true
		}

		// All questions answered — build response and resolve
		updatedInput := buildAskQuestionResponse(pending.ToolInput, pending.Questions, pending.Answers)

		if err := state.agentSession.RespondPermission(pending.RequestID, PermissionResult{
			Behavior:     "allow",
			UpdatedInput: updatedInput,
		}); err != nil {
			slog.Error("failed to send AskUserQuestion response", "error", err)
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgError), err))
		} else {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf("✅ %s: **%s**", q.Question, answer))
		}

		state.mu.Lock()
		state.pending = nil
		state.mu.Unlock()
		pending.resolve()
		return true
	}

	lower := strings.ToLower(strings.TrimSpace(content))

	if isApproveAllResponse(lower) {
		state.mu.Lock()
		state.approveAll = true
		state.mu.Unlock()

		if err := state.agentSession.RespondPermission(pending.RequestID, PermissionResult{
			Behavior:     "allow",
			UpdatedInput: pending.ToolInput,
		}); err != nil {
			slog.Error("failed to send permission response", "error", err)
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgError), err))
		} else {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgPermissionApproveAll))
		}
	} else if isAllowResponse(lower) {
		if err := state.agentSession.RespondPermission(pending.RequestID, PermissionResult{
			Behavior:     "allow",
			UpdatedInput: pending.ToolInput,
		}); err != nil {
			slog.Error("failed to send permission response", "error", err)
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgError), err))
		} else {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgPermissionAllowed))
		}
	} else if isDenyResponse(lower) {
		if err := state.agentSession.RespondPermission(pending.RequestID, PermissionResult{
			Behavior: "deny",
			Message:  "User denied this tool use.",
		}); err != nil {
			slog.Error("failed to send deny response", "error", err)
		}
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgPermissionDenied))
	} else {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgPermissionHint))
		return true
	}

	state.mu.Lock()
	state.pending = nil
	state.mu.Unlock()
	pending.resolve()

	return true
}

// ======================== 辅助函数 ========================

// SaveRestartNotify persists restart info so the new process can send
// a "restart successful" message after startup.
func SaveRestartNotify(dataDir string, req RestartRequest) error {
	dir := filepath.Join(dataDir, "run")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("SaveRestartNotify: mkdir failed", "dir", dir, "error", err)
	}
	data, _ := json.Marshal(req)
	return os.WriteFile(filepath.Join(dir, "restart_notify"), data, 0o644)
}

// 移除不被tc-connect追踪的agent sessions, 防止同目录下的外部CLI创建的session出现在
// /list /switch /delete 等命令中, 如果完全没有追踪(如首次运行) 直接返回
func filterOwnedSessions(sessions []AgentSessionInfo, known map[string]struct{}) []AgentSessionInfo {
	if len(known) == 0 {
		return sessions
	}
	filtered := make([]AgentSessionInfo, 0, len(sessions))
	for _, s := range sessions {
		if _, ok := known[s.ID]; ok {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

// 从参数字符串中解析要删除的id
func parseDeleteModeSelectedIDs(args []string) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, arg := range args {
		for _, id := range strings.Split(arg, ",") {
			if id == "" {
				continue
			}
			ids[id] = struct{}{}
		}

	}
	return ids
}

// 分离帮助Tab行
func splitHelpTabRows(useMultiRow bool, tabs []CardButton) [][]CardButton {
	if useMultiRow {
		rows := make([][]CardButton, 0, (len(tabs)+1)/2)
		for i := 0; i < len(tabs); i += 2 {
			end := i + 2
			end = min(end, len(tabs))
			rows = append(rows, tabs[i:end])
		}
		return rows
	}
	return [][]CardButton{tabs}
}

// 给出语言对应的文本
func langDisplayName(lang Language) string {
	switch lang {
	case LangEnglish:
		return "English"
	case LangChinese:
		return "中文"
	case LangTraditionalChinese:
		return "繁體中文"
	case LangJapanese:
		return "日本語"
	default:
		return "Auto"
	}
}

// 返回持续时间文本
func formatDurationI18n(d time.Duration, lang Language) string {
	d = d.Round(time.Second)
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60

	switch lang {
	case LangChinese, LangTraditionalChinese:
		if days > 0 {
			return fmt.Sprintf("%d天 %d小时 %d分钟", days, hours, minutes)
		}
		if hours > 0 {
			return fmt.Sprintf("%d小时 %d分钟", hours, minutes)
		}
		return fmt.Sprintf("%d分钟", minutes)
	case LangJapanese:
		if days > 0 {
			return fmt.Sprintf("%d日 %d時間 %d分", days, hours, minutes)
		}
		if hours > 0 {
			return fmt.Sprintf("%d時間 %d分", hours, minutes)
		}
		return fmt.Sprintf("%d分", minutes)
	default:
		if days > 0 {
			return fmt.Sprintf("%dd %dh %dm", days, hours, minutes)
		}
		if hours > 0 {
			return fmt.Sprintf("%dh %dm", hours, minutes)
		}
		return fmt.Sprintf("%dm", minutes)
	}
}

// 以 \n\n 切分 title 和其他部分
func splitCardTitleBody(content string) (string, string) {
	content = strings.TrimSpace(content)
	parts := strings.SplitN(content, "\n\n", 2)
	title := strings.TrimSpace(parts[0])
	if len(parts) == 1 {
		return title, ""
	}
	return title, strings.TrimSpace(parts[1])
}

// : 分割sessionKey 返回第三部分 {}:{}:{UserID}
func extractUserID(sessionKey string) string {
	parts := strings.SplitN(sessionKey, ":", 3)
	if len(parts) >= 3 {
		return parts[2]
	}
	return ""
}

// t 和 now 是否跨年决定格式是否带 年份
func cronTimeFormat(t, now time.Time) string {
	if t.Year() != now.Year() {
		return "2006-01-02 15:04"
	}
	return "01-02 15:04"
}

// 从sessionKey中提取 channelID
func extractChannelID(sessionKey string) string {
	// Format: "platform:channelID:userID" or "platform:channelID"
	parts := strings.SplitN(sessionKey, ":", 3)
	if len(parts) >= 2 {
		return parts[1]
	}
	return ""
}

// 提取platform名称
func extractPlatformName(sessionKey string) string {
	if i := strings.IndexByte(sessionKey, ':'); i >= 0 {
		return sessionKey[:i]
	}
	return sessionKey
}

//	getOrCreateInteractiveStateWith 接收一个可选的agent agentOverride (multi-workspace mode)
//
// adoptPendingFromPlaceholder从当前的placeholder中复制排队消息 状态到新状态,这样当map entry被
// 替换时排队消息不会丢失.必须在interactiveMu下被调用
func adoptPendingFromPlaceholder(existing, newState *interactiveState) {
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

// 忽略event中的任意buffed 事件,
// 在new turn开始前调用，以防止将上一回合代理进程中的过时事件误认为是新回合的响应。
func drainEvents(ch <-chan Event) {
	drained := 0
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				// channel被关闭, 立即停止避免死循环
				return
			}
			drained++
		default:
			if drained > 0 {
				slog.Warn("drained stale events from previous turn", "count", drained)
			}
			return
		}
	}
}

func splitMessage(text string, maxLen int) []string {
	runes := []rune(text)
	if len(runes) <= maxLen {
		return []string{text}
	}
	var chunks []string

	for len(runes) > 0 {
		if len(runes) <= maxLen {
			chunks = append(chunks, string(runes))
			break
		}

		end := maxLen

		// 尝试在rune 窗口以换行符为边界进行拆分
		// 将候选块转换回字符串，以便进行换行符搜索。
		candidate := string(runes[:end])
		if idx := strings.LastIndex(candidate, "\n"); idx > 0 {
			// idx is a byte offset within candidate; convert to rune offset.
			runeIdx := utf8.RuneCountInString(candidate[:idx])
			if runeIdx >= end/2 {
				end = runeIdx + 1
			}
		}

		chunks = append(chunks, string(runes[:end]))
		runes = runes[end:]
	}
	return chunks
}

// truncateIf 函数将字符串 s 截断至最多 maxLen 个字符。0 表示不截断。
func truncateIf(s string, maxLen int) string {
	if maxLen <= 0 {
		return s
	}
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}
	return string([]rune(s)[:maxLen]) + "..."
}

// 正则匹配类似 "[ctx: ~42%]
var ctxSelfReportRe = regexp.MustCompile(`(?m)\n?\[ctx: ~\d+%\]`)

// 从self-reported "[ctx: ~XX%]" 行 提取百分比
func parseSelfReportedCtx(s string) int {
	m := ctxSelfReportRe.FindString(s)
	if m == "" {
		return 0
	}
	start := strings.Index(m, "~") + 1
	end := strings.Index(m, "%")
	if start <= 0 || end <= start {
		return 0
	}
	v, _ := strconv.Atoi(m[start:end])
	return v
}

// 类似estimateTokens但是包括一个还没有写入history的assistant信息(AddHistory之前在EventResult使用)
func estimateTokensWithPendingAssistant(entries []HistoryEntry, pendingAssistant string) int {
	// Heuristic: ~1 token per 4 characters in mixed English/Chinese.
	count := 0
	for _, h := range entries {
		count += len([]rune(h.Content))
	}
	if pendingAssistant != "" {
		count += len([]rune(pendingAssistant))
	}
	if count == 0 {
		return 0
	}
	return (count + 3) / 4
}

const modelContextWindow = 200_000 // Claude 上下文窗口, Opencode - 65535??

// 介于SDK-reported 输出token 返回一个类似于 \n[ctx ~42%]的后缀
func contextIndicator(inputTokens int) string {
	if inputTokens <= 0 {
		return ""
	}
	pct := inputTokens * 100 / modelContextWindow
	pct = min(pct, 100)
	return fmt.Sprintf("\n[ctx: ~%d%%]", pct)
}

func isApproveAllResponse(s string) bool {
	for _, w := range []string{
		"allow all", "allowall", "approve all", "yes all",
		"允许所有", "允许全部", "全部允许", "所有允许", "都允许", "全部同意",
	} {
		if s == w {
			return true
		}
	}
	return false
}

func isAllowResponse(s string) bool {
	for _, w := range []string{"allow", "yes", "y", "ok", "允许", "同意", "可以", "好", "好的", "是", "确认", "approve"} {
		if s == w {
			return true
		}
	}
	return false
}

func isDenyResponse(s string) bool {
	for _, w := range []string{"deny", "no", "n", "reject", "拒绝", "不允许", "不行", "不", "否", "取消", "cancel"} {
		if s == w {
			return true
		}
	}
	return false
}
