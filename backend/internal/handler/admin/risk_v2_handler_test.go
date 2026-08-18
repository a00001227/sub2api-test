package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// —— fakes 实现 service 接口 ——

type fakeV2Repo struct {
	schemaErr  error
	list       []service.RiskV2AssessmentListItem
	lastFilter service.RiskV2ListFilter
	lastPage   service.RiskV2Pagination
	detail     *service.RiskV2Assessment
	detailErr  error
	listErr    error
}

func (r *fakeV2Repo) UpsertCurrentAssessment(ctx context.Context, uid int64, a service.RiskV2Assessment) (service.RiskV2UpsertResult, error) {
	return service.RiskV2Noop, nil
}
func (r *fakeV2Repo) GetCurrentAssessment(ctx context.Context, uid int64) (*service.RiskV2Assessment, bool, error) {
	if r.detailErr != nil {
		return nil, false, r.detailErr
	}
	if r.detail == nil {
		return nil, false, nil
	}
	return r.detail, true, nil
}
func (r *fakeV2Repo) ListCurrentAssessments(ctx context.Context, f service.RiskV2ListFilter, p service.RiskV2Pagination) ([]service.RiskV2AssessmentListItem, error) {
	r.lastFilter, r.lastPage = f, p
	return r.list, r.listErr
}
func (r *fakeV2Repo) DeleteByUserID(ctx context.Context, uid int64) error { return nil }
func (r *fakeV2Repo) CheckSchemaReady(ctx context.Context) error          { return r.schemaErr }

type fakeV2Users struct {
	calls     int32
	lastIDs   []int64
	summaries map[int64]service.RiskV2UserSummary
}

func (u *fakeV2Users) GetSummariesByIDs(ctx context.Context, ids []int64) (map[int64]service.RiskV2UserSummary, error) {
	atomic.AddInt32(&u.calls, 1)
	u.lastIDs = ids
	if u.summaries != nil {
		return u.summaries, nil
	}
	m := map[int64]service.RiskV2UserSummary{}
	for _, id := range ids {
		m[id] = service.RiskV2UserSummary{UserID: id, Email: "u@x", Username: "user", Status: "active", Known: true}
	}
	return m, nil
}

// fakeV2Live 实现 §5.1 轻量 RiskV2AdminSummaryReader（ReadForScoring）。
type fakeV2Live struct {
	calls int32
	err   error
	snap  service.RiskV2ScoringSnapshot
	block chan struct{} // 非 nil 时阻塞直到关闭（测试并发上限）
}

func (l *fakeV2Live) ReadForScoring(ctx context.Context, uid int64) (service.RiskV2ScoringSnapshot, error) {
	atomic.AddInt32(&l.calls, 1)
	if l.block != nil {
		select {
		case <-l.block:
		case <-ctx.Done():
			return service.RiskV2ScoringSnapshot{}, ctx.Err()
		}
	}
	if l.err != nil {
		return service.RiskV2ScoringSnapshot{}, l.err
	}
	return l.snap, nil
}

type fakeV2Status struct{ st service.RiskV2RuntimeStatus }

func (s *fakeV2Status) RuntimeStatus(ctx context.Context) service.RiskV2RuntimeStatus { return s.st }

// —— test harness ——

