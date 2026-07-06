package renderer

import (
	"tc-connect/core/cron"
	"tc-connect/core/i18n"
	"tc-connect/core/interactive"
	"tc-connect/core/session"
	"tc-connect/core/skill"
	"tc-connect/core/types"
	"time"
)

const listPageSize = 20

// 纯格式化 应剔除e.ctx相关IO操作
type RenderContext interface {
	// 通用配置
	DisplayCfg() *types.DisplayCfg
	I18n() *i18n.I18n
	Name() string
	GetConfigItems() []types.ConfigItem

	// 核心功能
	Agent() types.Agent
	CommandRegistry() *types.CommandRegistry
	CronScheduler() *cron.CronScheduler
	Platform() types.Platform
	Sessions() *session.SessionManager
	SkillRegistry() *skill.SkillRegistry

	// 其他依赖
	StatManager() *interactive.Manager

	// 辅助
	StartedAt() time.Time
	GetAliases() map[string]string
}

type Renderer struct {
	references ReferenceRenderCfg
	rCtx       RenderContext // 命名防止与Engine 原有 ctx冲突
}

// 构造
func NewRenderer(rCtx RenderContext) *Renderer {
	return &Renderer{
		rCtx:       rCtx,
		references: DefaultReferenceRenderCfg(),
	}
}

// ================== 封装注入的Engine依赖 ==================
// 防止代码过长，封装一层
func (r *Renderer) I18nT(key i18n.MsgKey) string {
	return r.rCtx.I18n().T(key)
}

// 防止代码过长，封装一层
func (r *Renderer) I18nTf(key i18n.MsgKey, args ...interface{}) string {
	return r.rCtx.I18n().Tf(key, args...)
}

// ===================== Renderer辅助方法 =====================
func (r *Renderer) cardBackButton() types.CardButton {
	return types.DefaultBtn(r.I18nT(i18n.MsgCardBack), "nav:/help")
}

func (r *Renderer) cardPrevButton(action string) types.CardButton {
	return types.DefaultBtn(r.I18nT(i18n.MsgCardPrev), action)
}

func (r *Renderer) cardNextButton(action string) types.CardButton {
	return types.DefaultBtn(r.I18nT(i18n.MsgCardNext), action)
}

func (r *Renderer) simpleCard(title, color, content string) *types.Card {
	return types.NewCard().Title(title, color).Markdown(content).Buttons(r.cardBackButton()).Build()
}
