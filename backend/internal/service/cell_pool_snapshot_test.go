package service

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestDerivePoolRunState_Priority(t *testing.T) {
	now := time.Now()
	future := now.Add(5 * time.Minute)
	cases := []struct {
		name string
		acc  Account
		m    *ProviderAccountMetrics
		want string
	}{
		{"error status wins", Account{Status: StatusError, Schedulable: false}, nil, "error"},
		{"disabled", Account{Status: StatusDisabled, Schedulable: true}, nil, "disabled"},
		{"paused = active but not schedulable", Account{Status: StatusActive, Schedulable: false}, nil, "paused"},
		{"temp unschedulable", Account{Status: StatusActive, Schedulable: true, TempUnschedulableUntil: &future}, nil, "temp_unschedulable"},
		{"rate limited", Account{Status: StatusActive, Schedulable: true, RateLimitResetAt: &future}, nil, "rate_limited"},
		{"overloaded", Account{Status: StatusActive, Schedulable: true, OverloadUntil: &future}, nil, "overloaded"},
		{"busy", Account{Status: StatusActive, Schedulable: true}, &ProviderAccountMetrics{ConcurrencyUsed: 1}, "busy"},
		{"idle", Account{Status: StatusActive, Schedulable: true}, &ProviderAccountMetrics{}, "idle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acc := tc.acc
			require.Equal(t, tc.want, derivePoolRunState(&acc, tc.m, now))
		})
	}
}

func TestCellPoolSnapshotService_SnapshotURLAndStartGuard(t *testing.T) {
	cfg := &config.Config{}
	cfg.CellRegistry.URL = "https://portal-api.example/internal/cells/register"
	svc := NewCellPoolSnapshotService(cfg, nil, nil, nil)
	require.Equal(t, "https://portal-api.example/internal/cells/pool-snapshot", svc.snapshotURL())

	cfg.CellRegistry.URL = "https://portal-api.example/internal/cells/"
	require.Equal(t, "https://portal-api.example/internal/cells/pool-snapshot", svc.snapshotURL())

	// 非边缘 / 未接线 → Start 不起 goroutine,Stop 幂等
	svc.Start(t.Context())
	svc.Stop()
	svc.Stop()
	var nilSvc *CellPoolSnapshotService
	nilSvc.Stop()
}
