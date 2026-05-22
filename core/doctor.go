package core

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// 给检查到的名称提供日文翻译
var checkNameJa = map[string]string{
	"Memory (Go runtime)": "メモリ (Go runtime)",
	"System Memory":       "システムメモリ",
	"CPU Load":            "CPU 負荷",
	"Disk Space":          "ディスク容量",
	"FFmpeg (voice)":      "FFmpeg (音声)",
	"Data Directory":      "データディレクトリ",
	"Config File":         "設定ファイル",
	"Platforms":           "プラットフォーム",
}

// 给检查到的名称提供中文翻译
var checkNameZh = map[string]string{
	"Memory (Go runtime)": "内存 (Go runtime)",
	"System Memory":       "系统内存",
	"CPU":                 "CPU",
	"CPU Load":            "CPU 负载",
	"Disk Space":          "磁盘空间",
	"Git":                 "Git",
	"SQLite3":             "SQLite3",
	"FFmpeg (voice)":      "FFmpeg (语音)",
	"HTTPS (Anthropic)":   "HTTPS (Anthropic)",
	"Data Directory":      "数据目录",
	"Config File":         "配置文件",
	"Platforms":           "平台",
}

type DoctorStatus int

const (
	DoctorPass DoctorStatus = iota
	DoctorWarn
	DoctorFail
)

func (s DoctorStatus) Icon() string {
	switch s {
	case DoctorPass:
		return "✅"
	case DoctorWarn:
		return "⚠️"
	default:
		return "❌"
	}
}

type DoctorCheckResult struct {
	Name    string
	Status  DoctorStatus
	Detail  string
	Latency time.Duration
}

type DoctorChecker interface {
	DoctorChecks(ctx context.Context) []DoctorCheckResult
}

// 执行所有诊断检查
func RunDoctorChecks(ctx context.Context, agent Agent, platform Platform) []DoctorCheckResult {
	var results []DoctorCheckResult

	// results = append(results, checkAgentBinary(ctx, agent)...) // 默认CLI文件存在(opencode)
	// results = append(results, checkAgentAuth(ctx, agent)...) // 默认执行CLI --version无异常
	results = append(results, checkPlatform(platform)...)
	results = append(results, checkSystem(ctx)...)
	results = append(results, checkDependencies()...)
	results = append(results, checkNetwork(ctx)...)

	if dc, ok := agent.(DoctorChecker); ok {
		results = append(results, dc.DoctorChecks(ctx)...)
	}

	return results
}

// 直接添加为connected 
func checkPlatform(platform Platform) []DoctorCheckResult {
	return []DoctorCheckResult{{
		Name:   fmt.Sprintf("Platform (%s)", platform.Name()),
		Status: DoctorPass,
		Detail: "connected",
	}}

}

