package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

type stubSeedAccountLister struct {
	accounts []Account
}

func (s *stubSeedAccountLister) List(ctx context.Context, params pagination.PaginationParams) ([]Account, *pagination.PaginationResult, error) {
	return s.accounts, &pagination.PaginationResult{Total: int64(len(s.accounts))}, nil
}

type stubSeedPlanService struct {
	existing map[int64][]*ScheduledTestPlan // accountID -> plans already present
	created  []*ScheduledTestPlan
}

func (s *stubSeedPlanService) ListPlansByAccount(ctx context.Context, accountID int64) ([]*ScheduledTestPlan, error) {
	return s.existing[accountID], nil
}

func (s *stubSeedPlanService) CreatePlan(ctx context.Context, plan *ScheduledTestPlan) (*ScheduledTestPlan, error) {
	s.created = append(s.created, plan)
	if s.existing == nil {
		s.existing = map[int64][]*ScheduledTestPlan{}
	}
	s.existing[plan.AccountID] = append(s.existing[plan.AccountID], plan)
	return plan, nil
}

func TestCellSelfHealSeeder_SeedsInServiceAccountsOnce(t *testing.T) {
	lister := &stubSeedAccountLister{accounts: []Account{
		{ID: 1, Status: "active"},
		{ID: 2, Status: "rate_limited"},
		{ID: 3, Status: "invalid"},
		{ID: 4, Status: "paused"},     // skipped
		{ID: 5, Status: "removed"},    // skipped
		{ID: 6, Status: "onboarding"}, // skipped
	}}
	plans := &stubSeedPlanService{}
	seeder := NewCellSelfHealSeeder(lister, plans)

	seeder.reconcile()

	if len(plans.created) != 3 {
		t.Fatalf("expected 3 plans seeded (active/rate_limited/invalid), got %d", len(plans.created))
	}
	seededFor := map[int64]bool{}
	for _, p := range plans.created {
		seededFor[p.AccountID] = true
		if !p.Enabled || !p.AutoRecover {
			t.Errorf("plan for account=%d must be Enabled+AutoRecover, got enabled=%v autorecover=%v", p.AccountID, p.Enabled, p.AutoRecover)
		}
		if p.ModelID != "" {
			t.Errorf("plan for account=%d must leave ModelID empty (platform auto-default), got %q", p.AccountID, p.ModelID)
		}
		if p.CronExpression != cellSelfHealCron {
			t.Errorf("plan for account=%d cron = %q, want %q", p.AccountID, p.CronExpression, cellSelfHealCron)
		}
	}
	for _, id := range []int64{1, 2, 3} {
		if !seededFor[id] {
			t.Errorf("expected account=%d to be seeded", id)
		}
	}
	for _, id := range []int64{4, 5, 6} {
		if seededFor[id] {
			t.Errorf("account=%d has a skip status and must NOT be seeded", id)
		}
	}

	// Second pass must be idempotent — no new plans.
	before := len(plans.created)
	seeder.reconcile()
	if len(plans.created) != before {
		t.Fatalf("reconcile must be idempotent: created grew from %d to %d", before, len(plans.created))
	}
}
