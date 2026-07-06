package command

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"tc-connect/core/cron"
	"tc-connect/core/doctor"
	"tc-connect/core/i18n"
	"tc-connect/core/mode"
	"tc-connect/core/renderer"
	"tc-connect/core/session"
	"tc-connect/core/types"
	"time"
)

// /new 启动新session
func (h *Handler) CmdNew(msg *types.Message, args []string) {
	sessions, interactiveKey := h.cCtx.Sessions(), msg.SessionKey

	slog.Info("cmdNew: cleaning up old session", "session_key", msg.SessionKey)
	h.cCtx.StatManager().CleanupWithLock(interactiveKey)
	slog.Info("cmdNew: cleanup done, creating new session", "session_key", msg.SessionKey)

	// Clear old session's agent session ID so it cannot be resumed
	old := sessions.GetOrCreateActive(msg.SessionKey)
	old.SetAgentSessionID("", "")
	old.ClearHistory()
	sessions.Save()

	name := ""
	if len(args) > 0 {
		name = strings.Join(args, " ")
	}
	sessions.NewSession(msg.SessionKey, name)
	if name != "" {
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgNewSessionCreatedName), name))
	} else {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgNewSessionCreated))
	}
}

func (h *Handler) CmdList(p types.Platform, msg *types.Message, args []string) {
	// 默认Feishu支持Card
	page := 1
	if len(args) > 0 {
		if n, err := strconv.Atoi(args[0]); err == nil && n > 0 {
			page = n
		}
	}
	card, err := h.Renderer().RenderListCard(msg.SessionKey, page)
	if err != nil {
		h.cCtx.Reply(msg.ReplyCtx, err.Error())
		return
	}
	h.replyWithCard(p, msg.ReplyCtx, card)
}

func (h *Handler) CmdSwitch(msg *types.Message, args []string) {
	if len(args) == 0 {
		h.cCtx.Reply(msg.ReplyCtx, "Usage: /switch <number | id_prefix | name>")
		return
	}
	query := strings.TrimSpace(strings.Join(args, " "))

	slog.Info("cmdSwitch: listing agent sessions", "session_key", msg.SessionKey)
	agent, sessions, interactiveKey := h.cCtx.Agent(), h.cCtx.Sessions(), msg.SessionKey
	agentSessions, err := agent.ListSessions()
	if err != nil {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nTf(i18n.MsgError, err))
		return
	}
	agentSessions = types.FilterOwnedSessions(agentSessions, sessions.KnownAgentSessionIDs())

	matched := session.MatchSession(agentSessions, sessions, query)
	if matched == nil {
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgSwitchNoMatch), query))
		return
	}

	slog.Info("cmdSwitch: cleaning up old session", "session_key", msg.SessionKey)
	h.cCtx.StatManager().CleanupWithLock(interactiveKey)
	slog.Info("cmdSwitch: cleanup done", "session_key", msg.SessionKey)

	session := sessions.GetOrCreateActive(msg.SessionKey)
	session.SetAgentInfo(matched.ID, agent.Name(), matched.Summary)
	session.ClearHistory()
	sessions.Save()

	shortID := matched.ID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	displayName := sessions.GetSessionName(matched.ID)
	if displayName == "" {
		displayName = matched.Summary
	}
	h.cCtx.Reply(msg.ReplyCtx,
		h.I18nTf(i18n.MsgSwitchSuccess, displayName, shortID, matched.MessageCount))
}

func (h *Handler) CmdName(msg *types.Message, args []string) {
	if len(args) == 0 {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgNameUsage))
		return
	}

	agent, sessions := h.cCtx.Agent(), h.cCtx.Sessions()

	// Check if first arg is a number → naming a specific session by list index
	var targetID string
	var name string

	if idx, err := strconv.Atoi(args[0]); err == nil && idx >= 1 {
		// /name <number> <name...>
		if len(args) < 2 {
			h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgNameUsage))
			return
		}
		agentSessions, err := agent.ListSessions()
		if err != nil {
			h.cCtx.Reply(msg.ReplyCtx, h.I18nTf(i18n.MsgError, err))
			return
		}
		agentSessions = types.FilterOwnedSessions(agentSessions, sessions.KnownAgentSessionIDs())
		if idx > len(agentSessions) {
			h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgSwitchNoSession), idx))
			return
		}
		targetID = agentSessions[idx-1].ID
		name = strings.Join(args[1:], " ")
	} else {
		// /name <name...> → current session
		session := sessions.GetOrCreateActive(msg.SessionKey)
		targetID = session.GetAgentSessionID()
		if targetID == "" {
			h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgNameNoSession))
			return
		}
		name = strings.Join(args, " ")
	}

	name = strings.TrimSpace(name)
	if name == "" {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgNameUsage))
		return
	}

	sessions.SetSessionName(targetID, name)

	shortID := targetID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgNameSet), name, shortID))
}

func (h *Handler) CmdCurrent(p types.Platform, msg *types.Message) {
	h.replyWithCard(p, msg.ReplyCtx, h.Renderer().RenderCurrentCard(msg.SessionKey))
}

func (h *Handler) CmdStatus(p types.Platform, msg *types.Message) {
	h.replyWithCard(p, msg.ReplyCtx, h.Renderer().RenderStatusCard(msg.SessionKey, msg.UserID))
}

func (h *Handler) CmdUsage(msg *types.Message) {
	h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgUsageNotSupported))
}

func (h *Handler) CmdHistory(p types.Platform, msg *types.Message, args []string) {
	if len(args) == 0 && types.SupportsCards(p) {
		h.replyWithCard(p, msg.ReplyCtx, h.Renderer().RenderHistoryCard(msg.SessionKey))
		return
	}
	if len(args) == 0 {
		args = []string{"10"}
	}

	sessions := h.cCtx.Sessions()
	s := sessions.GetOrCreateActive(msg.SessionKey)
	n := 10
	if v, err := strconv.Atoi(args[0]); err == nil && v > 0 {
		n = v
	}

	entries := s.GetHistory(n)

	if len(entries) == 0 {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgHistoryEmpty))
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📜 History (last %d):\n\n", len(entries)))
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
	h.cCtx.Reply(msg.ReplyCtx, sb.String())
}

