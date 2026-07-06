package command

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"tc-connect/core/cron"
	"tc-connect/core/i18n"
	"tc-connect/core/interactive"
	"tc-connect/core/session"
	"tc-connect/core/types"
)

type CommandContext interface {
	// 通用
	BaseWorkDir() string
	Context() context.Context
	DisplayCfg() *types.DisplayCfg
	I18n() *i18n.I18n
	WaitOutgoing() error
	GetConfigItems() []types.ConfigItem
	IsAdmin(userID string) bool

	// 核心功能
	Agent() types.Agent // agent.Agent (包含 ModelSwitcher, ModeSwitcher 等)
	CommandRegistry() *types.CommandRegistry
	CronScheduler() *cron.CronScheduler
	Platform() types.Platform
	Sessions() *session.SessionManager // session.SessionManager

	// 平台通信
	Reply(ctx any, msg string)

	// 别名管理
	ListAliases() map[string]string
	SaveAddAlias(name, command string) error // 带持久化的添加
	SaveDelAlias(name string) (bool, error)  // 带持久化的删除

	// 其他依赖
	Renderer() types.Renderer
	StatManager() *interactive.Manager

	// 持久化方法
	SaveDisplayCfg(thinkingMessages *bool, thinkingMaxLen, toolMaxLen *int, toolMessages *bool) error
	SaveCommand(name, description, prompt, exec, workDir string) error
	DelCommand(name string) error
	ReloadConfig() (*types.ConfigReloadResult, error)
}

// Handler 命令处理器，封装 CommandContext
type Handler struct {
	cCtx CommandContext
}

// NewHandler 创建命令处理器
func NewHandler(cCtx CommandContext) *Handler {
	return &Handler{cCtx: cCtx}
}

// ================== 封装注入的Engine依赖 ==================

// 防止代码过长，封装一层
func (h *Handler) I18nT(key i18n.MsgKey) string {
	return h.cCtx.I18n().T(key)
}

// 防止代码过长，封装一层
func (h *Handler) I18nTf(key i18n.MsgKey, args ...interface{}) string {
	return h.cCtx.I18n().Tf(key, args...)
}

func (h *Handler) Renderer() types.Renderer {
	return h.cCtx.Renderer()
}

// ===================== Command辅助方法 =====================

func (h *Handler) commandWorkDir() string {
	if switcher, ok := h.cCtx.Agent().(types.WorkDirSwitcher); ok {
		if wd := strings.TrimSpace(switcher.GetWorkDir()); wd != "" {
			return types.NormalizeWorkspacePath(wd)
		}
	}
	if wd, ok := h.cCtx.Agent().(interface{ GetWorkDir() string }); ok {
		if dir := strings.TrimSpace(wd.GetWorkDir()); dir != "" {
			return types.NormalizeWorkspacePath(dir)
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		return types.NormalizeWorkspacePath(cwd)
	}
	return ""
}

// 通过CardSender 发送接口card,
// 对于没有card支持的平台,渲染为plain text (不直接fallback)
func (h *Handler) replyWithCard(p types.Platform, replyCtx any, card *types.Card) {
	if card == nil {
		slog.Error("ReplyWithCard: nil card", "platform", p.Name())
		return
	}
	if err := h.cCtx.WaitOutgoing(); err != nil {
		slog.Warn("outgoing rate limit: context cancelled", "platform", p.Name(), "error", err)
		return
	}
	if cs, ok := p.(types.CardSender); ok {
		rendered := h.Renderer().RenderCardForPlatform(p, card)
		if err := cs.ReplyCard(h.cCtx.Context(), replyCtx, rendered); err != nil {
			slog.Error("card reply failed", "platform", p.Name(), "error", err)
		}
		return
	}
	h.cCtx.Reply(replyCtx, h.Renderer().RenderCardForPlatform(p, card).RenderText())
}

// 不支持InlineButtonSender, 使用plain text reply 发送返回消息
func (h *Handler) replyWithButtons(p types.Platform, replyCtx any, content string) {
	if err := h.cCtx.WaitOutgoing(); err != nil {
		slog.Warn("outgoing rate limit: context cancelled", "platform", p.Name(), "error", err)
		return
	}
	h.cCtx.Reply(replyCtx, content)
}

// ===================== Command辅助方法 =====================

// 与一系列子名称做前缀匹配
func matchSubCommand(input string, candidates []string) string {
	for _, c := range candidates {
		if input == c {
			return c
		}
	}
	var matched string
	for _, c := range candidates {
		if strings.HasPrefix(c, input) {
			if matched != "" {
				return input // ambiguous → return raw input (will hit default)
			}
			matched = c
		}
	}
	if matched != "" {
		return matched
	}
	return input
}

func isExplicitDeleteBatchArg(arg string) bool {
	if strings.Contains(arg, ",") {
		return true
	}
	if !strings.Contains(arg, "-") {
		return false
	}
	for _, r := range arg {
		if (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func parseDeleteBatchIndices(spec string, max int) ([]int, error) {
	parts := strings.Split(spec, ",")
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty batch spec")
	}
	seen := make(map[int]struct{}, len(parts))
	indices := make([]int, 0, len(parts))

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty batch item")
		}

		if strings.Contains(part, "-") {
			bounds := strings.Split(part, "-")
			if len(bounds) != 2 || bounds[0] == "" || bounds[1] == "" {
				return nil, fmt.Errorf("invalid range %q", part)
			}
			start, err := strconv.Atoi(bounds[0])
			if err != nil {
				return nil, err
			}
			end, err := strconv.Atoi(bounds[1])
			if err != nil {
				return nil, err
			}
			if start < 1 || end < 1 || start > end || end > max {
				return nil, fmt.Errorf("range %q out of bounds", part)
			}
			for idx := start; idx <= end; idx++ {
				if _, ok := seen[idx]; ok {
					continue
				}
				seen[idx] = struct{}{}
				indices = append(indices, idx)
			}
			continue
		}

		idx, err := strconv.Atoi(part)
		if err != nil {
			return nil, err
		}
		if idx < 1 || idx > max {
			return nil, fmt.Errorf("index %d out of bounds", idx)
		}
		if _, ok := seen[idx]; ok {
			continue
		}
		seen[idx] = struct{}{}
		indices = append(indices, idx)
	}

	return indices, nil
}

func diff2html(ctx context.Context, diff []byte, workDir, title string) ([]byte, error) {
	if _, err := exec.LookPath("diff2html"); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "diff2html", "-i", "stdin", "-o", "stdout", "--title", title)
	cmd.Dir = workDir
	cmd.Stdin = bytes.NewReader(diff)
	return cmd.Output()
}
