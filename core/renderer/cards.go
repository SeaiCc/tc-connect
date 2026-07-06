package renderer

import (
	"fmt"
	"sort"
	"strings"
	"tc-connect/core/cron"
	"tc-connect/core/doctor"
	"tc-connect/core/i18n"
	"tc-connect/core/mode"
	"tc-connect/core/session"
	"tc-connect/core/types"
	"time"
)

func (r *Renderer) RenderHelpCard() *types.Card {
	return r.RenderHelpGroupCard("session") // 默认session，可选：session|agent|tools|system
}

// 渲染帮助卡片
func (r *Renderer) RenderHelpGroupCard(groupKey string) *types.Card {
	sectionTitle := func(key i18n.MsgKey) string {
		section := r.I18nT(key)
		// 找到最后的一行返回
		if idx := strings.IndexByte(section, '\n'); idx >= 0 {
			return section[:idx]
		}
		return section
	}
	tabLabel := func(key i18n.MsgKey) string {
		return strings.Trim(sectionTitle(key), "*")
	}
	commandText := func(command string) string {
		return "**" + command + "**  " + r.I18nT(i18n.MsgKey(strings.TrimPrefix(command, "/")))
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
	cb := types.NewCard().Title(r.I18nT(i18n.MsgHelpTitle), "blue") // help_title
	var tabs []types.CardButton
	for _, group := range groups {
		btnType := "default"
		if group.key == current.key {
			btnType = "primary"
		}
		tabs = append(tabs, types.Btn(tabLabel(group.titleKey), btnType, "nav:/help "+group.key))
	}
	for _, row := range splitHelpTabRows(true, tabs) {
		cb.ButtonsEqual(row...)
	}
	for _, item := range current.items {
		cb.ListItem(commandText(item.command), "▶", item.action)
	}
	cb.Note(r.I18nT(i18n.MsgHelpTip))
	return cb.Build()
}

func (r *Renderer) RenderModeCard() *types.Card {
	switcher, ok := r.rCtx.Agent().(types.ModeSwitcher)
	if !ok {
		return r.simpleCard(r.I18nT(i18n.MsgCardTitleMode), "violet", r.I18nT(i18n.MsgModeNotSupported))
	}

	current := switcher.GetMode()
	modes := switcher.PermissionModes()
	zhLike := r.rCtx.I18n().IsZhLike()

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

	var opts []types.CardSelectOption
	initVal := ""
	for _, m := range modes {
		label := m.Name
		if zhLike {
			label = m.NameZh
		}
		val := "act:/mode " + m.Key
		opts = append(opts, types.CardSelectOption{Text: label, Value: val})
		if m.Key == current {
			initVal = val
		}
	}

	cb := types.NewCard().Title(r.I18nT(i18n.MsgCardTitleMode), "violet").
		Markdown(sb.String()).
		Select(r.I18nT(i18n.MsgModeSelectPlaceholder), opts, initVal).
		Buttons(r.cardBackButton())
	cb.Note(r.I18nTf(i18n.MsgModeUsage, types.FormatModeKeys(modes)))
	return cb.Build()
}

// 渲染语言卡片
func (r *Renderer) RenderLangCard() *types.Card {
	cur := r.rCtx.I18n().CurrentLang()
	name := i18n.LangDisplayName(cur)

	langs := []struct{ code, label string }{
		{"en", "English"}, {"zh", "中文"}, {"zh-TW", "繁體中文"},
		{"ja", "日本語"}, {"es", "Español"}, {"auto", "Auto"},
	}
	var opts []types.CardSelectOption
	initVal := ""
	for _, l := range langs {
		opts = append(opts, types.CardSelectOption{Text: l.label, Value: "act:/lang " + l.code})
		if string(cur) == l.code || (cur == i18n.LangAuto && l.code == "auto") {
			initVal = "act:/lang " + l.code
		}
	}

	return types.NewCard().
		Title(r.I18nT(i18n.MsgCardTitleLanguage), "wathet").
		Markdown(r.I18nTf(i18n.MsgLangCurrent, name)).
		Select(r.I18nT(i18n.MsgLangSelectPlaceholder), opts, initVal).
		Buttons(r.cardBackButton()).
		Build()
}

func (r *Renderer) RenderListCardSafe(sessionKey string, page int) *types.Card {
	card, err := r.RenderListCard(sessionKey, page)
	// 回退simpleCard, 列出session列表
	if err != nil {
		return r.simpleCard(r.I18nTf(i18n.MsgCardTitleSessions, r.rCtx.Agent().Name(), 0), "red", err.Error())
	}
	return card
}

// 展示session列表
func (r *Renderer) RenderListCard(sessionKey string, page int) (*types.Card, error) {
	agentSessions, err := r.rCtx.Agent().ListSessions()
	if err != nil {
		return nil, fmt.Errorf(r.I18nT(i18n.MsgListError), err)
	}
	// 剔除外部CLI创建的session
	agentSessions = types.FilterOwnedSessions(agentSessions, r.rCtx.Sessions().KnownAgentSessionIDs())
	if len(agentSessions) == 0 {
		return r.simpleCard(r.I18nTf(i18n.MsgCardTitleSessions, r.rCtx.Agent().Name(), 0), "turquoise", r.I18nT(i18n.MsgListEmpty)), nil
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

	agentToolName := r.rCtx.Agent().Name()
	activeSession := r.rCtx.Sessions().GetOrCreateActive(sessionKey)
	activeAgentID := activeSession.GetAgentSessionID()

	var titleStr string
	if totalPages > 1 {
		titleStr = r.I18nTf(i18n.MsgCardTitleSessionsPaged, agentToolName, total, page, totalPages)
	} else {
		titleStr = r.I18nTf(i18n.MsgCardTitleSessions, agentToolName, total)
	}

	cb := types.NewCard().Title(titleStr, "turquoise")
	for i := start; i < end; i++ {
		s := agentSessions[i]
		marker := "◻"
		if s.ID == activeAgentID {
			marker = "▶"
		}
		displayName := r.rCtx.Sessions().GetSessionName(s.ID)
		if displayName != "" {
			displayName = "📌 " + displayName
		} else {
			displayName = strings.ReplaceAll(s.Summary, "\n", " ")
			displayName = strings.Join(strings.Fields(displayName), " ")
			if displayName == "" {
				displayName = r.I18nT(i18n.MsgListEmptySummary)
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
			r.I18nTf(i18n.MsgListItem, marker, i+1, displayName, s.MessageCount, s.ModifiedAt.Format("01-02 15:04")),
			fmt.Sprintf("#%d", i+1),
			btnType,
			fmt.Sprintf("act:/switch %d", i+1),
		)
	}

	var navBtns []types.CardButton
	if page > 1 {
		navBtns = append(navBtns, r.cardPrevButton(fmt.Sprintf("nav:/list %d", page-1)))
	}
	navBtns = append(navBtns, r.cardBackButton())
	if page < totalPages {
		navBtns = append(navBtns, r.cardNextButton(fmt.Sprintf("nav:/list %d", page+1)))
	}
	cb.Buttons(navBtns...)

	if totalPages > 1 {
		cb.Note(fmt.Sprintf(r.I18nT(i18n.MsgListPageHint), page, totalPages))
	}

	return cb.Build(), nil
}

// 渲染状态卡片,具体条目见 i18n.go MsgStatusTitle
func (r *Renderer) RenderStatusCard(sessionKey string, userID string) *types.Card {

	platformStr := r.rCtx.Platform().Name()

	cur := r.rCtx.I18n().CurrentLang()
	uptimeStr := i18n.FormatDurationI18n(time.Since(r.rCtx.StartedAt()), cur)

	langStr := fmt.Sprintf("%s (%s)", string(cur), i18n.LangDisplayName(cur))

	var modeStr string
	if ms, ok := r.rCtx.Agent().(types.ModeSwitcher); ok {
		mode := ms.GetMode()
		if mode != "" {
			modeStr = r.I18nTf(i18n.MsgStatusMode, mode)
		}
	}
	thinkingStr := r.I18nT(i18n.MsgDisabledShort)
	if r.rCtx.DisplayCfg().ThinkingMessages {
		thinkingStr = r.I18nT(i18n.MsgEnabledShort)
	}
	toolStr := r.I18nT(i18n.MsgDisabledShort)
	if r.rCtx.DisplayCfg().ToolMessages {
		toolStr = r.I18nT(i18n.MsgEnabledShort)
	}
	modeStr += r.I18nTf(i18n.MsgStatusThinkingMessages, thinkingStr)
	modeStr += r.I18nTf(i18n.MsgStatusToolMessages, toolStr)

	s := r.rCtx.Sessions().GetOrCreateActive(sessionKey)
	sessionDisplayName := r.rCtx.Sessions().GetSessionName(s.GetAgentSessionID())
	if sessionDisplayName == "" {
		sessionDisplayName = s.GetName()
	}
	sessionStr := r.I18nTf(i18n.MsgStatusSession, sessionDisplayName, len(s.History))

	var cronStr string
	if r.rCtx.CronScheduler() != nil {
		if jobs := r.rCtx.CronScheduler().Store().ListBySessionKey(sessionKey); len(jobs) > 0 {
			enabledCount := 0
			for _, j := range jobs {
				if j.Enabled {
					enabledCount++
				}
			}
			cronStr = r.I18nTf(i18n.MsgStatusCron, len(jobs), enabledCount)
		}
	}

	sessionKeyStr := r.I18nTf(i18n.MsgStatusSessionKey, sessionKey)

	userIDStr := ""
	if userID != "" {
		userIDStr = r.I18nTf(i18n.MsgStatusUserID, userID)
	}

	statusText := r.I18nTf(i18n.MsgStatusTitle,
		r.rCtx.Name(),
		r.rCtx.Agent().Name(),
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

	return types.NewCard().
		Title(title, "green").
		Markdown(body).
		Buttons(r.cardBackButton()).
		Build()
}

// 展示当前session Name agentID 历史对话
func (r *Renderer) RenderCurrentCard(sessionKey string) *types.Card {
	s := r.rCtx.Sessions().GetOrCreateActive(sessionKey)
	agentID := s.GetAgentSessionID()
	if agentID == "" {
		agentID = r.I18nT(i18n.MsgSessionNotStarted)
	}
	content := fmt.Sprintf(r.I18nT(i18n.MsgCurrentSession), s.Name, agentID, len(s.History))
	return types.NewCard().
		Title(r.I18nT(i18n.MsgCardTitleCurrentSession), "turquoise").
		Markdown(content).
		Buttons(r.cardBackButton()).
		Build()
}

// 渲染历史对话消息
func (r *Renderer) RenderHistoryCard(sessionKey string) *types.Card {
	s := r.rCtx.Sessions().GetOrCreateActive(sessionKey)
	entries := s.GetHistory(10)

	// OpenCode Agent do not support History provider

	if len(entries) == 0 {
		return r.simpleCard(r.I18nT(i18n.MsgCardTitleHistory), "turquoise", r.I18nT(i18n.MsgHistoryEmpty))
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

	return types.NewCard().
		Title(r.I18nTf(i18n.MsgCardTitleHistoryLast, len(entries)), "turquoise").
		Markdown(sb.String()).
		Buttons(r.cardBackButton()).
		Build()
}

// 渲染定时任务卡片
func (r *Renderer) RenderCronCard(sessionKey string, userID string) *types.Card {
	// 判CronScheduler空
	if r.rCtx.CronScheduler() == nil {
		return r.simpleCard(r.I18nT(i18n.MsgCardTitleCron), "orange", r.I18nT(i18n.MsgCronNotAvailable))
	}
	// 判断CronStore中的job是否为空
	jobs := r.rCtx.CronScheduler().Store().ListBySessionKey(sessionKey)
	if len(jobs) == 0 {
		return r.simpleCard(r.I18nT(i18n.MsgCardTitleCron), "orange", r.I18nT(i18n.MsgCronEmpty))
	}

	lang := r.rCtx.I18n().CurrentLang()
	now := time.Now()
	// 初始化卡片
	cb := types.NewCard().Title(r.I18nT(i18n.MsgCardTitleCron), "orange")
	cb.Markdown(fmt.Sprintf(r.I18nT(i18n.MsgCronListTitle), len(jobs)))

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
				desc = "🖥 " + types.TruncateStr(j.Exec, 60)
			} else {
				desc = types.TruncateStr(j.Prompt, 60)
			}
		}
		// Mute 加后缀
		if j.Mute {
			desc += " [mute]"
		}
		// 将 min, hour, dom, month, dow 五个字段的表示转化为人类可读形式
		human := cron.CronExprToHuman(j.CronExpr, lang)

		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("%s %s\n", status, desc))
		sb.WriteString(r.I18nTf(i18n.MsgCronIDLabel, j.ID))
		sb.WriteString(r.I18nTf(i18n.MsgCronScheduleLabel, human, j.CronExpr))
		nextRun := r.rCtx.CronScheduler().NextRun(j.ID)
		if !nextRun.IsZero() {
			fmtStr := cron.CronTimeFormat(nextRun, now)
			sb.WriteString(r.I18nTf(i18n.MsgCronNextRunLabel, nextRun.Format(fmtStr)))
		}
		if !j.LastRun.IsZero() {
			fmtStr := cron.CronTimeFormat(j.LastRun, now)
			sb.WriteString(r.I18nTf(i18n.MsgCronLastRunLabel, j.LastRun.Format(fmtStr)))
			if j.LastError != "" {
				sb.WriteString(r.I18nTf(i18n.MsgCronFailedSuffix, types.TruncateStr(j.LastError, 40)))
			}
			sb.WriteString("\n")
		}
		cb.Markdown(sb.String())

		var btns []types.CardButton
		if j.Enabled {
			btns = append(btns, types.DefaultBtn(r.I18nT(i18n.MsgCronBtnDisable), fmt.Sprintf("act:/cron disable %s", j.ID)))
		} else {
			btns = append(btns, types.PrimaryBtn(r.I18nT(i18n.MsgCronBtnEnable), fmt.Sprintf("act:/cron enable %s", j.ID)))
		}
		if j.Mute {
			btns = append(btns, types.DefaultBtn(r.I18nT(i18n.MsgCronBtnUnmute), fmt.Sprintf("act:/cron unmute %s", j.ID)))
		} else {
			btns = append(btns, types.DefaultBtn(r.I18nT(i18n.MsgCronBtnMute), fmt.Sprintf("act:/cron mute %s", j.ID)))
		}
		btns = append(btns, types.DangerBtn(r.I18nT(i18n.MsgCronBtnDelete), fmt.Sprintf("act:/cron delete %s", j.ID)))
		cb.ButtonsEqual(btns...)
	}

	cb.Divider()
	cb.Note(r.I18nT(i18n.MsgCronCardHint))
	cb.Buttons(r.cardBackButton())
	return cb.Build()
}

// 渲染命令卡片
func (r *Renderer) RenderCommandsCard() *types.Card {
	cmds := r.rCtx.CommandRegistry().ListAll()
	if len(cmds) == 0 {
		return r.simpleCard(r.I18nT(i18n.MsgCardTitleCommands), "purple", r.I18nT(i18n.MsgCommandsEmpty))
	}

	var sb strings.Builder
	sb.WriteString(r.I18nTf(i18n.MsgCommandsTitle, len(cmds)))
	for _, c := range cmds {
		tag := ""
		if c.Source == "agent" {
			tag = r.I18nT(i18n.MsgCommandsTagAgent)
		} else if c.Exec != "" {
			tag = r.I18nT(i18n.MsgCommandsTagShell)
		}
		desc := c.Description
		if desc == "" {
			if c.Exec != "" {
				desc = "$ " + types.TruncateStr(c.Exec, 60)
			} else {
				desc = types.TruncateStr(c.Prompt, 60)
			}
		}
		sb.WriteString(fmt.Sprintf("/%s%s — %s\n", c.Name, tag, desc))
	}

	return types.NewCard().Title(r.I18nT(i18n.MsgCardTitleCommands), "purple").
		Markdown(sb.String()).
		Note(r.I18nT(i18n.MsgCommandsHint)).
		Buttons(r.cardBackButton()).
		Build()
}

// 渲染别名卡片(帮助 -> /help)
func (r *Renderer) RenderAliasCard() *types.Card {
	aliases := r.rCtx.GetAliases()

	if len(aliases) == 0 {
		return r.simpleCard(r.I18nT(i18n.MsgCardTitleAlias), "purple", r.I18nT(i18n.MsgAliasEmpty))
	}

	names := make([]string, 0, len(aliases))
	for n := range aliases {
		names = append(names, n)
	}
	sort.Strings(names)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(r.I18nT(i18n.MsgAliasListHeader), len(aliases)))
	sb.WriteString("\n")
	for _, n := range names {
		sb.WriteString(fmt.Sprintf("`%s` → `%s`\n", n, aliases[n]))
	}

	return types.NewCard().Title(r.I18nT(i18n.MsgCardTitleAlias), "purple").
		Markdown(sb.String()).
		Buttons(r.cardBackButton()).
		Build()
}