func (h *Handler) CmdAllow(msg *types.Message, args []string) {
	if len(args) == 0 {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgToolAuthNotSupported))
		return
	}
	h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgToolAuthNotSupported))
}

func (h *Handler) CmdModel(p types.Platform, msg *types.Message, args []string) {
	agent, sessions, interactiveKey := h.cCtx.Agent(), h.cCtx.Sessions(), msg.SessionKey

	// FIXME: 暂时没有那么多模型 保留interface用于后续添加功能
	switcher, ok := agent.(types.ModelSwitcher)
	if !ok {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgModelNotSupported))
		return
	}

	if len(args) == 0 {
		fetchCtx, cancel := context.WithTimeout(h.cCtx.Context(), 3*time.Second)
		defer cancel()
		models := switcher.AvailableModels(fetchCtx)
		current := switcher.GetModel()
		h.replyWithCard(p, msg.ReplyCtx, h.Renderer().RenderModelCard(models, current))
		return
	}

	targetInput, ok := types.ParseModelSwitchArgs(args)
	if !ok {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgModelUsage))
		return
	}

	target := strings.TrimSpace(targetInput)
	if types.ModelSwitchNeedsLookup(target) {
		fetchCtx, cancel := context.WithTimeout(h.cCtx.Context(), 10*time.Second)
		defer cancel()
		models := switcher.AvailableModels(fetchCtx)
		target = types.ResolveModelSwitchTarget(target, models)
	}

	// FIXME: NOT SUPPORT AGENT切换模型
	h.cCtx.StatManager().CleanupWithLock(interactiveKey)

	s := sessions.GetOrCreateActive(msg.SessionKey)
	s.SetAgentSessionID("", "")
	s.ClearHistory()
	sessions.Save()

	h.cCtx.Reply(msg.ReplyCtx, h.I18nTf(i18n.MsgModelChanged, target))
}

func (h *Handler) CmdReasoning(msg *types.Message) {
	h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgReasoningNotSupported))
}

func (h *Handler) CmdMode(msg *types.Message) {
	_, ok := h.cCtx.Agent().(types.ModeSwitcher)
	if !ok {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgModeNotSupported))
		return
	}
	// TODO: agent 切换mode 用于其他agent
	h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgModeNotSupported))
}

func (h *Handler) CmdLang(p types.Platform, msg *types.Message, args []string) {
	if len(args) == 0 {
		cur := h.cCtx.I18n().CurrentLang()
		name := i18n.LangDisplayName(cur)
		text := h.I18nTf(i18n.MsgLangCurrent, name)
		if _, ok := p.(types.CardSender); ok {
			h.replyWithCard(p, msg.ReplyCtx, h.Renderer().RenderLangCard())
			return
		}
		var sb strings.Builder
		sb.WriteString(text)
		sb.WriteString("\n\n")
		sb.WriteString("- English: `/lang en`\n")
		sb.WriteString("- 中文: `/lang zh`\n")
		sb.WriteString("- 繁體中文: `/lang zh-TW`\n")
		sb.WriteString("- 日本語: `/lang ja`\n")
		sb.WriteString("- Español: `/lang es`\n")
		sb.WriteString("- Auto: `/lang auto`")
		h.cCtx.Reply(msg.ReplyCtx, sb.String())
		return
	}

	target := strings.ToLower(strings.TrimSpace(args[0]))
	var lang i18n.Language
	switch target {
	case "en", "english":
		lang = i18n.LangEnglish
	case "zh", "cn", "chinese", "中文":
		lang = i18n.LangChinese
	case "zh-tw", "zh_tw", "zhtw", "繁體", "繁体":
		lang = i18n.LangTraditionalChinese
	case "ja", "jp", "japanese", "日本語":
		lang = i18n.LangJapanese
	case "auto":
		lang = i18n.LangAuto
	default:
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgLangInvalid))
		return
	}

	h.cCtx.I18n().SetLang(lang)
	name := i18n.LangDisplayName(lang)
	h.cCtx.Reply(msg.ReplyCtx, h.I18nTf(i18n.MsgLangChanged, name))
}

func (h *Handler) CmdQuiet(msg *types.Message) {
	display := h.cCtx.DisplayCfg()
	// ThinkingMessages 和 ToolMessage 的静音toggle
	isQuiet := display.ThinkingMessages || display.ToolMessages
	display.ThinkingMessages = !isQuiet
	display.ToolMessages = !isQuiet

	tm := display.ThinkingMessages
	tool := display.ToolMessages
	if err := h.cCtx.SaveDisplayCfg(&tm, nil, nil, &tool); err != nil {
		slog.Error("failed to persist display config after /quiet", "error", err)
	}

	if isQuiet {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgQuietOn))
	} else {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgQuietOff))
	}
}

func (h *Handler) CmdProvider(msg *types.Message) {
	h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgProviderNotSupported))
}

// ===================== cmdMemory =====================

func (h *Handler) CmdMemory(msg *types.Message, args []string) {
	mp, ok := h.cCtx.Agent().(types.MemoryFileProvider)
	if !ok {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgMemoryNotSupported))
		return
	}

	if len(args) == 0 {
		// /memory — show project memory
		h.showMemoryFile(msg, mp.ProjectMemoryFile(), false)
		return
	}

	sub := matchSubCommand(strings.ToLower(args[0]), []string{"add", "global", "show", "help"})
	switch sub {
	case "add":
		text := strings.TrimSpace(strings.Join(args[1:], " "))
		if text == "" {
			h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgMemoryAddUsage))
			return
		}
		h.appendMemoryFile(msg, mp.ProjectMemoryFile(), text)

	case "global":
		if len(args) == 1 {
			// /memory global — show global memory
			h.showMemoryFile(msg, mp.GlobalMemoryFile(), true)
			return
		}
		if strings.ToLower(args[1]) == "add" {
			text := strings.TrimSpace(strings.Join(args[2:], " "))
			if text == "" {
				h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgMemoryAddUsage))
				return
			}
			h.appendMemoryFile(msg, mp.GlobalMemoryFile(), text)
		} else {
			h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgMemoryAddUsage))
		}

	case "show":
		h.showMemoryFile(msg, mp.ProjectMemoryFile(), false)

	case "help", "--help", "-h":
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgMemoryAddUsage))

	default:
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgMemoryAddUsage))
	}
}

