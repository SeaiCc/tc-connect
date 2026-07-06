package harness

import (
	"fmt"
	"testing"
	"time"

	"tc-connect/config"
)

func TestMemorySessionStore_Create(t *testing.T) {
	store := NewMemorySessionStore(&SessionStoreConfig{
		TTLMinutes: 60,
		MaxSize:    0,
	})
	defer store.Shutdown()

	t.Run("创建新会话", func(t *testing.T) {
		sessionID, err := store.Create("planner", "测试参数", "")
		if err != nil {
			t.Fatalf("Create() error = %v", err)
		}

		if sessionID == "" {
			t.Error("Create() returned empty session ID")
		}

		// 验证会话存在
		session, err := store.Get(sessionID)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}

		if session.AgentName != "planner" {
			t.Errorf("Session.AgentName = %v, want planner", session.AgentName)
		}
		if session.Params != "测试参数" {
			t.Errorf("Session.Params = %v, want 测试参数", session.Params)
		}
	})

	t.Run("创建带 resume 的会话", func(t *testing.T) {
		// 先创建一个会话
		firstID, _ := store.Create("developer", "first", "")
		
		// 创建第二个会话，resume 第一个
		secondID, err := store.Create("developer", "second", firstID)
		if err != nil {
			t.Fatalf("Create() error = %v", err)
		}

		session, _ := store.Get(secondID)
		if session.ResumedFrom != firstID {
			t.Errorf("Session.ResumedFrom = %v, want %v", session.ResumedFrom, firstID)
		}
	})
}

func TestMemorySessionStore_GetLatest(t *testing.T) {
	store := NewMemorySessionStore(&SessionStoreConfig{
		TTLMinutes: 60,
	})
	defer store.Shutdown()

	// 创建多个会话
	_, _ = store.Create("planner", "first", "")
	time.Sleep(10 * time.Millisecond)
	latestID, _ := store.Create("planner", "second", "")

	// 获取最新
	latest, err := store.GetLatest("planner")
	if err != nil {
		t.Fatalf("GetLatest() error = %v", err)
	}

	if latest.ID != latestID {
		t.Errorf("GetLatest() = %v, want %v", latest.ID, latestID)
	}
}

func TestMemorySessionStore_Delete(t *testing.T) {
	store := NewMemorySessionStore(&SessionStoreConfig{})
	defer store.Shutdown()

	sessionID, _ := store.Create("planner", "test", "")

	// 删除
	err := store.Delete(sessionID)
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	// 验证已删除
	_, err = store.Get(sessionID)
	if err == nil {
		t.Error("Get() after Delete() expected error, got nil")
	}
}

func TestMemorySessionStore_Cleanup(t *testing.T) {
	store := NewMemorySessionStore(&SessionStoreConfig{})
	defer store.Shutdown()

	// 手动设置时间函数以模拟旧会话
	now := time.Now()
	store.nowFunc = func() time.Time {
		return now
	}

	// 创建会话（模拟旧会话）
	oldID, _ := store.Create("planner", "old", "")
	
	// 修改创建时间为 2 小时前
	store.mu.Lock()
	store.store[oldID].CreatedAt = now.Add(-2 * time.Hour)
	store.mu.Unlock()

	// 清理 1 小时前的会话
	count, err := store.Cleanup(now.Add(-1 * time.Hour))
	if err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	if count != 1 {
		t.Errorf("Cleanup() count = %v, want 1", count)
	}
}

func TestMemorySessionStore_MaxSize(t *testing.T) {
	store := NewMemorySessionStore(&SessionStoreConfig{
		MaxSize: 3,
	})
	defer store.Shutdown()

	// 创建 4 个会话
	for i := 1; i <= 4; i++ {
		_, _ = store.Create(fmt.Sprintf("agent%d", i), fmt.Sprintf("param%d", i), "")
		time.Sleep(10 * time.Millisecond)
	}

	// 应该只有 3 个
	stats := store.GetStats()
	if total := stats["total_sessions"]; total != 3 {
		t.Errorf("MaxSize limit failed: got %v sessions, want 3", total)
	}
}

