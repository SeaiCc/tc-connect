package harness

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"tc-connect/config"
)

// SessionID 会话 ID 类型
type SessionID string

// Session 会话结构
type Session struct {
	ID            SessionID    // 会话 ID (ses_xxx)
	AgentName     string      // 子 Agent 名称
	CreatedAt     time.Time   // 创建时间
	LastUsedAt    time.Time   // 最后使用时间
	Params        string      // 执行参数
	Output        string      // 执行输出（可选存储）
	Error         error       // 错误信息
	ResumedFrom   SessionID   // 如果 resume=true，记录来源会话 ID
	Metadata      map[string]any // 扩展元数据
}

// SessionStore 会话存储接口
type SessionStore interface {
	// Create 创建新会话
	Create(agentName, params string, resumedFrom SessionID) (SessionID, error)
	// Get 获取会话
	Get(sessionID SessionID) (*Session, error)
	// UpdateLastUsed 更新最后使用时间（用于 TTL 刷新）
	UpdateLastUsed(sessionID SessionID) error
	// Delete 删除会话
	Delete(sessionID SessionID) error
	// Cleanup 清理过期会话
	Cleanup(before time.Time) (int, error)
	// List 列出会话（分页）
	List(limit, offset int, agentName string) ([]*Session, error)
	// GetLatest 获取指定 agent 的最新会话
	GetLatest(agentName string) (*Session, error)
	// UpdateSessionID 更新会话的实际 session_id
	UpdateSessionID(oldID, newID SessionID, agentName, output string, err error) error
}

// MemorySessionStore 内存会话存储实现
type MemorySessionStore struct {
	mu       sync.RWMutex
	store    map[SessionID]*Session       // 会话存储
	latest   map[string]SessionID         // agent_name -> 最新 session_id 索引
	ttlMins  int                          // 会话过期时间（分钟）
	maxSize  int                          // 最大存储数量
	cliPath  string                       // opencode CLI 路径
	nowFunc  func() time.Time             // 时间函数（用于测试）
	cleanup  context.Context              // 清理上下文
	cancel   context.CancelFunc           // 取消函数
	cleanupMu sync.Mutex                  // 清理守护进程锁
	logger   *HarnessLogger               // 日志器
}

// SessionStoreConfig 会话存储配置
type SessionStoreConfig struct {
	TTLMinutes int    // 会话过期时间（分钟），0 表示永不过期
	MaxSize    int    // 最大存储数量，0 表示无限制
	CLIPath    string // opencode CLI 路径
	Logger     *HarnessLogger // 日志器（可选）
	ProjectName string       // 项目名称（用于日志）
}

// NewMemorySessionStore 创建内存会话存储
func NewMemorySessionStore(cfg *SessionStoreConfig) *MemorySessionStore {
	store := &MemorySessionStore{
		store:  make(map[SessionID]*Session),
		latest: make(map[string]SessionID),
		cliPath: cfg.CLIPath,
		nowFunc: time.Now,
		logger:  cfg.Logger,
	}

	// 设置 TTL（默认 60 分钟）
	if cfg.TTLMinutes > 0 {
		store.ttlMins = cfg.TTLMinutes
	} else {
		store.ttlMins = 60
	}

	// 设置最大容量（默认 1000）
	if cfg.MaxSize > 0 {
		store.maxSize = cfg.MaxSize
	}

	// 确保 nowFunc 不为 nil
	if store.nowFunc == nil {
		store.nowFunc = time.Now
	}

	// 如果 logger 为 nil，创建一个默认的
	if store.logger == nil {
		store.logger = &HarnessLogger{
			logger: slog.Default(),
			config: &LoggerConfig{Level: "info", ProjectName: cfg.ProjectName},
		}
	}

	// 启动清理守护进程
	store.startCleanupDaemon()

	return store
}

// startCleanupDaemon 启动后台清理守护进程
func (s *MemorySessionStore) startCleanupDaemon() {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()

	if s.cleanup != nil {
		return // 已启动
	}

	s.cleanup, s.cancel = context.WithCancel(context.Background())
	go s.cleanupWorker()
}

