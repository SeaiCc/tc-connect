package core

import (
	"context"
	"sync"
	"time"
	"log/slog"
	"strings"
)

// =============================== Interface ===================================

// 用于平台需要在最终response发送之后清理预览消息 (Discord 删除preview 并发送一个新消息)
type PreviewCleaner interface {
	DeletePreviewMessage(ctx context.Context, previewHandle any) error
}

// 平台接口可以启动一个流式预览消息，并返回一个句柄以供后续更新。
// 控制streaming 预览行为
type PreviewStarter interface {
	// 发送内部preview信息管理并返回一个可以被发送到UpdateMessage用于编辑的handle
	// 如果preview不支持此context返回nil
	SendPreviewStart(ctx context.Context, replyCtx any, content string) (previewHandle any, err error)
}

// platform 用来保持preview message在普通completion上最终分发消息
type PreviewFinishPreference interface {
	KeepPreviewOnFinish() bool
}

type StreamPreviewCfg struct {
	Enabled           bool     // 全局开关
	DisabledPlatforms []string // 禁用streaming的平台 如 feishu
	IntervalMs        int      // 两次更新之间的最小毫秒 (默认1500)
	MinDeltaChars     int      // 发送update之前的最小新字符 (默认30)
	MaxChars          int      // 最大预览长度(默认2000)
}


// 管理单个流式预览的状态和流量限制。
// 它从EventText事件中累积文本，并定期通过MessageUpdater.UpdateMessage向平台推送更新。
type streamPreview struct {
	mu sync.Mutex

	cfg       StreamPreviewCfg
	platform  Platform
	replyCtx  any
	ctx       context.Context
	transform func(string) string

	fullText          string // 累加至今为止的全文
	lastSentText      string // 上一次发送到平台的文本
	lastSentAt        time.Time
	lastSentViaUpdate bool // true 如果lastSentText通过UpdateMessage 分发 (not SendPreviewStart)
	previewMsgID      any  // platform-specific ID 用于 preview message (returned by SendPreviewStart)
	degraded          bool // 若为true, 停止尝试 (平台不支持或永久error)

	timer     *time.Timer
	timerStop chan struct{} // closed when preview ends
}

func newStreamPreview(cfg StreamPreviewCfg, p Platform, replyCtx any, ctx context.Context, transform func(string) string) *streamPreview {
	return &streamPreview{
		cfg:       cfg,
		platform:  p,
		replyCtx:  replyCtx,
		ctx:       ctx,
		transform: transform,
		timerStop: make(chan struct{}),
	}
}

func (sp *streamPreview) cancelTimerLocked() {
	if sp.timer != nil {
		sp.timer.Stop()
		sp.timer = nil
	}
}

// 当允许时移除预览消息, 并关闭further预览更新, 当调用者打算发送一个分离的non-preview消息
// (如当tool使用或者terminal error)
func (sp *streamPreview) discard() {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	// 关闭计时器
	sp.cancelTimerLocked()

	select {
	case <-sp.timerStop:
	default:
		close(sp.timerStop)
	}

	if sp.previewMsgID != nil {
		if cleaner, ok := sp.platform.(PreviewCleaner); ok {
			slog.Debug("stream preview discard: deleting preview")
			_ = cleaner.DeletePreviewMessage(sp.ctx, sp.previewMsgID)
		}
	}

	sp.previewMsgID = nil
	sp.degraded = true
}

// 停止临时streaming预览: 取消排队计时器, 使用累积文本原地更新消息预览, 标记preview为降级,这样没有
// 后来的更新被发送, 当premission prompt或者其他打断发生时调用
func (sp *streamPreview) freeze() {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	sp.cancelTimerLocked()

	if sp.previewMsgID != nil && !sp.degraded {
		if updater, ok := sp.platform.(MessageUpdater); ok {
			text := sp.fullText
			maxChars := sp.cfg.MaxChars
			if maxChars > 0 && len([]rune(text)) > maxChars {
				text = string([]rune(text)[:maxChars]) + "…"
			}
			if text != "" {
				if sp.transform != nil {
					text = sp.transform(text)
				}
				_ = updater.UpdateMessage(sp.ctx, sp.previewMsgID, text)
			}
		}
	}

	sp.degraded = true
}

