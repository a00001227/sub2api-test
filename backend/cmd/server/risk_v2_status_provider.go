package main

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 切片 5：Risk V2 运行态只读状态提供者（接线层实现，持运行组件引用）。
// 只读、低基数；绝不暴露 lease token / Redis key/member / prompt / hmac / api key / secret / DSN / redis 地址。
type riskV2StatusProvider struct {
	cfg           *config.Config
	dispatcher    *service.RiskV2Dispatcher
	worker        *service.RiskV2ScoringWorker
	health        service.RiskV2IngestionHealthReader
	reporterReady bool
	schemaReady   bool
	interval      time.Duration
}

// RuntimeStatus 组装只读快照。V2 完全关闭时仍安全返回（enabled=false）。
func (p *riskV2StatusProvider) RuntimeStatus(ctx context.Context) service.RiskV2RuntimeStatus {
	in := service.RiskV2RuntimeStatusInputs{NowUnix: time.Now().Unix()}
	if p == nil || p.cfg == nil {
		return service.BuildRiskV2RuntimeStatus(in)
	}
	v2 := p.cfg.Risk.V2
	in.Enabled = v2.Enabled
	in.AggregationEnabled = v2.AggregationEnabled
	in.ScoringEnabled = v2.ScoringEnabled
	in.PersistEnabled = v2.ScoringPersistEnabled
	in.DispatcherReady = p.dispatcher != nil
	in.ReporterReady = p.reporterReady
	in.SchemaReady = p.schemaReady
	in.Worker = p.worker

	if p.dispatcher != nil {
		st := p.dispatcher.Stats()
		in.DispatcherStats = &st
	}
	// Ingestion Health：有界读；失败 → 标记 DEGRADED（health_available=false），不影响 status 200。
	if p.health != nil && v2.AggregationActive() {
		iv := int64(p.interval.Seconds())
		if iv <= 0 {
			iv = 300
		}
		cycleEnd := (time.Now().Unix() / iv) * iv
		windowMin := int(p.interval.Minutes())
		if windowMin < 1 {
			windowMin = 1
		}
		hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		h, err := p.health.ReadIngestionHealth(hctx, cycleEnd, windowMin)
		cancel()
		if err != nil {
			in.LastErrorCode = "redis_unavailable"
		} else {
			in.Health = &h
		}
	}
	return service.BuildRiskV2RuntimeStatus(in)
}
