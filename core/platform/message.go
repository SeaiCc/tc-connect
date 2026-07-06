package platform

import (
	"fmt"
	"log/slog"
	"strings"
	"tc-connect/core/i18n"
	"tc-connect/core/types"
	"time"
)

// 慢-op阈值, 操作超出阈值会产生slog.Warn 以便快速找出瓶颈
const (
	slowPlatformSend = 2 * time.Second // 平台响应,发送
)

// ============================ Message Reply ============================

// 应用outgoing 比率限制和p.Reply
func (m *MessageHandler) ReplyWithError(p types.Platform, replyCtx any, content string) error {
	// context 错误
	if err := m.mCtx.WaitOutgoing(); err != nil {
		slog.Warn("outgoing rate limit: context cancelled", "platform", p.Name(), "error", err)
		return err
	}
	start := time.Now()
	// reply错误
	if err := p.Reply(m.mCtx.Context(), replyCtx, content); err != nil {
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
func (m *MessageHandler) reply(p types.Platform, replyCtx any, content string) {
	_ = m.ReplyWithError(p, replyCtx, content)
}

// ============================ Message Send ============================

// 调用 Send判断是否发送失败或响应发送是否较慢
func (m *MessageHandler) sendAlreadyRenderedWithError(p types.Platform, replyCtx any, content string) error {
	start := time.Now()
	if err := p.Send(m.mCtx.Context(), replyCtx, content); err != nil {
		slog.Error("platform send failed", "platform", p.Name(), "error", err, "content_len", len(content))
		return err
	}
	if elapsed := time.Since(start); elapsed >= slowPlatformSend {
		slog.Warn("slow platform send", "platform", p.Name(), "elapsed", elapsed, "content_len", len(content))
	}
	return nil
}

// 应用了速率限制和p.Send. 打印 等待取消和平台失败,并返回一个非nil的错误
func (m *MessageHandler) SendWithError(p types.Platform, replyCtx any, content string) error {
	if err := m.mCtx.WaitOutgoing(); err != nil {
		slog.Warn("outgoing rate limit: context cancelled", "platform", p.Name, "error", err)
		return err
	}
	return m.sendAlreadyRenderedWithError(p, replyCtx, content)
}

// 发送消息， 携带带工作空间 并返回错误
func (m *MessageHandler) SendWithErrorForWorkspace(p types.Platform, replyCtx any, content, workspaceDir string) error {
	if err := m.mCtx.WaitOutgoing(); err != nil {
		slog.Warn("outgoing rate limit: context cancelled", "platform", p.Name(), "error", err)
		return err
	}
	content = m.Renderer().RenderOutgoingContentForWorkspace(p, content, workspaceDir)
	return m.sendAlreadyRenderedWithError(p, replyCtx, content)
}

// 发送消息， 携带带工作空间 无返回错误
func (m *MessageHandler) SendForWorkspace(p types.Platform, replyCtx any, content, workspaceDir string) {
	_ = m.SendWithErrorForWorkspace(p, replyCtx, content, workspaceDir)
}

// 发送一个卡片(非reply)作为一个新message
func (m *MessageHandler) sendWithCard(p types.Platform, replyCtx any, card *types.Card) {
	if card == nil {
		slog.Error("sendWithCard: nil card", "platform", p.Name())
		return
	}
	if err := m.mCtx.WaitOutgoing(); err != nil {
		slog.Warn("outgoing rate limit: context cancelled", "platform", p.Name(), "error", err)
		return
	}
	if cs, ok := p.(types.CardSender); ok {
		rendered := m.Renderer().RenderCardForPlatform(p, card)
		if err := cs.SendCard(m.mCtx.Context(), replyCtx, rendered); err != nil {
			slog.Error("card send failed", "platform", p.Name(), "error", err)
		}
		return
	}
	m.SendWithError(p, replyCtx, m.Renderer().RenderCardForPlatform(p, card).RenderText())
}

// 从AskUserQuestion list渲染一个问题(根据index), qIdx 是0-based 的用于展示的问题下标
func (m *MessageHandler) SendAskQuestionPrompt(p types.Platform, replyCtx any, questions []types.UserQuestion, qIdx int) {
	if qIdx >= len(questions) {
		return
	}
	q := questions[qIdx]
	total := len(questions)

	titleSuffix := ""
	if total > 1 {
		titleSuffix = fmt.Sprintf(" (%d/%d)", qIdx+1, total)
	}

	// Try card (Feishu/Lark)
	if _, ok := p.(types.CardSender); ok {
		cb := types.NewCard().Title(m.I18nT(i18n.MsgAskQuestionTitle)+titleSuffix, "blue")
		body := "**" + q.Question + "**"
		if q.MultiSelect {
			body += m.I18nT(i18n.MsgAskQuestionMulti)
		}
		cb.Markdown(body)
		for i, opt := range q.Options {
			desc := opt.Label
			if opt.Description != "" {
				desc += " — " + opt.Description
			}
			answerData := fmt.Sprintf("askq:%d:%d", qIdx, i+1)
			cb.ListItemBtnExtra(desc, opt.Label, "default", answerData, map[string]string{
				"askq_label":    opt.Label,
				"askq_question": q.Question,
			})
		}
		cb.Note(m.I18nT(i18n.MsgAskQuestionNote))
		m.sendWithCard(p, replyCtx, cb.Build())
		return
	}

	// Plain text fallback
	var sb strings.Builder
	sb.WriteString("❓ **")
	sb.WriteString(q.Question)
	sb.WriteString("**")
	sb.WriteString(titleSuffix)
	if q.MultiSelect {
		sb.WriteString(m.I18nT(i18n.MsgAskQuestionMulti))
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
	sb.WriteString(fmt.Sprintf("\n%s", m.I18nT(i18n.MsgAskQuestionNote)))
	m.SendWithError(p, replyCtx, sb.String())
}
