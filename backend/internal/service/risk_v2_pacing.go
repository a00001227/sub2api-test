package service

import (
	"context"
	"sync"
	"time"
)

// 切片 4：Token Bucket 节流。目标是把 Redis 读取均匀摊在整个周期内，而不是以 Benchmark 的最快速度突发冲完。
// Clock（now/sleep）注入，单元测试不依赖真实时间。

type RiskV2Pacer struct {
	mu       sync.Mutex
	rate     float64 // tokens/sec
	capacity float64 // 桶容量（突发上限）
	tokens   float64
	last     time.Time
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
}

// NewRiskV2Pacer 构造节流器。ratePerSec<=0 → 不节流（WaitN 立即返回）。
// sleep 允许注入（默认基于 timer + ctx 取消）；now 默认 time.Now。
func NewRiskV2Pacer(ratePerSec int, now func() time.Time, sleep func(context.Context, time.Duration) error) *RiskV2Pacer {
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = ctxSleep
	}
	cap := float64(ratePerSec)
	if cap < 1 {
		cap = 1
	}
	return &RiskV2Pacer{
		rate:     float64(ratePerSec),
		capacity: cap, // 突发上限=1 秒的量，避免一次放行太多
		tokens:   0,   // 从空桶起：第一批也要按速率获得，避免开局突发
		last:     now(),
		now:      now,
		sleep:    sleep,
	}
}

// WaitN 获取 n 个令牌，必要时按速率睡眠。ratePerSec<=0 → 立即返回。ctx 取消 → 返回 ctx.Err。
func (p *RiskV2Pacer) WaitN(ctx context.Context, n int) error {
	if p == nil || p.rate <= 0 || n <= 0 {
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.mu.Lock()
		now := p.now()
		elapsed := now.Sub(p.last).Seconds()
		if elapsed > 0 {
			p.tokens += elapsed * p.rate
			if p.tokens > p.capacity {
				p.tokens = p.capacity
			}
			p.last = now
		}
		need := float64(n)
		if p.tokens >= need {
			p.tokens -= need
			p.mu.Unlock()
			return nil
		}
		deficit := need - p.tokens
		wait := time.Duration(deficit / p.rate * float64(time.Second))
		p.mu.Unlock()
		if wait <= 0 {
			wait = time.Millisecond
		}
		if err := p.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

func ctxSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