// 渲染配置卡片
func (r *Renderer) RenderConfigCard() *types.Card {
	items := r.rCtx.GetConfigItems()
	isZh := r.rCtx.I18n().IsZhLike()

	var sb strings.Builder
	sb.WriteString(r.I18nT(i18n.MsgConfigTitle))
	for _, item := range items {
		sb.WriteString(fmt.Sprintf("`%s` = `%s`\n  %s\n\n", item.Key, item.Value, item.Description(isZh)))
	}

	return types.NewCard().Title(r.I18nT(i18n.MsgCardTitleConfig), "grey").
		Markdown(sb.String()).
		Note(r.I18nT(i18n.MsgConfigHint)).
		Buttons(r.cardBackButton()).
		Build()
}

// 渲染doctor 卡片
func (r *Renderer) RenderDoctorCard(results []doctor.DoctorCheckResult) *types.Card {
	report := doctor.FormatDoctorResults(results, r.rCtx.I18n())
	return types.NewCard().
		Title(r.I18nT(i18n.MsgCardTitleDoctor), "orange").
		Markdown(report).
		Buttons(r.cardBackButton()).
		Build()
}

// 渲染用户信息卡片
func (r *Renderer) RenderWhoamiCard(msg *types.Message) *types.Card {
	userID := msg.UserID
	if userID == "" {
		userID = "(unknown)"
	}

	var body strings.Builder
	body.WriteString(fmt.Sprintf("**User ID:**  `%s`\n", userID))
	if msg.UserName != "" {
		body.WriteString(fmt.Sprintf("**%s:**  %s\n", r.I18nT(i18n.MsgWhoamiName), msg.UserName))
	}
	if msg.Platform != "" {
		body.WriteString(fmt.Sprintf("**%s:**  %s\n", r.I18nT(i18n.MsgWhoamiPlatform), msg.Platform))
	}
	chatID := types.ChannelID(msg.SessionKey)
	if chatID != "" {
		body.WriteString(fmt.Sprintf("**Chat ID:**  `%s`\n", chatID))
	}
	body.WriteString(fmt.Sprintf("**Session Key:**  `%s`\n", msg.SessionKey))

	return types.NewCard().
		Title(r.I18nT(i18n.MsgWhoamiCardTitle), "blue").
		Markdown(body.String()).
		Divider().
		Note(r.I18nT(i18n.MsgWhoamiUsage)).
		Buttons(r.cardBackButton()).
		Build()
}

