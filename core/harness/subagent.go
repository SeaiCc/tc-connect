package harness

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"tc-connect/config"
)

// SubAgentManager 管理子 Agent 的验证和配置
type SubAgentManager struct {
	mu           sync.RWMutex
	available    map[string]bool      // 已注册的子 Agent
	aliases      map[string]string    // 别名映射
	agentConfigs map[string]AgentMeta // 子 Agent 配置元数据
	cliPath      string               // opencode CLI 路径
}

// AgentMeta 子 Agent 元数据
type AgentMeta struct {
	Name        string // 真实名称
	Description string // 描述
	TimeoutMins int    // 超时时间（分钟）
	MaxRetries  int    // 最大重试次数
	WorkDir     string // 工作目录
}

// SubAgentResult 子 Agent 执行结果
type SubAgentResult struct {
	Output     string    // 子 Agent 执行输出
	AgentName  string    // 子 Agent 名称（用于监控指标记录）
	SessionID  SessionID // 本次执行的 session_id，用于溯源
	Error      error     // 错误信息
	Duration   time.Duration // 执行耗时
	RetryCount int         // 实际重试次数
}

// SubAgentExecutor 子 Agent 执行器
type SubAgentExecutor struct {
	manager     *SubAgentManager // 子 Agent 管理器
	sessionMgr  *SessionManager  // 会话管理器
	defaultTimeout time.Duration // 默认超时时间
	defaultRetries int          // 默认重试次数
	logger      *HarnessLogger  // 日志器
	metrics     *Metrics        // 监控指标（可选）
}

// NewSubAgentManager 创建子 Agent 管理器
// 参数:
//   - cfg: 全局配置
//   - cliPath: opencode CLI 路径（可选，空则使用环境变量中的 opencode）
func NewSubAgentManager(cfg *config.HarnessConfig, cliPath string) (*SubAgentManager, error) {
	manager := &SubAgentManager{
		available:    make(map[string]bool),
		aliases:      make(map[string]string),
		agentConfigs: make(map[string]AgentMeta),
		cliPath:      cliPath,
	}

	// 加载配置
	if err := manager.loadConfig(cfg); err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	// 扫描已注册的子 Agent
	if err := manager.scanAvailableAgents(); err != nil {
		return nil, fmt.Errorf("scan agents: %w", err)
	}

	return manager, nil
}

// loadConfig 加载子 Agent 配置
func (m *SubAgentManager) loadConfig(cfg *config.HarnessConfig) error {
	if cfg == nil {
		return nil
	}

	// 加载别名映射
	if cfg.Aliases != nil {
		for alias, realName := range cfg.Aliases {
			if alias == "" || realName == "" {
				continue
			}
			m.aliases[strings.ToLower(alias)] = realName
		}
	}

	// 加载子 Agent 独立配置
	if cfg.Agents != nil {
		for name, agentCfg := range cfg.Agents {
			timeout := 5 // 默认 5 分钟
			if agentCfg.TimeoutMins != nil {
				timeout = *agentCfg.TimeoutMins
			}
			retries := 3 // 默认 3 次
			if agentCfg.MaxRetries != nil {
				retries = *agentCfg.MaxRetries
			}

			m.agentConfigs[name] = AgentMeta{
				Name:        name,
				Description: agentCfg.Description,
				TimeoutMins: timeout,
				MaxRetries:  retries,
				WorkDir:     agentCfg.WorkDir,
			}
		}
	}

	return nil
}

// scanAvailableAgents 扫描 opencode 已注册的子 Agent
// 通过执行 `opencode agent list` 获取所有可用 Agent
// 输出格式：第一行是 agent 名称和类型，后续行是 JSON 权限列表
func (m *SubAgentManager) scanAvailableAgents() error {
	cmd := m.getOpencodeCmd("agent", "list")
	if cmd == nil {
		return fmt.Errorf("opencode CLI not found")
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("execute 'opencode agent list': %w", err)
	}

	// 解析输出，格式为：
	// agent_name (type)
	// [
	// {permissions...}
	// ]
	// agent_name2 ...
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		// 跳过 JSON 行（以 [ { } ] 开头）
		if line[0] == '[' || line[0] == '{' || line[0] == '}' || line[0] == ']' {
			continue
		}

		// 提取 agent 名称（行首到第一个空格）
		// 格式：agent_name (type) 或 agent_name [permissions...]
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}

		agentName := parts[0]
		// 移除可能的末尾括号
		agentName = strings.TrimRight(agentName, "()[]")

		// 验证是否是有效的 agent 名称格式（避免误识别）
		if isValidAgentNameFormat(agentName) && agentName != "" {
			m.available[agentName] = true
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan agent list: %w", err)
	}

	return nil
}

