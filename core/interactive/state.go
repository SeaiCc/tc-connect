package interactive

import (
	"sync"
	"tc-connect/core/mode"
	"tc-connect/core/types"
	"time"
)

// 当session繁忙时hold到达的消息, 排队时不发送到agent的stdin,
// 时间循环在当前turn完成后发送它以避免trun当中的干扰
type queuedMessage struct {
	replyCtx      any
	platform      types.Platform
	content       string
	images        []types.ImageAttachment
	files         []types.FileAttachment
	fromVoice     bool
	userID        string
	msgPlatform   string // 用于发送injection的平台名称
	msgSessionKey string // 用于提取chatID的session key
}

// 追踪一个运行的agent sesion 及其 权限状态
type State struct {
	agentSession types.AgentSession // agent侧::session
	platform     types.Platform
	replyCtx     any
	workspaceDir string

	mu      sync.Mutex
	stopCh  chan struct{}
	stopped bool

	pending         *pendingPermission
	pendingMessages []queuedMessage // session繁忙时排队
	approveAll      bool            // 当为true时, 自动统一所有的权限请求

	fromVoice bool // 是否为语音来源
	sideText  string

	deleteMode *mode.DeleteModeState

	lastAutoCompressAt     time.Time // 上次压缩时间
	lastAutoCompressTokens int       // 上次压缩数量
}

// IsStopped 检查是否已停止
func (s *State) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

// MarkStopped 标记为关闭
func (s *State) MarkStopped() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return // 已经关闭，返回
	}
	s.stopped = true
	if s.stopCh == nil {
		// 创建一个chan
		s.stopCh = make(chan struct{})
	}
	// close需要传入一个chan
	close(s.stopCh)
}

// StopSignal 获取停止信号 channel
func (s *State) StopSignal() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopCh == nil {
		s.stopCh = make(chan struct{})
		if s.stopped {
			close(s.stopCh)
		}
	}
	return s.stopCh
}

// GetContext 原子获取上下文快照（线程安全）
func (s *State) GetContext() (workspaceDir string, platform types.Platform, replyCtx any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.workspaceDir, s.platform, s.replyCtx
}

// pendingPermission 等待用户返回的权限请求
type pendingPermission struct {
	RequestID       string
	ToolName        string
	ToolInput       map[string]any
	InputPreview    string
	Questions       []types.UserQuestion
	Answers         map[int]string
	CurrentQuestion int
	Resolved        chan struct{}
	resolveOnce     sync.Once
}

func (pp *pendingPermission) Resolve() {
	pp.resolveOnce.Do(func() { close(pp.Resolved) })
}
