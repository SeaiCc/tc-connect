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
	DataDir  string        `toml:"data_dir"` // session store directory, default .cc-connect
	Project  ProjectConfig `toml:"project"`
	Log      LogConfig     `toml:"log"`
	Language string        `toml:"language"`

	Bridge BridgeConfig `toml:"bridge"`
}

// 控制websocket桥接用于外部平台adapters
type BridgeConfig struct {
	Enabled     *bool    `toml:"enabled"`                // 默认false
	Port        int      `toml:"port,omitempty"`         // 监听端口， 默认9810
	Token       string   `toml:"token,omitempty"`        // 验证token 共享密钥
	Path        string   `toml:"path,omitempty"`         // URL路径 默认 "/bridge/ws"
	CORSOrigins []string `toml:"cors_origins,omitempty"` // 允许CORS 域， empty = 没有
}

// 绑定一个agent (带有特定的work_dir)
type ProjectConfig struct {
	Name     string         `toml:"name"`
	Agent    AgentConfig    `toml:"agent"`
	Platform PlatformConfig `toml:"platform"`
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

func Load() (*Config, error) {
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
