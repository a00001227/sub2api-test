//go:build unit

package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 切片 4.1 §十一.10-13：真实 Redis + PostgreSQL 的重启/leader 切换/cursor 恢复/retry。
// 复用 workerITEnv（env 未设则 skip）。

func recoveryDeps(store service.RiskV2CycleStore, repo service.UserRiskV2Repository, tRun time.Time, readerCache *riskV2AggCache) service.RiskV2ScoringWorkerDeps {
	return service.RiskV2ScoringWorkerDeps{
		Reader:                readerCache,
		Repo:                  repo,
		Lease:                 NewRiskV2Lease(readerCache.rdb, "s1", "v1"),
		Health:                NewRiskV2IngestionHealthReader(readerCache.rdb, "s1", "v1", func() time.Time { return tRun }),
		CycleStore:            store,
		ScoringConfig:         service.DefaultRiskV2ScoringConfig(),
		FingerprintKeyVersion: "v1",
		Persist:               repo != nil,
		Now:                   func() time.Time { return tRun },
	}
}

// §十一.10/11/13：进程重启/leader 切换后从持久化 cursor 恢复，完成同周期，无重复行。
func TestWorkerIT_RestartCursorRecovery(t *testing.T) {
	db, rdb := workerITEnv(t)
	setupSchema(t, db)
	ctx := context.Background()

	tAgg := time.Date(2026, 6, 1, 12, 1, 0, 0, time.UTC) // cycle 12:05
	cache := itCache(rdb, func() time.Time { return tAgg })
	for _, uid := range []int64{9001, 9002, 9003, 9004} {
		require.NoError(t, cache.Aggregate(ctx, itEnv("it", uid, 1)))
	}
	tRun := time.Date(2026, 6, 1, 12, 5, 20, 0, time.UTC)
	store := NewRiskV2CycleStore(rdb, "s1", "v1", time.Hour, time.Hour, func() time.Time { return tRun })
	repo := NewUserRiskV2Repository(db)

	// Worker #1：MaxUsersPerCycle=2 → 处理 2 个后 INCOMPLETE，持久化 cursor。
	p1 := itParams()
	p1.MaxUsersPerCycle = 2
	p1.BatchSize = 2
	w1 := service.NewRiskV2ScoringWorker(recoveryDeps(store, repo, tRun, itCache(rdb, func() time.Time { return tRun })), p1)
	w1.RunDueCycles(ctx)
	require.EqualValues(t, 1, w1.Metrics().CyclesIncomplete)
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM user_risk_v2 WHERE user_id IN (9001,9002,9003,9004)`).Scan(&n))
	require.Equal(t, 2, n, "worker#1 persisted 2 of 4")

	st, ok, _ := store.LoadCycleState(ctx, time.Date(2026, 6, 1, 12, 5, 0, 0, time.UTC).Unix())
	require.True(t, ok)
	require.Equal(t, service.RiskV2CycleIncomplete, st.Status)
	require.NotEmpty(t, st.Cursor, "cursor persisted for resume")

	// Worker #2（模拟重启/新 leader，不同 lease token）：从 cursor 恢复完成剩余。
	w2 := service.NewRiskV2ScoringWorker(recoveryDeps(store, repo, tRun, itCache(rdb, func() time.Time { return tRun })), itParams())
	w2.RunDueCycles(ctx)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM user_risk_v2 WHERE user_id IN (9001,9002,9003,9004)`).Scan(&n))
	require.Equal(t, 4, n, "all 4 users persisted after resume, no duplicates/omissions")

	st, _, _ = store.LoadCycleState(ctx, time.Date(2026, 6, 1, 12, 5, 0, 0, time.UTC).Unix())
	require.Equal(t, service.RiskV2CycleCompleted, st.Status)
}

// §十一.12：失败用户进入下一周期 retry（真实 Redis 周期 store）。
func TestWorkerIT_FailedUserRetryReal(t *testing.T) {
	_, rdb := workerITEnv(t)
	ctx := context.Background()

	tAgg := time.Date(2026, 6, 1, 12, 1, 0, 0, time.UTC)
	cache := itCache(rdb, func() time.Time { return tAgg })
	for _, uid := range []int64{7001, 7002, 7003} {
		require.NoError(t, cache.Aggregate(ctx, itEnv("it", uid, 1)))
	}
	tRun := time.Date(2026, 6, 1, 12, 5, 20, 0, time.UTC)
	store := NewRiskV2CycleStore(rdb, "s1", "v1", time.Hour, time.Hour, func() time.Time { return tRun })

	// fake repo：对 7002 返回瞬时 DB 错误 → 可重试。
	repo := newFakeRepo()
	repo.errByUser[7002] = errors.New("transient db error")

	w := service.NewRiskV2ScoringWorker(recoveryDeps(store, repo, tRun, itCache(rdb, func() time.Time { return tRun })), itParams())
	w.RunDueCycles(ctx)

	cyc := time.Date(2026, 6, 1, 12, 5, 0, 0, time.UTC).Unix()
	next := cyc + int64(itParams().Interval.Seconds())
	retryUsers, err := store.LoadRetryUsers(ctx, next, 100)
	require.NoError(t, err)
	found := false
	for _, ru := range retryUsers {
		if ru.UserID == 7002 {
			found = true
			require.Equal(t, 1, ru.Attempts)
		}
	}
	require.True(t, found, "failed user 7002 must be queued for next-cycle retry in real Redis store")
}
