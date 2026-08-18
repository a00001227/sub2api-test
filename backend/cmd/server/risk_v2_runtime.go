package main

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// 切片 4.1：Risk V2 Shadow 运行态构造（非生成源码，wire_gen.go 只调用本函数）。
//
// 启动顺序（§一.3）：Dispatcher/Aggregation（调用方先建）→ Health Reporter → Scoring Worker。
// 全部 flags 门控；任一前置不满足 → 返回 nil（不启动 goroutine）。persist 模式 Schema 不就绪 → Worker 不启动、DEGRADED，主程序继续。
//
// 返回的 loop / worker 供 provideCleanup 按关闭顺序停止。二者可能为 nil（nil-safe Stop）。
func provideRiskV2Runtime(
	cfg *config.Config,
	rdb *redis.Client,
	db *sql.DB,
	dispatcher *service.RiskV2Dispatcher,
	params service.RiskV2WorkerParams,
) (*service.RiskV2HealthReportLoop, *service.RiskV2ScoringWorker) {
	// 前置：master + aggregation 生效，且 Redis 可用（Health Reporter 与 Worker 都依赖 Redis）。
	if cfg == nil || rdb == nil || !cfg.Risk.V2.AggregationActive() {
		return nil, nil
	}
	schema := service.RiskV2AggSchemaVersion
	fpVer := cfg.Risk.V2.FingerprintKeyVersion

	// —— Health Reporter（每实例定期上报 dispatcher delta）——
	instanceID := service.NewRiskV2InstanceID()
	reporter := repository.NewRiskV2HealthReporter(rdb, schema, fpVer, instanceID, nil)
	healthLoop := service.NewRiskV2HealthReportLoop(dispatcher, reporter, 30*time.Second, nil)
	healthLoop.Start()

	// —— Scoring Worker（仅 scoring 生效时）——
	if !cfg.Risk.V2.ScoringActive() {
		return healthLoop, nil
	}

	persist := cfg.Risk.V2.ScoringPersistActive()
	var repo service.UserRiskV2Repository
	if persist {
		// §三 Schema readiness：persist 前只读探针；不就绪 → Worker 不启动（DEGRADED），主程序继续。
		r := repository.NewUserRiskV2Repository(db)
		if err := r.CheckSchemaReady(context.Background()); err != nil {
			log.Printf("[risk_v2.worker] persist enabled but user_risk_v2 schema not ready → worker NOT started (DEGRADED); main service continues: %v", err)
			return healthLoop, nil
		}
		repo = r
	}

	deps := service.RiskV2ScoringWorkerDeps{
		Reader:                repository.NewRiskV2ScoringReader(rdb, schema, fpVer),
		Repo:                  repo,
		Lease:                 repository.NewRiskV2Lease(rdb, schema, fpVer),
		Health:                repository.NewRiskV2IngestionHealthReader(rdb, schema, fpVer, nil),
		CycleStore:            repository.NewRiskV2CycleStore(rdb, schema, fpVer, cfg.Risk.V2.Worker.CycleStateTTL(), cfg.Risk.V2.Worker.RetryTTL(), nil),
		ScoringConfig:         service.DefaultRiskV2ScoringConfig(),
		FingerprintKeyVersion: fpVer,
		Persist:               persist,
		// 动态开关：运行中 scoring 被关掉 → Worker 立即中止当前周期。
		EnabledFn:   func() bool { return cfg.Risk.V2.ScoringActive() },
		MetricsSink: service.NewRiskV2WorkerLogSink(),
	}
	worker := service.NewRiskV2ScoringWorker(deps, params)
	if !worker.Ready() {
		log.Printf("[risk_v2.worker] preconditions not met → DEGRADED, not started")
		return healthLoop, nil
	}
	worker.Start(context.Background())
	return healthLoop, worker
}
