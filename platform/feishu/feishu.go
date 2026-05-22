package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"tc-connect/core"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkapplication "github.com/larksuite/oapi-sdk-go/v3/service/application/v6"
	larkcontact "github.com/larksuite/oapi-sdk-go/v3/service/contact/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// ===================== 常量 =============================
// 交互卡片限制table 最多5个, 超过造成API error 11310
const maxCardTables = 5

// 网络级故障的瞬态重试常数
const (
	maxTransientRetries    = 3
	transientRetryInitial  = 500 * time.Millisecond
	transientRetryMaxDelay = 5 * time.Second
)

// ==================== 用于larkws.NewClient的日志 ====================

// sanitizingLogger 包装logger，标注URL参数
type sanitizingLogger struct {
	inner larkcore.Logger
}

func (l *sanitizingLogger) maskURL(args ...interface{}) []interface{} {
	masked := make([]interface{}, len(args))
	for i, arg := range args {
		if s, ok := arg.(string); ok {
			masked[i] = l.sanitize(s)
		} else {
			masked[i] = arg
		}
	}
	return masked
}

func (l *sanitizingLogger) sanitize(s string) string {
	// Mask sensitive query parameters in URLs
	sensitiveParams := []string{
		"device_id=", "access_key=", "ticket=", "conn_id=",
		"secret=", "token=", "password=", "key=",
	}
	for _, param := range sensitiveParams {
		if idx := strings.Index(s, param); idx != -1 {
			// Find the end of the value (either & or end of string)
			end := idx + len(param)
			for end < len(s) && s[end] != '&' && s[end] != ' ' {
				end++
			}
			s = s[:idx+len(param)] + "***" + s[end:]
		}
	}
	return s
}

func (l *sanitizingLogger) Debug(ctx context.Context, args ...interface{}) {
	for _, arg := range args {
		s, ok := arg.(string)
		if !ok {
			continue
		}
		msg := strings.ToLower(s)
		if strings.Contains(msg, "ping success") || strings.Contains(msg, "receive pong") {
			return
		}
	}
	l.inner.Debug(ctx, l.maskURL(args...)...)
}

func (l *sanitizingLogger) Info(ctx context.Context, args ...interface{}) {
	l.inner.Info(ctx, l.maskURL(args...)...)
}

func (l *sanitizingLogger) Warn(ctx context.Context, args ...interface{}) {
	l.inner.Warn(ctx, l.maskURL(args...)...)
}

func (l *sanitizingLogger) Error(ctx context.Context, args ...interface{}) {
	l.inner.Error(ctx, l.maskURL(args...)...)
}

// 存储消息ID哟ing与可编辑的预览消息
type feishuPreviewHandle struct {
	messageID string
	chatID    string
}

// ==================== Feishu Platform  ====================

func init() {
	core.RegisterPlatform("feishu", func(opts map[string]any) (core.Platform, error) {
		return newPlatform("feishu", lark.FeishuBaseUrl, opts)
	})
}

type replyContext struct {
	messageID  string
	chatID     string
	sessionKey string
}

type Platform struct {
	platformName          string
	domain                string
	appID                 string
	appSecret             string
	progressStyle         string
	useInteractiveCard    bool // 使用交互卡片
	self                  core.Platform
	reactionEmoji         string
	shareSessionInChannel bool
	threadIsolation       bool
	// 当为true时，通过Create而非Im.Message.Reply发送（不对用户消息进行引用）。
	noReplyToTrigger bool

	client         *lark.Client
	replayClient   *lark.Client
	replayClientMu sync.Mutex
	wsClient       *larkws.Client
	cancel         context.CancelFunc
	dedup          core.MessageDedup

	handler        core.MessageHandler
	cardNavHandler core.CardNavigationHandler

	botOpenID     string
	userNameCache sync.Map // open_id -> display name
	chatNameCache sync.Map // chat_id -> chat name

	encryptKey   string
	eventHandler *dispatcher.EventDispatcher
}

type feishuRequestFunc func(client *lark.Client, options ...larkcore.RequestOptionFunc) error

func newPlatform(name, domain string, opts map[string]any) (core.Platform, error) {
	// 解析 app_id | app_secret | domain
	appID, _ := opts["app_id"].(string)
	appSecret, _ := opts["app_secret"].(string)
	if appID == "" || appSecret == "" {
		return nil, fmt.Errorf("%s: app_id and app_secret are required", name)
	}
	if v, ok := opts["domain"].(string); ok {
		v = strings.TrimSpace(v)
		if v != "" {
			if _, err := url.ParseRequestURI(v); err != nil {
				return nil, fmt.Errorf("%s: invalid domain %q: %w", name, v, err)
			}
			domain = v
		}
	}

	reactionEmoji := "OnIt"
	if v, ok := opts["reaction_emoji"].(string); ok && v == "none" {
		reactionEmoji = ""
	}
	// TODO: 解析其他一些参数
	encryptKey, _ := opts["encrypt_key"].(string)
	threadIsolation, _ := opts["thread_isolation"].(bool)
	shareSessionInChannel, _ := opts["share_session_in_channel"].(bool)

	// 给lark.Client实例创建准备参数
	var clientOpts []lark.ClientOptionFunc
	if domain != lark.FeishuBaseUrl {
		clientOpts = append(clientOpts, lark.WithOpenBaseUrl(domain))
	}

	base := &Platform{
		platformName:          name,
		domain:                domain,
		appID:                 appID,
		appSecret:             appSecret,
		reactionEmoji:         reactionEmoji,
		shareSessionInChannel: shareSessionInChannel,
		threadIsolation:       threadIsolation,
		client:                lark.NewClient(appID, appSecret, clientOpts...),
		replayClient:          newFeishuReplayClient(appID, appSecret, domain),
		encryptKey:            encryptKey,
	}
	base.self = base
	return base, nil
}

// ==================== 公开方法（首字母大写） ====================

func (p *Platform) Name() string { return p.platformName }

func (p *Platform) ProgressStyle() string { return p.progressStyle }

func (p *Platform) KeepPreviewOnFinish() bool {
	return p.useInteractiveCard
}

// 赋值handler 启动服务
func (p *Platform) Start(handler core.MessageHandler) error {
	// 设置messageHandler
	p.handler = handler

	// 获取飞书 bot openid
	if openID, err := p.fetchBotOpenID(); err != nil {
		slog.Warn(p.platformName+": failed to get bot open_id, group chat filtering disabled", "error", err)
	} else {
		p.botOpenID = openID
		slog.Info(p.platformName+": bot identified", "open_id", openID)
	}
	p.eventHandler = dispatcher.NewEventDispatcher("", p.encryptKey).
		OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
			slog.Debug("===========================================>")
			slog.Debug(p.platformName+": message received", "app_id", p.appID)
			return p.onMessage(ctx, event)
		}).
		OnP2MessageReadV1(func(ctx context.Context, event *larkim.P2MessageReadV1) error {
			return nil // ignore read receipts
		}).
		OnP2ChatAccessEventBotP2pChatEnteredV1(func(ctx context.Context, event *larkim.P2ChatAccessEventBotP2pChatEnteredV1) error {
			slog.Debug(p.platformName+": user opened bot chat", "app_id", p.appID)
			return nil
		}).
		OnP1P2PChatCreatedV1(func(ctx context.Context, event *larkim.P1P2PChatCreatedV1) error {
			slog.Debug(p.platformName+": p2p chat created", "app_id", p.appID)
			return nil
		}).
		OnP2MessageReactionCreatedV1(func(ctx context.Context, event *larkim.P2MessageReactionCreatedV1) error {
			return nil // ignore reaction events (triggered by our own addReaction)
		}).
		OnP2MessageReactionDeletedV1(func(ctx context.Context, event *larkim.P2MessageReactionDeletedV1) error {
			return nil // ignore reaction removal events (triggered by our own removeReaction)
		}).
		OnP2CardActionTrigger(func(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
			return p.onCardAction(event)
		}).
		OnP2BotMenuV6(func(ctx context.Context, event *larkapplication.P2BotMenuV6) error {
			return p.onBotMenu(event)
		})

	return p.startWebSocketMode()
}

