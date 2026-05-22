package core

import "sync"
import "strings"
import "os"
import "path/filepath"

// 表示一个注册的 /命令(来自配置文件或agent cmd 文件)
type CustomCommand struct {
	Name        string // 不带 "/" 的命令名称
	Description string
	Prompt      string // 带placeholders的模板  {{1}}, {{2}}, {{2*}}, {{args}}
	Exec        string // 要执行的shell命令 (与Prompt互斥)
	Source      string // "config" or "agent" (for display)
}

// hold 所有可用自定义commands 并解析agent 命令文件
type CommandRegistry struct {
	mu        sync.RWMutex
	commands  map[string]*CustomCommand // 从config.toml或运行时添加
	agentDirs []string                  // 扫描*.md命令文件的目录
}

// 返回所有注册的命令
func (r *CommandRegistry) ListAll() []*CustomCommand {
	r.mu.RLock()
	defer r.mu.RUnlock()

	seen := make(map[string]bool)
	var result []*CustomCommand

	for _, c := range r.commands {
		result = append(result, c)
		seen[strings.ToLower(c.Name)] = true
	}

	for _, dir := range r.agentDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			name := strings.TrimSuffix(entry.Name(), ".md")
			if seen[strings.ToLower(name)] {
				continue
			}
			seen[strings.ToLower(name)] = true

			desc := ""
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err == nil {
				first, _, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
				if len([]rune(first)) > 60 {
					first = string([]rune(first)[:60]) + "..."
				}
				desc = first
			}

			result = append(result, &CustomCommand{
				Name:        name,
				Description: desc,
				Source:      "agent",
			})
		}
	}

	return result
}
