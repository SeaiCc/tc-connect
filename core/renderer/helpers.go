package renderer

import (
	"strings"
	"tc-connect/core/i18n"
	"tc-connect/core/types"
)

type helpCardItem struct {
	command string
	action  string
}

type helpCardGroup struct {
	key      string
	titleKey i18n.MsgKey
	items    []helpCardItem
}

// 返回session|agent|tools|system 组的相关命令
func helpCardGroups() []helpCardGroup {
	return []helpCardGroup{
		{
			key:      "session",
			titleKey: i18n.MsgHelpSessionSection,
			items: []helpCardItem{
				{command: "/new", action: "act:/new"},
				{command: "/list", action: "nav:/list"},
				{command: "/current", action: "nav:/current"},
				{command: "/switch", action: "nav:/list"},
				{command: "/search", action: "cmd:/search"},
				{command: "/history", action: "nav:/history"},
				{command: "/delete", action: "cmd:/delete"},
				{command: "/name", action: "cmd:/name"},
			},
		},
		{
			key:      "agent",
			titleKey: i18n.MsgHelpAgentSection,
			items: []helpCardItem{
				{command: "/model", action: "nav:/model"},
				{command: "/reasoning", action: "nav:/reasoning"},
				{command: "/mode", action: "nav:/mode"},
				{command: "/lang", action: "nav:/lang"},
				{command: "/provider", action: "nav:/provider"},
				{command: "/memory", action: "cmd:/memory"},
				{command: "/allow", action: "cmd:/allow"},
				{command: "/quiet", action: "cmd:/quiet"},
			},
		},
		{
			key:      "tools",
			titleKey: i18n.MsgHelpToolsSection,
			items: []helpCardItem{
				{command: "/shell", action: "cmd:/shell"},
				{command: "/show", action: "cmd:/show"},
				{command: "/cron", action: "nav:/cron"},
				{command: "/heartbeat", action: "nav:/heartbeat"},
				{command: "/commands", action: "nav:/commands"},
				{command: "/alias", action: "nav:/alias"},
				{command: "/skills", action: "nav:/skills"},
				{command: "/compress", action: "cmd:/compress"},
				{command: "/stop", action: "act:/stop"},
			},
		},
		{
			key:      "system",
			titleKey: i18n.MsgHelpSystemSection,
			items: []helpCardItem{
				{command: "/status", action: "nav:/status"},
				{command: "/doctor", action: "nav:/doctor"},
				{command: "/usage", action: "cmd:/usage"},
				{command: "/config", action: "nav:/config"},
				{command: "/bind", action: "cmd:/bind"},
				{command: "/workspace", action: "cmd:/workspace"},
				{command: "/dir", action: "nav:/dir"},
				{command: "/version", action: "nav:/version"},
				{command: "/upgrade", action: "nav:/upgrade"},
				{command: "/restart", action: "cmd:/restart"},
			},
		},
	}
}

// 分离帮助Tab行
func splitHelpTabRows(useMultiRow bool, tabs []types.CardButton) [][]types.CardButton {
	if useMultiRow {
		rows := make([][]types.CardButton, 0, (len(tabs)+1)/2)
		for i := 0; i < len(tabs); i += 2 {
			end := i + 2
			end = min(end, len(tabs))
			rows = append(rows, tabs[i:end])
		}
		return rows
	}
	return [][]types.CardButton{tabs}
}

// 以 \n\n 切分 title 和其他部分
func splitCardTitleBody(content string) (string, string) {
	content = strings.TrimSpace(content)
	parts := strings.SplitN(content, "\n\n", 2)
	title := strings.TrimSpace(parts[0])
	if len(parts) == 1 {
		return title, ""
	}
	return title, strings.TrimSpace(parts[1])
}