// 渲染版本卡片
func (r *Renderer) RenderVersionCard() *types.Card {
	return types.NewCard().
		Title(r.I18nT(i18n.MsgCardTitleVersion), "grey").
		Markdown(types.VersionInfo).
		Buttons(r.cardBackButton()).
		Build()
}

// 渲染删除Mode卡片
func (r *Renderer) RenderDeleteModeCard(sessionKey string) *types.Card {
	aSessions, err := r.rCtx.Agent().ListSessions()
	if err != nil {
		return r.simpleCard(r.I18nT(i18n.MsgDeleteModeTitle), "red", err.Error())
	}
	aSessions = types.FilterOwnedSessions(aSessions, r.rCtx.Sessions().KnownAgentSessionIDs())
	dm := r.rCtx.StatManager().GetDeleteModeState(sessionKey)
	if dm == nil {
		return r.simpleCard(r.I18nT(i18n.MsgDeleteModeTitle), "red", r.I18nT(i18n.MsgDeleteUsage))
	}
	switch dm.Phase {
	case "confirm":
		return r.renderDeleteModeConfirmCard(r.rCtx.Sessions(), dm, aSessions)
	case "result":
		return r.renderDeleteModeResultCard(dm)
	default:
		return r.renderDeleteModeSelectCard(sessionKey, r.rCtx.Sessions(), dm, aSessions)
	}
}