// cleanupWorker 清理工作协程
func (s *MemorySessionStore) cleanupWorker() {
	if s.ttlMins <= 0 {
		return // 永不过期，不启动清理
	}

	// 加锁检查并获取 cleanup 上下文，避免与 Shutdown() 的竞态条件
	s.cleanupMu.Lock()
	if s.cleanup == nil {
		s.cleanupMu.Unlock()
		return // 上下文未初始化，退出
	}
	cleanupCtx := s.cleanup
	s.cleanupMu.Unlock()

	// 保存上下文引用到局部变量，避免后续竞态条件

	// 每 5 分钟清理一次（或 TTL 的 1/4，取较小值）
	interval := time.Duration(5) * time.Minute
	if s.ttlMins > 0 && s.ttlMins/4 < 5 {
		interval = time.Duration(s.ttlMins/4) * time.Minute
	}
	if interval < time.Minute {
		interval = time.Minute
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-cleanupCtx.Done():
			return
		case <-ticker.C:
			// 清理过期会话（超过 TTL）
			nowFunc := s.nowFunc
			if nowFunc == nil {
				continue
			}
			now := nowFunc()
			cutoff := now.Add(-time.Duration(s.ttlMins) * time.Minute)
			count, err := s.Cleanup(cutoff)
			if err != nil {
				// 记录错误（这里可以添加 slog 调用）
				continue
			}
			if count > 0 {
				// 记录日志（这里可以添加 slog 调用）
			}
		}
	}
}

// Create 创建新会话
// agentName: 子 Agent 名称
// params: 执行参数
// resumedFrom: 如果从旧会话 resume，传入原 session_id
func (s *MemorySessionStore) Create(agentName, params string, resumedFrom SessionID) (SessionID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 检查容量限制
	if s.maxSize > 0 && len(s.store) >= s.maxSize {
		// 删除最旧的会话
		if deleted, err := s.deleteOldest(); err != nil {
			return "", fmt.Errorf("cleanup old session: %w", err)
		} else {
			// 记录清理信息
			_ = deleted
		}
	}

	// 生成 session_id（占位符，实际执行后会更新）
	generatedID := s.generateSessionID(agentName)

	// 创建会话记录
	now := s.nowFunc()
	session := &Session{
		ID:        generatedID,
		AgentName: agentName,
		CreatedAt: now,
		LastUsedAt: now,
		Params:    params,
		ResumedFrom: resumedFrom,
		Metadata:  make(map[string]any),
	}

	// 存储
	s.store[generatedID] = session

	// 更新 latest 索引
	s.latest[agentName] = generatedID

	// 记录日志
	if s.logger != nil {
		s.logger.LogSessionCreated(agentName, generatedID, resumedFrom)
	}

	return generatedID, nil
}

// generateSessionID 生成临时 session_id 占位符
// 实际 session_id 在执行后会通过 opencode session list 获取并更新
func (s *MemorySessionStore) generateSessionID(agentName string) SessionID {
	// 格式：tmp_agentname_timestamp_random
	return SessionID(fmt.Sprintf("tmp_%s_%d", agentName, time.Now().UnixNano()))
}

// Get 获取会话
func (s *MemorySessionStore) Get(sessionID SessionID) (*Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	session, ok := s.store[sessionID]
	if !ok {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}

	// 返回副本（避免并发修改）
	return s.copySession(session), nil
}

