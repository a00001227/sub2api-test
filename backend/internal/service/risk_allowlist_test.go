package service

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

// 记录清理调用的假仓储(审核记录 / 提示词留存 / 蒸馏评分)。
type allowlistPurgeRepo struct {
	contentModerationTestRepo
	mu            sync.Mutex
	delUsers      [][]int64
	delGroups     [][]int64
	groupUsers    map[int64][]int64
	promptUsers   [][]int64
	promptGroups  [][]int64
	riskDeleted   []int64
	listGroupsErr error
}

func (r *allowlistPurgeRepo) DeleteLogsByUserIDs(_ context.Context, ids []int64) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.delUsers = append(r.delUsers, ids)
	return int64(len(ids)), nil
}
func (r *allowlistPurgeRepo) DeleteLogsByGroupIDs(_ context.Context, ids []int64) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.delGroups = append(r.delGroups, ids)
	return int64(len(ids)), nil
}
func (r *allowlistPurgeRepo) ListUserIDsByGroupIDs(_ context.Context, ids []int64) ([]int64, error) {
	if r.listGroupsErr != nil {
		return nil, r.listGroupsErr
	}
	var out []int64
	for _, g := range ids {
		out = append(out, r.groupUsers[g]...)
	}
	return out, nil
}

type allowlistPromptRepo struct{ owner *allowlistPurgeRepo }

func (p allowlistPromptRepo) CreateEvent(context.Context, *PromptAuditEvent) error { return nil }
func (p allowlistPromptRepo) ListEvents(context.Context, PromptAuditEventFilter) ([]PromptAuditEvent, *pagination.PaginationResult, error) {
	return nil, nil, nil
}
func (p allowlistPromptRepo) SummarizeResults(context.Context, PromptAuditEventFilter) (PromptAuditResultSummary, error) {
	return PromptAuditResultSummary{}, nil
}
func (p allowlistPromptRepo) GetEvent(context.Context, int64) (*PromptAuditEvent, error) {
	return nil, nil
}
func (p allowlistPromptRepo) DeleteEvent(context.Context, int64) error                 { return nil }
func (p allowlistPromptRepo) DeleteAll(context.Context) (int64, error)                 { return 0, nil }
func (p allowlistPromptRepo) CleanupExpired(context.Context, time.Time) (int64, error) { return 0, nil }
func (p allowlistPromptRepo) DeleteEventsByUserIDs(_ context.Context, ids []int64) (int64, error) {
	p.owner.mu.Lock()
	defer p.owner.mu.Unlock()
	p.owner.promptUsers = append(p.owner.promptUsers, ids)
	return int64(len(ids)), nil
}
func (p allowlistPromptRepo) DeleteEventsByGroupIDs(_ context.Context, ids []int64) (int64, error) {
	p.owner.mu.Lock()
	defer p.owner.mu.Unlock()
	p.owner.promptGroups = append(p.owner.promptGroups, ids)
	return int64(len(ids)), nil
}

type allowlistRiskRepo struct {
	fakeUserRiskV2Repo
	owner *allowlistPurgeRepo
}

func (r *allowlistRiskRepo) DeleteByUserID(_ context.Context, id int64) error {
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	r.owner.riskDeleted = append(r.owner.riskDeleted, id)
	return nil
}

func newAllowlistTestService(t *testing.T, users, groups []int64) (*ContentModerationService, *allowlistPurgeRepo) {
	t.Helper()
	cfg := defaultContentModerationConfig()
	cfg.AllowedUserIDs = users
	cfg.AllowedGroupIDs = groups
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	repo := &allowlistPurgeRepo{groupUsers: map[int64][]int64{}}
	svc := &ContentModerationService{
		settingRepo: &contentModerationTestSettingRepo{values: map[string]string{
			SettingKeyRiskControlEnabled:      "true",
			SettingKeyContentModerationConfig: string(raw),
		}},
		repo:      repo,
		hashCache: &contentModerationTestHashCache{hashes: map[string]struct{}{}},
		keyHealth: make(map[string]*contentModerationKeyHealth),
	}
	svc.SetAllowlistCleanupDeps(allowlistPromptRepo{owner: repo}, &allowlistRiskRepo{owner: repo})
	return svc, repo
}

func alI64p(v int64) *int64 { return &v }

func TestRiskAllowlist_IsAllowlisted_UserAndGroup(t *testing.T) {
	svc, _ := newAllowlistTestService(t, []int64{7}, []int64{30})
	ctx := context.Background()
	require.True(t, svc.IsAllowlisted(ctx, 7, nil), "user hit")
	require.True(t, svc.IsAllowlisted(ctx, 99, i64p(30)), "group hit")
	require.False(t, svc.IsAllowlisted(ctx, 99, i64p(31)), "neither")
	require.False(t, svc.IsAllowlisted(ctx, 0, nil), "no identity")
	require.False(t, (*ContentModerationService)(nil).IsAllowlisted(ctx, 7, nil), "nil service fail-closed")
}

