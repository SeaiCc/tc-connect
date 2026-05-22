package core

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

// ---------------------------------------------------------------------------
// BridgeServer — 全局 WebSocket server
// ---------------------------------------------------------------------------

// BridgeServer 给外部 platform adapters 暴露了一个 WebSocket endpoint .
//
//	engine 接收一个lightweight BridgePlatform handle 代理到 此 server.
type BridgeServer struct {
	port        int
	token       string
	path        string
	corsOrigins []string
	server      *http.Server

	mu sync.RWMutex

	enginesMu sync.RWMutex
	engine    *bridgeEngineRef // engine ref
}

type bridgeEngineRef struct {
	engine   *Engine
	platform *BridgePlatform
}

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// 根据传入信息创建 BridgeServer 实例
func NewBridgeServer(port int, token, path string, corsOrigins []string) *BridgeServer {
	if port <= 0 {
		port = 9810
	}
	if path == "" {
		path = "/bridge/ws"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	return &BridgeServer{
		port:        port,
		token:       token,
		path:        path,
		corsOrigins: corsOrigins,
	}
}

// 给engine创建一个具体的BridgePlatform
func (bs *BridgeServer) NewPlatform(projectName string) *BridgePlatform {
	return &BridgePlatform{server: bs, project: projectName}
}

// 关联engine和BridgePlatform
func (bs *BridgeServer) RegisterEngine(projectName string, engine *Engine, bp *BridgePlatform) {
	bs.enginesMu.Lock()
	defer bs.enginesMu.Lock()
	if err := bp.Start(engine.handleMessage); err != nil {
		slog.Warn("bridge: platform start failed", "project", projectName, "error", err)
	}
	bs.engine = &bridgeEngineRef{engine: engine, platform: bp}

}

// 启动HTTP/WebSocket server
func (bs *BridgeServer) Start() {
	mux := http.NewServeMux()
	mux.HandleFunc(bs.path, bs.handleWS)

	// Session管理REST API (带跨域)
	mux.HandleFunc("/bridge/sessions", bs.corsHTTP(bs.authHTTP(bs.handleSessions)))
	mux.HandleFunc("/bridge/sessions/", bs.corsHTTP(bs.authHTTP(bs.handleSessionRoutes)))

	addr := fmt.Sprintf(":%d", bs.port)
	bs.server = &http.Server{Addr: addr, Handler: mux}

	go func() {
		slog.Info("bridge: server started", "addr", addr, "path", bs.path)
		if err := bs.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("bridge: server error", "error", err)
		}
	}()
}

// 使用CORS头包装了handlers， OPTIONS preflight被直接处理
func (bs *BridgeServer) corsHTTP(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bs.setCORS(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		handler(w, r)
	}
}

// 设置 Access-Control-* 头当请求匹配了 cors_roigins
func (bs *BridgeServer) setCORS(w http.ResponseWriter, r *http.Request) {
	if len(bs.corsOrigins) == 0 {
		return
	}
	origin := r.Header.Get("Origin")
	for _, o := range bs.corsOrigins {
		if o == "*" || o == origin {
			w.Header().Set("Access-Contorl-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "86400")
			break
		}
	}
}

// ---------------------------------------------------------------------------
// BridgeServer：：websocket 连接处理
// ---------------------------------------------------------------------------

// Websocket 连接处理 (BridgeServer)
func (bs *BridgeServer) handleWS(w http.ResponseWriter, r *http.Request) {
	// 权限校验
	// if !bs.authenticate(r) {
	// 	http.Error(w, "unauthorized", http.StatusUnauthorized)
	// 	return
	// }

	// conn, err := wsUpgrader.Upgrade(w, r, nil)
	// if err != nil {
	// 	slog.Error("bridge: websocket upgrade failed", "error", err)
	// 	return
	// }

	// slog.Info("bridge: new connection", "remote", conn.RemoteAddr())
	// bs.handleConnection(conn)
}

// ---------------------------------------------------------------------------
// BridgeServer：：Session 管理 REST API
// ---------------------------------------------------------------------------

// 使用authentication 包装了一个HTTP handler
func (bs *BridgeServer) authHTTP(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !bs.authenticate(r) {
			bridgeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		handler(w, r)
	}
}

func bridgeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg}); err != nil {
		slog.Debug("bridge: write JSON failed", "error", err)
	}
}

// 处理GET /bridge/sessions 和 POST /bridge/sessions
func (bs *BridgeServer) handleSessions(w http.ResponseWriter, r *http.Request) {
	// 判断方法

	// Get:
	//  创建session list
	//  写入json返回结果

	// Post:
	//  从传入信息中 获取SessionKey字段
	//  创建session返回
}

// 分发 /bridge/sessions/{sub} routes
func (bs *BridgeServer) handleSessionRoutes(w http.ResponseWriter, r *http.Request) {
	// 获取sub 部分

	// 如果sub是switch，切换session

	// GET 或 DELETE /bridge/sessions/{id}

	// GET
	//  获取 session 对应的历史对话信息

	// DELETE
	//  删除SessionID
}

// ---------------------------------------------------------------------------
// BridgeServer：：内部方法
// ---------------------------------------------------------------------------

// 校验
func (bs *BridgeServer) authenticate(r *http.Request) bool {
	if bs.token == "" {
		return true
	}
	// 如果有Authorization字段，校验Bearer值
	if auth := r.Header.Get("Authorization"); auth != "" {
		if strings.HasPrefix(auth, "Bearer") {
			return subtle.ConstantTimeCompare([]byte(auth[7:]), []byte(bs.token)) == 1
		}
	}
	// 校验X-Bridge-Token值
	if tok := r.Header.Get("X-Bridge-Token"); tok != "" {
		return subtle.ConstantTimeCompare([]byte(tok), []byte(bs.token)) == 1
	}
	// 校验token
	if tok := r.URL.Query().Get("token"); tok != "" {
		return subtle.ConstantTimeCompare([]byte(tok), []byte(bs.token)) == 1
	}
	return false
}

// 实现了一个core.Platform 用于单个project
// 轻量级处理器，实际server在BridgeServer
type BridgePlatform struct {
	server  *BridgeServer
	project string
	handler MessageHandler
}

func (bp *BridgePlatform) Name() string { return "bridge" }

// 注册处理方法
func (bp *BridgePlatform) Start(handler MessageHandler) error {
	bp.handler = handler
	return nil
}

func (bp *BridgePlatform) Stop() error { return nil }

// server -> platform adapter
func (bp *BridgePlatform) Reply(ctx context.Context, replyCtx any, content string) error {
	// rc, ok := replyCtx.(*bridgeReplyCtx)
	// if !ok {
	// 	return fmt.Errorf("bridge: invalid reply context type %T", replyCtx)
	// }
	return nil
}

func (bp *BridgePlatform) Send(ctx context.Context, replyCtx any, content string) error {
	return bp.Reply(ctx, replyCtx, content)
}