// UpdateLastUsed 更新最后使用时间
func (s *MemorySessionStore) UpdateLastUsed(sessionID SessionID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	session, ok := s.store[sessionID]
	if !ok {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	session.LastUsedAt = s.nowFunc()
	return nil
}

// Delete 删除会话
func (s *MemorySessionStore) Delete(sessionID SessionID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.store[sessionID]; !ok {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	delete(s.store, sessionID)

	// 清理 latest 索引（如果指向已删除的 session）
	for agentName, latestID := range s.latest {
		if latestID == sessionID {
			// 找到该 agent 的最新有效 session
			var newLatest SessionID
			for _, sess := range s.store {
				if sess.AgentName == agentName && sess.CreatedAt.After(s.nowFunc().Add(-time.Hour*24*30)) {
					if newLatest == "" || sess.CreatedAt.After(s.store[newLatest].CreatedAt) {
						newLatest = sess.ID
					}
				}
			}
			s.latest[agentName] = newLatest
			break
		}
	}

	return nil
}

// Cleanup 清理过期会话
// before: 清理该时间之前创建的所有会话
func (s *MemorySessionStore) Cleanup(before time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var deleted []SessionID
	for id, session := range s.store {
		if session.CreatedAt.Before(before) {
			deleted = append(deleted, id)
		}
	}

	for _, id := range deleted {
		delete(s.store, id)
		// 更新 latest 索引
		for agentName, latestID := range s.latest {
			if latestID == id {
				// 重新计算最新 session
				var newLatest SessionID
				for _, sess := range s.store {
					if sess.AgentName == agentName {
						if newLatest == "" || sess.CreatedAt.After(s.store[newLatest].CreatedAt) {
							newLatest = sess.ID
						}
					}
				}
				s.latest[agentName] = newLatest
				break
			}
		}
	}

	return len(deleted), nil
}

// List 列出会话
func (s *MemorySessionStore) List(limit, offset int, agentName string) ([]*Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// 过滤和排序
	var filtered []*Session
	for _, session := range s.store {
		if agentName != "" && session.AgentName != agentName {
			continue
		}
		filtered = append(filtered, s.copySession(session))
	}

	// 按创建时间倒序排序
	sortSessions(filtered)

	// 分页
	if offset >= len(filtered) {
		return []*Session{}, nil
	}

	end := offset + limit
	if end > len(filtered) {
		end = len(filtered)
	}

	return filtered[offset:end], nil
}

// GetLatest 获取指定 agent 的最新会话
func (s *MemorySessionStore) GetLatest(agentName string) (*Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	latestID, ok := s.latest[agentName]
	if !ok {
		return nil, fmt.Errorf("no sessions found for agent: %s", agentName)
	}

	session, ok := s.store[latestID]
	if !ok {
		return nil, fmt.Errorf("session not found: %s", latestID)
	}

	return s.copySession(session), nil
}

// copySession 复制会话（避免并发修改）
func (s *MemorySessionStore) copySession(src *Session) *Session {
	copy := &Session{
		ID:        src.ID,
		AgentName: src.AgentName,
		CreatedAt: src.CreatedAt,
		LastUsedAt: src.LastUsedAt,
		Params:    src.Params,
		Output:    src.Output,
		Error:     src.Error,
		ResumedFrom: src.ResumedFrom,
		Metadata:  make(map[string]any),
	}

	// 深拷贝 metadata
	for k, v := range src.Metadata {
		copy.Metadata[k] = v
	}

	return copy
}

// deleteOldest 删除最旧的会话
func (s *MemorySessionStore) deleteOldest() (int, error) {
	if len(s.store) == 0 {
		return 0, nil
	}

	var oldestID SessionID
	var oldestTime time.Time

	for id, session := range s.store {
		if oldestID == "" || session.CreatedAt.Before(oldestTime) {
			oldestID = id
			oldestTime = session.CreatedAt
		}
	}

	delete(s.store, oldestID)

	// 更新 latest 索引
	for agentName, latestID := range s.latest {
		if latestID == oldestID {
			var newLatest SessionID
			for _, sess := range s.store {
				if sess.AgentName == agentName {
					if newLatest == "" || sess.CreatedAt.After(s.store[newLatest].CreatedAt) {
						newLatest = sess.ID
					}
				}
			}
			s.latest[agentName] = newLatest
			break
		}
	}

	return 1, nil
}

// sortSessions 按创建时间倒序排序
func sortSessions(sessions []*Session) {
	for i := 0; i < len(sessions)-1; i++ {
		for j := i + 1; j < len(sessions); j++ {
			if sessions[i].CreatedAt.Before(sessions[j].CreatedAt) {
				sessions[i], sessions[j] = sessions[j], sessions[i]
			}
		}
	}
}

// UpdateSessionID 更新会话的实际 session_id（执行后从 opencode 获取）
// 这是关键方法：临时 session_id -> 真实 session_id
func (s *MemorySessionStore) UpdateSessionID(oldID, newID SessionID, agentName, output string, err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 查找旧 session
	session, ok := s.store[oldID]
	if !ok {
		return fmt.Errorf("session not found: %s", oldID)
	}

	// 删除旧记录
	delete(s.store, oldID)

	// 创建新记录（保留时间戳等信息）
	newSession := &Session{
		ID:        newID,
		AgentName: agentName,
		CreatedAt: session.CreatedAt,
		LastUsedAt: session.LastUsedAt,
		Params:    session.Params,
		Output:    output,
		Error:     err,
		ResumedFrom: session.ResumedFrom,
		Metadata:  session.Metadata,
	}

	s.store[newID] = newSession

	// 更新 latest 索引
	s.latest[agentName] = newID

	// 记录日志
	if s.logger != nil {
		s.logger.LogSessionUpdated(oldID, newID, agentName)
	}

	return nil
}

// GetActualSessionID 获取实际 session_id（如果已更新）
func (s *MemorySessionStore) GetActualSessionID(sessionID SessionID) (SessionID, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	session, ok := s.store[sessionID]
	if !ok {
		return "", fmt.Errorf("session not found: %s", sessionID)
	}

	// 检查是否是临时 ID
	if strings.HasPrefix(string(session.ID), "tmp_") {
		return "", fmt.Errorf("session %s has not been updated with actual session_id yet", sessionID)
	}

	return session.ID, nil
}

// parseSessionID 解析 session_id 从 opencode 输出
func parseSessionID(output string) (SessionID, error) {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return "", fmt.Errorf("empty output from opencode session list")
	}

	// 拒绝 JSON 格式（数组或对象）
	if strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{") {
		return "", fmt.Errorf("unexpected JSON format in output: %s", trimmed)
	}

	// 尝试匹配 ses_xxx 格式
	// 正则：ses_ 后跟字母数字
	re := regexp.MustCompile(`(ses_[a-zA-Z0-9]+)`)
	matches := re.FindStringSubmatch(trimmed)

	if len(matches) >= 2 {
		return SessionID(matches[1]), nil
	}

	// 如果第一行就是 session_id（无额外信息）
	lines := strings.SplitN(trimmed, "\n", 2)
	firstLine := strings.TrimSpace(lines[0])
	if firstLine != "" && strings.HasPrefix(firstLine, "ses_") {
		// 纯 session_id 格式
		return SessionID(firstLine), nil
	}

	return "", fmt.Errorf("unable to parse session_id from output: %s", trimmed)
}

// Shutdown 关闭会话存储（停止清理守护进程）
func (s *MemorySessionStore) Shutdown() {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()

	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
		s.cleanup = nil
	}
}