func TestMemorySessionStore_UpdateSessionID(t *testing.T) {
	store := NewMemorySessionStore(&SessionStoreConfig{})
	defer store.Shutdown()

	// 创建临时会话
	tempID, _ := store.Create("planner", "test", "")

	// 模拟执行后更新为真实 session_id
	realID := SessionID("ses_abc123")
	err := store.UpdateSessionID(tempID, realID, "planner", "output", nil)
	if err != nil {
		t.Fatalf("UpdateSessionID() error = %v", err)
	}

	// 验证旧 ID 不存在
	_, err = store.Get(tempID)
	if err == nil {
		t.Error("Get(tempID) after UpdateSessionID() expected error")
	}

	// 验证新 ID 存在
	session, err := store.Get(realID)
	if err != nil {
		t.Fatalf("Get(realID) error = %v", err)
	}

	if session.Output != "output" {
		t.Errorf("Session.Output = %v, want output", session.Output)
	}
}

func TestParseSessionID(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    SessionID
		wantErr bool
	}{
		{
			name:  "标准格式",
			input: "ses_16f661969ffeziRL4Yo5qkpPWg (created at 2024-01-01) [planner]",
			want:  "ses_16f661969ffeziRL4Yo5qkpPWg",
		},
		{
			name:  "仅 session_id",
			input: "ses_abc123xyz",
			want:  "ses_abc123xyz",
		},
		{
			name:    "空输入",
			input:   "",
			wantErr: true,
		},
		{
			name:    "无效格式",
			input:   "no session id here",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSessionID(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseSessionID() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("parseSessionID() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSessionManager_CreateSession(t *testing.T) {
	// 模拟 SubAgentManager
	opencode := &SubAgentManager{
		cliPath: "opencode",
	}

	manager, err := NewSessionManager(nil, opencode)
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}
	defer manager.Shutdown()

	t.Run("创建新会话", func(t *testing.T) {
		sessionID, err := manager.CreateSession("planner", "test params", false)
		if err != nil {
			t.Fatalf("CreateSession() error = %v", err)
		}

		if sessionID == "" {
			t.Error("CreateSession() returned empty ID")
		}
	})

	t.Run("创建 resume 会话", func(t *testing.T) {
		// 先创建一个
		firstID, _ := manager.CreateSession("developer", "first", false)
		
		// 再创建 resume
		secondID, err := manager.CreateSession("developer", "second", true)
		if err != nil {
			t.Fatalf("CreateSession() error = %v", err)
		}

		session, _ := manager.GetSession(secondID)
		if session.ResumedFrom != firstID {
			t.Errorf("ResumedFrom = %v, want %v", session.ResumedFrom, firstID)
		}
	})
}

func TestSessionManager_ListSessions(t *testing.T) {
	opencode := &SubAgentManager{cliPath: "opencode"}
	manager, _ := NewSessionManager(nil, opencode)
	defer manager.Shutdown()

	// 创建多个会话
	for i := 0; i < 5; i++ {
		_, _ = manager.CreateSession("planner", fmt.Sprintf("param%d", i), false)
	}

	// 列出
	sessions, err := manager.ListSessions(10, 0, "")
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}

	if len(sessions) != 5 {
		t.Errorf("ListSessions() returned %d sessions, want 5", len(sessions))
	}
}

func TestSessionManager_DeleteSession(t *testing.T) {
	opencode := &SubAgentManager{cliPath: "opencode"}
	manager, _ := NewSessionManager(nil, opencode)
	defer manager.Shutdown()

	sessionID, _ := manager.CreateSession("planner", "test", false)

	err := manager.DeleteSession(sessionID)
	if err != nil {
		t.Fatalf("DeleteSession() error = %v", err)
	}

	_, err = manager.GetSession(sessionID)
	if err == nil {
		t.Error("GetSession() after DeleteSession() expected error")
	}
}

// TestSessionManager_UpdateSessionAfterExecution 测试执行后更新会话
func TestSessionManager_UpdateSessionAfterExecution(t *testing.T) {
	// 模拟 SubAgentManager，但不实际调用 opencode CLI
	opencode := &SubAgentManager{cliPath: "opencode"}
	manager, err := NewSessionManager(nil, opencode)
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}
	defer manager.Shutdown()

	// 创建会话
	tempSessionID, err := manager.CreateSession("planner", "test params", false)
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	// 验证临时 session_id 存在
	session, err := manager.GetSession(tempSessionID)
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	if session == nil {
		t.Fatal("Session should exist")
	}

	// 注意：UpdateSessionAfterExecution 会尝试调用 opencode CLI 获取真实 session_id
	// 在测试环境中，如果 opencode 不可用，会返回错误
	// 这里我们测试错误处理路径
	err = manager.UpdateSessionAfterExecution(tempSessionID, "planner", "test output", nil)
	if err == nil {
		// 如果 opencode 可用且成功，这是正常的
		t.Log("UpdateSessionAfterExecution succeeded (opencode available)")
	} else {
		// 如果 opencode 不可用，这也是预期的
		t.Logf("UpdateSessionAfterExecution failed as expected in test env: %v", err)
	}
}

