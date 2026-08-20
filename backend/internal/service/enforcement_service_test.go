package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

// —— fakes ——

type fakeEnfStore struct {
	allow      map[int64]bool
	counters   map[int64]int64
	modelCount map[string]int64
	rules      map[string]string
	loadErr    error
	incrErr    error
}

func newFakeEnfStore() *fakeEnfStore {
	return &fakeEnfStore{allow: map[int64]bool{}, counters: map[int64]int64{}, modelCount: map[string]int64{}, rules: map[string]string{}}
}

func (s *fakeEnfStore) LoadAllowlist(ctx context.Context) ([]int64, error) {
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	out := make([]int64, 0, len(s.allow))
	for id, on := range s.allow {
		if on {
			out = append(out, id)
		}
	}
	return out, nil
}
func (s *fakeEnfStore) AddAllowlist(ctx context.Context, userID int64) error {
	s.allow[userID] = true
	return nil
}
func (s *fakeEnfStore) RemoveAllowlist(ctx context.Context, userID int64) error {
	delete(s.allow, userID)
	return nil
}
func (s *fakeEnfStore) IncrThrottleCounter(ctx context.Context, userID int64, ttl time.Duration) (int64, error) {
	if s.incrErr != nil {
		return 0, s.incrErr
	}
	s.counters[userID]++
	return s.counters[userID], nil
}
func (s *fakeEnfStore) IncrModelThrottleCounter(ctx context.Context, userID int64, model string, ttl time.Duration) (int64, error) {
	if s.incrErr != nil {
		return 0, s.incrErr
	}
	k := model
	s.modelCount[k]++
	return s.modelCount[k], nil
}
func (s *fakeEnfStore) LoadModelRules(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range s.rules {
		out[k] = v
	}
	return out, nil
}
func (s *fakeEnfStore) SetModelRule(ctx context.Context, model, action string) error {
	s.rules[model] = action
	return nil
}
func (s *fakeEnfStore) DeleteModelRule(ctx context.Context, model string) error {
	delete(s.rules, model)
	return nil
}

// fakeUserRiskV2Repo 仅实现 ListCurrentAssessments；其余返回零值（满足接口）。
type fakeUserRiskV2Repo struct {
	items   []RiskV2AssessmentListItem
	listErr error
}

func (r *fakeUserRiskV2Repo) UpsertCurrentAssessment(ctx context.Context, userID int64, a RiskV2Assessment) (RiskV2UpsertResult, error) {
	return RiskV2Noop, nil
}
func (r *fakeUserRiskV2Repo) GetCurrentAssessment(ctx context.Context, userID int64) (*RiskV2Assessment, bool, error) {
	return nil, false, nil
}
func (r *fakeUserRiskV2Repo) ListCurrentAssessments(ctx context.Context, filter RiskV2ListFilter, page RiskV2Pagination) ([]RiskV2AssessmentListItem, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	if page.Offset > 0 {
		return nil, nil // 单页足矣
	}
	return r.items, nil
}
func (r *fakeUserRiskV2Repo) DeleteByUserID(ctx context.Context, userID int64) error { return nil }
func (r *fakeUserRiskV2Repo) CheckSchemaReady(ctx context.Context) error             { return nil }

func enfCfg(enabled bool, rpm int) EnforcementConfigView {
	return EnforcementConfigView{Enabled: enabled, ThrottleRPM: rpm, ConfidenceMin: 0.6, RefreshInterval: time.Minute, CounterTTL: time.Minute}
}

// —— tests ——

func TestEnforcement_DisabledGate(t *testing.T) {
	store := newFakeEnfStore()
	repo := &fakeUserRiskV2Repo{items: []RiskV2AssessmentListItem{{UserID: 1, Confidence: 0.9, DataSufficient: true}}}
	svc := NewEnforcementService(store, repo, enfCfg(false, 3))
	if svc.Active() {
		t.Fatal("master off → Active must be false")
	}
	if svc.ShouldThrottle(1) {
		t.Fatal("master off → never throttle")
	}
}

func TestEnforcement_RefreshBuildsHighSetWithConfidenceFloor(t *testing.T) {
	store := newFakeEnfStore()
	repo := &fakeUserRiskV2Repo{items: []RiskV2AssessmentListItem{
		{UserID: 1, Confidence: 0.9, DataSufficient: true},  // 合格
		{UserID: 2, Confidence: 0.5, DataSufficient: true},  // 置信度低于地板 → 排除
		{UserID: 3, Confidence: 0.9, DataSufficient: false}, // 数据不足 → 排除
	}}
	svc := NewEnforcementService(store, repo, enfCfg(true, 3))
	if !svc.Active() {
		t.Fatal("有 HIGH 用户 → Active 应为 true")
	}
	if !svc.ShouldThrottle(1) {
		t.Fatal("user1 合格应被限速")
	}
	if svc.ShouldThrottle(2) {
		t.Fatal("user2 置信度不足不应限速")
	}
	if svc.ShouldThrottle(3) {
		t.Fatal("user3 数据不足不应限速")
	}
}

func TestEnforcement_AllowlistOverrides(t *testing.T) {
	store := newFakeEnfStore()
	store.allow[1] = true
	repo := &fakeUserRiskV2Repo{items: []RiskV2AssessmentListItem{{UserID: 1, Confidence: 0.9, DataSufficient: true}}}
	svc := NewEnforcementService(store, repo, enfCfg(true, 3))
	if svc.ShouldThrottle(1) {
		t.Fatal("豁免用户应一票否决限速")
	}
	if !svc.IsAllowlisted(1) {
		t.Fatal("IsAllowlisted 应为 true")
	}
	// 移出豁免后应恢复限速。
	if err := svc.RemoveAllowlist(context.Background(), 1, 99); err != nil {
		t.Fatal(err)
	}
	if !svc.ShouldThrottle(1) {
		t.Fatal("移出豁免后应恢复限速")
	}
}

