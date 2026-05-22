package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"tc-connect/config"

	"tc-connect/core"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "feishu" {
		runFeishu(os.Args[2:])
		return
	}

	var logWriter io.Writer // 日志
	// 读取通用配置
	configFlag := flag.String("config", "", "path to config file (default: ./config.toml) or ~/.tc-connect/config.toml")
	flag.Usage = printUsage
	flag.Parse()

	initConfigPath(*configFlag)
	configPath := config.ConfigPath

	// 获取实例锁防止出现重复进程
	instanceLock, err := AcquireInstanceLock(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	slog.Info("acquired instance lock", "path", instanceLock.Path())

	// 检查文件是否存在 os.IsNotExist: 判断错误类型
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		// 尝试创建模板后退出
		if err := bootstrapConfig(configPath); err != nil {
			fmt.Fprintf(os.Stderr, "Error creating config: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Created default config at %s\n", configPath)
		fmt.Println("Please edit this file to add your agent and platform credentials, then run cc-connect again.")
		os.Exit(0)
	}
	// 从文件中加载配置
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config (%s): %v\n", configPath, err)
		os.Exit(1)
	}

	slog.Info("config loaded", "path", configPath)
	// 日志等级配置
	setupLogger(cfg.Log.Level, logWriter)
	// 创建 Agent
	proj := cfg.Project
	agent, err := core.CreateAgent(proj.Agent.Type, buildAgentOptions(cfg.DataDir, proj))
	if err != nil {
		slog.Error("failed to create agent", "project", proj.Name, "error", err)
	}
	// 创Platform对象
	pc := proj.Platform // 配置
	opts := make(map[string]any, len(pc.Options)+2)
	for k, v := range pc.Options {
		opts[k] = v
	}
	opts["cc_data_dir"] = cfg.DataDir
	opts["cc_project"] = proj.Name
	platform, err := core.CreatePlatform(pc.Type, opts)
	if err != nil {
		slog.Error("failed to create platform", "project", proj.Name, "type", pc.Type, "error", err)
		os.Exit(1)
	}

	// Session相关
	workDir, _ := proj.Agent.Options["work_dir"].(string)
	// 项目运行时状态工作区状态修改({proj.Name}.state.json)
	projectState := core.NewProjectStateStore(projectStatePath(cfg.DataDir, proj.Name))
	// 引用project状态重写
	effectiveWorkDir := applyProjectStateOverride(proj.Name, agent, workDir, projectState)
	// 获取session文件
	sessionFile := sessionStorePath(cfg.DataDir, proj.Name, effectiveWorkDir)

	// 解析语言
	var lang core.Language
	switch cfg.Language {
	case "zh", "chinese":
		lang = core.LangChinese
	case "zh-TW", "zh_TW", "zhtw":
		lang = core.LangTraditionalChinese
	case "ja", "japanese":
		lang = core.LangJapanese
	case "en", "english":
		lang = core.LangEnglish
	default:
		lang = core.LangAuto
	}

	//  创建engine
	engine := core.NewEngine(proj.Name, agent, platform, sessionFile, lang)
	showCtx := true // 始终展示上下文
	engine.SetShowContextIndicator(showCtx)
	engine.SetBaseWorkDir(workDir)
	engine.SetProjectStateStore(projectState)

	// 一系列wire配置
	// Wire multi-workspace mode
	// Wire terminal observation (--observe / [projects.observe])
	// Wire global custom commands
	// Wire command persistence callbacks
	// Wire global aliases
	// Wire banned words
	// Wire disabled commands (project-level)
	// Wire admin allowlist for privileged commands
	// Wire per-user role-based policies
	// Wire display truncation settings (includes legacy quiet → display mapping)
	// Wire local reference normalization / rendering
	// Wire streaming preview
	// Wire rate limiting
	// Wire outgoing rate limiting
	// Wire idle timeout
	// Wire auto-compress settings
	// Wire sender injection

	// 设置回调用于自动识别语言
	if lang == core.LangAuto {
		engine.SetLanguageSaveFunc(func(l core.Language) error {
			return config.SaveLanguage(string(l))
		})
	}
	// Wire 配置重加载
	// capturedEngine := engine
	// capturedProjName := proj.Name
	// engine.SetConfigReloadFunc(func() (*core.ConfigReloadResult, error) {
	// 	return reloadConfig(configPath, capturedProjName, capturedEngine)
	// })

	// Wire /web command callbacks

	// 开启定时任务
	// cronStore, err := core.NewCronStore(cfg.DataDir)
	// if err != nil {
	// 	slog.Warn("cron store unavailable", "error", err)
	// }
	// var cronSched *core.CronScheduler
	// if cronStore != nil {
	// 	cronSched = core.NewCronScheduler(cronStore)
	// 	if cfg.Cron.Silent != nil && *cfg.Cron.Silent {
	// 		cronSched.SetDefaultSilent(true)
	// 	}
	// 	cronSched.RegisterEngine(cfg.Project.Name, engine)
	// 	engine.SetCronScheduler(cronSched)
	// }

	// 开启心跳定时任务
	// heartbeatSched := core.NewHeartbeatScheduler(cfg.DataDir)
	// hbCfg := buildHeartbeatConfig(proj.Heartbeat)
	// if hbCfg.Enabled {
	// 	heartbeatSched.Register(proj.Name, hbCfg, engine, effectiveWorkDir)
	// }
	// engine.SetHeartbeatScheduler(heartbeatSched)

	if err := engine.Start(); err != nil {
		slog.Error("all engines failed to start, exiting")
		os.Exit(1)
	}

	// if cronSched != nil {
	// 	if err := cronSched.Start(); err != nil {
	// 		slog.Error("cron scheduler start failed", "error", err)
	// 	}
	// }
	// heartbeatSched.Start()

	// Start bridge server if enabled
	var bridgeSrv *core.BridgeServer
	if cfg.Bridge.Enabled != nil && *cfg.Bridge.Enabled {
		port := cfg.Bridge.Port
		if port <= 0 {
			port = 9810
		}
		path := cfg.Bridge.Path
		if path == "" {
			path = "/bridge/ws"
		}
		bridgeSrv = core.NewBridgeServer(port, cfg.Bridge.Token, path, cfg.Bridge.CORSOrigins)
		bp := bridgeSrv.NewPlatform(cfg.Project.Name)
		bridgeSrv.RegisterEngine(cfg.Project.Name, engine, bp)
		engine.SetPlatform(bp)

		bridgeSrv.Start()
	}

	// 若webhook server可用,则启用
	// var webhookSrv *core.WebhookServer
	// if cfg.Webhook.Enabled != nil && *cfg.Webhook.Enabled {
	// 	port := cfg.Webhook.port
	// 	if port <= 0 {
	// 		port = 9111
	// 	}
	// 	path := cfg.Webhook.Path
	// 	if path == "" {
	// 		path = "/hook"
	// 	}
	// 	webhookSrv = core.NewWebhookServer(port, cfg.Webhook.Token, path)
	// 	webhookSrv.RegisterEngine(cfg.Project.Name, engine)
	// 	webhookSrv.Start()
	// }

	// 若管理API server 可用, 启用
	// var mgmtSrv *core.ManagementServer
	// if cfg.Management.Enabled != nil && *cfg.Management.Enabled {
	// 	if port <= 0 {
	// 		port = 9820
	// 	}
	// 	mgmtSrv = core.NewManagementServer(port, cfg.Management.Token, cfg.Management.CORSOrigins)
	// 	mgmtSrv.RegisterEngine(cfg.Project.Name, engine)
	// 	if cronSched != nil {
	// 		mgmtSrv.SetCronScheduler(cronSched)
	// 	}
	// 	mgmtSrv.SetHeartbeatScheduler(heartbeatSched)
	// 	if bridgeSrv != nil {
	// 		mgmtSrv.SetBridgeServer(bridgeSrv)
	// 	}
	// 	mgmtSrv.SetGetProjectConfig(config.GetProjectConfigDetails)
	// 	mgmtSrv.SetConfigFilePath(configPath)
	// 	mgmtSrv.SetGetGlobalSettings(config.SetGetGlobalSettings)
	// 	mgmtSrv.Start()
	// }

	// 启用内部API server 用于CLI 发送
	apiSrv, err := core.NewAPIServer(cfg.DataDir)
	if err != nil {
		slog.Warn("api server unavailable", "error", err)
	} else {
		// relayMgr := core.NewRelayManager(cfg.DataDir)
		// if cfg.Relay.TimeoutSecs != nil {
		// 	secs := *cfg.Relay.TimeoutSecs
		// 	if secs <= 0 {
		// 		relayMgr.SetTimeout(0)
		// 	} else {
		// 		relayMgr.SetTimeout(time.Duration(secs) * time.Second)
		// 	}
		// }
		// apiSrv.SetRelayManager(relayMgr)

		// 创建DirHistory
		// dirHistory := core.NewDirHistory(cfg.DataDir)

		apiSrv.RegisterEngine(cfg.Project.Name, engine)
		// engine.SetRelayManager(relayMgr)
		// engine.SetDirHistory(dirHistory)

		// //  确保初始 work_dir 在历史
		// if effectiveWorkDir != "" {
		// 	if !dirHistory.Contains(cfg.Project.Name, effectiveWorkDir) {
		// 		dirHistory.Add(cfg.Project.Name, effectiveWorkDir)
		// 	}
		// }
		// if cronSched != nil {
		// 	apiSrv.SetCronScheduler(cronSched)
		// }
		apiSrv.Start()
	}

	slog.Info("tc-connect is running", "project", cfg.Project.Name)

	//  启动后检查是否是重启,然后发送提示信息
	// if notify := core.ConsumeRestartNotify(cfg.DataDir); notify != nil {
	// 	slog.Info("post-restart: sending success notificaton", "platform", notify.Platform, "session", notify.SessionKey)
	// 	engine.SendRestartNotification(notify.Platform, notify.SessionKey)
	// }

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	var restartReq *core.RestartRequest
	select {
	case <-sigCh:
	case req := <-core.RestartCh:
		restartReq = &req
		slog.Info("restart requested via /restart command", "session", req.SessionKey, "platform", req.Platform)
	}

	// slog.Info("shutting down...")
	// if mgmtSrv != nil {
	// 	mgmtSrv.Stop()
	// }
	// if bridgeSrv != nil {
	// 	bridgeSrv.Stop()
	// }
	// if webhookSrv != nil {
	// 	webhookSrv.Stop()
	// }
	// heartbeatSched.Stop()
	// if cronSched != nil {
	// 	cronSched.Stop()
	// }
	// if apiSrv != nil {
	// 	apiSrv.Stop()
	// }
	// if err := engine.Stop(); err != nil {
	// 	slog.Error("shutdown error", "error", err)
	// }
	// instanceLock.Release()

	if restartReq != nil {
		if err := core.SaveRestartNotify(cfg.DataDir, *restartReq); err != nil {
			slog.Error("restart: save notify failed", "error", err)
		}
		execPath, err := os.Executable()
		if err != nil {
			slog.Error("restart: cannot determine executable path", "error", err)
			os.Exit(1)
		}
		// 自动更新之后, linux上os.Executable()可能返回 .old 路径
		// 剔除.old 后缀来从更新之后的二进制重启
		if strings.HasSuffix(execPath, ".old") {
			newPath := strings.TrimSuffix(execPath, ".old")
			if _, err := os.Stat(newPath); err == nil {
				execPath = newPath
			}
		}
		// slog.Info("restarting...", "path", execPath, "args", os.Args)
		// if err := restartProcess(execPath); err != nil {
		// 	slog.Error("restart: failed", "error", err)
		// 	os.Exit(1)
		// }
	}
	slog.Info("bye")

}

