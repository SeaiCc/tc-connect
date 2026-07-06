package harness

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ConcurrentController 并发控制器
// 管理全局和项目级的并发限制
type ConcurrentController struct {
	mu              sync.RWMutex
	globalSemaphore chan struct{}          // 全局并发信号量
	projectPools    map[string]chan struct{} // 项目级并发池
	maxGlobal       int                    // 全局最大并发数
	maxPerProject   int                    // 每项目最大并发数
	stats           controllerStats        // 统计信息
	waitingQueue    []WaitingTask          // 等待队列（用于调试）
	// queueMu removed: using mu for both queue and stats
}

// WaitingTask 等待中的任务
type WaitingTask struct {
	ProjectName string
	AgentName   string
	StartTime   time.Time
	Ctx         context.Context
}

// controllerStats 并发控制器统计
type controllerStats struct {
	activeGlobal    int // 当前全局活跃数
	activeProjects  map[string]int // 各项目活跃数
	waitingCount    int // 等待数
	totalProcessed  int // 总处理数
	totalTimeouts   int // 总超时数
}

// ConcurrentControllerConfig 并发控制器配置
type ConcurrentControllerConfig struct {
	MaxGlobal     int // 全局最大并发数（默认 5）
	MaxPerProject int // 每项目最大并发数（默认 2）
	DefaultTimeout time.Duration // 默认等待超时（默认 30 秒）
}

// NewConcurrentController 创建并发控制器
func NewConcurrentController(cfg *ConcurrentControllerConfig) *ConcurrentController {
	if cfg == nil {
		cfg = &ConcurrentControllerConfig{
			MaxGlobal:     5,
			MaxPerProject: 2,
			DefaultTimeout: 30 * time.Second,
		}
	}

	// 设置合理的默认值
	if cfg.MaxGlobal <= 0 {
		cfg.MaxGlobal = 5
	}
	if cfg.MaxPerProject <= 0 {
		cfg.MaxPerProject = 2
	}
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = 30 * time.Second
	}

	controller := &ConcurrentController{
		globalSemaphore: make(chan struct{}, cfg.MaxGlobal),
		projectPools:    make(map[string]chan struct{}),
		maxGlobal:       cfg.MaxGlobal,
		maxPerProject:   cfg.MaxPerProject,
		stats: controllerStats{
			activeProjects: make(map[string]int),
		},
		waitingQueue: make([]WaitingTask, 0),
	}

	return controller
}

// Acquire 获取许可（带项目隔离）
// projectName: 项目标识（用于资源隔离）
// agentName: 子 Agent 名称（用于日志）
// timeout: 等待超时时间（0 表示使用默认值）
func (c *ConcurrentController) Acquire(ctx context.Context, projectName, agentName string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 30 * time.Second // 默认 30 秒
	}

	// 创建带超时的上下文
	acquireCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 记录到等待队列（用于监控）
	c.mu.Lock()
	task := WaitingTask{
		ProjectName: projectName,
		AgentName:   agentName,
		StartTime:   time.Now(),
		Ctx:         acquireCtx,
	}
	c.waitingQueue = append(c.waitingQueue, task)
	c.stats.waitingCount++
	c.mu.Unlock()

	// 清理函数：从队列中移除
	defer func() {
		c.mu.Lock()
		for i, t := range c.waitingQueue {
			if t.Ctx == acquireCtx {
				c.waitingQueue = append(c.waitingQueue[:i], c.waitingQueue[i+1:]...)
				c.stats.waitingCount--
				break
			}
		}
		c.mu.Unlock()
	}()

	// 第一步：获取全局许可
	if err := c.acquireGlobal(acquireCtx); err != nil {
		return fmt.Errorf("acquire global semaphore: %w", err)
	}

	// 第二步：获取项目级许可（资源隔离）
	if err := c.acquireProject(acquireCtx, projectName); err != nil {
		// 释放全局许可
		c.releaseGlobal()
		return fmt.Errorf("acquire project semaphore: %w", err)
	}

	// 更新统计
	c.mu.Lock()
	c.stats.activeGlobal++
	c.stats.activeProjects[projectName]++
	c.mu.Unlock()

	return nil
}

// acquireGlobal 获取全局许可
func (c *ConcurrentController) acquireGlobal(ctx context.Context) error {
	select {
	case c.globalSemaphore <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("timeout or cancelled: %w", ctx.Err())
	}
}

// acquireProject 获取项目级许可（自动创建项目池）
func (c *ConcurrentController) acquireProject(ctx context.Context, projectName string) error {
	if projectName == "" {
		projectName = "default" // 默认项目
	}

	// 获取或创建项目池（在持有锁的情况下）
	c.mu.Lock()
	pool, exists := c.projectPools[projectName]
	if !exists {
		// 创建新项目池
		pool = make(chan struct{}, c.maxPerProject)
		c.projectPools[projectName] = pool
	}
	c.mu.Unlock()

	select {
	case pool <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("timeout or cancelled: %w", ctx.Err())
	}
}

