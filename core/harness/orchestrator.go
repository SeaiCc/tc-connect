package harness

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"tc-connect/config"
)

// OrchestratorState 编排器状态
type OrchestratorState int

const (
	StateIdle    OrchestratorState = iota // 空闲
	StateRunning                          // 运行中
	StateError                            // 错误状态
	StateEnd                              // 已结束
)

func (s OrchestratorState) String() string {
	switch s {
	case StateIdle:
		return "IDLE"
	case StateRunning:
		return "RUNNING"
	case StateError:
		return "ERROR"
	case StateEnd:
		return "END"
	default:
		return "UNKNOWN"
	}
}

// Orchestrator 编排器接口
type Orchestrator interface {
	// Start 启动编排流程
	Start(ctx context.Context, input string) (*OrchestratorResult, error)
	// ExecuteSubAgent 执行单个子 Agent（外部调用）
	ExecuteSubAgent(req *HarnessRequest) (*SubAgentResult, error)
	// Stop 停止编排
	Stop() error
	// GetState 获取当前状态
	GetState() OrchestratorState
	// GetStats 获取统计信息
	GetStats() map[string]any
}

// OrchestratorImpl 编排器实现
type OrchestratorImpl struct {
	mu             sync.RWMutex
	state          OrchestratorState
	executor       *SubAgentExecutor           // 子 Agent 执行器
	sessionMgr     *SessionManager             // 会话管理器
	config         *config.HarnessConfig       // 配置
	concurrentCtrl *ConcurrentController       // 并发控制器
	stats          orchestratorStats           // 统计信息
	metrics        *Metrics                    // 监控指标
	resultChannel  chan orchestratorStepResult // 结果通道（用于流式输出）
	projectName    string                      // 项目名（用于资源隔离）
	logger         *HarnessLogger              // 日志器
}

// orchestratorStats 编排器统计信息
type orchestratorStats struct {
	totalSteps   int         // 总步数
	successSteps int         // 成功步数
	errorSteps   int         // 错误步数
	startTime    time.Time   // 开始时间
	endTime      time.Time   // 结束时间
	lastError    error       // 最后错误
	sessionIDs   []SessionID // 所有 session_id 列表
	outputs      []string    // 所有输出
}

// orchestratorStepResult 编排步骤结果
type orchestratorStepResult struct {
	Step      int           // 步骤序号
	Success   bool          // 是否成功
	AgentName string        // 子 Agent 名称
	Output    string        // 输出
	SessionID SessionID     // 会话 ID
	Error     error         // 错误
	Duration  time.Duration // 耗时
}

// OrchestratorResult 编排器最终结果
type OrchestratorResult struct {
	Success    bool          // 是否成功完成
	Steps      int           // 执行步数
	Duration   time.Duration // 总耗时
	SessionIDs []SessionID   // 所有会话 ID
	LastError  error         // 最后错误
	Outputs    []string      // 所有输出（可选，可能很大）
}

// NewOrchestrator 创建编排器
// 参数:
//   - cfg: 配置
//   - executor: 子 Agent 执行器
//   - sessionMgr: 会话管理器
//   - projectName: 项目名（用于资源隔离）
func NewOrchestrator(
	cfg *config.HarnessConfig,
	executor *SubAgentExecutor,
	sessionMgr *SessionManager,
	projectName string,
) *OrchestratorImpl {
	return NewOrchestratorWithLogger(cfg, executor, sessionMgr, projectName, nil)
}

