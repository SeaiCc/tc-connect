package core

import (
	"context"
	"unicode/utf8"
	"encoding/json"
	"log/slog"
	"strings"
	"time"
	"strconv"
)

// ========================= 常量声明 ====================

const (
	progressStyleLegacy  = "legacy"
	progressStyleCompact = "compact"
	progressStyleCard    = "card"

	// 标记一个structured payload 用于card-style progress
	ProgressCardPayloadPrefix = "__cc_connect_progress_card_v1__:"

	// 确保为Marddown包裹器/代码围栏保留的边距低于平台硬限制
	compactProgressMaxChars = maxPlatformMessageLen - 200

	// 绑定每个平台的progress-card API 调用, hung upstream 请求永久阻塞整个turn 
	compactProgressAPITimeout = 15 * time.Second
)

type ProgressCardState string

const (
	ProgressCardStateRunning   ProgressCardState = "running"
	ProgressCardStateCompleted ProgressCardState = "completed"
	ProgressCardStateFailed    ProgressCardState = "failed"
)

type ProgressCardEntryKind string

const (
	ProgressEntryInfo       ProgressCardEntryKind = "info"
	ProgressEntryThinking ProgressCardEntryKind = "thinking"
	ProgressEntryError      ProgressCardEntryKind = "error"
)

// 
type ProgressCardPayload struct {
	Version   int                 `json:"version,omitempty"`
	Agent     string              `json:"agent,omitempty"`
	Lang      string              `json:"lang,omitempty"`
	State     ProgressCardState   `json:"state,omitempty"`
	Entries   []string            `json:"entries,omitempty"` // legacy fallback
	Items     []ProgressCardEntry `json:"items,omitempty"`   // ordered typed events
	Truncated bool                `json:"truncated"`
}

// 为那些呈现自定义进度卡的平台提供结构化的进度条目
type ProgressCardEntry struct {
	Kind     ProgressCardEntryKind `json:"kind"`
	Text     string                `json:"text"`
	Tool     string                `json:"tool,omitempty"`
	Status   string                `json:"status,omitempty"`
	ExitCode *int                  `json:"exit_code,omitempty"`
	Success  *bool                 `json:"success,omitempty"`
}

// compactProgressWriter 整合中间进度（思考/工具使用）
// 对于支持消息更新的平台，将其整合为一条可编辑的消息。
type compactProgressWriter struct {
	ctx       context.Context
	platform  Platform
	replyCtx  any
	transform func(string) string

	starter PreviewStarter
	updater MessageUpdater
	handle  any

	enabled    bool
	failed     bool
	style      string
	usePayload bool

	content    string
	entries    []string
	items      []ProgressCardEntry
	state      ProgressCardState
	agentName  string
	lang       Language
	truncated  bool
	lastSent   string
	maxEntries int
}

func newCompactProgressWriter(ctx context.Context, p Platform, replyCtx any, agentName string, lang Language, transform func(string) string) *compactProgressWriter {
	w := &compactProgressWriter{
		ctx:        ctx,
		platform:   p,
		replyCtx:   replyCtx,
		transform:  transform,
		style:      progressStyleForPlatform(p),
		state:      ProgressCardStateRunning,
		agentName:  normalizeProgressAgentLabel(agentName),
		lang:       lang,
		maxEntries: 10,
	}
	if w.style != progressStyleCompact && w.style != progressStyleCard {
		slog.Debug("progress writer disabled: unsupported style", "platform", p.Name(), "style", w.style)
		return w
	}
	updater, ok := p.(MessageUpdater)
	if !ok {
		slog.Debug("progress writer disabled: platform has no MessageUpdater", "platform", p.Name(), "style", w.style)
		return w
	}
	w.enabled = true
	w.updater = updater
	if starter, ok := p.(PreviewStarter); ok {
		w.starter = starter
	}
	slog.Debug("progress writer enabled", "platform", p.Name(), "style", w.style, "use_payload", w.usePayload)
	return w
}