func newV2Engine(t *testing.T, repo *fakeV2Repo, users *fakeV2Users, live *fakeV2Live, status *fakeV2Status, authCode int) (*gin.Engine, *RiskV2Handler) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	fixed := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	svc := service.NewRiskV2AdminService(repo, users, live, status, time.Hour, 200*time.Millisecond, func() time.Time { return fixed })
	h := NewRiskV2Handler(svc, 1000, 1000)
	e := gin.New()
	grp := e.Group("/admin")
	// 复用「现有 admin 鉴权」的契约：这里用 stub 中间件模拟 401/403/200 的边界。
	grp.Use(func(c *gin.Context) {
		if authCode == http.StatusUnauthorized {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		if authCode == http.StatusForbidden {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		c.Next()
	})
	v2 := grp.Group("/risk/v2")
	v2.GET("/status", h.GetStatus)
	v2.GET("/users", h.ListUsers)
	v2.GET("/users/:user_id", h.GetUser)
	v2.GET("/users/:user_id/windows", h.GetWindows)
	return e, h
}

func do(e *gin.Engine, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	e.ServeHTTP(w, req)
	return w
}

func strongDetail() *service.RiskV2Assessment {
	return &service.RiskV2Assessment{
		RiskIndex: 82, RiskTier: "HIGH", Confidence: 0.9, DataSufficient: true,
		Automation:        service.RiskV2PlaneScore{Score: 70, Available: true},
		Harvest:           service.RiskV2PlaneScore{Available: false}, // nullable plane
		Campaign:          service.RiskV2PlaneScore{Score: 40, Available: true},
		Exposure:          service.RiskV2PlaneScore{Score: 55, Available: true},
		EvidenceFamilies:  []service.RiskV2EvidenceFamily{{Family: "PROMPT_PATTERN", Group: "PROMPT_PATTERN", Available: true, Strength: 80, MeetsHigh: true, Window: "1h"}},
		EvidenceGroups:    []service.RiskV2EvidenceGroup{{Group: "PROMPT_PATTERN", MetHigh: true, Strength: 80}},
		ReasonCodes:       []service.RiskV2ReasonCode{{Code: "scaffold_reuse_high", Window: "1h", ObservedValue: 0.9, Threshold: 0.7, EvidenceFamily: "PROMPT_PATTERN", EvidenceGroup: "PROMPT_PATTERN", ConfidenceContribution: 0.3}},
		IncompleteReasons: []string{},
		HealthAvailable:   true,
		FeatureVersion:    "score-v2", PolicyVersion: "shadow-uncalibrated-1", FingerprintKeyVersion: "v1",
		AssessedAtUnix: time.Date(2026, 7, 1, 11, 59, 30, 0, time.UTC).Unix(), EffectiveAction: "NONE",
	}
}

// §十五.1/2/3：RBAC。
func TestRiskV2API_RBAC(t *testing.T) {
	for _, tc := range []struct{ code, want int }{{http.StatusUnauthorized, 401}, {http.StatusForbidden, 403}, {http.StatusOK, 200}} {
		e, _ := newV2Engine(t, &fakeV2Repo{}, &fakeV2Users{}, &fakeV2Live{}, &fakeV2Status{}, tc.code)
		w := do(e, "/admin/risk/v2/status")
		require.Equal(t, tc.want, w.Code)
	}
}

// §十五.4/27：Runtime disabled 仍 200；无 lease token。
func TestRiskV2API_StatusDisabled(t *testing.T) {
	e, _ := newV2Engine(t, &fakeV2Repo{}, &fakeV2Users{}, &fakeV2Live{}, &fakeV2Status{st: service.RiskV2RuntimeStatus{Enabled: false}}, http.StatusOK)
	w := do(e, "/admin/risk/v2/status")
	require.Equal(t, 200, w.Code)
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	var m map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &m))
	require.Equal(t, true, m["available"])
	require.Equal(t, false, m["enabled"])
	require.Equal(t, true, m["shadow"])
	require.NotContains(t, strings.ToLower(w.Body.String()), "token")
}

// §十五.5：Runtime DEGRADED（worker_degraded / health unavailable）。
func TestRiskV2API_StatusDegraded(t *testing.T) {
	st := service.RiskV2RuntimeStatus{Enabled: true, AggregationEnabled: true, ScoringEnabled: true, WorkerDegraded: true, HealthAvailable: false, LastErrorCode: "redis_unavailable"}
	e, _ := newV2Engine(t, &fakeV2Repo{}, &fakeV2Users{}, &fakeV2Live{}, &fakeV2Status{st: st}, http.StatusOK)
	w := do(e, "/admin/risk/v2/status")
	require.Equal(t, 200, w.Code)
	var m map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &m))
	require.Equal(t, true, m["worker_degraded"])
	require.Equal(t, false, m["health_available"])
}

