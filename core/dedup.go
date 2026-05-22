package core

import (
	"sync"
	"time"
)

const dedupTTL = 60 * time.Second

// 进程启动时StartTime被设置一次，平台使用该值丢弃其之前创建的消息
// 防止重启后处理已重发/未确认的消息
var StartTime = time.Now()

// 追踪最近看到的ID来防止复数进程，并发安全
type MessageDedup struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// 如果msgID在TTL window 中早已经出现返回true
// 空msgID永远不认为是复数
func (d *MessageDedup) IsDuplicate(msgID string) bool {
	if msgID == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen == nil {
		d.seen = make(map[string]time.Time)
	}
	now := time.Now()
	for k, t := range d.seen {
		if now.Sub(t) > dedupTTL {
			delete(d.seen, k)
		}
	}
	if _, ok := d.seen[msgID]; ok {
		return true
	}
	d.seen[msgID] = now
	return false
}

// msgTime如果早于进程StartTime返回true
// 加入了一个短暂的宽期限(2s)， 避免启动发送消息时出现竞争条件
func IsOldMessage(msgTime time.Time) bool {
	return msgTime.Before(StartTime.Add(-2 * time.Second))
}