// 更新card progress状态(running/copleted/failed) 不添加新的progress entry
func (w *compactProgressWriter) Finalize(state ProgressCardState) bool {
	if !w.enabled || w.failed || w.style != progressStyleCard || !w.usePayload || w.handle == nil {
		return false
	}
	if state == "" {
		state = ProgressCardStateCompleted
	}
	if w.state == state {
		return true
	}
	w.state = state
	w.content = BuildProgressCardPayloadV2(w.items, w.truncated, w.agentName, w.lang, w.state)
	if w.content == "" || w.content == w.lastSent {
		return w.content != ""
	}
	callCtx, cancel := w.withAPITimeout()
	err := w.updater.UpdateMessage(callCtx, w.handle, w.content)
	cancel()
	if err != nil {
		slog.Warn("progress writer: Finalize UpdateMessage failed", "platform", w.platform.Name(), "style", w.style, "error", err)
		w.failed = true
		return false
	}
	w.lastSent = w.content
	return true
}

func (w *compactProgressWriter) withAPITimeout() (context.Context, context.CancelFunc) {
	if _, hasDeadline := w.ctx.Deadline(); hasDeadline {
		return w.ctx, func() {}
	}
	return context.WithTimeout(w.ctx, compactProgressAPITimeout)
}

// 添加一个typed progress 时间并原地更新消息.
// 当style-specific 渲染不可用时, 使用fallback进行紧凑/普通渲染。
func (w *compactProgressWriter) AppendEvent(kind ProgressCardEntryKind, text string, tool string, fallback string) bool {
	return w.AppendStructured(ProgressCardEntry{
		Kind: kind,
		Text: text,
		Tool: tool,
	}, fallback)
}

func (w *compactProgressWriter) AppendStructured(item ProgressCardEntry, fallback string) bool {
	if !w.enabled || w.failed {
		return false
	}
	text := strings.TrimSpace(item.Text)
	fallback = strings.TrimSpace(fallback)
	if text == "" && fallback == "" {
		return true
	}
	if text == "" {
		text = fallback
	}
	if fallback == "" {
		fallback = text
	}
	switch item.Kind {
	case ProgressEntryThinking, ProgressEntryError, ProgressEntryInfo:
		if w.transform != nil {
			text = w.transform(text)
			fallback = w.transform(fallback)
		}
	}
	kind := item.Kind
	if kind == "" {
		kind = ProgressEntryInfo
	}
	item.Kind = kind
	item.Text = text
	item.Tool = strings.TrimSpace(item.Tool)
	item.Status = strings.TrimSpace(item.Status)

	switch w.style {
	case progressStyleCard:
		w.items = append(w.items, item)
		w.entries = append(w.entries, fallback)
		truncated := false
		if w.maxEntries > 0 && len(w.items) > w.maxEntries {
			w.items = w.items[len(w.items)-w.maxEntries:]
			if len(w.entries) > w.maxEntries {
				w.entries = w.entries[len(w.entries)-w.maxEntries:]
			}
			truncated = true
		} else if w.maxEntries > 0 && len(w.entries) > w.maxEntries {
			w.entries = w.entries[len(w.entries)-w.maxEntries:]
			truncated = true
		}
		w.truncated = truncated
		if w.usePayload {
			w.content = BuildProgressCardPayloadV2(w.items, w.truncated, w.agentName, w.lang, w.state)
			if w.content == "" {
				slog.Warn("progress writer: failed to build structured payload", "platform", w.platform.Name())
				w.failed = true
				return false
			}
		} else {
			w.content = renderCardProgressMarkdownFallback(w.entries, truncated)
			w.content = trimCompactProgressText(w.content, compactProgressMaxChars)
		}
	default:
		if w.content == "" {
			w.content = fallback
		} else {
			w.content += "\n\n" + fallback
		}
		w.content = trimCompactProgressText(w.content, compactProgressMaxChars)
	}

	if w.content == w.lastSent {
		return true
	}

	if w.handle == nil {
		if w.starter != nil {
			callCtx, cancel := w.withAPITimeout()
			handle, err := w.starter.SendPreviewStart(callCtx, w.replyCtx, w.content)
			cancel()
			if err != nil || handle == nil {
				slog.Warn("progress writer: SendPreviewStart failed", "platform", w.platform.Name(), "style", w.style, "error", err, "handle_nil", handle == nil)
				w.failed = true
				return false
			}
			w.handle = handle
			w.lastSent = w.content
			return true
		}
		callCtx, cancel := w.withAPITimeout()
		err := w.platform.Send(callCtx, w.replyCtx, w.content)
		cancel()
		if err != nil {
			slog.Warn("progress writer: initial Send failed", "platform", w.platform.Name(), "style", w.style, "error", err)
			w.failed = true
			return false
		}
		w.handle = w.replyCtx
		w.lastSent = w.content
		return true
	}

	callCtx, cancel := w.withAPITimeout()
	err := w.updater.UpdateMessage(callCtx, w.handle, w.content)
	cancel()
	if err != nil {
		slog.Warn("progress writer: UpdateMessage failed", "platform", w.platform.Name(), "style", w.style, "error", err)
		w.failed = true
		return false
	}
	w.lastSent = w.content
	return true
}

