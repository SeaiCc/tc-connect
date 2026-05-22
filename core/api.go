package core

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// 暴露了本地的Unix socket API 用于内部工具(如定时任务) 给session方法消息
type APIServer struct {
	socketPath string
	listener   net.Listener
	server     *http.Server
	mux        *http.ServeMux
	engine     *Engine
	// cron       *CronScheduler
	// relay      *RelayManager
	mu sync.RWMutex
}

// 再Unix socket上创建一个API server
func NewAPIServer(dataDir string) (*APIServer, error) {
	socketDir := filepath.Join(dataDir, "run")
	if err := os.MkdirAll(socketDir, 0o755); err != nil {
		return nil, fmt.Errorf("create run dir: %w", err)
	}
	sockPath := filepath.Join(socketDir, "api.sock")

	// 移除 stale socket
	os.Remove(sockPath)
	// 创建针对sockPath的监听器
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen unix socket: %w", err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	// 创建API server
	s := &APIServer{
		socketPath: sockPath,
		listener:   listener,
		mux:        http.NewServeMux(),
	}
	// 绑定API
	s.mux.HandleFunc("/send", s.handleSend)
	s.mux.HandleFunc("/sessons", s.handleSessions)

	return s, nil
}

func (s *APIServer) RegisterEngine(name string, e *Engine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.engine = e
}

func (s *APIServer) Start() {
	s.server = &http.Server{Handler: s.mux}
	go func() {
		if err := s.server.Serve(s.listener); err != nil && err != http.ErrServerClosed {
			slog.Error("api server error", "error", err)
		}
	}()
	slog.Info("api server started", "socket", s.socketPath)
}

func (s *APIServer) handleSend(w http.ResponseWriter, r *http.Request) {
	// 只允许POST

	// 解析接收body

	// 调用engine.SendToSessionWithAttachments

	// 返回JSON
}

func (s *APIServer) handleSessions(w http.ResponseWriter, r *http.Request) {
	// 找出engine的所有interactiveState

	// 返回JSON
}
