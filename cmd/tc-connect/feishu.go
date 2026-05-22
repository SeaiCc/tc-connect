package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"tc-connect/config"
)

const (
	openFeishuBaseURL = "https://open.feishu.cn"
)

type tenantTokenResponse struct {
	Code              int    `json:"code"`
	Msg               string `json:"msg"`
	TenantAccessToken string `json:"tenant_access_token"`
}

func runFeishu(args []string) {
	if len(args) == 0 {
		printFeishuUsage()
		return
	}

	switch args[0] {
	case "setup": // 暂时只用绑定模式
		runFeishuSetup(args[:])
	// TODO: new -> new mode | bind, link -> bind
	default:
		fmt.Fprintf(os.Stderr, "Unknown feishu subcommand: %s\n\n", args[0])
		printFeishuUsage()
		os.Exit(1)
	}
}

func runFeishuSetup(args []string) {
	fs := flag.NewFlagSet("feishu bind", flag.ExitOnError)
	configFile := fs.String("config", "", "path to config file")

	_ = fs.Parse(args)

	// 如果命令行参数中有 config 配置则使用， 否则从当前或用户目录查找config.toml
	initConfigPath(*configFile)

	// 解析appID 和 appSecret
	resolvedAppID, resolvedAppSecret, err := config.GetAppPair()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	// 校验appID 和 appSecret
	err = validateAppCredentials(resolvedAppID, resolvedAppSecret)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: app_id/app_secret validattion failed: %v\n", err)
	}
	// 解析project名
	targetProject, err := config.ListProjects()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✅ Feishu bot configured for project %q\n", targetProject)
	fmt.Printf("   App ID: 	%s\n", resolvedAppID)
	fmt.Println()

	printBotMenuGuidance()
}

func printBotMenuGuidance() {
	base := "https://open.feishu.cn"

	fmt.Println("📋 机器人菜单配置（可选）：")
	fmt.Println("   飞书机器人支持自定义悬浮菜单，可将常用命令固定在输入框上方。")
	fmt.Println("   菜单需在开发者后台手动配置（暂不支持 API 设置），步骤：")
	fmt.Printf("   1. 打开开发者后台: %s/app\n", base)
	fmt.Println("   2. 选择你的应用 → 应用能力 → 机器人")
	fmt.Println("   3. 开启「机器人自定义菜单」，选择「悬浮菜单」样式")
	fmt.Println("   4. 添加菜单项，响应动作选择「发送文字消息」")
	fmt.Println("   5. 创建版本并发布（生效约需 5 分钟）")
	fmt.Println()
	fmt.Println("   推荐菜单配置：")
	fmt.Println("   ┌─────────────────────────────────────────────┐")
	fmt.Println("   │ 主菜单: cc-connect                          │")
	fmt.Println("   │   ├── /help     帮助                        │")
	fmt.Println("   │   ├── /status   状态                        │")
	fmt.Println("   │   ├── /new      新会话                      │")
	fmt.Println("   │   ├── /list     会话列表                    │")
	fmt.Println("   │   └── /stop     停止                        │")
	fmt.Println("   │ 主菜单: 设置                                 │")
	fmt.Println("   │   ├── /model    切换模型                    │")
	fmt.Println("   │   ├── /mode     切换模式                    │")
	fmt.Println("   │   ├── /quiet    静默模式                    │")
	fmt.Println("   │   ├── /lang     语言                        │")
	fmt.Println("   │   └── /config   配置                        │")
	fmt.Println("   │ 主菜单: 工具                                 │")
	fmt.Println("   │   ├── /compress 压缩上下文                  │")
	fmt.Println("   │   ├── /memory   记忆                        │")
	fmt.Println("   │   ├── /cron     定时任务                    │")
	fmt.Println("   │   ├── /whoami   查看我的ID                  │")
	fmt.Println("   │   └── /doctor   诊断                        │")
	fmt.Println("   └─────────────────────────────────────────────┘")
	fmt.Println()
}

func printFeishuUsage() {
	fmt.Println(`用法：cc-connect feishu <命令> [选项]

命令：
  setup   统一入口：目前只有 --app/--app-id => BIND 流程
  bind    强制 BIND 流程（需要 app_id/app_secret）。

选项：
  --config <path>             配置文件路径
  --project <name>            目标项目（如果不存在则自动创建）
  --platform-index <n>        项目中基于 1 的飞书/Lark 平台索引（默认：第一个）
  --platform-type <type>      强制平台类型：feishu 或 lark
  --app <id:secret>           现有凭证（bind/setup 推荐）
  --set-allow-from-empty      当可用时将所有者 open_id 合并到 allow_from（默认：false）
  --debug                     打印授权调试日志

示例：
  # 推荐：一个命令处理两种流程
  cc-connect feishu setup --project my-project
  cc-connect feishu setup --project my-project --app cli_xxx:sec_xxx

  # 等同于 "setup --app ..."
  cc-connect feishu bind --project my-project --app cli_xxx:sec_xxx

  # 仅当必须强制扫码授权时使用
  cc-connect feishu new --project my-project --platform-type lark`)
}

// 校验AppID 和 AppSecret
func validateAppCredentials(appID, appSecret string) error {
	appID = strings.TrimSpace(appID)
	appSecret = strings.TrimSpace(appSecret)

	base := openFeishuBaseURL
	ok, err := validateAppCredentialsAgainstBae(base, appID, appSecret)
	if ok {
		return nil
	}
	if err == nil {
		err = fmt.Errorf("unknown validation error")
	}
	return err
}

// 校验AppID 和 AppSecret
func validateAppCredentialsAgainstBae(baseURL, appID, appSecret string) (bool, error) {
	body, _ := json.Marshal(map[string]string{
		"app_id":     appID,
		"app_secret": appSecret,
	})
	req, err := http.NewRequest(http.MethodPost, baseURL+"/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, err
	}

	var parsed tenantTokenResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return false, fmt.Errorf("decode response: %w", err)
	}
	if parsed.Code == 0 && parsed.TenantAccessToken != "" {
		return true, nil
	}
	if parsed.Msg != "" {
		return false, fmt.Errorf("code=%d msg=%s", parsed.Code, parsed.Msg)
	}
	return false, nil
}