// 提交删除
func (r *Renderer) renderDeleteModeConfirmCard(sessions *session.SessionManager, dm *mode.DeleteModeState, aSessions []types.AgentSessionInfo) *types.Card {
	selectedNames := mode.DeleteModeSelectionNames(sessions, dm, aSessions)
	body := strings.Join(selectedNames, "\n")
	if body == "" {
		body = r.I18nT(i18n.MsgDeleteModeEmptySelection)
	}
	return types.NewCard().
		Title(r.I18nT(i18n.MsgDeleteModeConfirmTitle), "carmine").
		Markdown(body).
		Buttons(
			types.DangerBtn(r.I18nT(i18n.MsgDeleteModeConfirmButton), "act:/delete-mode submit"),
			types.DefaultBtn(r.I18nT(i18n.MsgDeleteModeBackButton), "act:/delete-mode back"),
		).
		Build()
}

// 展示删除结果
func (r *Renderer) renderDeleteModeResultCard(dm *mode.DeleteModeState) *types.Card {
	return types.NewCard().
		Title(r.I18nT(i18n.MsgDeleteModeResultTitle), "turquoise").
		Markdown(dm.Result).
		Buttons(types.DefaultBtn(r.I18nT(i18n.MsgCardBack), "nav:/list 1")).
		Build()
}