// §十五.6：Schema missing → 503 RISK_V2_SCHEMA_NOT_READY（List/Detail）。
func TestRiskV2API_SchemaMissing(t *testing.T) {
	repo := &fakeV2Repo{schemaErr: errors.New("no such table")}
	e, _ := newV2Engine(t, repo, &fakeV2Users{}, &fakeV2Live{}, &fakeV2Status{}, http.StatusOK)
	for _, p := range []string{"/admin/risk/v2/users", "/admin/risk/v2/users/7"} {
		w := do(e, p)
		require.Equal(t, 503, w.Code)
		var ae RiskV2APIError
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &ae))
		require.Equal(t, riskV2ErrSchemaNotReady, ae.Code)
	}
}

// §十五.7：Dry Run 空列表（不解释为全部低风险）。
func TestRiskV2API_DryRunEmptyList(t *testing.T) {
	e, _ := newV2Engine(t, &fakeV2Repo{list: nil}, &fakeV2Users{}, &fakeV2Live{}, &fakeV2Status{}, http.StatusOK)
	w := do(e, "/admin/risk/v2/users")
	require.Equal(t, 200, w.Code)
	var resp RiskV2AssessmentListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Empty(t, resp.Items)
	require.Equal(t, true, resp.Meta.Shadow)
}

// §十五.8/9/10/11/12：分页 + 最大 limit + tier/index/fresh 过滤翻译。
func TestRiskV2API_ListFiltersAndPagination(t *testing.T) {
	repo := &fakeV2Repo{}
	e, _ := newV2Engine(t, repo, &fakeV2Users{}, &fakeV2Live{}, &fakeV2Status{}, http.StatusOK)
	// limit 超上限 → 夹到 200；tier / min_risk_index / freshness=fresh。
	w := do(e, "/admin/risk/v2/users?limit=9999&tier=HIGH&min_risk_index=50&freshness=fresh")
	require.Equal(t, 200, w.Code)
	// limit 夹到 200；service 取 limit+1=201 探测 has_more。
	require.Equal(t, 201, repo.lastPage.Limit)
	require.Equal(t, "HIGH", repo.lastFilter.Tier)
	require.NotNil(t, repo.lastFilter.MinRiskIndex)
	require.InDelta(t, 50, *repo.lastFilter.MinRiskIndex, 1e-9)
	require.Greater(t, repo.lastFilter.AssessedFromUnix, int64(0), "fresh → assessed_from boundary pushed to SQL")

	// freshness=stale → assessed_to boundary。
	repo2 := &fakeV2Repo{}
	e2, _ := newV2Engine(t, repo2, &fakeV2Users{}, &fakeV2Live{}, &fakeV2Status{}, http.StatusOK)
	do(e2, "/admin/risk/v2/users?freshness=stale")
	require.Greater(t, repo2.lastFilter.AssessedToUnix, int64(0), "stale → assessed_to boundary pushed to SQL")
}

// §十五.14/28：无 N+1（批量一次） + List 不调用重量级 Live Reader。
func TestRiskV2API_NoNPlusOneAndNoLiveReader(t *testing.T) {
	repo := &fakeV2Repo{list: []service.RiskV2AssessmentListItem{
		{UserID: 1, RiskTier: "HIGH", EffectiveAction: "NONE"}, {UserID: 2, RiskTier: "WATCH", EffectiveAction: "NONE"}, {UserID: 3, RiskTier: "MEDIUM", EffectiveAction: "NONE"},
	}}
	users := &fakeV2Users{}
	live := &fakeV2Live{}
	e, _ := newV2Engine(t, repo, users, live, &fakeV2Status{}, http.StatusOK)
	w := do(e, "/admin/risk/v2/users")
	require.Equal(t, 200, w.Code)
	require.EqualValues(t, 1, atomic.LoadInt32(&users.calls), "user summaries must be fetched in a single batch (no N+1)")
	require.ElementsMatch(t, []int64{1, 2, 3}, users.lastIDs)
	require.EqualValues(t, 0, atomic.LoadInt32(&live.calls), "list must NOT call heavy live redis reader")
}

