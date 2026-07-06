/*引擎应该只负责协调和调度，不应该包含纯 UI 组件构建代码*/
package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"tc-connect/core/command"
	"tc-connect/core/cron"
	"tc-connect/core/doctor"
	"tc-connect/core/i18n"
	"tc-connect/core/interactive"
	"tc-connect/core/platform"
	"tc-connect/core/renderer"
	"tc-connect/core/session"
	"tc-connect/core/skill"
	"tc-connect/core/streaming"
	"tc-connect/core/types"
	"time"
)

const listPageSize = 20

const defaultHelpGroup = "session"

// semvar tag, 由main设置 (如 "v1.2.0-beta.1")
var CurrentVersion string

// RestartCh is signaled when /restart is invoked. main listens on it
// to perform a graceful shutdown followed by syscall.Exec.
var RestartCh = make(chan types.RestartRequest, 1)

// 用于在平台和agent之间路由数据
type Engine struct {
	name         string
	agent        types.Agent
	platform     types.Platform
	sessions     *session.SessionManager
	ctx          context.Context
	cancel       context.CancelFunc
	i18n         *i18n.I18n
	display      *types.DisplayCfg
	injectSender bool
	startedAt    time.Time

	commandSaveAddFunc func(name, description, prompt, exec, workDir string) error
	commandSaveDelFunc func(name string) error

	// 持久化，保存到配置方法
	displaySaveFunc  func(thinkingMessages *bool, thinkingMaxLen, toolMaxLen *int, toolMessages *bool) error
	configReloadFunc func() (*types.ConfigReloadResult, error)

	aliasSaveAddFunc func(name, command string) error
	aliasSaveDelFunc func(name string) error

	cronScheduler *cron.CronScheduler

	commands *types.CommandRegistry
	skills   *skill.SkillRegistry
	aliases  map[string]string // trigger -> command (帮助 -> /help)
	aliasMu  sync.RWMutex

	bannedWords []string
	bannedMu    sync.RWMutex

	streamPreview    streaming.StreamPreviewCfg
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

	adminFrom   string           // 逗号分割的特权命令用户ID; "*" = 允许所有用户; "" = deny
	userRoles   *UserRoleManager // nil = legacy mode (no per-user policies)
	userRolesMu sync.RWMutex     // protects userRoles, disabledCmds, and adminFrom

	rateLimiter *RateLimiter
	outgoingRL  *OutgoingRateLimiter

	platformLifecycleMu sync.Mutex
	platformReady       map[types.Platform]bool
	stopping            bool

	render      *renderer.Renderer
	cmdHandler  *command.Handler
	statManager *interactive.Manager
	msgHandler  *platform.MessageHandler
}

func NewEngine(name string, ag types.Agent, pf types.Platform, sessionStorePath string, lang i18n.Language) *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{
		name:          name, // 项目名
		agent:         ag,
		platform:      pf,
		sessions:      session.NewSessionManager(sessionStorePath),
		ctx:           ctx,
		cancel:        cancel,
		i18n:          i18n.NewI18n(lang),
		commands:      types.NewCommandRegistry(),
		skills:        skill.NewSkillRegistry(),
		platformReady: make(map[types.Platform]bool),
		startedAt:     time.Now(),
		streamPreview: streaming.DefaultStreamPreviewCfg(),
	}
	// 注册回调
	e.render = renderer.NewRenderer(e)
	e.cmdHandler = command.NewHandler(e)
	e.statManager = interactive.NewManager(e)
	e.msgHandler = platform.NewMessageHandle(e)

	return e
}

// ======================= Engine 初始化 =======================

// Engine启动
func (e *Engine) Start() error {
	p := e.platform

	if err := p.Start(e.handleMessage); err != nil {
		slog.Warn("platform start failed", "project", e.name, "platform", p.Name(), "error", err)
		return err
	}
	// 初始化平台（核心：注册cardNavHandler方法)
	e.onPlatformReady(p)

	slog.Info("engine started", "project", e.name, "agent", e.agent.Name(), "platform", e.platform.Name())

	return nil
}

// 标记一个异步平台就绪,并且每次ready循环初始化平台级别的能力
func (e *Engine) onPlatformReady(p types.Platform) {
	if !e.markPlatformReady(p) {
		return
	}
	slog.Info("platform ready", "project", e.name, "platform", p.Name())
	e.initPlatformCapabilities(p)
}

