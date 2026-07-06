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
	Aliases  []AliasConfig   `toml:"aliases"` // global command aliases

	Log      LogConfig `toml:"log"`
	Language string    `toml:"language"`

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

// SubAgentConfig 子 Agent 配置
type SubAgentConfig struct {
	TimeoutMins *int   `toml:"timeout_mins,omitempty"` // 超时时间（分钟），覆盖全局配置
	MaxRetries  *int   `toml:"max_retries,omitempty"`  // 最大重试次数，覆盖全局配置
	WorkDir     string `toml:"work_dir,omitempty"`     // 工作目录
	Description string `toml:"description,omitempty"`  // 描述
}

// HarnessConfig 多 Agent 编排配置
type HarnessConfig struct {
	Enabled       *bool                     `toml:"enabled"`                    // 是否启用编排模式
	SubAgentDir   string                    `toml:"sub_agent_dir,omitempty"`    // 子 Agent 提示词目录
	TimeoutMins   *int                      `toml:"timeout_mins,omitempty"`     // 超时时间（分钟）
	MaxRetries    *int                      `toml:"max_retries,omitempty"`      // 最大重试次数
	MaxConcurrent *int                      `toml:"max_concurrent,omitempty"`   // 最大并发数
	MaxPerProject *int                      `toml:"max_per_project,omitempty"`  // 每项目最大并发数
	SessionTTL    *int                      `toml:"session_ttl_mins,omitempty"` // 会话过期时间（分钟）
	LogLevel      string                    `toml:"log_level,omitempty"`        // 日志级别：debug, info, warn, error
	LogOutputPath string                    `toml:"log_output_path,omitempty"`  // 日志输出路径
	Aliases       map[string]string         `toml:"aliases,omitempty"`          // 子 Agent 别名映射
	Agents        map[string]SubAgentConfig `toml:"agents,omitempty"`           // 子 Agent 独立配置
}

type AgentConfig struct {
	Type    string         `toml:"type"`
	Options map[string]any `toml:"options"`
	Harness HarnessConfig  `toml:"harness"` // 多 Agent 编排配置
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

// ReloadHarnessConfig 重新加载配置并返回新的 HarnessConfig
// 支持运行时配置重载
func ReloadHarnessConfig() (*HarnessConfig, error) {
	configMu.Lock()
	defer configMu.Unlock()
	if ConfigPath == "" {
		return nil, fmt.Errorf("config path not set")
	}
	data, err := os.ReadFile(ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg.Project.Agent.Harness, nil
}

// GetSubAgentConfig 获取指定子 Agent 的有效配置，支持独立配置覆盖全局配置
// 如果 agentConfig 为空，返回全局默认配置
func GetSubAgentConfig(harnessCfg *HarnessConfig, agentName string) SubAgentConfig {
	if harnessCfg == nil {
		return SubAgentConfig{}
	}

	// 获取子 Agent 的独立配置
	agentCfg, exists := harnessCfg.Agents[agentName]
	if !exists {
		// 如果没有独立配置，返回空配置（将使用全局默认值）
		return SubAgentConfig{}
	}

	return agentCfg
}

// GetEffectiveTimeoutMins 获取有效超时时间
// 优先使用子 Agent 独立配置，其次使用全局配置，默认 5 分钟
func GetEffectiveTimeoutMins(harnessCfg *HarnessConfig, agentName string) int {
	defaultTimeout := 5

	// 检查子 Agent 独立配置
	if agentCfg, exists := harnessCfg.Agents[agentName]; exists && agentCfg.TimeoutMins != nil {
		return *agentCfg.TimeoutMins
	}

	// 检查全局配置
	if harnessCfg.TimeoutMins != nil {
		return *harnessCfg.TimeoutMins
	}

	// 默认值
	return defaultTimeout
}

// GetEffectiveMaxRetries 获取有效最大重试次数
// 优先使用子 Agent 独立配置，其次使用全局配置，默认 3 次
func GetEffectiveMaxRetries(harnessCfg *HarnessConfig, agentName string) int {
	defaultRetries := 3

	// 检查子 Agent 独立配置
	if agentCfg, exists := harnessCfg.Agents[agentName]; exists && agentCfg.MaxRetries != nil {
		return *agentCfg.MaxRetries
	}

	// 检查全局配置
	if harnessCfg.MaxRetries != nil {
		return *harnessCfg.MaxRetries
	}

	// 默认值
	return defaultRetries
}

// GetEffectiveMaxConcurrent 获取有效最大并发数，默认 5
func GetEffectiveMaxConcurrent(harnessCfg *HarnessConfig) int {
	if harnessCfg == nil || harnessCfg.MaxConcurrent == nil {
		return 5
	}
	return *harnessCfg.MaxConcurrent
}

// GetEffectiveMaxPerProject 获取有效每项目最大并发数，默认 2
func GetEffectiveMaxPerProject(harnessCfg *HarnessConfig) int {
	if harnessCfg == nil || harnessCfg.MaxPerProject == nil {
		return 2
	}
	return *harnessCfg.MaxPerProject
}

// IsHarnessEnabled 检查 harness 是否启用，默认 false
func IsHarnessEnabled(harnessCfg *HarnessConfig) bool {
	if harnessCfg == nil || harnessCfg.Enabled == nil {
		return false
	}
	return *harnessCfg.Enabled
}

// GetSessionTTL 获取会话过期时间（分钟），默认 30 分钟
func GetSessionTTL(harnessCfg *HarnessConfig) int {
	if harnessCfg == nil || harnessCfg.SessionTTL == nil {
		return 30
	}
	return *harnessCfg.SessionTTL
}

// GetSubAgentAlias 获取子 Agent 别名，如果不存在则返回原名
func GetSubAgentAlias(harnessCfg *HarnessConfig, alias string) string {
	if harnessCfg == nil || harnessCfg.Aliases == nil {
		return alias
	}
	if realName, exists := harnessCfg.Aliases[alias]; exists {
		return realName
	}
	return alias
}