func (h *Handler) showMemoryFile(msg *types.Message, filePath string, isGlobal bool) {
	if filePath == "" {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgMemoryNotSupported))
		return
	}

	data, err := os.ReadFile(filePath)
	if err != nil || len(strings.TrimSpace(string(data))) == 0 {
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgMemoryEmpty), filePath))
		return
	}

	content := string(data)
	if len([]rune(content)) > 2000 {
		content = string([]rune(content)[:2000]) + "\n\n... (truncated)"
	}

	if isGlobal {
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgMemoryShowGlobal), filePath, content))
	} else {
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgMemoryShowProject), filePath, content))
	}
}

func (h *Handler) appendMemoryFile(msg *types.Message, filePath, text string) {
	if filePath == "" {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgMemoryNotSupported))
		return
	}

	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgMemoryAddFailed), err))
		return
	}

	f, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgMemoryAddFailed), err))
		return
	}
	defer f.Close()

	entry := "\n- " + text + "\n"
	if _, err := f.WriteString(entry); err != nil {
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgMemoryAddFailed), err))
		return
	}

	h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgMemoryAdded), filePath))
}

// ================= cmdCron 相关 =================

func (h *Handler) CmdCron(p types.Platform, msg *types.Message, args []string) {
	if h.cCtx.CronScheduler() == nil {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgCronNotAvailable))
		return
	}

	if len(args) == 0 {
		if _, ok := p.(types.CardSender); !ok {
			slog.Debug("cmdCron:: Do not support Card")
			h.cmdCronList(msg)
			return
		}
		h.replyWithCard(p, msg.ReplyCtx, h.Renderer().RenderCronCard(msg.SessionKey, msg.UserID))
		return
	}

	sub := matchSubCommand(strings.ToLower(args[0]), []string{
		"add", "addexec", "list", "del", "delete", "rm", "remove", "enable", "disable", "mute", "unmute", "setup",
	})
	switch sub {
	case "add":
		h.cmdCronAdd(msg, args[1:])
	case "addexec":
		h.cmdCronAddExec(msg, args[1:])
	case "list":
		h.cmdCronList(msg)
	case "del", "delete", "rm", "remove":
		h.cmdCronDel(msg, args[1:])
	case "enable":
		h.cmdCronToggle(msg, args[1:], true)
	case "disable":
		h.cmdCronToggle(msg, args[1:], false)
	case "mute":
		h.cmdCronMute(msg, args[1:], true)
	case "unmute":
		h.cmdCronMute(msg, args[1:], false)
	case "setup":
		h.cmdCronSetup(msg)
	default:
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgCronUsage))
	}
}

func (h *Handler) cmdCronList(msg *types.Message) {
	jobs := h.cCtx.CronScheduler().Store().ListBySessionKey(msg.SessionKey)
	if len(jobs) == 0 {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgCronEmpty))
		return
	}

	lang := h.cCtx.I18n().CurrentLang()
	now := time.Now()
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(h.I18nT(i18n.MsgCronListTitle), len(jobs)))
	sb.WriteString("\n")
	sb.WriteString("\n")

	for i, j := range jobs {
		if i > 0 {
			sb.WriteString("\n")
		}

		status := "✅"
		if !j.Enabled {
			status = "⏸"
		}
		desc := j.Description
		if desc == "" {
			if j.IsShellJob() {
				desc = "🖥 " + types.TruncateStr(j.Exec, 60)
			} else {
				desc = types.TruncateStr(j.Prompt, 60)
			}
		}
		if j.Mute {
			desc += " [mute]"
		}
		sb.WriteString(fmt.Sprintf("%s %s\n", status, desc))

		sb.WriteString(fmt.Sprintf("ID: %s\n", j.ID))

		human := cron.CronExprToHuman(j.CronExpr, lang)
		sb.WriteString(h.I18nTf(i18n.MsgCronScheduleLabel, human, j.CronExpr))

		nextRun := h.cCtx.CronScheduler().NextRun(j.ID)
		if !nextRun.IsZero() {
			fmtStr := cron.CronTimeFormat(nextRun, now)
			sb.WriteString(h.I18nTf(i18n.MsgCronNextRunLabel, nextRun.Format(fmtStr)))
		}

		if !j.LastRun.IsZero() {
			fmtStr := cron.CronTimeFormat(j.LastRun, now)
			sb.WriteString(h.I18nTf(i18n.MsgCronLastRunLabel, j.LastRun.Format(fmtStr)))
			if j.LastError != "" {
				sb.WriteString(fmt.Sprintf(" (failed: %s)", types.TruncateStr(j.LastError, 40)))
			}
			sb.WriteString("\n")
		}
	}

	sb.WriteString(fmt.Sprintf("\n%s", h.I18nT(i18n.MsgCronListFooter)))
	h.cCtx.Reply(msg.ReplyCtx, sb.String())
}

// /cron add <min> <hour> <day> <month> <weekday> <prompt...>
// /cron add 0 6 * * * 收集 GitHub Trending 数据整理成简报发给我
func (h *Handler) cmdCronAdd(msg *types.Message, args []string) {
	// /cron add <min> <hour> <day> <month> <weekday> <prompt...>
	if len(args) < 6 {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgCronAddUsage))
		return
	}

	cronExpr := strings.Join(args[:5], " ")
	prompt := strings.Join(args[5:], " ")

	job := &cron.CronJob{
		ID:         cron.GenerateCronID(),
		Project:    h.cCtx.Agent().Name(),
		SessionKey: msg.SessionKey,
		CronExpr:   cronExpr,
		Prompt:     prompt,
		Enabled:    true,
		CreatedAt:  time.Now(),
	}

	if err := h.cCtx.CronScheduler().AddJob(job); err != nil {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nTf(i18n.MsgError, err))
		return
	}

	h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgCronAdded), job.ID, cronExpr, types.TruncateStr(prompt, 60)))
}