// 尝试标记ready
func (e *Engine) markPlatformReady(p types.Platform) bool {
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

func (e *Engine) initPlatformCapabilities(p types.Platform) {
	// 注册card 处理方法
	if nav, ok := p.(types.CardNavigable); ok {
		nav.SetCardNavigationHandler(e.handleCardNav)
	}
}

// ======================= Engine 变量设置 =======================

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
func (e *Engine) SetLanguageSaveFunc(fn func(i18n.Language) error) {
	e.i18n.SetSaveFunc(fn)
}

func (e *Engine) SetPlatform(p types.Platform) {
	e.platform = p
}

func (e *Engine) SetCronScheduler(cs *cron.CronScheduler) {
	e.cronScheduler = cs
}

// SetAdminFrom 用于设置特权命令的管理员白名单。
// "*" 表示所有通过 allow_from 的用户都是管理员。
// 空字符串表示拒绝所有人执行特权命令。
func (e *Engine) SetAdminFrom(adminFrom string) {
	e.userRolesMu.Lock()
	e.adminFrom = strings.TrimSpace(adminFrom)
	e.userRolesMu.Unlock()
}

func (e *Engine) SetConfigReloadFunc(fn func() (*types.ConfigReloadResult, error)) {
	e.configReloadFunc = fn
}

// 重写默认的truncations 设置
func (e *Engine) SetDisplayConfig(cfg types.DisplayCfg) {
	e.display = &cfg
}

// 配置自动上下文压缩
func (e *Engine) SetAutoCompressConfig(enabled bool, maxTokens int, minGap time.Duration) {
	e.autoCompressEnabled = enabled
	e.autoCompressMaxTokens = maxTokens
	if minGap <= 0 {
		minGap = 30 * time.Minute
	}
	e.autoCompressMinGap = minGap
}

// SetResetOnIdle 配置用于在长时间不活动后自动轮换会话。
// 持续时间为零或负数时，该行为将被禁用。
func (e *Engine) SetResetOnIdle(d time.Duration) {
	if d <= 0 {
		e.resetOnIdle = 0
		return
	}
	e.resetOnIdle = d
}

// SetInjectSender 控制是否在将消息转发给代理之前，将发送者身份（平台和用户 ID）
// 添加到每条消息之前。启用后，代理会收到类似以下的前缀行：
// [cc-connect sender_id=ou_abc123 platform=feishu]
// 这使得代理能够识别消息的发送者，并相应地调整行为（例如，个人任务视图、基于角色的访问控制）。
func (e *Engine) SetInjectSender(v bool) {
	e.injectSender = v
}

// SetUserRoles 用于配置基于用户角色的策略。若要禁用，请传入 nil。
func (e *Engine) SetUserRoles(urm *UserRoleManager) {
	e.userRolesMu.Lock()
	defer e.userRolesMu.Unlock()
	if e.userRoles != nil {
		e.userRoles.Stop()
	}
	e.userRoles = urm
}

// 注册一个自定义/ 命令
func (e *Engine) AddCommand(name, description, prompt, exec, workDir, source string) {
	e.commands.Add(name, description, prompt, exec, workDir, source)
}

func (e *Engine) SetCommandSaveAddFunc(fn func(name, description, prompt, exec, workDir string) error) {
	e.commandSaveAddFunc = fn
}

func (e *Engine) SetCommandSaveDelFunc(fn func(name string) error) {
	e.commandSaveDelFunc = fn
}

// 从给定的源中移除所有commands
func (e *Engine) ClearCommands(source string) {
	e.commands.ClearSource(source)
}

// =================== 外部Context使用方法(renderer cmdHandler...) ===================

func (e *Engine) Agent() types.Agent {
	return e.agent
}

func (e *Engine) AutoCompressEnabled() bool {
	return e.autoCompressEnabled
}

func (e *Engine) AutoCompressMaxTokens() int {
	return e.AutoCompressMaxTokens()
}

func (e *Engine) AutoCompressMinGap() time.Duration {
	return e.autoCompressMinGap
}

func (e *Engine) BaseWorkDir() string {
	return e.baseWorkDir
}

func (e *Engine) CommandRegistry() *types.CommandRegistry {
	return e.commands
}

func (e *Engine) Context() context.Context {
	return e.ctx
}

func (e *Engine) CronScheduler() *cron.CronScheduler {
	return e.cronScheduler
}

func (e *Engine) DisplayCfg() *types.DisplayCfg {
	return e.display
}

func (e *Engine) EventIdleTimeout() time.Duration {
	return e.eventIdleTimeout
}

func (e *Engine) I18n() *i18n.I18n {
	return e.i18n
}

func (e *Engine) InjectSender() bool {
	return e.injectSender
}

func (e *Engine) MsgHandlder() *platform.MessageHandler {
	return e.msgHandler
}

func (e *Engine) Name() string {
	return e.name
}

func (e *Engine) Platform() types.Platform {
	return e.platform
}

func (e *Engine) Renderer() types.Renderer {
	return e.render
}

func (e *Engine) ResetOnIdle() time.Duration {
	return e.resetOnIdle
}

func (e *Engine) Sessions() *session.SessionManager {
	return e.sessions
}

func (e *Engine) ShowContextIndicator() bool {
	return e.showContextIndicator
}

func (e *Engine) SkillRegistry() *skill.SkillRegistry {
	return e.skills
}

func (e *Engine) StartedAt() time.Time {
	return e.startedAt
}

func (e *Engine) StatManager() *interactive.Manager {
	return e.statManager
}

func (e *Engine) StreamPreviewCfg() streaming.StreamPreviewCfg {
	return e.streamPreview
}

func (e *Engine) GetConfigItems() []types.ConfigItem {
	return types.NewConfigItems(
		e.display.ThinkingMessages,
		e.display.ThinkingMaxLen,
		e.display.ToolMessages,
		e.display.ToolMaxLen,
	)
}

func (e *Engine) IsAdmin(userID string) bool {
	e.userRolesMu.RLock()
	af := e.adminFrom
	e.userRolesMu.RUnlock()
	if af == "" {
		return false
	}
	if af == "*" {
		return true
	}
	for _, id := range strings.Split(af, ",") {
		if strings.EqualFold(strings.TrimSpace(id), userID) {
			return true
		}
	}
	return false
}

func (e *Engine) SaveDisplayCfg(tm *bool, tml, tml2 *int, tool *bool) error {
	if e.displaySaveFunc != nil {
		return e.displaySaveFunc(tm, tml, tml2, tool)
	}
	return nil // 或 errors.New("save not supported")
}

func (e *Engine) SaveCommand(name, description, prompt, exec, workDir string) error {
	if e.commandSaveAddFunc != nil {
		if err := e.commandSaveAddFunc(name, "", prompt, "", ""); err != nil {
			slog.Error("failed to persist command", "error", err)
		}
	}
	return nil
}

func (e *Engine) DelCommand(name string) error {
	if err := e.commandSaveDelFunc(name); err != nil {
		slog.Error("failed to persist command removal", "error", err)
	}
	return nil
}

func (e *Engine) ReloadConfig() (*types.ConfigReloadResult, error) {
	if e.configReloadFunc == nil {
		return nil, errors.New("config reload not available")
	}
	return e.configReloadFunc()
}

// =================== Aliases相关方法 ===================

func (e *Engine) GetAliases() map[string]string {
	e.aliasMu.RLock()
	defer e.aliasMu.RUnlock()

	// 返回副本以防外部修改
	result := make(map[string]string, len(e.aliases))
	for k, v := range e.aliases {
		result[k] = v
	}
	return result
}

// ListAliases 返回别名副本
func (e *Engine) ListAliases() map[string]string {
	e.aliasMu.RLock()
	defer e.aliasMu.RUnlock()

	// 返回副本，避免外部修改
	cp := make(map[string]string, len(e.aliases))
	for k, v := range e.aliases {
		cp[k] = v
	}
	return cp
}

// 注册一个别名
func (e *Engine) AddAlias(name, command string) {
	e.aliasMu.Lock()
	defer e.aliasMu.Unlock()
	e.aliases[name] = command
}

// AddAlias 添加别名并持久化
func (e *Engine) SaveAddAlias(name, command string) error {
	e.AddAlias(name, command)

	// 持久化
	if e.aliasSaveAddFunc != nil {
		return e.aliasSaveAddFunc(name, command)
	}
	return nil
}

// DeleteAlias 删除别名并持久化
func (e *Engine) SaveDelAlias(name string) (bool, error) {
	e.aliasMu.Lock()
	_, exists := e.aliases[name]
	if exists {
		delete(e.aliases, name)
	}
	e.aliasMu.Unlock()

	if !exists {
		return false, nil
	}

	// 持久化
	if e.aliasSaveDelFunc != nil {
		return true, e.aliasSaveDelFunc(name)
	}
	return true, nil
}

// 移除所有别名(用于config reload)
func (e *Engine) ClearAliases() {
	e.aliasMu.Lock()
	defer e.aliasMu.Unlock()
	e.aliases = make(map[string]string)
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

// ======================= 频率限制 =======================

// 可用时 阻塞 per-platform outgoing 频率限制
func (e *Engine) WaitOutgoing() error {
	if e.outgoingRL == nil {
		return nil
	}
	return e.outgoingRL.Wait(e.ctx)
}

// 检查是否超出频率限制, 先价差per-user role-base 显示, 然后 回退到全局限制器
func (e *Engine) checkRateLimit(msg *types.Message) bool {
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

// ======================== Engine 消息收发相关 ========================

func (e *Engine) Reply(ctx any, msg string) {
	e.msgHandler.ReplyWithError(e.platform, ctx, msg)
}

// 带platform 消息回复
func (e *Engine) reply(p types.Platform, ctx any, msg string) {
	e.msgHandler.ReplyWithError(p, ctx, msg)
}

// 对p.Send封装,加入错误日志记录,满操作和超速率限制
func (e *Engine) Send(p types.Platform, replyCtx any, content string) {
	_ = e.msgHandler.SendWithError(p, replyCtx, content)
}

// ======================== 内部方法 主Message处理流程 ========================

func (e *Engine) handleMessage(p types.Platform, msg *types.Message) {
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
		e.reply(p, msg.ReplyCtx, e.i18n.T(i18n.MsgRateLimited))
		return
	}

	// 检查黑名单词汇
	if !strings.HasPrefix(content, "/") {
		if word := e.matchBannedWord(content); word != "" {
			slog.Info("message blocked by banned word", "word", word, "user", msg.UserName)
			e.reply(p, msg.ReplyCtx, e.i18n.T(i18n.MsgBannedWordBlocked))
			return
		}
	}

	// FIXME: Multi-workspace
	var resolvedWorkspace string

	if len(msg.Images) == 0 && strings.HasPrefix(content, "/") {
		if e.handleCommand(p, msg, content) {
			return
		}
		// 未识别 slash 命令 - 作为普通消息回退到agent
	}

	// 权限申请处理
	if e.statManager.HandlePendingPermission(p, msg, content) {
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
		if e.statManager.QueueMessageForBusySession(p, msg, interactiveKey) {
			// 竞争保护：processInteractiveMessageWith 中的耗尽循环可能
			// 刚刚在我们的 TryLock 失败和队列添加之间完成（会话已解锁）。
			// 重试 TryLock — 如果成功，则没有人在耗尽队列，因此我们必须自己启动一个处理器。
			if session.TryLock() {
				go e.statManager.DrainOrphanedQueue(session, sessions, interactiveKey, agent, resolvedWorkspace)
			}
			return
		}
		e.reply(p, msg.ReplyCtx, e.i18n.T(i18n.MsgPreviousProcessing))
		return
	}

	// AutoResetSessionOnIdle
	if rotated := e.statManager.MaybeAutoResetSessionOnIdle(p, msg, sessions, interactiveKey, session); rotated != nil {
		session = rotated
	}

	// 在启动异步处理器之前，确保存在一个interactiveState条目，
	// 以便在会话启动期间到达的消息可以排队，而不是被丢弃
	e.statManager.EnsureInteractiveStateForQueueing(interactiveKey, p, msg.ReplyCtx)

	slog.Info("processing message",
		"platform", msg.Platform,
		"user", msg.UserName,
		"session", session.ID,
	)

	// 开始处理 （并发）interactiveKey == msg.SessionKey，传两个值的意义？？
	go e.statManager.ProcessInteractiveMessageWith(p, msg, session, agent, sessions, interactiveKey, resolvedWorkspace, msg.SessionKey)
}

// ======================= 内部方法 Card navigation =======================

// handleCardNav is called by platforms that support in-place card updates.
// It routes nav: and act: prefixed actions to the appropriate render function.
func (e *Engine) handleCardNav(action string, sessionKey string) *types.Card {
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
		return e.render.RenderHelpGroupCard(args)
	case "/model": // NOT SUPPORT
		slog.Error("engine:: not support /model")
		return nil
	case "/reasoning": // NOT SUPPORT
		slog.Error("engine:: not support /reasoning")
		return nil
	case "/mode":
		return e.render.RenderModeCard()
	case "/lang":
		return e.render.RenderLangCard()
	case "/status":
		return e.render.RenderStatusCard(sessionKey, extractUserID(sessionKey))
	case "/list":
		page := 1
		if args != "" {
			if n, err := strconv.Atoi(args); err == nil && n > 0 {
				page = n
			}
		}
		return e.render.RenderListCardSafe(sessionKey, page)
	case "/dir": // NOT SUPPORT
		slog.Error("engine:: not support /dir")
		return nil
	case "/current":
		return e.render.RenderCurrentCard(sessionKey)
	case "/history":
		return e.render.RenderHistoryCard(sessionKey)
	case "/provider": // NOT SUPPORT
		slog.Error("engine:: not support /provider")
		return nil
	case "/cron":
		return e.render.RenderCronCard(sessionKey, extractUserID(sessionKey))
	case "/heartbeat": // NOT SUPPORT
		slog.Error("engine:: do not support /heartbeat")
		return nil
	case "/commands":
		return e.render.RenderCommandsCard()
	case "/alias":
		return e.render.RenderAliasCard()
	case "/config":
		return e.render.RenderConfigCard()
	case "/skills": // NOT SUPPORT
		slog.Error("engine:: not support /skills")
		return nil
	case "/doctor":
		// 职责拆分： IO + (数据 + 渲染)
		results := doctor.RunDoctorChecks(e.ctx, e.agent, e.platform)
		return e.render.RenderDoctorCard(results)
	case "/whoami":
		return e.render.RenderWhoamiCard(&types.Message{
			SessionKey: sessionKey,
			UserID:     extractUserID(sessionKey),
			Platform:   extractPlatformName(sessionKey),
		})
	case "/version":
		return e.render.RenderVersionCard()
	case "/new":
		return e.render.RenderCurrentCard(sessionKey)
	case "/switch":
		return e.render.RenderListCardSafe(sessionKey, 1) // ??
	case "/delete-mode":
		if strings.HasPrefix(args, "cancel") {
			return e.render.RenderListCardSafe(sessionKey, 1)
		}
		return e.render.RenderDeleteModeCard(sessionKey)
	case "/stop":
		return e.render.RenderStatusCard(sessionKey, extractUserID(sessionKey))
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
		slog.Warn("Do not support ModeSwitcher")
		_, ok := e.agent.(types.ModeSwitcher)
		if !ok {
			return
		}
		return

	case "/lang":
		if args == "" {
			return
		}
		target := strings.ToLower(strings.TrimSpace(args))
		var lang i18n.Language
		switch target {
		case "en", "english":
			lang = i18n.LangEnglish
		case "zh", "cn", "chinese":
			lang = i18n.LangChinese
		case "zh-tw", "zh_tw", "zhtw":
			lang = i18n.LangTraditionalChinese
		case "ja", "jp", "japanese":
			lang = i18n.LangJapanese
		case "auto":
			lang = i18n.LangAuto
		default:
			return
		}
		e.i18n.SetLang(lang)

	case "/provider": // NOT SUPPORT
		slog.Error("Do not support to switch provider")
		return

	case "/new":
		sessions := e.sessions
		// 清理当前状态并生成一个新的空session
		e.statManager.CleanupWithLock(sessionKey)
		sessions.NewSession(sessionKey, "")

	case "/delete-mode":
		// 例: /delete-mode toggle <id>
		e.statManager.ExecuteDeleteModeAction(sessionKey, args)

	case "/switch":
		if args == "" {
			return
		}
		agentSessions, err := e.agent.ListSessions()
		if err != nil || len(agentSessions) == 0 {
			return
		}
		// 移除外部CLI创建的session
		agentSessions = types.FilterOwnedSessions(agentSessions, e.sessions.KnownAgentSessionIDs())
		matched := session.MatchSession(agentSessions, e.sessions, args)
		if matched == nil {
			return
		}
		// sessionKey?? 当前sessionKey 还是要切换的key
		e.statManager.CleanupWithLock(sessionKey)
		session := e.sessions.GetOrCreateActive(sessionKey)
		// 设置agentID agentType name
		session.SetAgentInfo(matched.ID, e.agent.Name(), matched.Summary)
		session.ClearHistory()
		e.sessions.Save()

	case "/dir":
		slog.Error("Do not support to change dir ")

	case "/stop":
		e.statManager.StopInteractiveSession(sessionKey, nil)

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

// ======================= 内部方法 command 处理 =======================

func (e *Engine) handleCommand(p types.Platform, msg *types.Message, raw string) bool {
	parts := strings.Fields(raw)
	cmd := strings.ToLower(strings.TrimPrefix(parts[0], "/"))
	args := parts[1:]

	cmdID := matchPrefix(cmd, builtinCommands)

	// FIXME: diableCmds

	// 特殊命令鉴权
	if cmdID != "" && privilegedCommands[cmdID] && !e.IsAdmin(msg.UserID) {
		slog.Info("audit: command_blocked",
			"user_id", msg.UserID, "platform", msg.Platform,
			"project", e.name, "command", cmdID, "reason", "unauthorized")
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(i18n.MsgAdminRequired), "/"+cmdID))
		return true
	}

	if cmdID != "" {
		slog.Info("audit: command_executed",
			"user_id", msg.UserID, "platform", msg.Platform,
			"project", e.name, "command", cmdID)
	}

	switch cmdID {
	case "new":
		e.cmdHandler.CmdNew(msg, args)
	case "list":
		e.cmdHandler.CmdList(p, msg, args)
	case "switch":
		e.cmdHandler.CmdSwitch(msg, args)
	case "name":
		e.cmdHandler.CmdName(msg, args)
	case "current":
		e.cmdHandler.CmdCurrent(p, msg)
	case "status":
		e.cmdHandler.CmdStatus(p, msg)
	case "usage":
		e.cmdHandler.CmdUsage(msg)
	case "history":
		e.cmdHandler.CmdHistory(p, msg, args)
	case "allow":
		e.cmdHandler.CmdAllow(msg, args)
	case "model":
		e.cmdHandler.CmdModel(p, msg, args)
	case "reasoning":
		e.cmdHandler.CmdReasoning(msg)
	case "mode":
		e.cmdHandler.CmdMode(msg)
	case "lang":
		e.cmdHandler.CmdLang(p, msg, args)
	case "quiet":
		e.cmdHandler.CmdQuiet(msg)
	case "provider":
		e.cmdHandler.CmdProvider(msg)
	case "memory":
		e.cmdHandler.CmdMemory(msg, args)
	case "cron":
		e.cmdHandler.CmdCron(p, msg, args)
	case "heartbeat":
		e.cmdHandler.CmdHeartbeat(msg)
	case "compress":
		e.cmdHandler.CmdCompress(msg)
	case "stop":
		e.cmdHandler.CmdStop(msg)
	case "help":
		e.cmdHandler.CmdHelp(p, msg)
	case "version":
		e.reply(p, msg.ReplyCtx, types.VersionInfo)
	case "commands":
		e.cmdHandler.CmdCommands(p, msg, args)
	case "skills":
		e.cmdHandler.CmdSkills(p, msg)
	case "config":
		e.cmdHandler.CmdConfig(p, msg, args)
	case "doctor":
		e.cmdHandler.CmdDoctor(msg)
	case "upgrade":
		e.cmdHandler.CmdUpgrade(msg)
	case "restart":
		e.cmdHandler.CmdRestart(p, msg)
	case "alias":
		e.cmdHandler.CmdAlias(p, msg, args)
	case "delete":
		e.cmdHandler.CmdDelete(p, msg, args)
	// TODO: bind 绑定群聊中的bots
	case "search":
		e.cmdHandler.CmdSearch(msg, args)
	case "shell":
		e.cmdHandler.CmdShell(msg, raw)
	case "diff":
		e.cmdHandler.CmdDiff(p, msg, raw)
	case "show":
		e.cmdHandler.CmdShow(msg, args)
	case "dir":
		e.cmdHandler.CmdDir(msg)
	// TODO: TTS && workspaceDir
	case "whoami":
		e.cmdHandler.CmdWhoami(p, msg)
	case "web":
		e.cmdHandler.CmdWeb(msg, args)
	default:
		if custom, ok := e.commands.Resolve(cmd); ok {
			// TODO: 根据角色禁用某些命令
			slog.Info("audit: command_executed",
				"user_id", msg.UserID, "platform", msg.Platform,
				"project", e.name, "command", custom.Name, "type", "custom")
			e.executeCustomCommand(p, msg, custom, args)
			return true
		}
		if skill := e.skills.Resolve(cmd); skill != nil {
			// TODO: 禁用命令
			slog.Info("audit: command_executed",
				"user_id", msg.UserID, "platform", msg.Platform,
				"project", e.name, "command", skill.Name, "type", "skill")
			e.statManager.ExecuteSkill(p, msg, skill, args)
			return true
		}
		// Not a tc-connect command — notify user, then fall through to agent
		e.Send(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(i18n.MsgUnknownCommand), "/"+cmd))
		return false
	}
	return true
}