// CPU 内存 磁盘检查
func checkSystem(ctx context.Context) []DoctorCheckResult {
	var results []DoctorCheckResult

	// Memory
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	allocMB := memStats.Alloc / 1024 / 1024
	sysMB := memStats.Sys / 1024 / 1024
	results = append(results, DoctorCheckResult{
		Name:   "Memory (Go runtime)",
		Status: DoctorPass,
		Detail: fmt.Sprintf("alloc %d MB / sys %d MB", allocMB, sysMB),
	})

	// System memory (Linux)
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile("/proc/meminfo"); err == nil {
			var totalKB, availKB uint64
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "MemTotal:") {
					_, _ = fmt.Sscanf(line, "MemTotal: %d kB", &totalKB)
				} else if strings.HasPrefix(line, "MemAvailable:") {
					_, _ = fmt.Sscanf(line, "MemAvailable: %d kB", &availKB)
				}
			}
			if totalKB > 0 {
				totalMB := totalKB / 1024
				availMB := availKB / 1024
				usedPct := 100 - (availKB*100)/totalKB
				status := DoctorPass
				if usedPct > 90 {
					status = DoctorFail
				} else if usedPct > 75 {
					status = DoctorWarn
				}
				results = append(results, DoctorCheckResult{
					Name:   "System Memory",
					Status: status,
					Detail: fmt.Sprintf("%d MB available / %d MB total (%d%% used)", availMB, totalMB, usedPct),
				})
			}
		}
	}

	// CPU
	results = append(results, DoctorCheckResult{
		Name:   "CPU",
		Status: DoctorPass,
		Detail: fmt.Sprintf("%d cores, %s/%s", runtime.NumCPU(), runtime.GOOS, runtime.GOARCH),
	})

	// Load average (Linux/macOS)
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile("/proc/loadavg"); err == nil {
			parts := strings.Fields(string(data))
			if len(parts) >= 3 {
				status := DoctorPass
				detail := fmt.Sprintf("load avg: %s %s %s", parts[0], parts[1], parts[2])
				// Rough check: if 1-min load > 2x CPU count, warn
				var load1 float64
				_, _ = fmt.Sscanf(parts[0], "%f", &load1)
				if load1 > float64(runtime.NumCPU()*2) {
					status = DoctorWarn
				}
				results = append(results, DoctorCheckResult{
					Name:   "CPU Load",
					Status: status,
					Detail: detail,
				})
			}
		}
	}

	// Disk space
	if wd, err := os.Getwd(); err == nil {
		tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(tctx, "df", "-h", wd).Output(); err == nil {
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			if len(lines) >= 2 {
				fields := strings.Fields(lines[len(lines)-1])
				if len(fields) >= 5 {
					status := DoctorPass
					usePct := strings.TrimSuffix(fields[4], "%")
					var pct int
					_, _ = fmt.Sscanf(usePct, "%d", &pct)
					if pct > 95 {
						status = DoctorFail
					} else if pct > 85 {
						status = DoctorWarn
					}
					results = append(results, DoctorCheckResult{
						Name:   "Disk Space",
						Status: status,
						Detail: fmt.Sprintf("%s available / %s total (%s used)", fields[3], fields[1], fields[4]),
					})
				}
			}
		}
	}

	return results
}

// git sqlite ffmpeg 依赖检查
func checkDependencies() []DoctorCheckResult {
	deps := []struct {
		bin      string
		label    string
		required bool
	}{
		{"git", "Git", true},
		{"sqlite3", "SQLite3", false},
		{"ffmpeg", "FFmpeg (voice)", false},
	}

	var results []DoctorCheckResult
	for _, d := range deps {
		path, err := exec.LookPath(d.bin)
		if err != nil {
			status := DoctorWarn
			if d.required {
				status = DoctorFail
			}
			results = append(results, DoctorCheckResult{
				Name:   d.label,
				Status: status,
				Detail: "not found",
			})
		} else {
			results = append(results, DoctorCheckResult{
				Name:   d.label,
				Status: DoctorPass,
				Detail: path,
			})
		}
	}
	return results
}