// 默认展示delte 卡片
func (r *Renderer) renderDeleteModeSelectCard(sKey string, sessions *session.SessionManager, dm *mode.DeleteModeState, aSessions []types.AgentSessionInfo) *types.Card {
	if len(aSessions) == 0 {
		return r.simpleCard(r.I18nT(i18n.MsgDeleteModeTitle), "red", r.I18nT(i18n.MsgListEmpty))
	}
	total := len(aSessions)
	totalPages := (total + listPageSize - 1) / listPageSize
	page := dm.Page
	page = max(page, 1)
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * listPageSize
	end := start + listPageSize
	end = min(end, total)

	cb := types.NewCard().Title(r.I18nT(i18n.MsgDeleteModeTitle), "carmine")
	activeAgentID := sessions.GetOrCreateActive(sKey).GetAgentSessionID()
	selectedCount := 0
	for i := start; i < end; i++ {
		s := aSessions[i]
		isActive := activeAgentID == s.ID
		isSelected := false
		if !isActive {
			_, isSelected = dm.SelectedIDs[s.ID]
		}
		marker := "◻"
		if isActive {
			marker = "▶"
		} else if isSelected {
			marker = "☑"
			selectedCount++
		}
		btnText := r.I18nT(i18n.MsgDeleteModeSelect)
		btnType := "default"
		action := fmt.Sprintf("act:/delete-mode toggle %s", s.ID)
		if isActive {
			btnText = r.I18nT(i18n.MsgCardTitleCurrentSession)
			btnType = "primary"
			action = fmt.Sprintf("act:/delete-mode noop %s", s.ID)
		} else if isSelected {
			btnText = r.I18nT(i18n.MsgDeleteModeSelected)
			btnType = "primary"
		}
		cb.ListItemBtn(
			r.I18nTf(i18n.MsgListItem, marker, i+1, mode.DeleteSessionDisplayName(sessions, &s), s.MessageCount, s.ModifiedAt.Format("01-02 15:04")),
			btnText,
			btnType,
			action,
		)
	}
	cb.TaggedNote("delete-mode-selected-count", r.I18nTf(i18n.MsgDeleteModeSelectedCount, selectedCount))
	if dm.Hint != "" {
		cb.Note(dm.Hint)
	}
	cb.Buttons(
		types.DangerBtn(r.I18nT(i18n.MsgDeleteModeDeleteSelected), "act:/delete-mode confirm"),
		types.DefaultBtn(r.I18nT(i18n.MsgDeleteModeCancel), "act:/delete-mode cancel"),
	)

	var navBtns []types.CardButton
	if page > 1 {
		navBtns = append(navBtns, types.DefaultBtn(r.I18nT(i18n.MsgCardPrev), fmt.Sprintf("act:/delete-mode page %d", page-1)))
	}
	if page < totalPages {
		navBtns = append(navBtns, types.DefaultBtn(r.I18nT(i18n.MsgCardNext), fmt.Sprintf("act:/delete-mode page %d", page+1)))
	}
	if len(navBtns) > 0 {
		cb.Buttons(navBtns...)
	}
	return cb.Build()
}