// 执行shell命令
// /cron addexec 0 6 * * * df -h
func (h *Handler) cmdCronAddExec(msg *types.Message, args []string) {
	if !h.cCtx.IsAdmin(msg.UserID) {
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgAdminRequired), "/cron addexec"))
		return
	}

	// /cron addexec <min> <hour> <day> <month> <weekday> <shell command...>
	if len(args) < 6 {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgCronAddExecUsage))
		return
	}

	cronExpr := strings.Join(args[:5], " ")
	shellCmd := strings.Join(args[5:], " ")

	job := &cron.CronJob{
		ID:         cron.GenerateCronID(),
		Project:    h.cCtx.Agent().Name(),
		SessionKey: msg.SessionKey,
		CronExpr:   cronExpr,
		Exec:       shellCmd,
		Enabled:    true,
		CreatedAt:  time.Now(),
	}

	if err := h.cCtx.CronScheduler().AddJob(job); err != nil {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nTf(i18n.MsgError, err))
		return
	}

	h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgCronAddedExec), job.ID, cronExpr, types.TruncateStr(shellCmd, 60)))
}

// /cron del <id>
func (h *Handler) cmdCronDel(msg *types.Message, args []string) {
	if len(args) == 0 {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgCronDelUsage))
		return
	}
	id := args[0]
	if h.cCtx.CronScheduler().RemoveJob(id) {
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgCronDeleted), id))
	} else {
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgCronNotFound), id))
	}
}

// /cron enable|disable <id>
func (h *Handler) cmdCronToggle(msg *types.Message, args []string, enable bool) {
	if len(args) == 0 {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgCronDelUsage))
		return
	}
	id := args[0]
	var err error
	if enable {
		err = h.cCtx.CronScheduler().EnableJob(id)
	} else {
		err = h.cCtx.CronScheduler().DisableJob(id)
	}
	if err != nil {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nTf(i18n.MsgError, err))
		return
	}
	if enable {
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgCronEnabled), id))
	} else {
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgCronDisabled), id))
	}
}

// /cron mute|unmute <id>
func (h *Handler) cmdCronMute(msg *types.Message, args []string, mute bool) {
	if len(args) == 0 {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgCronDelUsage))
		return
	}
	id := args[0]
	if !h.cCtx.CronScheduler().Store().SetMute(id, mute) {
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgCronNotFound), id))
		return
	}
	if mute {
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgCronMuted), id))
	} else {
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgCronUnmuted), id))
	}
}

const tcConnectInstructionMarker = "<!-- tc-connect-instructions -->"

type setupResult int

const (
	setupOK       setupResult = iota // 写入成功
	setupExists                      // 已经存在
	setupNoMemory                    // 不支持memory file
	setupError                       // 写入错误
)

// /cron setup
func (h *Handler) cmdCronSetup(msg *types.Message) {
	result, baseName, err := h.setupMemoryFile()
	// 根据返回枚举状态reply
	switch result {
	case setupNoMemory:
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgRelaySetupNoMemory))
	case setupExists:
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgRelaySetupExists), baseName))
	case setupError:
		h.cCtx.Reply(msg.ReplyCtx, h.I18nTf(i18n.MsgError, err))
	case setupOK:
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgCronSetupOK), baseName))
	}
}

// 给agent project 的memroy文件 添加 AgentSystemPrompt(). 返回results
// 文件名(对于message) 和任意错误
func (h *Handler) setupMemoryFile() (setupResult, string, error) {

	mp, ok := h.cCtx.Agent().(types.MemoryFileProvider)
	if !ok {
		return setupNoMemory, "", nil
	}

	filePath := mp.ProjectMemoryFile()
	if filePath == "" {
		return setupNoMemory, "", nil
	}

	baseName := filepath.Base(filePath)

	existing, _ := os.ReadFile(filePath)
	existingText := string(existing)
	block := "\n" + tcConnectInstructionMarker + "\n" + types.AgentSystemPrompt() + "\n"
	if idx := strings.Index(existingText, tcConnectInstructionMarker); idx >= 0 {
		if strings.Contains(existingText[idx:], types.AgentSystemPrompt()) {
			return setupExists, baseName, nil
		}
		updated := strings.TrimRight(existingText[:idx], "\n") + block
		if err := os.WriteFile(filePath, []byte(updated), 0o644); err != nil {
			return setupError, baseName, err
		}
		return setupOK, baseName, nil
	}

	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return setupError, baseName, err
	}

	f, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return setupError, baseName, err
	}
	defer f.Close()

	if _, err := f.WriteString(block); err != nil {
		return setupError, baseName, err
	}

	return setupOK, baseName, nil
}

func (h *Handler) CmdHeartbeat(msg *types.Message) {
	h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgHeartbeatNotAvailable))
}

func (h *Handler) CmdCompress(msg *types.Message) {
	compressor, ok := h.cCtx.Agent().(types.ContextCompressor)
	if !ok || compressor.CompressCommand() == "" {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgCompressNotSupported))
		return
	}

	// TODO: agent压缩功能实现
	h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgCompressNotSupported))
}

// 停止session
func (h *Handler) CmdStop(msg *types.Message) {
	if !h.cCtx.StatManager().StopInteractiveSession(msg.SessionKey, msg.ReplyCtx) {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgNoExecution))
		return
	}
	h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgExecutionStopped))
}

func (h *Handler) CmdHelp(p types.Platform, msg *types.Message) {
	if _, ok := p.(types.CardSender); !ok {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgHelp))
		return
	}
	h.replyWithCard(p, msg.ReplyCtx, h.Renderer().RenderHelpCard())
}

// ========================= cmdCommands相关 =========================

func (h *Handler) CmdCommands(p types.Platform, msg *types.Message, args []string) {
	if len(args) == 0 {
		if _, ok := p.(types.CardSender); !ok {
			h.cmdCommandsList(msg)
			return
		}
		h.replyWithCard(p, msg.ReplyCtx, h.Renderer().RenderCommandsCard())
		return
	}

	sub := matchSubCommand(strings.ToLower(args[0]), []string{
		"list", "add", "addexec", "del", "delete", "rm", "remove",
	})
	switch sub {
	case "list":
		h.cmdCommandsList(msg)
	case "add":
		h.cmdCommandsAdd(msg, args[1:])
	case "addexec":
		h.cmdCommandsAddExec(msg, args[1:])
	case "del", "delete", "rm", "remove":
		h.cmdCommandsDel(msg, args[1:])
	default:
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgCommandsUsage))
	}
}