func (e *Engine) executeCustomCommand(p types.Platform, msg *types.Message, cmd *types.CustomCommand, args []string) {
	if cmd.Exec != "" && !e.IsAdmin(msg.UserID) {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(i18n.MsgAdminRequired), "/"+cmd.Name))
		return
	}
	// If this is an exec command, run shell command directly
	if cmd.Exec != "" {
		go e.executeShellCommand(p, msg, cmd, args)
		return
	}

	// Otherwise, use prompt template
	prompt := types.ExpandPrompt(cmd.Prompt, args)

	session := e.sessions.GetOrCreateActive(msg.SessionKey)
	if !session.TryLock() {
		e.reply(p, msg.ReplyCtx, e.i18n.T(i18n.MsgPreviousProcessing))
		return
	}

	slog.Info("executing custom command",
		"command", cmd.Name,
		"source", cmd.Source,
		"user", msg.UserName,
	)

	msg.Content = prompt
	go e.StatManager().ProcessInteractiveMessage(p, msg, session)
}

// 执行shell并将结果发送给用户
func (e *Engine) executeShellCommand(p types.Platform, msg *types.Message, cmd *types.CustomCommand, args []string) {
	slog.Info("executing shell command",
		"command", cmd.Name,
		"exec", cmd.Exec,
		"user", msg.UserName,
	)

	// Expand placeholders in exec command
	execCmd := types.ExpandPrompt(cmd.Exec, args)

	// Determine working directory
	workDir := cmd.WorkDir
	if workDir == "" {
		// Default to agent's work_dir if available
		if e.agent != nil {
			if agentOpts, ok := e.agent.(interface{ GetWorkDir() string }); ok {
				workDir = agentOpts.GetWorkDir()
			}
		}
	}
	if workDir == "" {
		workDir, _ = os.Getwd()
	}

	// Create context with timeout
	ctx, cancel := context.WithTimeout(e.ctx, 60*time.Second)
	defer cancel()

	// Execute command using the native shell so Windows config commands work too.
	var shellCmd *exec.Cmd
	if runtime.GOOS == "windows" {
		shellCmd = exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", execCmd)
	} else {
		shellCmd = exec.CommandContext(ctx, "sh", "-c", execCmd)
	}
	shellCmd.Dir = workDir
	envVars := []string{
		"CC_PROJECT=" + e.name,
		"CC_SESSION_KEY=" + msg.SessionKey,
	}
	// Prepend the cc-connect binary dir on Windows only (native shell fix);
	// on Unix it would change command resolution for user scripts.
	if runtime.GOOS == "windows" {
		if exePath, err := os.Executable(); err == nil {
			binDir := filepath.Dir(exePath)
			if curPath := os.Getenv("PATH"); curPath != "" {
				envVars = append(envVars, "PATH="+binDir+string(filepath.ListSeparator)+curPath)
			} else {
				envVars = append(envVars, "PATH="+binDir)
			}
		}
	}
	shellCmd.Env = types.MergeEnv(os.Environ(), envVars)
	output, err := shellCmd.CombinedOutput()

	if ctx.Err() == context.DeadlineExceeded {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(i18n.MsgCommandExecTimeout), cmd.Name))
		return
	}

	if err != nil {
		errMsg := string(output)
		if errMsg == "" {
			errMsg = err.Error()
		}
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(i18n.MsgCommandExecError), cmd.Name, types.TruncateStr(errMsg, 1000)))
		return
	}

	result := strings.TrimSpace(string(output))
	if result == "" {
		result = e.i18n.T(i18n.MsgCommandExecSuccess)
	} else if len(result) > 4000 {
		result = result[:3997] + "..."
	}

	e.reply(p, msg.ReplyCtx, result)
}