func (p *Platform) Reply(ctx context.Context, rctx any, content string) error {
	// 类型转换
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("%s: invalid reply context type %T", p.tag(), rctx)
	}
	// 根据content构建replyContext
	msgType, msgBody := buildReplyContent(content)

	if !p.shouldUseThreadOrReplyAPI(rc) {
		return p.sendNewMessageToChat(ctx, rc, msgType, msgBody)
	}
	return p.replyMessage(ctx, rc, msgType, msgBody)
}

// 发送message，当原始messageID可用时，message作为一个reply (引号原始内容) ，这样对话
// 能保持连贯。当没有 messageID时 回退到创建一个独立的message
func (p *Platform) Send(ctx context.Context, rctx any, content string) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("%s: invalid reply context type %T", p.tag(), rctx)
	}

	if p.shouldUseThreadOrReplyAPI(rc) {
		return p.Reply(ctx, rctx, content)
	}

	msgType, msgBody := buildReplyContent(content)
	return p.sendNewMessageToChat(ctx, rc, msgType, msgBody)
}

func (p *Platform) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	return nil
}

func (p *Platform) SetCardNavigationHandler(h core.CardNavigationHandler) {
	p.cardNavHandler = h
}

func (p *Platform) ReconstructReplyCtx(sessionKey string) (any, error) {
	// {platformName}:{chatID}:{userID}
	parts := strings.SplitN(sessionKey, ":", 3)
	if len(parts) < 2 || parts[0] != p.platformName {
		return nil, fmt.Errorf("%s: invalid session key %q", p.tag(), sessionKey)
	}
	rc := replyContext{chatID: parts[1], sessionKey: sessionKey}
	if len(parts) == 3 {
		if rootID, ok := parseThreadRootID(parts[2]); ok {
			rc.messageID = rootID
		}
	}
	return rc, nil
}

// 给用户的消息一个emoji响应并返回一个stop func 当处理完成时移除
func (p *Platform) StartTyping(ctx context.Context, rctx any) (stop func()) {
	rc, ok := rctx.(replyContext)
	if !ok || rc.messageID == "" {
		return func() {}
	}
	reactionID := p.addReaction(rc.messageID)
	return func() {
		go p.removeReaction(rc.messageID, reactionID)
	}
}

// 移除review消息,这样调用这可以发送一个分离的最终消息而不会留下一张过时的互动卡片
func (p *Platform) DeletePreviewMessage(ctx context.Context, previewHandle any) error {
	if !p.useInteractiveCard {
		return errors.New("operation not support by this platform")
	}

	h, ok := previewHandle.(*feishuPreviewHandle)
	if !ok {
		return fmt.Errorf("%s: invalid preview handle type %T", p.tag(), previewHandle)
	}

	req := larkim.NewDeleteMessageReqBuilder().
		MessageId(h.messageID).
		Build()
	return p.withTransientRetry(ctx, "delete preview message", func() error {
		return p.withFreshTenantAccessTokenRetry(ctx, "delete preview message", func(client *lark.Client, options ...larkcore.RequestOptionFunc) error {
			resp, err := client.Im.Message.Delete(ctx, req, options...)
			if err != nil {
				return fmt.Errorf("%s: delete preview message: %w", p.tag(), err)
			}
			if !resp.Success() {
				return fmt.Errorf("%s: delete preview message code=%d msg=%s", p.tag(), resp.Code, resp.Msg)
			}
			return nil
		})
	})
}

// ==================== 私有方法（首字母小写） =============

// 注册eventHandler, 启动wsClinet
func (p *Platform) startWebSocketMode() error {
	wsOpts := []larkws.ClientOption{
		larkws.WithEventHandler(p.eventHandler),
		larkws.WithLogLevel(larkcore.LogLevelInfo),
		larkws.WithLogger(&sanitizingLogger{inner: larkcore.NewEventLogger()}),
	}
	p.wsClient = larkws.NewClient(p.appID, p.appSecret, wsOpts...)

	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel

	go func() {
		if err := p.wsClient.Start(ctx); err != nil {
			slog.Error(p.tag()+": websocket error", "error", err)
		}
	}()

	return nil
}

// 构建 replyConent
func buildReplyContent(content string) (msgType string, body string) {
	// 没有markdown 则 生成{"text": content} jsonStr 返回
	if !containsMarkdown(content) {
		b, _ := json.Marshal(map[string]string{"text": content})
		return larkim.MsgTypeText, string(b)
	}
	// 三层渲染策略:
	// 1. Code blocks / tables → card (schema 2.0 markdown)
	// 2. Many \n\n paragraphs (help, status, etc.) → post rich-text (preserves blank lines)
	// 3. Other markdown → post md tag (best native rendering)
	//
	// 飞书卡片支持最多5个table
	// Feishu cards support at most 5 tables (API error 11310).
	// When content exceeds this limit, fall back to post with md tag
	// which still renders tables without the card table cap.
	if hasComplexMarkdown(content) && countMarkdownTables(content) <= maxCardTables {
		return larkim.MsgTypeInteractive, buildCardJSON(sanitizeMarkdownURLs(preprocessFeishuMarkdown(content)))
	}
	if strings.Count(content, "\n\n") >= 2 {
		return larkim.MsgTypePost, buildPostJSON(content)
	}
	return larkim.MsgTypePost, buildPostMdJSON(content)
}

func (p *Platform) dispatchPlatform() core.Platform {
	if p.self != nil {
		return p.self
	}
	return p
}

// ==================== 私有方法（markdown相关） ====================

// 根据 "```" '|' 判断需要渲染的表格
func hasComplexMarkdown(s string) bool {
	if strings.Contains(s, "```") {
		return true
	}
	// Table: line starting and ending with |
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) > 1 && trimmed[0] == '|' && trimmed[len(trimmed)-1] == '|' {
			return true
		}
	}
	return false
}

// 使用md tag 构建飞书post格式，用标准chat字体渲染markdown
func buildPostMdJSON(content string) string {
	content = sanitizeMarkdownURLs(content)
	post := map[string]any{
		"zh_cn": map[string]any{
			"content": [][]map[string]any{
				{
					{"tag": "md", "text": content},
				},
			},
		},
	}
	b, _ := json.Marshal(post)
	return string(b)
}

// s中有多少markdown tables
// table定义： 连续行组 以 '|' 开始和结束
func countMarkdownTables(s string) int {
	count := 0
	inTable := false
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		isTableLine := len(trimmed) > 1 && trimmed[0] == '|' && trimmed[len(trimmed)-1] == '|'
		if isTableLine && !inTable {
			count++
			inTable = true
		} else if !isTableLine {
			inTable = false
		}
	}
	return count
}

// 通过 Feishu bot info API 获取openid
func (p *Platform) fetchBotOpenID() (string, error) {
	resp, err := p.client.Get(context.Background(),
		"/open-apis/bot/v3/info", nil, larkcore.AccessTokenTypeTenant)
	if err != nil {
		return "", fmt.Errorf("api call: %w", err)
	}
	var result struct {
		Code int `json:"code"`
		Bot  struct {
			OpenID string `json:"open_id"`
		} `json:"bot"`
	}
	if err := json.Unmarshal(resp.RawBody, &result); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("api code=%d", result.Code)
	}
	return result.Bot.OpenID, nil
}

// 当应调用Im.Message.Reply (ReplyInThread可选) 返回true
func (p *Platform) shouldUseThreadOrReplyAPI(rc replyContext) bool {
	if rc.messageID == "" {
		return false
	}
	return !p.noReplyToTrigger
}

// ==================== 私有方法（message相关） ====================