// Claude Code 官网链接测试
func checkNetwork(ctx context.Context) []DoctorCheckResult {
	endpoints := []struct {
		label string
		host  string
		url   string
	}{
		{"API (Anthropic)", "api.anthropic.com:443", "https://api.anthropic.com"},
		{"API (OpenAI)", "api.openai.com:443", "https://api.openai.com"},
	}

	var results []DoctorCheckResult
	for _, ep := range endpoints {
		tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		start := time.Now()
		conn, err := (&net.Dialer{}).DialContext(tctx, "tcp", ep.host)
		latency := time.Since(start)
		cancel()

		if err != nil {
			results = append(results, DoctorCheckResult{
				Name:    ep.label,
				Status:  DoctorWarn,
				Detail:  fmt.Sprintf("docker::checkNetwork::connect failed: %v", err),
				Latency: latency,
			})
			continue
		}
		conn.Close()

		status := DoctorPass
		if latency > 3*time.Second {
			status = DoctorWarn
		}
		results = append(results, DoctorCheckResult{
			Name:    ep.label,
			Status:  status,
			Detail:  "TCP connect OK",
			Latency: latency,
		})
	}

	// HTTP check to verify proxy/firewall
	tctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	start := time.Now()
	client := &http.Client{Timeout: 8 * time.Second}
	req, _ := http.NewRequestWithContext(tctx, "HEAD", "https://api.anthropic.com", nil)
	resp, err := client.Do(req)
	latency := time.Since(start)
	if err != nil {
		results = append(results, DoctorCheckResult{
			Name:    "HTTPS (Anthropic)",
			Status:  DoctorWarn,
			Detail:  "HTTPS request failed: " + err.Error(),
			Latency: latency,
		})
	} else {
		resp.Body.Close()
		status := DoctorPass
		if latency > 5*time.Second {
			status = DoctorWarn
		}
		results = append(results, DoctorCheckResult{
			Name:    "HTTPS (Anthropic)",
			Status:  status,
			Detail:  fmt.Sprintf("HTTP %d", resp.StatusCode),
			Latency: latency,
		})
	}

	// Check config file
	if cfgPath := os.Getenv("CC_CONFIG_PATH"); cfgPath != "" {
		if _, err := os.Stat(cfgPath); err != nil {
			results = append(results, DoctorCheckResult{
				Name:   "Config File",
				Status: DoctorFail,
				Detail: cfgPath + ": " + err.Error(),
			})
		}
	}

	// Check data directory
	if home, err := os.UserHomeDir(); err == nil {
		dataDir := filepath.Join(home, ".cc-connect")
		if info, err := os.Stat(dataDir); err != nil {
			results = append(results, DoctorCheckResult{
				Name:   "Data Directory",
				Status: DoctorWarn,
				Detail: dataDir + " does not exist",
			})
		} else if !info.IsDir() {
			results = append(results, DoctorCheckResult{
				Name:   "Data Directory",
				Status: DoctorFail,
				Detail: dataDir + " is not a directory",
			})
		} else {
			results = append(results, DoctorCheckResult{
				Name:   "Data Directory",
				Status: DoctorPass,
				Detail: dataDir,
			})
		}
	}

	return results
}

// 将检查结果中的文本统一为i18n的语言
func localizeCheckName(name string, lang Language) string {
	switch lang {
	case LangChinese, LangTraditionalChinese:
		// Translate known names; parametric names (e.g. "Agent CLI (claude)") need prefix matching
		if zh, ok := checkNameZh[name]; ok {
			return zh
		}
		if strings.HasPrefix(name, "Agent CLI") {
			return strings.Replace(name, "Agent CLI", "Agent 命令行", 1)
		}
		if strings.HasPrefix(name, "Platform (") {
			return strings.Replace(name, "Platform", "平台", 1)
		}
		if strings.Contains(name, "Auth") {
			return strings.Replace(name, "Auth", "认证", 1)
		}
	case LangJapanese:
		if ja, ok := checkNameJa[name]; ok {
			return ja
		}
		if strings.HasPrefix(name, "Agent CLI") {
			return strings.Replace(name, "Agent CLI", "Agent CLI", 1)
		}
		if strings.HasPrefix(name, "Platform (") {
			return strings.Replace(name, "Platform", "プラットフォーム", 1)
		}
		if strings.Contains(name, "Auth") {
			return strings.Replace(name, "Auth", "認証", 1)
		}
	}
	return name
}

// 系统语言格式化检查结果
func FormatDoctorResults(results []DoctorCheckResult, i18n *I18n) string {
	lang := i18n.currentLang()

	var sb strings.Builder
	sb.WriteString(i18n.T(MsgDoctorTitle))

	passCount, warnCount, failCount := 0, 0, 0
	for _, r := range results {
		switch r.Status {
		case DoctorPass:
			passCount++
		case DoctorWarn:
			warnCount++
		case DoctorFail:
			failCount++
		}

		displayName := localizeCheckName(r.Name, lang)
		latStr := ""
		if r.Latency > 0 {
			latStr = fmt.Sprintf(" (%s)", r.Latency.Round(time.Millisecond))
		}
		sb.WriteString(fmt.Sprintf("%s %s%s\n   %s\n\n", r.Status.Icon(), displayName, latStr, r.Detail))
	}

	sb.WriteString(i18n.Tf(MsgDoctorSummary, passCount, warnCount, failCount))
	return sb.String()
}

