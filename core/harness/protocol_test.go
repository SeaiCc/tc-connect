package harness

import (
	"testing"
)

func TestParseProtocol(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantAgent  string
		wantParams string
		wantResume bool
		wantErr    bool
		wantNil    bool // 期望返回 nil（END 标识）
	}{
		{
			name:       "正常解析 planner agent",
			input:      "planner||生成项目规划||false",
			wantAgent:  "planner",
			wantParams: "生成项目规划",
			wantResume: false,
			wantErr:    false,
			wantNil:    false,
		},
		{
			name:       "正常解析 developer agent",
			input:      "developer||实现登录功能||true",
			wantAgent:  "developer",
			wantParams: "实现登录功能",
			wantResume: true,
			wantErr:    false,
			wantNil:    false,
		},
		{
			name:    "识别 END 标识",
			input:   "END",
			wantNil: true,
			wantErr: false,
		},
		{
			name:    "空字符串",
			input:   "",
			wantErr: true,
		},
		{
			name:    "只有空白字符",
			input:   "   ",
			wantErr: true,
		},
		{
			name:    "字段数量不足",
			input:   "planner||生成规划",
			wantErr: true,
		},
		{
			name:    "字段数量过多",
			input:   "planner||生成规划||false||extra",
			wantErr: true,
		},
		{
			name:    "agent_name 为空",
			input:   "||生成规划||false",
			wantErr: true,
		},
		{
			name:       "带前导和尾随空格",
			input:      "  planner || 生成规划 || false  ",
			wantAgent:  "planner",
			wantParams: "生成规划",
			wantResume: false,
			wantErr:    false,
		},
		{
			name:       "params 为空",
			input:      "planner||||false",
			wantAgent:  "planner",
			wantParams: "",
			wantResume: false,
			wantErr:    false,
		},
		{
			name:       "resume 为 yes",
			input:      "planner||测试||yes",
			wantAgent:  "planner",
			wantParams: "测试",
			wantResume: true,
			wantErr:    false,
		},
		{
			name:       "resume 为 1",
			input:      "planner||测试||1",
			wantAgent:  "planner",
			wantParams: "测试",
			wantResume: true,
			wantErr:    false,
		},
		{
			name:       "resume 为 no",
			input:      "planner||测试||no",
			wantAgent:  "planner",
			wantParams: "测试",
			wantResume: false,
			wantErr:    false,
		},
		{
			name:       "resume 为 0",
			input:      "planner||测试||0",
			wantAgent:  "planner",
			wantParams: "测试",
			wantResume: false,
			wantErr:    false,
		},
		{
			name:       "大小写混合的 resume",
			input:      "planner||测试||True",
			wantAgent:  "planner",
			wantParams: "测试",
			wantResume: true,
			wantErr:    false,
		},
		{
			name:    "END 带空格（应报错）",
			input:   " END ",
			wantNil: true, // 因为 TrimSpace 会去除空格
			wantErr: false,
		},
		{
			name:    "类似 END 但不是",
			input:   "ENDS",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseProtocol(tt.input)

			if tt.wantNil {
				if err != nil {
					t.Errorf("ParseProtocol() unexpected error: %v", err)
				}
				if got != nil {
					t.Errorf("ParseProtocol() expected nil, got %v", got)
				}
				return
			}

			if (err != nil) != tt.wantErr {
				t.Errorf("ParseProtocol() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if err != nil {
				return
			}

			if got.AgentName != tt.wantAgent {
				t.Errorf("ParseProtocol().AgentName = %v, want %v", got.AgentName, tt.wantAgent)
			}
			if got.Params != tt.wantParams {
				t.Errorf("ParseProtocol().Params = %v, want %v", got.Params, tt.wantParams)
			}
			if got.Resume != tt.wantResume {
				t.Errorf("ParseProtocol().Resume = %v, want %v", got.Resume, tt.wantResume)
			}
		})
	}
}

func TestIsValidAgentName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"有效：planner", "planner", true},
		{"有效：developer", "developer", true},
		{"有效：test_agent", "test_agent", true},
		{"有效：test-agent", "test-agent", true},
		{"有效：A1", "A1", true},
		{"有效：_a", "_a", true},
		{"无效：空字符串", "", false},
		{"无效：以数字开头", "1test", false},
		{"无效：包含空格", "test agent", false},
		{"无效：包含特殊字符", "test@agent", false},
		{"无效：超长", string(make([]byte, 100)), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsValidAgentName(tt.input); got != tt.want {
				t.Errorf("IsValidAgentName() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsEndToken(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"END 标识", "END", true},
		{"带空格 END", " END ", true},
		{"非 END", "ENDS", false},
		{"空字符串", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsEndToken(tt.input); got != tt.want {
				t.Errorf("IsEndToken() = %v, want %v", got, tt.want)
			}
		})
	}
}