func (p *Platform) tag() string { return p.platformName }

// 接收到消息之后的响应
func (p *Platform) onMessage(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
	msg := event.Event.Message
	sender := event.Event.Sender
	// 从msg中获取对应的字段
	msgType := ""
	if msg.MessageType != nil {
		msgType = *msg.MessageType
	}
	chatID := ""
	if msg.ChatId != nil {
		chatID = *msg.ChatId
	}
	userID := ""
	if sender.SenderId != nil && sender.SenderId.OpenId != nil {
		userID = *sender.SenderId.OpenId
	}

	// userName 和 chatName 在 dispatchMessage 中解析 来避免同步HTTP调用阻塞SDK调度器协程
	messageID := ""
	if msg.MessageId != nil {
		messageID = *msg.MessageId
	}
	// 重复消息过滤：防止 SDK 重连或网络抖动导致消息重复处理
	if p.dedup.IsDuplicate(messageID) {
		slog.Debug(p.tag()+": duplicate message ignored", "message_id", messageID)
		return nil
	}
	// 历史消息过滤：重启后忽略启动前的旧消息，避免重复处理
	if msg.CreateTime != nil {
		if ms, err := strconv.ParseInt(*msg.CreateTime, 10, 64); err == nil {
			msgTime := time.Unix(ms/1000, (ms%1000)*int64(time.Millisecond))
			if core.IsOldMessage(msgTime) {
				slog.Debug(p.tag()+": ignoring old message after restart", "create_time", *msg.CreateTime)
				return nil
			}
		}
	}
	chatType := ""
	if msg.ChatType != nil {
		chatType = *msg.ChatType
	}
	mentionCount := len(msg.Mentions)
	slog.Debug(p.tag()+": inbound message",
		"message_id", messageID,
		"chat_id", chatID,
		"chat_type", chatType,
		"root_id", stringValue(msg.RootId),
		"thread_id", stringValue(msg.ThreadId),
		"parent_id", stringValue(msg.ParentId),
		"mentions", mentionCount,
	)

	if msg.Content == nil && msgType != "merge_forward" {
		slog.Debug(p.tag()+": message content is nil", "message_id", messageID, "type", msgType)
		return nil
	}

	// 执行异步之前先捕获内容 - SDK 可能重新使用event对象
	// 数据捕获：SDK 可能重用 event 对象，需提前保存引用避免竞态条件
	content := ""
	if msg.Content != nil {
		content = *msg.Content
	}
	mentions := msg.Mentions
	parentID := stringValue(msg.ParentId)
	// 会话键生成：根据 thread_isolation 配置决定会话隔离粒度（根线程/会话/用户）
	sessionKey := p.makeSessionKey(msg, chatID, userID)
	rctx := replyContext{messageID: messageID, chatID: chatID, sessionKey: sessionKey}
	slog.Debug(p.tag()+": routed inbound message",
		"message_id", messageID,
		"session_key", sessionKey,
		"reply_in_thread", p.shouldReplyInThread(rctx),
	)

	// 异步分发消息处理，这样SDK event loop 不会被IO-heavy操作（文件下载/处理HTTP调用）阻塞
	// 上面的dedup和old-message 检查保持同步来保证生成goroutine之前的正确性
	go p.dispatchMessage(ctx, msgType, content, mentions, messageID, sessionKey, userID, chatID, rctx, parentID)

	return nil
}

func (p *Platform) dispatchMessage(ctx context.Context, msgType, content string, mentions []*larkim.MentionEvent, messageID, sessionKey, userID, chatID string, rctx replyContext, parentID string) {
	// Resolve user and chat names asynchronously so SDK dispatcher is not blocked.
	userName := ""
	if userID != "" {
		userName = p.resolveUserName(userID)
	}
	chatName := p.resolveChatName(chatID)

	// If this message is a reply to another message, fetch the quoted content
	// and prepend it so the agent has full context.
	quotedPrefix := ""
	if parentID != "" {
		quotedPrefix = p.fetchQuotedMessage(ctx, parentID)
	}

	switch msgType {
	case "text":
		var textBody struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(content), &textBody); err != nil {
			slog.Error(p.tag()+": failed to parse text content", "error", err)
			return
		}
		text := stripMentions(textBody.Text, mentions, p.botOpenID)
		if text == "" {
			slog.Debug(p.tag()+": dropping empty text after mention stripping",
				"message_id", messageID,
				"raw_text_len", len(textBody.Text),
				"mentions", len(mentions),
			)
			return
		}
		p.handler(p.dispatchPlatform(), &core.Message{
			SessionKey: sessionKey, Platform: p.platformName,
			MessageID: messageID,
			UserID:    userID, UserName: userName, ChatName: chatName,
			Content: quotedPrefix + text, ReplyCtx: rctx,
		})

	case "image":
		slog.Warn("Do not support image content.")
		return

	case "audio":
		slog.Warn("Do not support audio content.")
		return

	case "post":
		slog.Warn("Do not support post content.")
		return

	case "file":
		slog.Warn("Do not support file content.")
		return
	case "merge_forward":
		slog.Warn("Do not support merge_forward content.")
		return

	default:
		slog.Debug(p.tag()+": ignoring unsupported message type", "type", msgType)
	}
}

// 通过Content API /cache 解析userName
func (p *Platform) resolveUserName(openID string) string {
	if cached, ok := p.userNameCache.Load(openID); ok {
		return cached.(string)
	}
	resp, err := p.client.Contact.User.Get(context.Background(),
		larkcontact.NewGetUserReqBuilder().
			UserId(openID).
			UserIdType("open_id").
			Build())
	if err != nil {
		slog.Debug(p.tag()+": resolve user name failed", "open_id", openID, "error", err)
		return openID
	}
	if !resp.Success() || resp.Data == nil || resp.Data.User == nil || resp.Data.User.Name == nil {
		slog.Debug(p.tag()+": resolve user name: no data", "open_id", openID, "code", resp.Code)
		return openID
	}
	name := *resp.Data.User.Name
	p.userNameCache.Store(openID, name)
	return name
}

// 通过IM API /cache 获取 chat/group 的名称
func (p *Platform) resolveChatName(chatID string) string {
	if chatID == "" {
		return ""
	}
	if cached, ok := p.chatNameCache.Load(chatID); ok {
		return cached.(string)
	}
	resp, err := p.client.Im.Chat.Get(context.Background(),
		larkim.NewGetChatReqBuilder().ChatId(chatID).Build())
	if err != nil {
		slog.Debug(p.tag()+": resolve chat name failed", "chat_id", chatID, "error", err)
		return chatID
	}
	if !resp.Success() || resp.Data == nil || resp.Data.Name == nil {
		slog.Debug(p.tag()+": resolve chat name: no data", "chat_id", chatID, "code", resp.Code)
		return chatID
	}
	name := *resp.Data.Name
	if name == "" {
		return chatID
	}
	p.chatNameCache.Store(chatID, name)
	return name
}

