package platform

import (
	"context"
	"tc-connect/core/i18n"
	"tc-connect/core/types"
)

type MessagingContext interface {
	Context() context.Context
	I18n() *i18n.I18n
	WaitOutgoing() error

	// 其他依赖
	Renderer() types.Renderer
}

type MessageHandler struct {
	mCtx MessagingContext
}

func NewMessageHandle(mCtx MessagingContext) *MessageHandler {
	return &MessageHandler{mCtx: mCtx}
}

func (m *MessageHandler) Renderer() types.Renderer {
	return m.mCtx.Renderer()
}

// ================== 封装注入的Engine依赖 ==================

// 防止代码过长，封装一层
func (m *MessageHandler) I18nT(key i18n.MsgKey) string {
	return m.mCtx.I18n().T(key)
}

// 防止代码过长，封装一层
func (m *MessageHandler) I18nTf(key i18n.MsgKey, args ...interface{}) string {
	return m.mCtx.I18n().Tf(key, args...)
}
