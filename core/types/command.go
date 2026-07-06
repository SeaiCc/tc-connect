package types

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// 表示一个注册的 /命令(来自配置文件或agent cmd 文件)
type CustomCommand struct {
	Name        string // 不带 "/" 的命令名称
	Description string
	Prompt      string // 带placeholders的模板  {{1}}, {{2}}, {{2*}}, {{args}}
	Exec        string // 要执行的shell命令 (与Prompt互斥)
	WorkDir     string // 可选: 命令的执行目录
	Source      string // "config" or "agent" (for display)
}

// hold 所有可用自定义commands 并解析agent 命令文件
type CommandRegistry struct {
	mu        sync.RWMutex
	commands  map[string]*CustomCommand // 从config.toml或运行时添加
	agentDirs []string                  // 扫描*.md命令文件的目录
}

func NewCommandRegistry() *CommandRegistry {
	return &CommandRegistry{
		commands: make(map[string]*CustomCommand),
	}
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

// 注册一个自定义命令
func (r *CommandRegistry) Add(name, description, prompt, exec, workDir, source string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands[strings.ToLower(name)] = &CustomCommand{
		Name:        name,
		Description: description,
		Prompt:      prompt,
		Exec:        exec,
		WorkDir:     workDir,
		Source:      source,
	}
}

// 根据命令名称删除自定义命令, 没找到返回false
func (r *CommandRegistry) Remove(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	lower := strings.ToLower(name)
	if _, ok := r.commands[lower]; ok {
		delete(r.commands, lower)
		return true
	}
	return false
}

// 通过名称查找command, 优先配置命令,然后扫描agent command 目录 查找 .md文件
// 连字符和下划线被视为等效, 因此经过Telegram处理的名称（例如“my_cmd”）与原始命令名称（“my-cmd”）相匹配
func (r *CommandRegistry) Resolve(name string) (*CustomCommand, bool) {
	lower := strings.ToLower(name)
	norm := NormalizeCommandName(name)

	r.mu.RLock()
	// Exact match first
	if c, ok := r.commands[lower]; ok {
		r.mu.RUnlock()
		return c, true
	}
	// Normalized match (hyphen ↔ underscore)
	for key, c := range r.commands {
		if NormalizeCommandName(key) == norm {
			r.mu.RUnlock()
			return c, true
		}
	}
	r.mu.RUnlock()

	// Scan agent command directories; try both original name and hyphenated variant
	candidates := []string{name}
	if alt := strings.ReplaceAll(name, "_", "-"); alt != name {
		candidates = append(candidates, alt)
	}
	for _, dir := range r.agentDirs {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		for _, candidate := range candidates {
			mdPath := filepath.Join(dir, candidate+".md")
			absPath, err := filepath.Abs(mdPath)
			if err != nil || !strings.HasPrefix(absPath, absDir+string(filepath.Separator)) {
				continue
			}
			data, err := os.ReadFile(mdPath)
			if err != nil {
				continue
			}
			content := strings.TrimSpace(string(data))
			if content == "" {
				continue
			}
			slog.Debug("command: loaded agent command file", "path", mdPath)
			return &CustomCommand{
				Name:   candidate,
				Prompt: content,
				Source: "agent",
			}, true
		}
	}

	return nil, false
}

// 从给定的srouce中移除所有commands
func (r *CommandRegistry) ClearSource(source string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, c := range r.commands {
		if c.Source == source {
			delete(r.commands, k)
		}
	}
}

// ========================== 辅助方法 ==========================

// 匹配 {{1}}, {{2*}}, {{args}}, 以及默认变量如 {{1:foo}}.
var placeholderRe = regexp.MustCompile(`\{\{(\d+\*?|args)(:[^}]*)?\}\}`)

// 使用提供的参数替换模板中的placeholders
//
// Supported placeholders:
//
//   - {{1}}, {{2}}, ...       — positional argument (1-based)
//
//   - {{1:default}}           — positional with default value if arg not provided
//
//   - {{2*}}                  — argument N and everything after it
//
//   - {{2*:default}}          — same, with default
//
//   - {{args}}                — all arguments joined by space
//
//   - {{args:default}}        — all arguments, with default if none provided
//
//     如果模板中没有placeholders, 参数添加至最后
func ExpandPrompt(template string, args []string) string {
	if !placeholderRe.MatchString(template) {
		if len(args) > 0 {
			return template + "\n\n" + strings.Join(args, " ")
		}
		return template
	}

	result := placeholderRe.ReplaceAllStringFunc(template, func(match string) string {
		inner := match[2 : len(match)-2] // strip {{ and }}
		key, defaultVal, hasDefault := strings.Cut(inner, ":")

		if key == "args" {
			if len(args) > 0 {
				return strings.Join(args, " ")
			}
			if hasDefault {
				return defaultVal
			}
			return ""
		}
		if strings.HasSuffix(key, "*") {
			idx := 0
			_, _ = fmt.Sscanf(key, "%d", &idx)
			if idx >= 1 && idx-1 < len(args) {
				return strings.Join(args[idx-1:], " ")
			}
			if hasDefault {
				return defaultVal
			}
			return ""
		}
		idx := 0
		_, _ = fmt.Sscanf(key, "%d", &idx)
		if idx >= 1 && idx-1 < len(args) {
			return args[idx-1]
		}
		if hasDefault {
			return defaultVal
		}
		return ""
	})

	return result
}