// 检索用户正在回复的父消息的内容， 返回一个格式化的前缀字符串用于内容注入
// 失败返回空字符串（优雅回退 - 用户自己的信息仍然不带引号提供）
func (p *Platform) fetchQuotedMessage(ctx context.Context, parentID string) string {
	// 使用带有card_msg_content_type=raw_card_content的原生API调用，这样
	// 交互式卡片消息会返回完整的卡片JSON数据（包含 json_card 字段）而不是简化的最终状态
	// 调用API
	apiPath := fmt.Sprintf("/open-apis/im/v1/messages/%s?card_msg_content_type=raw_card_content", parentID)
	apiResp, err := p.client.Get(ctx, apiPath, nil, larkcore.AccessTokenTypeTenant)
	if err != nil {
		slog.Debug(p.tag()+": fetch quoted message failed", "parent_id", parentID, "error", err)
		return ""
	}
	// 解析获取的JSON数据
	var resp struct {
		Code int `json:"code"`
		Data struct {
			Items []struct {
				MsgType string `json:"msg_type"`
				Sender  struct {
					ID string `json:"id"`
				} `json:"sender"`
				Body struct {
					Content string `json:"content"`
				} `json:"body"`
				Mentions []*larkim.Mention `json:"mentions"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(apiResp.RawBody, &resp); err != nil || resp.Code != 0 || len(resp.Data.Items) == 0 {
		slog.Debug(p.tag()+": fetch quoted message: parse failed or no data", "parent_id", parentID)
		return ""
	}

	item := resp.Data.Items[0]
	msgType := item.MsgType
	content := item.Body.Content
	if content == "" {
		return ""
	}

	// 基于消息类型提取 plain text
	var quotedText string
	switch msgType {
	case "text":
		var textBody struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(content), &textBody); err == nil {
			quotedText = replaceMentions(textBody.Text, item.Mentions)
		}
	case "post":
		// Rich text — extract text elements from the post structure.
		quotedText = extractPostPlainText(content)
	case "interactive":
		quotedText = extractInteractiveCardText(content)
	default:
		// For non-text types (image, file, audio, etc.), use a type indicator.
		quotedText = fmt.Sprintf("[%s]", msgType)
	}

	if quotedText == "" {
		return ""
	}

	// 解析 sender name
	senderName := ""
	if item.Sender.ID != "" {
		senderName = p.resolveUserName(item.Sender.ID)
	}
	if senderName == "" {
		senderName = "unknown"
	}

	return fmt.Sprintf("[Quoted message from %s]:\n%s\n\n", senderName, quotedText)
}

// 向chat发送Message
func (p *Platform) sendNewMessageToChat(ctx context.Context, rc replyContext, msgType, content string) error {
	if rc.chatID == "" {
		return fmt.Errorf("%s: chatID is empty, cannot send new message", p.tag())
	}
	return p.createMessage(ctx, rc.chatID, msgType, content, "send")
}

func (p *Platform) createMessage(ctx context.Context, chatID, msgType, content, op string) error {
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.ReceiveIdTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType(msgType).
			Content(content).
			Build()).
		Build()
	return p.withTransientRetry(ctx, op, func() error {
		return p.withFreshTenantAccessTokenRetry(ctx, op, func(client *lark.Client, options ...larkcore.RequestOptionFunc) error {
			resp, err := client.Im.Message.Create(ctx, req, options...)
			if err != nil {
				return fmt.Errorf("%s: %s api call: %w", p.tag(), op, err)
			}
			if !resp.Success() {
				return fmt.Errorf("%s: %s failed code=%d msg=%s", p.tag(), op, resp.Code, resp.Msg)
			}
			return nil
		})
	})
}

func (p *Platform) withFreshTenantAccessTokenRetry(ctx context.Context, operation string, fn feishuRequestFunc) error {
	err := fn(p.client)
	if !isTenantAccessTokenInvalid(err) {
		return err
	}

	freshToken, refreshErr := p.fetchFreshTenantAccessToken(ctx)
	if refreshErr != nil {
		return fmt.Errorf("%s: %s failed after token refresh attempt: %w (original error: %v)", p.tag(), operation, refreshErr, err)
	}

	slog.Warn(p.tag()+": retrying request with fresh tenant access token", "operation", operation)
	return fn(p.replayAPIClient(), larkcore.WithTenantAccessToken(freshToken))
}

// 拉取刷新AccessToken
func (p *Platform) fetchFreshTenantAccessToken(ctx context.Context) (string, error) {
	resp, err := p.replayAPIClient().GetTenantAccessTokenBySelfBuiltApp(ctx, &larkcore.SelfBuiltTenantAccessTokenReq{
		AppID:     p.appID,
		AppSecret: p.appSecret,
	})
	if err != nil {
		return "", fmt.Errorf("%s: fetch tenant access token: %w", p.tag(), err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("%s: fetch tenant access token code=%d msg=%s", p.tag(), resp.Code, resp.Msg)
	}
	if strings.TrimSpace(resp.TenantAccessToken) == "" {
		return "", fmt.Errorf("%s: fetch tenant access token returned empty token", p.tag())
	}
	return resp.TenantAccessToken, nil
}

// 创建用于replay的lark.Client
func (p *Platform) replayAPIClient() *lark.Client {
	p.replayClientMu.Lock()
	defer p.replayClientMu.Unlock()
	if p.replayClient == nil {
		p.replayClient = newFeishuReplayClient(p.appID, p.appSecret, p.domain)
	}
	return p.replayClient
}

// 根据 p的threadIsolation 属性以及sessionKey 是否含有 root thread 字段来判断是否应该reply
func (p *Platform) shouldReplyInThread(rc replyContext) bool {
	if rc.messageID == "" {
		return false
	}
	return p.threadIsolation && isThreadSessionKey(rc.sessionKey)
}

// 对暂时性网络错误采用指数退避重试机制来包装操作。非暂时性错误则立即返回。
// 添加抖动（最高可达延迟的+25%）以防止羊群效应重试
func (p *Platform) withTransientRetry(ctx context.Context, operation string, fn func() error) error {
	var lastErr error
	delay := transientRetryInitial
	for attempt := 0; attempt <= maxTransientRetries; attempt++ {
		lastErr = fn()
		if lastErr == nil {
			if attempt > 0 {
				slog.Info(p.tag()+": transient retry succeeded",
					"operation", operation,
					"attempt", attempt+1,
				)
			}
			return nil
		}
		if !isTransientError(lastErr) {
			return lastErr
		}
		if attempt == maxTransientRetries {
			break
		}
		// Add jitter: up to +25% of delay to spread out concurrent retries.
		jitter := time.Duration(rand.Int64N(int64(delay / 4)))
		actualDelay := delay + jitter
		slog.Warn(p.tag()+": transient error, retrying",
			"operation", operation,
			"attempt", attempt+1,
			"max_retries", maxTransientRetries,
			"delay", actualDelay,
			"error", lastErr,
		)
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %s retry cancelled: %w (last error: %v)", p.tag(), operation, ctx.Err(), lastErr)
		case <-time.After(actualDelay):
		}
		delay = min(delay*2, transientRetryMaxDelay)
	}
	return fmt.Errorf("%s failed after %d retries: %w", operation, maxTransientRetries, lastErr)
}

// TODO: 会话密钥推导和回复线程行为在此处被拆分到多个代码路径中。
// 在不改变 thread_isolation=false 的行为的前提下，应重新审视线程/根处理。
func (p *Platform) makeSessionKey(msg *larkim.EventMessage, chatID, userID string) string {
	if p.threadIsolation && msg != nil && stringValue(msg.ChatType) == "group" {
		rootID := stringValue(msg.RootId)
		if rootID == "" {
			rootID = stringValue(msg.MessageId)
		}
		if rootID != "" {
			return fmt.Sprintf("%s:%s:root:%s", p.tag(), chatID, rootID)
		}
	}
	if p.shareSessionInChannel {
		return fmt.Sprintf("%s:%s", p.tag(), chatID)
	}
	return fmt.Sprintf("%s:%s:%s", p.tag(), chatID, userID)
}

func (p *Platform) buildReplyMessageReqBody(rc replyContext, msgType, content string) *larkim.ReplyMessageReqBody {
	body := larkim.NewReplyMessageReqBodyBuilder().
		MsgType(msgType).
		Content(content)
	if p.shouldReplyInThread(rc) {
		body.ReplyInThread(true)
	}
	return body.Build()
}

func (p *Platform) replyMessage(ctx context.Context, rc replyContext, msgType, content string) error {
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(rc.messageID).
		Body(p.buildReplyMessageReqBody(rc, msgType, content)).
		Build()
	return p.withTransientRetry(ctx, "reply", func() error {
		return p.withFreshTenantAccessTokenRetry(ctx, "reply", func(client *lark.Client, options ...larkcore.RequestOptionFunc) error {
			resp, err := client.Im.Message.Reply(ctx, req, options...)
			if err != nil {
				return fmt.Errorf("%s: reply api call: %w", p.tag(), err)
			}
			if !resp.Success() {
				return fmt.Errorf("%s: reply failed code=%d msg=%s", p.tag(), resp.Code, resp.Msg)
			}
			return nil
		})
	})
}

func (p *Platform) addReaction(messageID string) string {
	if p.reactionEmoji == "" {
		return ""
	}
	emojiType := p.reactionEmoji
	resp, err := p.client.Im.MessageReaction.Create(context.Background(),
		larkim.NewCreateMessageReactionReqBuilder().
			MessageId(messageID).
			Body(larkim.NewCreateMessageReactionReqBodyBuilder().
				ReactionType(&larkim.Emoji{EmojiType: &emojiType}).
				Build()).
			Build())
	if err != nil {
		slog.Debug(p.tag()+": add reaction failed", "error", err)
		return ""
	}
	if !resp.Success() {
		slog.Debug(p.tag()+": add reaction failed", "code", resp.Code, "msg", resp.Msg)
		return ""
	}
	if resp.Data != nil && resp.Data.ReactionId != nil {
		return *resp.Data.ReactionId
	}
	return ""
}

func (p *Platform) removeReaction(messageID, reactionID string) {
	if reactionID == "" || messageID == "" {
		return
	}
	resp, err := p.client.Im.MessageReaction.Delete(context.Background(),
		larkim.NewDeleteMessageReactionReqBuilder().
			MessageId(messageID).
			ReactionId(reactionID).
			Build())
	if err != nil {
		slog.Debug(p.tag()+": remove reaction failed", "error", err)
		return
	}
	if !resp.Success() {
		slog.Debug(p.tag()+": remove reaction failed", "code", resp.Code, "msg", resp.Msg)
	}
}

// ==================== 内部方法(Card相关) ====================

//	handles card.action.trigger 通过官方的SDK evnent 分发器回调
//
// 支持三种前缀:
//   - nav:/xxx   — 渲染card page and 原地更新 original card
//   - act:/xxx   — 执行 action, 然后原地渲染更新 the card
//   - cmd:/xxx   — 旧: 作为用户cmd分发 (发送新消息)
func (p *Platform) onCardAction(event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	if event.Event == nil || event.Event.Action == nil {
		return nil, nil
	}
	// 获取action字段
	actionVal, _ := event.Event.Action.Value["action"].(string)
	// select_static 回调函数会将所选值放入 event.Event.Action 中
	if actionVal == "" && event.Event.Action.Option != "" {
		actionVal = event.Event.Action.Option
	}
	if actionVal == "" {
		switch event.Event.Action.Name {
		case "delete_mode_submit":
			actionVal = "act:/delete-mode form-submit"
		case "delete_mode_cancel":
			actionVal = "act:/delete-mode cancel"
		}
	}
	// 找到要删除的ids
	if actionVal == "act:/delete-mode form-submit" {
		ids := collectDeleteModeSelectedFromFormValue(event.Event.Action.FormValue)
		if len(ids) > 0 {
			actionVal += " " + strings.Join(ids, ",")
		}
	}

	userID := ""
	if event.Event.Operator != nil {
		userID = event.Event.Operator.OpenID
	}
	chatID := ""
	messageID := ""
	if event.Event.Context != nil {
		chatID = event.Event.Context.OpenChatID
		messageID = event.Event.Context.OpenMessageID
	}
	if chatID == "" {
		chatID = userID
	}
	// 从CardAction中获取sessionKey
	sessionKey := p.sessionKeyFromCardAction(chatID, userID, event.Event.Action.Value)

	// nav: / act: — synchronous card update
	if strings.HasPrefix(actionVal, "nav:") || strings.HasPrefix(actionVal, "act:") {
		// Feishu uses native form checker for delete-mode toggle,
		// so return a toast without calling cardNavHandler to avoid a full card refresh.
		if strings.HasPrefix(actionVal, "act:/delete-mode toggle ") {
			return &callback.CardActionTriggerResponse{
				Toast: &callback.Toast{
					Type:    "info",
					Content: "已记录选择（Selection recorded）",
				},
			}, nil
		}
		if strings.HasPrefix(actionVal, "act:/model ") {
			cmdText := strings.TrimPrefix(actionVal, "act:")
			rctx := replyContext{messageID: messageID, chatID: chatID, sessionKey: sessionKey}
			go p.handler(p.dispatchPlatform(), &core.Message{
				SessionKey: sessionKey,
				Platform:   p.platformName,
				UserID:     userID,
				UserName:   p.resolveUserName(userID),
				ChatName:   p.resolveChatName(chatID),
				Content:    cmdText,
				ReplyCtx:   rctx,
			})
			return &callback.CardActionTriggerResponse{
				Toast: &callback.Toast{
					Type:    "info",
					Content: "正在切换模型（Switching model...）",
				},
			}, nil
		}
		if p.cardNavHandler != nil {
			card := p.cardNavHandler(actionVal, sessionKey)
			if card != nil {
				return &callback.CardActionTriggerResponse{
					Card: &callback.Card{
						Type: "raw",
						Data: renderCardMap(card, sessionKey),
					},
				}, nil
			}
		}
		if strings.HasPrefix(actionVal, "act:") {
			slog.Debug(p.tag()+": card action produced no card update", "action", actionVal)
			return nil, nil
		}
		slog.Warn(p.tag()+": card nav returned nil, ignoring", "action", actionVal)
		return nil, nil
	}

	// perm: — 权限回复原地更新
	if strings.HasPrefix(actionVal, "perm:") {
		var responseText string
		switch actionVal {
		case "perm:allow":
			responseText = "allow"
		case "perm:deny":
			responseText = "deny"
		case "perm:allow_all":
			responseText = "allow all"
		default:
			return nil, nil
		}

		rctx := replyContext{messageID: messageID, chatID: chatID, sessionKey: sessionKey}
		go p.handler(p.dispatchPlatform(), &core.Message{
			SessionKey: sessionKey,
			Platform:   p.platformName,
			UserID:     userID,
			UserName:   p.resolveUserName(userID),
			ChatName:   p.resolveChatName(chatID),
			Content:    responseText,
			ReplyCtx:   rctx,
		})

		permLabel, _ := event.Event.Action.Value["perm_label"].(string)
		permColor, _ := event.Event.Action.Value["perm_color"].(string)
		permBody, _ := event.Event.Action.Value["perm_body"].(string)
		if permColor == "" {
			permColor = "green"
		}
		cb := core.NewCard().Title(permLabel, permColor)
		if permBody != "" {
			cb.Markdown(permBody)
		}
		return &callback.CardActionTriggerResponse{
			Card: &callback.Card{
				Type: "raw",
				Data: renderCardMap(cb.Build(), sessionKey),
			},
		}, nil
	}

	// askq: — AskUserQuestion option selected, forward as user message
	if strings.HasPrefix(actionVal, "askq:") {
		rctx := replyContext{messageID: messageID, chatID: chatID, sessionKey: sessionKey}
		go p.handler(p.dispatchPlatform(), &core.Message{
			SessionKey: sessionKey,
			Platform:   p.platformName,
			UserID:     userID,
			UserName:   p.resolveUserName(userID),
			ChatName:   p.resolveChatName(chatID),
			Content:    actionVal,
			ReplyCtx:   rctx,
		})

		answerLabel, _ := event.Event.Action.Value["askq_label"].(string)
		askqQuestion, _ := event.Event.Action.Value["askq_question"].(string)
		if answerLabel == "" {
			answerLabel = actionVal
		}
		cb := core.NewCard().Title("✅ "+answerLabel, "green")
		if askqQuestion != "" {
			cb.Markdown(askqQuestion)
		}
		cb.Markdown("**→ " + answerLabel + "**")
		return &callback.CardActionTriggerResponse{
			Card: &callback.Card{
				Type: "raw",
				Data: renderCardMap(cb.Build(), sessionKey),
			},
		}, nil
	}

	// cmd: — async command dispatch
	if strings.HasPrefix(actionVal, "cmd:") {
		cmdText := strings.TrimPrefix(actionVal, "cmd:")
		rctx := replyContext{messageID: messageID, chatID: chatID, sessionKey: sessionKey}

		slog.Info(p.tag()+": card action dispatched as command", "cmd", cmdText, "user", userID)

		go p.handler(p.dispatchPlatform(), &core.Message{
			SessionKey: sessionKey,
			Platform:   p.platformName,
			UserID:     userID,
			UserName:   p.resolveUserName(userID),
			ChatName:   p.resolveChatName(chatID),
			Content:    cmdText,
			ReplyCtx:   rctx,
		})
	}

	return nil, nil
}

// 从value中获取session_key字段
func (p *Platform) sessionKeyFromCardAction(chatID, userID string, value map[string]any) string {
	if value != nil {
		if sessionKey, _ := value["session_key"].(string); sessionKey != "" {
			return sessionKey
		}
	}
	if p.shareSessionInChannel {
		return fmt.Sprintf("%s:%s", p.tag(), chatID)
	}
	return fmt.Sprintf("%s:%s:%s", p.tag(), chatID, userID)
}

// ==================== 内部方法(BotMenu相关) ====================

// 处理bot自定义menu点击事件,当 带有"/" menu item的event_key 开始, 作为一个slash 命令分发
// 允许用户再Fei数开发者控制台配置meun item, 将event_key 设置为 /help /status 等
func (p *Platform) onBotMenu(event *larkapplication.P2BotMenuV6) error {
	if event == nil || event.Event == nil || event.Event.EventKey == nil {
		return nil
	}
	eventKey := *event.Event.EventKey

	userID := ""
	if event.Event.Operator != nil && event.Event.Operator.OperatorId != nil && event.Event.Operator.OperatorId.OpenId != nil {
		userID = *event.Event.Operator.OperatorId.OpenId
	}
	if userID == "" {
		slog.Debug(p.tag()+": bot menu event without user id", "event_key", eventKey)
		return nil
	}

	slog.Info(p.tag()+": bot menu clicked", "event_key", eventKey, "user", userID)

	content := eventKey
	if !strings.HasPrefix(content, "/") {
		content = "/" + content
	}

	userName := p.resolveUserName(userID)
	sessionKey := p.platformName + ":" + userID + ":" + userID

	p.handler(p.dispatchPlatform(), &core.Message{
		SessionKey: sessionKey,
		Platform:   p.platformName,
		Content:    content,
		UserID:     userID,
		UserName:   userName,
		ReplyCtx:   replyContext{chatID: userID, sessionKey: sessionKey},
	})
	return nil
}

// ==================== 辅助函数/工具函数 ====================

// 检查URL是否被Feishu post 连接接受， feishu拒绝非http url "invalid href" (code 230001)
func isValidFeishuHref(u string) bool {
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

var mdLinkRe = regexp.MustCompile(`\[([^\]]*)\]\(([^)]+)\)`)

// 使用非HTTP(s) schemes重写makrdown连接为纯文本
// 防止飞书API拒绝
func sanitizeMarkdownURLs(md string) string {
	return mdLinkRe.ReplaceAllStringFunc(md, func(match string) string {
		parts := mdLinkRe.FindStringSubmatch(match)
		if len(parts) < 3 {
			return match
		}
		if isValidFeishuHref(parts[2]) {
			return match
		}
		// Convert invalid-scheme link to "text (url)" plain text
		return parts[1] + " (" + parts[2] + ")"
	})
}

// 创建lark client用于reply
func newFeishuReplayClient(appID, appSecret, domain string) *lark.Client {
	var opts []lark.ClientOptionFunc
	opts = append(opts, lark.WithEnableTokenCache(false))
	return lark.NewClient(appID, appSecret, opts...)
}

// 使用markdown 元素构建可交互的卡片JSON，填充进["body"]["elements"]["content"]中
// schema 2.0 支持code blocks, tables, inline 格式
// Card字体小于Post/Text - 飞书平台限制
func buildCardJSON(content string) string {
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"wide_screen_mode": true,
		},
		"body": map[string]any{
			"elements": []map[string]any{
				{
					"tag":     "markdown",
					"content": content,
				},
			},
		},
	}
	b, _ := json.Marshal(card)
	return string(b)
}

// 确保 每行前有换行符， 防止卡片渲染错误
// Table, headings, blockquotes 等被卡片markdown元素原生渲染
func preprocessFeishuMarkdown(md string) string {
	//  确保``` 之前有一个新行（除非是整个文本开头）
	var b strings.Builder
	// 预分配空间
	b.Grow(len(md) + 32)
	for i := 0; i < len(md); i++ {
		if i > 0 && md[i] == '`' && i+2 < len(md) && md[i+1] == '`' && md[i+2] == '`' && md[i-1] != '\n' {
			b.WriteByte('\n')
		}
		b.WriteByte(md[i])
	}
	return b.String()
}

var markdownIndicators = []string{
	"```", "**", "~~", "`", "\n- ", "\n* ", "\n1. ", "\n# ", "---",
}

// 根据常用的markdown 符号内容是否包含markdown格式
func containsMarkdown(s string) bool {
	for _, ind := range markdownIndicators {
		if strings.Contains(s, ind) {
			return true
		}
	}
	return false
}

// 将markdown转换为飞书post 格式(rish text)
func buildPostJSON(content string) string {
	// 按行分割
	lines := strings.Split(content, "\n")
	var postLines [][]map[string]any
	inCodeBlock := false
	var codeLines []string
	codeLang := ""

	for _, line := range lines {
		// 清理空白
		trimmed := strings.TrimSpace(line)
		// 找到```段
		if strings.HasPrefix(trimmed, "```") {
			// 判断是否在block内部
			if !inCodeBlock {
				inCodeBlock = true
				// 清理前缀``` 获取code语言类型
				codeLang = strings.TrimPrefix(trimmed, "```")
				codeLines = nil
			} else {
				inCodeBlock = false
				// 将两个```的内容写入 postLines
				postLines = append(postLines, []map[string]any{{
					"tag":      "code_block",
					"language": codeLang,
					"text":     strings.Join(codeLines, "\n"),
				}})
			}
			continue
		}

		if inCodeBlock {
			// ```之间的内容之间添加
			codeLines = append(codeLines, line)
			continue
		}

		// 标题 # headers 粗体
		headerLine := line
		for level := 6; level >= 1; level-- {
			prefix := strings.Repeat("#", level) + " "
			if strings.HasPrefix(line, prefix) {
				headerLine = "**" + strings.TrimPrefix(line, prefix) + "**"
				break
			}
		}

		// 解析行内的markdown
		elements := parseInlineMarkdown(headerLine)
		if len(elements) > 0 {
			postLines = append(postLines, elements)
		} else {
			postLines = append(postLines, []map[string]any{{"tag": "text", "text": ""}})
		}
	}

	// 处理未封闭的code block
	if inCodeBlock && len(codeLines) > 0 {
		postLines = append(postLines, []map[string]any{{
			"tag":      "code_block",
			"language": codeLang,
			"text":     strings.Join(codeLines, "\n"),
		}})
	}

	post := map[string]any{
		"zh_cn": map[string]any{
			"content": postLines,
		},
	}
	b, _ := json.Marshal(post)
	return string(b)

}

// 将单行markdown解析成Feishu post元素
// 支持**blod** 和 `code` 行内格式
func parseInlineMarkdown(line string) []map[string]any {
	type markerDef struct {
		pattern string
		tag     string
		style   string // for text elements with style
	}
	markers := []markerDef{
		{pattern: "**", tag: "text", style: "bold"},
		{pattern: "~~", tag: "text", style: "lineThrough"},
		{pattern: "`", tag: "text", style: "code"},
		{pattern: "*", tag: "text", style: "italic"},
	}

	var elements []map[string]any
	remaining := line

	for len(remaining) > 0 {
		// // 优先解析 [text](url) 链接格式，验证 URL 有效性后转为飞书 "a" 标签元素
		linkIdx := strings.Index(remaining, "[")
		if linkIdx >= 0 {
			parenClose := -1
			bracketClose := strings.Index(remaining[linkIdx:], "](")
			if bracketClose >= 0 {
				bracketClose += linkIdx
				parenClose = strings.Index(remaining[bracketClose+2:], ")")
				if parenClose >= 0 {
					parenClose += bracketClose + 2
				}
			}
			if parenClose >= 0 {
				// Check if any marker comes before this link
				foundEarlierMarker := false
				for _, m := range markers {
					idx := strings.Index(remaining, m.pattern)
					if idx >= 0 && idx < linkIdx {
						foundEarlierMarker = true
						break
					}
				}
				if !foundEarlierMarker {
					linkText := remaining[linkIdx+1 : bracketClose]
					linkURL := remaining[bracketClose+2 : parenClose]
					if isValidFeishuHref(linkURL) {
						if linkIdx > 0 {
							elements = append(elements, map[string]any{"tag": "text", "text": remaining[:linkIdx]})
						}
						elements = append(elements, map[string]any{
							"tag":  "a",
							"text": linkText,
							"href": linkURL,
						})
						remaining = remaining[parenClose+1:]
						continue
					}
				}
			}

		}

		// 扫描剩余字符串，找到位置最靠前的格式标记（**、~~、`、*），
		// 单字符 * 需排除成对 ** 的干扰（通过 findSingleAsterisk 辅助判断）
		bestIdx := -1
		var bestMarker markerDef
		for _, m := range markers {
			idx := strings.Index(remaining, m.pattern)
			if idx < 0 {
				continue
			}
			// For single * marker, skip if it's actually ** (bold)
			if m.pattern == "*" && idx+1 < len(remaining) && remaining[idx+1] == '*' {
				idx = findSingleAsterisk(remaining)
				if idx < 0 {
					continue
				}
			}
			if bestIdx < 0 || idx < bestIdx {
				bestIdx = idx
				bestMarker = m
			}
		}

		// 若无标记，将剩余全部文本作为普通 text 元素添加；
		// 若有标记，先将标记前文本添加为普通元素
		if bestIdx < 0 {
			if remaining != "" {
				elements = append(elements, map[string]any{"tag": "text", "text": remaining})
			}
			break
		}

		// 定位闭合标记，提取中间内容包裹为带 style 的 text 元素，
		// 剩余字符串继续进入下一轮循环解析
		if bestIdx > 0 {
			elements = append(elements, map[string]any{"tag": "text", "text": remaining[:bestIdx]})
		}
		remaining = remaining[bestIdx+len(bestMarker.pattern):]

		closeIdx := strings.Index(remaining, bestMarker.pattern)
		// For single *, make sure we don't match ** as close
		if bestMarker.pattern == "*" {
			closeIdx = findSingleAsterisk(remaining)
		}
		if closeIdx < 0 {
			elements = append(elements, map[string]any{"tag": "text", "text": bestMarker.pattern + remaining})
			remaining = ""
			break
		}

		inner := remaining[:closeIdx]
		remaining = remaining[closeIdx+len(bestMarker.pattern):]

		elements = append(elements, map[string]any{
			"tag":   bestMarker.tag,
			"text":  inner,
			"style": []string{bestMarker.style},
		})
	}

	return elements
}

// 找到s中单个 '*' 的位置
func findSingleAsterisk(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '*' {
			if i+1 < len(s) && s[i+1] == '*' {
				i++ // skip **
				continue
			}
			return i
		}
	}
	return -1
}