// NewOrchestratorWithLogger 创建编排器（带日志器）
func NewOrchestratorWithLogger(
	cfg *config.HarnessConfig,
	executor *SubAgentExecutor,
	sessionMgr *SessionManager,
	projectName string,
	logger *HarnessLogger,
) *OrchestratorImpl {
	// 构建并发控制器配置
	ctrlCfg := &ConcurrentControllerConfig{
		MaxGlobal:      5,
		MaxPerProject:  2,
		DefaultTimeout: 30 * time.Second,
	}

	// 从配置中读取并发设置
	if cfg != nil {
		if cfg.MaxConcurrent != nil && *cfg.MaxConcurrent > 0 {
			ctrlCfg.MaxGlobal = *cfg.MaxConcurrent
		}
		if cfg.MaxPerProject != nil && *cfg.MaxPerProject > 0 {
			ctrlCfg.MaxPerProject = *cfg.MaxPerProject
		}
	}

	// 创建日志器配置（如果未提供 logger）
	if logger == nil {
		loggerCfg := &LoggerConfig{
			ProjectName: projectName,
			Level:       "info", // 默认 info
		}
		if cfg != nil {
			if cfg.LogLevel != "" {
				loggerCfg.Level = cfg.LogLevel
			}
			if cfg.LogOutputPath != "" {
				loggerCfg.OutputPath = cfg.LogOutputPath
			}
		}

		var err error
		logger, err = NewHarnessLogger(loggerCfg)
		if err != nil {
			// 如果日志器创建失败，使用默认 slog
			logger = &HarnessLogger{logger: slog.Default(), config: loggerCfg, projectName: projectName}
		}
	}

	orch := &OrchestratorImpl{
		state:          StateIdle,
		executor:       executor,
		sessionMgr:     sessionMgr,
		config:         cfg,
		concurrentCtrl: NewConcurrentController(ctrlCfg),
		metrics:        NewMetrics(),
		projectName:    projectName,
		logger:         logger,
		stats: orchestratorStats{
			sessionIDs: make([]SessionID, 0),
			outputs:    make([]string, 0),
		},
	}

	return orch
}

// Start 启动编排流程
// input: 主 Agent 的初始输出（可能是协议格式或普通文本）
func (o *OrchestratorImpl) Start(ctx context.Context, input string) (*OrchestratorResult, error) {
	// 检查上下文是否已取消
	if ctx.Err() != nil {
		return nil, fmt.Errorf("context already cancelled: %w", ctx.Err())
	}

	// 更新状态
	o.mu.Lock()
	if o.state != StateIdle && o.state != StateEnd {
		o.mu.Unlock()
		return nil, fmt.Errorf("orchestrator is not in IDLE state, current state: %s", o.state)
	}
	o.state = StateRunning
	o.stats.startTime = time.Now()
	o.stats.totalSteps = 0
	o.stats.successSteps = 0
	o.stats.errorSteps = 0
	o.stats.sessionIDs = make([]SessionID, 0)
	o.stats.outputs = make([]string, 0)
	o.mu.Unlock()

	// 初始化结果通道
	o.resultChannel = make(chan orchestratorStepResult, 100) // 缓冲 100 个结果
	defer close(o.resultChannel)

	// 记录编排开始日志
	o.logger.LogOrchestrationStart(input)

	// 主循环
	var stepCount int

	for {
		// 检查上下文
		if ctx.Err() != nil {
			return nil, fmt.Errorf("orchestration cancelled: %w", ctx.Err())
		}

		// 解析协议
		req, err := ParseProtocol(input)
		if err != nil {
			// 记录协议解析日志
			o.logger.LogProtocolParse(input, false, "", "", false, err)
			// 记录错误并设置状态
			o.recordError(stepCount+1, err)
			o.setErrorState(fmt.Errorf("step %d: parse protocol failed: %w", stepCount+1, err))
			return nil, fmt.Errorf("step %d: parse protocol failed: %w", stepCount+1, err)
		}

		// 检查 END 标识
		if req == nil {
			// 记录协议解析日志（END 情况）
			o.logger.LogProtocolParse(input, true, "", "", false, nil)
			// 正常结束
			o.setEndState()
			o.logger.LogOrchestrationEnd(true, stepCount, o.stats.sessionIDs, time.Since(o.stats.startTime), nil)
			return o.buildResult(nil)
		}

		// 记录协议解析成功日志
		if req != nil {
			o.logger.LogProtocolParse(input, true, req.AgentName, req.Params, req.Resume, nil)
		}

		stepCount++

		// 并发控制（带项目隔离）
		if o.concurrentCtrl != nil {
			// 从配置中获取超时时间
			timeout := 30 * time.Second
			if o.config != nil && o.config.TimeoutMins != nil && *o.config.TimeoutMins > 0 {
				timeout = time.Duration(*o.config.TimeoutMins) * time.Minute
			}

			// 记录获取许可前的时间
			acquireStart := time.Now()

			// 获取许可（可能阻塞）
			if err := o.concurrentCtrl.Acquire(ctx, o.projectName, req.AgentName, timeout); err != nil {
				// 记录错误并设置状态
				o.recordError(stepCount, err)
				o.setErrorState(fmt.Errorf("step %d: acquire concurrent lock failed: %w", stepCount, err))
				return o.buildResult(err)
			}

			// 记录并发控制日志
			o.logger.LogConcurrentAcquire(req.AgentName, o.projectName, time.Since(acquireStart))

			// 确保释放许可（防止 panic 导致许可泄漏）
			defer func() {
				o.concurrentCtrl.Release(o.projectName)
			}()
		}

		// 执行子 Agent
		stepStart := time.Now()
		result, execErr := o.executeWithStateProtection(ctx, req)
		stepDuration := time.Since(stepStart)

		// 处理执行结果
		if execErr != nil {
			// 记录错误
			o.recordError(stepCount, execErr)

			// 记录步骤日志
			o.logger.LogOrchestrationStep(stepCount, req.AgentName, false, stepDuration, execErr)

			// 发送错误结果到通道（用于流式处理）
			select {
			case o.resultChannel <- orchestratorStepResult{
				Step:      stepCount,
				Success:   false,
				AgentName: req.AgentName,
				Error:     execErr,
			}: // 发送成功
			default: // 通道满，跳过
			}

			// 错误时返回，不继续编排
			o.setErrorState(execErr)
			o.logger.LogOrchestrationEnd(false, stepCount, o.stats.sessionIDs, time.Since(o.stats.startTime), execErr)
			return o.buildResult(execErr)
		}

		// 成功执行
		o.recordSuccess(stepCount, result)

		// 记录步骤日志
		o.logger.LogOrchestrationStep(stepCount, req.AgentName, true, stepDuration, nil)

		// 发送成功结果到通道
		select {
		case o.resultChannel <- orchestratorStepResult{
			Step:      stepCount,
			Success:   true,
			AgentName: req.AgentName,
			Output:    result.Output,
			SessionID: result.SessionID,
			Duration:  result.Duration,
		}: // 发送成功
		default: // 通道满，跳过
		}

		// 将输出返回给主 Agent，等待下一步指令
		// 这里我们假设输入是下一步的指令（在实际集成中，需要从主 Agent 获取）
		// 为了演示，我们在这里等待新的输入
		input, err = o.waitForNextInput(ctx, result.Output)
		if err != nil {
			return nil, fmt.Errorf("step %d: wait for next input failed: %w", stepCount, err)
		}
	}
}

