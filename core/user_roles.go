package core

import (
	"sync"
	"time"
	"strings"
)

type RateLimitCfg struct {
	MaxMessages int           // 每时间窗口允许最多消息, 0 = diabled
	Window      time.Duration // 时间窗口大小
}

type UserRole struct {
	Name         string
	DisabledCmds map[string]bool // 解析 command IDs (包括 "*" wildcard)
	RateLimitCfg *RateLimitCfg   // nil = no role-specific 限制; 使用全局fallbck
}

// 解析user ID 到roles 并管理per-role rate limiters
type UserRoleManager struct {
	mu          sync.RWMutex
	roles       []roleEntry              // ordered list 用于迭代
	defaultRole string                   // fallback 角色名称
	roleMap     map[string]*UserRole     // role name → resolved policy
	limiters    map[string]*RateLimiter  // role name → shared per-role rate limiter
}


// 对给定的user ID 返回一个role
// 优先级: 显式匹配 -> default role -> wildcard -> nil
func (m *UserRoleManager) ResolveRole(userID string) *UserRole {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	uid := strings.ToLower(userID)

	// 1. 显示匹配non-wildcard 角色
	for _, entry := range m.roles {
		if !entry.wildcard && entry.userIDs[uid] {
			return m.roleMap[entry.roleName]
		}
	}

	// 2. 默认role
	if m.defaultRole != "" {
		if role, ok := m.roleMap[m.defaultRole]; ok {
			return role
		}
	}

	// 3. Wildcard role
	for _, entry := range m.roles {
		if entry.wildcard {
			return m.roleMap[entry.roleName]
		}
	}

	return nil
}


// 检查per-user rate limit 基于 user 的角色,
// 返回(allowd, handled). handled=false意味着没有找到role-specific限制, 调用者应回退调全局limiter
// Nil-receiver safe
func (m *UserRoleManager) AllowRate(userID string) (allowed, handled bool) {
	if m == nil {
		return true, false
	}
	role := m.ResolveRole(userID)
	if role == nil || role.RateLimitCfg == nil {
		return true, false
	}
	m.mu.RLock()
	rl := m.limiters[role.Name]
	m.mu.RUnlock()
	if rl == nil {
		return true, false
	}
	return rl.Allow(userID), true
}

type roleEntry struct {
	roleName string
	userIDs  map[string]bool // normalized user IDs; nil when wildcard
	wildcard bool            // true if user_ids contains "*"
}