func (r *Renderer) RenderCardForPlatform(p types.Platform, card *types.Card) *types.Card {
	return r.renderCardForPlatformWorkspace(p, card, "")
}

func (r *Renderer) renderCardForPlatformWorkspace(p types.Platform, card *types.Card, workspaceDir string) *types.Card {
	if card == nil {
		return nil
	}
	out := &types.Card{}
	if card.Header != nil {
		h := *card.Header
		out.Header = &h
	}
	out.Elements = make([]types.CardElement, 0, len(card.Elements))
	for _, elem := range card.Elements {
		switch v := elem.(type) {
		case types.CardMarkdown:
			content := v.Content
			if workspaceDir != "" {
				content = r.RenderOutgoingContentForWorkspace(p, v.Content, workspaceDir)
			}
			out.Elements = append(out.Elements, types.CardMarkdown{Content: content})
		case types.CardNote:
			text := v.Text
			if workspaceDir != "" {
				text = r.RenderOutgoingContentForWorkspace(p, v.Text, workspaceDir)
			}
			out.Elements = append(out.Elements, types.CardNote{Text: text, Tag: v.Tag})
		case types.CardListItem:
			text := v.Text
			if workspaceDir != "" {
				text = r.RenderOutgoingContentForWorkspace(p, v.Text, workspaceDir)
			}
			out.Elements = append(out.Elements, types.CardListItem{
				Text:     text,
				BtnText:  v.BtnText,
				BtnType:  v.BtnType,
				BtnValue: v.BtnValue,
				Extra:    v.Extra,
			})
		default:
			out.Elements = append(out.Elements, elem)
		}
	}
	return out
}