// executeWithStateProtection 执行子 Agent（带状态保护）
func (o *OrchestratorImpl) executeWithStateProtection(ctx context.Context, req *HarnessRequest) (*SubAgentResult, error) {
	// 创建带超时的上下文（如果配置了）
	execCtx := ctx
	if o.config != nil && o.config.TimeoutMins != nil && *o.config.TimeoutMins > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, time.Duration(*o.config.TimeoutMins)*time.Minute)
		defer cancel()
	}

	// 执行
	return o.executor.Execute(execCtx, req, "")
}

// waitForNextInput 等待下一个输入
// 在实际实现中，这里应该从主 Agent 获取下一步输出
// 当前实现为占位符，直接返回空字符串（表示结束）
func (o *OrchestratorImpl) waitForNextInput(ctx context.Context, lastOutput string) (string, error) {
	// TODO: 实现与主 Agent 的交互逻辑
	// 当前版本：返回 "END" 表示编排结束
	return "END", nil
}

// ExecuteSubAgent 执行单个子 Agent（外部调用，不进入主循环）
func (o *OrchestratorImpl) ExecuteSubAgent(req *HarnessRequest) (*SubAgentResult, error) {
	if req == nil {
		return nil, fmt.Errorf("harness request is nil")
	}

	// 执行
	result, err := o.executor.Execute(context.Background(), req, "")
	if err != nil {
		return nil, err
	}

	// 记录统计信息
	o.mu.Lock()
	o.stats.sessionIDs = append(o.stats.sessionIDs, result.SessionID)
	o.mu.Unlock()

	return result, nil
}