// 如果错误是一个瞬时网络错误，（如连接重置、超时、EOF等）需要重试，则返回true
func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	// Typed系统化调用 — 比字符串匹配更稳健.
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	// net.Error 包含超时和stdlib中的错误
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// EOF 通常表示服务回复中关闭连接
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// 错误关键字检查
	// 对可能出现在封装后的飞书SDK错误中的常见瞬态症状进行未封装字符串检查。
	msg := err.Error()
	for _, substr := range []string{
		"connection reset by peer",
		"broken pipe",
		"i/o timeout",
		"TLS handshake timeout",
		"server misbehaving",
		"connection refused",
	} {
		if strings.Contains(msg, substr) {
			return true
		}
	}
	return false
}

// 根据错误码和内容检查是否为token问题
func isTenantAccessTokenInvalid(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "99991663") || strings.Contains(msg, "invalid access token")
}

func stringValue(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// 获取非空的 root: thread: 后的字段
func parseThreadRootID(sessionTail string) (string, bool) {
	for _, prefix := range []string{"root:", "thread:"} {
		if strings.HasPrefix(sessionTail, prefix) {
			rootID := strings.TrimPrefix(sessionTail, prefix)
			if rootID != "" {
				return rootID, true
			}
			return "", false
		}
	}
	return "", false
}

// 按 : 分割后检查 是否有 root: thread: 开头的字段
func isThreadSessionKey(sessionKey string) bool {
	parts := strings.SplitN(sessionKey, ":", 3)
	if len(parts) != 3 {
		return false
	}
	_, ok := parseThreadRootID(parts[2])
	return ok
}

// 使用从Mentions list中得到的真实名称替换@_user_N 字段
func replaceMentions(text string, mentions []*larkim.Mention) string {
	for _, m := range mentions {
		if m.Key != nil && m.Name != nil {
			text = strings.ReplaceAll(text, *m.Key, "@"+*m.Name)
		}
	}
	return text
}

// ==================== 辅助函数/工具函数（消息处理） ====================

// 从Lark post（富文本） JSON内容中提取plain text
func extractPostPlainText(content string) string {
	var post struct {
		Content [][]struct {
			Tag      string `json:"tag"`
			Text     string `json:"text"`
			Language string `json:"language,omitempty"`
		} `json:"content"`
		Title string `json:"title"`
	}
	// 可能被本地key包装 {"zh_cn": {...}}.
	// 先直接解析，再尝试localwarper解析
	if err := json.Unmarshal([]byte(content), &post); err != nil || len(post.Content) == 0 {
		var localeWrapper map[string]json.RawMessage
		if err2 := json.Unmarshal([]byte(content), &localeWrapper); err2 == nil {
			for _, v := range localeWrapper {
				if err3 := json.Unmarshal(v, &post); err3 == nil && len(post.Content) > 0 {
					break
				}
			}
		}
	}
	if len(post.Content) == 0 {
		return ""
	}
	var parts []string
	if post.Title != "" {
		parts = append(parts, post.Title)
	}
	for _, para := range post.Content {
		var line []string
		for _, elem := range para {
			switch elem.Tag {
			case "text":
				if elem.Text != "" {
					line = append(line, elem.Text)
				}
			case "code_block":
				if elem.Text != "" {
					lang := elem.Language
					line = append(line, "```"+lang+"\n"+elem.Text+"\n```")
				}
			}
		}
		if len(line) > 0 {
			parts = append(parts, strings.Join(line, ""))
		}
	}
	return strings.Join(parts, "\n")
}

// 从飞书交互card JSON 中提取可读的文本
// 使用 raw_card_content, 回复使用{"json_card": "...", ...} 包装
// 支持 schema 2.0 (body.property.elements with recursive nesting)
// 旧版本格式 （top-level title + elements）
func extractInteractiveCardText(content string) string {
	// 尝试 raw_card_content 格式: {"json_card": "<escaped JSON>", ...}
	var wrapper struct {
		JsonCard string `json:"json_card"`
	}
	cardJSON := content
	if json.Unmarshal([]byte(content), &wrapper) == nil && wrapper.JsonCard != "" {
		cardJSON = wrapper.JsonCard
	}

	var card map[string]json.RawMessage
	if err := json.Unmarshal([]byte(cardJSON), &card); err != nil {
		return "[interactive card]"
	}

	var parts []string

	// Schema 2.0: body 可能使用 property.elements (标准) or direct elements (简化).
	if raw, ok := card["body"]; ok {
		var body struct {
			Tag      string            `json:"tag"`
			Elements []json.RawMessage `json:"elements"`
			Property struct {
				Elements []json.RawMessage `json:"elements"`
			} `json:"property"`
		}
		if json.Unmarshal(raw, &body) == nil {
			if body.Tag == "body" && len(body.Property.Elements) > 0 {
				extractCardElements(body.Property.Elements, &parts)
			} else if len(body.Elements) > 0 {
				extractCardElements(body.Elements, &parts)
			}
		}
	}

	// 旧版本: direct title string + flat/nested elements.
	if len(parts) == 0 {
		if raw, ok := card["header"]; ok {
			var header struct {
				Title struct {
					Content string `json:"content"`
				} `json:"title"`
			}
			if json.Unmarshal(raw, &header) == nil && header.Title.Content != "" {
				parts = append(parts, header.Title.Content)
			}
		}
		if len(parts) == 0 {
			if raw, ok := card["title"]; ok {
				var title string
				if json.Unmarshal(raw, &title) == nil && title != "" {
					parts = append(parts, title)
				}
			}
		}
		var elements []json.RawMessage
		if raw, ok := card["elements"]; ok {
			var nested [][]json.RawMessage
			if json.Unmarshal(raw, &nested) == nil && len(nested) > 0 {
				for _, row := range nested {
					elements = append(elements, row...)
				}
			} else {
				_ = json.Unmarshal(raw, &elements)
			}
		}
		for _, raw := range elements {
			var elem struct {
				Tag  string `json:"tag"`
				Text string `json:"text"`
			}
			if json.Unmarshal(raw, &elem) == nil && elem.Tag == "text" && strings.TrimSpace(elem.Text) != "" {
				parts = append(parts, elem.Text)
			}
		}
	}

	if len(parts) == 0 {
		return "[interactive card]"
	}
	return strings.Join(parts, "\n")
}

// 递归地从schema2.0 卡片消息中提取文本
// 处理: property.content, property.text (嵌套元素), property.elements (递归),
// code_span, code_block (with tokenized 内容), text_tag, hr, etc.
func extractCardElements(elements []json.RawMessage, parts *[]string) {
	for _, raw := range elements {
		var elem struct {
			Tag      string `json:"tag"`
			Content  string `json:"content"`
			Property struct {
				Content  string            `json:"content"`
				Contents json.RawMessage   `json:"contents"`
				Language string            `json:"language"`
				Elements []json.RawMessage `json:"elements"`
				Text     json.RawMessage   `json:"text"`
				Items    json.RawMessage   `json:"items"`
				Columns  json.RawMessage   `json:"columns"`
				Rows     json.RawMessage   `json:"rows"`
			} `json:"property"`
		}
		if json.Unmarshal(raw, &elem) != nil {
			continue
		}
		switch elem.Tag {
		case "code_block":
			var lines []struct {
				Contents []struct {
					Content string `json:"content"`
				} `json:"contents"`
			}
			if json.Unmarshal(elem.Property.Contents, &lines) == nil {
				var codeLines []string
				for _, line := range lines {
					var lineText string
					for _, tok := range line.Contents {
						lineText += tok.Content
					}
					codeLines = append(codeLines, lineText)
				}
				code := strings.Join(codeLines, "")
				if strings.TrimSpace(code) != "" {
					lang := elem.Property.Language
					if lang != "" {
						*parts = append(*parts, fmt.Sprintf("```%s\n%s```", lang, code))
					} else {
						*parts = append(*parts, fmt.Sprintf("```\n%s```", code))
					}
				}
			}
		case "code_span":
			if elem.Property.Content != "" {
				*parts = append(*parts, "`"+elem.Property.Content+"`")
			}
		case "hr":
			*parts = append(*parts, "---")
		case "table":
			extractCardTable(elem.Property.Columns, elem.Property.Rows, parts)
		case "list":
			extractCardListItems(elem.Property.Items, parts)
		default:
			content := elem.Property.Content
			if content == "" {
				content = elem.Content
			}
			if content != "" {
				*parts = append(*parts, content)
			}
			if len(elem.Property.Text) > 0 {
				var textElem struct {
					Property struct {
						Content string `json:"content"`
					} `json:"property"`
				}
				if json.Unmarshal(elem.Property.Text, &textElem) == nil && textElem.Property.Content != "" {
					*parts = append(*parts, textElem.Property.Content)
				}
			}
		}
		if len(elem.Property.Elements) > 0 {
			extractCardElements(elem.Property.Elements, parts)
		}
	}
}

// 从 Feishu card table 元素提取文本.
// 表结构: property.columns - 列名,
// property.rows row objects 的列表，其值包含data 字段（markdonw/plain_text）元素
func extractCardTable(columnsRaw, rowsRaw json.RawMessage, parts *[]string) {
	var columns []struct {
		DisplayName string `json:"displayName"`
		Name        string `json:"name"`
	}
	if err := json.Unmarshal(columnsRaw, &columns); err != nil || len(columns) == 0 {
		return
	}
	var rows []map[string]struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rowsRaw, &rows); err != nil {
		return
	}

	// Build markdown table.
	header := make([]string, len(columns))
	for i, col := range columns {
		header[i] = col.DisplayName
	}
	*parts = append(*parts, "| "+strings.Join(header, " | ")+" |")
	sep := make([]string, len(columns))
	for i := range sep {
		sep[i] = "---"
	}
	*parts = append(*parts, "| "+strings.Join(sep, " | ")+" |")

	for _, row := range rows {
		cells := make([]string, len(columns))
		for i, col := range columns {
			cell := row[col.Name]
			var cellParts []string
			extractCardElements([]json.RawMessage{cell.Data}, &cellParts)
			cells[i] = strings.Join(cellParts, " ")
		}
		*parts = append(*parts, "| "+strings.Join(cells, " | ")+" |")
	}
}