func (h *Handler) cmdCommandsList(msg *types.Message) {
	cmds := h.cCtx.CommandRegistry().ListAll()
	if len(cmds) == 0 {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgCommandsEmpty))
		return
	}

	var sb strings.Builder
	sb.WriteString(h.I18nTf(i18n.MsgCommandsTitle, len(cmds)))

	for _, c := range cmds {
		// Tag
		tag := ""
		if c.Source == "agent" {
			tag = " [agent]"
		} else if c.Exec != "" {
			tag = " [shell]"
		}
		sb.WriteString(fmt.Sprintf("/%s%s\n", c.Name, tag))

		// Description or fallback
		desc := c.Description
		if desc == "" {
			if c.Exec != "" {
				desc = "$ " + types.TruncateStr(c.Exec, 60)
			} else {
				desc = types.TruncateStr(c.Prompt, 60)
			}
		}
		sb.WriteString(fmt.Sprintf("  %s\n\n", desc))
	}

	sb.WriteString(h.I18nT(i18n.MsgCommandsHint))
	h.cCtx.Reply(msg.ReplyCtx, sb.String())
}

// /commands add finduser 在数据库中查找用户「{{1}}」
// [[commands]]
//
//	name = "finduser"
//	description = ""
//	prompt = "在数据库中查找用户「{{1}}」"
//	exec = ""
//	work_dir = ""
func (h *Handler) cmdCommandsAdd(msg *types.Message, args []string) {
	// /commands add <name> <prompt...>
	if len(args) < 2 {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgCommandsAddUsage))
		return
	}

	name := strings.ToLower(args[0])
	prompt := strings.Join(args[1:], " ")

	if _, exists := h.cCtx.CommandRegistry().Resolve(name); exists {
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgCommandsAddExists), name, name))
		return
	}

	h.cCtx.CommandRegistry().Add(name, "", prompt, "", "", "config")

	if err := h.cCtx.SaveCommand(name, "", prompt, "", ""); err != nil {
		slog.Error("failed to persist command", "error", err)
	}

	h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgCommandsAdded), name, types.TruncateStr(prompt, 80)))
}

// /commands addexec status git status {{args}}
func (h *Handler) cmdCommandsAddExec(msg *types.Message, args []string) {
	if !h.cCtx.IsAdmin(msg.UserID) {
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgAdminRequired), "/commands addexec"))
		return
	}
	// /commands addexec <name> <shell command...>
	// /commands addexec --work-dir <dir> <name> <shell command...>
	if len(args) < 2 {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgCommandsAddExecUsage))
		return
	}

	// Parse --work-dir flag
	workDir := ""
	i := 0
	if args[0] == "--work-dir" && len(args) >= 3 {
		workDir = args[1]
		i = 2
	}

	if i >= len(args) {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgCommandsAddExecUsage))
		return
	}

	name := strings.ToLower(args[i])
	execCmd := ""
	if i+1 < len(args) {
		execCmd = strings.Join(args[i+1:], " ")
	}

	if execCmd == "" {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgCommandsAddExecUsage))
		return
	}

	if _, exists := h.cCtx.CommandRegistry().Resolve(name); exists {
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgCommandsAddExists), name, name))
		return
	}

	h.cCtx.CommandRegistry().Add(name, "", "", execCmd, workDir, "config")

	if err := h.cCtx.SaveCommand(name, "", "", execCmd, workDir); err != nil {
		slog.Error("failed to persist command", "error", err)
	}

	h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgCommandsExecAdded), name, types.TruncateStr(execCmd, 80)))
}

func (h *Handler) cmdCommandsDel(msg *types.Message, args []string) {
	if len(args) == 0 {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgCommandsDelUsage))
		return
	}
	name := strings.ToLower(args[0])

	if !h.cCtx.CommandRegistry().Remove(name) {
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgCommandsNotFound), name))
		return
	}

	if err := h.cCtx.DelCommand(name); err != nil {
		slog.Error("failed to persist command removal", "error", err)
	}

	h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgCommandsDeleted), name))
}

func (h *Handler) CmdSkills(p types.Platform, msg *types.Message) {
	h.replyWithCard(p, msg.ReplyCtx, h.Renderer().RenderSkillsCard())
}

func (h *Handler) CmdConfig(p types.Platform, msg *types.Message, args []string) {
	if len(args) == 0 {
		if _, ok := p.(types.CardSender); !ok {
			items := h.cCtx.GetConfigItems()
			isZh := h.cCtx.I18n().IsZhLike()
			var sb strings.Builder
			sb.WriteString(h.I18nT(i18n.MsgConfigTitle))
			for _, item := range items {
				sb.WriteString(fmt.Sprintf("`%s` = `%s`\n  %s\n\n", item.Key, item.Value, item.Description(isZh)))
			}
			sb.WriteString(h.I18nT(i18n.MsgConfigHint))
			h.cCtx.Reply(msg.ReplyCtx, sb.String())
			return
		}

		h.replyWithCard(p, msg.ReplyCtx, h.Renderer().RenderConfigCard())
		return
	}

	items := h.cCtx.GetConfigItems()
	isZh := h.cCtx.I18n().IsZhLike()
	sub := matchSubCommand(strings.ToLower(args[0]), []string{"get", "set", "reload"})

	switch sub {
	case "reload":
		h.cmdConfigReload(msg)
		return
	case "get":
		if len(args) < 2 {
			h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgConfigGetUsage))
			return
		}
		key := strings.ToLower(args[1])
		for _, item := range items {
			if item.Key == key {
				h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf("`%s` = `%s`\n  %s", key, item.Value, item.Description(isZh)))
				return
			}
		}
		h.cCtx.Reply(msg.ReplyCtx, h.I18nTf(i18n.MsgConfigKeyNotFound, key))

	case "set":
		if len(args) < 3 {
			h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgConfigSetUsage))
			return
		}
		key := strings.ToLower(args[1])
		value := args[2]
		for _, item := range items {
			if item.Key == key {
				if err := h.cCtx.DisplayCfg().Set(key, value); err != nil {
					// FIXME: displaySaveFunc持久化存储
					h.cCtx.Reply(msg.ReplyCtx, h.I18nTf(i18n.MsgError, err))
					return
				}
				h.cCtx.Reply(msg.ReplyCtx, h.I18nTf(i18n.MsgConfigUpdated, key, item.Value))
				return
			}
		}
		h.cCtx.Reply(msg.ReplyCtx, h.I18nTf(i18n.MsgConfigKeyNotFound, key))

	default:
		key := strings.ToLower(sub)
		for _, item := range items {
			if item.Key == key {
				if len(args) >= 2 {
					if err := h.cCtx.DisplayCfg().Set(key, args[1]); err != nil {
						h.cCtx.Reply(msg.ReplyCtx, h.I18nTf(i18n.MsgError, err))
						return
					}
					h.cCtx.Reply(msg.ReplyCtx, h.I18nTf(i18n.MsgConfigUpdated, key, item.Value))
				} else {
					h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf("`%s` = `%s`\n  %s", key, item.Value, item.Description(isZh)))
				}
				return
			}
		}
		h.cCtx.Reply(msg.ReplyCtx, h.I18nTf(i18n.MsgConfigKeyNotFound, key))
	}
}