// 构建一个唯一的project name + work_dir, 检查dataDir遗留的session文件确保向后兼容
// 如果找到，使用这个路径，否则使用dataDir/sessions/
func sessionStorePath(dataDir, name, workDir string) string {
	var filename string
	if workDir == "" {
		filename = name + ".json"
	} else {
		abs, err := filepath.Abs(workDir)
		if err != nil {
			abs = workDir
		}
		h := sha256.Sum256([]byte(abs))
		short := hex.EncodeToString(h[:4])
		filename = fmt.Sprintf("%s_%s.json", name, short)
	}
	// 检查dataDir中的历史文件确保向后兼容
	// 同时检查旧的.session.json命名约定
	for _, legacy := range []string{
		filepath.Join(dataDir, filename),
		filepath.Join(dataDir, strings.TrimSuffix(filename, ".json")+".sessions.json"),
	} {
		if _, err := os.Stat(legacy); err == nil {
			slog.Info("session: using legacy file in dataDir", "path", legacy)
			return legacy
		}
	}

	return filepath.Join(dataDir, "sessions", filename)
}

// 路径中的非法字符都替换为 _, 创建 project/{projectName}.state.json文件
func projectStatePath(dataDir, projectName string) string {
	replacer := strings.NewReplacer(
		"\\", "_",
		"/", "_",
		":", "_",
		"*", "_",
		"?", "_",
		"\"", "_",
		"<", "_",
		">", "_",
		"|", "_",
	)
	name := strings.TrimSpace(projectName)
	name = replacer.Replace(name)
	if name == "" {
		name = "project"
	}
	return filepath.Join(dataDir, "projects", name+".state.json")
}

