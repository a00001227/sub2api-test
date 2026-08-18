package admin

import (
	"sync"
	"time"
)

// 切片 5.1 §二：Admin Live Window 进程内令牌桶限流（每管理员 + 全局轻量）。
// 有界状态 + 自动过期；不引入 Redis 限流系统。clock 注入以便测试。

type rlBucket struct {
	tokens float64
	last   time.Time
	seen   time.Time
}

type riskV2RateLimiter struct {
	mu                      sync.Mutex
	rate, burst             float64 // 每管理员
	globalRate, globalBurst float64
	perKey                  map[string]*rlBucket
	global                  *rlBucket
	now                     func() time.Time
	lastGC                  time.Time
}

const (
	riskV2RateDefaultPerSec = 5
	riskV2RateDefaultBurst  = 10
	riskV2RateKeyTTL        = 10 * time.Minute
	riskV2RateGCInterval    = time.Minute
	riskV2RateMaxKeys       = 10000
)

func newRiskV2RateLimiter(ratePerSec, burst int, now func() time.Time) *riskV2RateLimiter {
	if ratePerSec <= 0 {
		ratePerSec = riskV2RateDefaultPerSec
	}
	if burst <= 0 {
		burst = riskV2RateDefaultBurst
	}
	if now == nil {
		now = time.Now
	}
	r := float64(ratePerSec)
	b := float64(burst)
	return &riskV2RateLimiter{
		rate: r, burst: b,
		globalRate: r * 8, globalBurst: b * 8, // 全局比单管理员宽，仅防总量失控
		perKey: map[string]*rlBucket{},
		global: &rlBucket{tokens: b * 8},
		now:    now,
	}
}

func consume(b *rlBucket, rate, burst float64, now time.Time) bool {
	if b.last.IsZero() {
		b.last = now
		b.tokens = burst
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * rate
		if b.tokens > burst {
			b.tokens = burst
		}
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// Allow 消费一个令牌：先每管理员，再全局；任一不足 → false（被限流）。
func (l *riskV2RateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.gc(now)
	b := l.perKey[key]
	if b == nil {
		b = &rlBucket{tokens: l.burst, last: now}
		l.perKey[key] = b
	}
	b.seen = now
	if !consume(b, l.rate, l.burst, now) {
		return false
	}
	return consume(l.global, l.globalRate, l.globalBurst, now)
}

// gc 有界化：定期清理过期 key；超上限则整表重置（有界，粗粒度）。
func (l *riskV2RateLimiter) gc(now time.Time) {
	if now.Sub(l.lastGC) < riskV2RateGCInterval && len(l.perKey) <= riskV2RateMaxKeys {
		return
	}
	l.lastGC = now
	if len(l.perKey) > riskV2RateMaxKeys {
		l.perKey = map[string]*rlBucket{}
		return
	}
	for k, b := range l.perKey {
		if now.Sub(b.seen) > riskV2RateKeyTTL {
			delete(l.perKey, k)
		}
	}
}
