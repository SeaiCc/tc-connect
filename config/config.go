package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

// configMu serializes read-modify-write cycles to prevent lost updates.
var configMu sync.Mutex

// 存储要保存的配置文件路径
var ConfigPath string

type Config struct {
	DataDir string `toml:"data_dir"` // session store directory, default .tc-connect

	Quiet    *bool           `toml:"quiet,omitempty"`
	Commands []CommandConfig `toml:"commands"` // 全局自定义 / 命令
	Project  ProjectConfig   `toml:"project"`
	Aliases           []AliasConfig           `toml:"aliases"`      // global command aliases

	Log      LogConfig       `toml:"log"`
	Language string          `toml:"language"`

	Display DisplayConfig `toml:"display"`

	Cron   CronConfig   `toml:"cron"`
	Bridge BridgeConfig `toml:"bridge"`
}

// 映射trigger string 到command 
type AliasConfig struct {
	Name    string `toml:"name"`    // trigger text (e.g. "帮助")
	Command string `toml:"command"` // target command (e.g. "/help")
}

// 定时任务配置
type CronConfig struct {
	Silent      *bool  `toml:"silent"`       // suppress cron start notification; default false
	SessionMode string `toml:"session_mode"` // default session mode: "" or "reuse" (default) or "new_per_run"
}

// 定义了用户自定义slash 命令,(扩展prompt模板或执行一个shell命令)
type CommandConfig struct {
	Name        string `toml:"name"`
	Description string `toml:"description"`
	Prompt      string `toml:"prompt"`   // prompt template (mutually exclusive with Exec)
	Exec        string `toml:"exec"`     // shell command to execute (mutually exclusive with Prompt)
	WorkDir     string `toml:"work_dir"` // optional: working directory for exec command
}

// 控制websocket桥接用于外部平台adapters
type BridgeConfig struct {
	Enabled     *bool    `toml:"enabled"`                // 默认false
	Port        int      `toml:"port,omitempty"`         // 监听端口， 默认9810
	Token       string   `toml:"token,omitempty"`        // 验证token 共享密钥
	Path        string   `toml:"path,omitempty"`         // URL路径 默认 "/bridge/ws"
	CORSOrigins []string `toml:"cors_origins,omitempty"` // 允许CORS 域， empty = 没有
}

// how intermediate messages (thinking, tool output) are shown
type DisplayConfig struct {
	ThinkingMessages *bool `toml:"thinking_messages"` // whether thinking messages are shown; default true
	ThinkingMaxLen   *int  `toml:"thinking_max_len"`  // max chars for thinking messages; 0 = no truncation; default 300
	ToolMaxLen       *int  `toml:"tool_max_len"`      // max chars for tool use messages; 0 = no truncation; default 500
	ToolMessages     *bool `toml:"tool_messages"`     // whether tool progress messages are shown; default true
}

// 绑定一个agent (带有特定的work_dir)
type ProjectConfig struct {
	Name         string             `toml:"name"`
	Agent        AgentConfig        `toml:"agent"`
	Platform     PlatformConfig     `toml:"platform"`
	AutoCompress AutoCompressConfig `toml:"auto_compress"`

	// 在当前会话处于非活动状态达到指定分钟数后，ResetOnIdleMins会自动切换到新的tc-connect会话。
	// 0 或 nil 表示禁用该行为。
	ResetOnIdleMins *int  `toml:"reset_on_idle_mins,omitempty"`
	InjectSender    *bool `toml:"inject_sender,omitempty"` // 发送给agent前 每条消息添加发送者身份

	AdminFrom string `toml:"admin_from,omitempty"` // 逗号分隔的特权用户; "*" = all allowed users
	Quiet     *bool  `toml:"quiet,omitempty"`
}

// project 的原子上下文压缩
type AutoCompressConfig struct {
	Enabled    *bool `toml:"enabled,omitempty"`      // default false
	MaxTokens  *int  `toml:"max_tokens,omitempty"`   // estimated token threshold to trigger /compress
	MinGapMins *int  `toml:"min_gap_mins,omitempty"` // minimum minutes between auto-compress runs (default 30)
}

type AgentConfig struct {
	Type    string         `toml:"type"`
	Options map[string]any `toml:"options"`
}

type PlatformConfig struct {
	Type    string         `toml:"type"`
	Options map[string]any `toml:"options"`
}