func TestRiskAllowlist_UpdateConfig_PurgesOnlyNewlyAdded(t *testing.T) {
	svc, repo := newAllowlistTestService(t, []int64{7}, nil)
	repo.groupUsers[30] = []int64{41, 42}
	ctx := context.Background()

	users := []int64{7, 8} // 7 已在名单,只有 8 是新加
	groups := []int64{30}
	_, err := svc.UpdateConfig(ctx, UpdateContentModerationConfigInput{AllowedUserIDs: &users, AllowedGroupIDs: &groups})
	require.NoError(t, err)

	// 审核记录:新用户 8 + 分组展开的 41/42;分组 30 直接按 group_id 再删一遍。
	require.Len(t, repo.delUsers, 1)
	require.ElementsMatch(t, []int64{8, 41, 42}, repo.delUsers[0])
	require.Equal(t, [][]int64{{30}}, repo.delGroups)
	// 提示词留存同口径。
	require.Len(t, repo.promptUsers, 1)
	require.ElementsMatch(t, []int64{8, 41, 42}, repo.promptUsers[0])
	require.Equal(t, [][]int64{{30}}, repo.promptGroups)
	// 蒸馏评分按用户逐个删。
	require.ElementsMatch(t, []int64{8, 41, 42}, repo.riskDeleted)

	// 快照缓存已失效:新名单立刻生效。
	require.True(t, svc.IsAllowlisted(ctx, 8, nil))
	require.True(t, svc.IsAllowlisted(ctx, 1, i64p(30)))

	// 再保存一次同样的名单 → 没有新增 → 不再清理。
	before := len(repo.delUsers)
	_, err = svc.UpdateConfig(ctx, UpdateContentModerationConfigInput{AllowedUserIDs: &users, AllowedGroupIDs: &groups})
	require.NoError(t, err)
	require.Len(t, repo.delUsers, before)
}

func TestRiskAllowlist_Check_GroupAllowSkipsEverything(t *testing.T) {
	svc, repo := newAllowlistTestService(t, nil, []int64{30})
	cfg, err := svc.loadConfig(context.Background())
	require.NoError(t, err)
	cfg.Enabled = true
	cfg.Mode = ContentModerationModeObserve
	raw, _ := json.Marshal(cfg)
	require.NoError(t, svc.settingRepo.Set(context.Background(), SettingKeyContentModerationConfig, string(raw)))

	dec, err := svc.Check(context.Background(), ContentModerationCheckInput{
		UserID:   5,
		GroupID:  i64p(30),
		Endpoint: "/v1/messages",
		Protocol: ContentModerationProtocolAnthropicMessages,
		Body:     []byte(`{"messages":[{"role":"user","content":"anything"}]}`),
	})
	require.NoError(t, err)
	require.NotNil(t, dec)
	require.False(t, dec.Flagged)
	require.Empty(t, repo.logs, "allowlisted group must not be recorded")
}

func TestRiskAllowlist_PromptAuditCaptureSkips(t *testing.T) {
	svc, repo := newAllowlistTestService(t, []int64{7}, []int64{30})
	pa := &PromptAuditService{
		settingRepo: &contentModerationTestSettingRepo{values: map[string]string{
			SettingKeyPromptAuditConfig: `{"enabled":true,"retention_days":30}`,
		}},
		repo:  allowlistPromptRepo{owner: repo},
		queue: make(chan promptAuditTask, 8),
	}
	pa.SetAllowlist(svc)
	ctx := context.Background()

	pa.Capture(ctx, pa.BuildTask("r1", i64p(7), "", nil, "", nil, "", "anthropic", "/v1/messages", "anthropic", "m", []byte("{}")))
	pa.Capture(ctx, pa.BuildTask("r2", i64p(9), "", nil, "", i64p(30), "", "anthropic", "/v1/messages", "anthropic", "m", []byte("{}")))
	pa.Capture(ctx, pa.BuildTask("r3", i64p(9), "", nil, "", i64p(31), "", "anthropic", "/v1/messages", "anthropic", "m", []byte("{}")))
	require.Equal(t, 1, len(pa.queue), "only the non-allowlisted request is queued")
	require.Equal(t, int64(2), pa.skippedAllowlisted.Load())
}

func TestDiffAddedInt64(t *testing.T) {
	require.Equal(t, []int64{3, 4}, diffAddedInt64([]int64{1, 2}, []int64{1, 3, 4}))
	require.Empty(t, diffAddedInt64([]int64{1}, []int64{1}))
	require.Empty(t, diffAddedInt64([]int64{1}, nil))
}
