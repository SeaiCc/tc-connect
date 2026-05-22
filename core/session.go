package core

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AgentSessionID的哨兵值，告诉agent使用--continue(恢复最近session)而不是特定的会话ID
const ContinueSession = "__continue__"

// 追踪对话
type Session struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	AgentSessionID string         `json:"agent_session_id"`
	AgentType      string         `json:"agent_type,omitempty"`
	History        []HistoryEntry `json:"history"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`

	mu   sync.Mutex `json:"-"`
	busy bool       `json:"-"`
}

func (s *Session) TryLock() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		return false
	}
	s.busy = true
	return true
}

func (s *Session) Unlock() {
	s.unlock(true)
}

func (s *Session) UnlockWithoutUpdate() {
	s.unlock(false)
}

func (s *Session) unlock(update bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy = false
	if update {
		s.UpdatedAt = time.Now()
	}
}

// 获取Name
func (s *Session) GetName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Name
}

func (s *Session) GetUpdatedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.UpdatedAt
}

// 若session的ID为ContinueSession，将其置为空
func (s *Session) stripContinueSessionSentinel() {
	s.mu.Lock()
	if s.AgentSessionID == ContinueSession {
		s.AgentSessionID = ""
	}
	s.mu.Unlock()
}

// 原子设置agent sessionID和agent type, 哨兵 ContinueSession 永远不会被持久化 -
// 仅在开启agent时短暂使用; 存储到磁盘并打断回复
func (s *Session) SetAgentSessionID(id, agentType string) {
	if id == ContinueSession {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.AgentSessionID = id
	s.AgentType = agentType
}

// 原子读取agent的sessionID
func (s *Session) GetAgentSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.AgentSessionID
}

// CompareAndSetAgentSessionID 仅在当前代理会话 ID 为空或仍保留错误的持久 ContinueSession 标记时，
// 才设置代理会话 ID。
// 如果值已设置，则返回 true；如果已存储真实的会话 ID，则返回 false。
func (s *Session) CompareAndSetAgentSessionID(id, agentType string) bool {
	if id == "" || id == ContinueSession {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.AgentSessionID != "" && s.AgentSessionID != ContinueSession {
		return false
	}
	s.AgentSessionID = id
	s.AgentType = agentType
	return true
}

// 原子设置agentID agent type 和名称
func (s *Session) SetAgentInfo(agentSessionID, agentType, name string) {
	if agentSessionID == ContinueSession {
		agentSessionID = ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.AgentSessionID = agentSessionID
	s.AgentType = agentType
	s.Name = name
}

// 返回历史信息HistoryEntry数组
func (s *Session) GetHistory(n int) []HistoryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := len(s.History)
	if n <= 0 || n > total {
		n = total
	}
	out := make([]HistoryEntry, n)
	copy(out, s.History[total-n:])
	return out
}

// 添加历史消息 role content Timestamp
func (s *Session) AddHistory(role, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.History = append(s.History, HistoryEntry{
		Role:      role,
		Content:   content,
		Timestamp: time.Now(),
	})
}

func (s *Session) ClearHistory() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.History = nil
}

// 展示了session key的用户可读的展示信息
type UserMeta struct {
	UserName string `json:"user_name,omitempty"`
	ChatName string `json:"chat_name,omitempty"`
}

type sessionSnapshot struct {
	Sessions      map[string]*Session  `json:"sessions"`
	ActiveSession map[string]string    `json:"active_session"`
	UserSessions  map[string][]string  `json:"user_session"`
	Counter       int64                `json:"counter"`
	SessionNames  map[string]string    `json:"session_names,omitempty"` // 给session 生成一个命名
	UserMeta      map[string]*UserMeta `json:"user_meta,omitempty"`     // sessionKey →展示信息
}

// ========================  核心类 SessionManager ===========================
//
// ========================  核心类  ===========================

