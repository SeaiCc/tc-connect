package core

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

const (
	defaultDirHistorySize = 10
	dirHistoryFileName    = "dir_history.json"
)

// 管理目录切换历史
type DirHistory struct {
	mu        sync.RWMutex
	storePath string
	entries   map[string][]string // project name -> dir list
	maxSize   int
}

// 使用给定的data directory 创建DirHistory
func NewDirHistory(dataDir string) *DirHistory {
	dh := &DirHistory{
		storePath: filepath.Join(dataDir, dirHistoryFileName),
		entries:   make(map[string][]string),
		maxSize:   defaultDirHistorySize,
	}
	dh.load()
	return dh
}

func (dh *DirHistory) load() {
	if dh.storePath == "" {
		return
	}

	data, err := os.ReadFile(dh.storePath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Error("dir_history: failed to read", "path", dh.storePath, "error", err)
		}
		return
	}

	var entries map[string][]string
	if err := json.Unmarshal(data, &entries); err != nil {
		slog.Error("dir_history: failed to unmarshal", "path", dh.storePath, "error", err)
		return
	}

	if entries != nil {
		dh.entries = entries
	}
}
