package core

import "fmt"

// AgentFactory creates an Agent from config options
type AgentFactory func(opts map[string]any) (Agent, error)

// 从options中创建AgentFactory
type PlatformFactory func(opts map[string]any) (Platform, error)

var (
	agentFactories    = make(map[string]AgentFactory)
	platformFactories = make(map[string]PlatformFactory)
)

func RegisterPlatform(name string, factory PlatformFactory) {
	platformFactories[name] = factory
}

func RegisterAgent(name string, factory AgentFactory) {
	agentFactories[name] = factory
}

// 根据传入的平台名找到对应创建方法并执行
func CreatePlatform(name string, opts map[string]any) (Platform, error) {
	f, ok := platformFactories[name]
	if !ok {
		available := make([]string, 0, len(platformFactories))
		for k := range platformFactories {
			available = append(available, k)
		}
		return nil, fmt.Errorf("unknown platform %q, available: %v", name, available)
	}
	return f(opts)
}

// 传入agent名称(opencode, cc ...) 由对应的工厂类
func CreateAgent(name string, opts map[string]any) (Agent, error) {
	// 获取创建方法
	f, ok := agentFactories[name]
	if !ok {
		available := make([]string, 0, len(agentFactories))
		for k := range agentFactories {
			available = append(available, k)
		}
		return nil, fmt.Errorf("unknown agent %q, available: %v", name, available)
	}
	return f(opts)
}
