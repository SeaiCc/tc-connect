package core

import (
	"sync"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"log/slog"
)

// 从SKILL.md文件中获得的skill
type Skill struct {
	Name        string // skill 名称 (= 子目录name)
	DisplayName string // 从frontmatter中获得的展示名称
	Description string // 从frontmatter 中获得或 内容第一行
	Prompt      string // the instruction 内容 ( frontmatter 之后的body)
	Source      string // skill 发现的目录
}

// 从skill 目录中发现和cache agent Skill 
// skill 为项目级别: 每个Engine 有自己的SkillRegistry 
type SkillRegistry struct {
	mu   sync.RWMutex
	dirs []string
	// cached results; nil means not yet scanned
	cache []*Skill
}

func NewSkillRegistry() *SkillRegistry {
	return &SkillRegistry{}
}

func (r *SkillRegistry) Resolve(name string) *Skill {
	norm := normalizeCommandName(name)
	for _, s := range r.ListAll() {
		if normalizeCommandName(s.Name) == norm {
			return s
		}
	}
	return nil
}

// 返回所有skill, 第一次scan后会扫描
func (r *SkillRegistry) ListAll() []*Skill {
	r.mu.RLock()
	if r.cache != nil {
		defer r.mu.RUnlock()
		return r.cache
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()

	// double-check after acquiring write lock
	if r.cache != nil {
		return r.cache
	}

	var result []*Skill
	seen := make(map[string]bool)

	for _, dir := range r.dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			fullPath := filepath.Join(dir, entry.Name())
			info, err := os.Stat(fullPath)
			if err != nil {
				continue
			}
			if !info.IsDir() {
				continue
			}
			skillName := entry.Name()
			if seen[strings.ToLower(skillName)] {
				continue
			}

			mdPath := filepath.Join(dir, skillName, "SKILL.md")
			data, err := os.ReadFile(mdPath)
			if err != nil {
				continue
			}

			skill := parseSkillMD(skillName, string(data), dir)
			if skill == nil {
				continue
			}

			seen[strings.ToLower(skillName)] = true
			result = append(result, skill)
			slog.Debug("skill: discovered", "name", skillName, "dir", dir)
		}
	}

	r.cache = result
	return result
}

// ====================== 辅助方法 ============================

// 解析一个SKILL.md 文件, 可选YAML frontmatter
//
// Format:
//
//	---
//	description: 简要描述
//	name: 展示名称
//	---
//	Prompt/instruction content here...
func parseSkillMD(skillName, raw, sourceDir string) *Skill {
	content := strings.TrimSpace(raw)
	if content == "" {
		return nil
	}

	var frontmatter map[string]string
	body := content

	if strings.HasPrefix(content, "---") {
		rest := content[3:]
		endIdx := strings.Index(rest, "\n---")
		if endIdx >= 0 {
			fmBlock := rest[:endIdx]
			body = strings.TrimSpace(rest[endIdx+4:])
			frontmatter = parseFrontmatter(fmBlock)
		}
	}

	if body == "" {
		return nil
	}

	description := ""
	displayName := ""
	if frontmatter != nil {
		description = frontmatter["description"]
		displayName = frontmatter["name"]
	}

	if description == "" {
		first, _, _ := strings.Cut(body, "\n")
		first = strings.TrimSpace(first)
		if len([]rune(first)) > 80 {
			first = string([]rune(first)[:80]) + "..."
		}
		description = first
	}

	return &Skill{
		Name:        skillName,
		DisplayName: displayName,
		Description: description,
		Prompt:      body,
		Source:      sourceDir,
	}
}

// 从YMAL-like block, 提取一个简单的key: value 对
// 处理逗号分隔的value, 以及YAML block scalar indicators (>-, |-, >, |)
// 将以下缩进行作为值读取
func parseFrontmatter(block string) map[string]string {
	m := make(map[string]string)
	lines := strings.Split(block, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)

		// Handle YAML block scalar indicators: >-, |-, >, |
		if val == ">-" || val == "|-" || val == ">" || val == "|" {
			var blockLines []string
			for i+1 < len(lines) {
				next := lines[i+1]
				// Block continues while lines are indented (start with space/tab)
				if len(next) == 0 || (next[0] != ' ' && next[0] != '\t') {
					break
				}
				i++
				blockLines = append(blockLines, strings.TrimSpace(next))
			}
			val = strings.Join(blockLines, " ")
		}

		val = strings.Trim(val, `"'`)
		if key != "" {
			m[strings.ToLower(key)] = val
		}
	}
	return m
}


// 用于执行skill时, 构建要发送到agent的message. 指示agent 执行skill, 而不是原始的提示扩展
func BuildSkillInvocationPrompt(skill *Skill, args []string) string {
	var sb strings.Builder

	sb.WriteString("The user is asking you to execute the following skill.\n\n")

	name := skill.DisplayName
	if name == "" {
		name = skill.Name
	}
	fmt.Fprintf(&sb, "## Skill: %s\n", name)

	if skill.Description != "" {
		fmt.Fprintf(&sb, "## Description: %s\n", skill.Description)
	}

	sb.WriteString("\n## Skill Instructions:\n")
	sb.WriteString(skill.Prompt)

	if len(args) > 0 {
		sb.WriteString("\n\n## User Arguments:\n")
		sb.WriteString(strings.Join(args, " "))
	}

	sb.WriteString("\n\nPlease follow the skill instructions above to complete the task.")
	return sb.String()
}
