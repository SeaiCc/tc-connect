package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"sync/atomic"
	"tc-connect/core"
	"time"
)

// 使用OpencCodeCLI 管理多轮对话, 每次执行Send() 相当于
// 启动`opencode run --format json`进程， --session参数 用于继续会话
type opencodeSession struct {
	cmd     string
	workDir string
	model   string
	events  chan core.Event
	chatID  atomic.Value
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	alive   atomic.Bool
}

// 创建一个新的opencodeSession
func newOpencodeSession(ctx context.Context, cmd, workDir, model, resumeID string) (*opencodeSession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)

	s := &opencodeSession{
		cmd:     cmd,
		workDir: workDir,
		model:   model,
		events:  make(chan core.Event, 64),
		ctx:     sessionCtx,
		cancel:  cancel,
	}
	s.alive.Store(true)

	if resumeID != "" && resumeID != core.ContinueSession {
		s.chatID.Store(resumeID)
	}
	return s, nil
}

// 发送用户信息(可带有图片和文件)来运行agent 进程
func (s *opencodeSession) Send(prompt string, images []core.ImageAttachment, files []core.FileAttachment) error {
	// 获取file路径和prompt
	// if len(files) > 0 {
	// 	filePaths := core.SaveFilesToDisk(s.workDir, files)
	// 	prompt = core.AppendFileRefs(prompt, filePaths)
	// }
	if !s.alive.Load() {
		return fmt.Errorf("session is closed")
	}
	// 获取session ID
	chatID := s.CurrentSessionID()
	isResume := chatID != ""
	// 构建命令行
	args := []string{"run", "--format", "json"}

	if isResume {
		args = append(args, "--session", chatID)
	}
	if s.workDir != "" {
		args = append(args, "--dir", s.workDir)
	}
	// 启用思考
	args = append(args, "--thinking")
	// 使用prompt作为位置参数
	args = append(args, prompt)

	slog.Debug("opencodeSession: launching", "resume", isResume, "session", chatID, "dir", s.workDir)

	// 执行命令
	cmd := exec.CommandContext(s.ctx, s.cmd, args...)
	cmd.Dir = s.workDir
	//
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("opencodeSession: stdout pipe: %w", err)
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("opencodeSession: start: %w", err)
	}

	s.wg.Add(1)
	go s.readLoop(cmd, stdout, &stderrBuf)

	return nil
}

func (s *opencodeSession) readLoop(cmd *exec.Cmd, stdout io.ReadCloser, stderrBuf *bytes.Buffer) {
	defer s.wg.Done()
	defer func() {
		if err := cmd.Wait(); err != nil {
			stderrMsg := stderrBuf.String()
			if stderrMsg != "" {
				slog.Error("opencodeSession: process failed", "error", err, "stderr", stderrMsg)
				evt := core.Event{Type: core.EventError, Error: fmt.Errorf("%s", stderrMsg)}
				select {
				case s.events <- evt:
				case <-s.ctx.Done():
					return
				}
			}
		}
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			slog.Debug("opencodeSession: non-JSON line", "line", line)
			continue
		}
		s.handleEvent(raw)
	}

	if err := scanner.Err(); err != nil {
		slog.Error("opencodeSession: scanner error", "error", err)
		evt := core.Event{Type: core.EventError, Error: fmt.Errorf("read stdout: %w", err)}
		select {
		case s.events <- evt:
		case <-s.ctx.Done():
			return
		}
	}

	// 所有步骤结束之后触发EventResult 处理最终写入
	sid := s.CurrentSessionID()
	evt := core.Event{Type: core.EventResult, SessionID: sid, Done: true}
	select {
	case s.events <- evt:
	case <-s.ctx.Done():
	}

}

// OpenCode NDJSON 事件structure
//
//	{ "type": "text|tool_use|reasoning|step_start|step_finish",
//	  "part": { "type": "text|tool|reasoning|step-start|step-finish", ... } }
func (s *opencodeSession) handleEvent(raw map[string]any) {
	eventType, _ := raw["type"].(string)

	switch eventType {
	case "text":
		s.handleText(raw)
	default:
		b, _ := json.Marshal(raw)
		slog.Debug("opencodeSession: unhandled event", "type", eventType, "raw", string(b))
	}
}

// 处理文本
func (s *opencodeSession) handleText(raw map[string]any) {
	part, _ := raw["part"].(map[string]any)
	if part == nil {
		return
	}
	text, _ := part["text"].(string)
	if text != "" {
		evt := core.Event{Type: core.EventText, Content: text}
		select {
		case s.events <- evt:
		case <-s.ctx.Done():
			return
		}
	}
}

// 空操作， OpenCode 处理内部处理权限
func (s *opencodeSession) RespondPermission(_ string, _ core.PermissionResult) error {
	return nil
}

// 返回触发agent事件的channel (在truns之间保持打开)
func (s *opencodeSession) Events() <-chan core.Event {
	return s.events
}

// 返回当前agent端的会话ID
func (s *opencodeSession) CurrentSessionID() string {
	v, _ := s.chatID.Load().(string)
	return v
}

// 如果underlying 进程仍然运行则返回true
func (s *opencodeSession) Alive() bool {
	return s.alive.Load()
}

// 关闭会话和底层进程
func (s *opencodeSession) Close() error {
	s.alive.Store(false)
	s.cancel()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		slog.Warn("opencodeSession: close timed out, abandoning wg.Wait")
	}
	close(s.events)
	return nil
}
