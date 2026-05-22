package core

import (
	"context"
	"math"
	"sync"
	"time"
)

type rateBucket struct {
	timestamps []int64
	lastAccess int64
}

// 实现了一个per-key 滑动窗口速率控制, 追踪key的时间戳,
// 拒绝在时间窗口内配置的请求限制
type RateLimiter struct {
	mu sync.Mutex
	buckets map[string]*rateBucket
	maxMessages int
	windowMs    int64
}

func (rl *RateLimiter) Allow(key string) bool {
	if rl.maxMessages <= 0 {
		return true
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now().UnixMilli()
	b := rl.buckets[key]
	if b == nil {
		b = &rateBucket{}
		rl.buckets[key] = b
	}
	b.lastAccess = now

	cutoff := now - rl.windowMs
	filtered := b.timestamps[:0]
	for _, ts := range b.timestamps {
		if ts > cutoff {
			filtered = append(filtered, ts)
		}
	}
	b.timestamps = filtered

	if len(b.timestamps) >= rl.maxMessages {
		return false
	}
	b.timestamps = append(b.timestamps, now)
	return true
}


type tokenBucket struct {
	tokens     float64
	maxTokens  float64
	refillRate float64 // 每秒多少token
	lastRefill time.Time
}

// 基于上次从上次refill的时间添加token
func (b *tokenBucket) refill() {
	now := time.Now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed <= 0 {
		return
	}
	b.tokens += elapsed * b.refillRate
	if b.tokens > b.maxTokens {
		b.tokens = b.maxTokens
	}
	b.lastRefill = now
}

// 控制超限消息的解析比率显示
type OutgoingRateLimitCfg struct {
	MaxPerSecond float64 // 每秒的消息数量; 0 = 取消 / 不限制
	Burst        int
}

// 判断是否关闭(<=0)
func (c OutgoingRateLimitCfg) disabled() bool {
	return c.MaxPerSecond <= 0
}

func (c OutgoingRateLimitCfg) effectiveBrust() int {
	if c.Burst > 0 {
		return c.Burst
	}
	return int(math.Ceil(c.MaxPerSecond))
}

// 通过每个平台从token bucket来限制发送到平台的出战消息,从不丢弃消息;
// 调用者会一致阻塞知道速率预算允许发送
type OutgoingRateLimiter struct {
	mu       sync.Mutex
	bucket   *tokenBucket
	defaults OutgoingRateLimitCfg
	// overrides map[string]OutgoingRateLimitCfg // 目前只考虑一个平台

}

// 返回(或者lazy 创建) 平台的token bucket, 必须被orl.mu 控制调用
func (orl *OutgoingRateLimiter) bucketFor() *tokenBucket {
	if orl.bucket != nil {
		return orl.bucket
	}
	burst := orl.defaults.effectiveBrust()
	b := &tokenBucket{
		tokens: float64(burst), // 从full开始
	}
	orl.bucket = b
	return b
}

// 等待知道允许发送一条message, 返回nil表示成功,或者context 错误 当等待时ctx取消
func (orl *OutgoingRateLimiter) Wait(ctx context.Context) error {
	cfg := orl.defaults
	if cfg.disabled() {
		return nil
	}

	for {
		orl.mu.Lock()
		b := orl.bucketFor()
		b.refill()

		if b.tokens >= 1 {
			b.tokens--
			orl.mu.Unlock()
			return nil
		}

		// 计算等待时间,知道1token可用
		deficit := 1.0 - b.tokens
		waitSecs := deficit / b.refillRate
		orl.mu.Unlock()

		timer := time.NewTimer(time.Duration(waitSecs * float64(time.Second)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
			// Loop back 尝试消耗一个token
		}
	}
}
