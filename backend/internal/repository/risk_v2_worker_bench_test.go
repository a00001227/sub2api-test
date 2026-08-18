//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Worker 纯评分+持久化开销基准（fakes，无 I/O 延迟）：衡量每用户 scoring+upsert 的 CPU 成本。
func BenchmarkWorkerCycle_1kUsers(b *testing.B) {
	now := time.Date(2026, 3, 1, 12, 6, 5, 0, time.UTC)
	p := defaultWorkerParams()
	p.MaxUsersPerSecond = 0 // 关闭节流，测纯计算
	iv := int64(p.Interval.Seconds())
	cyc := (now.Add(-p.GraceDelay).Unix() / iv) * iv
	users := make([]int64, 1000)
	for i := range users {
		users[i] = int64(i + 1)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reader := newFakeReader()
		reader.setCycle(cyc, users)
		for _, u := range users {
			reader.snaps[u] = strongSnapshot(u)
		}
		repo := newFakeRepo()
		w := service.NewRiskV2ScoringWorker(service.RiskV2ScoringWorkerDeps{
			Reader: reader, Lease: &fakeLease{}, Health: healthyHealth(), Repo: repo,
			ScoringConfig: service.DefaultRiskV2ScoringConfig(), FingerprintKeyVersion: "v1",
			Persist: true, Now: func() time.Time { return now },
		}, p)
		w.RunDueCycles(context.Background())
	}
}