// isValidAgentNameFormat 验证字符串是否为有效的 agent 名称格式
func isValidAgentNameFormat(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i, c := range s {
		if i == 0 {
			// 首字符必须是字母或下划线
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_') {
				return false
			}
		} else {
			// 后续字符可以是字母、数字、下划线、连字符
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
				return false
			}
		}
	}
	return true
}

// getOpencodeCmd 获取 opencode 命令
func (m *SubAgentManager) getOpencodeCmd(args ...string) *exec.Cmd {
	cmd := "opencode"
	if m.cliPath != "" {
		cmd = m.cliPath
	}
	return exec.Command(cmd, args...)
}

// IsSubAgentAvailable 检查子 Agent 是否可用
// 支持别名，会先解析别名再检查
func (m *SubAgentManager) IsSubAgentAvailable(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// 先检查别名
	lowerName := strings.ToLower(name)
	if realName, ok := m.aliases[lowerName]; ok {
		name = realName
	}

	// 检查是否在已注册列表中
	_, exists := m.available[name]
	return exists
}

// ResolveAgentName 解析 Agent 名称（支持别名）
// 返回真实名称和是否找到
func (m *SubAgentManager) ResolveAgentName(name string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// 检查别名
	lowerName := strings.ToLower(name)
	if realName, ok := m.aliases[lowerName]; ok {
		return realName, true
	}

	// 直接返回原名称（可能不存在，调用方需要检查）
	return name, false
}

// GetAgentConfig 获取子 Agent 配置
// 如果未配置，返回默认配置
func (m *SubAgentManager) GetAgentConfig(name string) AgentMeta {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// 先解析别名
	if realName, ok := m.aliases[strings.ToLower(name)]; ok {
		name = realName
	}

	// 查找配置
	if meta, ok := m.agentConfigs[name]; ok {
		return meta
	}

	// 返回默认配置
	return AgentMeta{
		Name:        name,
		TimeoutMins: 5, // 默认 5 分钟
		MaxRetries:  3, // 默认 3 次
	}
}

// ListAvailableAgents 返回所有可用的子 Agent 名称列表
func (m *SubAgentManager) ListAvailableAgents() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	agents := make([]string, 0, len(m.available))
	for name := range m.available {
		agents = append(agents, name)
	}
	return agents
}

// RefreshAgentList 刷新子 Agent 列表（重新扫描）
func (m *SubAgentManager) RefreshAgentList() error {
	return m.scanAvailableAgents()
}

// ReloadConfig 重新加载配置（支持运行时更新）
func (m *SubAgentManager) ReloadConfig(cfg *config.HarnessConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.loadConfig(cfg)
}

// GetAvailableCount 返回可用子 Agent 数量
func (m *SubAgentManager) GetAvailableCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.available)
}

// ValidateAgentName 验证 Agent 名称格式（仅格式检查，不检查是否存在）
// 规则：只能包含字母、数字、下划线、连字符，不能以数字开头
func (m *SubAgentManager) ValidateAgentName(name string) error {
	if name == "" {
		return fmt.Errorf("agent name cannot be empty")
	}

	if len(name) > 64 {
		return fmt.Errorf("agent name too long (max 64 chars)")
	}

	// 检查首字符（必须是字母或下划线）
	first := name[0]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || first == '_') {
		return fmt.Errorf("agent name must start with a letter or underscore, got: %c", first)
	}

	// 检查后续字符（只允许字母、数字、下划线、连字符）
	for i := 1; i < len(name); i++ {
		c := name[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return fmt.Errorf("agent name contains invalid character at position %d: %c", i, c)
		}
	}

	return nil
}