// Release 释放许可
func (c *ConcurrentController) Release(projectName string) {
	// 释放项目级许可
	c.releaseProject(projectName)

	// 释放全局许可
	c.releaseGlobal()

	// 更新统计
	c.mu.Lock()
	if c.stats.activeGlobal > 0 {
		c.stats.activeGlobal--
	}
	if c.stats.activeProjects[projectName] > 0 {
		c.stats.activeProjects[projectName]--
	}
	c.stats.totalProcessed++
	c.mu.Unlock()
}

// releaseGlobal 释放全局许可
func (c *ConcurrentController) releaseGlobal() {
	select {
	case <-c.globalSemaphore:
		// 释放成功
	default:
		// 通道为空，说明之前没有获取（异常情况）
	}
}

// releaseProject 释放项目级许可
func (c *ConcurrentController) releaseProject(projectName string) {
	if projectName == "" {
		return
	}

	c.mu.RLock()
	pool, exists := c.projectPools[projectName]
	c.mu.RUnlock()

	if !exists {
		return
	}

	select {
	case <-pool:
		// 释放成功
	default:
		// 通道为空
	}
}

// GetStats 获取统计信息
func (c *ConcurrentController) GetStats() map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// 复制 activeProjects 映射
	projectsCopy := make(map[string]int)
	for k, v := range c.stats.activeProjects {
		projectsCopy[k] = v
	}

	return map[string]any{
		"max_global":      c.maxGlobal,
		"max_per_project": c.maxPerProject,
		"active_global":   c.stats.activeGlobal,
		"active_projects": projectsCopy,
		"waiting_count":   c.stats.waitingCount,
		"total_processed": c.stats.totalProcessed,
		"total_timeouts":  c.stats.totalTimeouts,
		"utilization":     float64(c.stats.activeGlobal) / float64(c.maxGlobal),
	}
}

// GetWaitingQueue 获取等待队列（用于调试）
func (c *ConcurrentController) GetWaitingQueue() []WaitingTask {
	c.mu.RLock()
	defer c.mu.RUnlock()

	queue := make([]WaitingTask, len(c.waitingQueue))
	copy(queue, c.waitingQueue)
	return queue
}

// GetActiveCount 获取当前活跃数
// 返回全局信号量通道中的元素数量，代表当前活跃的任务数
// 使用通道长度而不是统计字段，避免竞态条件
func (c *ConcurrentController) GetActiveCount() int {
	return len(c.globalSemaphore)
}

// GetActiveProjects 获取活跃项目列表
func (c *ConcurrentController) GetActiveProjects() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	projects := make([]string, 0, len(c.stats.activeProjects))
	for proj, count := range c.stats.activeProjects {
		if count > 0 {
			projects = append(projects, proj)
		}
	}
	return projects
}

// IsAvailable 检查是否有可用许可（不阻塞）
// 注意：此方法仅用于检查，不实际获取许可
func (c *ConcurrentController) IsAvailable(projectName string) bool {
	// 检查全局是否有可用许可（通过检查通道长度）
	if len(c.globalSemaphore) >= c.maxGlobal {
		return false // 全局已满
	}

	// 检查项目级
	if projectName != "" {
		c.mu.RLock()
		pool, exists := c.projectPools[projectName]
		c.mu.RUnlock()

		if exists {
			// 项目池存在，检查是否已满
			if len(pool) >= c.maxPerProject {
				return false // 项目级已满
			}
		}
		// 项目池不存在，说明还没创建，肯定有可用许可
	}

	return true
}

// Cleanup 清理长时间未使用的项目池（内存管理）
func (c *ConcurrentController) Cleanup(inactiveThreshold time.Duration) int {
	if inactiveThreshold <= 0 {
		inactiveThreshold = 5 * time.Minute // 默认 5 分钟
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	removed := 0
	for projectName := range c.projectPools {
		// 检查项目是否还有活跃任务
		if c.stats.activeProjects[projectName] == 0 {
			// 关闭通道并删除（注意：不能实际 close 通道，因为可能有 goroutine 在等待）
			// 这里我们只删除映射，通道会被垃圾回收
			delete(c.projectPools, projectName)
			delete(c.stats.activeProjects, projectName)
			removed++
		}
	}

	return removed
}

// Reset 重置控制器（用于测试）
func (c *ConcurrentController) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 清空全局信号量（ drain ）
	for len(c.globalSemaphore) > 0 {
		select {
		case <-c.globalSemaphore:
		default:
		}
	}

	// 清空所有项目池
	for projectName := range c.projectPools {
		// drain 项目池
		for len(c.projectPools[projectName]) > 0 {
			select {
			case <-c.projectPools[projectName]:
			default:
			}
		}
		delete(c.projectPools, projectName)
	}

	// 重置统计
	c.stats = controllerStats{
		activeProjects: make(map[string]int),
	}
	c.waitingQueue = make([]WaitingTask, 0)
}

// Close 关闭控制器（释放资源）
func (c *ConcurrentController) Close() {
	c.Reset()
}