// JSON 文件管理session
type SessionManager struct {
	mu            sync.RWMutex
	sessions      map[string]*Session
	activeSession map[string]string
	userSessions  map[string][]string
	sessionNames  map[string]string    //agent session ID -> 自定义名称
	userMeta      map[string]*UserMeta // sessionKey -> display info
	counter       int64
	storePath     string // empty = 无持久化
}

func NewSessionManager(storePath string) *SessionManager {
	sm := &SessionManager{
		sessions:      make(map[string]*Session),
		activeSession: make(map[string]string),
		userSessions:  make(map[string][]string),
		sessionNames:  make(map[string]string),
		userMeta:      make(map[string]*UserMeta),
		storePath:     storePath,
	}
	if storePath != "" {
		sm.load()
	}
	return sm
}

// 递增计数值 拼接前缀作为ID, 创建session时使用
func (sm *SessionManager) nextID() string {
	sm.counter++
	return fmt.Sprintf("s%d", sm.counter)
}

// 获取sessionID
func (sm *SessionManager) GetSessionName(agentSessionID string) string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.sessionNames[agentSessionID]
}

func (sm *SessionManager) SetSessionName(agentSessionID, name string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if name == "" {
		delete(sm.sessionNames, agentSessionID)
	} else {
		sm.sessionNames[agentSessionID] = name
	}
	sm.saveLocked()
}

// 使用userKey注册一个新session, 不改变当前的session. 用于独立的one-off runs
//(如带session_mode=new_per_run的cron). 这样用户当前的chat保持默认普通message的target
func (sm *SessionManager) NewSideSession(userKey, name string) *Session {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	id := sm.nextID()
	now := time.Now()
	s := &Session{
		ID:        id,
		Name:      name,
		CreatedAt: now,
		UpdatedAt: now,
	}
	sm.sessions[id] = s
	sm.userSessions[userKey] = append(sm.userSessions[userKey], id)
	sm.saveLocked()
	return s
}

