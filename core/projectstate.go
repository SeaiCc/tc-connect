package core

import (
	"encoding/json"
	"log/slog"
	"os"
	"sync"
)

// 记录工作区修改状态
type projectStateData struct {
	WorkDirOverride      string            `json:"work_dir_override,omitempty"`
	WorksapceDirOverides map[string]string `json:"workspace_dir_overrides,omitempty"`
}

// 为一个项目保持轻量级运行状态
type ProjectStateStore struct {
	mu        sync.RWMutex
	storePath string
	state     projectStateData
}

func NewProjectStateStore(path string) *ProjectStateStore {
	ps := &ProjectStateStore{storePath: path}
	if path != "" {
		ps.load()
	}
	return ps
}

// 线程安全地读取WorkDirOverride（修改后的工作路径）值
func (ps *ProjectStateStore) WorkDirOverride() string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.state.WorkDirOverride
}

func (ps *ProjectStateStore) load() {
	data, err := os.ReadFile(ps.storePath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Error("project_state: failed to read", "path", ps.storePath, "error", err)
		}
		return
	}

	var state projectStateData
	if err := json.Unmarshal(data, &state); err != nil {
		slog.Error("project_state: failed to unmarshal", "path", ps.storePath, "error", err)
		return
	}
	ps.state = state
}