func (h *Handler) cmdConfigReload(msg *types.Message) {
	result, err := h.cCtx.ReloadConfig()
	if err != nil {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nTf(i18n.MsgError, err))
		return
	}
	h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgConfigReloaded),
		result.DisplayUpdated, result.ProvidersUpdated, result.CommandsUpdated))
}

func (h *Handler) CmdDoctor(msg *types.Message) {
	results := doctor.RunDoctorChecks(h.cCtx.Context(), h.cCtx.Agent(), h.cCtx.Platform())
	report := doctor.FormatDoctorResults(results, h.cCtx.I18n())
	h.cCtx.Reply(msg.ReplyCtx, report)
}

func (h *Handler) CmdUpgrade(msg *types.Message) {
	h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgUpgradeDevBuild))
}

// RestartCh is signaled when /restart is invoked. main listens on it
// to perform a graceful shutdown followed by syscall.Exec.
var RestartCh = make(chan types.RestartRequest, 1)

func (h *Handler) CmdRestart(p types.Platform, msg *types.Message) {
	h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgRestarting))
	select {
	case RestartCh <- types.RestartRequest{
		SessionKey: msg.SessionKey,
		Platform:   p.Name(),
	}:
	default:
	}
}

// ============================= cmdAlias相关 =============================

func (h *Handler) CmdAlias(p types.Platform, msg *types.Message, args []string) {
	if len(args) == 0 {
		if _, ok := p.(types.CardSender); !ok {
			h.cmdAliasList(msg)
			return
		}
		h.replyWithCard(p, msg.ReplyCtx, h.Renderer().RenderAliasCard())
		return
	}

	sub := matchSubCommand(strings.ToLower(args[0]), []string{"list", "add", "del", "delete", "remove"})
	switch sub {
	case "list":
		h.cmdAliasList(msg)
	case "add":
		h.cmdAliasAdd(msg, args[1:])
	case "del", "delete", "remove":
		h.cmdAliasDel(msg, args[1:])
	default:
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgAliasUsage))
	}
}

func (h *Handler) cmdAliasList(msg *types.Message) {
	aliases := h.cCtx.ListAliases()
	if len(aliases) == 0 {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgAliasEmpty))
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(h.I18nT(i18n.MsgAliasListHeader), len(aliases)))
	sb.WriteString("\n")

	names := make([]string, 0, len(aliases))
	for n := range aliases {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, n := range names {
		sb.WriteString(fmt.Sprintf("  %s → %s\n", n, aliases[n]))
	}
	h.cCtx.Reply(msg.ReplyCtx, strings.TrimRight(sb.String(), "\n"))
}

func (h *Handler) cmdAliasAdd(msg *types.Message, args []string) {
	if len(args) < 2 {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgAliasUsage))
		return
	}
	name := args[0]
	command := strings.Join(args[1:], " ")
	if !strings.HasPrefix(command, "/") {
		command = "/" + command
	}

	if err := h.cCtx.SaveAddAlias(name, command); err != nil {
		slog.Error("alias: save failed", "error", err)
	}

	h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgAliasAdded), name, command))
}

func (h *Handler) cmdAliasDel(msg *types.Message, args []string) {
	if len(args) < 1 {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgAliasUsage))
		return
	}
	name := args[0]

	exists, err := h.cCtx.SaveDelAlias(name)
	if err != nil {
		slog.Error("alias: save failed", "error", err)
	}

	if !exists {
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgAliasNotFound), name))
		return
	}

	h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgAliasDeleted), name))
}

func (h *Handler) CmdDelete(p types.Platform, msg *types.Message, args []string) {
	agent, sessions := h.cCtx.Agent(), h.cCtx.Sessions()
	deleter, ok := agent.(types.SessionDeleter)
	if !ok {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgDeleteNotSupported))
		return
	}

	if len(args) == 0 {
		if _, ok := p.(types.CardSender); ok {
			_ = h.cCtx.StatManager().GetOrCreateDeleteModeState(msg.SessionKey, p, msg.ReplyCtx)
			h.replyWithCard(p, msg.ReplyCtx, h.Renderer().RenderDeleteModeCard(msg.SessionKey))
			return
		}
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgDeleteUsage))
		return
	}
	if len(args) > 1 {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgDeleteUsage))
		return
	}

	agentSessions, err := agent.ListSessions()
	if err != nil {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nTf(i18n.MsgError, err))
		return
	}
	agentSessions = types.FilterOwnedSessions(agentSessions, sessions.KnownAgentSessionIDs())

	prefix := strings.TrimSpace(args[0])
	if isExplicitDeleteBatchArg(prefix) {
		indices, err := parseDeleteBatchIndices(prefix, len(agentSessions))
		if err != nil {
			h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgDeleteUsage))
			return
		}
		h.cmdDeleteBatch(msg, deleter, agentSessions, indices)
		return
	}
	var matched *types.AgentSessionInfo

	if idx, err := strconv.Atoi(prefix); err == nil && idx >= 1 && idx <= len(agentSessions) {
		matched = &agentSessions[idx-1]
	} else {
		for i := range agentSessions {
			if strings.HasPrefix(agentSessions[i].ID, prefix) {
				matched = &agentSessions[i]
				break
			}
		}
	}

	if matched == nil {
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgSwitchNoMatch), prefix))
		return
	}

	h.cCtx.Reply(msg.ReplyCtx, h.deleteSingleSessionReply(msg, deleter, matched))

}