// §十五.15/16/17/18/19/20：Detail found/notfound/nullable/evidence DTO/EffectiveAction NONE/risk_index 非概率。
func TestRiskV2API_DetailAndSemantics(t *testing.T) {
	repo := &fakeV2Repo{detail: strongDetail()}
	e, _ := newV2Engine(t, repo, &fakeV2Users{}, &fakeV2Live{}, &fakeV2Status{}, http.StatusOK)
	w := do(e, "/admin/risk/v2/users/7")
	require.Equal(t, 200, w.Code)
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	var resp RiskV2AssessmentDetailResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, "HIGH", resp.RiskTier)
	require.Equal(t, "NONE", resp.EffectiveAction)
	require.Nil(t, resp.Harvest.Score, "unavailable plane score must be null")
	require.False(t, resp.Harvest.Available)
	require.NotNil(t, resp.Automation.Score)
	require.Len(t, resp.ReasonCodes, 1)
	require.Equal(t, "scaffold_reuse_high", resp.ReasonCodes[0].Code)
	require.Len(t, resp.EvidenceFamilies, 1)
	// risk_index 非概率：无 probability 键；meta 明示。
	var raw map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	_, hasProb := raw["probability"]
	require.False(t, hasProb)
	require.Contains(t, raw, "risk_index")
	meta := raw["meta"].(map[string]any)
	require.Equal(t, false, meta["risk_index_is_probability"])
	require.Equal(t, false, meta["enforcement"])

	// not found → 404。
	repo2 := &fakeV2Repo{detail: nil}
	e2, _ := newV2Engine(t, repo2, &fakeV2Users{}, &fakeV2Live{}, &fakeV2Status{}, http.StatusOK)
	w2 := do(e2, "/admin/risk/v2/users/7")
	require.Equal(t, 404, w2.Code)
	var ae RiskV2APIError
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &ae))
	require.Equal(t, riskV2ErrAssessmentNotFnd, ae.Code)
}

// §十五.21：Live Window 正常（用户级 + multikey；无 per-key/HMAC）。
func TestRiskV2API_LiveWindowOK(t *testing.T) {
	// §5.1：轻量 snapshot（用户级 + multikey，结构上无 per-key）。
	snap := service.RiskV2ScoringSnapshot{
		UserID: 7, FingerprintKeyVersion: "v1", AssessedAtUnix: 123,
		User:     service.RiskV2EntityWindows{W1h: service.RiskV2Window{WindowLabel: "1h", RequestCount: 10}},
		MultiKey: service.RiskV2MultiKeyRollup{MultiKeyAvailable: true, ActiveAPIKeyCount24h: 3},
	}
	e, _ := newV2Engine(t, &fakeV2Repo{}, &fakeV2Users{}, &fakeV2Live{snap: snap}, &fakeV2Status{}, http.StatusOK)
	w := do(e, "/admin/risk/v2/users/7/windows")
	require.Equal(t, 200, w.Code)
	var resp RiskV2WindowSummaryResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.EqualValues(t, 10, resp.W1h.RequestCount)
	require.True(t, resp.W1h.Available)
	require.True(t, resp.DataAvailable)
	require.True(t, resp.MultiKey.MultiKeyAvailable)
	require.Equal(t, 3, resp.MultiKey.ActiveAPIKeyCount24h)
	// 响应体不含 per-key map / hmac。
	require.NotContains(t, w.Body.String(), "per_api_key")
	require.NotContains(t, strings.ToLower(w.Body.String()), "hmac")
}

// §五：Redis 正常但用户无近期数据 → 200 + data_available=false（不伪装成零风险完整测量）。
func TestRiskV2API_LiveWindowNoData(t *testing.T) {
	snap := service.RiskV2ScoringSnapshot{UserID: 7, FingerprintKeyVersion: "v1"} // 全零窗口
	e, _ := newV2Engine(t, &fakeV2Repo{}, &fakeV2Users{}, &fakeV2Live{snap: snap}, &fakeV2Status{}, http.StatusOK)
	w := do(e, "/admin/risk/v2/users/7/windows")
	require.Equal(t, 200, w.Code)
	var resp RiskV2WindowSummaryResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.False(t, resp.DataAvailable, "no recent data must be data_available=false, not fake zero-risk windows")
	require.False(t, resp.W1h.Available)
}