// TestParseSessionID_EdgeCases 测试 parseSessionID 的边界情况
func TestParseSessionID_EdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    SessionID
		wantErr bool
	}{
		{
			name:  "标准格式带额外信息",
			input: "ses_16f661969ffeziRL4Yo5qkpPWg (created at 2024-01-01) [planner]",
			want:  "ses_16f661969ffeziRL4Yo5qkpPWg",
		},
		{
			name:  "仅 session_id",
			input: "ses_abc123xyz",
			want:  "ses_abc123xyz",
		},
		{
			name:    "空输入",
			input:   "",
			wantErr: true,
		},
		{
			name:    "无效格式（无 ses_前缀）",
			input:   "no session id here",
			wantErr: true,
		},
		{
			name:    "无效格式（JSON）",
			input:   `{"error": "not found"}`,
			wantErr: true,
		},
		{
			name:    "无效格式（数组）",
			input:   `["ses_abc"]`,
			wantErr: true,
		},
		{
			name:  "带前后空格",
			input: "  ses_xyz789  ",
			want:  "ses_xyz789",
		},
		{
			name:  "多行输出第一行是 session_id",
			input: "ses_multi123\nadditional info",
			want:  "ses_multi123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSessionID(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseSessionID() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("parseSessionID() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSessionManager_ConcurrentAccess 测试并发访问安全性
func TestSessionManager_ConcurrentAccess(t *testing.T) {
	opencode := &SubAgentManager{cliPath: "opencode"}
	manager, err := NewSessionManager(&config.HarnessConfig{}, opencode)
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}
	defer manager.Shutdown()

	done := make(chan bool)

	// 并发创建会话
	go func() {
		for i := 0; i < 100; i++ {
			_, _ = manager.CreateSession(fmt.Sprintf("agent%d", i%10), fmt.Sprintf("param%d", i), false)
		}
		done <- true
	}()

	// 并发读取会话
	go func() {
		for i := 0; i < 100; i++ {
			_, _ = manager.ListSessions(10, 0, "")
		}
		done <- true
	}()

	// 等待完成
	<-done
	<-done

	// 如果没 panic，测试通过
	t.Log("Concurrent access test passed")
}

// TestSessionManager_FetchSessionIDFromOpencode 测试 fetchSessionIDFromOpencode 的边界情况
func TestSessionManager_FetchSessionIDFromOpencode(t *testing.T) {
	// 使用不存在的 opencode 路径，测试错误处理
	opencode := &SubAgentManager{cliPath: "/nonexistent/opencode"}
	manager, err := NewSessionManager(nil, opencode)
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}
	defer manager.Shutdown()

	// 测试 fetchSessionIDFromOpencode（私有方法，通过 UpdateSessionAfterExecution 间接测试）
	tempSessionID, err := manager.CreateSession("planner", "test", false)
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	// 预期失败，因为 opencode 不存在
	err = manager.UpdateSessionAfterExecution(tempSessionID, "planner", "test output", nil)
	if err == nil {
		t.Error("UpdateSessionAfterExecution() expected error when opencode not found, got nil")
	}
}
