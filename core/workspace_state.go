package core

import (
	"path/filepath"
	"log/slog"
)

// 清理并解析workspace 路径, 防止因trailing slashes, symlinks, or relative segments
// 造成的误匹配, 如果路基无法被解析(如不存在), fall back 到 filepath.Clean 
func normalizeWorkspacePath(path string) string {
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