// GetStats 获取统计信息
func (s *MemorySessionStore) GetStats() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return map[string]any{
		"total_sessions": len(s.store),
		"agents":         len(s.latest),
		"ttl_mins":       s.ttlMins,
		"max_size":       s.maxSize,
	}
}

// SessionManager 会话管理器（高层 API）
type SessionManager struct {
	store     SessionStore
	config    *config.HarnessConfig
	opencode  *SubAgentManager // 用于获取 CLI 路径
}

// NewSessionManager 创建会话管理器
func NewSessionManager(cfg *config.HarnessConfig, opencode *SubAgentManager) (*SessionManager, error) {
	return NewSessionManagerWithLogger(cfg, opencode, nil)
}

// NewSessionManagerWithLogger 创建会话管理器（带日志器）
func NewSessionManagerWithLogger(cfg *config.HarnessConfig, opencode *SubAgentManager, logger *HarnessLogger) (*SessionManager, error) {
	// 构建存储配置
	storeCfg := &SessionStoreConfig{
		CLIPath:   opencode.cliPath,
		Logger:    logger,
		ProjectName: "default", // TODO: 从 cfg 或其他来源获取
	}

	// 从配置读取 TTL
	if cfg != nil && cfg.SessionTTL != nil && *cfg.SessionTTL > 0 {
		storeCfg.TTLMinutes = *cfg.SessionTTL
	}

	// 创建存储
	store := NewMemorySessionStore(storeCfg)

	return &SessionManager{
		store:  store,
		config: cfg,
		opencode: opencode,
	}, nil
}