// 给session key更新一个人可读的metadata, 仅non-empty field 被应用 (合并behavior)
func (sm *SessionManager) UpdateUserMeta(sessionKey, userName, chatName string) {
	if userName == "" && chatName == "" {
		return
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	meta, ok := sm.userMeta[sessionKey]
	if !ok {
		meta = &UserMeta{}
		sm.userMeta[sessionKey] = meta
	}
	if userName != "" {
		meta.UserName = userName
	}
	if chatName != "" {
		meta.ChatName = chatName
	}
}

// ======================== SessionManager Session ===========================

// 根据传递的sessionKey创建new session
func (sm *SessionManager) NewSession(userKey, name string) *Session {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	s := sm.createLocked(userKey, name)
	sm.saveLocked()
	return s
}

// 返回tc-connect 追踪的一系列agent session IDs
// 用于过滤agent.ListSessions()的输出, 仅保留由tc-connect 拥有的会话,排除同目录
// 下通过CLI创建的session
func (sm *SessionManager) KnownAgentSessionIDs() map[string]struct{} {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	ids := make(map[string]struct{})
	for _, s := range sm.sessions {
		s.mu.Lock()
		aid := s.AgentSessionID
		s.mu.Unlock()
		if aid != "" {
			ids[aid] = struct{}{}
		}
	}
	return ids
}

func (sm *SessionManager) GetOrCreateActive(userKey string) *Session {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// 获取session id
	if sid, ok := sm.activeSession[userKey]; ok {
		// 获取session
		if s, ok := sm.sessions[sid]; ok {
			return s
		}
	}
	s := sm.createLocked(userKey, "default")
	sm.saveLocked()
	return s
}

func (sm *SessionManager) DeleteByAgentSessionID(agentSessionID string) int {
	if agentSessionID == "" {
		return 0
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	removed := 0
	//
	for id, s := range sm.sessions {
		s.mu.Lock()
		matched := s.AgentSessionID == agentSessionID
		s.mu.Unlock()
		if !matched {
			continue
		}
		// 删除
		sm.deleteByIDLocked(id)
		removed++
	}
	if removed > 0 { // 若移除了,更新持久化
		sm.saveLocked()
	}
	return removed
}

func (sm *SessionManager) deleteByIDLocked(id string) {
	delete(sm.sessions, id)
	// userSessions: key - user  value - []sessionStr ???
	for userKey, ids := range sm.userSessions {
		for i, sid := range ids {
			if sid == id { // 从userSession中剔除id, break
				sm.userSessions[userKey] = append(ids[:i], ids[i+1:]...)
				break
			}
		}
		if sm.activeSession[userKey] == id {
			delete(sm.activeSession, userKey)
		}
	}
}

// 创建session (with Lock)
func (sm *SessionManager) createLocked(userKey, name string) *Session {
	id := sm.nextID()
	now := time.Now()
	s := &Session{
		ID:        id,
		Name:      name,
		CreatedAt: now,
		UpdatedAt: now,
	}
	sm.sessions[id] = s
	sm.activeSession[userKey] = id
	sm.userSessions[userKey] = append(sm.userSessions[userKey], id)
	return s
}

func (sm *SessionManager) saveLocked() {
	if sm.storePath == "" {
		return
	}

	// 构建深拷贝快照避免并发session锁竞争
	snapSessions := make(map[string]*Session, len(sm.sessions))
	// 遍历sessions, 拷贝
	for id, s := range sm.sessions {
		s.mu.Lock()
		agentSID := s.AgentSessionID
		// 若为SID为协程标志, 置为空
		if agentSID == ContinueSession {
			agentSID = ""
			s.AgentSessionID = ""
		}
		snapSessions[id] = &Session{
			ID:             s.ID,
			Name:           s.Name,
			AgentSessionID: agentSID,
			AgentType:      s.AgentType,
			History:        append([]HistoryEntry(nil), s.History...),
			CreatedAt:      s.CreatedAt,
			UpdatedAt:      s.UpdatedAt,
		}
		s.mu.Unlock()
	}

	snap := sessionSnapshot{
		Sessions:      snapSessions,
		ActiveSession: sm.activeSession,
		UserSessions:  sm.userSessions,
		Counter:       sm.counter,
		SessionNames:  sm.sessionNames,
		UserMeta:      sm.userMeta,
	}
	// json格式化
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		slog.Error("session: failed to marshal", "error", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(sm.storePath), 0o755); err != nil {
		slog.Error("session: failed to create dir", "error", err)
		return
	}
	// 原子写入 sm storePath
	if err := AtomicWriteFile(sm.storePath, data, 0o644); err != nil {
		slog.Error("session: failed to write", "path", sm.storePath, "error", err)
	}

}

func (sm *SessionManager) load() {
	// 读取session文件
	data, err := os.ReadFile(sm.storePath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Error("session: failed to read", "path", sm.storePath, "error", err)
		}
		return
	}
	var snap sessionSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		slog.Error("session: failed to unmarshal", "path", sm.storePath, "error", err)
		return
	}
	sm.sessions = snap.Sessions
	sm.activeSession = snap.ActiveSession
	sm.sessionNames = snap.SessionNames
	sm.userMeta = snap.UserMeta
	sm.counter = snap.Counter

	if sm.sessions == nil {
		sm.sessions = make(map[string]*Session)
	}
	if sm.activeSession == nil {
		sm.activeSession = make(map[string]string)
	}
	if sm.userSessions == nil {
		sm.userSessions = make(map[string][]string)
	}
	if sm.sessionNames == nil {
		sm.sessionNames = make(map[string]string)
	}
	if sm.userMeta == nil {
		sm.userMeta = make(map[string]*UserMeta)
	}

	for _, s := range sm.sessions {
		s.stripContinueSessionSentinel()
	}
	slog.Info("session: loaded from disk", "path", sm.storePath, "sessions", len(sm.sessions))
}

// 持久化当前的状态到磁盘, 从外部调用安全(message处理之后)
func (sm *SessionManager) Save() {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	sm.saveLocked()
}
