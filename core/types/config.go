package types

import (
	"errors"
	"fmt"
	"strconv"
)

// 配置重载结果
type ConfigReloadResult struct {
	DisplayUpdated   bool
	ProvidersUpdated int
	CommandsUpdated  int
}

// 展示配置
type DisplayCfg struct {
	ThinkingMessages bool
	ThinkingMaxLen   int // thing语言的最大runes数量; 0 = 不截断
	ToolMaxLen       int // tool使用预览的最大runes数量; 0 = 不截断
	ToolMessages     bool
}

func (c *DisplayCfg) Set(key, value string) error {
	switch key {
	case "thinking_messages":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("invalid boolean: %s", value)
		}
		c.ThinkingMessages = b
		return nil
	case "thinking_max_len":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer: %s", value)
		}
		if n < 0 {
			return fmt.Errorf("value must be >= 0")
		}
		c.ThinkingMaxLen = n
		return nil
	case "tool_messages":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("invalid boolean: %s", value)
		}
		c.ToolMessages = b
		return nil
	case "tool_max_len":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer: %s", value)
		}
		if n < 0 {
			return fmt.Errorf("value must be >= 0")
		}
		c.ToolMaxLen = n
	}
	return errors.New("unknown key")
}

// 描述一个可配置的运行时参数
type ConfigItem struct {
	Key    string
	Desc   string // en description
	DescZh string // zh description
	Value  string
}

func NewConfigItems(
	thinkingMessages bool, thinkingMaxLen int,
	toolMessages bool, toolMaxLen int,
) []ConfigItem {
	return []ConfigItem{
		{
			Key:    "thinking_messages",
			Desc:   "Whether thinking messages are shown (true/false)",
			DescZh: "是否显示思考消息 (true/false)",
			Value:  fmt.Sprintf("%t", thinkingMessages),
		},
		{
			Key:    "thinking_max_len",
			Desc:   "Max chars for thinking messages (0=no truncation)",
			DescZh: "思考消息最大长度 (0=不截断)",
			Value:  fmt.Sprintf("%d", thinkingMaxLen),
		},
		{
			Key:    "tool_messages",
			Desc:   "Whether tool progress messages are shown (true/false)",
			DescZh: "是否显示工具进度消息 (true/false)",
			Value:  fmt.Sprintf("%t", toolMessages),
		},
		{
			Key:    "tool_max_len",
			Desc:   "Max chars for tool use messages (0=no truncation)",
			DescZh: "工具消息最大长度 (0=不截断)",
			Value:  fmt.Sprintf("%d", toolMaxLen),
		},
	}
}

func (ci ConfigItem) Description(isZh bool) string {
	if isZh && ci.DescZh != "" {
		return ci.DescZh
	}
	return ci.Desc
}