func (h *Handler) cmdDeleteBatch(msg *types.Message, deleter types.SessionDeleter, sessions []types.AgentSessionInfo, indices []int) {
	lines := make([]string, 0, len(indices))
	for _, idx := range indices {
		matched := &sessions[idx-1]
		if line := h.deleteSingleSessionReply(msg, deleter, matched); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgDeleteUsage))
		return
	}
	h.cCtx.Reply(msg.ReplyCtx, strings.Join(lines, "\n"))
}

func (h *Handler) deleteSingleSessionReply(msg *types.Message, deleter types.SessionDeleter, matched *types.AgentSessionInfo) string {
	if matched == nil {
		return ""
	}

	// Prevent deleting the currently active session
	activeSession := h.cCtx.Sessions().GetOrCreateActive(msg.SessionKey)
	if activeSession.GetAgentSessionID() == matched.ID {
		return h.I18nT(i18n.MsgDeleteActiveDenied)
	}

	displayName := mode.DeleteSessionDisplayName(h.cCtx.Sessions(), matched)

	if err := deleter.DeleteSession(matched.ID); err != nil {
		return h.I18nTf(i18n.MsgFailedToDeleteSession, displayName, err)
	}

	// Keep local session snapshot aligned with agent-side deletion.
	h.cCtx.Sessions().DeleteByAgentSessionID(matched.ID)
	h.cCtx.Sessions().SetSessionName(matched.ID, "")
	return fmt.Sprintf(h.I18nT(i18n.MsgDeleteSuccess), displayName)
}

// 根据关键词搜索会话
func (h *Handler) CmdSearch(msg *types.Message, args []string) {
	if len(args) == 0 {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgSearchUsage))
		return
	}

	keyword := strings.ToLower(strings.Join(args, " "))

	// Get all agent sessions
	agent, sessions := h.cCtx.Agent(), h.cCtx.Sessions()
	agentSessions, err := agent.ListSessions()
	if err != nil {
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgSearchError), err))
		return
	}
	agentSessions = types.FilterOwnedSessions(agentSessions, sessions.KnownAgentSessionIDs())

	type searchResult struct {
		id           string
		name         string
		summary      string
		matchType    string // "name" or "message"
		messageCount int
	}

	var results []searchResult

	for _, s := range agentSessions {
		// Check session name (custom name or summary)
		customName := sessions.GetSessionName(s.ID)
		displayName := customName
		if displayName == "" {
			displayName = s.Summary
		}

		// Match by name/summary
		if strings.Contains(strings.ToLower(displayName), keyword) {
			results = append(results, searchResult{
				id:           s.ID,
				name:         displayName,
				summary:      s.Summary,
				matchType:    "name",
				messageCount: s.MessageCount,
			})
			continue
		}

		// Match by session ID prefix
		if strings.HasPrefix(strings.ToLower(s.ID), keyword) {
			results = append(results, searchResult{
				id:           s.ID,
				name:         displayName,
				summary:      s.Summary,
				matchType:    "id",
				messageCount: s.MessageCount,
			})
			continue
		}
	}

	if len(results) == 0 {
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgSearchNoResult), keyword))
		return
	}

	// Build result message
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(h.I18nT(i18n.MsgSearchResult), len(results), keyword))

	for i, r := range results {
		shortID := r.id
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		sb.WriteString(fmt.Sprintf("\n%d. [%s] %s", i+1, shortID, r.name))
	}

	sb.WriteString("\n\n" + h.I18nT(i18n.MsgSearchHint))

	h.cCtx.Reply(msg.ReplyCtx, sb.String())
}

func (h *Handler) CmdShell(msg *types.Message, raw string) {
	// Strip the command prefix ("/shell ", "/sh ", "/exec ", "/run ")
	shellCmd := raw
	for _, prefix := range []string{"/shell ", "/sh ", "/exec ", "/run "} {
		if strings.HasPrefix(strings.ToLower(raw), prefix) {
			shellCmd = raw[len(prefix):]
			break
		}
	}
	shellCmd = strings.TrimSpace(shellCmd)

	if shellCmd == "" {
		h.cCtx.Reply(msg.ReplyCtx, "Usage: /shell <command>\nExample: /shell ls -la")
		return
	}

	workDir := h.commandWorkDir()
	if workDir == "" {
		workDir, _ = os.Getwd()
	}

	go func() {
		ctx, cancel := context.WithTimeout(h.cCtx.Context(), 60*time.Second)
		defer cancel()

		cmd := exec.CommandContext(ctx, "sh", "-c", shellCmd)
		cmd.Dir = workDir
		output, err := cmd.CombinedOutput()

		if ctx.Err() == context.DeadlineExceeded {
			h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgCommandTimeout), shellCmd))
			return
		}

		result := strings.TrimSpace(string(output))
		if err != nil && result == "" {
			result = err.Error()
		}
		if result == "" {
			result = "(no output)"
		}
		if runes := []rune(result); len(runes) > 4000 {
			result = string(runes[:3997]) + "..."
		}

		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf("$ %s\n```\n%s\n```", shellCmd, result))
	}()
}