// 当agent返回完成时调用,取消任意排队timer并可选清理preview message
// 如果preview active 并且最终消息通过preview发送返回true (这样调用者应该跳过分离发送完整消息)
func (sp *streamPreview) finish(finalText string) bool {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	sp.cancelTimerLocked()

	select {
	case <-sp.timerStop:
	default:
		close(sp.timerStop)
	}

	if sp.transform != nil {
		finalText = sp.transform(finalText)
	}
	if sp.previewMsgID == nil || sp.degraded {
		if sp.previewMsgID != nil && sp.degraded {
			if cleaner, ok := sp.platform.(PreviewCleaner); ok {
				slog.Debug("stream preview finish: deleting stale preview (degraded)")
				_ = cleaner.DeletePreviewMessage(sp.ctx, sp.previewMsgID)
			}
		}
		slog.Debug("stream preview finish: no active preview", "hasHandle", sp.previewMsgID != nil, "degraded", sp.degraded)
		return false
	}

	keepPreview := false
	if pref, ok := sp.platform.(PreviewFinishPreference); ok {
		keepPreview = pref.KeepPreviewOnFinish()
	}

	// If platform wants to delete the preview and send fresh, let it.
	if cleaner, ok := sp.platform.(PreviewCleaner); ok && !keepPreview {
		slog.Debug("stream preview finish: deleting preview (PreviewCleaner)")
		_ = cleaner.DeletePreviewMessage(sp.ctx, sp.previewMsgID)
		return false
	}

	updater, ok := sp.platform.(MessageUpdater)
	if !ok {
		slog.Debug("stream preview finish: no MessageUpdater")
		return false
	}

	if finalText == "" {
		slog.Debug("stream preview finish: empty final text")
		return false
	}

	// If the final text is identical to what was last sent via UpdateMessage,
	// skip the redundant API call. This prevents duplicate messages on platforms
	// (e.g. Feishu) where patching with identical content may fail.
	// Only skip when lastSentViaUpdate is true — if the text was only sent via
	// SendPreviewStart (first flush), we must still call UpdateMessage because
	// it may apply different formatting (e.g. Markdown→HTML for Telegram).
	if finalText == sp.lastSentText && sp.lastSentViaUpdate {
		slog.Debug("stream preview finish: text unchanged since last UpdateMessage, skipping",
			"text_len", len(finalText))
		return true
	}

	// Try to update the preview in-place with the full final text.
	// maxChars only throttles intermediate streaming updates; at finish time
	// we always attempt a single final update regardless of length.
	slog.Debug("stream preview finish: sending final UpdateMessage",
		"text_len", len(finalText), "lastSent_len", len(sp.lastSentText),
		"same", finalText == sp.lastSentText, "viaUpdate", sp.lastSentViaUpdate)
	if err := updater.UpdateMessage(sp.ctx, sp.previewMsgID, finalText); err != nil {
		slog.Debug("stream preview finish: final update FAILED, cleaning up preview", "error", err)
		// Update failed (e.g. text too long for platform edit API).
		// Try to delete the stale preview so caller can send a fresh message.
		if cleaner, ok := sp.platform.(PreviewCleaner); ok {
			_ = cleaner.DeletePreviewMessage(sp.ctx, sp.previewMsgID)
		}
		return false
	}
	slog.Debug("stream preview finish: success via UpdateMessage")
	return true
}

// 如果平台支持消息更新并没有被禁用
func (sp *streamPreview) canPreview() bool {
	sp.mu.Lock()
	degraded := sp.degraded
	sp.mu.Unlock()
	if degraded || !sp.cfg.Enabled {
		return false
	}
	// 检查平台是否在不可用名单中
	platformName := sp.platform.Name()
	for _, disabled := range sp.cfg.DisabledPlatforms {
		if strings.EqualFold(disabled, platformName) {
			return false
		}
	}
	_, ok := sp.platform.(MessageUpdater)
	return ok
}

