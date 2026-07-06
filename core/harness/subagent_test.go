package harness

import (
	"context"
	"testing"

	"tc-connect/config"
)

func TestSubAgentManager(t *testing.T) {
	// 创建测试配置
	cfg := &config.HarnessConfig{
		Aliases: map[string]string{
			"plan": "planner",
			"dev":  "developer",
			"test": "tester",
		},
		Agents: map[string]config.SubAgentConfig{
			"planner": {
				TimeoutMins: intPtr(10),
				MaxRetries:  intPtr(2),
				Description: "规划 Agent",
				WorkDir:     "/tmp/planner",
			},
			"developer": {
				TimeoutMins: intPtr(15),
				MaxRetries:  intPtr(3),
				Description: "开发 Agent",
				WorkDir:     "/tmp/dev",
			},
		},
	}

	manager, err := NewSubAgentManager(cfg, "")
	if err != nil {
		t.Fatalf("NewSubAgentManager() error = %v", err)
	}

	t.Run("检查可用 Agent 数量", func(t *testing.T) {
		count := manager.GetAvailableCount()
		if count == 0 {
			t.Error("GetAvailableCount() = 0, expected > 0 (opencode 应该至少有一个默认 agent)")
		}
		t.Logf("Available agents count: %d", count)
	})

	t.Run("别名解析测试", func(t *testing.T) {
		tests := []struct {
			name     string
			wantReal string
			wantOk   bool
		}{
			{"plan", "planner", true},
			{"dev", "developer", true},
			{"test", "tester", true},
			{"unknown", "", false},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				realName, found := manager.ResolveAgentName(tt.name)
				if found != tt.wantOk {
					t.Errorf("ResolveAgentName(%q) found = %v, want %v", tt.name, found, tt.wantOk)
				}
				if found && realName != tt.wantReal {
					t.Errorf("ResolveAgentName(%q) = %v, want %v", tt.name, realName, tt.wantReal)
				}
			})
		}
	})

	t.Run("Agent 配置获取", func(t *testing.T) {
		// 测试已配置的 agent
		meta := manager.GetAgentConfig("planner")
		if meta.TimeoutMins != 10 {
			t.Errorf("planner TimeoutMins = %d, want 10", meta.TimeoutMins)
		}
		if meta.MaxRetries != 2 {
			t.Errorf("planner MaxRetries = %d, want 2", meta.MaxRetries)
		}
		if meta.Description != "规划 Agent" {
			t.Errorf("planner Description = %q, want \"规划 Agent\"", meta.Description)
		}

		// 测试未配置的 agent（应返回默认值）
		meta = manager.GetAgentConfig("unknown")
		if meta.TimeoutMins != 5 {
			t.Errorf("unknown TimeoutMins = %d, want default 5", meta.TimeoutMins)
		}
		if meta.MaxRetries != 3 {
			t.Errorf("unknown MaxRetries = %d, want default 3", meta.MaxRetries)
		}
	})

	t.Run("可用性检查", func(t *testing.T) {
		// 检查真实存在的 agent（opencode 默认的）
		if !manager.IsSubAgentAvailable("build") {
			t.Log("Note: 'build' agent not found, this might be expected if not configured")
		}

		// 检查别名
		if manager.IsSubAgentAvailable("plan") {
			t.Log("Alias 'plan' resolved successfully")
		}
	})

	t.Run("名称验证", func(t *testing.T) {
		tests := []struct {
			name  string
			valid bool
		}{
			{"valid_name", true},
			{"valid-name", true},
			{"valid123", true},
			{"_valid", true},
			{"", false},
			{"123invalid", false},
			{"invalid@name", false},
			{"invalid name", false},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				err := manager.ValidateAgentName(tt.name)
				if tt.valid && err != nil {
					t.Errorf("ValidateAgentName(%q) error = %v, want nil", tt.name, err)
				}
				if !tt.valid && err == nil {
					t.Errorf("ValidateAgentName(%q) error = nil, want error", tt.name)
				}
			})
		}
	})

	t.Run("列出可用 Agent", func(t *testing.T) {
		agents := manager.ListAvailableAgents()
		if len(agents) == 0 {
			t.Error("ListAvailableAgents() = empty, expected some agents")
		}
		t.Logf("Available agents: %v", agents)
	})

	t.Run("上下文超时", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 100000000) // 100ms
		defer cancel()

		// 这个应该快速返回，因为只是内存查找
		available, err := manager.IsSubAgentAvailableWithContext(ctx, "test")
		if err != nil {
			t.Errorf("IsSubAgentAvailableWithContext() error = %v", err)
		}
		t.Logf("WithContext check result: %v", available)
	})
}

// 辅助函数
func intPtr(i int) *int {
	return &i
}