// 在workdir 中执行 git diff操作
// Parse optional target: /diff [target]
func (h *Handler) CmdDiff(p types.Platform, msg *types.Message, raw string) {
	// Parse optional target: /diff [target]
	diffTarget := ""
	if strings.HasPrefix(strings.ToLower(raw), "/diff ") {
		diffTarget = strings.TrimSpace(raw[6:])
	}

	if strings.HasPrefix(diffTarget, "-") {
		h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgError), "diff target must not start with '-'"))
		return
	}

	// Resolve working directory (same pattern as cmdShell)
	var workDir string

	if workDir == "" {
		if wd, ok := h.cCtx.Agent().(interface{ GetWorkDir() string }); ok {
			workDir = wd.GetWorkDir()
		}
	}
	if workDir == "" {
		workDir, _ = os.Getwd()
	}

	go func() {
		ctx, cancel := context.WithTimeout(h.cCtx.Context(), 60*time.Second)
		defer cancel()

		// Get current branch name and short commit ID
		branchCmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
		branchCmd.Dir = workDir
		branchOut, _ := branchCmd.Output()
		currentBranch := strings.TrimSpace(string(branchOut))
		if currentBranch == "" {
			currentBranch = "unknown"
		}

		commitCmd := exec.CommandContext(ctx, "git", "rev-parse", "--short", "HEAD")
		commitCmd.Dir = workDir
		commitOut, _ := commitCmd.Output()
		commitID := strings.TrimSpace(string(commitOut))
		if commitID == "" {
			commitID = "0000000"
		}

		gitArgs := []string{"diff"}
		if diffTarget != "" {
			gitArgs = append(gitArgs, "--", diffTarget)
		}
		gitCmd := exec.CommandContext(ctx, "git", gitArgs...)
		gitCmd.Dir = workDir
		diffOutput, err := gitCmd.Output()

		if ctx.Err() == context.DeadlineExceeded {
			h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgCommandTimeout), "git diff"))
			return
		}
		if err != nil && len(diffOutput) == 0 {
			h.cCtx.Reply(msg.ReplyCtx, h.I18nTf(i18n.MsgError, err))
			return
		}

		target := diffTarget
		if target == "" {
			target = "HEAD"
		}
		if len(strings.TrimSpace(string(diffOutput))) == 0 {
			h.cCtx.Reply(msg.ReplyCtx, fmt.Sprintf(h.I18nT(i18n.MsgDiffEmpty), target))
			return
		}

		// Try diff2html + FileSender
		if fileSender, ok := p.(types.FileSender); ok {
			title := fmt.Sprintf("%s vs %s", currentBranch, target)
			htmlData, err := diff2html(ctx, diffOutput, workDir, title)
			if err == nil {
				fileName := fmt.Sprintf("%s-%s.html", currentBranch, commitID)
				_ = h.cCtx.WaitOutgoing()
				if err := fileSender.SendFile(h.cCtx.Context(), msg.ReplyCtx, types.FileAttachment{
					MimeType: "text/html", Data: htmlData, FileName: fileName,
				}); err == nil {
					return
				}
			}
			if errors.Is(err, exec.ErrNotFound) {
				h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgDiffNoDiff2HTML))
			}
		}

		// Fallback: plain text diff
		result := strings.TrimSpace(string(diffOutput))
		if runes := []rune(result); len(runes) > 4000 {
			result = string(runes[:3997]) + "..."
		}
		h.cCtx.Reply(msg.ReplyCtx, "```diff\n"+result+"\n```")
	}()
}

// 展示传入文件内容
func (h *Handler) CmdShow(msg *types.Message, args []string) {
	rawRef := strings.TrimSpace(strings.Join(args, " "))
	if rawRef == "" {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgShowUsage))
		return
	}

	workDir := h.commandWorkDir()
	req, err := renderer.BuildReferenceViewRequest(rawRef, workDir)
	if err != nil {
		h.cCtx.Reply(msg.ReplyCtx, h.I18nTf(i18n.MsgShowParseError, rawRef))
		return
	}
	content, err := renderer.RenderReferenceView(req)
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "path does not exist"):
			h.cCtx.Reply(msg.ReplyCtx, h.I18nTf(i18n.MsgShowNotFound, rawRef))
		case strings.Contains(err.Error(), "directory reference cannot carry a location"):
			h.cCtx.Reply(msg.ReplyCtx, h.I18nTf(i18n.MsgShowDirWithLocation, rawRef))
		default:
			h.cCtx.Reply(msg.ReplyCtx, h.I18nTf(i18n.MsgShowReadFailed, err))
		}
		return
	}
	h.cCtx.Reply(msg.ReplyCtx, content)
}

func (h *Handler) CmdDir(msg *types.Message) {
	currentDir := h.cCtx.BaseWorkDir()
	h.cCtx.Reply(msg.ReplyCtx, h.I18nTf(i18n.MsgDirCurrent, currentDir))
}

func (h *Handler) CmdWhoami(p types.Platform, msg *types.Message) {
	if _, ok := p.(types.CardSender); ok {
		h.replyWithCard(p, msg.ReplyCtx, h.Renderer().RenderWhoamiCard(msg))
		return
	}
	h.cCtx.Reply(msg.ReplyCtx, h.formatWhoamiText(msg))
}

func (h *Handler) formatWhoamiText(msg *types.Message) string {
	var sb strings.Builder
	sb.WriteString(h.I18nT(i18n.MsgWhoamiTitle))
	sb.WriteString("\n")

	if msg.UserID != "" {
		sb.WriteString(fmt.Sprintf("User ID: `%s`\n", msg.UserID))
	} else {
		sb.WriteString("User ID: (unknown)\n")
	}
	if msg.UserName != "" {
		sb.WriteString(fmt.Sprintf("Name: %s\n", msg.UserName))
	}
	if msg.Platform != "" {
		sb.WriteString(fmt.Sprintf("Platform: %s\n", msg.Platform))
	}

	chatID := types.ChannelID(msg.SessionKey)
	if chatID != "" {
		sb.WriteString(fmt.Sprintf("Chat ID: `%s`\n", chatID))
	}
	sb.WriteString(fmt.Sprintf("Session Key: `%s`\n", msg.SessionKey))

	sb.WriteString("\n")
	sb.WriteString(h.I18nT(i18n.MsgWhoamiUsage))
	return sb.String()
}

func (h *Handler) CmdWeb(msg *types.Message, args []string) {
	h.cCtx.Reply(msg.ReplyCtx, h.I18nT(i18n.MsgWebNotSupported))
}
