package service

import (
	"context"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

/*
CellSelfHealSeeder(#92 cell 自动健康检测/自愈配置)

背景:sub2api 的定时测试 + 自愈闭环已经存在 —— ScheduledTestRunnerService 周期性
扫「测试计划」执行,测失败会让号进入 error 状态(经 #87 回流成 Portal 的 invalid),
测成功且计划 AutoRecover 时调 RateLimitService.RecoverAccountAfterSuccessfulTest 把
瞬时 error / 限流态清回可调度(同样回流)。但计划本身是中央 admin UI 手建的;cell 上
没有 admin 面、没人建计划,于是 runner 在 cell 上空转,自愈闭环形同虚设。

这个播种器补上这一环:EDGE_MODE 下用一个声明式对账循环,保证「每个在役本地号 desired=
1 个 enabled 计划」。幂等 —— 已有计划的号跳过;新接入的号在下一次 tick 被补上。计划用
AutoRecover=true、ModelID 留空(RunTestBackground 按平台自动取各自 DefaultTestModel)、
每 10 分钟探活一次(见 cellSelfHealCron,足以及时发现吊销/掉线,又不至于烧配额触发风控)。

只在 EDGE_MODE 启动(见 ProvideCellSelfHealSeeder);中央仍走 admin 自建计划,不受影响。
*/

const (
	// 对账间隔:新接入的号最迟这么久后被补上计划。
	cellSelfHealReconcileInterval = 5 * time.Minute
	// 启动后延迟首次对账,避开 boot 期(等 DB / 号加载稳定)。
	cellSelfHealInitialDelay = 30 * time.Second
	// 每个号的探活频率。
	cellSelfHealCron = "*/10 * * * *"
	// 计划保留的历史结果条数(cell 上无需留太多)。
	cellSelfHealMaxResults = 10
	// 一次对账最多处理的号数(cell 池很小,给个宽松上限即可)。
	cellSelfHealListPageSize = 1000
)

// cellSelfHealSkipStatuses:这些状态不主动探活 —— 要么是被人为关停/移除,要么还没上线。
var cellSelfHealSkipStatuses = map[string]bool{
	"paused":     true,
	"removed":    true,
	"draft":      true,
	"onboarding": true,
	"verifying":  true,
}

// cellSeedAccountLister 是播种器需要的最小账号读取面(账号仓储已实现)。
type cellSeedAccountLister interface {
	List(ctx context.Context, params pagination.PaginationParams) ([]Account, *pagination.PaginationResult, error)
}

// cellSeedPlanService 是播种器需要的最小计划面(*ScheduledTestService 已实现)。
type cellSeedPlanService interface {
	ListPlansByAccount(ctx context.Context, accountID int64) ([]*ScheduledTestPlan, error)
	CreatePlan(ctx context.Context, plan *ScheduledTestPlan) (*ScheduledTestPlan, error)
}

// CellSelfHealSeeder periodically ensures every in-service local account has an
// enabled scheduled-test plan, so the existing test + auto-recover loop actually
// runs on a cell. EDGE_MODE only.
type CellSelfHealSeeder struct {
	accounts  cellSeedAccountLister
	scheduled cellSeedPlanService

	stopCh    chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
}

// NewCellSelfHealSeeder creates a seeder. It does not start on its own — the
// provider decides whether to Start() based on EDGE_MODE.
func NewCellSelfHealSeeder(accounts cellSeedAccountLister, scheduled cellSeedPlanService) *CellSelfHealSeeder {
	return &CellSelfHealSeeder{accounts: accounts, scheduled: scheduled}
}

// Start launches the reconcile loop (idempotent — safe to call once).
func (s *CellSelfHealSeeder) Start() {
	if s == nil || s.accounts == nil || s.scheduled == nil {
		return
	}
	s.startOnce.Do(func() {
		s.stopCh = make(chan struct{})
		go s.loop()
		logger.LegacyPrintf("service.cell_self_heal", "[CellSelfHealSeeder] started (interval=%s, cron=%q)", cellSelfHealReconcileInterval, cellSelfHealCron)
	})
}

func (s *CellSelfHealSeeder) loop() {
	// 启动后先等一会,再首次对账。
	select {
	case <-s.stopCh:
		return
	case <-time.After(cellSelfHealInitialDelay):
	}
	s.reconcile()

	t := time.NewTicker(cellSelfHealReconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			s.reconcile()
		}
	}
}

// reconcile ensures each in-service account has at least one plan. Idempotent.
func (s *CellSelfHealSeeder) reconcile() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	accs, _, err := s.accounts.List(ctx, pagination.PaginationParams{Page: 1, PageSize: cellSelfHealListPageSize})
	if err != nil {
		logger.LegacyPrintf("service.cell_self_heal", "[CellSelfHealSeeder] list accounts error: %v", err)
		return
	}

	seeded := 0
	for i := range accs {
		acc := accs[i]
		if cellSelfHealSkipStatuses[acc.Status] {
			continue
		}
		plans, err := s.scheduled.ListPlansByAccount(ctx, acc.ID)
		if err != nil {
			logger.LegacyPrintf("service.cell_self_heal", "[CellSelfHealSeeder] account=%d list plans error: %v", acc.ID, err)
			continue
		}
		if len(plans) > 0 {
			continue // 已有计划,幂等跳过。
		}
		_, err = s.scheduled.CreatePlan(ctx, &ScheduledTestPlan{
			AccountID:      acc.ID,
			ModelID:        "", // 留空 → RunTestBackground 按平台取各自 DefaultTestModel。
			CronExpression: cellSelfHealCron,
			Enabled:        true,
			MaxResults:     cellSelfHealMaxResults,
			AutoRecover:    true,
		})
		if err != nil {
			logger.LegacyPrintf("service.cell_self_heal", "[CellSelfHealSeeder] account=%d create plan error: %v", acc.ID, err)
			continue
		}
		seeded++
	}

	if seeded > 0 {
		logger.LegacyPrintf("service.cell_self_heal", "[CellSelfHealSeeder] seeded %d self-heal test plan(s)", seeded)
	}
}

// Stop halts the reconcile loop.
func (s *CellSelfHealSeeder) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		if s.stopCh != nil {
			close(s.stopCh)
		}
	})
}