// §十五.22/23/24：Live Redis timeout/unavailable → 503；DB List/Detail 不受影响。
func TestRiskV2API_LiveUnavailableDoesNotAffectDB(t *testing.T) {
	repo := &fakeV2Repo{detail: strongDetail(), list: []service.RiskV2AssessmentListItem{{UserID: 1, EffectiveAction: "NONE"}}}
	live := &fakeV2Live{err: errors.New("redis down")}
	e, _ := newV2Engine(t, repo, &fakeV2Users{}, live, &fakeV2Status{}, http.StatusOK)
	// windows → 503 live unavailable。
	w := do(e, "/admin/risk/v2/users/7/windows")
	require.Equal(t, 503, w.Code)
	var ae RiskV2APIError
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &ae))
	require.Equal(t, riskV2ErrLiveUnavailable, ae.Code)
	// list + detail 仍 200（DB 独立）。
	require.Equal(t, 200, do(e, "/admin/risk/v2/users").Code)
	require.Equal(t, 200, do(e, "/admin/risk/v2/users/7").Code)
}

// §十五.29：Live Window 并发上限 → 部分 429。
func TestRiskV2API_LiveConcurrencyLimit(t *testing.T) {
	block := make(chan struct{})
	live := &fakeV2Live{block: block, snap: service.RiskV2ScoringSnapshot{UserID: 7}}
	e, _ := newV2Engine(t, &fakeV2Repo{}, &fakeV2Users{}, live, &fakeV2Status{}, http.StatusOK)
	n := riskV2LiveConcurrency + 3
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) { defer wg.Done(); codes[idx] = do(e, "/admin/risk/v2/users/7/windows").Code }(i)
	}
	// 等并发占满信号量后放行。
	time.Sleep(150 * time.Millisecond)
	close(block)
	wg.Wait()
	busy := 0
	for _, c := range codes {
		if c == http.StatusTooManyRequests {
			busy++
		}
	}
	require.GreaterOrEqual(t, busy, 1, "live window concurrency limit must reject excess with 429")
}

// §十五.25/26：no-store header + 响应无敏感字段。
func TestRiskV2API_NoStoreAndNoSecrets(t *testing.T) {
	repo := &fakeV2Repo{detail: strongDetail(), list: []service.RiskV2AssessmentListItem{{UserID: 1, EffectiveAction: "NONE"}}}
	e, _ := newV2Engine(t, repo, &fakeV2Users{}, &fakeV2Live{}, &fakeV2Status{}, http.StatusOK)
	for _, p := range []string{"/admin/risk/v2/status", "/admin/risk/v2/users", "/admin/risk/v2/users/7"} {
		w := do(e, p)
		require.Equal(t, "no-store", w.Header().Get("Cache-Control"), p)
		body := strings.ToLower(w.Body.String())
		// 敏感数据（原文/指纹/密钥/token/请求ID）绝不出现。注意 "prompt_pattern" 是合法证据族标签，
		// 故只禁 "raw_prompt" 与作为 JSON 键的 "prompt"，不禁子串 "prompt"。
		for _, bad := range []string{"hmac", "simhash", "raw_prompt", "\"prompt\"", "password", "lease_token", "request_id", "\"api_key\""} {
			require.NotContains(t, body, bad, "%s must not appear in %s", bad, p)
		}
	}
}

