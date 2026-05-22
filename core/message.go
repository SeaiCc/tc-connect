package core

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 表示用户发送的图片
type ImageAttachment struct {
	MimeType string // e.g. "image/png", "image/jepg"
	Data     []byte // raw image bytes
	FileName string // 文件名(可选)
}

// 表示用户发送的文件(PDF, doc, preadsheet..)
type FileAttachment struct {
	MimeType string // e.g. "application/pdf", "text/plain"
	Data     []byte // raw image bytes
	FileName string // 文件名
}

// 保存文件attachments 到 .tc-connect/attachments/ 并返回据对路径列表
// Agents 能在prompts中引用这些路径，CLI可使用内置的工具读取
func SaveFilesToDisk(workDir string, files []FileAttachment) []string {
	if len(files) == 0 {
		return nil
	}
	attachDir := filepath.Join(workDir, ".cc-connect", "attachments")
	if err := os.MkdirAll(attachDir, 0o755); err != nil {
		slog.Warn("SaveFilesToDisk: mkdir failed", "dir", attachDir, "error", err)
	}
	var paths []string
	for i, f := range files {
		fname := f.FileName
		if fname == "" {
			fname = fmt.Sprintf("file_%d_%d", time.Now().UnixMilli(), i)
		}
		fpath := filepath.Join(attachDir, fname)
		if err := os.WriteFile(fpath, f.Data, 0o644); err != nil {
			slog.Error("SaveFilesToDisk: write failed", "error", err)
			continue
		}
		paths = append(paths, fpath)
		slog.Debug("SaveFilesToDisk: file saved", "path", fpath, "name", f.FileName, "mime", f.MimeType, "size", len(f.Data))
	}
	return paths
}

// 添加文件路径引用到prompt字符串中
func AppendFileRefs(prompt string, filePaths []string) string {
	if len(filePaths) == 0 {
		return prompt
	}
	if prompt == "" {
		prompt = "Please analyze the attached file(s)."
	}
	return prompt + "\n\n(Files saved locally, please read them: " + strings.Join(filePaths, ",") + ")"
}

// 表示来自平台的统一消息
type Message struct {
	SessionKey   string // 用于用户上下文的唯一key "feishu:{chatID}:{userID}"
	Platform     string
	MessageID    string // 用于追踪的平台消息ID
	UserID       string
	UserName     string
	ChatName     string // 可读的chat/group 名称
	Content      string
	Images       []ImageAttachment
	Files        []FileAttachment
	ExtraContent string // 为平台准备的平台富内容(位置文本 回复引用)
	ChannelKey   string // 平台提供的通道标识符 用于工作区绑定
	ReplyCtx     any    // 回复时需要特定平台的上下文信息
	FromVoice    bool   // 消息是否来源于语音转录
	ModeOverride string // 如果设置,临时覆盖agent权限,用于此条消息
}

// 区分不同类型的agent输出
type EventType string

// 接收到的事件状态engine:: processInteractiveEvents使用
const (
	EventText              EventType = "text"               // 文本
	EventResult            EventType = "result"             // 最终聚合result
	EventError             EventType = "error"              // error occurred
	EventPermissionRequest EventType = "permission_request" // agent requests permission via
	EventThinking          EventType = "thinking"           // thinking/processing status
)

// AskUserQuestion中的question结构体
type UserQuestion struct {
	Question    string               `json:"question"`
	Header      string               `json:"header"`
	Options     []UserQuestionOption `json:"options"`
	MultiSelect bool                 `json:"multiSelect"`
}

// UserQuestion中的一个可选项
type UserQuestionOption struct {
	Label       string `json:"lable"`
	Description string `json:"description"`
}

// 表示单个agent的输出,以流式方式返回给引擎
type Event struct {
	Type         EventType // 类型, text/tool_use ...
	Content      string
	ToolName     string         // 用于EventToolUse, EventPermissionRequest
	ToolInput    string         // 可读的tool input 总结
	ToolInputRaw map[string]any // 原始tool输入(用于EventPermissionRequest, 在allow response中使用 )
	ToolResult   string         // 用于EventToolResult
	ToolStatus   string         // 用于EventToolResult的可选状态 (e.g. completed/failed)
	ToolExitCode *int           // 用于EventToolResult可选退出码
	ToolSuccess  *bool          // 用于EventToolResult的可选成功标志
	SessionID    string         // agent管理的 session ID用以保证对话连贯性
	RequestID    string         // 用于EventPermissionRequest的唯一requestID
	Questions    []UserQuestion // 当 ToolName == "AskUserQuestion" 时填充
	Done         bool
	Error        error
	InputTokens  int // agent result events中的token使用
	OutputTokens int
}

// session中的一次交互
type HistoryEntry struct {
	Role      string    `json:"role"` // "user" | "assistant"
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

// 描述了agent后端报告的一个会话
type AgentSessionInfo struct {
	ID           string
	Summary      string
	MessageCount int
	ModifiedAt   time.Time
	GitBranch    string
}