// WithContext 返回一个带上下文的验证方法（用于超时控制）
func (m *SubAgentManager) IsSubAgentAvailableWithContext(ctx context.Context, name string) (bool, error) {
	// 创建一个完成通道
	done := make(chan bool, 1)
	go func() {
		result := m.IsSubAgentAvailable(name)
		done <- result
	}()

	select {
	case result := <-done:
		return result, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// NewSubAgentExecutor 创建子 Agent 执行器
// 参数:
//   - manager: 子 Agent 管理器（用于获取配置和验证）
//   - sessionMgr: 会话管理器（用于创建和更新会话）
//   - defaultTimeout: 默认超时时间（秒），0 表示使用配置中的默认值（5 分钟）
//   - defaultRetries: 默认重试次数，0 表示使用配置中的默认值（3 次）
//   - logger: 日志器（可选，nil 则使用默认 slog）
//   - metrics: 监控指标收集器（可选，nil 则不记录监控指标）
func NewSubAgentExecutor(
	manager *SubAgentManager,
	sessionMgr *SessionManager,
	defaultTimeout time.Duration,
	defaultRetries int,
	logger *HarnessLogger,
	metrics *Metrics,
) *SubAgentExecutor {
	executor := &SubAgentExecutor{
		manager:      manager,
		sessionMgr:   sessionMgr,
		defaultTimeout: defaultTimeout,
		defaultRetries: defaultRetries,
		logger:      logger,
		metrics:     metrics,
	}

	// 设置默认值
	if executor.defaultTimeout == 0 {
		executor.defaultTimeout = 5 * time.Minute
	}
	if executor.defaultRetries == 0 {
		executor.defaultRetries = 3
	}

	// 如果 logger 为 nil，创建一个默认的
	if executor.logger == nil {
		executor.logger = &HarnessLogger{
			logger: slog.Default(),
			config: &LoggerConfig{Level: "info"},
		}
	}

	return executor
}

// Execute 执行子 Agent
// 参数:
//   - ctx: 上下文（用于超时控制）
//   - req: 编排请求（包含 agent_name, params, resume）
//   - workDir: 工作目录（可选，空则使用 agent 配置中的 work_dir）
//
// 返回:
//   - *SubAgentResult: 执行结果，包含输出、session_id、错误等
//   - error: 执行过程中的错误
func (e *SubAgentExecutor) Execute(ctx context.Context, req *HarnessRequest, workDir string) (*SubAgentResult, error) {
	if req == nil {
		return nil, fmt.Errorf("harness request is nil")
	}

	if req.AgentName == "" {
		return nil, fmt.Errorf("agent name is empty")
	}

	// 验证子 Agent 是否可用
	if !e.manager.IsSubAgentAvailable(req.AgentName) {
		return nil, fmt.Errorf("sub-agent %q is not available", req.AgentName)
	}

	// 获取子 Agent 配置
	meta := e.manager.GetAgentConfig(req.AgentName)

	// 确定超时时间（优先级：上下文 > 配置 > 默认）
	timeout := e.defaultTimeout
	if meta.TimeoutMins > 0 {
		timeout = time.Duration(meta.TimeoutMins) * time.Minute
	}
	if ctx != nil {
		// 检查上下文是否有超时设置
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining < timeout {
				timeout = remaining
			}
		}
	}

	// 确定重试次数
	maxRetries := e.defaultRetries
	if meta.MaxRetries > 0 {
		maxRetries = meta.MaxRetries
	}

	// 确定工作目录
	finalWorkDir := workDir
	if finalWorkDir == "" && meta.WorkDir != "" {
		finalWorkDir = meta.WorkDir
	}

		// 创建会话
		sessionID, err := e.sessionMgr.CreateSession(req.AgentName, req.Params, req.Resume)
		if err != nil {
			return nil, fmt.Errorf("create session: %w", err)
		}

		// 记录子 Agent 启动日志
		if e.logger != nil {
			e.logger.LogSubAgentStart(req.AgentName, req.Params, req.Resume, sessionID)
		}

		// 执行子 Agent（带重试）
		result, lastErr := e.executeWithRetry(ctx, req, sessionID, finalWorkDir, timeout, maxRetries)
		
		// 设置 AgentName（用于监控指标记录）
		if result != nil {
			result.AgentName = req.AgentName
		}

		// 记录监控指标
		if e.metrics != nil {
			if lastErr == nil {
				// 成功
				e.metrics.RecordSuccess(req.AgentName, result.Duration, result.RetryCount)
			} else {
				// 失败，判断是否超时
				isTimeout := strings.Contains(lastErr.Error(), "context deadline exceeded") ||
							  strings.Contains(lastErr.Error(), "timeout")
				e.metrics.RecordFailure(req.AgentName, result.Duration, isTimeout, result.RetryCount)
			}
		}

		// 记录执行完成日志
		if e.logger != nil {
			if lastErr == nil {
				e.logger.LogSubAgentComplete(req.AgentName, sessionID, len(result.Output), result.Duration, true, nil)
			} else {
				e.logger.LogSubAgentComplete(req.AgentName, sessionID, 0, result.Duration, false, lastErr)
			}
		}

	// 更新会话信息（获取实际 session_id）
	if updateErr := e.sessionMgr.UpdateSessionAfterExecution(
		sessionID,
		req.AgentName,
		result.Output,
		lastErr,
	); updateErr != nil {
		// 记录警告但不影响返回结果
		// 这里可以添加 slog.Warn 调用
	} else {
		// 获取更新后的实际 session_id 并设置到结果中
		actualSession, getErr := e.sessionMgr.GetSession(sessionID)
		if getErr == nil && actualSession != nil {
			result.SessionID = actualSession.ID
		}
	}

	return result, lastErr
}

// executeWithRetry 执行子 Agent（带重试逻辑）
func (e *SubAgentExecutor) executeWithRetry(
	ctx context.Context,
	req *HarnessRequest,
	sessionID SessionID,
	workDir string,
	timeout time.Duration,
	maxRetries int,
) (*SubAgentResult, error) {
	var lastErr error
	var lastResult *SubAgentResult

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// 重试前短暂等待（指数退避）
			backoff := time.Duration(attempt) * time.Second
			time.Sleep(backoff)

			// 记录重试日志
			if e.logger != nil {
				e.logger.LogSubAgentRetry(req.AgentName, attempt+1, maxRetries+1, lastErr)
			}
		}

		// 创建带超时的上下文
		execCtx := ctx
		if timeout > 0 {
			var cancel context.CancelFunc
			execCtx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}

		// 执行单次尝试
		result, err := e.executeOnce(execCtx, req, sessionID, workDir)
		lastResult = result
		lastErr = err

		if err == nil {
			// 成功，记录重试次数
			result.RetryCount = attempt
			return result, nil
		}

		// 检查是否应该继续重试
		if !e.shouldRetry(err) {
			// 不可重试的错误，直接返回
			if result == nil {
				result = &SubAgentResult{}
			}
			result.RetryCount = attempt
			return result, err
		}
	}

	// 所有重试都失败
	if lastResult == nil {
		lastResult = &SubAgentResult{}
	}
	lastResult.RetryCount = maxRetries
	return lastResult, fmt.Errorf("all %d attempts failed: %w", maxRetries+1, lastErr)
}