type LogConfig struct {
	Level string `toml:"level"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	cfg := &Config{
		Log: LogConfig{Level: "info"},
	}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	// 省去cfg的格式检查自己明白配置规则即可 validate
	return cfg, nil
}

// 将Config类保存到文件
func saveConfig(cfg *Config) error {
	dir := filepath.Dir(ConfigPath)
	// 创建临时文件
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpPath := tmp.Name()
	// Config对象 -> builder
	var buf strings.Builder
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("encode config: %w", err)
	}
	// builder -> toml格式 -> tmp文件写入
	formatted := formatTOML(buf.String())
	if _, err := tmp.WriteString(formatted); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write config: %w", err)
	}
	// flush
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}

	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, ConfigPath)
}

// 后处理 raw TOML encoder 输出来提高可读性，
//   - 添加空行
//   - 移除空section headers
//
// 特意保留了键值对行不变，包括值为0的行（如`thinking_messages = false`, `port = 0`）
// 因为可能是用户明确设置
func formatTOML(raw string) string {
	lines := strings.Split(raw, "\n")

	//
	var out []string
	preBlank := false
	for _, line := range lines {
		line = strings.TrimRight(line, "\t")
		trimmed := strings.TrimSpace(line)
		isBlank := trimmed == ""

		if isBlank {
			if preBlank {
				continue
			}
			preBlank = true
			out = append(out, "")
		}
		preBlank = false

		if trimmed[0] == '[' {
			if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
				out = append(out, "")
			}
		}
		out = append(out, line)
	}

	// 移除首位空行，确保尾部有单个空行
	for len(out) > 0 && strings.TrimSpace(out[0]) == "" {
		out = out[1:]
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n") + "\n"
}

// 保存语言配置到文件
func SaveLanguage(lang string) error {
	// 配置文件锁
	configMu.Lock()
	defer configMu.Unlock()
	if ConfigPath == "" {
		return fmt.Errorf("config path not set")
	}
	// 读取配置文件内容
	data, err := os.ReadFile(ConfigPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	cfg.Language = lang
	return saveConfig(cfg)
}

// 从配置文件中读取app对
func GetAppPair() (string, string, error) {
	if ConfigPath == "" {
		return "", "", fmt.Errorf("GetAppPair:: config path not set")
	}
	data, err := os.ReadFile(ConfigPath)
	if err != nil {
		return "", "", fmt.Errorf("GetAppPair:: read config: %w", err)
	}
	cfg := &Config{}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return "", "", fmt.Errorf("GetAppPair:: parse config: %w", err)
	}
	fmt.Println(cfg.Project.Platform)
	appID, ok := cfg.Project.Platform.Options["app_id"].(string)
	if !ok || appID == "" {
		return "", "", fmt.Errorf("GetAppPair:: app_id requied and must be string")
	}
	appSecret, ok := cfg.Project.Platform.Options["app_secret"].(string)
	if !ok || appSecret == "" {
		return "", "", fmt.Errorf("GetAppPair:: app_secret requied and must be string")
	}
	if appID == "" || appSecret == "" {
		return "", "", fmt.Errorf("GetAppPair:: Both app_id/app_scret required in config file.")
	}
	return appID, appSecret, nil
}

// 列出项目名称
func ListProjects() (string, error) {
	if ConfigPath == "" {
		return "", fmt.Errorf("config path not set")
	}
	data, err := os.ReadFile(ConfigPath)
	if err != nil {
		return "", fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return "", fmt.Errorf("parse config: %w", err)
	}
	return cfg.Project.Name, nil
}

// 添加全局自定义命令,并持久化到config文件
func AddCommand(cmd CommandConfig) error {
	configMu.Lock()
	defer configMu.Unlock()
	if ConfigPath == "" {
		return fmt.Errorf("config path not set")
	}
	data, err := os.ReadFile(ConfigPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	for _, c := range cfg.Commands {
		if c.Name == cmd.Name {
			return fmt.Errorf("command %q already exists", cmd.Name)
		}
	}
	cfg.Commands = append(cfg.Commands, cmd)
	return saveConfig(cfg)
}

// 移除一个全局自定义命令 持久化到config
func RemoveCommand(name string) error {
	configMu.Lock()
	defer configMu.Unlock()
	if ConfigPath == "" {
		return fmt.Errorf("config path not set")
	}
	data, err := os.ReadFile(ConfigPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	found := false
	var remaining []CommandConfig
	for _, c := range cfg.Commands {
		if c.Name == name {
			found = true
		} else {
			remaining = append(remaining, c)
		}
	}
	if !found {
		return fmt.Errorf("command %q not found", name)
	}
	cfg.Commands = remaining
	return saveConfig(cfg)
}

// EffectiveDisplay 解析全局 [display] 以及遗留的 quiet（根级或项目级）。
// 如果处于静音状态且未在[display]中明确设置thinking_messages / tool_messages，
// 它们映射为false（与显示前的quiet = true向后兼容）。
func EffectiveDisplay(cfg *Config, proj *ProjectConfig) (thinkingMessages, toolMessages bool, thinkingMaxLen, toolMaxLen int) {
	thinkingMessages = true
	toolMessages = true
	thinkingMaxLen = 300
	toolMaxLen = 500
	if cfg.Display.ThinkingMessages != nil {
		thinkingMessages = *cfg.Display.ThinkingMessages
	}
	if cfg.Display.ToolMessages != nil {
		toolMessages = *cfg.Display.ToolMessages
	}
	if cfg.Display.ThinkingMaxLen != nil {
		thinkingMaxLen = *cfg.Display.ThinkingMaxLen
	}
	if cfg.Display.ToolMaxLen != nil {
		toolMaxLen = *cfg.Display.ToolMaxLen
	}
	if projectQuietEffective(cfg, proj) {
		if cfg.Display.ThinkingMessages == nil {
			thinkingMessages = false
		}
		if cfg.Display.ToolMessages == nil {
			toolMessages = false
		}
	}
	return thinkingMessages, toolMessages, thinkingMaxLen, toolMaxLen
}

// projectQuietEffective 返回此项目是否应用了遗留的静默模式：如果存在显式的//每个项目的静默模式覆盖，则应用该覆盖；否则，应用全局根级的静默模式。
func projectQuietEffective(cfg *Config, proj *ProjectConfig) bool {
	if proj.Quiet != nil {
		return *proj.Quiet
	}
	if cfg.Quiet != nil {
		return *cfg.Quiet
	}
	return false
}