// ======================= 禁用词处理 =======================

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

// // ======================== Engine CronJob相关 ========================

// 忽略所有outgoing message的Platform, 用于静音不应发送对话消息的静音定时任务
type mutePlatform struct {
	types.Platform
}

// // 通过注入同步消息到engine运行一个定时任务, 重构reply context, 处理消息就像用户发送的它
func (e *Engine) ExecuteCronJob(job *cron.CronJob) error {
	sessionKey := job.SessionKey
	platformName := ""
	if idx := strings.Index(sessionKey, ":"); idx > 0 {
		platformName = sessionKey[:idx]
	}

	var targetPlatform types.Platform
	if e.platform.Name() == platformName {
		targetPlatform = e.platform
	}

	if targetPlatform == nil {
		return fmt.Errorf("platform %q not match for session %q", platformName, sessionKey)
	}

	rc, ok := targetPlatform.(types.ReplyContextReconstructor)
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
					desc = types.TruncateStr(job.Exec, 40)
				} else {
					desc = types.TruncateStr(job.Prompt, 40)
				}
			}
			e.msgHandler.SendWithError(targetPlatform, replyCtx, fmt.Sprintf("⏰ %s", desc))
		}
	}

	if job.IsShellJob() {
		return e.executeCronShell(effectivePlatform, replyCtx, job)
	}

	msg := &types.Message{
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
		e.statManager.ProcessInteractiveMessageWith(effectivePlatform, msg, session, e.agent, e.sessions, iKey, "", runSessionKey)
		e.statManager.CleanupWithLock(iKey)
		return nil
	}

	session := e.sessions.GetOrCreateActive(sessionKey)
	if !session.TryLock() {
		return fmt.Errorf("session %q is busy", sessionKey)
	}

	e.statManager.ProcessInteractiveMessageWith(effectivePlatform, msg, session, e.agent, e.sessions, sessionKey, "", sessionKey)
	return nil

}

// 运行shell命令,发送输出
func (e *Engine) executeCronShell(p types.Platform, replyCtx any, job *cron.CronJob) error {
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
		e.msgHandler.SendWithError(p, replyCtx, fmt.Sprintf("⏰ ⚠️ timeout: `%s`", types.TruncateStr(job.Exec, 60)))
		return fmt.Errorf("shell command timed out")
	}

	result := strings.TrimSpace(string(output))
	if err != nil {
		if result != "" {
			e.msgHandler.SendWithError(p, replyCtx, fmt.Sprintf("⏰ ❌ `%s`\n\n%s\n\nerror: %v", types.TruncateStr(job.Exec, 60), types.TruncateStr(result, 3000), err))
		} else {
			e.msgHandler.SendWithError(p, replyCtx, fmt.Sprintf("⏰ ❌ `%s`\nerror: %v", types.TruncateStr(job.Exec, 60), err))
		}
		return fmt.Errorf("shell: %w", err)
	}

	if result == "" {
		result = "(no output)"
	}
	e.msgHandler.SendWithError(p, replyCtx, fmt.Sprintf("⏰ ✅ `%s`\n\n%s", types.TruncateStr(job.Exec, 60), types.TruncateStr(result, 3000)))
	return nil
}