// executeOnce 执行单次子 Agent 调用
func (e *SubAgentExecutor) executeOnce(
	ctx context.Context,
	req *HarnessRequest,
	sessionID SessionID,
	workDir string,
) (*SubAgentResult, error) {
	start := time.Now()

	// 构建命令
	cmd := e.buildCommand(req, workDir)
	if cmd == nil {
		return nil, fmt.Errorf("opencode CLI not found")
	}

	// 设置工作目录
	if workDir != "" {
		cmd.Dir = workDir
	}

	// 捕获输出
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	// 启动进程
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start process: %w", err)
	}

	// 等待完成的通道
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	// 等待完成或超时
	var err error
	select {
	case err = <-done:
		// 正常完成
	case <-ctx.Done():
		// 超时或取消，杀死进程
		e.killProcess(cmd)
		// 等待 done 通道，确保 cmd.Wait() 完成，这样输出 buffer 才会停止写入
		err = <-done
		// 使用原始超时错误
		err = ctx.Err()
	}

	duration := time.Since(start)

	// 组合输出（注意：必须在 done 通道返回后才能安全读取，因为 exec 包的 goroutine 可能仍在写入）
	output := stdoutBuf.String()
	if stderrBuf.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += stderrBuf.String()
	}

	result := &SubAgentResult{
		Output:   output,
		Duration: duration,
	}

	if err != nil {
		return result, fmt.Errorf("execution failed: %w, output: %s", err, output)
	}

	// 成功执行后，获取 session_id
	// 注意：这里我们依赖 SessionManager 在 Execute 方法中调用 UpdateSessionAfterExecution
	// 来获取实际的 session_id，因此这里不直接设置 result.SessionID

	return result, nil
}

