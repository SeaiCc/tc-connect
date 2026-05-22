package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"tc-connect/core"
	"time"
)

func init() {
	core.RegisterAgent("opencode", New)
}

// 后台运行OpenCode CLI `opencode run --format json`
type Agent struct {
	workDir    string
	model      string
	mode       string
	cmd        string
	activeIdx  int
	sessionEnv []string
	mu         sync.RWMutex
}

func New(opts map[string]any) (core.Agent, error) {
	workDir, _ := opts["wordir"].(string)
	if workDir == "" {
		workDir = "."
	}
	model, _ := opts["model"].(string)
	cmd := "opencode"

	return &Agent{
		workDir:   workDir,
		model:     model,
		cmd:       cmd,
		activeIdx: -1,
	}, nil
}

func (a *Agent) Name() string { return "opencode" }

// 运行`opencode session list` 解析json输出
func (a *Agent) ListSessions(_ context.Context) ([]core.AgentSessionInfo, error) {
	return listOpencodeSessions(a.cmd, a.workDir)
}

// 通过`opencode session delete <id>` 删除session  
func (a *Agent) DeleteSession(_ context.Context, sessionID string) error {
	a.mu.RLock()
	cmd := a.cmd
	workDir := a.workDir
	a.mu.RUnlock()

	c := exec.Command(cmd, "session", "delete", sessionID)
	c.Dir = workDir
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("opencode: delete session %s: %w: %s", sessionID, err, strings.TrimSpace(string(out)))
	}
	return nil
}


func (a *Agent) SetSessionEnv(env []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessionEnv = env
}

func (a *Agent) Stop() error { return nil }

func (a *Agent) CompressCommand() string { return "/compact" }

// ========================== 公共方法:: Mode切换 ==========================

func (a *Agent) SetMode(mode string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mode = normalizeMode(mode)
	slog.Info("opencode: mode changed", "mode", a.mode)
}

func (a *Agent) GetMode() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mode
}

func (a *Agent) PermissionModes() []core.PermissionModeInfo {
	return []core.PermissionModeInfo{
		{Key: "default", Name: "Default", NameZh: "默认", Desc: "Standard mode", DescZh: "标准模式"},
		{Key: "yolo", Name: "YOLO", NameZh: "全自动", Desc: "Auto-approve all tool calls", DescZh: "自动批准所有工具调用"},
	}
}

// ========================== 公共方法:: Session 相关 ==========================

// 创建或回复一个交互session
func (a *Agent) StartSession(ctx context.Context, sessionID string) (core.AgentSession, error) {
	a.mu.Lock()
	model := a.model
	cmd := a.cmd
	workDir := a.workDir
	return newOpencodeSession(ctx, cmd, workDir, model, sessionID)
}

// `opencode session list`命令输出中的一个session.
type opencodeSessionEntry struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Updated int64  `json:"updated"` // Unix timestamp in milliseconds
	Created int64  `json:"created"`
}

// ========================== 辅助方法 ==========================

// 运行`opencode session list` 解析json输出
func listOpencodeSessions(cmd, workDir string) ([]core.AgentSessionInfo, error) {
	// 执行命令
	c := exec.Command(cmd, "session", "list", "--format", "json")
	c.Dir = workDir
	// 获取输出
	out, err := c.Output()
	if err != nil {
		return nil, fmt.Errorf("opencode: session list: %w", err)
	}
	//
	var entries []opencodeSessionEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, fmt.Errorf("opencode: parse session list: %w", err)
	}

	msgCounts := querySessionMessageCounts()

	var sessions []core.AgentSessionInfo
	for _, e := range entries {
		sessions = append(sessions, core.AgentSessionInfo{
			ID:           e.ID,
			Summary:      e.Title,
			MessageCount: msgCounts[e.ID],
			ModifiedAt:   time.UnixMilli(e.Updated),
		})
	}

	return sessions, nil
}

// 使用sqlite3 CLI从OpenCode 本地数据库中读取message数量，失败返回空map
func querySessionMessageCounts() map[string]int {
	// 获取opencode本地数据库路径
	dbPath := opencodeDBPath()
	if dbPath == "" {
		return nil
	}
	if _, err := os.Stat(dbPath); err != nil {
		return nil
	}
	sqlite3, err := exec.LookPath("sqlite3")
	if err != nil {
		slog.Warn("opencode: sqlite3 CLI not found, message counts unavailable", "err", err)
		return nil
	}
	//  执行SQL命令
	out, err := exec.Command(sqlite3, dbPath,
		"SELECT session_id, COUNT(*) FROM message GROUP BY session_id").Output()
	if err != nil {
		slog.Warn("opencode: sqlite3 query failed", "db_path", dbPath, "err", err)
		return nil
	}
	// out的每一行 用 "|" 分割 左侧为sessionID 右侧为 message数量
	counts := make(map[string]int)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, "|", 2)
		if len(parts) != 2 {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(parts[1], "%d", &n); err == nil {
			counts[parts[0]] = n
		}
	}
	return counts
}

// 获取数据库路径
func opencodeDBPath() string {
	// 环境变量中找数据库
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "opencode", "opencode.db")
	}
	// 从家目录获取
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "opencode", "opencode.db")
}

// opencode mode切换
func normalizeMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "yolo", "auto", "force", "bypasspermissions":
		return "yolo"
	default:
		return "default"
	}
}