func TestEnforcement_ThrottleAfterN(t *testing.T) {
	store := newFakeEnfStore()
	repo := &fakeUserRiskV2Repo{items: []RiskV2AssessmentListItem{{UserID: 1, Confidence: 0.9, DataSufficient: true}}}
	svc := NewEnforcementService(store, repo, enfCfg(true, 3))
	ctx := context.Background()
	// 前 3 次放行（count 1..3 ≤ 3），第 4 次起 429。
	for i := 1; i <= 3; i++ {
		if tr, _ := svc.Throttled(ctx, 1); tr {
			t.Fatalf("第 %d 次不应被限（≤ ThrottleRPM）", i)
		}
	}
	tr, retry := svc.Throttled(ctx, 1)
	if !tr {
		t.Fatal("第 4 次应被限速")
	}
	if retry <= 0 || retry > 60 {
		t.Fatalf("retryAfter 应在 (0,60]，got %d", retry)
	}
}

func TestEnforcement_ThrottleFailOpenOnRedisErr(t *testing.T) {
	store := newFakeEnfStore()
	store.incrErr = errors.New("redis down")
	repo := &fakeUserRiskV2Repo{items: []RiskV2AssessmentListItem{{UserID: 1, Confidence: 0.9, DataSufficient: true}}}
	svc := NewEnforcementService(store, repo, enfCfg(true, 1))
	if tr, _ := svc.Throttled(context.Background(), 1); tr {
		t.Fatal("Redis 故障应 fail-open（不拦截）")
	}
}

func TestEnforcement_ModelRules(t *testing.T) {
	store := newFakeEnfStore()
	repo := &fakeUserRiskV2Repo{items: []RiskV2AssessmentListItem{{UserID: 1, Confidence: 0.9, DataSufficient: true}}}
	svc := NewEnforcementService(store, repo, enfCfg(true, 3))
	ctx := context.Background()

	if svc.HasModelRules() {
		t.Fatal("初始应无规则")
	}
	if err := svc.SetModelRule(ctx, "opus", EnforcementActionBlock, 9); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetModelRule(ctx, "sonnet", EnforcementActionThrottle, 9); err != nil {
		t.Fatal(err)
	}
	// 非法 action 拒绝。
	if err := svc.SetModelRule(ctx, "x", "nope", 9); err == nil {
		t.Fatal("非法 action 应报错")
	}
	if !svc.HasModelRules() {
		t.Fatal("应有规则")
	}
	if a, ok := svc.ModelAction("opus"); !ok || a != EnforcementActionBlock {
		t.Fatalf("opus 应 block, got %s ok=%v", a, ok)
	}
	if _, ok := svc.ModelAction("gpt"); ok {
		t.Fatal("未配置模型应无规则")
	}
	if list := svc.ListModelRules(); len(list) != 2 || list[0].Model != "opus" || list[1].Model != "sonnet" {
		t.Fatalf("规则列表应按名排序 2 条, got %+v", list)
	}
	// 删除。
	if err := svc.DeleteModelRule(ctx, "opus", 9); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.ModelAction("opus"); ok {
		t.Fatal("删除后 opus 不应有规则")
	}
	if svc.Status().ModelRuleCount != 1 {
		t.Fatalf("status 模型规则数应为 1, got %d", svc.Status().ModelRuleCount)
	}
}

func TestEnforcement_ThrottledModelPerModel(t *testing.T) {
	store := newFakeEnfStore()
	repo := &fakeUserRiskV2Repo{items: []RiskV2AssessmentListItem{{UserID: 1, Confidence: 0.9, DataSufficient: true}}}
	svc := NewEnforcementService(store, repo, enfCfg(true, 2))
	ctx := context.Background()
	// 前 2 次放行，第 3 次超限。
	for i := 1; i <= 2; i++ {
		if tr, _ := svc.ThrottledModel(ctx, 1, "opus"); tr {
			t.Fatalf("第 %d 次不应限", i)
		}
	}
	if tr, _ := svc.ThrottledModel(ctx, 1, "opus"); !tr {
		t.Fatal("第 3 次应限速")
	}
	// 另一个模型独立计数。
	if tr, _ := svc.ThrottledModel(ctx, 1, "sonnet"); tr {
		t.Fatal("sonnet 独立桶不应受 opus 影响")
	}
}

func TestEnforcement_ListHighUsersAnnotates(t *testing.T) {
	store := newFakeEnfStore()
	store.allow[2] = true
	repo := &fakeUserRiskV2Repo{items: []RiskV2AssessmentListItem{
		{UserID: 1, Confidence: 0.9, DataSufficient: true},
		{UserID: 2, Confidence: 0.9, DataSufficient: true}, // 豁免
	}}
	svc := NewEnforcementService(store, repo, enfCfg(true, 3))
	users, err := svc.ListHighUsers(context.Background())
	if err != nil || len(users) != 2 {
		t.Fatalf("list high users: %v n=%d", err, len(users))
	}
	byID := map[int64]EnforcementHighUser{}
	for _, u := range users {
		byID[u.UserID] = u
	}
	if !byID[1].Throttled || byID[1].Allowlisted {
		t.Fatalf("user1 应 throttled 且非豁免: %+v", byID[1])
	}
	if byID[2].Throttled || !byID[2].Allowlisted {
		t.Fatalf("user2 应豁免且非 throttled: %+v", byID[2])
	}
}