// 从Feishu card list 元素提取文本.
// List结构: property.items - items 列表, 每一个包含 "elements"数组.
func extractCardListItems(itemsRaw json.RawMessage, parts *[]string) {
	var items []struct {
		Elements []json.RawMessage `json:"elements"`
	}
	if err := json.Unmarshal(itemsRaw, &items); err != nil {
		return
	}
	for _, item := range items {
		var itemParts []string
		extractCardElements(item.Elements, &itemParts)
		if len(itemParts) > 0 {
			*parts = append(*parts, "- "+strings.Join(itemParts, " "))
		}
	}
}

// 处理 text中的 @mention placeholders (如 @_user_1)， bot自己的@ 被移除，
// 其他被@用户 被他们的display name替换，这样agent可以看谁引用了
func stripMentions(text string, mentions []*larkim.MentionEvent, botOpenID string) string {
	if len(mentions) == 0 {
		return text
	}
	for _, m := range mentions {
		if m.Key == nil {
			continue
		}
		if botOpenID != "" && m.Id != nil && m.Id.OpenId != nil && *m.Id.OpenId == botOpenID {
			text = strings.ReplaceAll(text, *m.Key, "")
		} else if m.Name != nil && *m.Name != "" {
			text = strings.ReplaceAll(text, *m.Key, "@"+*m.Name)
		} else {
			text = strings.ReplaceAll(text, *m.Key, "")
		}
	}
	return strings.TrimSpace(text)
}