// CreateSession 创建会话
func (sm *SessionManager) CreateSession(agentName, params string, resume bool) (SessionID, error) {
	var resumedFrom SessionID

	if resume {
		// 获取最新会话作为来源
		latest, err := sm.store.GetLatest(agentName)
		if err != nil {
			// 如果没有历史会话，则不 resume
			resumedFrom = ""
		} else {
			resumedFrom = latest.ID
		}
	}

	return sm.store.Create(agentName, params, resumedFrom)
}

// UpdateSessionAfterExecution 执行后更新会话信息
// 这是关键步骤：将临时 session_id 替换为实际从 opencode 获取的 session_id
func (sm *SessionManager) UpdateSessionAfterExecution(
	tempSessionID SessionID,
	agentName string,
	output string,
	err error,
) error {
	// 从 opencode 获取实际 session_id
	actualID, fetchErr := sm.fetchSessionIDFromOpencode()

	if fetchErr != nil {
		// 获取失败，记录警告但继续更新
		// 可能是因为 opencode 未配置或未执行
		return fmt.Errorf("update session: %w", err)
	}

	// 更新存储中的 session_id
	if updateErr := sm.store.UpdateSessionID(tempSessionID, actualID, agentName, output, err); updateErr != nil {
		return fmt.Errorf("update session id: %w", updateErr)
	}

	return nil
}

// fetchSessionIDFromOpencode 从 opencode 获取最新 session_id
// 执行：opencode session list -n 1
func (sm *SessionManager) fetchSessionIDFromOpencode() (SessionID, error) {
	cmd := sm.getOpencodeCmd("session", "list", "-n", "1")
	if cmd == nil {
		return "", fmt.Errorf("opencode CLI not found")
	}

	output, execErr := cmd.CombinedOutput()
	if execErr != nil {
		return "", fmt.Errorf("execute 'opencode session list -n 1': %w, output: %s", execErr, string(output))
	}

	return parseSessionID(string(output))
}

// getOpencodeCmd 获取 opencode 命令
func (sm *SessionManager) getOpencodeCmd(args ...string) *exec.Cmd {
	cmd := "opencode"
	if sm.opencode != nil && sm.opencode.cliPath != "" {
		cmd = sm.opencode.cliPath
	}
	return exec.Command(cmd, args...)
}

// GetSession 获取会话详情
func (sm *SessionManager) GetSession(sessionID SessionID) (*Session, error) {
	return sm.store.Get(sessionID)
}

// ListSessions 列出会话
func (sm *SessionManager) ListSessions(limit, offset int, agentName string) ([]*Session, error) {
	return sm.store.List(limit, offset, agentName)
}

// DeleteSession 删除会话
func (sm *SessionManager) DeleteSession(sessionID SessionID) error {
	return sm.store.Delete(sessionID)
}

// CleanupExpired 清理过期会话
func (sm *SessionManager) CleanupExpired() (int, error) {
	if sm.config == nil || sm.config.SessionTTL == nil || *sm.config.SessionTTL <= 0 {
		return 0, nil
	}

	cutoff := time.Now().Add(-time.Duration(*sm.config.SessionTTL) * time.Minute)
	return sm.store.Cleanup(cutoff)
}

// Shutdown 关闭管理器
func (sm *SessionManager) Shutdown() {
	if memStore, ok := sm.store.(*MemorySessionStore); ok {
		memStore.Shutdown()
	}
}