func (sp *streamPreview) appendText(text string) {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	if sp.degraded || !sp.cfg.Enabled {
		return
	}

	sp.fullText += text

	displayText := sp.fullText
	maxChars := sp.cfg.MaxChars
	if maxChars > 0 && len([]rune(displayText)) > maxChars {
		displayText = string([]rune(displayText)[:maxChars]) + "…"
	}

	delta := len([]rune(displayText)) - len([]rune(sp.lastSentText))
	elapsed := time.Since(sp.lastSentAt)
	interval := time.Duration(sp.cfg.IntervalMs) * time.Millisecond

	if delta < sp.cfg.MinDeltaChars && !sp.lastSentAt.IsZero() {
		sp.scheduleFlushLocked(interval)
		return
	}

	if elapsed < interval && !sp.lastSentAt.IsZero() {
		remaining := interval - elapsed
		sp.scheduleFlushLocked(remaining)
		return
	}

	sp.cancelTimerLocked()
	sp.flushLocked(displayText)
}

func (sp *streamPreview) scheduleFlushLocked(delay time.Duration) {
	if sp.timer != nil {
		return // already scheduled
	}
	sp.timer = time.AfterFunc(delay, func() {
		sp.mu.Lock()
		defer sp.mu.Unlock()
		sp.timer = nil
		if sp.degraded {
			return
		}
		displayText := sp.fullText
		maxChars := sp.cfg.MaxChars
		if maxChars > 0 && len([]rune(displayText)) > maxChars {
			displayText = string([]rune(displayText)[:maxChars]) + "…"
		}
		sp.flushLocked(displayText)
	})
}


// 发送当前preview的文本到平台, 必须持有 sp.mu
func (sp *streamPreview) flushLocked(text string) {
	if sp.transform != nil {
		text = sp.transform(text)
	}
	if text == sp.lastSentText || text == "" {
		return
	}

	updater, ok := sp.platform.(MessageUpdater)
	if !ok {
		slog.Debug("stream preview: platform does not support UpdateMessage, degrading")
		sp.degraded = true
		return
	}

	if sp.previewMsgID == nil {
		// First preview: try to send a new preview message
		if starter, ok := sp.platform.(PreviewStarter); ok {
			slog.Debug("stream preview: sending first preview via SendPreviewStart", "text_len", len(text))
			handle, err := starter.SendPreviewStart(sp.ctx, sp.replyCtx, text)
			if err != nil {
				slog.Debug("stream preview: start failed, degrading", "error", err)
				sp.degraded = true
				return
			}
			sp.previewMsgID = handle
		} else {
			if err := sp.platform.Send(sp.ctx, sp.replyCtx, text); err != nil {
				slog.Debug("stream preview: initial send failed", "error", err)
				sp.degraded = true
				return
			}
			sp.previewMsgID = sp.replyCtx
		}
		sp.lastSentText = text
		sp.lastSentViaUpdate = false
		sp.lastSentAt = time.Now()
		return
	}

	// Update existing preview message
	slog.Debug("stream preview: updating via UpdateMessage", "text_len", len(text))
	if err := updater.UpdateMessage(sp.ctx, sp.previewMsgID, text); err != nil {
		slog.Debug("stream preview: update failed, degrading", "error", err)
		sp.degraded = true
		return
	}
	sp.lastSentText = text
	sp.lastSentViaUpdate = true
	sp.lastSentAt = time.Now()
}

// 清理预览消息处理,finish()不会删除它, freeze()之后调用 当frozen preview 应该保留可见 作为一个临时消息
// (如第一次tool调用之前的文本)
func (sp *streamPreview) detachPreview() {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.previewMsgID = nil
}