func (r *Renderer) RenderOutgoingContentForWorkspace(p types.Platform, content, workspaceDir string) string {
	if strings.TrimSpace(content) == "" {
		return content
	}
	return TransformLocalReferences(content, r.references, r.rCtx.Agent().Name(), p.Name(), workspaceDir)
}

func (r *Renderer) RenderModelCard(models []types.ModelOption, current string) *types.Card {

	var sb strings.Builder
	if current == "" {
		sb.WriteString(r.I18nT(i18n.MsgModelDefault))
	} else {
		sb.WriteString(r.I18nTf(i18n.MsgModelCurrent, current))
	}

	var opts []types.CardSelectOption
	initVal := ""
	for i, m := range models {
		label := m.Name
		if m.Alias != "" {
			label = m.Alias + " - " + m.Name
		} else if m.Desc != "" {
			label += " — " + m.Desc
		}
		val := fmt.Sprintf("act:/model switch %d", i+1)
		opts = append(opts, types.CardSelectOption{Text: label, Value: val})
		if m.Name == current {
			initVal = val
		}
	}

	cb := types.NewCard().Title(r.I18nT(i18n.MsgCardTitleModel), "indigo").
		Markdown(sb.String()).
		Select(r.I18nT(i18n.MsgModelSelectPlaceholder), opts, initVal).
		Buttons(r.cardBackButton())
	cb.Note(r.I18nT(i18n.MsgModelUsage))
	return cb.Build()
}

func (r *Renderer) RenderSkillsCard() *types.Card {
	skills := r.rCtx.SkillRegistry().ListAll()
	if len(skills) == 0 {
		return r.simpleCard(r.I18nT(i18n.MsgCardTitleSkills), "purple", r.I18nT(i18n.MsgSkillsEmpty))
	}

	var sb strings.Builder
	sb.WriteString(r.I18nTf(i18n.MsgSkillsTitle, r.rCtx.Agent().Name(), len(skills)))
	for _, s := range skills {
		sb.WriteString(fmt.Sprintf("  /%s — %s\n", s.Name, s.Description))
	}

	return types.NewCard().Title(r.I18nT(i18n.MsgCardTitleSkills), "purple").
		Markdown(sb.String()).
		Note(r.I18nT(i18n.MsgSkillsHint)).
		Buttons(r.cardBackButton()).
		Build()
}

// 使用error card 包装一个renderListCard
func (r *Renderer) RenderDirCardSafe(sessionKey string, page int) *types.Card {
	card, err := r.renderDirCard(sessionKey, page)
	if err != nil {
		return r.simpleCard(r.I18nT(i18n.MsgDirCardTitle), "red", err.Error())
	}
	return card
}

func (r *Renderer) renderDirCard(sessionKey string, page int) (*types.Card, error) {
	return nil, nil
}
