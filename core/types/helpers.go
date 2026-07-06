// 无依赖包，存放通用类和辅助方法供其他层使用
package types

import (
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
)

// main启动时设置以便/version工作
var VersionInfo string

// RestartRequest carries info needed to send a post-restart notification.
type RestartRequest struct {
	SessionKey string `json:"session_key"`
	Platform   string `json:"platform"`
}

// 移除不被tc-connect追踪的agent sessions, 防止同目录下的外部CLI创建的session出现在
// /list /switch /delete 等命令中, 如果完全没有追踪(如首次运行) 直接返回
// AgentSession 与core/session 不同
func FilterOwnedSessions(sessions []AgentSessionInfo, known map[string]struct{}) []AgentSessionInfo {
	if len(known) == 0 {
		return sessions
	}
	filtered := make([]AgentSessionInfo, 0, len(sessions))
	for _, s := range sessions {
		if _, ok := known[s.ID]; ok {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

// 从sessionKey中提取 channelID
func ChannelID(sessionKey string) string {
	// Format: "platform:channelID:userID" or "platform:channelID"
	parts := strings.SplitN(sessionKey, ":", 3)
	if len(parts) >= 2 {
		return parts[1]
	}
	return ""
}

// 规范化大小写和下划线|连字符 视为等效
func NormalizeCommandName(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "-", "_"))
}

func SupportsCards(p Platform) bool {
	_, ok := p.(CardSender)
	return ok
}

// ============= Model 切换 =============

func ParseModelSwitchArgs(args []string) (string, bool) {
	if len(args) == 0 {
		return "", false
	}
	if len(args) == 1 {
		if strings.EqualFold(strings.TrimSpace(args[0]), "switch") {
			return "", false
		}
		return args[0], true
	}
	if strings.EqualFold(strings.TrimSpace(args[0]), "switch") && len(args) >= 2 {
		return strings.TrimSpace(args[1]), true
	}
	return "", false
}

func ModelSwitchNeedsLookup(input string) bool {
	input = strings.TrimSpace(input)
	if input == "" {
		return false
	}
	if _, err := strconv.Atoi(input); err == nil {
		return true
	}
	return !strings.Contains(input, "/")
}

// 解析用户提供的model 名称string, 先检查外部的别名匹配, 然后fall back 到原始的值
// (可能)
func resolveModelAlias(models []ModelOption, input string) string {
	for _, m := range models {
		if m.Alias != "" && strings.EqualFold(m.Alias, input) {
			return m.Name
		}
	}
	return input
}

func ResolveModelSwitchTarget(input string, models []ModelOption) string {
	input = strings.TrimSpace(input)
	if idx, err := strconv.Atoi(input); err == nil && idx >= 1 && idx <= len(models) {
		return models[idx-1].Name
	}
	if resolved := resolveModelAlias(models, input); resolved != input {
		return resolved
	}
	for _, m := range models {
		if strings.EqualFold(m.Name, input) {
			return m.Name
		}
	}
	return input
}

// 切换模式文本
func FormatModeKeys(modes []PermissionModeInfo) string {
	keys := make([]string, 0, len(modes))
	for _, mode := range modes {
		keys = append(keys, "`"+mode.Key+"`")
	}
	return strings.Join(keys, " / ")
}

// 参数列表省略string形式
// rune解析 s, 如果小于n直接返回,大于返回前n个+ ...
func TruncateStr(s string, n int) string {
	// rune : int32别名
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}

// 清理并解析workspace 路径, 防止因trailing slashes, symlinks, or relative segments
// 造成的误匹配, 如果路基无法被解析(如不存在), fall back 到 filepath.Clean
func NormalizeWorkspacePath(path string) string {
	cleaned := filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		return cleaned
	}
	if resolved != path {
		slog.Debug("workspace path normalized", "original", path, "normalized", resolved)
	}
	return resolved
}

// 忽略event中的任意buffed 事件,
// 在new turn开始前调用，以防止将上一回合代理进程中的过时事件误认为是新回合的响应。
func DrainEvents(ch <-chan Event) {
	drained := 0
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				// channel被关闭, 立即停止避免死循环
				return
			}
			drained++
		default:
			if drained > 0 {
				slog.Warn("drained stale events from previous turn", "count", drained)
			}
			return
		}
	}
}