// 从store中 获取要override的工作路径，应用修改
func applyProjectStateOverride(projectName string, agent core.Agent, configuredWorkDir string, store *core.ProjectStateStore) string {
	effectiveWorkDir := configuredWorkDir
	if store == nil {
		return effectiveWorkDir
	}
	// 判断agent是否实现了WorkDirSwitcher
	switcher, ok := agent.(core.WorkDirSwitcher)
	if !ok {
		return effectiveWorkDir
	}
	// 获取新的工作路径
	override := store.WorkDirOverride()
	if override == "" {
		return effectiveWorkDir
	}
	if abs, err := filepath.Abs(override); err == nil {
		override = abs
	}

	info, err := os.Stat(override)
	if err != nil || !info.IsDir() {
		slog.Warn("project_state: ignoring invvalid work_dir overrid", "project", projectName, "work_dir", override)
		return effectiveWorkDir
	}
	// 设置
	switcher.SetWorkDir(override)
	slog.Info("project_state: applied work_dir override", "project", projectName, "work_dir", override)
	return override
}

// 显式flag -> ./config.toml(main.go同目录) -> ~/.tc-connect/config.toml
func resolveConfigPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if _, err := os.Stat("config.toml"); err == nil {
		return "config.toml"
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".tc-connect", "config.toml")
	}
	return "config.toml"
}