// recordSuccess 记录成功步骤
func (o *OrchestratorImpl) recordSuccess(step int, result *SubAgentResult) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.stats.successSteps++
	o.stats.sessionIDs = append(o.stats.sessionIDs, result.SessionID)
	o.stats.outputs = append(o.stats.outputs, result.Output)

	// 记录监控指标
	if o.metrics != nil {
		o.metrics.RecordSuccess(result.AgentName, result.Duration, result.RetryCount)
	}
}

// recordError 记录错误步骤
func (o *OrchestratorImpl) recordError(step int, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.stats.errorSteps++
	o.stats.lastError = err

	// 注意：这里不记录监控指标，因为具体的 agent 名称和时长信息
	// 在 orchestrator 层面无法获取，应该在 executor 层面记录
	// 或者通过扩展此方法传递更多信息
}

// setEndState 设置为结束状态
func (o *OrchestratorImpl) setEndState() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.state = StateEnd
	o.stats.endTime = time.Now()
}

// setErrorState 设置为错误状态
func (o *OrchestratorImpl) setErrorState(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.state = StateError
	o.stats.lastError = err
	o.stats.endTime = time.Now()
}

// buildResult 构建最终结果
func (o *OrchestratorImpl) buildResult(err error) (*OrchestratorResult, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()

	// 记录最终日志
	if o.state == StateEnd {
		o.logger.LogOrchestrationEnd(true, o.stats.successSteps, o.stats.sessionIDs, o.stats.endTime.Sub(o.stats.startTime), nil)
	} else if o.state == StateError {
		o.logger.LogOrchestrationEnd(false, o.stats.successSteps, o.stats.sessionIDs, o.stats.endTime.Sub(o.stats.startTime), o.stats.lastError)
	}

	result := &OrchestratorResult{
		Success:    o.state == StateEnd,
		Steps:      o.stats.successSteps,
		Duration:   o.stats.endTime.Sub(o.stats.startTime),
		SessionIDs: o.stats.sessionIDs,
		LastError:  o.stats.lastError,
		Outputs:    o.stats.outputs,
	}

	return result, err
}

// Stop 停止编排
func (o *OrchestratorImpl) Stop() error {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.state == StateIdle || o.state == StateEnd || o.state == StateError {
		return nil // 已经停止
	}

	// 强制设置为错误状态
	o.state = StateError
	o.stats.endTime = time.Now()
	o.stats.lastError = fmt.Errorf("orchestrator stopped manually")

	return nil
}

// GetState 获取当前状态
func (o *OrchestratorImpl) GetState() OrchestratorState {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.state
}

// GetStats 获取统计信息
func (o *OrchestratorImpl) GetStats() map[string]any {
	o.mu.RLock()
	defer o.mu.RUnlock()

	stats := map[string]any{
		"state":         o.state.String(),
		"total_steps":   o.stats.totalSteps,
		"success_steps": o.stats.successSteps,
		"error_steps":   o.stats.errorSteps,
		"start_time":    o.stats.startTime,
		"end_time":      o.stats.endTime,
		"duration":      o.stats.endTime.Sub(o.stats.startTime),
		"session_ids":   o.stats.sessionIDs,
		"last_error":    o.stats.lastError,
		"project_name":  o.projectName,
	}

	// 添加并发控制器统计
	if o.concurrentCtrl != nil {
		stats["concurrent"] = o.concurrentCtrl.GetStats()
	}

	// 添加监控指标统计
	if o.metrics != nil {
		stats["metrics"] = map[string]any{
			"global": o.metrics.GetGlobalStats(),
			"agents": o.metrics.GetAllAgentStats(),
		}
	}

	return stats
}

// GetResultChannel 获取结果通道（用于流式处理）
func (o *OrchestratorImpl) GetResultChannel() <-chan orchestratorStepResult {
	return o.resultChannel
}

// Reset 重置编排器状态（用于测试）
func (o *OrchestratorImpl) Reset() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.state = StateIdle
	o.stats = orchestratorStats{
		sessionIDs: make([]SessionID, 0),
		outputs:    make([]string, 0),
	}
	// 重置并发控制器
	if o.concurrentCtrl != nil {
		o.concurrentCtrl.Reset()
	}
}
