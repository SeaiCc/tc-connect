package feishu

import (
	"encoding/hex"
	"sort"
	"strings"
)

const deleteModeCheckerNamePrefix = "delete_sel_"

// 生成唯一ID并加 delete_sel_ 前缀
func deleteModeCheckerName(sessionID string) string {
	return deleteModeCheckerNamePrefix + hex.EncodeToString([]byte(sessionID))
}

// 根据前缀找到要删除的内容
func parseDeleteModeCheckerName(name string) (string, bool) {
	if !strings.HasPrefix(name, deleteModeCheckerNamePrefix) {
		return "", false
	}
	raw := strings.TrimPrefix(name, deleteModeCheckerNamePrefix)
	if raw == "" {
		return "", false
	}
	b, err := hex.DecodeString(raw)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// 根据内容判断bool值
func isTruthyFormValue(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		s := strings.ToLower(strings.TrimSpace(x))
		return s == "true" || s == "1" || s == "yes" || s == "on"
	case float64:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	default:
		return false
	}
}

// 逐条判断formValue, 找到delete_sel_ 前缀且内容为含义为true的item到ids中
func collectDeleteModeSelectedFromFormValue(formValue map[string]any) []string {
	if len(formValue) == 0 {
		return nil
	}
	ids := make([]string, 0, len(formValue))
	seen := make(map[string]struct{}, len(formValue))
	for key, val := range formValue {
		sessionID, ok := parseDeleteModeCheckerName(key)
		if !ok || !isTruthyFormValue(val) {
			continue
		}
		if _, exists := seen[sessionID]; exists {
			continue
		}
		seen[sessionID] = struct{}{}
		ids = append(ids, sessionID)
	}
	sort.Strings(ids)
	return ids
}