// 创建config模板
func bootstrapConfig(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	const tmpl = `# tc-connect configuration
# Docs: xxx

[log]
level = "info"

[[projects]]
name = "my-project"

[projects.agent]
type = "opencode"

[projects.agent.options]
work_dir = "/path/to/your/project"
mode = "default"
# model = ""

# --- Choose at least one platform below ---

# Feishu / Lark (WebSocket, no public IP needed)
[[projects.platforms]]
type = "feishu"

[projects.platforms.options]
app_id = "your-feishu-app-id"
app_secret = "your-feishu-app-secret"

`
	return os.WriteFile(path, []byte(tmpl), 0o644)
}

func setupLogger(level string, w io.Writer) {
	var logLevel slog.Level
	switch level {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}
	if w == nil {
		w = os.Stdout
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: logLevel,
	})))
}

func buildAgentOptions(dataDir string, proj config.ProjectConfig) map[string]any {
	opts := make(map[string]any, len(proj.Agent.Options)+2)
	for k, v := range proj.Agent.Options {
		opts[k] = v
	}
	opts["cc_data_dir"] = dataDir
	opts["cc_project"] = proj.Name
	return opts
}

// 打印使用方式
func printUsage() {
	v := "dev"
	fmt.Fprintf(os.Stderr, `
  _                                              _
 __| |_  ___        ___ ___  _ __  _ __   ___  ___| |_
|__| __|/ __|_____ / __/ _ \| '_ \| '_ \ / _ \/ __| __|
   | |_| (_|_____|  (_| (_) | | | | | | |  __/ (__| |_
   \__| \__|      \___\___/|_| |_|_| |_|\___|\___|\__|  %s%s

  Bridge your messaging platforms to local AI coding agents.
  Supports: OpenCode
  Platforms: Feishu

Usage:
  go run . [flags]

Flags:
  --config <path>    Path to config file (default: ./config.toml or ~/.tc-connect/config.toml)
  --force            Kill any existing instance with the same config before starting
  --version          Print version and exit
  --help             Show this help message

Commands:
  daemon             Manage tc-connect as a background service (systemd/launchd)
    install          Install and start the daemon service
    uninstall        Remove the daemon service
    start            Start the daemon
    stop             Stop the daemon
    restart          Restart the daemon
    status           Show daemon status
    logs             View daemon logs (-f to follow, -n N for last N lines)

  send               Send a message to an active session via internal API
                     (-m <text> | --stdin, -p <project>, -s <session>)

  cron               Manage scheduled tasks
    add              Create a scheduled task (-c <expr> --prompt <text>)
    list             List scheduled tasks
    del              Delete a scheduled task by ID

  sessions           Browse session history
    list             List all sessions (pipe-friendly)
    show <id>        Show session messages (-n N for last N)

  relay              Cross-project message relay
    send             Send a message to another project and get the response

  provider           Manage API providers for projects
    add              Add a provider (--project, --name, --api-key, ...)
    list             List providers (--project)
    remove           Remove a provider (--project, --name)
    import           Import providers from cc-switch

  feishu             Setup Feishu/Lark bot credentials
    setup            Smart setup (QR create or bind when --app is provided)
    new              Force QR onboarding to create a new bot
    bind             Bind existing app_id/app_secret

  config             Manage configuration
    example          Print a complete annotated config.toml example
    format           Format the config file (alias: fmt)
    path             Print the resolved config file path

`, v, "")
}