// =============================== 辅助方法 ===============================

func progressStyleForPlatform(p Platform) string {
	ps := progressStyleLegacy
	if sp, ok := p.(ProgressStyleProvider); ok {
		ps = normalizeProgressStyle(sp.ProgressStyle())
	}
	return ps
}

// 根据传入的style返回string
func normalizeProgressStyle(style string) string {
	switch strings.ToLower(strings.TrimSpace(style)) {
	case "", progressStyleLegacy:
		return progressStyleLegacy
	case progressStyleCompact:
		return progressStyleCompact
	case progressStyleCard:
		return progressStyleCard
	default:
		return progressStyleLegacy
	}
}

// 标准化平台名称
func normalizeProgressAgentLabel(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "agent":
		return "Agent"
	case "codex":
		return "Codex"
	case "claudecode", "claude-code", "cc":
		return "CC"
	case "gemini":
		return "Gemini"
	case "cursor":
		return "Cursor"
	case "qoder":
		return "Qoder"
	case "iflow":
		return "iFlow"
	case "opencode":
		return "OpenCode"
	case "pi":
		return "PI"
	default:
		n := strings.TrimSpace(name)
		if n == "" {
			return "Agent"
		}
		return strings.ToUpper(n[:1]) + n[1:]
	}
}

func BuildProgressCardPayloadV2(items []ProgressCardEntry, truncated bool, agent string, lang Language, state ProgressCardState) string {
	cleaned := make([]ProgressCardEntry, 0, len(items))
	for _, item := range items {
		text := strings.TrimSpace(item.Text)
		if text == "" {
			continue
		}
		kind := item.Kind
		if kind == "" {
			kind = ProgressEntryInfo
		}
		cleaned = append(cleaned, ProgressCardEntry{
			Kind:     kind,
			Text:     text,
			Tool:     strings.TrimSpace(item.Tool),
			Status:   strings.TrimSpace(item.Status),
			ExitCode: item.ExitCode,
			Success:  item.Success,
		})
	}
	if len(cleaned) == 0 {
		return ""
	}
	if state == "" {
		state = ProgressCardStateRunning
	}
	payload := ProgressCardPayload{
		Version:   2,
		Agent:     strings.TrimSpace(agent),
		Lang:      string(lang),
		State:     state,
		Items:     cleaned,
		Truncated: truncated,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return ProgressCardPayloadPrefix + string(b)
}

func renderCardProgressMarkdownFallback(entries []string, truncated bool) string {
	var b strings.Builder
	b.WriteString("⏳ **Progress**\n")
	if truncated {
		b.WriteString("_Showing latest updates only._\n")
	}
	for i, entry := range entries {
		b.WriteString("\n")
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(". ")
		b.WriteString(strings.ReplaceAll(entry, "\n", "\n   "))
	}
	return b.String()
}

func trimCompactProgressText(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return s
	}
	s = strings.TrimPrefix(s, "…\n")
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	rs := []rune(s)
	tail := strings.TrimLeft(string(rs[len(rs)-maxRunes:]), "\n")
	return "…\n" + tail
}