// §三：严格 query 校验（表驱动，非法一律 400 RISK_V2_BAD_REQUEST）。
func TestRiskV2API_QueryValidation(t *testing.T) {
	e, _ := newV2Engine(t, &fakeV2Repo{}, &fakeV2Users{}, &fakeV2Live{}, &fakeV2Status{}, http.StatusOK)
	invalid := []string{
		"tier=BOGUS", "min_risk_index=abc", "min_risk_index=NaN", "min_risk_index=Inf",
		"min_risk_index=-1", "min_risk_index=101", "data_sufficient=maybe", "degraded=2",
		"user_id=0", "user_id=-5", "user_id=abc", "assessed_from=-1", "assessed_to=-1",
		"assessed_from=100&assessed_to=50", "freshness=bogus", "limit=0", "limit=-1",
		"limit=abc", "offset=-1", "offset=abc",
	}
	for _, q := range invalid {
		w := do(e, "/admin/risk/v2/users?"+q)
		require.Equal(t, http.StatusBadRequest, w.Code, "query %q must be 400", q)
		var ae RiskV2APIError
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &ae))
		require.Equal(t, riskV2ErrBadRequest, ae.Code, "query %q", q)
	}
	valid := []string{
		"", "tier=HIGH", "tier=WATCH", "min_risk_index=50", "min_risk_index=0", "min_risk_index=100",
		"freshness=fresh", "freshness=stale", "freshness=all", "limit=1", "limit=200",
		"offset=0", "data_sufficient=true", "degraded=false", "assessed_from=1&assessed_to=2", "user_id=5",
	}
	for _, q := range valid {
		w := do(e, "/admin/risk/v2/users?"+q)
		require.Equal(t, http.StatusOK, w.Code, "query %q must be 200", q)
	}
}

// §四：分页 has_more / next_offset（limit+1 探测，不 COUNT）。
func TestRiskV2API_PaginationHasMore(t *testing.T) {
	// repo 返回 limit+1=4 行 → has_more=true, next_offset=offset+limit。
	repo := &fakeV2Repo{list: []service.RiskV2AssessmentListItem{
		{UserID: 1, EffectiveAction: "NONE"}, {UserID: 2, EffectiveAction: "NONE"},
		{UserID: 3, EffectiveAction: "NONE"}, {UserID: 4, EffectiveAction: "NONE"},
	}}
	e, _ := newV2Engine(t, repo, &fakeV2Users{}, &fakeV2Live{}, &fakeV2Status{}, http.StatusOK)
	w := do(e, "/admin/risk/v2/users?limit=3&offset=10")
	require.Equal(t, 200, w.Code)
	var resp RiskV2AssessmentListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Items, 3, "must return only limit rows")
	require.True(t, resp.HasMore)
	require.Equal(t, 13, resp.NextOffset)
	require.Equal(t, 4, repo.lastPage.Limit, "repo queried limit+1")

	// 返回 <=limit → has_more=false。
	repo2 := &fakeV2Repo{list: []service.RiskV2AssessmentListItem{{UserID: 1, EffectiveAction: "NONE"}}}
	e2, _ := newV2Engine(t, repo2, &fakeV2Users{}, &fakeV2Live{}, &fakeV2Status{}, http.StatusOK)
	w2 := do(e2, "/admin/risk/v2/users?limit=3")
	var resp2 RiskV2AssessmentListResponse
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &resp2))
	require.False(t, resp2.HasMore)
	require.Equal(t, 0, resp2.NextOffset)
}

// §二：Live Window 每管理员限流 → 429 RATE_LIMITED（与 BUSY 区分）。
func TestRiskV2API_LiveRateLimited(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fixed := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	svc := service.NewRiskV2AdminService(&fakeV2Repo{}, &fakeV2Users{}, &fakeV2Live{snap: service.RiskV2ScoringSnapshot{UserID: 7}}, &fakeV2Status{}, time.Hour, time.Second, func() time.Time { return fixed })
	h := NewRiskV2Handler(svc, 1, 2) // rate 1/s, burst 2
	e := gin.New()
	e.GET("/admin/risk/v2/users/:user_id/windows", h.GetWindows)
	// 快速连打 6 次（同一 IP key）→ burst 2 后被限。
	rateLimited := 0
	for i := 0; i < 6; i++ {
		w := do(e, "/admin/risk/v2/users/7/windows")
		if w.Code == http.StatusTooManyRequests {
			var ae RiskV2APIError
			_ = json.Unmarshal(w.Body.Bytes(), &ae)
			if ae.Code == riskV2ErrLiveRateLimited {
				rateLimited++
			}
		}
	}
	require.GreaterOrEqual(t, rateLimited, 1, "excess live-window requests must be rate-limited (429 RATE_LIMITED)")
}