// buildCommand 构建 opencode 命令
// 注意：工作目录通过 exec.Cmd.Dir 设置，不通过 --work-dir 参数传递
func (e *SubAgentExecutor) buildCommand(req *HarnessRequest, workDir string) *exec.Cmd {
	// 基础命令：opencode run --agent <name>
	args := []string{"run", "--agent", req.AgentName}

	// 添加参数（如果有）
	if req.Params != "" {
		args = append(args, req.Params)
	}

	cmd := e.manager.getOpencodeCmd(args...)
	
	// 设置工作目录（如果有）
	if workDir != "" {
		cmd.Dir = workDir
	}

	return cmd
}

// killProcess 杀死进程（先 SIGTERM，后 SIGKILL）
// 注意：此方法不阻塞等待进程退出，只负责发送信号。调用者需要等待 done 通道来确保进程完全退出。
func (e *SubAgentExecutor) killProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}

	// 先发送 SIGTERM
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		// 如果 SIGTERM 失败，直接 SIGKILL
		e.forceKillProcess(cmd)
		return
	}

	// 给进程 200ms 时间优雅退出，如果超时则强制杀死
	time.Sleep(200 * time.Millisecond)
	
	// 检查进程是否还在运行
	if checkErr := cmd.Process.Signal(syscall.Signal(0)); checkErr == nil {
		// 进程仍在运行，强制杀死
		if err := cmd.Process.Kill(); err != nil {
			// 如果 Kill 失败，尝试 SIGKILL
			cmd.Process.Signal(syscall.SIGKILL)
		}
		// 等待进程退出（避免僵尸进程），带超时，忽略错误（可能已由其他 goroutine 等待）
		go func() {
			cmd.Wait()
		}()
		time.Sleep(500 * time.Millisecond)
	}
}

// forceKillProcess 强制杀死进程
func (e *SubAgentExecutor) forceKillProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}

	if err := cmd.Process.Kill(); err != nil {
		// 记录错误但不返回
	}
}

// shouldRetry 判断是否应该重试
func (e *SubAgentExecutor) shouldRetry(err error) bool {
	if err == nil {
		return false
	}

	errStr := err.Error()

	// 以下情况不重试
	if strings.Contains(errStr, "not available") {
		return false // Agent 不可用
	}
	if strings.Contains(errStr, "not found") {
		return false // 文件/命令未找到
	}
	if strings.Contains(errStr, "permission") {
		return false // 权限错误
	}
	if strings.Contains(errStr, "invalid") {
		return false // 无效参数
	}

	// 以下情况重试
	if strings.Contains(errStr, "context deadline exceeded") {
		return true // 超时
	}
	if strings.Contains(errStr, "connection") {
		return true // 连接错误
	}
	if strings.Contains(errStr, "timeout") {
		return true // 超时
	}

	// 默认重试（其他错误）
	return true
}

// GetAgentInfo 获取子 Agent 信息（用于调试）
func (e *SubAgentExecutor) GetAgentInfo(name string) (AgentMeta, bool) {
	if !e.manager.IsSubAgentAvailable(name) {
		return AgentMeta{}, false
	}
	return e.manager.GetAgentConfig(name), true
}

// ListAgents 列出所有可用的子 Agent
func (e *SubAgentExecutor) ListAgents() []string {
	return e.manager.ListAvailableAgents()
}
